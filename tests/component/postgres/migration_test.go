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
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/packages/coordinator"

	"github.com/palgroup/palai/storage"
)

// THE `TestMigrationNN…` NAMES IN THIS FILE ARE HISTORY, NOT A CLAIM THAT MIGRATION NN EXISTS. The chain
// was squashed from sixty-seven files to a two-link baseline on 2026-08-04, so there is no 000037 and no
// 000045; each name still records which migration first introduced the property its test pins, which is
// the only thing those numbers were ever good for here. The properties themselves are unchanged and are
// asserted against the schema the baseline builds.
//
// THEY ARE NOT RENAMED, AND THE REASON IS MECHANICAL RATHER THAN SENTIMENTAL: scripts/test/component
// selects this tier with a `-run` allow-list of literal test names, so a rename that missed that list
// would not fail — it would make the test silently stop running, which is the exact defect this tree has
// found seven times. A rename here is a rename in two places or it is a deletion in disguise.

// allTables is every relation the core migration must create (brief Step 3).
//
// A TABLE LEFT THIS LIST WITH A.2 TASK 6 — the one that carried the tenant boundary above the project.
// The removal is recorded here rather than left silent because this list is read in BOTH directions —
// every table must exist after apply AND be gone after rollback — so a removal that was a mistake would
// look exactly like a removal that was a decision.
var allTables = []string{
	"projects", "principals", "api_keys",
	"idempotency_records",
	"sessions", "responses", "messages", "runs", "attempts",
	"session_sequences", "events", "commands",
	"config_revisions",
	"durable_jobs", "job_attempts", "outbox", "inbox",
	// The runner tables 000001 created as empty skeletons; E24 T1's migration 000045 gives the first two
	// a shape (a pool has a posture, a runner has a server-minted identity) and leaves runner_leases as
	// it found it. They have been listed here since the beginning, which is why 000045 creates NEITHER.
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
	"audit_events",
	// usage_events is intentionally absent: 000034 contracts it away (superseded by usage_ledger in
	// 000032). schema_revisions is the E15 T1 migration journal 000033 creates.
	"schema_revisions",
	// The E17 T7 queue-adapter tables (000037): the durable consumer queue, its append-only idempotency
	// ledger, and the outbound result-delivery outbox.
	"queue_connections", "queue_messages", "queue_effect_receipts", "queue_deliveries",
	// The E17 T2 A2A server-projection tables (000038): the published Agent Card projection and the
	// external A2A task/context <-> canonical run/session bridge.
	"a2a_interfaces", "a2a_task_refs",
	// The E17 T3 A2A client-registration table (000039): a registered outbound remote A2A agent's trust
	// envelope (card/endpoint, negotiated version, auth secret_ref handle, allowlists, timeout pins).
	"a2a_remote_agents",
	// The E17 T9 CapabilityWorker contract tables (000040): the enrolled-worker registry and the
	// APPEND-ONLY job journal (self-re-asserting REVOKE).
	"capability_workers", "capability_jobs",
	// The E19 Slack tables: the workspace binding registry and the thread<->session correlation (000035),
	// and the return leg's order-to-post outbox (000041). The first two were never registered here; a table
	// this ledger does not name is a table its drop can go unnoticed.
	"slack_connections", "slack_thread_sessions", "slack_reply_deliveries",
	// The E20 turn handle (000042): which turn a Slack message became, so an edit can supersede it and a
	// deletion can retract it.
	"slack_message_turns",
	// The E23 T1 approval delivery (000044 R4): the order to POST one approval question, keyed
	// UNIQUE (approval_id) because a single run can owe a human several separate answers.
	"slack_approval_deliveries",
	// The E24 T1 runner-fleet tables (000045): the hashed per-pool enrolment credential and the
	// APPEND-ONLY issuance journal (self-re-asserting REVOKE). These two ARE new; runner_pools and
	// runners above are not.
	"runner_pool_keys", "runner_enrollments",
	// The E25 T3 environment tables (000046): the grouping identity and the key MEMBERSHIP rows. Neither
	// holds a credential — the values are secret_refs versions under the derived name
	// `env:<environment_id>:<key>` — which is why there is no third table here.
	"environments", "environment_values",
	// The E26 T1 background task table (000047): one row per PROCESS a run owns, which is a different
	// thing from the tool_calls row that spawned it — the process outlives that row, which is exactly why
	// it needs one of its own.
	"background_tasks",
	// The E29 desired-configuration journal: what this MACHINE should be running with, appended one
	// revision at a time by the admin panel and read by the next bring-up. It carries NO tenant column at
	// all and that absence is deliberate — four of its writable settings are the admission bounds that
	// exist to hold a tenant, and a tenant-scoped home for them would let a tenant raise its own limit —
	// so it is also a BY-NAME entry in tests/security/tenancy's nonTenantTables, which is where a reader
	// looking for "why is this one outside RLS" will look.
	"deployment_desired",
	// The kind-agnostic bot registry (2026-08-03 plan Task 4). Referenced by NO other
	// table — a later session.bot_id column carries this row's id as a plain opaque string, on purpose:
	// the control plane must never learn what a bot IS, and an FK would be that knowledge.
	"integration_bots",
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
	tenant := coordinator.Tenant{Project: newID("prj")}
	sessionID := newID("ses")
	runID := newID("run")
	exec(t, pool, `INSERT INTO projects (id) VALUES ($1)`, tenant.Project)
	exec(t, pool, `INSERT INTO sessions (id, project_id) VALUES ($1, $2)`,
		sessionID, tenant.Project)
	exec(t, pool, `INSERT INTO runs (id, project_id, session_id) VALUES ($1, $2, $3)`,
		runID, tenant.Project, sessionID)
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

// TestSessionChainingMigrationBackfillsPreexistingEvents was REMOVED when the chain was squashed to a
// baseline on 2026-08-04, and what it proved is worth recording rather than deleting in silence.
//
// It drove the one-shot backfill in what was then 000003: events written before that migration carried a
// NULL response_id the per-response retention scrub could not reach, so the migration keyed each session's
// legacy events to its sole response and closed the upgrade-boundary gap. The test cleared the version
// marker to make the marker-gated backfill run again, which drove the real migration path rather than a
// copy of it.
//
// THE SUBJECT IS GONE, NOT THE COVERAGE OF A LIVE RISK. A backfill exists to repair rows written before a
// column did; the baseline creates events.response_id with the table, so on every database this chain can
// now produce there has never been an event that predates it. There is no upgrade boundary left inside the
// chain for a backfill to sit on.
//
// WHAT THIS DOES NOT COVER, AND DID NOT COVER BEFORE: an event written with a NULL response_id by ordinary
// code rather than by an upgrade. That is the retention suite's question, not a migration's. If a future
// migration ever backfills again, restore this test's shape — clear the marker, re-Migrate, count — rather
// than asserting the backfill's SQL by reading it.

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
		`INSERT INTO commands (id, project_id, session_id, run_id, kind, delivery, payload, state, applied_sequence)
		 VALUES ($1, $2, $3, $4, 'send_message', 'steer', '{"message":"also do Y"}', 'applied', 7)`,
		cmdID, tenant.Project, sessionID, runID)
	exec(t, pool,
		`INSERT INTO delivered_messages (command_id, project_id, run_id, boundary_request_id, applied_sequence)
		 VALUES ($1, $2, $3, 'mr_step2', 7)`,
		cmdID, tenant.Project, runID)
	if got := pgCode(mustFail(pool.Exec(storage.WithSystemScope(ctx),
		`INSERT INTO delivered_messages (command_id, project_id, run_id, applied_sequence)
		 VALUES ('cmd_missing', $1, $2, 1)`,
		tenant.Project, runID))); got != "23503" {
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
		`INSERT INTO runs (id, project_id, session_id, state, parent_run_id, depth)
		 VALUES ($1, $2, $3, 'running', $4, 1)`,
		newID("run"), tenant.Project, sessionID, rootRunID); err != nil {
		t.Fatalf("child run in the parent's session error = %v, want admitted (excluded from one-active-root)", err)
	}
	// A second concurrent ROOT run (parent_run_id NULL) for the same session is still the
	// one-active-root violation — the child did not free or fill the root slot.
	_, err := pool.Exec(storage.WithSystemScope(ctx),
		`INSERT INTO runs (id, project_id, session_id, state) VALUES ($1, $2, $3, 'running')`,
		newID("run"), tenant.Project, sessionID)
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
			`INSERT INTO runs (id, project_id, session_id, state) VALUES ($1, $2, $3, $4)`,
			newID("run"), tenant.Project, session, state)
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
	exec(t, pool, `INSERT INTO sessions (id, project_id) VALUES ($1, $2)`,
		otherSession, tenant.Project)
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
	exec(t, pool, `INSERT INTO responses (id, project_id, session_id, state) VALUES ($1, $2, $3, 'queued')`,
		respID, tenant.Project, sessionID)
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
	exec(t, pool, `INSERT INTO runs (id, project_id, session_id, state, parent_run_id, depth) VALUES ($1,$2,$3,'completed',$4,1)`,
		childRun, tenant.Project, sessionID, parentRun)

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
		`INSERT INTO tool_calls (id, project_id, run_id, fence, state, name, arguments, result)
		 VALUES ($1, $2, $3, 3, 'completed', 'add', '{"a":1}', '{"sum":1}')`,
		legacyID, tenant.Project, runID)
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
		`INSERT INTO tool_calls (id, project_id, run_id, fence, state, name, arguments, replay_class, request_hash, external_idempotency_key, lease_owner, reconciliation_state, commit_boundary)
		 VALUES ($1, $2, $3, 4, 'uncertain', 'push', '{}', 'irreversible', 'sha256:abc', 'push:main', '4', 'reconciling', 'mr_step2')`,
		uncertainID, tenant.Project, runID)
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
	exec(t, pool, `INSERT INTO agent_profiles (id, project_id, name) VALUES ($1,$2,'reviewer')`,
		profileID, tenant.Project)
	exec(t, pool, `INSERT INTO agent_revisions (id, project_id, profile_id, revision_number, model, tools, instructions)
	               VALUES ($1,$2,$3,1,'model-x','["file"]','be careful')`,
		revID, tenant.Project, profileID)

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
	exec(t, pool, `INSERT INTO events (id, project_id, session_id, seq, type) VALUES ($1,$2,$3,1,'run.completed.v1')`,
		newID("evt"), tenant.Project, sessionID)
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT max(journal_id) FROM events WHERE session_id=$1`, sessionID).Scan(&j1); err != nil {
		t.Fatalf("read first journal_id error = %v", err)
	}
	exec(t, pool, `INSERT INTO events (id, project_id, session_id, seq, type) VALUES ($1,$2,$3,2,'run.failed.v1')`,
		newID("evt"), tenant.Project, sessionID)
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT max(journal_id) FROM events WHERE session_id=$1`, sessionID).Scan(&j2); err != nil {
		t.Fatalf("read second journal_id error = %v", err)
	}
	if j2 <= j1 {
		t.Fatalf("journal_id not monotonic: second=%d <= first=%d", j2, j1)
	}

	// A delivery keyed to a real endpoint inserts; a duplicate (endpoint, event) is the fan-out dedupe.
	endpointID := newID("whe")
	exec(t, pool, `INSERT INTO webhook_endpoints (id, project_id, url) VALUES ($1,$2,'https://hooks.example.com/x')`,
		endpointID, tenant.Project)
	deliveryID := newID("whd")
	exec(t, pool, `INSERT INTO webhook_deliveries (id, project_id, endpoint_id, session_id, event_id, event_type) VALUES ($1,$2,$3,$4,'evt_x','run.completed.v1')`,
		deliveryID, tenant.Project, endpointID, sessionID)
	if got := pgCode(mustFail(pool.Exec(storage.WithSystemScope(ctx),
		`INSERT INTO webhook_deliveries (id, project_id, endpoint_id, session_id, event_id, event_type) VALUES ($1,$2,$3,$4,'evt_x','run.completed.v1')`,
		newID("whd"), tenant.Project, endpointID, sessionID))); got != "23505" {
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

	tenant, _, _ := seedRun(t, pool)

	// A trigger + two revisions: revise is a NEW immutable INSERT, not an in-place UPDATE, keyed by a
	// monotonic revision_number that is UNIQUE per trigger.
	triggerID := newID("trg")
	exec(t, pool, `INSERT INTO triggers (id, project_id, name, type) VALUES ($1,$2,'nightly','manual_api')`,
		triggerID, tenant.Project)
	rev1 := newID("trev")
	exec(t, pool, `INSERT INTO trigger_revisions (id, project_id, trigger_id, revision_number) VALUES ($1,$2,$3,1)`,
		rev1, tenant.Project, triggerID)
	if got := pgCode(mustFail(pool.Exec(storage.WithSystemScope(ctx),
		`INSERT INTO trigger_revisions (id, project_id, trigger_id, revision_number) VALUES ($1,$2,$3,1)`,
		newID("trev"), tenant.Project, triggerID))); got != "23505" {
		t.Fatalf("duplicate revision_number code = %q, want 23505 unique_violation", got)
	}

	// The canonical dedupe index: a live canonical row (duplicate_of IS NULL) for a non-empty dedupe_key
	// is unique per trigger; a second canonical insert with the same key is rejected, while a duplicate
	// row (duplicate_of set) is exempt.
	exec(t, pool, `INSERT INTO trigger_deliveries (id, project_id, trigger_id, trigger_revision_id, dedupe_key) VALUES ($1,$2,$3,$4,'k1')`,
		newID("tdel"), tenant.Project, triggerID, rev1)
	if got := pgCode(mustFail(pool.Exec(storage.WithSystemScope(ctx),
		`INSERT INTO trigger_deliveries (id, project_id, trigger_id, trigger_revision_id, dedupe_key) VALUES ($1,$2,$3,$4,'k1')`,
		newID("tdel"), tenant.Project, triggerID, rev1))); got != "23505" {
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

	tenant, _, _ := seedRun(t, pool)

	// A trigger the schedule fires, then a schedule pinned to it.
	triggerID := newID("trg")
	exec(t, pool, `INSERT INTO triggers (id, project_id, name, type) VALUES ($1,$2,'nightly','cron')`,
		triggerID, tenant.Project)
	scheduleID := newID("sch")
	exec(t, pool, `INSERT INTO schedules (id, project_id, name, trigger_id, timezone, cron_expr) VALUES ($1,$2,'nightly-cron',$3,'America/New_York','30 2 * * *')`,
		scheduleID, tenant.Project, triggerID)

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

	seedRun(t, pool)

	// A session's project must EXIST. This arm used to make a different and stronger claim — it seeded a
	// second tenant ABOVE the project, gave it a project, and required that pairing one tenant's id with
	// the other's project be refused — and A.2 Task 6 is what took the strength away, deliberately: the
	// composite foreign key that enforced the pairing is rebuilt on the project alone, because with that
	// tenant gone there is no second member for the pair to agree with. What survives is the half
	// that still means something, and it is asserted against an id no projects row carries so the check
	// cannot pass on a technicality: drop the foreign key entirely and this insert succeeds.
	_, err := pool.Exec(storage.WithSystemScope(ctx),
		`INSERT INTO sessions (id, project_id) VALUES ($1, $2)`,
		newID("ses"), newID("prj"))
	if got := pgCode(err); got != "23503" {
		t.Fatalf("session insert naming an absent project code = %q (%v), want 23503 foreign_key_violation", got, err)
	}

	// A response cannot exist without a project scope at all.
	_, err = pool.Exec(storage.WithSystemScope(ctx),
		`INSERT INTO responses (id, project_id, session_id) VALUES ($1, NULL, $2)`,
		newID("resp"), newID("ses"))
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
			`INSERT INTO attempts (id, project_id, run_id, fence, state) VALUES ($1, $2, $3, $4, $5)`,
			newID("att"), tenant.Project, runID, fence, state)
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
	exec(t, pool, `INSERT INTO principals (id, project_id, kind) VALUES ($1, $2, 'api_key')`,
		principal, tenant.Project)

	insert := func(key string) error {
		_, err := pool.Exec(storage.WithSystemScope(ctx),
			`INSERT INTO idempotency_records
			 (project_id, principal_id, method, route, idempotency_key, request_hash, status)
			 VALUES ($1, $2, 'POST', '/v1/responses', $3, 'hash', 'completed')`,
			tenant.Project, principal, key)
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

// TestUsageDedupeKeyUnique was removed in E15 T1: 000034 contracts away usage_events (superseded by
// usage_ledger in 000032, which carries its own dedupe uniqueness proven in usage_ledger_test.go). The
// dedupe guarantee this test asserted now lives on the successor table.

func TestAuditAppendOnlyToApplicationRole(t *testing.T) {
	cs := openHarness(t)
	pool := cs.Pool()
	// The connection is acquired under the system scope because this test drops to the runtime role
	// BY HAND to prove the append-only GRANTs; without a scope the tenant policies would deny the
	// insert first and the grant assertion would never be reached.
	ctx := storage.WithSystemScope(context.Background())
	seedRun(t, pool)

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
		`INSERT INTO audit_events (actor, action, outcome) VALUES ('actor','run.create','allowed')`); err != nil {
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

	tenant, _, _ := seedRun(t, pool)
	callID := newID("tcall") // a correlation key; no tool_calls row need exist (no FK)

	openOp := func(id, call, state string) error {
		_, err := pool.Exec(storage.WithSystemScope(ctx),
			`INSERT INTO remote_tool_operations (id, project_id, tool_call_id, secret_ref, callback_token_hash, deadline, state, fence)
			 VALUES ($1, $2, $3, 'sig-ref', 'tokenhash', clock_timestamp() + interval '30 seconds', $4, 5)`,
			id, tenant.Project, call, state)
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

	tenant, _, _ := seedRun(t, pool)
	insertHook := func(id, name, point, category, executor string) error {
		_, err := pool.Exec(storage.WithSystemScope(ctx),
			`INSERT INTO hooks (id, project_id, name, hook_point, category, executor, config)
			 VALUES ($1,$2,$3,$4,$5,$6,'{}'::jsonb)`,
			id, tenant.Project, name, point, category, executor)
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
	exec(t, pool, `INSERT INTO principals (id, project_id, kind) VALUES ($1,$2,'service')`,
		prin, tenant.Project)
	liveTok, expTok := newID("sk"), newID("sk")
	exec(t, pool, `INSERT INTO api_keys (id, project_id, principal_id, key_hash, expires_at)
		VALUES ($1,$2,$3,$4,now() + interval '1 hour')`,
		newID("key"), tenant.Project, prin, coordinator.HashAPIKey(liveTok))
	exec(t, pool, `INSERT INTO api_keys (id, project_id, principal_id, key_hash, expires_at)
		VALUES ($1,$2,$3,$4,now() - interval '1 hour')`,
		newID("key"), tenant.Project, prin, coordinator.HashAPIKey(expTok))
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

// TestMigration31SecretRefsGrantsAppendOnlyAcrossReboots pins the append-only grant on secret_refs across a
// SECOND boot (E13 Task 3). secret_refs is the first table created after 000029's blanket
// `GRANT ... ON ALL TABLES`, so on boot #2 both 000001 and 000029's blanket grants re-run and — now that the
// table exists — hand palai_app UPDATE+DELETE it must never hold (silent ciphertext replacement / version
// deletion). 000031's REVOKE (it runs LAST in the chain, self-re-asserting every boot) is what keeps them
// withheld — the load-bearing half the 000015 precedent documents. This test migrates TWICE, then proves as
// the runtime role that SELECT+INSERT are held but UPDATE+DELETE are denied (42501). It FAILS without the
// REVOKE line and PASSES with it; the RLS-catalogue gate checks policies, not grants, so this class needs
// its own grant test.
func TestMigration31SecretRefsGrantsAppendOnlyAcrossReboots(t *testing.T) {
	cs := openHarness(t)
	ctx := storage.WithSystemScope(context.Background())
	pool := cs.Pool()

	// The second boot: main.go re-runs the whole chain on every start. This is the boot that re-exposes the
	// blanket grants to the now-existing secret_refs table.
	if err := cs.Migrate(context.Background()); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}

	// The definitive privilege assertion (RLS-independent): after two boots, palai_app holds SELECT+INSERT
	// and NOT UPDATE/DELETE.
	assertPriv := func(priv string, want bool) {
		var got bool
		if err := pool.QueryRow(ctx, `SELECT has_table_privilege('palai_app', 'secret_refs', $1)`, priv).Scan(&got); err != nil {
			t.Fatalf("has_table_privilege(%s) error = %v", priv, err)
		}
		if got != want {
			t.Fatalf("palai_app %s on secret_refs = %v, want %v (append-only grant eroded across reboots)", priv, got, want)
		}
	}
	assertPriv("SELECT", true)
	assertPriv("INSERT", true)
	assertPriv("UPDATE", false)
	assertPriv("DELETE", false)

	// Behavioral half: as the runtime role itself, an UPDATE/DELETE on secret_refs is refused by the
	// privilege check (42501) before RLS is even consulted — a compromised handler cannot silently replace a
	// ciphertext or delete version history.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET ROLE palai_app`); err != nil {
		t.Fatalf("SET ROLE palai_app error = %v", err)
	}
	defer func() { _, _ = conn.Exec(ctx, `RESET ROLE`) }()

	if got := pgCode(mustFail(conn.Exec(ctx, `UPDATE secret_refs SET ciphertext = '\x00'`))); got != "42501" {
		t.Fatalf("secret_refs UPDATE code = %q, want 42501 (append-only: UPDATE withheld)", got)
	}
	if got := pgCode(mustFail(conn.Exec(ctx, `DELETE FROM secret_refs`))); got != "42501" {
		t.Fatalf("secret_refs DELETE code = %q, want 42501 (append-only: DELETE withheld)", got)
	}
}

// TestMigration37Queues proves 000037 adds the queue-adapter tables (E17 Task 7, spec §34.1-34.5)
// idempotently: present after apply, a re-apply is a clean no-op (every object IF NOT EXISTS), version 37
// recorded exactly once. It pins the load-bearing invariants: the queue_messages enqueue-dedupe UNIQUE
// (a producer double-publish collapses), and — the crux — the queue_effect_receipts idempotency ledger is
// append-only ACROSS REBOOTS: even after the chain re-runs (re-exposing 000001/000029's blanket grants),
// palai_app holds SELECT+INSERT but NOT UPDATE/DELETE, so the process can neither rewrite nor erase a
// receipt (which would let a redelivered message re-run its effect). Mirrors the secret_refs reboot proof.
func TestMigration37Queues(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()

	// A second boot re-runs the whole chain (the boot that re-exposes the blanket grants to the now-existing
	// queue tables) — every object is IF NOT EXISTS / idempotent, so it is a clean no-op.
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	for _, name := range []string{"queue_connections", "queue_messages", "queue_effect_receipts", "queue_deliveries"} {
		if !tableExists(t, pool, name) {
			t.Fatalf("after apply, %q is missing", name)
		}
	}
	if !indexExists(t, pool, "queue_messages_deliverable_idx") || !indexExists(t, pool, "queue_deliveries_due_idx") {
		t.Fatal("after apply, a 000037 index is missing")
	}

	// Seed a tenant + a connection so the unique/append-only inserts have a valid scope + FK target.
	tenant, _, _ := seedRun(t, pool)
	connID := newID("qconn")
	exec(t, pool,
		`INSERT INTO queue_connections (id, project_id, name) VALUES ($1,$2,'q')`,
		connID, tenant.Project)

	// Enqueue-dedupe: a second message with the same (connection, idempotency_key) is rejected.
	insertMsg := func(key string) error {
		_, err := pool.Exec(storage.WithSystemScope(ctx),
			`INSERT INTO queue_messages (id, project_id, queue_connection_id, idempotency_key, body)
			 VALUES ($1,$2,$3,$4,$5)`,
			newID("qmsg"), tenant.Project, connID, key, []byte("x"))
		return err
	}
	if err := insertMsg("k1"); err != nil {
		t.Fatalf("first queue_messages insert error = %v", err)
	}
	if got := pgCode(insertMsg("k1")); got != "23505" {
		t.Fatalf("duplicate (connection, idempotency_key) code = %q, want 23505 unique_violation", got)
	}

	// The append-only privilege assertion (RLS-independent), after two boots: palai_app SELECT+INSERT, not
	// UPDATE/DELETE.
	sctx := storage.WithSystemScope(ctx)
	assertPriv := func(priv string, want bool) {
		var got bool
		if err := pool.QueryRow(sctx,
			`SELECT has_table_privilege('palai_app', 'queue_effect_receipts', $1)`, priv).Scan(&got); err != nil {
			t.Fatalf("has_table_privilege(%s) error = %v", priv, err)
		}
		if got != want {
			t.Fatalf("palai_app %s on queue_effect_receipts = %v, want %v (append-only grant eroded across reboots)", priv, got, want)
		}
	}
	assertPriv("SELECT", true)
	assertPriv("INSERT", true)
	assertPriv("UPDATE", false)
	assertPriv("DELETE", false)

	// Behavioral half: as the runtime role, an UPDATE/DELETE on the ledger is refused (42501) before RLS is
	// even consulted — a redelivered message cannot re-run its effect by tampering with its receipt.
	conn, err := pool.Acquire(sctx)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(sctx, `SET ROLE palai_app`); err != nil {
		t.Fatalf("SET ROLE palai_app error = %v", err)
	}
	defer func() { _, _ = conn.Exec(sctx, `RESET ROLE`) }()
	if got := pgCode(mustFail(conn.Exec(sctx, `UPDATE queue_effect_receipts SET idempotency_key = 'x'`))); got != "42501" {
		t.Fatalf("queue_effect_receipts UPDATE code = %q, want 42501 (append-only: UPDATE withheld)", got)
	}
	if got := pgCode(mustFail(conn.Exec(sctx, `DELETE FROM queue_effect_receipts`))); got != "42501" {
		t.Fatalf("queue_effect_receipts DELETE code = %q, want 42501 (append-only: DELETE withheld)", got)
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

	tenant, _, _ := seedRun(t, pool)
	skillID := newID("skill")
	if _, err := pool.Exec(storage.WithSystemScope(ctx), `INSERT INTO skills (id, project_id, name) VALUES ($1,$2,'commit')`,
		skillID, tenant.Project); err != nil {
		t.Fatalf("insert skill error = %v", err)
	}
	// A duplicate skill name in the same project is rejected (tenant-scoped unique).
	if got := pgCode(mustFail(pool.Exec(storage.WithSystemScope(ctx), `INSERT INTO skills (id, project_id, name) VALUES ($1,$2,'commit')`,
		newID("skill"), tenant.Project))); got != "23505" {
		t.Fatalf("duplicate skill name code = %q, want 23505 unique_violation", got)
	}

	insertRev := func(id string, revNo int, state string) error {
		_, err := pool.Exec(storage.WithSystemScope(ctx),
			`INSERT INTO skill_revisions (id, project_id, skill_id, revision_number, digest, state, archive)
			 VALUES ($1,$2,$3,$4,'sha256:x',$5,'\x00')`,
			id, tenant.Project, skillID, revNo, state)
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

// TestMigration41SlackReplies pins the RETURN LEG's durable order-to-post (000041). Two properties earn the
// migration and both are asserted at the DATABASE, not in Go: UNIQUE (run_id) is the exactly-once claim —
// a second terminal transaction for the same run must insert NOTHING, whatever the caller believes — and
// the session index is what keeps the enqueue (which runs on EVERY terminal transition in the deployment,
// almost all of them non-Slack) off a sequential scan inside the hot transaction.
func TestMigration41SlackReplies(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()

	// A second boot re-runs the whole chain; every object is IF NOT EXISTS / idempotent.
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	if !tableExists(t, pool, "slack_reply_deliveries") {
		t.Fatal("after apply, slack_reply_deliveries is missing")
	}
	if !indexExists(t, pool, "slack_reply_deliveries_due_idx") || !indexExists(t, pool, "slack_thread_sessions_session_idx") {
		t.Fatal("after apply, a 000041 index is missing — the enqueue would scan slack_thread_sessions on every terminal transition")
	}

	tenant, _, runID := seedRun(t, pool)
	connID := newID("slkc")
	exec(t, pool,
		`INSERT INTO slack_connections (id, project_id, team_id, signing_secret_ref)
		 VALUES ($1,$2,$3,'slack/signing')`,
		connID, tenant.Project, strings.ToUpper(newID("T")))

	insert := func() error {
		_, err := pool.Exec(storage.WithSystemScope(ctx),
			`INSERT INTO slack_reply_deliveries
			   (id, project_id, connection_id, run_id, channel_id, thread_ts, run_state)
			 VALUES ($1,$2,$3,$4,'C1','100.0','completed')`,
			newID("sdel"), tenant.Project, connID, runID)
		return err
	}
	if err := insert(); err != nil {
		t.Fatalf("first slack_reply_deliveries insert error = %v", err)
	}
	if err := insert(); err == nil {
		t.Fatal("a SECOND order-to-post for the same run was accepted; UNIQUE (run_id) is the whole exactly-once claim")
	}

	// An unbound workspace must take its undelivered replies with it: a connection we no longer hold is one
	// we must not post into.
	exec(t, pool, `DELETE FROM slack_connections WHERE id = $1`, connID)
	var left int
	if err := pool.QueryRow(storage.WithSystemScope(ctx),
		`SELECT count(*) FROM slack_reply_deliveries WHERE connection_id = $1`, connID).Scan(&left); err != nil {
		t.Fatalf("count orphaned deliveries: %v", err)
	}
	if left != 0 {
		t.Fatalf("%d delivery row(s) outlived their connection, want 0", left)
	}
}

// TestMigration42SlackMessageTurns is 000042: the handle from a Slack MESSAGE to the turn it became, and the
// withdrawn marker the history query filters on. Both exist because of one live defect and its mirror image —
// the app answered a deleted message, and a deleted message that stays in the history goes on shaping every
// later answer in the thread.
func TestMigration42SlackMessageTurns(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()

	// A second boot re-runs the whole chain; every object is IF NOT EXISTS / idempotent.
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	if !tableExists(t, pool, "slack_message_turns") {
		t.Fatal("after apply, slack_message_turns is missing")
	}
	if !columnExists(t, pool, "responses", "retracted_at") {
		t.Fatal("after apply, responses.retracted_at is missing — nothing could withdraw a turn from history")
	}

	tenant, sessionID, _ := seedRun(t, pool)
	connID := newID("slkc")
	exec(t, pool,
		`INSERT INTO slack_connections (id, project_id, team_id, signing_secret_ref)
		 VALUES ($1,$2,$3,'slack/signing')`,
		connID, tenant.Project, strings.ToUpper(newID("T")))
	responseID := newID("resp")
	exec(t, pool, `INSERT INTO responses (id, project_id, session_id) VALUES ($1,$2,$3)`,
		responseID, tenant.Project, sessionID)

	insert := func() error {
		_, err := pool.Exec(storage.WithSystemScope(ctx),
			`INSERT INTO slack_message_turns
			   (id, project_id, connection_id, team_id, channel_id, message_ts, response_id, session_id)
			 VALUES ($1,$2,$3,'T1','C1','100.0',$4,$5)`,
			newID("slkmt"), tenant.Project, connID, responseID, sessionID)
		return err
	}
	if err := insert(); err != nil {
		t.Fatalf("first slack_message_turns insert error = %v", err)
	}
	// ONE TURN PER MESSAGE. Slack delivers a top-level mention twice (app_mention plus its message.channels
	// twin) under a single message ts, and a redelivery replays onto the same response: the unique index is
	// what makes the FIRST response the turn instead of the last writer winning.
	if err := insert(); err == nil {
		t.Fatal("a SECOND turn was accepted for one message ts; the handle would then point at whichever event arrived last")
	}

	// A reaped response takes its handle with it, so nothing is left pointing at a turn that no longer exists.
	exec(t, pool, `DELETE FROM responses WHERE id = $1`, responseID)
	var left int
	if err := pool.QueryRow(storage.WithSystemScope(ctx),
		`SELECT count(*) FROM slack_message_turns WHERE response_id = $1`, responseID).Scan(&left); err != nil {
		t.Fatalf("count orphaned turn handles: %v", err)
	}
	if left != 0 {
		t.Fatalf("%d turn handle(s) outlived their response, want 0", left)
	}
}

// TestMigration43SlackRequester is 000043: the DURABLE identity the agent's own question is addressed to.
// Two properties earn it and both are asserted at the database. The DEFAULT is the fail-closed value — an
// insert that names no requester gets ”, which is what every row written before today carries and what the
// renderer treats as "send the words, mention nobody" — and the id CASCADES with the row it hangs off, so a
// reaped response or an unbound workspace cannot leave an address behind pointing at a conversation that no
// longer exists.
func TestMigration43SlackRequester(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()

	// A second boot re-runs the whole chain; both ALTERs are ADD COLUMN IF NOT EXISTS.
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	if !columnExists(t, pool, "slack_message_turns", "requester_user_id") {
		t.Fatal("after apply, slack_message_turns.requester_user_id is missing — admission would have nowhere to record who asked")
	}
	if !columnExists(t, pool, "slack_reply_deliveries", "requester_user_id") {
		t.Fatal("after apply, slack_reply_deliveries.requester_user_id is missing — the reply pump could not address the mention")
	}

	tenant, sessionID, runID := seedRun(t, pool)
	connID := newID("slkc")
	exec(t, pool,
		`INSERT INTO slack_connections (id, project_id, team_id, signing_secret_ref)
		 VALUES ($1,$2,$3,'slack/signing')`,
		connID, tenant.Project, strings.ToUpper(newID("T")))
	responseID := newID("resp")
	exec(t, pool, `INSERT INTO responses (id, project_id, session_id) VALUES ($1,$2,$3)`,
		responseID, tenant.Project, sessionID)

	// FAIL-CLOSED BACKFILL: the shape of every row that existed before this migration. An insert that names
	// no requester must be accepted and must read back as the empty string — not NULL, which would make every
	// consumer decide what a missing id means.
	exec(t, pool,
		`INSERT INTO slack_reply_deliveries
		   (id, project_id, connection_id, run_id, channel_id, thread_ts, run_state)
		 VALUES ($1,$2,$3,$4,'C1','100.0','completed')`,
		newID("sdel"), tenant.Project, connID, runID)
	var legacy string
	if err := pool.QueryRow(storage.WithSystemScope(ctx),
		`SELECT requester_user_id FROM slack_reply_deliveries WHERE run_id = $1`, runID).Scan(&legacy); err != nil {
		t.Fatalf("read the defaulted requester: %v", err)
	}
	if legacy != "" {
		t.Fatalf("a delivery written without a requester reads back %q, want the empty fail-closed value", legacy)
	}

	exec(t, pool,
		`INSERT INTO slack_message_turns
		   (id, project_id, connection_id, team_id, channel_id, message_ts, response_id, session_id, requester_user_id)
		 VALUES ($1,$2,$3,'T1','C1','100.0',$4,$5,'U0ASKER')`,
		newID("slkmt"), tenant.Project, connID, responseID, sessionID)

	// The identity cannot outlive what it describes: reaping the response takes the turn handle AND the id
	// with it, so a purge cannot leave a person's id attached to a conversation nobody can read.
	//
	// THE COUNT IS TENANT-SCOPED, and it was not until E21 T7's exit gate co-ran this package with the store
	// suite against ONE Postgres. `U0ASKER` is a shared fixture id — the store tier's mention test uses the
	// same literal — so an unscoped sweep counts ANOTHER test's rows and fails on work this test never did.
	// The same shape defeated an E19 T6 outbound assertion for the same reason: a global count is not an
	// assertion about this test's cascade, it is an assertion about whatever else touched the database.
	exec(t, pool, `DELETE FROM responses WHERE id = $1`, responseID)
	var left int
	if err := pool.QueryRow(storage.WithSystemScope(ctx),
		`SELECT count(*) FROM slack_message_turns
		  WHERE  project_id = $1 AND requester_user_id = 'U0ASKER'`,
		tenant.Project).Scan(&left); err != nil {
		t.Fatalf("count orphaned requesters: %v", err)
	}
	if left != 0 {
		t.Fatalf("%d requester id(s) outlived the turn they describe, want 0", left)
	}
}

// TestMigration45RunnerFleet is 000045, and the first thing it pins is a CORRECTION: runner_pools,
// runners and runner_leases are NOT new tables. 000001_core.up.sql:201-227 created all three as empty
// skeletons, so 000029's catalogue sweep already secured them and its blanket GRANT already covered
// them — which means this migration's job on those two is to CHANGE THEIR SHAPE, and the shape change
// that matters is runners.project_id: it flips the has_project half of the policy expression 000029
// derives, and 29 has already run by the time 45 applies. Without 45's own re-CALL the table would
// carry the org-only rule for a whole boot.
//
// The rest is what the registry has to be true for: an APPEND-ONLY journal that survives the blanket
// grants re-running (the secret_refs/capability_jobs reboot proof, applied to runner_enrollments), a
// runner_dns that cannot be issued twice, a state CHECK that rejects a value nothing types, and the
// seeded default pool without which an upgrading install's runner has nowhere to enrol.
func TestMigration45RunnerFleet(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()

	// The SECOND boot: main.go re-runs the whole chain on every start, and it is this boot that
	// re-exposes 000001's and 000029's blanket grants to the now-existing runner_enrollments.
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	for _, table := range []string{"runner_pools", "runner_pool_keys", "runners", "runner_enrollments"} {
		if !tableExists(t, pool, table) {
			t.Fatalf("after apply, %s is missing", table)
		}
	}
	for _, col := range [][2]string{
		{"runner_pools", "posture"}, {"runner_pools", "strict_enrollment"},
		{"runners", "project_id"}, {"runners", "label"}, {"runners", "runner_dns"},
		{"runners", "state"}, {"runners", "last_seen_at"}, {"runners", "enrolled_via_key_id"},
		{"runs", "pool_id"},
	} {
		if !columnExists(t, pool, col[0], col[1]) {
			t.Fatalf("after apply, %s.%s is missing", col[0], col[1])
		}
	}
	// status is GONE and that is the assertion, not a detail: two columns meaning "what state is this
	// runner in" is how a later reader writes to the one nothing reads.
	if columnExists(t, pool, "runners", "status") {
		t.Fatal("runners.status survived; the 000001 column it replaces must not coexist with state")
	}

	// THE POLICY EXPRESSION, read out of the catalogue. `runners` gained a project_id in THIS migration,
	// so its tenant_isolation policy must narrow on project_id — the whole reason 45 re-CALLs the
	// procedure 29 already called. Asserting on the rendered expression rather than on ENABLE/FORCE is
	// what makes this test catch the failure the tenancy corpus cannot: that corpus checks a table is
	// secured, not that it is secured by the RIGHT rule.
	for _, table := range []string{"runner_pools", "runners", "runner_pool_keys", "runner_enrollments"} {
		var qual string
		if err := pool.QueryRow(storage.WithSystemScope(ctx),
			`SELECT qual FROM pg_policies WHERE schemaname = 'public' AND tablename = $1 AND policyname = 'tenant_isolation'`,
			table).Scan(&qual); err != nil {
			t.Fatalf("read %s tenant_isolation policy: %v", table, err)
		}
		if !strings.Contains(qual, "project_id") {
			t.Fatalf("%s tenant_isolation does not narrow on project_id: %s", table, qual)
		}
		// ENABLE and FORCE, read per table and LOGGED. tests/security/tenancy sweeps the whole catalogue
		// and demands both, which is the gate — but it names no table on the way past, so a reader
		// checking that THESE four are secured has nowhere to look. Under -v this is that place.
		var enabled, forced bool
		if err := pool.QueryRow(storage.WithSystemScope(ctx),
			`SELECT c.relrowsecurity, c.relforcerowsecurity FROM pg_class c
			   JOIN pg_namespace n ON n.oid = c.relnamespace
			  WHERE n.nspname = 'public' AND c.relname = $1`, table).Scan(&enabled, &forced); err != nil {
			t.Fatalf("read %s row-security flags: %v", table, err)
		}
		if !enabled || !forced {
			t.Fatalf("%s: row security enabled=%v forced=%v, want both true", table, enabled, forced)
		}
		t.Logf("RLS %-20s enabled=%v forced=%v qual=%s", table, enabled, forced, qual)
	}

	tenant, _, runID := seedRun(t, pool)
	poolID := newID("pool")
	exec(t, pool,
		`INSERT INTO runner_pools (id, project_id, name, posture) VALUES ($1,$2,'macs','unsandboxed-host')`,
		poolID, tenant.Project)

	// A posture nothing implements is refused by the database, not by a switch statement somebody
	// remembers to update.
	if got := pgCode(mustFail(pool.Exec(storage.WithSystemScope(ctx),
		`INSERT INTO runner_pools (id, project_id, name, posture) VALUES ($1,$2,'weird','windows-vm')`,
		newID("pool"), tenant.Project))); got != "23514" {
		t.Fatalf("an unknown posture was accepted (code %q, want 23514)", got)
	}
	// One pool per name per project.
	if got := pgCode(mustFail(pool.Exec(storage.WithSystemScope(ctx),
		`INSERT INTO runner_pools (id, project_id, name) VALUES ($1,$2,'macs')`,
		newID("pool"), tenant.Project))); got != "23505" {
		t.Fatalf("a second pool named `macs` was accepted in one project (code %q, want 23505)", got)
	}

	insertRunner := func(id, dns string) error {
		_, err := pool.Exec(storage.WithSystemScope(ctx),
			`INSERT INTO runners (id, project_id, pool_id, label, runner_dns, state)
			 VALUES ($1,$2,$3,'runner-local',$4,'active')`,
			id, tenant.Project, poolID, dns)
		return err
	}
	firstID, secondID := newID("rnr"), newID("rnr")
	if err := insertRunner(firstID, firstID+".runners.palai.internal"); err != nil {
		t.Fatalf("first runner insert error = %v", err)
	}
	// TWO MACHINES, ONE LABEL — the compose default (runner-entrypoint.sh:10 hardcodes "runner-local",
	// so `--scale runner=3` is three machines with one name). Both rows must be accepted: the label is
	// not the identity, and a registry that refused the second would have re-created the single slot it
	// exists to replace.
	if err := insertRunner(secondID, secondID+".runners.palai.internal"); err != nil {
		t.Fatalf("a second machine sharing the label `runner-local` was refused: %v", err)
	}
	// The DNS is the identity, and the CA cannot be asked to issue one name twice.
	if got := pgCode(mustFail(pool.Exec(storage.WithSystemScope(ctx),
		`INSERT INTO runners (id, project_id, pool_id, runner_dns, state)
		 VALUES ($1,$2,$3,$4,'active')`,
		newID("rnr"), tenant.Project, poolID, firstID+".runners.palai.internal"))); got != "23505" {
		t.Fatalf("two runners were accepted for one certificate DNS (code %q, want 23505)", got)
	}
	if got := pgCode(mustFail(pool.Exec(storage.WithSystemScope(ctx),
		`INSERT INTO runners (id, project_id, pool_id, state) VALUES ($1,$2,$3,'zombie')`,
		newID("rnr"), tenant.Project, poolID))); got != "23514" {
		t.Fatalf("an unknown runner state was accepted (code %q, want 23514)", got)
	}

	// R5: a run records where it was placed, and it CANNOT name a pool that does not exist.
	exec(t, pool, `UPDATE runs SET pool_id = $1 WHERE id = $2`, poolID, runID)
	if got := pgCode(mustFail(pool.Exec(storage.WithSystemScope(ctx),
		`UPDATE runs SET pool_id = 'pool_nonexistent' WHERE id = $1`, runID))); got != "23503" {
		t.Fatalf("a run was placed into a pool that does not exist (code %q, want 23503)", got)
	}

	// R6: NO MIGRATION SEEDS THE DEFAULT POOL. A migration once inserted it for an install upgrading into
	// the fleet tables; that seed went with the chain squash, and it had already been unreachable before
	// it — it selected from a projects table that is empty on a first boot (the identity bootstrap runs
	// AFTER the migrations), and its own guard skipped it on every boot after. Every pool alive today
	// comes from identity.Store.provision, which runs InsertDefaultRunnerPool in the same transaction as
	// the four identity rows.
	//
	// THE QUESTION IS ABOUT THE SEED'S FIXED ID, NOT ABOUT A COUNT, AND THAT IS A CORRECTION. The first
	// version of this assertion counted runner_pools GLOBALLY and required zero. It failed, correctly:
	// this tier shares ONE database across dozens of tests and this very test creates a pool of its own a
	// few lines above, so a global count measures the neighbours rather than the chain. 'pool_default' is
	// the ON CONFLICT target the seed used and the id identity.Store.provision still writes for the
	// bootstrap tenant — and this harness runs no bootstrap, so nothing but a migration could put it here.
	//
	// "AT MOST ONE", WHICH THIS REPLACED, WAS VACUOUS: the row is absent in this harness, so a `> 1` check
	// passed without measuring anything at all.
	var seeded int
	if err := pool.QueryRow(storage.WithSystemScope(ctx),
		`SELECT count(*) FROM runner_pools WHERE id = 'pool_default'`).Scan(&seeded); err != nil {
		t.Fatalf("count the default pool after the chain: %v", err)
	}
	if seeded != 0 {
		t.Fatalf("the migration chain left a 'pool_default' row behind; no migration seeds a tenant row, " +
			"and identity.Store.provision is the only writer of the default pool")
	}

	// R4 — THE APPEND-ONLY JOURNAL, across the reboot that re-ran both blanket grants.
	assertPriv := func(priv string, want bool) {
		t.Helper()
		var got bool
		if err := pool.QueryRow(storage.WithSystemScope(ctx),
			`SELECT has_table_privilege('palai_app', 'runner_enrollments', $1)`, priv).Scan(&got); err != nil {
			t.Fatalf("has_table_privilege(%s) error = %v", priv, err)
		}
		if got != want {
			t.Fatalf("palai_app %s on runner_enrollments = %v, want %v (append-only grant eroded across reboots)", priv, got, want)
		}
	}
	assertPriv("SELECT", true)
	assertPriv("INSERT", true)
	assertPriv("UPDATE", false)
	assertPriv("DELETE", false)

	entryID := newID("renr")
	exec(t, pool,
		`INSERT INTO runner_enrollments (id, project_id, runner_id, pool_id, entry_kind, entry_seq)
		 VALUES ($1,$2,$3,$4,'issued',1)`,
		entryID, tenant.Project, firstID, poolID)
	if got := pgCode(mustFail(pool.Exec(storage.WithSystemScope(ctx),
		`INSERT INTO runner_enrollments (id, project_id, runner_id, pool_id, entry_kind, entry_seq)
		 VALUES ($1,$2,$3,$4,'revoked',1)`,
		newID("renr"), tenant.Project, firstID, poolID))); got != "23505" {
		t.Fatalf("two entries were accepted at seq 1 for one runner (code %q, want 23505)", got)
	}

	// The BEHAVIOURAL half: as the runtime role itself, rewriting or erasing an entry is refused by the
	// privilege check (42501) before RLS is consulted. A journal a compromised runner could rewrite — to
	// change which key issued its certificate — or delete — to erase its own revocation — is not a journal.
	conn, err := pool.Acquire(storage.WithSystemScope(ctx))
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET ROLE palai_app`); err != nil {
		t.Fatalf("SET ROLE palai_app error = %v", err)
	}
	defer func() { _, _ = conn.Exec(ctx, `RESET ROLE`) }()
	if got := pgCode(mustFail(conn.Exec(ctx, `UPDATE runner_enrollments SET key_id = 'other'`))); got != "42501" {
		t.Fatalf("runner_enrollments UPDATE code = %q, want 42501 (append-only: UPDATE withheld)", got)
	}
	if got := pgCode(mustFail(conn.Exec(ctx, `DELETE FROM runner_enrollments`))); got != "42501" {
		t.Fatalf("runner_enrollments DELETE code = %q, want 42501 (append-only: DELETE withheld)", got)
	}
}

// TestMigration46EnvironmentsGrantsAreAsymmetricAcrossReboots pins the ONE thing migration 000046's design
// rests on, and it is a set of GRANTS rather than a schema: `environment_values` may be DELETEd and
// `environments` may not, both after a SECOND boot.
//
// WHY THE ASYMMETRY IS THE WHOLE FEATURE. secret_refs can never lose a row (000031's REVOKE re-asserts on
// every boot, on purpose — version history is retained for audit). So "remove the JIRA_TOKEN key from the
// production environment" cannot mean "delete the bytes". 000046 splits MEMBERSHIP from BYTES precisely so
// that the removal has something real to remove: the binding goes, the sealed versions stay, and nothing
// names them afterwards because the derived name `env:<environment_id>:<key>` is only ever built from a
// membership row. A delete button that deletes something other than what it says is worse than no button,
// and the API's response body says which one this is.
//
// WHY IT NEEDS ITS OWN TEST. The RLS-catalogue gate (tests/security/tenancy) checks POLICIES, not grants,
// and every harness in this repository migrates more than once — so a grant eroded by the blanket
// `GRANT ... ON ALL TABLES` in 000001/000029 re-running on boot #2 is invisible to every other tier. This
// migrates TWICE and then asks the runtime role directly.
func TestMigration46EnvironmentsGrantsAreAsymmetricAcrossReboots(t *testing.T) {
	cs := openHarness(t)
	ctx := storage.WithSystemScope(context.Background())
	pool := cs.Pool()

	// The second boot: main.go re-runs the whole chain on every start, and this is the boot on which the
	// blanket grants are re-exposed to the now-existing pair.
	if err := cs.Migrate(context.Background()); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}

	assertPriv := func(table, priv string, want bool) {
		t.Helper()
		var got bool
		if err := pool.QueryRow(ctx, `SELECT has_table_privilege('palai_app', $1, $2)`, table, priv).Scan(&got); err != nil {
			t.Fatalf("has_table_privilege(%s, %s) error = %v", table, priv, err)
		}
		if got != want {
			t.Fatalf("palai_app %s on %s = %v, want %v (000046's grants eroded across reboots)", priv, table, got, want)
		}
	}
	// environments: append-only. An environment id is embedded in every derived secret name it groups, so
	// deleting or renaming one would orphan every version under it — and E25 ships no such button.
	assertPriv("environments", "SELECT", true)
	assertPriv("environments", "INSERT", true)
	assertPriv("environments", "UPDATE", false)
	assertPriv("environments", "DELETE", false)
	// environment_values: DELETE is GRANTED, UPDATE is not. A key's name is its identity here, so a
	// "rename" would silently orphan the versions stored under the old derived name; renaming is
	// remove-then-add, which is honest about the new key starting at version 1.
	assertPriv("environment_values", "SELECT", true)
	assertPriv("environment_values", "INSERT", true)
	assertPriv("environment_values", "DELETE", true)
	assertPriv("environment_values", "UPDATE", false)

	// The behavioural half, as the runtime role itself: the privilege check refuses before RLS is even
	// consulted (42501).
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET ROLE palai_app`); err != nil {
		t.Fatalf("SET ROLE palai_app error = %v", err)
	}
	defer func() { _, _ = conn.Exec(ctx, `RESET ROLE`) }()

	if got := pgCode(mustFail(conn.Exec(ctx, `DELETE FROM environments`))); got != "42501" {
		t.Fatalf("environments DELETE code = %q, want 42501 — an environment id addresses every secret it groups", got)
	}
	if got := pgCode(mustFail(conn.Exec(ctx, `UPDATE environments SET name = 'x'`))); got != "42501" {
		t.Fatalf("environments UPDATE code = %q, want 42501", got)
	}
	if got := pgCode(mustFail(conn.Exec(ctx, `UPDATE environment_values SET key = 'x'`))); got != "42501" {
		t.Fatalf("environment_values UPDATE code = %q, want 42501 — a rename would orphan the versions under the old derived name", got)
	}
	// And DELETE on the membership table SUCCEEDS (zero rows, but permitted) — the positive leg, without
	// which the three refusals above would pass on a build where nothing can be written at all.
	if _, err := conn.Exec(ctx, `DELETE FROM environment_values WHERE environment_id = 'nonexistent'`); err != nil {
		t.Fatalf("environment_values DELETE was refused (%v) — removing a key BINDING is the one deletion this pair exists to allow", err)
	}

	// The `environment` column 000046 adds to agent_revisions, with its honest default. Asserted here
	// because the column lands in the same change as the code that reads it (000019's own rule against
	// storing dead config), so a rollback that dropped one and not the other would be silent.
	var dflt, nullable string
	if err := pool.QueryRow(ctx, `SELECT column_default, is_nullable FROM information_schema.columns
	     WHERE table_schema='public' AND table_name='agent_revisions' AND column_name='environment'`).Scan(&dflt, &nullable); err != nil {
		t.Fatalf("agent_revisions.environment is absent: %v", err)
	}
	if nullable != "NO" || dflt != `''::text` {
		t.Fatalf("agent_revisions.environment is nullable=%s default=%s, want NOT NULL DEFAULT '' — a NULL would make `IS NULL` and `= ''` two different kinds of nothing", nullable, dflt)
	}
}

// TestMigration47BackgroundTasks pins the three column decisions migration 000047 makes, and each of them
// is a decision rather than a convenience — the migration says so in its own comments and this test is
// what makes those comments true.
//
// A NOTE ON WHY THIS TEST EXISTS AT ALL WHEN NO GO CODE READS THE TABLE YET. 000019's own comment forbids
// storing dead config, and a table nothing reads brushes against it. The boundary is deliberate: E26's
// other six tasks add NO migration, so one task owns 000047 and "two parallel tasks both took the next
// number" cannot happen in this epic. What that costs is a window in which the schema is ahead of its
// readers, and this test is the interest paid on it — the CHECKs, the uniqueness and the grants are
// enforced by the database from the day the table exists, not from the day something queries it.
func TestMigration47BackgroundTasks(t *testing.T) {
	cs := openHarness(t)
	ctx := storage.WithSystemScope(context.Background())
	pool := cs.Pool()

	// The second boot: main.go re-runs the whole chain on every start, so this is the boot on which
	// 000001's and 000029's blanket grants are re-exposed to the now-existing table.
	if err := cs.Migrate(context.Background()); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}

	// exit_code IS NULLABLE, and that is the sharpest of the three. A control plane that restarted knows a
	// task is gone and cannot know what it returned; NOT NULL would have forced a sentinel, and a -1 in
	// this column is a number a model compares against zero.
	var nullable, dataType string
	if err := pool.QueryRow(ctx, `SELECT is_nullable, data_type FROM information_schema.columns
	     WHERE table_schema='public' AND table_name='background_tasks' AND column_name='exit_code'`).Scan(&nullable, &dataType); err != nil {
		t.Fatalf("background_tasks.exit_code is absent: %v", err)
	}
	if nullable != "YES" {
		t.Fatalf("background_tasks.exit_code is NOT NULL, which forces a sentinel for every task whose "+
			"exit status nobody observed (data_type=%s)", dataType)
	}
	// deadline_at is nullable for the opposite reason: NULL means no ceiling, which is a choice an operator
	// may legitimately make for a credential-free task (E26 §0.2's `0` = unbounded).
	if err := pool.QueryRow(ctx, `SELECT is_nullable FROM information_schema.columns
	     WHERE table_schema='public' AND table_name='background_tasks' AND column_name='deadline_at'`).Scan(&nullable); err != nil {
		t.Fatalf("background_tasks.deadline_at is absent: %v", err)
	}
	if nullable != "YES" {
		t.Fatal("background_tasks.deadline_at is NOT NULL; an unbounded task is a legitimate operator choice")
	}

	// The CHECK on posture. It is not decoration: the reaper probes a DIFFERENT operating-system object
	// per posture (a container id versus a pgid), and an unrecognised posture would send it to the wrong
	// one — where the failure mode is not an error but a wrong answer, "no such container" reading as
	// "the task exited" for a process that is still compiling.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	defer conn.Release()

	project, session, response, run, call := seedBackgroundTaskParents(t, ctx, conn)
	insert := func(id, posture, state string) error {
		_, err := conn.Exec(ctx, `INSERT INTO background_tasks
		    (id, project_id, run_id, session_id, response_id, tool_call_id, posture, handle, state, output_path)
		  VALUES ($1,$2,$3,$4,$5,$6,$7,'4242:2026-07-30T12:00:00Z',$8,'.palai-session/bg/'||$1||'.log')`,
			id, project, run, session, response, call, posture, state)
		return err
	}
	if got := pgCode(insert("bgt-bad-posture", "kubernetes", "running")); got != "23514" {
		t.Fatalf("an unrecognised posture was accepted (code %q, want 23514): the reaper would probe the wrong "+
			"kind of operating-system object and read a running build as an exited one", got)
	}
	if got := pgCode(insert("bgt-bad-state", "unsandboxed-host", "zombie")); got != "23514" {
		t.Fatalf("an unrecognised state was accepted (code %q, want 23514)", got)
	}
	if err := insert("bgt-ok", "unsandboxed-host", "running"); err != nil {
		t.Fatalf("a well-formed background task was refused: %v", err)
	}

	// UNIQUE (tool_call_id): one call spawns one process. A retried dispatch that reached the operating
	// system twice would otherwise leave two live processes under one ledger row, and only one of them
	// would ever be reaped.
	if got := pgCode(insert("bgt-duplicate", "unsandboxed-host", "running")); got != "23505" {
		t.Fatalf("a second background task was accepted for the same tool_call_id (code %q, want 23505)", got)
	}

	// The PARTIAL index the reaper reads through. It is asserted by its predicate, not just its name: an
	// index over the whole table would still be an index and would still be called this.
	var indexDef string
	if err := pool.QueryRow(ctx, `SELECT indexdef FROM pg_indexes
	     WHERE schemaname='public' AND indexname='background_tasks_running_idx'`).Scan(&indexDef); err != nil {
		t.Fatalf("background_tasks_running_idx is absent: %v", err)
	}
	if !strings.Contains(indexDef, "WHERE (state = 'running'") {
		t.Fatalf("background_tasks_running_idx is not partial on the running state: %s", indexDef)
	}

	// Grants: UPDATE yes, DELETE no, re-asserted after the second boot. A row moves through its life cycle
	// and takes a notified_at stamp; it is never removed, because an operator hunting orphans needs the
	// `lost` rows to still be there. What a TTL deletes is the log FILE, which is bytes in an allocation.
	assertPriv := func(priv string, want bool) {
		t.Helper()
		var got bool
		if err := pool.QueryRow(ctx, `SELECT has_table_privilege('palai_app', 'background_tasks', $1)`, priv).Scan(&got); err != nil {
			t.Fatalf("has_table_privilege(background_tasks, %s) error = %v", priv, err)
		}
		if got != want {
			t.Fatalf("palai_app %s on background_tasks = %v, want %v (000047's grants eroded across reboots)", priv, got, want)
		}
	}
	assertPriv("SELECT", true)
	assertPriv("INSERT", true)
	assertPriv("UPDATE", true)
	assertPriv("DELETE", false)

	// And as the runtime role itself: the privilege check refuses before RLS is even consulted.
	if _, err := conn.Exec(ctx, `SET ROLE palai_app`); err != nil {
		t.Fatalf("SET ROLE palai_app error = %v", err)
	}
	defer func() { _, _ = conn.Exec(ctx, `RESET ROLE`) }()
	if got := pgCode(mustFail(conn.Exec(ctx, `DELETE FROM background_tasks`))); got != "42501" {
		t.Fatalf("background_tasks DELETE code = %q, want 42501 — the row is the audit record that a process existed", got)
	}
}

// seedBackgroundTaskParents writes the FK chain one background task needs. It is a helper rather than
// inline SQL because the chain is six tables deep and the test above is about the seventh.
func seedBackgroundTaskParents(t *testing.T, ctx context.Context, conn interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}) (project, session, response, run, call string) {
	t.Helper()
	project = newID("prj-bgt")
	session, response, run, call = newID("ses-bgt"), newID("resp-bgt"), newID("run-bgt"), newID("call-bgt")
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO projects (id) VALUES ($1)`, []any{project}},
		{`INSERT INTO sessions (id, project_id) VALUES ($1,$2)`, []any{session, project}},
		{`INSERT INTO responses (id, project_id, session_id, input) VALUES ($1,$2,$3,'{}'::jsonb)`,
			[]any{response, project, session}},
		{`INSERT INTO runs (id, project_id, session_id, response_id) VALUES ($1,$2,$3,$4)`,
			[]any{run, project, session, response}},
		{`INSERT INTO tool_calls (id, project_id, run_id, name) VALUES ($1,$2,$3,'palai.workspace.shell')`,
			[]any{call, project, run}},
	} {
		if _, err := conn.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed background task parents: %v", err)
		}
	}
	return project, session, response, run, call
}

// TestMigration48SessionListIndexesAndLabel is the 000048 half of the Sessions screen, and the three
// things it checks are the three that would be silently absent otherwise.
//
// FIRST, the label exists and is NOT unique. The reference screen shows several sessions carrying one
// name, so a unique index here would reject a legitimate rename — the check is that two sessions in the
// same project can hold the same name, which is what a later "tidy this up with a unique index" would
// break.
//
// SECOND, the four indexes are present BY NAME. Three of them are why the enriched row is affordable at
// all, and the fourth is a repair: `sessions` had carried only its primary key since 000001, so every
// page of every session list sequentially scanned and sorted the whole table. An index that quietly
// stopped being created would not fail a single behavioural test — the query returns the same rows —
// which is exactly why it is asserted here rather than trusted.
//
// THIRD, the plan. Asserting the index EXISTS is not asserting the query USES it: a wrong column order
// leaves the index in the catalogue and the Seq Scan in the plan. So the list query's own shape is run
// through EXPLAIN and the plan is required to contain no sequential scan of `sessions`.
func TestMigration48SessionListIndexesAndLabel(t *testing.T) {
	cs := openHarness(t)
	ctx := storage.WithSystemScope(context.Background())
	pool := cs.Pool()

	// A second boot re-runs the whole chain; every object is IF NOT EXISTS / idempotent.
	if err := cs.Migrate(context.Background()); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	if !columnExists(t, pool, "sessions", "name") {
		t.Fatal("after apply, sessions.name is missing")
	}
	for _, index := range []string{
		"sessions_tenant_keyset_idx",
		"responses_session_created_idx",
		"runs_session_created_idx",
		"usage_ledger_session_idx",
	} {
		if !indexExists(t, pool, index) {
			t.Fatalf("after apply, 000048's %s is missing — the session list and its aggregates go back to sequential scans", index)
		}
	}

	project := newID("prj")
	exec(t, pool, `INSERT INTO projects (id) VALUES ($1)`, project)
	// Two sessions, ONE label. A name is a label, not an identity.
	for _, id := range []string{newID("ses"), newID("ses")} {
		exec(t, pool, `INSERT INTO sessions (id, project_id, name) VALUES ($1,$2,$3)`,
			id, project, "Gece Doğrulama")
	}
	var shared int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM sessions WHERE  project_id=$1 AND name='Gece Doğrulama'`, project).Scan(&shared); err != nil {
		t.Fatalf("count shared labels: %v", err)
	}
	if shared != 2 {
		t.Fatalf("two sessions sharing one label = %d rows, want 2 — the label must not be unique", shared)
	}

	// The plan. `sessions` is tiny in this harness and a tiny table is where Postgres prefers a
	// sequential scan whatever indexes exist, so the planner is told — for this statement only, SET
	// LOCAL, dying with the tx — that a sequential scan is expensive.
	//
	// WHAT THIS CATCHES AND WHAT IT DOES NOT, because a plan assertion that overstates itself is worse
	// than none. It catches the index disappearing (the plan then has nothing to name) and it catches a
	// Sort reappearing, which is the shape the list had before it was indexed. It does NOT catch a wrong
	// column ORDER: measured 2026-07-31 on a fresh database, an index built with the keyset columns FIRST
	// produces a plan textually identical to the correct one — same Index Scan, same Index Cond — and
	// differs only in estimated cost
	// (15.16 vs 8.17). EXPLAIN cannot tell them apart, so neither can this test. The column order is
	// held by review and by the migration's own comment, not here.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin plan tx: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatalf("disable seqscan: %v", err)
	}
	rows, err := tx.Query(ctx, `EXPLAIN SELECT id, state, created_at, name FROM sessions
	     WHERE project_id = $1
	     ORDER BY created_at DESC, id DESC LIMIT 21`, project)
	if err != nil {
		t.Fatalf("explain session page: %v", err)
	}
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			rows.Close()
			t.Fatalf("scan plan line: %v", err)
		}
		plan.WriteString(line)
		plan.WriteString("\n")
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate plan: %v", err)
	}
	if !strings.Contains(plan.String(), "sessions_tenant_keyset_idx") {
		t.Fatalf("the session page's plan does not use sessions_tenant_keyset_idx; its column order no longer "+
			"matches project_id equality followed by the (created_at DESC, id DESC) keyset.\n%s",
			plan.String())
	}
	// And no Sort: the index supplies the order, which is the whole reason its trailing columns carry
	// their direction. A Sort here means every page re-sorts what it read.
	if strings.Contains(plan.String(), "Sort") {
		t.Fatalf("the session page SORTS; the keyset index is supposed to supply (created_at DESC, id DESC) directly.\n%s",
			plan.String())
	}
}
