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

	"github.com/palgroup/palai/storage"
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
	"workspaces", "workspace_allocations", "workspace_leases", "workspace_snapshots",
	"repository_bindings", "preparation_receipts",
	"merge_records",
	"changesets", "changeset_findings",
	"tasks",
	"publications", "approvals",
	"checkpoints", "transcript_boundaries",
	"delivered_messages",
	"host_quarantine",
	"agent_profiles", "agent_revisions", "run_template_revisions",
	"webhook_endpoints", "webhook_deliveries", "delivery_attempts",
	"triggers", "trigger_revisions", "trigger_deliveries",
	"schedules", "schedule_occurrences",
	"tools", "tool_revisions", "tool_set_revisions",
	"remote_tool_operations",
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
	if _, err := pool.Exec(storage.WithSystemScope(context.Background()), sql, args...); err != nil {
		t.Fatalf("exec %q error = %v", sql, err)
	}
}

// execAsOwner runs fixture SQL that the runtime role is deliberately not granted — a mutation of an
// append-only table (audit_events, checkpoints). The system scope clears RLS but not the GRANTs, so
// this steps the connection back off storage.RuntimeRole, exactly as the migration path does.
func execAsOwner(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	ctx := storage.WithSystemScope(context.Background())
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire owner connection: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `RESET ROLE`); err != nil {
		t.Fatalf("reset to owning role: %v", err)
	}
	if _, err := conn.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("exec (owner) %q error = %v", sql, err)
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
	if err := pool.QueryRow(storage.WithSystemScope(context.Background()), `SELECT to_regclass('public.' || $1)::text`, name).Scan(&reg); err != nil {
		t.Fatalf("to_regclass(%s) error = %v", name, err)
	}
	return reg != nil
}

func columnExists(t *testing.T, pool *pgxpool.Pool, table, column string) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(storage.WithSystemScope(context.Background()),
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

	// Clear the version marker so the one-shot backfill runs again on the next Migrate. Written as the
	// owner: migration 000030 (M1) revoked the runtime role's write on schema_migrations — only the
	// RESET-ROLE migration path may touch the ledger now, which is exactly what clearing a marker is.
	execAsOwner(t, pool, `DELETE FROM schema_migrations WHERE version = 3`)
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}

	// The legacy events are now keyed to their session's sole response, so the per-response
	// purge can reach them (the upgrade-boundary gap is closed).
	var keyed int
	if err := pool.QueryRow(storage.WithSystemScope(ctx),
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
	if err := pool.QueryRow(storage.WithSystemScope(context.Background()), `SELECT to_regclass('public.' || $1)::text`, name).Scan(&reg); err != nil {
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

// TestDeliveredMessagesMigration proves 000016 adds the durable delivered-message table (E10 Task 2,
// spec §26.9) idempotently and reverses cleanly: the table, its columns, and its redelivery index
// exist after apply (a re-apply is a clean no-op), are gone after rollback, and return after reapply
// (the 000006/000007 re-run-safety pattern). A row keyed to a real command inserts, and one keyed to
// a missing command is rejected — the FK to commands is the "content ref" the row carries.
func TestDeliveredMessagesMigration(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()

	// Present after apply, and a second Migrate is a clean no-op (CREATE TABLE/INDEX IF NOT EXISTS).
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	if !tableExists(t, pool, "delivered_messages") {
		t.Fatal("after apply, delivered_messages is missing")
	}
	for _, col := range []string{"command_id", "run_id", "boundary_request_id", "applied_sequence", "fold_state"} {
		if !columnExists(t, pool, "delivered_messages", col) {
			t.Fatalf("after apply, delivered_messages.%s is missing", col)
		}
	}
	if !indexExists(t, pool, "delivered_messages_run_boundary_idx") {
		t.Fatal("after apply, delivered_messages_run_boundary_idx is missing")
	}

	// The row references a real command; a row for a missing command is rejected (FK to commands —
	// the content ref). This proves the shape is usable and tenant-safe, not just present.
	tenant, sessionID, runID := seedRun(t, pool)
	cmdID := newID("cmd")
	exec(t, pool,
		`INSERT INTO commands (id, organization_id, project_id, session_id, run_id, kind, delivery, payload, state, applied_sequence)
		 VALUES ($1, $2, $3, $4, $5, 'send_message', 'steer', '{"message":"also do Y"}', 'applied', 7)`,
		cmdID, tenant.Organization, tenant.Project, sessionID, runID)
	exec(t, pool,
		`INSERT INTO delivered_messages (command_id, organization_id, project_id, run_id, boundary_request_id, applied_sequence)
		 VALUES ($1, $2, $3, $4, 'mr_step2', 7)`,
		cmdID, tenant.Organization, tenant.Project, runID)
	if got := pgCode(mustFail(pool.Exec(storage.WithSystemScope(ctx),
		`INSERT INTO delivered_messages (command_id, organization_id, project_id, run_id, applied_sequence)
		 VALUES ('cmd_missing', $1, $2, $3, 1)`,
		tenant.Organization, tenant.Project, runID))); got != "23503" {
		t.Fatalf("delivered_messages for a missing command code = %q, want 23503 foreign_key_violation", got)
	}

	if err := cs.Rollback(ctx); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	if tableExists(t, pool, "delivered_messages") {
		t.Fatal("after rollback, delivered_messages still exists")
	}

	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	if !tableExists(t, pool, "delivered_messages") || !indexExists(t, pool, "delivered_messages_run_boundary_idx") {
		t.Fatal("after reapply, a 000016 object is missing")
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
	if _, err := pool.Exec(storage.WithSystemScope(ctx),
		`INSERT INTO runs (id, organization_id, project_id, session_id, state, parent_run_id, depth)
		 VALUES ($1, $2, $3, $4, 'running', $5, 1)`,
		newID("run"), tenant.Organization, tenant.Project, sessionID, rootRunID); err != nil {
		t.Fatalf("child run in the parent's session error = %v, want admitted (excluded from one-active-root)", err)
	}
	// A second concurrent ROOT run (parent_run_id NULL) for the same session is still the
	// one-active-root violation — the child did not free or fill the root slot.
	_, err := pool.Exec(storage.WithSystemScope(ctx),
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
		_, err := pool.Exec(storage.WithSystemScope(ctx),
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
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT state, output FROM responses WHERE id=$1`, respID).Scan(&state, &output); err != nil {
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

// TestRepositoryBindingsMigration proves 000009 adds its two tables idempotently and reverses
// cleanly: repository_bindings and preparation_receipts exist after apply (a re-apply is a clean
// no-op), are gone after rollback, and return after reapply (spec §30.1/§30.3; the 000007/000008
// re-run-safety pattern).
func TestRepositoryBindingsMigration(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()

	// Present after apply, and a second Migrate is a clean no-op (CREATE TABLE IF NOT EXISTS).
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	for _, name := range []string{"repository_bindings", "preparation_receipts"} {
		if !tableExists(t, pool, name) {
			t.Fatalf("after apply, %s is missing", name)
		}
	}

	if err := cs.Rollback(ctx); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	for _, name := range []string{"repository_bindings", "preparation_receipts"} {
		if tableExists(t, pool, name) {
			t.Fatalf("after rollback, %s still exists", name)
		}
	}

	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	for _, name := range []string{"repository_bindings", "preparation_receipts"} {
		if !tableExists(t, pool, name) {
			t.Fatalf("after reapply, %s is missing", name)
		}
	}
}

// TestMergeRecordsMigration proves 000011 adds its table idempotently and reverses cleanly (spec
// §30.5; the 000009 re-run-safety pattern).
func TestMergeRecordsMigration(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()

	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	if !tableExists(t, pool, "merge_records") {
		t.Fatal("after apply, merge_records is missing")
	}
	if err := cs.Rollback(ctx); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	if tableExists(t, pool, "merge_records") {
		t.Fatal("after rollback, merge_records still exists")
	}
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	if !tableExists(t, pool, "merge_records") {
		t.Fatal("after reapply, merge_records is missing")
	}
}

// TestRecordMergeRoundTrip proves an explicit merge outcome is durably recorded with its source
// child run + conflict paths (spec §30.5, REP-011).
func TestRecordMergeRoundTrip(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()
	tenant, sessionID, parentRun := seedRun(t, pool)
	childRun := newID("run")
	exec(t, pool, `INSERT INTO runs (id, organization_id, project_id, session_id, state, parent_run_id, depth) VALUES ($1,$2,$3,$4,'completed',$5,1)`,
		childRun, tenant.Organization, tenant.Project, sessionID, parentRun)

	if err := cs.RecordMerge(ctx, tenant, coordinator.MergeRecordInput{
		MergeID: newID("mrg"), ParentRunID: parentRun, SourceChildRunID: childRun,
		ChildBranch: "agent/ses/run", Merged: false, ConflictPaths: []string{"f.txt"},
	}); err != nil {
		t.Fatalf("RecordMerge() error = %v", err)
	}

	var merged bool
	var source, conflicts string
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT merged, source_child_run_id, conflict_paths::text FROM merge_records WHERE parent_run_id=$1`, parentRun).
		Scan(&merged, &source, &conflicts); err != nil {
		t.Fatalf("read merge record: %v", err)
	}
	if merged || source != childRun || conflicts != `["f.txt"]` {
		t.Fatalf("merge record = merged:%v source:%s conflicts:%s, want false / %s / [\"f.txt\"]", merged, source, conflicts, childRun)
	}
}

// TestChangesetsMigration proves 000010 adds its tables + the richer §22.6 artifact columns
// idempotently and reverses cleanly (spec §30.6, §22.6; the 000009/000011 re-run-safety pattern).
func TestChangesetsMigration(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()

	// Present after apply, and a second Migrate is a clean no-op (CREATE/ADD COLUMN IF NOT EXISTS).
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	for _, name := range []string{"changesets", "changeset_findings"} {
		if !tableExists(t, pool, name) {
			t.Fatalf("after apply, %s is missing", name)
		}
	}
	// The richer §22.6 artifact columns land here (the T2 base row carried only id/object_key/size/checksum).
	for _, col := range []string{"media_type", "logical_type", "malware_scan_status", "provenance"} {
		if !columnExists(t, pool, "artifacts", col) {
			t.Fatalf("after apply, artifacts.%s is missing", col)
		}
	}

	if err := cs.Rollback(ctx); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	for _, name := range []string{"changesets", "changeset_findings"} {
		if tableExists(t, pool, name) {
			t.Fatalf("after rollback, %s still exists", name)
		}
	}

	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	for _, name := range []string{"changesets", "changeset_findings"} {
		if !tableExists(t, pool, name) {
			t.Fatalf("after reapply, %s is missing", name)
		}
	}
}

// TestRecoveryObjectsMigration proves 000015 adds the durable recovery objects — the checkpoints
// and transcript_boundaries tables plus the workspace_snapshots.boundary_id rider — idempotently
// and reverses cleanly (spec §26.1-26.2, E10 Task 1; the 000008/000014 re-run-safety pattern).
func TestRecoveryObjectsMigration(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()

	// Present after apply, and a second Migrate is a clean no-op (CREATE TABLE / ADD COLUMN IF NOT EXISTS).
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	for _, name := range []string{"checkpoints", "transcript_boundaries"} {
		if !tableExists(t, pool, name) {
			t.Fatalf("after apply, %s is missing", name)
		}
	}
	if !columnExists(t, pool, "workspace_snapshots", "boundary_id") {
		t.Fatal("after apply, workspace_snapshots.boundary_id rider is missing")
	}

	if err := cs.Rollback(ctx); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	for _, name := range []string{"checkpoints", "transcript_boundaries"} {
		if tableExists(t, pool, name) {
			t.Fatalf("after rollback, %s still exists", name)
		}
	}

	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	for _, name := range []string{"checkpoints", "transcript_boundaries"} {
		if !tableExists(t, pool, name) {
			t.Fatalf("after reapply, %s is missing", name)
		}
	}
	if !columnExists(t, pool, "workspace_snapshots", "boundary_id") {
		t.Fatal("after reapply, workspace_snapshots.boundary_id rider is missing")
	}
}

// TestToolCallLedgerMigration proves 000018 adds the tool-call replay-ledger rider columns (E10 Task 7,
// spec §26.6-26.7) idempotently and reverses cleanly: the columns exist after apply (a re-apply is a
// clean no-op), are gone after rollback, and return after reapply (the 000014/000016 re-run-safety
// pattern). A legacy completed row backfills to the 'pure' default — the ledger classification never has
// to backfill a NULL — and an uncertain row with a reconciliation sub-state round-trips, proving the
// columns are usable, not just present.
func TestToolCallLedgerMigration(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()

	// Present after apply, and a second Migrate is a clean no-op (ADD COLUMN IF NOT EXISTS).
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	ledgerCols := []string{"replay_class", "request_hash", "external_idempotency_key", "lease_owner", "reconciliation_state", "commit_boundary"}
	for _, col := range ledgerCols {
		if !columnExists(t, pool, "tool_calls", col) {
			t.Fatalf("after apply, tool_calls.%s is missing", col)
		}
	}

	// A legacy completed row inserted through the pre-000018 column list backfills replay_class to the
	// 'pure' default, so the ledger classification reads a value rather than a NULL.
	tenant, _, runID := seedRun(t, pool)
	legacyID := newID("tcall")
	exec(t, pool,
		`INSERT INTO tool_calls (id, organization_id, project_id, run_id, fence, state, name, arguments, result)
		 VALUES ($1, $2, $3, $4, 3, 'completed', 'add', '{"a":1}', '{"sum":1}')`,
		legacyID, tenant.Organization, tenant.Project, runID)
	var replayClass string
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT replay_class FROM tool_calls WHERE id=$1`, legacyID).Scan(&replayClass); err != nil {
		t.Fatalf("read replay_class error = %v", err)
	}
	if replayClass != "pure" {
		t.Fatalf("legacy row replay_class = %q, want the 'pure' backfill default", replayClass)
	}
	// An uncertain row with a reconciliation sub-state round-trips — the columns carry the §26.7 path.
	uncertainID := newID("tcall")
	exec(t, pool,
		`INSERT INTO tool_calls (id, organization_id, project_id, run_id, fence, state, name, arguments,
		 replay_class, request_hash, external_idempotency_key, lease_owner, reconciliation_state, commit_boundary)
		 VALUES ($1, $2, $3, $4, 4, 'uncertain', 'push', '{}', 'irreversible', 'sha256:abc', 'push:main', '4', 'reconciling', 'mr_step2')`,
		uncertainID, tenant.Organization, tenant.Project, runID)
	var state, reconState, boundary string
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT state, reconciliation_state, commit_boundary FROM tool_calls WHERE id=$1`, uncertainID).
		Scan(&state, &reconState, &boundary); err != nil {
		t.Fatalf("read uncertain row error = %v", err)
	}
	if state != "uncertain" || reconState != "reconciling" || boundary != "mr_step2" {
		t.Fatalf("uncertain row = state:%q recon:%q boundary:%q, want uncertain/reconciling/mr_step2", state, reconState, boundary)
	}

	if err := cs.Rollback(ctx); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	for _, col := range ledgerCols {
		if columnExists(t, pool, "tool_calls", col) {
			t.Fatalf("after rollback, tool_calls.%s still exists", col)
		}
	}

	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	for _, col := range ledgerCols {
		if !columnExists(t, pool, "tool_calls", col) {
			t.Fatalf("after reapply, tool_calls.%s is missing", col)
		}
	}
}

// TestAgentsMigration proves 000019 adds the automation-agent tables — agent_profiles,
// agent_revisions, run_template_revisions — plus the runs.agent_revision_id /
// run_template_revision_id pin riders, idempotently and reversibly (spec §10, §32.2, E11 Task 1;
// the 000015/000018 re-run-safety pattern). A usable-row assert proves the shape: a draft revision
// inserts, the conditional publish flips published_at exactly once (a second publish is a no-op,
// keeping the published row immutable), and a run pinned to a missing revision is FK-rejected.
func TestAgentsMigration(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()

	// Present after apply, and a second Migrate is a clean no-op (CREATE TABLE / ADD COLUMN IF NOT EXISTS).
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	for _, name := range []string{"agent_profiles", "agent_revisions", "run_template_revisions"} {
		if !tableExists(t, pool, name) {
			t.Fatalf("after apply, %s is missing", name)
		}
	}
	for _, col := range []string{"agent_revision_id", "run_template_revision_id"} {
		if !columnExists(t, pool, "runs", col) {
			t.Fatalf("after apply, runs.%s pin rider is missing", col)
		}
	}

	// Usable shape: a profile, a draft revision, publish flips once, a second publish is a no-op.
	tenant, sessionID, runID := seedRun(t, pool)
	profileID, revID := newID("aprof"), newID("arev")
	exec(t, pool, `INSERT INTO agent_profiles (id, organization_id, project_id, name) VALUES ($1,$2,$3,'reviewer')`,
		profileID, tenant.Organization, tenant.Project)
	exec(t, pool, `INSERT INTO agent_revisions (id, organization_id, project_id, profile_id, revision_number, model, tools, instructions)
	               VALUES ($1,$2,$3,$4,1,'model-x','["file"]','be careful')`,
		revID, tenant.Organization, tenant.Project, profileID)

	// The conditional publish flip sets published_at once; a re-run against the now-published row
	// affects zero rows, so a published revision never re-stamps (immutable publish boundary).
	tag, err := pool.Exec(storage.WithSystemScope(ctx), `UPDATE agent_revisions SET published_at = clock_timestamp() WHERE id=$1 AND published_at IS NULL`, revID)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("first publish rows = %d err = %v, want exactly 1", tag.RowsAffected(), err)
	}
	tag2, err := pool.Exec(storage.WithSystemScope(ctx), `UPDATE agent_revisions SET published_at = clock_timestamp() WHERE id=$1 AND published_at IS NULL`, revID)
	if err != nil || tag2.RowsAffected() != 0 {
		t.Fatalf("second publish rows = %d err = %v, want 0 (already published)", tag2.RowsAffected(), err)
	}

	// A run may pin the published revision (rider FK resolves); a pin to a missing revision is rejected.
	exec(t, pool, `UPDATE runs SET agent_revision_id=$1 WHERE id=$2`, revID, runID)
	if got := pgCode(mustFail(pool.Exec(storage.WithSystemScope(ctx), `UPDATE runs SET agent_revision_id='arev_missing' WHERE id=$1`, runID))); got != "23503" {
		t.Fatalf("pin to a missing revision code = %q, want 23503 foreign_key_violation", got)
	}
	_ = sessionID

	if err := cs.Rollback(ctx); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	for _, name := range []string{"agent_profiles", "agent_revisions", "run_template_revisions"} {
		if tableExists(t, pool, name) {
			t.Fatalf("after rollback, %s still exists", name)
		}
	}
	if columnExists(t, pool, "runs", "agent_revision_id") {
		t.Fatal("after rollback, runs.agent_revision_id rider still exists")
	}

	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	for _, name := range []string{"agent_profiles", "agent_revisions", "run_template_revisions"} {
		if !tableExists(t, pool, name) {
			t.Fatalf("after reapply, %s is missing", name)
		}
	}
	if !columnExists(t, pool, "runs", "run_template_revision_id") {
		t.Fatal("after reapply, runs.run_template_revision_id rider is missing")
	}
}

// TestWebhooksMigration proves 000020 adds the outbound-webhook tables plus the events journal_id
// IDENTITY cursor rider (E11 Task 4, spec §21.4-21.6) idempotently and reverses cleanly: the tables +
// the rider column exist after apply (a re-apply is a clean no-op), are gone after rollback, and
// return after reapply (the 000016/000018 re-run-safety pattern). The IDENTITY cursor is monotonic —
// two journal events get strictly increasing journal_ids — and a delivery keyed to a real endpoint
// round-trips while a duplicate (endpoint, event) is rejected (the fan-out dedupe, §21.6).
func TestWebhooksMigration(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()

	// Present after apply, and a second Migrate is a clean no-op (CREATE TABLE / ADD COLUMN IF NOT EXISTS).
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	for _, name := range []string{"webhook_endpoints", "webhook_deliveries", "delivery_attempts"} {
		if !tableExists(t, pool, name) {
			t.Fatalf("after apply, %s is missing", name)
		}
	}
	if !columnExists(t, pool, "events", "journal_id") {
		t.Fatal("after apply, the events.journal_id cursor rider is missing")
	}

	// The IDENTITY cursor is globally monotonic: two appended events get strictly increasing journal_ids.
	tenant, sessionID, _ := seedRun(t, pool)
	var j1, j2 int64
	exec(t, pool, `INSERT INTO events (id, organization_id, project_id, session_id, seq, type) VALUES ($1,$2,$3,$4,1,'run.completed.v1')`,
		newID("evt"), tenant.Organization, tenant.Project, sessionID)
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT max(journal_id) FROM events WHERE session_id=$1`, sessionID).Scan(&j1); err != nil {
		t.Fatalf("read first journal_id error = %v", err)
	}
	exec(t, pool, `INSERT INTO events (id, organization_id, project_id, session_id, seq, type) VALUES ($1,$2,$3,$4,2,'run.failed.v1')`,
		newID("evt"), tenant.Organization, tenant.Project, sessionID)
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT max(journal_id) FROM events WHERE session_id=$1`, sessionID).Scan(&j2); err != nil {
		t.Fatalf("read second journal_id error = %v", err)
	}
	if j2 <= j1 {
		t.Fatalf("journal_id not monotonic: second=%d <= first=%d", j2, j1)
	}

	// A delivery keyed to a real endpoint inserts; a duplicate (endpoint, event) is the fan-out dedupe.
	endpointID := newID("whe")
	exec(t, pool, `INSERT INTO webhook_endpoints (id, organization_id, project_id, url) VALUES ($1,$2,$3,'https://hooks.example.com/x')`,
		endpointID, tenant.Organization, tenant.Project)
	deliveryID := newID("whd")
	exec(t, pool, `INSERT INTO webhook_deliveries (id, organization_id, project_id, endpoint_id, session_id, event_id, event_type) VALUES ($1,$2,$3,$4,$5,'evt_x','run.completed.v1')`,
		deliveryID, tenant.Organization, tenant.Project, endpointID, sessionID)
	if got := pgCode(mustFail(pool.Exec(storage.WithSystemScope(ctx),
		`INSERT INTO webhook_deliveries (id, organization_id, project_id, endpoint_id, session_id, event_id, event_type) VALUES ($1,$2,$3,$4,$5,'evt_x','run.completed.v1')`,
		newID("whd"), tenant.Organization, tenant.Project, endpointID, sessionID))); got != "23505" {
		t.Fatalf("duplicate (endpoint, event) delivery code = %q, want 23505 unique_violation", got)
	}

	if err := cs.Rollback(ctx); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	for _, name := range []string{"webhook_endpoints", "webhook_deliveries", "delivery_attempts"} {
		if tableExists(t, pool, name) {
			t.Fatalf("after rollback, %s still exists", name)
		}
	}
	if columnExists(t, pool, "events", "journal_id") {
		t.Fatal("after rollback, events.journal_id rider still exists")
	}

	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	if !tableExists(t, pool, "webhook_deliveries") || !columnExists(t, pool, "events", "journal_id") {
		t.Fatal("after reapply, a 000020 object is missing")
	}
}

// TestTriggersMigration proves 000021 adds the trigger tables (triggers, immutable trigger_revisions,
// trigger_deliveries) idempotently and reverses cleanly (E11 Task 2, spec §20.2.2). Present after apply
// (a re-apply is a clean no-op — every object IF NOT EXISTS), gone after rollback (children before
// parents), returning after reapply. It also pins the load-bearing constraints: revise = a new
// immutable INSERT keyed UNIQUE(trigger_id, revision_number); the canonical dedupe partial-unique index
// rejects a second live canonical row for the same (trigger, dedupe_key); and version 21 is removed
// from schema_migrations on rollback.
func TestTriggersMigration(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()

	// Present after apply, and a second Migrate is a clean no-op.
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	for _, name := range []string{"triggers", "trigger_revisions", "trigger_deliveries"} {
		if !tableExists(t, pool, name) {
			t.Fatalf("after apply, %s is missing", name)
		}
	}
	// The forward migration records its version (the guarded down.sql DELETEs it before the table drop —
	// exercised, without a partial-rollback helper, by the clean full Rollback + reapply below).
	var version21 int
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT count(*) FROM schema_migrations WHERE version = 21`).Scan(&version21); err != nil {
		t.Fatalf("count version 21 error = %v", err)
	}
	if version21 != 1 {
		t.Fatalf("schema_migrations records version 21 %d times, want 1", version21)
	}

	tenant, _, _ := seedRun(t, pool)

	// A trigger + two revisions: revise is a NEW immutable INSERT, not an in-place UPDATE, keyed by a
	// monotonic revision_number that is UNIQUE per trigger.
	triggerID := newID("trg")
	exec(t, pool, `INSERT INTO triggers (id, organization_id, project_id, name, type) VALUES ($1,$2,$3,'nightly','manual_api')`,
		triggerID, tenant.Organization, tenant.Project)
	rev1 := newID("trev")
	exec(t, pool, `INSERT INTO trigger_revisions (id, organization_id, project_id, trigger_id, revision_number) VALUES ($1,$2,$3,$4,1)`,
		rev1, tenant.Organization, tenant.Project, triggerID)
	if got := pgCode(mustFail(pool.Exec(storage.WithSystemScope(ctx),
		`INSERT INTO trigger_revisions (id, organization_id, project_id, trigger_id, revision_number) VALUES ($1,$2,$3,$4,1)`,
		newID("trev"), tenant.Organization, tenant.Project, triggerID))); got != "23505" {
		t.Fatalf("duplicate revision_number code = %q, want 23505 unique_violation", got)
	}

	// The canonical dedupe index: a live canonical row (duplicate_of IS NULL) for a non-empty dedupe_key
	// is unique per trigger; a second canonical insert with the same key is rejected, while a duplicate
	// row (duplicate_of set) is exempt.
	exec(t, pool, `INSERT INTO trigger_deliveries (id, organization_id, project_id, trigger_id, trigger_revision_id, dedupe_key) VALUES ($1,$2,$3,$4,$5,'k1')`,
		newID("tdel"), tenant.Organization, tenant.Project, triggerID, rev1)
	if got := pgCode(mustFail(pool.Exec(storage.WithSystemScope(ctx),
		`INSERT INTO trigger_deliveries (id, organization_id, project_id, trigger_id, trigger_revision_id, dedupe_key) VALUES ($1,$2,$3,$4,$5,'k1')`,
		newID("tdel"), tenant.Organization, tenant.Project, triggerID, rev1))); got != "23505" {
		t.Fatalf("second live canonical dedupe row code = %q, want 23505 unique_violation", got)
	}

	if err := cs.Rollback(ctx); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	for _, name := range []string{"triggers", "trigger_revisions", "trigger_deliveries"} {
		if tableExists(t, pool, name) {
			t.Fatalf("after rollback, %s still exists", name)
		}
	}

	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	if !tableExists(t, pool, "trigger_deliveries") {
		t.Fatal("after reapply, a 000021 table is missing")
	}
}

// TestMigration22Schedules proves 000022 adds the schedule tables (schedules, schedule_occurrences,
// E11 Task 3, spec §33) idempotently and reverses cleanly: present after apply (a re-apply is a clean
// no-op — every object IF NOT EXISTS), gone after rollback (children before parents), returning after
// reapply. It also pins the load-bearing invariants: version 22 is recorded; the max_catch_up CHECK caps
// catch-up at 100 (uncrossable); and the occurrence UNIQUE(schedule_id, schedule_revision, planned_at)
// rejects a second row for the same (schedule, revision, instant) — the raw exactly-once guarantee the
// deterministic occurrence_id is derived from.
func TestMigration22Schedules(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()

	// Present after apply, and a second Migrate is a clean no-op (CREATE TABLE / INDEX IF NOT EXISTS).
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	for _, name := range []string{"schedules", "schedule_occurrences"} {
		if !tableExists(t, pool, name) {
			t.Fatalf("after apply, %s is missing", name)
		}
	}
	var version22 int
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT count(*) FROM schema_migrations WHERE version = 22`).Scan(&version22); err != nil {
		t.Fatalf("count version 22 error = %v", err)
	}
	if version22 != 1 {
		t.Fatalf("schema_migrations records version 22 %d times, want 1", version22)
	}

	tenant, _, _ := seedRun(t, pool)

	// A trigger the schedule fires, then a schedule pinned to it.
	triggerID := newID("trg")
	exec(t, pool, `INSERT INTO triggers (id, organization_id, project_id, name, type) VALUES ($1,$2,$3,'nightly','cron')`,
		triggerID, tenant.Organization, tenant.Project)
	scheduleID := newID("sch")
	exec(t, pool, `INSERT INTO schedules (id, organization_id, project_id, name, trigger_id, timezone, cron_expr) VALUES ($1,$2,$3,'nightly-cron',$4,'America/New_York','30 2 * * *')`,
		scheduleID, tenant.Organization, tenant.Project, triggerID)

	// The max_catch_up ceiling is a DB CHECK — a value above 100 is rejected (catch_up can never be
	// unbounded, §33.3).
	if got := pgCode(mustFail(pool.Exec(storage.WithSystemScope(ctx), `UPDATE schedules SET max_catch_up = 101 WHERE id=$1`, scheduleID))); got != "23514" {
		t.Fatalf("max_catch_up=101 code = %q, want 23514 check_violation (the cap is uncrossable)", got)
	}

	// The exactly-once invariant: a second occurrence for the same (schedule, revision, planned instant)
	// is a unique_violation — the raw guarantee behind ON CONFLICT DO NOTHING + RowsAffected discipline.
	planned := "2026-07-22T06:30:00Z"
	exec(t, pool, `INSERT INTO schedule_occurrences (occurrence_id, schedule_id, schedule_revision, planned_at) VALUES ($1,$2,1,$3)`,
		newID("occ"), scheduleID, planned)
	if got := pgCode(mustFail(pool.Exec(storage.WithSystemScope(ctx),
		`INSERT INTO schedule_occurrences (occurrence_id, schedule_id, schedule_revision, planned_at) VALUES ($1,$2,1,$3)`,
		newID("occ"), scheduleID, planned))); got != "23505" {
		t.Fatalf("second occurrence for the same (schedule, revision, instant) code = %q, want 23505 unique_violation", got)
	}

	if err := cs.Rollback(ctx); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	for _, name := range []string{"schedules", "schedule_occurrences"} {
		if tableExists(t, pool, name) {
			t.Fatalf("after rollback, %s still exists", name)
		}
	}

	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	if !tableExists(t, pool, "schedule_occurrences") {
		t.Fatal("after reapply, a 000022 table is missing")
	}
}

// TestMigration23InboundTriggerAuthIdempotentAndDown proves 000023 adds the three inbound-auth columns to
// triggers (created_by + inbound_secret_ref + inbound_secret_ref_next, E11 Task 5, spec §20.2.2/§21.7)
// idempotently and reverses cleanly: present after apply (a re-apply is a clean no-op — ADD COLUMN IF NOT
// EXISTS), gone after rollback, returning after reapply. Version 23 is recorded exactly once.
func TestMigration23InboundTriggerAuth(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()

	// Present after apply, and a second Migrate is a clean no-op (ADD COLUMN IF NOT EXISTS).
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	for _, col := range []string{"created_by", "inbound_secret_ref", "inbound_secret_ref_next"} {
		if !columnExists(t, pool, "triggers", col) {
			t.Fatalf("after apply, triggers.%s is missing", col)
		}
	}
	var version23 int
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT count(*) FROM schema_migrations WHERE version = 23`).Scan(&version23); err != nil {
		t.Fatalf("count version 23 error = %v", err)
	}
	if version23 != 1 {
		t.Fatalf("schema_migrations records version 23 %d times, want 1", version23)
	}

	if err := cs.Rollback(ctx); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	for _, col := range []string{"created_by", "inbound_secret_ref", "inbound_secret_ref_next"} {
		if columnExists(t, pool, "triggers", col) {
			t.Fatalf("after rollback, triggers.%s still exists", col)
		}
	}

	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	if !columnExists(t, pool, "triggers", "inbound_secret_ref") {
		t.Fatal("after reapply, a 000023 column is missing")
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
	_, err := pool.Exec(storage.WithSystemScope(ctx),
		`INSERT INTO sessions (id, organization_id, project_id) VALUES ($1, $2, $3)`,
		newID("ses"), tenant.Organization, otherProject)
	if got := pgCode(err); got != "23503" {
		t.Fatalf("cross-tenant session insert code = %q (%v), want 23503 foreign_key_violation", got, err)
	}

	// A response cannot exist without a project scope at all.
	_, err = pool.Exec(storage.WithSystemScope(ctx),
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
		_, err := pool.Exec(storage.WithSystemScope(ctx),
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
		_, err := pool.Exec(storage.WithSystemScope(ctx),
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
		_, err := pool.Exec(storage.WithSystemScope(ctx),
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
	// The connection is acquired under the system scope because this test drops to the runtime role
	// BY HAND to prove the append-only GRANTs; without a scope the tenant policies would deny the
	// insert first and the grant assertion would never be reached.
	ctx := storage.WithSystemScope(context.Background())
	tenant, _, _ := seedRun(t, pool)

	// Drop to the application role for the duration of this connection.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(storage.WithSystemScope(ctx), `SET ROLE palai_app`); err != nil {
		t.Fatalf("SET ROLE palai_app error = %v", err)
	}
	defer func() { _, _ = conn.Exec(storage.WithSystemScope(ctx), `RESET ROLE`) }()

	if _, err := conn.Exec(storage.WithSystemScope(ctx),
		`INSERT INTO audit_events (organization_id, actor, action, outcome) VALUES ($1, 'actor', 'run.create', 'allowed')`,
		tenant.Organization); err != nil {
		t.Fatalf("append audit as palai_app error = %v", err)
	}
	if got := pgCode(mustFail(conn.Exec(storage.WithSystemScope(ctx), `UPDATE audit_events SET outcome = 'tampered'`))); got != "42501" {
		t.Fatalf("audit UPDATE code = %q, want 42501 insufficient_privilege", got)
	}
	if got := pgCode(mustFail(conn.Exec(storage.WithSystemScope(ctx), `DELETE FROM audit_events`))); got != "42501" {
		t.Fatalf("audit DELETE code = %q, want 42501 insufficient_privilege", got)
	}
}

// TestMigration25RemoteTools proves 000025 adds the remote_tool_operations table (E12 Task 4, spec
// §28.24-28.25) idempotently and reverses cleanly: present after apply (a re-apply is a clean no-op —
// every object IF NOT EXISTS), gone after rollback, returning after reapply. Version 25 is recorded
// exactly once. It also pins the load-bearing invariants: the partial-unique index rejects a SECOND
// pending row for the same tool_call (a duplicate live invoke can never open two operations) while a
// resolved (completed) row lets a fresh pending one open. tool_call_id is a soft correlation key (NOT an
// FK): the operation opens before the invoke, before a pure/idempotent tool's tool_calls row is committed,
// so a row for a not-yet-committed call inserts fine.
func TestMigration25RemoteTools(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()

	// Present after apply, and a second Migrate is a clean no-op (CREATE TABLE / INDEX IF NOT EXISTS).
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	if !tableExists(t, pool, "remote_tool_operations") {
		t.Fatal("after apply, remote_tool_operations is missing")
	}
	if !indexExists(t, pool, "remote_tool_operations_one_pending") {
		t.Fatal("after apply, remote_tool_operations_one_pending is missing")
	}
	var version25 int
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT count(*) FROM schema_migrations WHERE version = 25`).Scan(&version25); err != nil {
		t.Fatalf("count version 25 error = %v", err)
	}
	if version25 != 1 {
		t.Fatalf("schema_migrations records version 25 %d times, want 1", version25)
	}

	tenant, _, _ := seedRun(t, pool)
	callID := newID("tcall") // a correlation key; no tool_calls row need exist (no FK)

	openOp := func(id, call, state string) error {
		_, err := pool.Exec(storage.WithSystemScope(ctx),
			`INSERT INTO remote_tool_operations (id, organization_id, project_id, tool_call_id, secret_ref, callback_token_hash, deadline, state, fence)
			 VALUES ($1, $2, $3, $4, 'sig-ref', 'tokenhash', clock_timestamp() + interval '30 seconds', $5, 5)`,
			id, tenant.Organization, tenant.Project, call, state)
		return err
	}
	if err := openOp(newID("rop"), callID, "pending"); err != nil {
		t.Fatalf("open pending operation error = %v", err)
	}
	// A second PENDING row for the same tool_call is rejected — a duplicate live invoke can never open two.
	if got := pgCode(openOp(newID("rop"), callID, "pending")); got != "23505" {
		t.Fatalf("second pending operation code = %q, want 23505 unique_violation (partial-unique on pending)", got)
	}
	// A resolved (completed) row for the same call is allowed (the partial-unique only indexes pending).
	if err := openOp(newID("rop"), callID, "completed"); err != nil {
		t.Fatalf("completed operation alongside a resolved one error = %v", err)
	}
	// tool_call_id is a soft correlation key (no FK): an operation for a not-yet-committed call inserts —
	// the executor opens it BEFORE the invoke, before a pure/idempotent tool's tool_calls row is committed.
	if err := openOp(newID("rop"), newID("tcall"), "pending"); err != nil {
		t.Fatalf("operation for an uncommitted tool_call error = %v, want accepted (no FK)", err)
	}

	if err := cs.Rollback(ctx); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	if tableExists(t, pool, "remote_tool_operations") {
		t.Fatal("after rollback, remote_tool_operations still exists")
	}

	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	if !tableExists(t, pool, "remote_tool_operations") || !indexExists(t, pool, "remote_tool_operations_one_pending") {
		t.Fatal("after reapply, a 000025 object is missing")
	}
}

// TestMigration28Hooks proves 000028 adds the hooks registry (E12 Task 8, spec §28.17, TOL-012)
// idempotently and reverses cleanly: the hooks table + its order index are present after apply (a re-apply
// is a clean no-op — every object IF NOT EXISTS), gone after rollback, returning after reapply. Version 28
// is recorded exactly once. It pins the load-bearing invariant — a duplicate hook name in one project is
// rejected (tenant-scoped unique, the admin management key) — and confirms there is NO CHECK on hook_point /
// category / executor (an out-of-matrix value is accepted at the SQL layer, enforced in app code instead,
// the 000024/000026 pattern).
func TestMigration28Hooks(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()

	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	if !tableExists(t, pool, "hooks") {
		t.Fatal("after apply, hooks is missing")
	}
	if !indexExists(t, pool, "hooks_point_order_idx") {
		t.Fatal("after apply, hooks_point_order_idx is missing")
	}
	var version28 int
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT count(*) FROM schema_migrations WHERE version = 28`).Scan(&version28); err != nil {
		t.Fatalf("count version 28 error = %v", err)
	}
	if version28 != 1 {
		t.Fatalf("schema_migrations records version 28 %d times, want 1", version28)
	}

	tenant, _, _ := seedRun(t, pool)
	insertHook := func(id, name, point, category, executor string) error {
		_, err := pool.Exec(storage.WithSystemScope(ctx),
			`INSERT INTO hooks (id, organization_id, project_id, name, hook_point, category, executor, config)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,'{}'::jsonb)`,
			id, tenant.Organization, tenant.Project, name, point, category, executor)
		return err
	}
	if err := insertHook(newID("hook"), "guard", "before_tool", "policy", "platform_inline"); err != nil {
		t.Fatalf("insert hook error = %v", err)
	}
	// A duplicate hook name in the same project is rejected (tenant-scoped unique).
	if got := pgCode(insertHook(newID("hook"), "guard", "after_tool", "observer", "remote_http")); got != "23505" {
		t.Fatalf("duplicate hook name code = %q, want 23505 unique_violation", got)
	}
	// No CHECK on hook_point/category/executor — an out-of-matrix combination inserts at the SQL layer (app
	// code is the closed-set + matrix gate). A distinct name so this is not the unique reject.
	if err := insertHook(newID("hook"), "raw", "no_such_point", "no_such_category", "no_such_executor"); err != nil {
		t.Fatalf("uncheck-constrained insert error = %v, want accepted (no SQL CHECK, app-validated)", err)
	}

	if err := cs.Rollback(ctx); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	if tableExists(t, pool, "hooks") {
		t.Fatal("after rollback, hooks still exists")
	}

	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	if !tableExists(t, pool, "hooks") || !indexExists(t, pool, "hooks_point_order_idx") {
		t.Fatal("after reapply, a 000028 object is missing")
	}
}

// TestMigration30APIKeyScope proves 000030 (E13 Task 2) adds the api_keys.scopes / expires_at columns
// idempotently and reverses cleanly, keeps api_keys under RLS (ENABLE+FORCE), records version 30 exactly
// once, and lands the two least-privilege hardening steps: M1 revokes the runtime role's WRITE on the
// migration ledger while retaining its SELECT, and M2's guarded role-membership grant leaves SET ROLE
// working (a superuser compose URL is a no-op branch, but the app pool still switches roles).
func TestMigration30APIKeyScope(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()

	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	if !columnExists(t, pool, "api_keys", "scopes") || !columnExists(t, pool, "api_keys", "expires_at") {
		t.Fatal("after apply, an api_keys provisioning column is missing")
	}

	// api_keys stays a tenant table under RLS (ENABLE + FORCE) — the corpus regression relies on it.
	var enabled, forced bool
	if err := pool.QueryRow(storage.WithSystemScope(ctx),
		`SELECT c.relrowsecurity, c.relforcerowsecurity FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'public' AND c.relname = 'api_keys'`).Scan(&enabled, &forced); err != nil {
		t.Fatalf("read api_keys RLS attributes: %v", err)
	}
	if !enabled || !forced {
		t.Fatalf("api_keys row security enabled=%v forced=%v, want both true", enabled, forced)
	}

	var version30 int
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT count(*) FROM schema_migrations WHERE version = 30`).Scan(&version30); err != nil {
		t.Fatalf("count version 30 error = %v", err)
	}
	if version30 != 1 {
		t.Fatalf("schema_migrations records version 30 %d times, want 1", version30)
	}

	// M1: the runtime role may READ the ledger but not write it.
	assertPriv := func(priv string, want bool) {
		var got bool
		if err := pool.QueryRow(storage.WithSystemScope(ctx),
			`SELECT has_table_privilege('palai_app', 'schema_migrations', $1)`, priv).Scan(&got); err != nil {
			t.Fatalf("has_table_privilege(%s) error = %v", priv, err)
		}
		if got != want {
			t.Fatalf("palai_app %s on schema_migrations = %v, want %v (M1)", priv, got, want)
		}
	}
	assertPriv("SELECT", true)
	assertPriv("INSERT", false)
	assertPriv("UPDATE", false)
	assertPriv("DELETE", false)

	// A key past its expires_at is invisible to VerifyAPIKey (expiry enforced at verify time). Seed an
	// expired and a live key on one tenant and confirm only the live one resolves.
	tenant, _, _ := seedRun(t, pool)
	prin := newID("prin")
	exec(t, pool, `INSERT INTO principals (id, organization_id, project_id, kind) VALUES ($1,$2,$3,'service')`,
		prin, tenant.Organization, tenant.Project)
	liveTok, expTok := newID("sk"), newID("sk")
	exec(t, pool, `INSERT INTO api_keys (id, organization_id, project_id, principal_id, key_hash, expires_at)
		VALUES ($1,$2,$3,$4,$5, now() + interval '1 hour')`,
		newID("key"), tenant.Organization, tenant.Project, prin, coordinator.HashAPIKey(liveTok))
	exec(t, pool, `INSERT INTO api_keys (id, organization_id, project_id, principal_id, key_hash, expires_at)
		VALUES ($1,$2,$3,$4,$5, now() - interval '1 hour')`,
		newID("key"), tenant.Organization, tenant.Project, prin, coordinator.HashAPIKey(expTok))
	if _, err := cs.VerifyAPIKey(ctx, liveTok); err != nil {
		t.Fatalf("VerifyAPIKey(live key) error = %v, want it to resolve", err)
	}
	if _, err := cs.VerifyAPIKey(ctx, expTok); !errors.Is(err, coordinator.ErrInvalidToken) {
		t.Fatalf("VerifyAPIKey(expired key) error = %v, want ErrInvalidToken (expiry enforced)", err)
	}

	// Reverse: the whole chain drops (api_keys with it), version 30 is removed, and a reapply restores the
	// columns — the guarded down.sql is valid SQL (a broken one would fail this Rollback).
	if err := cs.Rollback(ctx); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	if tableExists(t, pool, "api_keys") {
		t.Fatal("after full rollback, api_keys still exists")
	}
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	if !columnExists(t, pool, "api_keys", "scopes") || !columnExists(t, pool, "api_keys", "expires_at") {
		t.Fatal("after reapply, an api_keys provisioning column is missing")
	}
}

func mustFail(_ pgconn.CommandTag, err error) error { return err }

// TestMigration27Skills proves 000027 adds the skills registry (E12 Task 7, spec §28.15-28.16, TOL-011)
// idempotently and reverses cleanly: the skills + skill_revisions tables and the runs.skill_pins rider
// are present after apply (a re-apply is a clean no-op — every object IF NOT EXISTS), gone after rollback,
// returning after reapply. Version 27 is recorded exactly once. It pins the load-bearing invariants: a
// duplicate skill name in one project is rejected, a duplicate (skill_id, revision_number) is rejected,
// and the state CHECK rejects a value outside quarantined|approved|enabled.
func TestMigration27Skills(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()

	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	if !tableExists(t, pool, "skills") || !tableExists(t, pool, "skill_revisions") {
		t.Fatal("after apply, a 000027 table is missing")
	}
	if !columnExists(t, pool, "runs", "skill_pins") {
		t.Fatal("after apply, runs.skill_pins is missing")
	}
	var version27 int
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT count(*) FROM schema_migrations WHERE version = 27`).Scan(&version27); err != nil {
		t.Fatalf("count version 27 error = %v", err)
	}
	if version27 != 1 {
		t.Fatalf("schema_migrations records version 27 %d times, want 1", version27)
	}

	tenant, _, _ := seedRun(t, pool)
	skillID := newID("skill")
	if _, err := pool.Exec(storage.WithSystemScope(ctx), `INSERT INTO skills (id, organization_id, project_id, name) VALUES ($1,$2,$3,'commit')`,
		skillID, tenant.Organization, tenant.Project); err != nil {
		t.Fatalf("insert skill error = %v", err)
	}
	// A duplicate skill name in the same project is rejected (tenant-scoped unique).
	if got := pgCode(mustFail(pool.Exec(storage.WithSystemScope(ctx), `INSERT INTO skills (id, organization_id, project_id, name) VALUES ($1,$2,$3,'commit')`,
		newID("skill"), tenant.Organization, tenant.Project))); got != "23505" {
		t.Fatalf("duplicate skill name code = %q, want 23505 unique_violation", got)
	}

	insertRev := func(id string, revNo int, state string) error {
		_, err := pool.Exec(storage.WithSystemScope(ctx),
			`INSERT INTO skill_revisions (id, organization_id, project_id, skill_id, revision_number, digest, state, archive)
			 VALUES ($1,$2,$3,$4,$5,'sha256:x',$6,'\x00')`,
			id, tenant.Organization, tenant.Project, skillID, revNo, state)
		return err
	}
	if err := insertRev(newID("skillrev"), 1, "quarantined"); err != nil {
		t.Fatalf("insert revision error = %v", err)
	}
	// A duplicate (skill_id, revision_number) is rejected.
	if got := pgCode(insertRev(newID("skillrev"), 1, "approved")); got != "23505" {
		t.Fatalf("duplicate revision number code = %q, want 23505 unique_violation", got)
	}
	// The state CHECK rejects a value outside the closed set.
	if got := pgCode(insertRev(newID("skillrev"), 2, "bogus")); got != "23514" {
		t.Fatalf("invalid state code = %q, want 23514 check_violation", got)
	}

	if err := cs.Rollback(ctx); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	if tableExists(t, pool, "skills") || tableExists(t, pool, "skill_revisions") {
		t.Fatal("after rollback, a 000027 table still exists")
	}
	if columnExists(t, pool, "runs", "skill_pins") {
		t.Fatal("after rollback, runs.skill_pins still exists")
	}

	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	if !tableExists(t, pool, "skills") || !tableExists(t, pool, "skill_revisions") || !columnExists(t, pool, "runs", "skill_pins") {
		t.Fatal("after reapply, a 000027 object is missing")
	}
}
