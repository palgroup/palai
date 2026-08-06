//go:build security

// Package tenancy is the E13 Task 1 cross-tenant negative corpus (TEN-001/TEN-002). It proves the
// isolation the application's WHERE clauses claim is ALSO enforced one layer down, by Postgres row
// level security: a deliberately WHERE-less query issued on the runtime role returns only the
// tenant named by the transaction's palai.org_id, a connection that never set the GUC sees nothing,
// and a write that names a foreign organization is refused by the policy's WITH CHECK.
//
// The corpus drives raw pgx pools on purpose — it tests the DATABASE, not the Go pool wrapper, so it
// stays honest if the pool's context plumbing ever regresses.
//
// Honest ceiling: one database, one runtime role reached by SET ROLE from the owner connection. This
// stops a missing WHERE clause in application SQL; it does NOT stop a compromised control-plane
// process (which can RESET ROLE) or a hostile DBA, and nothing here is encrypted at rest. Those are
// E13-H/E15 (see storage/migrations/000029_row_level_security.up.sql).
package tenancy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/storage"
)

// runtimeRole is the non-owner role migration 000029 creates and the app connection runs as.
const runtimeRole = "palai_app"

// nonTenantTables are the tables migration 000029 deliberately leaves outside RLS because they hold
// no tenant data: the migration ledger, the coordinator's host quarantine registry, and the
// per-session monotonic counter (a session id and an integer). Listing them here is the point: a NEW
// tenant-scoped table that forgets its policy fails TestEveryTenantTableIsRowLevelSecured rather than
// silently leaking.
var nonTenantTables = map[string]bool{
	"schema_migrations": true,
	// schema_revisions is the 000033 boot-migration journal (E15 T1): installation-global, no
	// organization_id, append-only by its own REVOKE. Like schema_migrations it holds no tenant data, so it
	// is deliberately outside RLS. It was added to the chain (000033) without this allowlist entry, so this
	// gate failed on it in the Docker-bound security tier (not run by `make verify`); the entry is added
	// here with the E17 wave-1 migrations that first re-exercised the corpus.
	"schema_revisions":  true,
	"host_quarantine":   true,
	"session_sequences": true,
	// deployment_desired is the 000052 desired-configuration journal (E29): the configuration of the
	// PROCESS, appended by the admin panel and applied by the next bring-up. It is outside RLS because it
	// carries no tenant column, and it carries no tenant column ON PURPOSE rather than by omission — four
	// of the eleven settings it may hold are the ADMISSION BOUNDS that exist to hold a tenant
	// (PALAI_MAX_CONCURRENT_RUNS, PALAI_MAX_QUEUED_RUNS, PALAI_REQUEST_RATE_PER_SEC, PALAI_REQUEST_BURST),
	// so a per-tenant home for them would let a tenant raise the limit that bounds it. With no tenant
	// column there is nothing a later policy could key on, and the check above turns that into a test: add
	// organization_id to this table and this allowlist entry becomes a FAILURE rather than an exemption.
	"deployment_desired": true,
}

// suite holds the two connections the corpus contrasts: the migration owner (which seeds fixtures
// and is deliberately NOT subject to its own policies except under FORCE) and the runtime role the
// control plane actually serves on.
type suite struct {
	owner *pgxpool.Pool
	app   *pgxpool.Pool
	// The two tenants this corpus contrasts. They are PROJECTS since A.2 Task 6: a project is the tenant,
	// and there is no wider id left to hold one pair of them apart.
	projectA string
	projectB string
}

func newSuite(t *testing.T) *suite {
	t.Helper()
	url := os.Getenv("PALAI_SECURITY_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_SECURITY_POSTGRES_URL is not set; run via `make test-security TEST=tenancy`")
	}
	ctx := context.Background()

	owner, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("open owner pool: %v", err)
	}
	t.Cleanup(owner.Close)
	if _, err := owner.Exec(ctx, storage.MigrationUp()); err != nil {
		t.Fatalf("apply migration chain: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse app URL: %v", err)
	}
	// Every app connection runs as the non-owner runtime role — the same switch storage.OpenPool
	// performs in production.
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE "+runtimeRole)
		return err
	}
	app, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open app pool: %v", err)
	}
	t.Cleanup(app.Close)

	s := &suite{owner: owner, app: app, projectA: newID("prj"), projectB: newID("prj")}
	s.seedTenant(t, s.projectA)
	s.seedTenant(t, s.projectB)
	return s
}

// seedTenant writes one organization -> project -> session -> run -> artifact, as the owner. The run and
// artifact rows are the corpus's canaries: they are rows a WHERE-less SELECT must not return across the
// tenant boundary. The artifact makes DAT-006's cross-tenant denial first-class at the DB layer (E13 T5) —
// the retrieval API's 404 is only honest because the row is invisible one layer down.
func (s *suite) seedTenant(t *testing.T, project string) {
	t.Helper()
	ctx := context.Background()
	session, response, run, artifact := newID("ses"), newID("resp"), newID("run"), newID("art")
	environment := newID("env")
	tool, toolRevision, setRevision := newID("tool"), newID("trev"), newID("tsrev")
	toolCall, backgroundTask := newID("call"), newID("bgt")
	runnerPool, webhookEndpoint := newID("pool"), newID("whe")
	trigger, schedule, deletedSchedule, occurrence, hook := newID("trg"), newID("sch"), newID("sch"), newID("occ"), newID("hook")
	// The E30 repository-binding pair (000057): one LIVE, one ARCHIVED. Two rows rather than one because
	// every claim this suite makes about them is a claim about the DIFFERENCE — the list hides one, the
	// admission guard refuses one, and a single row could not tell either apart from "returned nothing".
	liveBinding, archivedBinding := newID("repo"), newID("repo")
	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO projects (id) VALUES ($1)`, []any{project}},
		{`INSERT INTO sessions (id, project_id) VALUES ($1, $2)`, []any{session, project}},
		{`INSERT INTO responses (id, project_id, session_id, input) VALUES ($1, $2, $3, '{}'::jsonb)`,
			[]any{response, project, session}},
		{`INSERT INTO runs (id, project_id, session_id, response_id) VALUES ($1, $2, $3, $4)`,
			[]any{run, project, session, response}},
		{`INSERT INTO artifacts (id, project_id, run_id, object_key, size_bytes, checksum) VALUES ($1, $2, $3, $4, $5, $6)`,
			[]any{artifact, project, run, project + "/" + project + "/" + run + "/" + artifact, 12, "sha256:deadbeef"}},
		// THE REPOSITORY BINDINGS (000009, archived_at from 000057). `connection_ref` is seeded NON-EMPTY on
		// the live row so the credential-rotation statement below has something to overwrite — an UPDATE
		// asserted against a column that was already '' cannot tell "the write was denied" from "the write
		// landed and changed nothing".
		{`INSERT INTO repository_bindings (id, project_id, provider, repository_identity, clone_url, connection_ref)
		  VALUES ($1, $2, 'github', 'palai/live', 'https://example.invalid/live.git', $3)`,
			[]any{liveBinding, project, "seeded-ref-" + project}},
		{`INSERT INTO repository_bindings (id, project_id, provider, repository_identity, clone_url, archived_at)
		  VALUES ($1, $2, 'github', 'palai/retired', 'https://example.invalid/retired.git', now())`,
			[]any{archivedBinding, project}},
		// The E25 T3 environment pair, PROJECT-scoped since 000006 — they were installation-wide for one
		// phase, which is why the surrounding comments changed with them.
		//
		// THEY ARE SEEDED BECAUSE THE CATALOGUE-DRIVEN TESTS ALONE ARE NOT ENOUGH. TestEveryTenantTableIsRow-
		// LevelSecured and TestConnectionWithoutTenantContextSeesNoTenantRows both walk pg_class, so they cover
		// a new table the moment it exists — but they can only prove "no rows come back", and a table with no
		// rows in it satisfies that vacuously. The canary below needs a row in EACH project to have anything to
		// fail on.
		//
		// THE NAME STAYS PER-TENANT EVEN THOUGH 000006 NO LONGER REQUIRES IT. Uniqueness went
		// `(organization_id, name)` -> `(name)` -> `(project_id, name)`, so two tenants may both own a
		// "production" again. The suffix is kept because this database is RETAINED between runs and shared:
		// a literal would collide with an earlier run's row in the SAME project. That is a fixture reason,
		// not a schema one, and saying which is the point — the middle phase's version of this comment
		// claimed the schema forced it, and a reader would have deleted the suffix on seeing the constraint
		// change.
		{`INSERT INTO environments (id, project_id, name) VALUES ($1, $2, $3)`,
			[]any{environment, project, "production-" + project}},
		{`INSERT INTO environment_values (environment_id, key) VALUES ($1, $2)`,
			[]any{environment, "JIRA_TOKEN"}},
		// usage_ledger, and it was NOT seeded until 000006 — which made the one claim its own test carried
		// unmeasurable. TestOnlyUsageLedgerStaysInstallationWide (below, then called
		// TestInstallationWideTablesAreVisibleToEveryTenant) named four tables and drove its VISIBILITY arm
		// against two of them; usage_ledger and secret_refs appeared only in the deny-by-default loop, where
		// every table answers zero. So "usage_ledger stays installation-wide" was asserted by a test name and
		// by a paragraph, and by no statement. It is the only such claim left in that test now, so the row
		// exists for it to be about.
		{`INSERT INTO usage_ledger (id, project_id, meter, quantity, unit, dedupe_key)
		  VALUES ($1, $2, 'model.input_tokens', 1, 'token', $3)`,
			[]any{"usg_" + project, project, "seed-" + project}},
		// The E25 T7 registry trio, and these are PROJECT-scoped (000024) — the opposite of the environment
		// pair above, which is why they are seeded separately rather than folded into it.
		//
		// THREE READ ROUTES LAND ON THESE TABLES: GET /v1/tools/{tool_id}/revisions and GET
		// /v1/tools/{tool_id}/revisions/{revision_id} read tool_revisions, and GET
		// /v1/tool-sets/{set}/revisions/{revision_id} reads tool_set_revisions. All carry an
		// organization/project predicate in their SQL, and that predicate is exactly the kind of claim this
		// corpus exists to distrust: the canary below issues the WHERE-LESS form of the same read and
		// requires the DATABASE to return nothing. The description is a REAL untrusted string here rather
		// than '' — a cross-tenant leak of this column would be an MCP server's text reaching another
		// tenant's console, which is the specific consequence worth having a row to fail on.
		{`INSERT INTO tools (id, project_id, canonical_name, model_visible_name) VALUES ($1,$2,$3,$4)`,
			[]any{tool, project, "mcp.jira.getJiraIssue", "jira__getJiraIssue"}},
		{`INSERT INTO tool_revisions (id, project_id, tool_id, revision_number, executor, description, input_schema, digest)
		  VALUES ($1,$2,$3,1,'mcp',$4,'{"type":"object"}'::jsonb,'sha256:tenancyseed')`,
			[]any{toolRevision, project, tool, "Get the details of a Jira issue by its key."}},
		{`INSERT INTO tool_set_revisions (id, project_id, set_name, revision_number, tool_pins, digest)
		  VALUES ($1,$2,'jira',1,jsonb_build_array(jsonb_build_object('tool_revision_id',$3::text)),'sha256:tenancyseed')`,
			[]any{setRevision, project, toolRevision}},
		// The E26 T1 background task (000047), PROJECT-scoped like the tool_calls row it hangs off. It needs
		// that row first: `UNIQUE (tool_call_id)` makes the FK the identity of the pair, so the seed writes
		// both rather than pointing at a call that never existed.
		//
		// WHAT MAKES THIS CANARY WORTH A ROW rather than only the catalogue sweep: `handle` names a LIVE
		// OPERATING-SYSTEM OBJECT — a container id, or a `pgid:starttime` on the machine the control plane
		// runs on. A cross-tenant read of this column would hand one tenant a handle the reaper signals, so
		// the leak this seed exists to fail on is not a disclosure, it is a kill.
		{`INSERT INTO tool_calls (id, project_id, run_id, name) VALUES ($1,$2,$3,'palai.workspace.shell')`,
			[]any{toolCall, project, run}},
		{`INSERT INTO background_tasks (id, project_id, run_id, session_id, response_id, tool_call_id, posture, handle, state, output_path)
		  VALUES ($1,$2,$3,$4,$5,$6,'unsandboxed-host','4242:2026-07-30T12:00:00Z','running','.palai-session/bg/'||$1||'.log')`,
			[]any{backgroundTask, project, run, session, response, toolCall}},
		// The E28 T1 pool (000045, PROJECT-scoped). The table has been tenant-policied since E24 and the
		// catalogue sweeps have covered it since — but only VACUOUSLY, because nothing seeded a row: E24's
		// surface was read-only and every pool in this corpus's database belonged to nobody in it.
		//
		// WHAT MAKES IT WORTH A ROW NOW. T1 opened `POST /v1/runner-pools` and
		// `PATCH /v1/runner-pools/{pool_id}`, so a pool is a thing an operator CREATES and MUTATES rather than
		// a row a migration seeded. `posture` decides WHERE a tenant's runs execute and `strict_enrollment`
		// decides whether a machine joining that pool needs a human — so a cross-tenant read of this row hands
		// one tenant the name of another's Mac pool, and a cross-tenant write places another tenant's runs on
		// hardware they did not choose or opens their waiting room. The canary below is what has something to
		// fail on; TestForeignPoolWriteAndUpdateAreRejected is the write half.
		{`INSERT INTO runner_pools (id, project_id, name, posture, os, arch, strict_enrollment)
		  VALUES ($1,$2,'mac-pool','unsandboxed-host','darwin','arm64',true)`,
			[]any{runnerPool, project}},
		// The E29 T1 automation trio (000022 schedules + schedule_occurrences, 000028 hooks), all
		// PROJECT-scoped. Neither table needed registering: 000029 drives its sweep off the CATALOGUE, so
		// every table carrying organization_id was policied the moment the chain re-ran — which
		// TestEveryTenantTableIsRowLevelSecured has been asserting all along.
		//
		// WHAT MAKES THEM WORTH A ROW NOW, which is the only thing the catalogue sweeps cannot supply. Until
		// T1 there was no route that ENUMERATED either table: a schedule and a hook were reachable only by an
		// id the caller already held, so the cross-tenant question was "can B guess A's id", and a guess is
		// bounded by 128 bits. A LIST is a different question — it asks the database for every row it can
		// see, and what it can see is decided here rather than by any WHERE clause T1 wrote. The canary below
		// issues the WHERE-LESS form of exactly those two new reads.
		//
		// The consequence if it leaked is not symmetric with the other rows. A schedule row names a
		// trigger and a principal, and it FIRES on a wall clock — cross-tenant visibility of one is the map
		// of when another tenant's automation runs and as whom. A hook row names an extension point that
		// executes inside every run of its project plus the secret_ref handle its remote worker signs with.
		//
		// The SOFT-DELETED schedule beside it is the second row for a different reason: `deleted_at` is an
		// APPLICATION predicate, not an RLS one, so this pair is what lets a test ask whether a tombstone is
		// filtered by the shipped query rather than by the database — see TestTheAutomationListQueries.
		{`INSERT INTO triggers (id, project_id, name, type) VALUES ($1,$2,$3,'cron')`,
			[]any{trigger, project, "nightly-trigger"}},
		{`INSERT INTO schedules (id, project_id, name, trigger_id, created_by, cron_expr, timezone, status)
		  VALUES ($1,$2,'nightly-close-the-books',$3,$4,'0 2 * * *','Europe/Istanbul','paused')`,
			[]any{schedule, project, trigger, project + "-principal"}},
		{`INSERT INTO schedules (id, project_id, name, trigger_id, created_by, cron_expr, timezone, deleted_at)
		  VALUES ($1,$2,'retired-schedule',$3,$4,'0 3 * * *','UTC',clock_timestamp())`,
			[]any{deletedSchedule, project, trigger, project + "-principal"}},
		{`INSERT INTO schedule_occurrences (occurrence_id, schedule_id, schedule_revision, planned_at, state)
		  VALUES ($1,$2,1,clock_timestamp(),'admitted')`,
			[]any{occurrence, schedule}},
		{`INSERT INTO hooks (id, project_id, name, hook_point, category, executor, config, secret_ref)
		  VALUES ($1,$2,'deny-external-writes','before_tool','policy','remote_http','{"url":"https://hooks.example.test/deny"}'::jsonb,$3)`,
			[]any{hook, project, "HOOK_SIGNING_KEY"}},
		// The E29 T3 webhook endpoint (000020, PROJECT-scoped). Like the pool above, this table has been
		// tenant-policied for epics and covered by the catalogue sweeps only VACUOUSLY — nothing seeded a row.
		//
		// WHAT MAKES IT WORTH A ROW NOW, and it is a different reason from every other seed here: T3 opened
		// `DELETE /v1/webhook-endpoints/{id}`, and DELETE is the verb this corpus had never issued. The three
		// tests below this seed cover a WHERE-less SELECT, a foreign INSERT and a foreign UPDATE; the
		// destructive verb was never asked about, so the policy's coverage of it was an assumption. A
		// cross-tenant delete here does not disclose anything — it stops another tenant's outbound
		// notifications and tells nobody, which is a failure they discover from the absence of alerts.
		//
		// url is a real address and signing_secret_ref a real handle for the same reason the tool description
		// above is real text: a cross-tenant READ of this row hands one tenant the address another sends its
		// events to, and the name of the secret that signs them.
		{`INSERT INTO webhook_endpoints (id, project_id, url, signing_secret_ref)
		  VALUES ($1,$2,$3,'secret_ref_tenancyseed')`,
			[]any{webhookEndpoint, project, "https://hooks." + project + ".example/events"}},
	}
	for _, stmt := range stmts {
		if _, err := s.owner.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed %s: %v", project, err)
		}
	}
}

// TestWhereLessQueryIsRejectedByTheDatabase is the essence of TEN-002: the fixture query is
// deliberately unscoped — no organization_id predicate at all, the shape an application bug would
// produce — and the database still returns only the caller's tenant.
func TestWhereLessQueryIsRejectedByTheDatabase(t *testing.T) {
	s := newSuite(t)
	// The seeded row count per table. It is spelled out rather than assumed to be 1 because `schedules`
	// carries TWO — a live one and a soft-deleted one — and a hard-coded 1 would have failed on the second
	// for a reason that has nothing to do with tenancy.
	seeded := map[string]int{
		"runs": 1, "responses": 1, "sessions": 1, "projects": 1, "artifacts": 1,
		"tools": 1, "tool_revisions": 1,
		"tool_set_revisions": 1, "background_tasks": 1, "runner_pools": 1,
		// The E29 T1 automation trio. `schedules` is 2: the second is a tombstone, and it belongs in this
		// count because RLS is the layer that decides VISIBILITY while `deleted_at` is the layer that
		// decides RELEVANCE — a leak here would expose a deleted schedule as readily as a live one.
		"triggers": 1, "schedules": 2, "hooks": 1,
		// environments and environment_values CAME BACK to this map with 000006, and the round trip is the
		// record. They left it in A.2 Task 6 — not silently, and the comment that carried them out said so:
		// with no project_id on either table the canary's claim ("the caller sees ONE row and the other
		// tenant's zero") had become false about them, so they moved to
		// TestInstallationWideTablesAreVisibleToEveryTenant, which asserted the OPPOSITE property. 000006
		// gives environments a project_id and environment_values its parent's, so the original claim is true
		// again and this is where it belongs.
		"environments": 1, "environment_values": 1,
	}
	for table, want := range seeded {
		s.asProject(t, s.projectA, func(tx pgx.Tx) {
			var foreign int
			// The only predicate names the OTHER tenant: the query asks for exactly the rows the
			// caller must never see, so a non-zero count is a leak and nothing else.
			//
			// TWO TABLES CANNOT BE ASKED WITH A BARE COLUMN, and each is a different shape rather than an
			// exception to one rule:
			//   - `projects` IS its own tenant key; the policy compares its `id`.
			//   - `environment_values` carries no tenant column at all. 000006 gave it
			//     palai_apply_child_policy, so its project is its PARENT's — the predicate has to reach
			//     through the FK, and a bare `project_id` here fails with 42703 rather than passing
			//     vacuously. That error is what put this branch here.
			query := fmt.Sprintf(`SELECT count(*) FROM %s WHERE project_id = $1`, table)
			switch table {
			case "projects":
				query = `SELECT count(*) FROM projects WHERE id = $1`
			case "environment_values":
				query = `SELECT count(*) FROM environment_values v
				           JOIN environments e ON e.id = v.environment_id
				          WHERE e.project_id = $1`
			}
			if err := tx.QueryRow(context.Background(), query, s.projectB).Scan(&foreign); err != nil {
				t.Fatalf("%s: count foreign rows: %v", table, err)
			}
			if foreign != 0 {
				t.Fatalf("%s: WHERE-less query saw %d row(s) of the foreign tenant; RLS did not deny", table, foreign)
			}
			var visible int
			if err := tx.QueryRow(context.Background(), fmt.Sprintf(`SELECT count(*) FROM %s`, table)).Scan(&visible); err != nil {
				t.Fatalf("%s: count visible rows: %v", table, err)
			}
			// runner_pools GAINED A SECOND LEGITIMATE SOURCE OF ROWS, and the count is corrected rather
			// than loosened. A pool with no project is the PLANE's and every tenant may see it
			// (palai_apply_fleet_policy), so `visible == seeded` stopped being a statement about tenancy
			// the moment one existed — it became a statement about how many free pools the corpus happens
			// to have created, which is not this test's subject.
			//
			// The free rows are counted by the OWNER, so the subtraction cannot be inflated by the very
			// policy under test: if RLS began exposing a foreign PRIVATE pool, visible would exceed
			// want+free and this still fails. `>= want` would not have.
			if table == "runner_pools" {
				var free int
				if err := s.owner.QueryRow(context.Background(),
					`SELECT count(*) FROM runner_pools WHERE project_id IS NULL`).Scan(&free); err != nil {
					t.Fatalf("count free pools as the owner: %v", err)
				}
				want += free
			}
			if visible != want {
				t.Fatalf("%s: own tenant sees %d row(s), want exactly the %d seeded (plus any free pools)",
					table, visible, want)
			}
		})
	}
}

// TestOnlyUsageLedgerStaysInstallationWide is the OTHER half of the sweep above, and it has now asserted
// the property in BOTH directions on the same tables — which is the whole reason it was not deleted when
// the direction changed.
//
// IT WAS TestInstallationWideTablesAreVisibleToEveryTenant AND IT NAMED FOUR TABLES. A.2 Task 6 removed a
// boundary rather than moving it: environments, environment_values and secret_refs carried no project_id at
// all, so 000002 could only key them on the installation, and this test asserted that a scoped tenant READ
// the other tenant's rows. Its own comment explained why it existed in that form: "a test suite that merely
// dropped these tables from the confinement canary would leave a reader with the impression the boundary is
// still there."
//
// Migration 000006 puts the boundary back on three of the four. Those three returned to the confinement
// canary above; usage_ledger stays HERE, and it is the reason this test survives rather than being deleted
// with them.
//
// WHY usage_ledger MUST STAY WIDE, restated because it is now the ONLY claim this test carries: it does
// carry a project_id, but an installation-wide budget that summed only the caller's own project would fail
// OPEN. Narrowing it is a change that looks like tidying and silently disables a spend ceiling, so it is
// asserted rather than assumed.
//
// The guarantee that survived every phase is the first assertion: a connection that declared no scope sees
// nothing, because the policy expression is `system OR palai.project_id IS NOT NULL`, never `true`.
func TestOnlyUsageLedgerStaysInstallationWide(t *testing.T) {
	s := newSuite(t)
	ctx := context.Background()

	// THE DENY-BY-DEFAULT ARM RUNS FIRST, AND THE ORDER IS LOAD-BEARING. A custom GUC never returns to
	// NULL once any transaction on that connection has set it — not even a TRANSACTION-LOCAL set, measured
	// against the pinned image:
	//
	//	never set                                    -> current_setting(...) IS NULL = t
	//	BEGIN; set_config('palai.tx1','v',true); COMMIT  -> IS NULL = f, value ''
	//
	// s.asProject publishes exactly that way, so a connection borrowed AFTER the visibility loop below has a
	// non-NULL project GUC and this policy admits it. Asserted before anything has published, this measures
	// the world it names; asserted after, it would measure the harness.
	//
	// IT STILL COVERS ALL FOUR TABLES even though three are now project-keyed, and that is deliberate: the
	// project-keyed expression has the same deny-by-default arm, so this is the one assertion that did not
	// need to change when the policies did. A table that lost row-level security entirely — rather than
	// being re-keyed — fails here.
	fresh, err := s.app.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	for _, table := range []string{"environments", "environment_values", "usage_ledger", "secret_refs"} {
		var visible int
		if err := fresh.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s`, table)).Scan(&visible); err != nil {
			fresh.Release()
			t.Fatalf("%s: count from a never-scoped connection: %v", table, err)
		}
		if visible != 0 {
			fresh.Release()
			t.Fatalf("%s: a connection that never declared a scope saw %d row(s); the policy is not deny-by-default", table, visible)
		}
	}
	fresh.Release()

	// usage_ledger STAYS WIDE. Named by the other tenant's own dedupe key rather than counted in total: this
	// suite re-seeds two tenants per test against one shared database, so a total says only "something is
	// visible" while the other tenant's specific row says the reach is still there.
	foreignUsage := "seed-" + s.projectB
	s.asProject(t, s.projectA, func(tx pgx.Tx) {
		var seen int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM usage_ledger WHERE dedupe_key = $1`, foreignUsage).Scan(&seen); err != nil {
			t.Fatalf("usage_ledger: count the other tenant's row: %v", err)
		}
		if seen != 1 {
			t.Fatalf("usage_ledger: the other tenant's seeded row is visible %d time(s), want 1 — this table "+
				"must stay installation-wide or an installation-wide budget sums only the caller and fails OPEN", seen)
		}
	})

	// AND THE THREE THAT NARROWED ARE CLOSED. Asserted here as well as in the confinement canary above,
	// because the two ask different questions: the canary asks "does the caller see its OWN row", and this
	// asks "does it see the OTHER tenant's". A policy that admitted everything would pass the canary.
	foreignEnv := "production-" + s.projectB
	s.asProject(t, s.projectA, func(tx pgx.Tx) {
		var seen int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM environments WHERE name = $1`, foreignEnv).Scan(&seen); err != nil {
			t.Fatalf("environments: count the other tenant's row: %v", err)
		}
		if seen != 0 {
			t.Fatalf("environments: the other tenant's row is visible %d time(s), want 0 — 000006 keys this "+
				"table on project_id", seen)
		}
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM environment_values v
			   JOIN environments e ON e.id = v.environment_id
			  WHERE e.name = $1`, foreignEnv).Scan(&seen); err != nil {
			t.Fatalf("environment_values: count the other tenant's row: %v", err)
		}
		if seen != 0 {
			t.Fatalf("environment_values: the other tenant's row is visible %d time(s), want 0 — its policy "+
				"resolves its PARENT's project, so a 1 here means the child policy is not installed", seen)
		}
	})
}

// TestTheAutomationListQueriesAreConfinedAndFilterTheirTombstones runs the SHIPPED named queries E29 T1
// added — ListSchedules, ListHooks, ListScheduleOccurrences — on the runtime role, and asks the two
// questions that only the real statements can answer.
//
// WHY THE SHIPPED SQL RATHER THAN A FIXTURE QUERY, which is what the rest of this corpus uses. The canary
// above proves the DATABASE denies a WHERE-less read; that is a claim about RLS and a hand-written query is
// the right instrument for it. These three are a claim about a STATEMENT — that the text T1 shipped, with
// the parameters the handler binds, returns what it should — and there is no way to make that claim with a
// query written here: a fixture that resembled the shipped one would prove the resemblance. storage.Query
// resolves the same embedded text the control plane runs.
//
// TWO FAILURES ARE SEPARATED BECAUSE THEY LIVE IN DIFFERENT LAYERS AND ONLY ONE OF THEM IS RLS.
//
//   - A foreign row coming back is a TENANCY failure: the statements carry their own organization/project
//     predicate AND run under the policy, and this asks whether either is load-bearing by binding the
//     FOREIGN tenant's ids into the shipped statement while the connection declares org A. The database
//     must return nothing even though the parameters ask for everything.
//   - A tombstone coming back is an APPLICATION failure: `deleted_at IS NULL` is a clause in the query and
//     nothing beneath it enforces that. No policy, no constraint, no role. If the clause is deleted, every
//     layer below this one is still perfectly correct and the list still resurrects the dead.
func TestTheAutomationListQueriesAreConfinedAndFilterTheirTombstones(t *testing.T) {
	s := newSuite(t)
	ctx := context.Background()
	projectA, projectB := s.projectA, s.projectB

	s.asProject(t, s.projectA, func(tx pgx.Tx) {
		// ListSchedules bound to org A: exactly the ONE live schedule. The tombstone is seeded and must not
		// appear, and it is the only thing distinguishing this assertion from the canary above.
		names := scanNames(t, ctx, tx, storage.Query("ListSchedules"),
			projectA, nil, nil, nil, "", nil, 100)
		if len(names) != 1 || names[0] != "nightly-close-the-books" {
			t.Fatalf("ListSchedules(org A) = %v, want exactly [nightly-close-the-books].\n"+
				"A second name here is the soft-deleted row: `deleted_at IS NULL` is an APPLICATION predicate "+
				"and no policy, constraint or role re-states it — if the clause goes, this is the only thing "+
				"that notices.", names)
		}

		// The same statement, parameters naming the FOREIGN tenant, connection still declaring org A. The
		// query's own predicate says orgB; the policy says orgA; the intersection must be empty.
		foreign := scanNames(t, ctx, tx, storage.Query("ListSchedules"),
			projectB, nil, nil, nil, "", nil, 100)
		if len(foreign) != 0 {
			t.Fatalf("ListSchedules asked for the FOREIGN tenant returned %v; RLS did not confine the shipped statement", foreign)
		}

		hooks := scanNames(t, ctx, tx, storage.Query("ListHooks"), projectA, nil, nil, nil, "", 100)
		if len(hooks) != 1 || hooks[0] != "deny-external-writes" {
			t.Fatalf("ListHooks(org A) = %v, want exactly [deny-external-writes]", hooks)
		}
		foreignHooks := scanNames(t, ctx, tx, storage.Query("ListHooks"), projectB, nil, nil, nil, "", 100)
		if len(foreignHooks) != 0 {
			t.Fatalf("ListHooks asked for the FOREIGN tenant returned %v; RLS did not confine the shipped statement", foreignHooks)
		}

		// The occurrence log is scoped THROUGH its parent schedule — schedule_occurrences carries no
		// organization_id of its own — so the confinement being asked about here is a JOIN's, and its policy
		// is the EXISTS sub-select 000029 wrote for exactly this shape. Own tenant: the one seeded row.
		// Foreign tenant, with the foreign schedule's real id bound in: nothing.
		if n := countRows(t, ctx, tx, storage.Query("ListScheduleOccurrences"),
			s.scheduleOf(t, s.projectA), projectA, nil, nil, nil, "", 100); n != 1 {
			t.Fatalf("ListScheduleOccurrences(org A) returned %d row(s), want the 1 seeded", n)
		}
		if n := countRows(t, ctx, tx, storage.Query("ListScheduleOccurrences"),
			s.scheduleOf(t, s.projectB), projectB, nil, nil, nil, "", 100); n != 0 {
			t.Fatalf("ListScheduleOccurrences on the FOREIGN tenant's schedule returned %d row(s); the parent join did not confine it", n)
		}
	})
}

// countRows runs a shipped statement and counts what comes back.
func countRows(t *testing.T, ctx context.Context, tx pgx.Tx, query string, args ...any) int {
	t.Helper()
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		t.Fatalf("run shipped statement: %v", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate rows: %v", err)
	}
	return n
}

// scheduleOf reads a tenant's seeded LIVE schedule id as the owner.
func (s *suite) scheduleOf(t *testing.T, project string) string {
	t.Helper()
	var id string
	if err := s.owner.QueryRow(context.Background(),
		`SELECT id FROM schedules WHERE project_id = $1 AND deleted_at IS NULL`, project).Scan(&id); err != nil {
		t.Fatalf("read schedule of %s: %v", project, err)
	}
	return id
}

// scanNames runs a shipped list statement and returns the `name` column of each row (the second column of
// both ListSchedules' and ListHooks' projection).
func scanNames(t *testing.T, ctx context.Context, tx pgx.Tx, query string, args ...any) []string {
	t.Helper()
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		t.Fatalf("run shipped list statement: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			t.Fatalf("scan row: %v", err)
		}
		name, _ := values[1].(string)
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate rows: %v", err)
	}
	return out
}

// TestConnectionWithoutTenantContextSeesNoTenantRows proves the deny-by-default half: a runtime
// connection that never set palai.org_id — a forgotten scope, a background path that skipped the
// context — reads zero rows from every tenant-scoped table, rather than everything.
func TestConnectionWithoutTenantContextSeesNoTenantRows(t *testing.T) {
	s := newSuite(t)
	for _, table := range tenantTables(t, s.owner) {
		s.asProject(t, "", func(tx pgx.Tx) {
			var visible int
			if err := tx.QueryRow(context.Background(), fmt.Sprintf(`SELECT count(*) FROM %s`, table)).Scan(&visible); err != nil {
				t.Fatalf("%s: count: %v", table, err)
			}
			if visible != 0 {
				t.Fatalf("%s: connection with no tenant GUC saw %d row(s), want 0", table, visible)
			}
		})
	}
}

// TestForeignWriteIsRejected proves the WITH CHECK half: a scoped connection cannot plant a row into
// another organization, so a compromised or buggy handler cannot write across the boundary either.
func TestForeignWriteIsRejected(t *testing.T) {
	s := newSuite(t)
	s.asProject(t, s.projectA, func(tx pgx.Tx) {
		_, err := tx.Exec(context.Background(),
			`INSERT INTO sessions (id, project_id) VALUES ($1, $2)`,
			newID("ses"), newID("prj"))
		if err == nil {
			t.Fatal("insert into the foreign project succeeded; the WITH CHECK policy did not deny it")
		}
		if code := sqlState(err); code != "42501" {
			t.Fatalf("insert failed with SQLSTATE %q, want 42501 (insufficient_privilege from the RLS policy): %v", code, err)
		}
	})
}

// TestForeignPoolWriteAndUpdateAreRejected is E28 T1's two new write paths at the layer beneath them.
//
// The routes carry an organization/project predicate in their SQL and the handler takes the tenant from
// the verified bearer scope — and that is exactly the kind of claim this corpus exists to distrust. So the
// WITH CHECK half is asked about `POST /v1/runner-pools`'s statement (a pool planted in another
// organization) and the USING half about `PATCH /v1/runner-pools/{pool_id}`'s (a foreign pool's waiting
// room opened), both issued directly on the runtime role.
//
// THE UPDATE IS ASSERTED BY ROW COUNT RATHER THAN BY AN ERROR, and the difference is the whole reason it is
// written out: RLS does not REFUSE a foreign UPDATE, it makes the row invisible, so the statement succeeds
// and touches nothing. A test that only checked for an error would have passed on a build where the policy
// was dropped entirely — and the row would have been rewritten.
func TestForeignPoolWriteAndUpdateAreRejected(t *testing.T) {
	s := newSuite(t)
	ctx := context.Background()
	s.asProject(t, s.projectA, func(tx pgx.Tx) {
		var foreignProject string
		if err := s.owner.QueryRow(ctx, `SELECT id FROM projects WHERE id = $1`, s.projectB).Scan(&foreignProject); err != nil {
			t.Fatalf("read the foreign tenant's project: %v", err)
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO runner_pools (id, project_id, name, posture) VALUES ($1,$2,'planted','unsandboxed-host')`,
			newID("pool"), foreignProject)
		if err == nil {
			t.Fatal("a pool was planted in the foreign project: a create that lands in another tenant places their runs on hardware they did not choose")
		}
		if code := sqlState(err); code != "42501" {
			t.Fatalf("the foreign pool insert failed with SQLSTATE %q, want 42501: %v", code, err)
		}
	})
	// A SECOND TRANSACTION, because the refused INSERT above aborted the first one: a follow-up statement in
	// an aborted transaction comes back 25P02, which would have been an "error" this test could have read as
	// a refusal it never actually made.
	s.asProject(t, s.projectA, func(tx pgx.Tx) {
		tag, err := tx.Exec(ctx, `UPDATE runner_pools SET strict_enrollment = false WHERE project_id = $1`, s.projectB)
		if err != nil {
			t.Fatalf("the foreign pool update errored rather than matching nothing: %v", err)
		}
		if tag.RowsAffected() != 0 {
			t.Fatalf("a PATCH-shaped UPDATE touched %d row(s) of the foreign tenant; the waiting room of somebody else's Mac pool was closed", tag.RowsAffected())
		}
	})
	// The row the foreign tenant owns is unchanged, read as the OWNER so the check is not itself confined by
	// the policy it is checking.
	var strict bool
	if err := s.owner.QueryRow(ctx, `SELECT strict_enrollment FROM runner_pools WHERE project_id = $1`, s.projectB).Scan(&strict); err != nil {
		t.Fatalf("re-read the foreign tenant's pool: %v", err)
	}
	if !strict {
		t.Fatal("the foreign tenant's pool is no longer strict; the UPDATE matched nothing and the row changed anyway")
	}
}

// TestForeignDeleteIsRejected is the corpus's THIRD VERB, and it was missing until E29 T3. Every test above
// this one drives a SELECT, an INSERT or an UPDATE; nothing had ever issued a DELETE on the runtime role, so
// the policy's coverage of the destructive verb was an assumption rather than a measurement. A sweep looks in
// one direction, and this one had never looked here.
//
// It became worth measuring because T3 shipped `DELETE /v1/webhook-endpoints/{id}` — the first route in this
// family that destroys a row. The application's own SQL carries an organization_id + project_id predicate,
// and that predicate is exactly the kind of claim this corpus exists to distrust: the WHERE-LESS form below
// is the shape an application bug produces, and the DATABASE has to be what stops it.
//
// The consequence is the reason this is not merely another canary. A cross-tenant read discloses; a
// cross-tenant DELETE of a webhook endpoint stops another tenant's outbound notifications and tells nobody —
// they find out from the alerts that never arrived.
func TestForeignDeleteIsRejected(t *testing.T) {
	s := newSuite(t)
	ctx := context.Background()

	// A DELETE with no tenant predicate AT ALL, issued under org A's GUC. It must reach exactly org A's row.
	s.asProject(t, s.projectA, func(tx pgx.Tx) {
		tag, err := tx.Exec(ctx, `DELETE FROM webhook_endpoints`)
		if err != nil {
			t.Fatalf("the where-less delete errored rather than being confined: %v", err)
		}
		// NON-VACUITY, and it is the half that matters most for a delete: a policy that made every row
		// invisible would also report zero, and this test would pass while proving nothing. One row is the
		// caller's own, and it has to go.
		if tag.RowsAffected() != 1 {
			t.Fatalf("a where-less DELETE touched %d row(s), want exactly the caller's own 1. Zero would mean this "+
				"test proves nothing (no row was ever visible); more than one would mean it reached another tenant.",
				tag.RowsAffected())
		}
	})

	// A DELETE that NAMES the foreign tenant — the deliberate attack rather than the accidental omission —
	// matches nothing. A second transaction, because the first was rolled back.
	s.asProject(t, s.projectA, func(tx pgx.Tx) {
		tag, err := tx.Exec(ctx, `DELETE FROM webhook_endpoints WHERE project_id = $1`, s.projectB)
		if err != nil {
			t.Fatalf("the foreign delete errored rather than matching nothing: %v", err)
		}
		if tag.RowsAffected() != 0 {
			t.Fatalf("a DELETE naming the foreign organization removed %d of their webhook endpoint(s); their "+
				"outbound notifications stop and nothing tells them", tag.RowsAffected())
		}
	})

	// Read as the OWNER, so the check is not itself confined by the policy it is checking. Both rows are
	// still there — the transactions above were rolled back, so this also confirms the corpus left the
	// fixture intact for whatever runs next.
	var surviving int
	if err := s.owner.QueryRow(ctx,
		`SELECT count(*) FROM webhook_endpoints WHERE project_id = ANY($1)`,
		[]string{s.projectA, s.projectB}).Scan(&surviving); err != nil {
		t.Fatalf("re-read both tenants' endpoints: %v", err)
	}
	if surviving != 2 {
		t.Fatalf("%d of the 2 seeded webhook endpoints survive", surviving)
	}
}

// TestRuntimeRoleIsNotTheTableOwner keeps the corpus honest: RLS is silently inert for a superuser or
// a table owner, so the guarantee above only means something while the runtime role is neither.
func TestRuntimeRoleIsNotTheTableOwner(t *testing.T) {
	s := newSuite(t)
	ctx := context.Background()
	var superuser, bypassRLS bool
	if err := s.owner.QueryRow(ctx,
		`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = $1`, runtimeRole).Scan(&superuser, &bypassRLS); err != nil {
		t.Fatalf("read runtime role attributes: %v", err)
	}
	if superuser || bypassRLS {
		t.Fatalf("runtime role %s is superuser=%v bypassrls=%v; RLS would be inert", runtimeRole, superuser, bypassRLS)
	}
	var owned int
	if err := s.owner.QueryRow(ctx,
		`SELECT count(*) FROM pg_tables WHERE schemaname = 'public' AND tableowner = $1`, runtimeRole).Scan(&owned); err != nil {
		t.Fatalf("count owned tables: %v", err)
	}
	if owned != 0 {
		t.Fatalf("runtime role %s owns %d table(s); FORCE would be the only thing left holding the boundary", runtimeRole, owned)
	}
}

// TestEveryTenantTableIsRowLevelSecured is the regression gate for every LATER migration: a new table
// carrying organization_id must arrive with RLS enabled AND forced, or this fails. A table that is
// genuinely not tenant-scoped goes on the nonTenantTables allowlist above, deliberately and visibly.
func TestEveryTenantTableIsRowLevelSecured(t *testing.T) {
	s := newSuite(t)
	ctx := context.Background()
	rows, err := s.owner.Query(ctx, `
		SELECT c.relname, c.relrowsecurity, c.relforcerowsecurity,
		       EXISTS (SELECT 1 FROM information_schema.columns col
		               WHERE col.table_schema = 'public' AND col.table_name = c.relname
		                 AND col.column_name = 'organization_id')
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'public' AND c.relkind = 'r'
		 ORDER BY c.relname`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var enabled, forced, tenantScoped bool
		if err := rows.Scan(&name, &enabled, &forced, &tenantScoped); err != nil {
			t.Fatalf("scan table row: %v", err)
		}
		if nonTenantTables[name] {
			// The allowlist is for genuinely non-tenant tables only; a table carrying organization_id must
			// never be exempted here (that would silently un-secure a real tenant table).
			if tenantScoped {
				t.Errorf("allowlisted table %q is tenant-scoped; remove from nonTenantTables", name)
			}
			continue
		}
		if !enabled || !forced {
			t.Errorf("table %s: row security enabled=%v forced=%v, want both true (tenant column present=%v)",
				name, enabled, forced, tenantScoped)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate tables: %v", err)
	}
}

// tenantTables lists the public tables under RLS, so the deny-by-default test covers whatever the
// migration chain currently defines rather than a hand-copied list that rots.
func tenantTables(t *testing.T, owner *pgxpool.Pool) []string {
	t.Helper()
	rows, err := owner.Query(context.Background(), `
		SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'public' AND c.relkind = 'r' AND c.relrowsecurity
		 ORDER BY c.relname`)
	if err != nil {
		t.Fatalf("list tenant tables: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan tenant table: %v", err)
		}
		out = append(out, name)
	}
	if len(out) == 0 {
		t.Fatal("no table has row level security enabled; migration 000029 did not take effect")
	}
	return out
}

func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// newID mints a collision-free fixture identifier, matching the prefixed-opaque id shape the
// control plane uses.
func newID(prefix string) string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(err)
	}
	return prefix + "_" + hex.EncodeToString(raw[:])
}

// bindingOf reads a tenant's seeded binding id as the OWNER, live or archived.
func (s *suite) bindingOf(t *testing.T, project string, archived bool) string {
	t.Helper()
	predicate := "archived_at IS NULL"
	if archived {
		predicate = "archived_at IS NOT NULL"
	}
	var id string
	// ORDER BY so the read is deterministic: exactly one row matches today, and a LIMIT over an unordered
	// result is how this tree has twice decided a security outcome by accident.
	if err := s.owner.QueryRow(context.Background(),
		`SELECT id FROM repository_bindings WHERE project_id = $1 AND `+predicate+` ORDER BY id LIMIT 1`,
		project).Scan(&id); err != nil {
		t.Fatalf("read %s binding of %s: %v", predicate, project, err)
	}
	return id
}

// TestTheRepositoryBindingLifecycleStatementsAreConfinedAndRefuseArchivedRows runs the SHIPPED statements
// E30 added — the three writes and the two reads that gained a lifecycle clause — and asks the two
// questions only the real statements can answer. It is modelled on the automation-list test above and
// separates the same two layers, because they fail independently and only one of them is RLS.
//
//   - A FOREIGN row moving is a TENANCY failure. Each write carries its own organization/project
//     predicate AND runs under the policy, so this binds the FOREIGN tenant's real ids into the shipped
//     statement while the connection declares org A. The database must change nothing even though the
//     parameters ask for everything, and the owner-side re-read afterwards is what proves the row is
//     untouched rather than merely unreported.
//   - An ARCHIVED row being admitted is an APPLICATION failure. `archived_at IS NULL` is a clause in the
//     query and nothing beneath it enforces that — no policy, no constraint, no role. If it is deleted
//     from RepositoryBindingExists, every layer below is still perfectly correct and a retired binding
//     silently starts serving runs again. That clause is the ONLY thing that makes archiving more than a
//     display flag, so it gets an assertion of its own rather than riding the list's.
func TestTheRepositoryBindingLifecycleStatementsAreConfinedAndRefuseArchivedRows(t *testing.T) {
	s := newSuite(t)
	ctx := context.Background()
	projectA, projectB := s.projectA, s.projectB
	liveA, archivedA := s.bindingOf(t, s.projectA, false), s.bindingOf(t, s.projectA, true)
	liveB := s.bindingOf(t, s.projectB, false)

	s.asProject(t, s.projectA, func(tx pgx.Tx) {
		// 1. THE LIST HIDES THE ARCHIVED ROW. $8=false is the default every caller takes.
		if n := countRows(t, ctx, tx, storage.Query("ListRepositoryBindings"),
			projectA, nil, nil, nil, "", 100, false); n != 1 {
			t.Fatalf("ListRepositoryBindings(org A, include_archived=false) returned %d row(s), want exactly the 1 live one.\n"+
				"A second row here is the ARCHIVED binding: `archived_at IS NULL` is an APPLICATION predicate and no "+
				"policy, constraint or role re-states it — if the clause goes, this is what notices.", n)
		}
		// 2. AND SHOWS IT WHEN ASKED. Without this the arm above would also pass if the statement returned
		// nothing at all, which is the shape of green this corpus keeps having to re-measure.
		if n := countRows(t, ctx, tx, storage.Query("ListRepositoryBindings"),
			projectA, nil, nil, nil, "", 100, true); n != 2 {
			t.Fatalf("ListRepositoryBindings(org A, include_archived=true) returned %d row(s), want both seeded", n)
		}
		// 3. THE SAME STATEMENT, FOREIGN PARAMETERS, connection still declaring org A.
		if n := countRows(t, ctx, tx, storage.Query("ListRepositoryBindings"),
			projectB, nil, nil, nil, "", 100, true); n != 0 {
			t.Fatalf("ListRepositoryBindings asked for the FOREIGN tenant returned %d row(s); RLS did not confine the shipped statement", n)
		}

		// 4. THE ADMISSION GUARD. This is the clause that makes archiving mean anything: a run's
		// `repository` attachment is verified through RepositoryBindingExists, so a retired binding must
		// look exactly as absent there as a foreign one.
		if n := countRows(t, ctx, tx, storage.Query("RepositoryBindingExists"), liveA, projectA); n != 1 {
			t.Fatalf("RepositoryBindingExists(live binding) returned %d row(s), want 1 — admission would refuse a healthy binding", n)
		}
		if n := countRows(t, ctx, tx, storage.Query("RepositoryBindingExists"), archivedA, projectA); n != 0 {
			t.Fatalf("RepositoryBindingExists(ARCHIVED binding) returned %d row(s), want 0.\n"+
				"An archived binding still admits runs, which makes archived_at a display flag: the retired row "+
				"keeps cloning and the operator who retired it is not told.", n)
		}
		if n := countRows(t, ctx, tx, storage.Query("RepositoryBindingExists"), liveB, projectB); n != 0 {
			t.Fatalf("RepositoryBindingExists asked for the FOREIGN tenant's live binding returned %d row(s), want 0", n)
		}

		// 5. THE CREDENTIAL WRITE, AIMED AT THE FOREIGN TENANT'S REAL BINDING ID. Every parameter names
		// org B; the policy says org A. Nothing may be returned, and nothing may move.
		if n := countRows(t, ctx, tx, storage.Query("SetRepositoryBindingConnection"),
			liveB, projectB, "hijacked-by-org-a"); n != 0 {
			t.Fatalf("SetRepositoryBindingConnection updated %d foreign row(s); one tenant re-credited another's binding", n)
		}
		// 6. AND IT REFUSES AN ARCHIVED ROW OF THE CALLER'S OWN TENANT — the application clause again,
		// asserted separately from the tenancy one so a failure names which layer broke.
		if n := countRows(t, ctx, tx, storage.Query("SetRepositoryBindingConnection"),
			archivedA, projectA, "re-credit-a-retired-binding"); n != 0 {
			t.Fatalf("SetRepositoryBindingConnection updated an ARCHIVED binding (%d row(s)); nothing can run against it, so the write is a no-op the caller would read as success", n)
		}
		// 7. AND IT WORKS AT ALL. Without this, arms 5 and 6 would both pass against a statement that
		// updates nothing ever — the perturbation that is invisible to a pure-absence proof.
		if n := countRows(t, ctx, tx, storage.Query("SetRepositoryBindingConnection"),
			liveA, projectA, "rotated-by-org-a"); n != 1 {
			t.Fatalf("SetRepositoryBindingConnection updated %d row(s) of the caller's OWN live binding, want 1", n)
		}

		// 8. ARCHIVE, AIMED AT THE FOREIGN TENANT. A tenant that could retire another's binding could stop
		// their runs, which is a denial of service with no audit trail on the victim's side.
		if n := countRows(t, ctx, tx, storage.Query("ArchiveRepositoryBinding"), liveB, projectB); n != 0 {
			t.Fatalf("ArchiveRepositoryBinding retired %d foreign row(s); one tenant disabled another's repository", n)
		}
		if n := countRows(t, ctx, tx, storage.Query("UnarchiveRepositoryBinding"), archivedA, projectA); n != 1 {
			t.Fatalf("UnarchiveRepositoryBinding restored %d row(s) of the caller's own archived binding, want 1", n)
		}
	})

	// THE OWNER RE-READS WHAT ORG B ACTUALLY HOLDS. Everything above ran under org A's policy, where a
	// denied write and a silently-successful one both surface as "no rows returned"; only a read outside
	// that policy can tell them apart. This is the arm that would catch a policy which reported nothing
	// while letting the UPDATE through.
	var refB string
	var archivedB *time.Time
	if err := s.owner.QueryRow(ctx,
		`SELECT connection_ref, archived_at FROM repository_bindings WHERE id = $1`, liveB).Scan(&refB, &archivedB); err != nil {
		t.Fatalf("owner re-read of org B's binding: %v", err)
	}
	if refB != "seeded-ref-"+s.projectB {
		t.Fatalf("org B's connection_ref is %q, want the seeded %q — org A's write reached another tenant's credential pointer",
			refB, "seeded-ref-"+s.projectB)
	}
	if archivedB != nil {
		t.Fatalf("org B's binding is archived (%v); org A retired another tenant's repository", archivedB)
	}
}
