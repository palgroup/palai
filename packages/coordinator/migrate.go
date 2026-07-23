package coordinator

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/palgroup/palai/packages/version"
	"github.com/palgroup/palai/storage"
)

// journalIntroVersion is the migration that creates the schema_revisions journal (000033). The boot
// runner records a journal row only from this version on; earlier migrations predate the journal and
// stay recorded in schema_migrations alone.
const journalIntroVersion = 33

// migrationLockKey is the fixed 64-bit key the boot chain holds a pg_advisory_lock on so only one
// migrator runs at a time (the bytes spell "PALAI_MG"). Any stable arbitrary constant works; it only has
// to be the same across every control-plane replica.
const migrationLockKey int64 = 0x50414c41495f4d47

// MigratorVersion is the version stamp written to schema_revisions.applied_by. Empty by default: the
// stamp then falls back to the shared build stamp (packages/version.Resolve — the ldflags-injected
// release stamp, else the embedded VCS revision, else "dev"). A caller may still override it directly
// with -ldflags "-X github.com/palgroup/palai/packages/coordinator.MigratorVersion=<v>", but E15 T2's
// scripts/release/build.sh injects the single shared packages/version.Stamp instead. Build id, never a secret.
var MigratorVersion = ""

// migrate applies the forward chain migration-by-migration (E15 T1): a boot PREFLIGHT, then each
// migration in its OWN bounded transaction with its own journal row. Splitting the old single-Exec chain
// is what makes an interrupted upgrade RESUMABLE — a crash leaves the chain at the last committed
// migration, and a restart re-runs from the top (every file is idempotent) to the head. It replaces the
// old asOwner(MigrationUp()) path; Rollback still uses asOwner(MigrationDown()).
func (s *Store) migrate(ctx context.Context) error {
	if err := s.preflight(ctx); err != nil {
		return err
	}
	// Single active migrator: hold a session advisory lock across the WHOLE chain on a dedicated
	// connection, so two control-planes booting at once (E15's multi-replica K8s) serialize — the second
	// waits, then re-runs the idempotent chain as a no-op. Without it, because usage_events is
	// create(000001)/drop(000034) every boot, two concurrent chains race on a CREATE/DROP (23505 or a
	// catalog 42P01) and one boot fails. The lock is session-scoped, so it auto-releases if this migrator
	// crashes — a dead migrator never wedges the next boot.
	lockConn, err := s.pool.Acquire(storage.WithSystemScope(ctx))
	if err != nil {
		return fmt.Errorf("acquire migration lock connection: %w", err)
	}
	defer lockConn.Release()
	if _, err := lockConn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockKey); err != nil {
		return fmt.Errorf("acquire migration advisory lock: %w", err)
	}
	defer func() { _, _ = lockConn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", migrationLockKey) }()

	// The interruption hook (OPS-006): PALAI_MIGRATE_FAULT_AFTER=<version> aborts the chain right after
	// that migration commits, standing in for a control-plane crash mid-chain. Nothing in production sets
	// it, so the chain runs to completion; the component test and the live drill set it to prove resume.
	faultAfter := envIntOr("PALAI_MIGRATE_FAULT_AFTER", -1)
	for _, m := range storage.OrderedMigrations() {
		if err := s.applyMigration(ctx, m); err != nil {
			return fmt.Errorf("apply migration %06d_%s: %w", m.Version, m.Name, err)
		}
		if m.Version == faultAfter {
			return fmt.Errorf("PALAI_MIGRATE_FAULT_AFTER=%d: injected interruption after migration %06d_%s (test/drill only)", faultAfter, m.Version, m.Name)
		}
	}
	return nil
}

// applyMigration applies ONE forward migration in a single owner-role transaction, bounded by
// lock_timeout/statement_timeout, and — from journalIntroVersion on — records its first-apply row in the
// same transaction so the journal head can never run ahead of the schema. The journal insert is
// ON CONFLICT DO NOTHING, so a re-run (the whole chain re-applies every boot) is a clean no-op that never
// double-inserts.
func (s *Store) applyMigration(ctx context.Context, m storage.Migration) error {
	conn, err := s.pool.Acquire(storage.WithSystemScope(ctx))
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Release()
	// RESET ROLE so DDL runs as the owning role: the runtime role (migration 000029's palai_app) owns
	// nothing and may not ALTER. RESET lasts only this acquisition; the pool re-applies the runtime scope
	// when the connection is next handed out.
	if _, err := conn.Exec(ctx, "RESET ROLE"); err != nil {
		return fmt.Errorf("reset to owning role: %w", err)
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	// Bounded lock: a migration that cannot acquire its lock, or a statement that never returns, aborts
	// the boot instead of hanging forever while holding a lock. SET LOCAL is scoped to this transaction
	// and auto-resets on commit/rollback, so no timeout leaks back onto the pooled connection. The values
	// are integers (milliseconds) built into the statement — SET does not take bind parameters — so
	// envIntOr's integer result is the only thing interpolated (no injection surface).
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = %d", envIntOr("PALAI_MIGRATE_LOCK_TIMEOUT_MS", 15000))); err != nil {
		return fmt.Errorf("set lock_timeout: %w", err)
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", envIntOr("PALAI_MIGRATE_STATEMENT_TIMEOUT_MS", 300000))); err != nil {
		return fmt.Errorf("set statement_timeout: %w", err)
	}
	if _, err := tx.Exec(ctx, m.Up); err != nil {
		return err
	}
	if m.Version >= journalIntroVersion {
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_revisions (version, checksum, applied_by) VALUES ($1, $2, $3)
			 ON CONFLICT (version) DO NOTHING`,
			m.Version, m.Checksum, migratorVersionStamp()); err != nil {
			return fmt.Errorf("record journal row: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit migration: %w", err)
	}
	return nil
}

// preflight is the boot pre-migration check (plan §48.3.1): a version window (always enforced), and two
// opt-in gates for disk headroom and backup status. It runs once, before the chain.
func (s *Store) preflight(ctx context.Context) error {
	ordered := storage.OrderedMigrations()
	binaryHead := ordered[len(ordered)-1].Version

	dbHead, err := s.schemaHead(ctx)
	if err != nil {
		return fmt.Errorf("migration preflight: read schema head: %w", err)
	}
	// Version window: refuse to run an OLDER binary against a NEWER database. A database whose recorded
	// head exceeds this binary's max migration was migrated by a newer control-plane, and applying this
	// older chain over it could mis-shape state — so abort with the operator's forward path (§48.2).
	if dbHead > binaryHead {
		return fmt.Errorf("migration preflight: database schema head %d is newer than this binary's %d — upgrade the control-plane binary before it migrates (never downgrade across a migration)", dbHead, binaryHead)
	}

	// Disk headroom (opt-in gate, default off): statfs the data path and refuse when free space is below
	// the configured floor, so a migration does not start on a disk that fills mid-DDL. Off by default
	// because the honest ceiling is that this measures the CONTROL-PLANE host's own path, not necessarily
	// the (possibly external/managed) database volume — an operator enables it for a risky upgrade.
	//
	// FAIL-CLOSED: this floor is a protection, so a SET-but-unparseable value (a "2G" typo where a byte
	// count is expected) FAILS the boot rather than silently disabling the gate.
	if raw := os.Getenv("PALAI_MIGRATE_MIN_DISK_BYTES"); raw != "" {
		floor, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || floor < 0 {
			return fmt.Errorf("migration preflight: PALAI_MIGRATE_MIN_DISK_BYTES=%q is not a non-negative byte count — fix it (plain bytes, e.g. 2147483648) or unset it", raw)
		}
		if floor > 0 {
			path := envStrOr("PALAI_MIGRATE_DISK_PATH", ".")
			free, err := freeDiskBytes(path)
			if err != nil {
				return fmt.Errorf("migration preflight: disk check on %s: %w", path, err)
			}
			if free < uint64(floor) {
				return fmt.Errorf("migration preflight: %s has %d bytes free, below the %d floor — free space before migrating", path, free, floor)
			}
		}
	}

	// Backup status (opt-in gate, default off): a destructive (contract) upgrade wants a restore point
	// first. When PALAI_MIGRATE_REQUIRE_BACKUP is true, the marker the backup tool writes
	// (PALAI_MIGRATE_BACKUP_MARKER) must exist, or the migration is refused. Ceiling: the runner trusts
	// the marker's presence; it does not itself verify backup integrity — that is `palai backup verify`.
	//
	// FAIL-CLOSED: a SET-but-unrecognized value (REQUIRE_BACKUP=required) FAILS the boot rather than
	// silently reading as false and dropping the protection.
	if raw := os.Getenv("PALAI_MIGRATE_REQUIRE_BACKUP"); raw != "" {
		require, ok := parseBool(raw)
		if !ok {
			return fmt.Errorf("migration preflight: PALAI_MIGRATE_REQUIRE_BACKUP=%q is not a boolean — use 1/true or 0/false", raw)
		}
		if require {
			marker := os.Getenv("PALAI_MIGRATE_BACKUP_MARKER")
			if marker == "" {
				return fmt.Errorf("migration preflight: PALAI_MIGRATE_REQUIRE_BACKUP is set but PALAI_MIGRATE_BACKUP_MARKER names no marker")
			}
			if _, err := os.Stat(marker); err != nil {
				return fmt.Errorf("migration preflight: required backup marker %s is absent (%v) — take a backup before this upgrade", marker, err)
			}
		}
	}
	return nil
}

// schemaHead reads the highest applied migration version, or 0 for a truly fresh database whose
// schema_migrations table 000001 has not created yet. The existence check is a SEPARATE query because
// Postgres resolves a table name at PARSE time even inside a never-taken CASE branch, so a single guarded
// query would still 42P01 on a fresh database.
func (s *Store) schemaHead(ctx context.Context) (int, error) {
	sys := storage.WithSystemScope(ctx)
	var reg *string
	if err := s.pool.QueryRow(sys, `SELECT to_regclass('public.schema_migrations')::text`).Scan(&reg); err != nil {
		return 0, err
	}
	if reg == nil {
		return 0, nil // fresh database: 000001 has not created the migration ledger yet
	}
	var head int
	if err := s.pool.QueryRow(sys, `SELECT coalesce(max(version), 0) FROM schema_migrations`).Scan(&head); err != nil {
		return 0, err
	}
	return head, nil
}

// migratorVersionStamp resolves the applied_by stamp: an explicit coordinator.MigratorVersion override,
// else the shared build stamp (packages/version.Resolve — the ldflags release stamp, the embedded VCS
// revision, or "dev"). Sharing version.Resolve keeps the migrator's applied_by identical to the version
// the runner advertises and the control-plane checks the support window against.
func migratorVersionStamp() string {
	if MigratorVersion != "" {
		return MigratorVersion
	}
	return version.Resolve()
}

// freeDiskBytes returns the bytes available to a non-root writer on the filesystem backing path.
// ponytail: syscall.Statfs, which the linux/darwin the control-plane and its tests run on both provide
// (Bavail * Bsize); the project ships no Windows target, so no build-tagged fallback is carried — add
// one only if a Windows build ever appears.
func freeDiskBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}

func envIntOr(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func envStrOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// parseBool recognizes the usual boolean spellings and reports ok=false for anything else, so a
// fail-closed caller can reject a typo instead of silently reading it as false.
func parseBool(v string) (value, ok bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	}
	return false, false
}
