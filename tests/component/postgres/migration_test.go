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
	"session_sequences", "events",
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
