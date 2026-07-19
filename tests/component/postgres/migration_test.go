//go:build component

// Package postgres holds the real-PostgreSQL component tests for the durable
// execution spine. They run only under `make test-component TEST=postgres`, which
// starts a throwaway container and exports PALAI_COMPONENT_POSTGRES_URL. The build
// tag keeps them out of the credential-free, Docker-free unit tier.
package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/packages/coordinator"
)

// allTables is every relation the core migration must create (brief Step 3).
var allTables = []string{
	"organizations", "projects", "principals", "api_keys",
	"idempotency_records",
	"sessions", "responses", "messages", "runs", "attempts",
	"session_sequences", "events", "commands",
	"config_revisions",
	"durable_jobs", "job_attempts", "outbox", "inbox",
	"runner_pools", "runners", "runner_leases",
	"model_connections", "model_routes", "model_route_revisions",
	"tool_calls",
	"artifacts",
	"usage_events", "audit_events",
	"schema_migrations",
}

func componentURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-component TEST=postgres")
	}
	return url
}

// openHarness returns a migrated durable-spine store. Migrate is idempotent, so
// every test starts from applied schema.
func openHarness(t *testing.T) *coordinator.Store {
	t.Helper()
	cs, err := coordinator.Open(context.Background(), componentURL(t))
	if err != nil {
		t.Fatalf("coordinator.Open() error = %v", err)
	}
	t.Cleanup(cs.Close)
	if err := cs.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	return cs
}

func newID(prefix string) string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return prefix + "_" + hex.EncodeToString(raw[:])
}

// seedRun creates org -> project -> session -> run and returns the scope and IDs.
func seedRun(t *testing.T, pool *pgxpool.Pool) (coordinator.Tenant, string, string) {
	t.Helper()
	tenant := coordinator.Tenant{Organization: newID("org"), Project: newID("prj")}
	sessionID := newID("ses")
	runID := newID("run")
	exec(t, pool, `INSERT INTO organizations (id) VALUES ($1)`, tenant.Organization)
	exec(t, pool, `INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, tenant.Project, tenant.Organization)
	exec(t, pool, `INSERT INTO sessions (id, organization_id, project_id) VALUES ($1, $2, $3)`,
		sessionID, tenant.Organization, tenant.Project)
	exec(t, pool, `INSERT INTO runs (id, organization_id, project_id, session_id) VALUES ($1, $2, $3, $4)`,
		runID, tenant.Organization, tenant.Project, sessionID)
	return tenant, sessionID, runID
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q error = %v", sql, err)
	}
}

// pgCode returns the SQLSTATE of a PostgreSQL error, or "" if err is not one.
func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func tableExists(t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var reg *string
	if err := pool.QueryRow(context.Background(), `SELECT to_regclass('public.' || $1)::text`, name).Scan(&reg); err != nil {
		t.Fatalf("to_regclass(%s) error = %v", name, err)
	}
	return reg != nil
}

func columnExists(t *testing.T, pool *pgxpool.Pool, table, column string) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = $1 AND column_name = $2)`,
		table, column).Scan(&exists); err != nil {
		t.Fatalf("column exists %s.%s error = %v", table, column, err)
	}
	return exists
}

// TestSessionChainingMigrationColumns proves 000003 adds its columns idempotently and
// reverses cleanly: the columns exist after apply (and a re-apply is a no-op), are gone
// after rollback, and return after reapply (spec §9 chaining, migration re-run safety).
func TestSessionChainingMigrationColumns(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()

	// The 000003 columns are present after apply, and a second Migrate is a clean no-op
	// (ADD COLUMN IF NOT EXISTS makes the whole chain safe to re-run).
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	if !columnExists(t, pool, "sessions", "active_root_run_id") {
		t.Fatal("after apply, sessions.active_root_run_id is missing")
	}
	if !columnExists(t, pool, "events", "response_id") {
		t.Fatal("after apply, events.response_id is missing")
	}

	if err := cs.Rollback(ctx); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	if columnExists(t, pool, "sessions", "active_root_run_id") {
		t.Fatal("after rollback, sessions.active_root_run_id still exists")
	}
	if columnExists(t, pool, "events", "response_id") {
		t.Fatal("after rollback, events.response_id still exists")
	}

	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	if !columnExists(t, pool, "sessions", "active_root_run_id") || !columnExists(t, pool, "events", "response_id") {
		t.Fatal("after reapply, a 000003 column is missing")
	}
}

// TestSessionChainingMigrationBackfillsPreexistingEvents proves the one-shot backfill closes
// the upgrade-boundary retention gap: events written before 000003 carry a NULL response_id
// the per-response scrub can't reach, so the migration keys each session's legacy events to
// its sole response (LP-0 1:1). Re-running the marker-gated backfill drives the real
// migration path, not a copy.
func TestSessionChainingMigrationBackfillsPreexistingEvents(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()

	tenant, sessionID, _ := seedRun(t, pool)
	// A pre-000003 response with events that predate the response_id column (NULL-keyed).
	respID := seedTerminalResponse(t, pool, tenant, sessionID, false, time.Hour)
	for seq := 1; seq <= 2; seq++ {
		exec(t, pool,
			`INSERT INTO events (id, organization_id, project_id, session_id, seq, type, payload)
			 VALUES ($1, $2, $3, $4, $5, 'output.item.v1', '{"content":"legacy"}')`,
			newID("evt"), tenant.Organization, tenant.Project, sessionID, seq)
	}

	// Clear the version marker so the one-shot backfill runs again on the next Migrate.
	exec(t, pool, `DELETE FROM schema_migrations WHERE version = 3`)
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}

	// The legacy events are now keyed to their session's sole response, so the per-response
	// purge can reach them (the upgrade-boundary gap is closed).
	var keyed int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM events WHERE session_id = $1 AND response_id = $2`, sessionID, respID).Scan(&keyed); err != nil {
		t.Fatalf("count keyed events error = %v", err)
	}
	if keyed != 2 {
		t.Fatalf("backfilled events keyed to the response = %d, want 2", keyed)
	}
}

// TestConfigRevisionsMigration proves 000005 adds its table and column idempotently and
// reverses cleanly: config_revisions and projects.config_policy exist after apply (a re-apply
// is a clean no-op), are gone after rollback, and return after reapply (spec §9.3, §14;
// migration re-run safety, the 000002/000003 pattern).
func TestConfigRevisionsMigration(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()

	// Present after apply, and a second Migrate is a clean no-op (CREATE TABLE / ADD COLUMN
	// IF NOT EXISTS makes the whole chain safe to re-run).
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	if !tableExists(t, pool, "config_revisions") {
		t.Fatal("after apply, config_revisions is missing")
	}
	if !columnExists(t, pool, "projects", "config_policy") {
		t.Fatal("after apply, projects.config_policy is missing")
	}

	if err := cs.Rollback(ctx); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	if tableExists(t, pool, "config_revisions") {
		t.Fatal("after rollback, config_revisions still exists")
	}
	if columnExists(t, pool, "projects", "config_policy") {
		t.Fatal("after rollback, projects.config_policy still exists")
	}

	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	if !tableExists(t, pool, "config_revisions") || !columnExists(t, pool, "projects", "config_policy") {
		t.Fatal("after reapply, a 000005 object is missing")
	}
}

// indexExists reports whether an index of the given name is present in the public schema.
func indexExists(t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var reg *string
	if err := pool.QueryRow(context.Background(), `SELECT to_regclass('public.' || $1)::text`, name).Scan(&reg); err != nil {
		t.Fatalf("to_regclass(%s) error = %v", name, err)
	}
	return reg != nil
}

// TestOneActiveRootMigration proves 000006 adds its one-active-root index idempotently and
// reverses cleanly: the index exists after apply (a re-apply is a clean no-op), is gone after
// rollback, and returns after reapply (spec §22.3; migration re-run safety, the 000002/000003
// pattern).
func TestOneActiveRootMigration(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()

	// Present after apply, and a second Migrate is a clean no-op (CREATE INDEX IF NOT EXISTS).
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	if !indexExists(t, pool, "runs_one_active_root_per_session") {
		t.Fatal("after apply, runs_one_active_root_per_session is missing")
	}

	if err := cs.Rollback(ctx); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	if indexExists(t, pool, "runs_one_active_root_per_session") {
		t.Fatal("after rollback, runs_one_active_root_per_session still exists")
	}

	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	if !indexExists(t, pool, "runs_one_active_root_per_session") {
		t.Fatal("after reapply, runs_one_active_root_per_session is missing")
	}
}

// TestChildRunsMigration proves 000007 adds its child-run columns idempotently and reverses
// cleanly: runs.parent_run_id/depth/delegation exist after apply (a re-apply is a clean no-op),
// are gone after rollback, and return after reapply (spec §11, §25.18-19; the 000005/000006
// re-run-safety pattern). The one-active-root index survives the DROP-and-recreate the migration
// uses to add the child-excluding predicate.
func TestChildRunsMigration(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()

	// Present after apply, and a second Migrate is a clean no-op (ADD COLUMN IF NOT EXISTS +
	// the DROP/CREATE index step is idempotent — a re-run recreates the same new-predicate index).
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	for _, col := range []string{"parent_run_id", "depth", "delegation"} {
		if !columnExists(t, pool, "runs", col) {
			t.Fatalf("after apply, runs.%s is missing", col)
		}
	}
	if !indexExists(t, pool, "runs_one_active_root_per_session") {
		t.Fatal("after apply, runs_one_active_root_per_session is missing")
	}

	if err := cs.Rollback(ctx); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	for _, col := range []string{"parent_run_id", "depth", "delegation"} {
		if columnExists(t, pool, "runs", col) {
			t.Fatalf("after rollback, runs.%s still exists", col)
		}
	}

	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	for _, col := range []string{"parent_run_id", "depth", "delegation"} {
		if !columnExists(t, pool, "runs", col) {
			t.Fatalf("after reapply, runs.%s is missing", col)
		}
	}
}

// TestChildRunDoesNotConsumeRootSlot proves the 000007 predicate change (spec §22.3, §22.8): a
// child run (parent_run_id set) shares its parent's session but is excluded from the
// one-active-root index, so it never consumes the session's single root slot — while a second
// concurrent ROOT run (parent_run_id NULL) still conflicts. This is the child leg of
// TestSecondConcurrentRootRunConflicts.
func TestChildRunDoesNotConsumeRootSlot(t *testing.T) {
	cs := openHarness(t)
	pool := cs.Pool()
	ctx := context.Background()
	tenant, sessionID, rootRunID := seedRun(t, pool)

	// The seeded root run is queued (non-terminal): it holds the session's single root slot.
	// A child run of that root, in the SAME session and non-terminal, is admitted — it is
	// excluded from the root-only index.
	if _, err := pool.Exec(ctx,
		`INSERT INTO runs (id, organization_id, project_id, session_id, state, parent_run_id, depth)
		 VALUES ($1, $2, $3, $4, 'running', $5, 1)`,
		newID("run"), tenant.Organization, tenant.Project, sessionID, rootRunID); err != nil {
		t.Fatalf("child run in the parent's session error = %v, want admitted (excluded from one-active-root)", err)
	}
	// A second concurrent ROOT run (parent_run_id NULL) for the same session is still the
	// one-active-root violation — the child did not free or fill the root slot.
	_, err := pool.Exec(ctx,
		`INSERT INTO runs (id, organization_id, project_id, session_id, state) VALUES ($1, $2, $3, $4, 'running')`,
		newID("run"), tenant.Organization, tenant.Project, sessionID)
	if got := pgCode(err); got != "23505" {
		t.Fatalf("second concurrent root run code = %q, want 23505 unique_violation", got)
	}
}

// TestSecondConcurrentRootRunConflicts proves the one-active-root invariant is a DB constraint,
// not an app-code race (spec §22.3): a session holds at most one non-terminal root run, so a
// second concurrent root run for the same session is a unique_violation (23505). The slot frees
// when the live root terminalizes, and it is per session. Mirrors TestActiveAttemptFenceIsUniquePerRun.
func TestSecondConcurrentRootRunConflicts(t *testing.T) {
	cs := openHarness(t)
	pool := cs.Pool()
	ctx := context.Background()
	tenant, sessionID, runID := seedRun(t, pool)

	insertRun := func(session, state string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO runs (id, organization_id, project_id, session_id, state) VALUES ($1, $2, $3, $4, $5)`,
			newID("run"), tenant.Organization, tenant.Project, session, state)
		return err
	}

	// seedRun's run is queued (non-terminal): it holds the session's single root slot. A second
	// concurrent non-terminal root run for the same session is the one-active-root violation.
	if got := pgCode(insertRun(sessionID, "running")); got != "23505" {
		t.Fatalf("second concurrent root run code = %q, want 23505 unique_violation", got)
	}
	// Terminalizing the live root frees the slot: the session's next response may open a root run.
	exec(t, pool, `UPDATE runs SET state='completed' WHERE id=$1`, runID)
	if err := insertRun(sessionID, "queued"); err != nil {
		t.Fatalf("root run after the live one terminalized error = %v", err)
	}
	// The slot is per session: a distinct session's root run is unaffected by the first's.
	otherSession := newID("ses")
	exec(t, pool, `INSERT INTO sessions (id, organization_id, project_id) VALUES ($1, $2, $3)`,
		otherSession, tenant.Organization, tenant.Project)
	if err := insertRun(otherSession, "running"); err != nil {
		t.Fatalf("root run in a distinct session error = %v", err)
	}
}

// TestLateTerminalCannotOverwriteTerminalRow proves the permanent, DB-level class-fix for the
// 2-tx cancel window (spec §22.3): once a response is finalized terminal, a later terminal
// projection loses at the database, because UpdateResponse is conditional (WHERE state NOT IN
// the terminal states). This is the durable form of the e08a898 app-guard — it holds even when
// a process is killed between the run transition and the projection write, so a reclaimed or
// in-flight attempt whose late run.terminal lands after a cancel cannot flip the canceled
// response to completed. FinalizeResponse stays a silent idempotent no-op on the blocked write.
func TestLateTerminalCannotOverwriteTerminalRow(t *testing.T) {
	cs := openHarness(t)
	pool := cs.Pool()
	ctx := context.Background()
	tenant, sessionID, runID := seedRun(t, pool)

	// A response whose run a user cancel terminalized (run.canceled.v1) and whose projection the
	// cancel finalized to canceled — the first, winning terminal write.
	respID := newID("resp")
	exec(t, pool, `INSERT INTO responses (id, organization_id, project_id, session_id, state) VALUES ($1, $2, $3, $4, 'queued')`,
		respID, tenant.Organization, tenant.Project, sessionID)
	exec(t, pool, `UPDATE runs SET state='canceled' WHERE id=$1`, runID)
	canceled, _ := json.Marshal(map[string]any{"output": []any{}, "model": ""})
	if err := cs.FinalizeResponse(ctx, tenant, respID, "canceled", canceled); err != nil {
		t.Fatalf("finalize canceled error = %v", err)
	}

	// The late terminal: an in-flight/reclaimed attempt that finished recovery just after the
	// cancel now finalizes the same response to completed. The conditional UPDATE must drop it.
	completed, _ := json.Marshal(map[string]any{"output": []any{map[string]any{"type": "message", "content": "late"}}, "model": "fake"})
	if err := cs.FinalizeResponse(ctx, tenant, respID, "completed", completed); err != nil {
		t.Fatalf("late finalize returned error = %v, want a silent no-op", err)
	}

	// The canceled terminal stands: the late completed projection lost at the DB level. The
	// canceled projection carries an empty output; the completed one carried a "late" item, so
	// an empty output array proves the completed write never landed (decoded, not byte-compared,
	// because JSONB normalizes key order and spacing on round-trip).
	var state string
	var output []byte
	if err := pool.QueryRow(ctx, `SELECT state, output FROM responses WHERE id=$1`, respID).Scan(&state, &output); err != nil {
		t.Fatalf("read response error = %v", err)
	}
	if state != "canceled" {
		t.Fatalf("response state after late terminal = %q, want canceled (a late completed overwrote the terminal row, §22.3)", state)
	}
	var proj struct {
		Output []any `json:"output"`
	}
	if err := json.Unmarshal(output, &proj); err != nil {
		t.Fatalf("decode response output %s error = %v", output, err)
	}
	if len(proj.Output) != 0 {
		t.Fatalf("response output after late terminal = %s, want the empty canceled projection (the completed write leaked in)", output)
	}
}

func TestMigrationApplyRollbackReapply(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()

	for _, name := range allTables {
		if !tableExists(t, pool, name) {
			t.Fatalf("after apply, table %q is missing", name)
		}
	}

	if err := cs.Rollback(ctx); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	for _, name := range allTables {
		if tableExists(t, pool, name) {
			t.Fatalf("after rollback, table %q still exists", name)
		}
	}

	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	for _, name := range allTables {
		if !tableExists(t, pool, name) {
			t.Fatalf("after reapply, table %q is missing", name)
		}
	}
}

func TestTenantScopeOwnsExecutionRows(t *testing.T) {
	cs := openHarness(t)
	pool := cs.Pool()
	ctx := context.Background()

	tenant, _, _ := seedRun(t, pool)

	// A second tenant, whose project is owned by a different organization.
	otherOrg := newID("org")
	otherProject := newID("prj")
	exec(t, pool, `INSERT INTO organizations (id) VALUES ($1)`, otherOrg)
	exec(t, pool, `INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, otherProject, otherOrg)

	// Claiming another org's project for this org's session is a composite-FK
	// violation: one project (within one organization) owns every session row.
	_, err := pool.Exec(ctx,
		`INSERT INTO sessions (id, organization_id, project_id) VALUES ($1, $2, $3)`,
		newID("ses"), tenant.Organization, otherProject)
	if got := pgCode(err); got != "23503" {
		t.Fatalf("cross-tenant session insert code = %q (%v), want 23503 foreign_key_violation", got, err)
	}

	// A response cannot exist without a project scope at all.
	_, err = pool.Exec(ctx,
		`INSERT INTO responses (id, organization_id, project_id, session_id) VALUES ($1, $2, NULL, $3)`,
		newID("resp"), tenant.Organization, newID("ses"))
	if got := pgCode(err); got != "23502" {
		t.Fatalf("unscoped response insert code = %q (%v), want 23502 not_null_violation", got, err)
	}
}

func TestActiveAttemptFenceIsUniquePerRun(t *testing.T) {
	cs := openHarness(t)
	pool := cs.Pool()
	ctx := context.Background()
	tenant, _, runID := seedRun(t, pool)

	insertAttempt := func(fence int, state string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO attempts (id, organization_id, project_id, run_id, fence, state) VALUES ($1, $2, $3, $4, $5, $6)`,
			newID("att"), tenant.Organization, tenant.Project, runID, fence, state)
		return err
	}

	if err := insertAttempt(1, "active"); err != nil {
		t.Fatalf("first active attempt insert error = %v", err)
	}
	// Only one non-terminal attempt may hold the live fence per run.
	if got := pgCode(insertAttempt(2, "active")); got != "23505" {
		t.Fatalf("second active attempt code = %q, want 23505 unique_violation", got)
	}
	// The fence itself is unique per run even across terminal attempts.
	if got := pgCode(insertAttempt(1, "failed")); got != "23505" {
		t.Fatalf("duplicate fence code = %q, want 23505 unique_violation", got)
	}
	// After the live attempt terminates, a higher fence may take over.
	exec(t, pool, `UPDATE attempts SET state = 'succeeded' WHERE run_id = $1 AND fence = 1`, runID)
	if err := insertAttempt(2, "active"); err != nil {
		t.Fatalf("reclaim attempt insert error = %v", err)
	}
}

func TestIdempotencyScopeKeyUnique(t *testing.T) {
	cs := openHarness(t)
	pool := cs.Pool()
	ctx := context.Background()
	tenant, _, _ := seedRun(t, pool)
	principal := newID("prin")
	exec(t, pool, `INSERT INTO principals (id, organization_id, project_id, kind) VALUES ($1, $2, $3, 'api_key')`,
		principal, tenant.Organization, tenant.Project)

	insert := func(key string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO idempotency_records
			 (organization_id, project_id, principal_id, method, route, idempotency_key, request_hash, status)
			 VALUES ($1, $2, $3, 'POST', '/v1/responses', $4, 'hash', 'completed')`,
			tenant.Organization, tenant.Project, principal, key)
		return err
	}
	if err := insert("key-1"); err != nil {
		t.Fatalf("first idempotency insert error = %v", err)
	}
	if got := pgCode(insert("key-1")); got != "23505" {
		t.Fatalf("duplicate idempotency key code = %q, want 23505 unique_violation", got)
	}
	if err := insert("key-2"); err != nil {
		t.Fatalf("distinct idempotency key insert error = %v", err)
	}
}

func TestUsageDedupeKeyUnique(t *testing.T) {
	cs := openHarness(t)
	pool := cs.Pool()
	ctx := context.Background()
	tenant, _, _ := seedRun(t, pool)

	insert := func(dedupe string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO usage_events (organization_id, project_id, dedupe_key, kind, quantity)
			 VALUES ($1, $2, $3, 'tokens', 100)`,
			tenant.Organization, tenant.Project, dedupe)
		return err
	}
	if err := insert("meter-1"); err != nil {
		t.Fatalf("first usage insert error = %v", err)
	}
	if got := pgCode(insert("meter-1")); got != "23505" {
		t.Fatalf("duplicate usage dedupe code = %q, want 23505 unique_violation", got)
	}
	if err := insert("meter-2"); err != nil {
		t.Fatalf("distinct usage dedupe insert error = %v", err)
	}
}

func TestAuditAppendOnlyToApplicationRole(t *testing.T) {
	cs := openHarness(t)
	pool := cs.Pool()
	ctx := context.Background()
	tenant, _, _ := seedRun(t, pool)

	// Drop to the application role for the duration of this connection.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET ROLE palai_app`); err != nil {
		t.Fatalf("SET ROLE palai_app error = %v", err)
	}
	defer func() { _, _ = conn.Exec(ctx, `RESET ROLE`) }()

	if _, err := conn.Exec(ctx,
		`INSERT INTO audit_events (organization_id, actor, action, outcome) VALUES ($1, 'actor', 'run.create', 'allowed')`,
		tenant.Organization); err != nil {
		t.Fatalf("append audit as palai_app error = %v", err)
	}
	if got := pgCode(mustFail(conn.Exec(ctx, `UPDATE audit_events SET outcome = 'tampered'`))); got != "42501" {
		t.Fatalf("audit UPDATE code = %q, want 42501 insufficient_privilege", got)
	}
	if got := pgCode(mustFail(conn.Exec(ctx, `DELETE FROM audit_events`))); got != "42501" {
		t.Fatalf("audit DELETE code = %q, want 42501 insufficient_privilege", got)
	}
}

func mustFail(_ pgconn.CommandTag, err error) error { return err }
