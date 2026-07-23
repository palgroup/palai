package coordinator

import (
	"context"
	"fmt"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"

	"github.com/palgroup/palai/storage"
)

// journalIntroVersion is the migration that creates the schema_revisions journal (000033). The boot
// runner records a journal row only from this version on; earlier migrations predate the journal and
// stay recorded in schema_migrations alone.
const journalIntroVersion = 33

// MigratorVersion is the version stamp written to schema_revisions.applied_by. It is empty by default and
// resolved from the build's embedded VCS revision at run time; a release build overrides it with
// -ldflags "-X github.com/palgroup/palai/packages/coordinator.MigratorVersion=<v>" (E15 T2 wires the real
// stamp). It is a build identifier, never a secret.
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
	if floor := envInt64Or("PALAI_MIGRATE_MIN_DISK_BYTES", 0); floor > 0 {
		path := envStrOr("PALAI_MIGRATE_DISK_PATH", ".")
		free, err := freeDiskBytes(path)
		if err != nil {
			return fmt.Errorf("migration preflight: disk check on %s: %w", path, err)
		}
		if free < uint64(floor) {
			return fmt.Errorf("migration preflight: %s has %d bytes free, below the %d floor — free space before migrating", path, free, floor)
		}
	}

	// Backup status (opt-in gate, default off): a destructive (contract) upgrade wants a restore point
	// first. When PALAI_MIGRATE_REQUIRE_BACKUP is truthy, the marker the backup tool writes
	// (PALAI_MIGRATE_BACKUP_MARKER) must exist, or the migration is refused. Ceiling: the runner trusts
	// the marker's presence; it does not itself verify backup integrity — that is `palai backup verify`.
	if truthy(os.Getenv("PALAI_MIGRATE_REQUIRE_BACKUP")) {
		marker := os.Getenv("PALAI_MIGRATE_BACKUP_MARKER")
		if marker == "" {
			return fmt.Errorf("migration preflight: PALAI_MIGRATE_REQUIRE_BACKUP is set but PALAI_MIGRATE_BACKUP_MARKER names no marker")
		}
		if _, err := os.Stat(marker); err != nil {
			return fmt.Errorf("migration preflight: required backup marker %s is absent (%v) — take a backup before this upgrade", marker, err)
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

// migratorVersionStamp resolves the applied_by stamp: an explicit ldflags override, else the build's
// embedded VCS revision (short, +"-dirty" for a modified tree), else "dev" for a `go test`/`go run`
// binary that carries no VCS stamp.
func migratorVersionStamp() string {
	if MigratorVersion != "" {
		return MigratorVersion
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		rev, dirty := "", false
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				rev = setting.Value
			case "vcs.modified":
				dirty = setting.Value == "true"
			}
		}
		if rev != "" {
			if len(rev) > 12 {
				rev = rev[:12]
			}
			if dirty {
				return rev + "-dirty"
			}
			return rev
		}
	}
	return "dev"
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

func envInt64Or(name string, def int64) int64 {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
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

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
