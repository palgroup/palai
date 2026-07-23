//go:build component

package postgres

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/storage"
)

// freshDatabase creates a uniquely-named database on the SAME server the component Postgres URL points
// at — a "palai_e15t1_<rand>" stack whose name no sibling task's leak-guard counts — and returns a
// connection URL to it, registering its drop. The tests that need a PRISTINE chain (interruption/resume
// and the newer-database preflight) use it so they never mutate the suite's shared database.
func freshDatabase(t *testing.T) string {
	t.Helper()
	base := componentURL(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base) // the URL carries the superuser credential; no applyScope wrapper
	if err != nil {
		t.Fatalf("connect to create fresh database: %v", err)
	}
	defer admin.Close(ctx)

	name := newID("palai_e15t1") // prefix_hex — a valid, injection-free identifier
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create fresh database %s: %v", name, err)
	}
	t.Cleanup(func() {
		dropper, err := pgx.Connect(context.Background(), base)
		if err != nil {
			return
		}
		defer dropper.Close(context.Background())
		_, _ = dropper.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse component URL: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

// TestMigrationJournalRecordsChainHead proves the migration journal (000033) records one row per applied
// migration from its own introduction on, with the file's real checksum and a non-empty version stamp,
// and that a second Migrate is a clean no-op (E15 T1). The journal head equals the binary's chain head,
// which is what an upgrade/DR audit reads to confirm the chain reached the expected version.
func TestMigrationJournalRecordsChainHead(t *testing.T) {
	cs := openHarness(t) // migrates the shared database to the chain head
	pool := cs.Pool()
	ctx := storage.WithSystemScope(context.Background())

	migrations := storage.OrderedMigrations()
	wantHead := migrations[len(migrations)-1].Version

	var head int
	if err := pool.QueryRow(ctx, `SELECT coalesce(max(version), 0) FROM schema_revisions`).Scan(&head); err != nil {
		t.Fatalf("read journal head: %v", err)
	}
	if head != wantHead {
		t.Fatalf("journal head = %d, want the chain head %d", head, wantHead)
	}

	for _, m := range migrations {
		var checksum, appliedBy string
		err := pool.QueryRow(ctx, `SELECT checksum, applied_by FROM schema_revisions WHERE version = $1`, m.Version).
			Scan(&checksum, &appliedBy)
		if m.Version < 33 {
			// Migrations older than the journal are deliberately NOT recorded — they predate it.
			if err == nil {
				t.Fatalf("migration %d is journaled but predates the journal (000033)", m.Version)
			}
			continue
		}
		if err != nil {
			t.Fatalf("migration %d is not journaled: %v", m.Version, err)
		}
		if checksum != m.Checksum {
			t.Fatalf("migration %d journal checksum = %q, want the file's %q", m.Version, checksum, m.Checksum)
		}
		if appliedBy == "" {
			t.Fatalf("migration %d journal applied_by is empty", m.Version)
		}
	}

	// Idempotent: a second Migrate re-applies the whole chain but the journal's ON CONFLICT DO NOTHING
	// records no new rows — a re-run never double-inserts.
	var before int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_revisions`).Scan(&before); err != nil {
		t.Fatalf("count journal rows: %v", err)
	}
	if err := cs.Migrate(context.Background()); err != nil {
		t.Fatalf("re-Migrate: %v", err)
	}
	var after int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_revisions`).Scan(&after); err != nil {
		t.Fatalf("count journal rows after re-Migrate: %v", err)
	}
	if before != after {
		t.Fatalf("journal row count changed on re-Migrate: %d -> %d (double insert)", before, after)
	}
}

// TestMigrationJournalIsAppendOnlyToApplicationRole proves schema_revisions is append-only to the runtime
// role: palai_app may read the head (doctor/health) but the self-re-asserting REVOKE denies it UPDATE and
// DELETE, so the process cannot restate or erase migration history (the usage_ledger/audit precedent).
func TestMigrationJournalIsAppendOnlyToApplicationRole(t *testing.T) {
	cs := openHarness(t)
	pool := cs.Pool()
	ctx := storage.WithSystemScope(context.Background())

	// An explicit re-Migrate makes the precondition self-contained (independent of in-file test order):
	// 000029's blanket GRANT re-runs with schema_revisions already present and re-hands palai_app
	// UPDATE/DELETE, then 000033's REVOKE takes them back — so this proves the REVOKE RE-ASSERTS, not just
	// that boot #1 never granted the privileges.
	if err := cs.Migrate(context.Background()); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET ROLE palai_app`); err != nil {
		t.Fatalf("SET ROLE palai_app error = %v", err)
	}
	defer func() { _, _ = conn.Exec(ctx, `RESET ROLE`) }()

	// SELECT is granted — the head must be readable by the app.
	var head int
	if err := conn.QueryRow(ctx, `SELECT coalesce(max(version), 0) FROM schema_revisions`).Scan(&head); err != nil {
		t.Fatalf("read journal head as palai_app error = %v", err)
	}
	if got := pgCode(mustFail(conn.Exec(ctx, `UPDATE schema_revisions SET checksum = 'tampered'`))); got != "42501" {
		t.Fatalf("journal UPDATE code = %q, want 42501 insufficient_privilege", got)
	}
	if got := pgCode(mustFail(conn.Exec(ctx, `DELETE FROM schema_revisions`))); got != "42501" {
		t.Fatalf("journal DELETE code = %q, want 42501 insufficient_privilege", got)
	}
}

// TestContractUsageEventsRoundTrip proves 000033 (schema_revisions) and 000034 (the usage_events
// contract) round-trip cleanly: the journal is present after apply and gone after rollback, while
// usage_events is ABSENT after apply — the CONTRACT dropped it — and both versions are recorded in
// schema_migrations exactly once (E15 T1).
func TestContractUsageEventsRoundTrip(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()

	if !tableExists(t, pool, "schema_revisions") {
		t.Fatal("after apply, schema_revisions is missing")
	}
	if tableExists(t, pool, "usage_events") {
		t.Fatal("after apply, usage_events still exists (000034 must have dropped it)")
	}
	for _, version := range []int{33, 34} {
		var count int
		if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT count(*) FROM schema_migrations WHERE version = $1`, version).Scan(&count); err != nil {
			t.Fatalf("count schema_migrations version %d: %v", version, err)
		}
		if count != 1 {
			t.Fatalf("schema_migrations records version %d %d times, want 1", version, count)
		}
	}

	if err := cs.Rollback(ctx); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	if tableExists(t, pool, "schema_revisions") {
		t.Fatal("after rollback, schema_revisions still exists")
	}

	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	if !tableExists(t, pool, "schema_revisions") {
		t.Fatal("after reapply, schema_revisions is missing")
	}
	if tableExists(t, pool, "usage_events") {
		t.Fatal("after reapply, usage_events still exists")
	}
}

// TestMigrationInterruptionResumes is claim OPS-006: a control-plane killed mid-chain restarts, the chain
// resumes to the correct head, and data is intact. It runs on a FRESH database so the partial state is
// real: PALAI_MIGRATE_FAULT_AFTER=33 aborts the chain right after 000033 commits, leaving the journal
// head at 33 and usage_events still present (000034 has not run); a restart (no fault) drives the chain to
// 34, drops usage_events, and leaves rows seeded between the two runs byte-identical.
func TestMigrationInterruptionResumes(t *testing.T) {
	dbURL := freshDatabase(t)
	ctx := context.Background()

	// First boot: crash right after 000033.
	t.Setenv("PALAI_MIGRATE_FAULT_AFTER", "33")
	cs, err := coordinator.Open(ctx, dbURL)
	if err != nil {
		t.Fatalf("open fresh store: %v", err)
	}
	t.Cleanup(cs.Close)
	if err := cs.Migrate(ctx); err == nil {
		t.Fatal("Migrate() returned nil, want the injected interruption after 000033")
	} else if !strings.Contains(err.Error(), "injected interruption after migration 000033") {
		t.Fatalf("Migrate() error = %v, want the injected-interruption-after-33 fault", err)
	}
	pool := cs.Pool()
	sys := storage.WithSystemScope(ctx)

	// Partial state: journal head 33, schema_migrations head 33, usage_events present (000001 created it,
	// 000034 has not dropped it).
	var journalHead, migrationHead int
	if err := pool.QueryRow(sys, `SELECT coalesce(max(version), 0) FROM schema_revisions`).Scan(&journalHead); err != nil {
		t.Fatalf("read journal head after fault: %v", err)
	}
	if journalHead != 33 {
		t.Fatalf("journal head after fault = %d, want 33 (partial)", journalHead)
	}
	if err := pool.QueryRow(sys, `SELECT coalesce(max(version), 0) FROM schema_migrations`).Scan(&migrationHead); err != nil {
		t.Fatalf("read schema_migrations head after fault: %v", err)
	}
	if migrationHead != 33 {
		t.Fatalf("schema_migrations head after fault = %d, want 33 (partial)", migrationHead)
	}
	if !tableExists(t, pool, "usage_events") {
		t.Fatal("usage_events is gone after the fault at 33, but 000034 should not have run yet")
	}

	// Seed rows between the crash and the resume; their checksum must survive the resumed chain (which
	// includes the destructive 000034 contract) unchanged — the "data intact" half of the claim.
	org1, org2 := newID("org"), newID("org")
	exec(t, pool, `INSERT INTO organizations (id) VALUES ($1)`, org1)
	exec(t, pool, `INSERT INTO organizations (id) VALUES ($1)`, org2)
	before := orgDigest(t, pool)

	// Restart with no fault: the chain resumes to the head.
	t.Setenv("PALAI_MIGRATE_FAULT_AFTER", "")
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("resume Migrate() error = %v", err)
	}
	if err := pool.QueryRow(sys, `SELECT coalesce(max(version), 0) FROM schema_revisions`).Scan(&journalHead); err != nil {
		t.Fatalf("read journal head after resume: %v", err)
	}
	wantHead := storage.OrderedMigrations()
	if journalHead != wantHead[len(wantHead)-1].Version {
		t.Fatalf("journal head after resume = %d, want the chain head %d", journalHead, wantHead[len(wantHead)-1].Version)
	}
	if tableExists(t, pool, "usage_events") {
		t.Fatal("usage_events survived the resumed chain, but 000034 should have dropped it")
	}
	if after := orgDigest(t, pool); after != before {
		t.Fatalf("seeded rows changed across the resumed migration: %q -> %q (data not intact)", before, after)
	}
}

// TestMigrationPreflightRejectsNewerDatabase proves the boot preflight refuses to run an OLDER binary
// against a NEWER database: a schema head above the binary's chain head aborts the migrate with a clear
// forward-path error rather than mis-applying an old chain over new state (plan §48.2/§48.3.1).
func TestMigrationPreflightRejectsNewerDatabase(t *testing.T) {
	dbURL := freshDatabase(t)
	ctx := context.Background()
	cs, err := coordinator.Open(ctx, dbURL)
	if err != nil {
		t.Fatalf("open fresh store: %v", err)
	}
	t.Cleanup(cs.Close)
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("initial Migrate() error = %v", err)
	}

	// Stamp a version far above the binary's head, as the owner (000030 revoked the runtime role's write).
	head := storage.OrderedMigrations()
	execAsOwner(t, cs.Pool(), `INSERT INTO schema_migrations (version) VALUES ($1)`, head[len(head)-1].Version+100)

	err = cs.Migrate(ctx)
	if err == nil {
		t.Fatal("Migrate() returned nil against a newer database, want a preflight rejection")
	}
	if !strings.Contains(err.Error(), "newer than this binary") {
		t.Fatalf("Migrate() error = %v, want a version-window rejection", err)
	}
}

// TestMigrationConcurrentBootsSerialize proves the advisory lock serializes concurrent migrators: three
// control-planes migrating the SAME fresh database at once all succeed and land the head, instead of
// racing on usage_events' create(000001)/drop(000034) into a 23505/42P01 boot failure (SF1).
func TestMigrationConcurrentBootsSerialize(t *testing.T) {
	dbURL := freshDatabase(t)
	ctx := context.Background()
	cs, err := coordinator.Open(ctx, dbURL)
	if err != nil {
		t.Fatalf("open fresh store: %v", err)
	}
	t.Cleanup(cs.Close)

	const boots = 3
	errs := make(chan error, boots)
	var wg sync.WaitGroup
	for i := 0; i < boots; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- cs.Migrate(ctx) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Migrate() error = %v, want serialized success", err)
		}
	}
	migrations := storage.OrderedMigrations()
	var head int
	if err := cs.Pool().QueryRow(storage.WithSystemScope(ctx), `SELECT coalesce(max(version), 0) FROM schema_revisions`).Scan(&head); err != nil {
		t.Fatalf("read journal head: %v", err)
	}
	if head != migrations[len(migrations)-1].Version {
		t.Fatalf("journal head after concurrent boots = %d, want %d", head, migrations[len(migrations)-1].Version)
	}
}

// TestMigrationPreflightFailsClosedOnBadGateVars proves the disk and backup gates FAIL the boot when
// their var is SET but unparseable (SF2) — an operator's typo must not silently drop the protection.
// Preflight rejects before the chain runs, so the shared database is left untouched.
func TestMigrationPreflightFailsClosedOnBadGateVars(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()

	t.Setenv("PALAI_MIGRATE_MIN_DISK_BYTES", "2G") // a suffix where plain bytes are expected
	if err := cs.Migrate(ctx); err == nil || !strings.Contains(err.Error(), "MIN_DISK_BYTES") {
		t.Fatalf("Migrate() with an unparseable disk floor error = %v, want a fail-closed rejection", err)
	}
	t.Setenv("PALAI_MIGRATE_MIN_DISK_BYTES", "")

	t.Setenv("PALAI_MIGRATE_REQUIRE_BACKUP", "required") // not a boolean
	if err := cs.Migrate(ctx); err == nil || !strings.Contains(err.Error(), "REQUIRE_BACKUP") {
		t.Fatalf("Migrate() with a non-boolean backup flag error = %v, want a fail-closed rejection", err)
	}
}

// orgDigest is a stable md5 over the organizations table's ids — the pre/post "row-checksum" the
// interruption/resume drill compares to prove the resumed chain left seeded data intact.
func orgDigest(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var digest string
	if err := pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT coalesce(md5(string_agg(id, '|' ORDER BY id)), '') FROM organizations`).Scan(&digest); err != nil {
		t.Fatalf("compute organizations digest: %v", err)
	}
	return digest
}
