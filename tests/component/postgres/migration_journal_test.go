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

// TestMigrationJournalRecordsChainHead proves the migration journal records one row per applied migration
// with the file's real checksum and a non-empty version stamp, and that a second Migrate is a clean no-op
// (E15 T1). The journal head equals the binary's chain head, which is what an upgrade/DR audit reads to
// confirm the chain reached the expected version.
//
// EVERY MIGRATION IS JOURNALED NOW. The old sixty-seven-link chain introduced schema_revisions two thirds
// of the way along, so this loop carried a `m.Version < 33` arm asserting the earlier links were
// deliberately absent. The baseline creates the journal table in 000001, the runner's floor is gone, and
// an arm that skips versions below 33 in a two-link chain would skip the entire chain.
//
// IT ALSO CHECKS schema_migrations, AND THAT IS NOT DUPLICATION — IT IS TWELVE SPOT CHECKS REPLACED BY ONE
// COMPLETE ONE. Twelve tests in migration_test.go each counted their own migration's marker row
// ("schema_migrations records version 21 1 time"), which between them covered twelve of sixty-seven
// versions and none of the rest. The chain's markers are now asserted here for EVERY version, and
// statically for every migration file in storage.TestOrderedMigrationsIsContiguousVersionOrder.
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
		if err := pool.QueryRow(ctx, `SELECT checksum, applied_by FROM schema_revisions WHERE version = $1`, m.Version).
			Scan(&checksum, &appliedBy); err != nil {
			t.Fatalf("migration %d is not journaled: %v", m.Version, err)
		}
		if checksum != m.Checksum {
			t.Fatalf("migration %d journal checksum = %q, want the file's %q", m.Version, checksum, m.Checksum)
		}
		if appliedBy == "" {
			t.Fatalf("migration %d journal applied_by is empty", m.Version)
		}
		// The applied schema's own marker, which the boot preflight reads as the database head. Exactly
		// one row per version: the marker is `ON CONFLICT DO NOTHING`, so a chain re-applied on every
		// boot must not accumulate duplicates.
		var marked int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE version = $1`, m.Version).Scan(&marked); err != nil {
			t.Fatalf("count schema_migrations version %d: %v", m.Version, err)
		}
		if marked != 1 {
			t.Fatalf("schema_migrations records version %d %d times, want exactly 1", m.Version, marked)
		}
	}

	// And NOTHING BEYOND the chain: a marker for a version this binary does not carry would put the
	// database ahead of the binary and the preflight would refuse the next boot.
	var extra int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE version > $1`, wantHead).Scan(&extra); err != nil {
		t.Fatalf("count schema_migrations beyond the head: %v", err)
	}
	if extra != 0 {
		t.Fatalf("schema_migrations carries %d version(s) above the chain head %d", extra, wantHead)
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

// TestMigrationChainRoundTripsThroughRollback proves the chain survives apply -> rollback -> re-apply: the
// journal and the tables are present after apply, GONE after rollback, and back after the re-apply. The
// component tier leans on Rollback to return this shared database to empty between tests, so a down that
// leaves anything behind is a whole tier's worth of cross-contamination.
//
// IT WAS TestContractUsageEventsRoundTrip, and what it lost is worth naming rather than quietly dropping.
// It used to also prove the expand/migrate/CONTRACT pattern end to end: 000001 created usage_events,
// 000032 superseded it with usage_ledger, and 000034 DROPPED it, so "absent after apply" was evidence that
// a destructive contraction had really run. The squash removed every intermediate state, so there is no
// contraction left in the chain to witness — the baseline simply never creates the table. That pattern is
// unproven in this tier until a migration performs one again, and the first one that does should restore
// this half of the assertion rather than write a new test.
func TestMigrationChainRoundTripsThroughRollback(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()

	// A table from each of the two links: the journal is 000001's and the policy procedure is 000002's,
	// so the round trip below is over the WHOLE chain rather than its first half.
	if !tableExists(t, pool, "schema_revisions") {
		t.Fatal("after apply, schema_revisions is missing")
	}
	if !rowLevelSecurityEnabled(t, pool, "projects") {
		t.Fatal("after apply, projects has no row-level security (000002 did not run)")
	}
	for _, m := range storage.OrderedMigrations() {
		var count int
		if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT count(*) FROM schema_migrations WHERE version = $1`, m.Version).Scan(&count); err != nil {
			t.Fatalf("count schema_migrations version %d: %v", m.Version, err)
		}
		if count != 1 {
			t.Fatalf("schema_migrations records version %d %d times, want 1", m.Version, count)
		}
	}

	if err := cs.Rollback(ctx); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	if tableExists(t, pool, "schema_revisions") {
		t.Fatal("after rollback, schema_revisions still exists")
	}
	if tableExists(t, pool, "projects") {
		t.Fatal("after rollback, projects still exists")
	}

	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	if !tableExists(t, pool, "schema_revisions") {
		t.Fatal("after reapply, schema_revisions is missing")
	}
	if !rowLevelSecurityEnabled(t, pool, "projects") {
		t.Fatal("after reapply, projects has no row-level security")
	}
}

// rowLevelSecurityEnabled reports whether a table has row-level security both ENABLEd and FORCEd, which is
// the pair 000002's procedures assert. Reading only relrowsecurity would pass on a table whose owner still
// bypasses its own policies.
func rowLevelSecurityEnabled(t *testing.T, pool *pgxpool.Pool, table string) bool {
	t.Helper()
	var enabled, forced bool
	err := pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT c.relrowsecurity, c.relforcerowsecurity FROM pg_class c
		   JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE n.nspname = 'public' AND c.relname = $1`, table).Scan(&enabled, &forced)
	if err != nil {
		return false
	}
	return enabled && forced
}

// TestMigrationInterruptionResumes is claim OPS-006: a control-plane killed mid-chain restarts, the chain
// resumes to the correct head, and data is intact. It runs on a FRESH database so the partial state is
// real: PALAI_MIGRATE_FAULT_AFTER=1 aborts the chain right after 000001 commits, leaving the tables present
// and NOT ONE of them row-level secured (000002 has not run); a restart with no fault drives the chain to
// the head, secures them, and leaves rows seeded between the two runs byte-identical.
//
// THE FAULT MOVED FROM 33 TO 1, AND THE INTERRUPTION IS WHY THE SQUASH KEPT TWO FILES. This drill needs a
// state that is neither "nothing applied" nor "chain complete", and a one-migration chain has none: its
// single transaction either commits or rolls back. Splitting at the chain's real seam — tables, then the
// policies that can only be applied to tables that exist — is what keeps that state, and this test, real.
//
// THE PARTIAL STATE IS ALSO THE DANGEROUS ONE, which makes it worth witnessing rather than merely
// resuming from: between the two links every table exists with row-level security not enabled at all.
func TestMigrationInterruptionResumes(t *testing.T) {
	dbURL := freshDatabase(t)
	ctx := context.Background()

	// First boot: crash right after 000001.
	t.Setenv("PALAI_MIGRATE_FAULT_AFTER", "1")
	cs, err := coordinator.Open(ctx, dbURL)
	if err != nil {
		t.Fatalf("open fresh store: %v", err)
	}
	t.Cleanup(cs.Close)
	if err := cs.Migrate(ctx); err == nil {
		t.Fatal("Migrate() returned nil, want the injected interruption after 000001")
	} else if !strings.Contains(err.Error(), "injected interruption after migration 000001") {
		t.Fatalf("Migrate() error = %v, want the injected-interruption-after-1 fault", err)
	}
	pool := cs.Pool()
	sys := storage.WithSystemScope(ctx)

	// Partial state: both heads at 1, the tables there, and `projects` NOT yet secured.
	var journalHead, migrationHead int
	if err := pool.QueryRow(sys, `SELECT coalesce(max(version), 0) FROM schema_revisions`).Scan(&journalHead); err != nil {
		t.Fatalf("read journal head after fault: %v", err)
	}
	if journalHead != 1 {
		t.Fatalf("journal head after fault = %d, want 1 (partial)", journalHead)
	}
	if err := pool.QueryRow(sys, `SELECT coalesce(max(version), 0) FROM schema_migrations`).Scan(&migrationHead); err != nil {
		t.Fatalf("read schema_migrations head after fault: %v", err)
	}
	if migrationHead != 1 {
		t.Fatalf("schema_migrations head after fault = %d, want 1 (partial)", migrationHead)
	}
	if !tableExists(t, pool, "projects") {
		t.Fatal("projects is absent after the fault at 1, but 000001 creates it")
	}
	if rowLevelSecurityEnabled(t, pool, "projects") {
		t.Fatal("projects is already row-level secured after the fault at 1, but 000002 has not run — " +
			"the partial state this drill resumes from is not the one it claims")
	}

	// Seed rows between the crash and the resume; their digest must survive the resumed chain unchanged —
	// the "data intact" half of the claim.
	//
	// THE SEEDED TABLE IS `projects`, and the digest is over `id` alone. It has to be a table something
	// really writes: an earlier revision of this test checksummed a table this build no longer populates,
	// which made the comparison "" == "" — one that can never fail.
	prj1, prj2 := newID("prj"), newID("prj")
	exec(t, pool, `INSERT INTO projects (id) VALUES ($1)`, prj1)
	exec(t, pool, `INSERT INTO projects (id) VALUES ($1)`, prj2)
	before := seededRowDigest(t, pool)
	if before == "" {
		t.Fatal("the pre-migration digest is empty — nothing was seeded, so the comparison below is vacuous")
	}

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
	// The link the fault stopped short of really ran: the table seeded above is now secured.
	if !rowLevelSecurityEnabled(t, pool, "projects") {
		t.Fatal("projects is still unsecured after the resume — the chain reported the head without 000002 taking effect")
	}
	if after := seededRowDigest(t, pool); after != before {
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

// seededRowDigest is a stable md5 over the projects table's ids — the pre/post "row-checksum" the
// interruption/resume drill compares to prove the resumed chain left seeded data intact.
//
// IT MUST READ A TABLE THIS BUILD ACTUALLY WRITES. It once pointed at a table that had stopped being
// populated, and a digest over an empty table is "" on both sides of the migration — it reports "intact"
// whatever the chain did to anything else.
func seededRowDigest(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var digest string
	if err := pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT coalesce(md5(string_agg(id, '|' ORDER BY id)), '') FROM projects`).Scan(&digest); err != nil {
		t.Fatalf("compute projects digest: %v", err)
	}
	return digest
}
