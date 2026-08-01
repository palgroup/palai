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
	orgA  string
	orgB  string
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

	s := &suite{owner: owner, app: app, orgA: newID("org"), orgB: newID("org")}
	s.seedTenant(t, s.orgA)
	s.seedTenant(t, s.orgB)
	return s
}

// seedTenant writes one organization -> project -> session -> run -> artifact, as the owner. The run and
// artifact rows are the corpus's canaries: they are rows a WHERE-less SELECT must not return across the
// tenant boundary. The artifact makes DAT-006's cross-tenant denial first-class at the DB layer (E13 T5) —
// the retrieval API's 404 is only honest because the row is invisible one layer down.
func (s *suite) seedTenant(t *testing.T, org string) {
	t.Helper()
	ctx := context.Background()
	project, session, response, run, artifact := newID("prj"), newID("ses"), newID("resp"), newID("run"), newID("art")
	environment := newID("env")
	tool, toolRevision, setRevision := newID("tool"), newID("trev"), newID("tsrev")
	toolCall, backgroundTask := newID("call"), newID("bgt")
	runnerPool := newID("pool")
	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO organizations (id) VALUES ($1)`, []any{org}},
		{`INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, []any{project, org}},
		{`INSERT INTO sessions (id, organization_id, project_id) VALUES ($1, $2, $3)`, []any{session, org, project}},
		{`INSERT INTO responses (id, organization_id, project_id, session_id, input) VALUES ($1, $2, $3, $4, '{}'::jsonb)`,
			[]any{response, org, project, session}},
		{`INSERT INTO runs (id, organization_id, project_id, session_id, response_id) VALUES ($1, $2, $3, $4, $5)`,
			[]any{run, org, project, session, response}},
		{`INSERT INTO artifacts (id, organization_id, project_id, run_id, object_key, size_bytes, checksum) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			[]any{artifact, org, project, run, org + "/" + project + "/" + run + "/" + artifact, 12, "sha256:deadbeef"}},
		// The E25 T3 environment pair (000046). Both are ORG-scoped rather than project-scoped, matching
		// secret_refs (000031:16), so both are seeded with the org and no project — a mismatch would make the
		// WHERE-less canary below pass for the wrong reason.
		//
		// THEY ARE SEEDED BECAUSE THE CATALOGUE-DRIVEN TESTS ALONE ARE NOT ENOUGH. TestEveryTenantTableIsRow-
		// LevelSecured and TestConnectionWithoutTenantContextSeesNoTenantRows both walk pg_class, so they cover
		// a new table the moment it exists — but they can only prove "no rows come back", and a table with no
		// rows in it satisfies that vacuously. The canary below needs a row in EACH org to have anything to
		// fail on.
		{`INSERT INTO environments (id, organization_id, name) VALUES ($1, $2, $3)`,
			[]any{environment, org, "production"}},
		{`INSERT INTO environment_values (environment_id, organization_id, key) VALUES ($1, $2, $3)`,
			[]any{environment, org, "JIRA_TOKEN"}},
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
		{`INSERT INTO tools (id, organization_id, project_id, canonical_name, model_visible_name) VALUES ($1,$2,$3,$4,$5)`,
			[]any{tool, org, project, "mcp.jira.getJiraIssue", "jira__getJiraIssue"}},
		{`INSERT INTO tool_revisions (id, organization_id, project_id, tool_id, revision_number, executor, description, input_schema, digest)
		  VALUES ($1,$2,$3,$4,1,'mcp',$5,'{"type":"object"}'::jsonb,'sha256:tenancyseed')`,
			[]any{toolRevision, org, project, tool, "Get the details of a Jira issue by its key."}},
		{`INSERT INTO tool_set_revisions (id, organization_id, project_id, set_name, revision_number, tool_pins, digest)
		  VALUES ($1,$2,$3,'jira',1,jsonb_build_array(jsonb_build_object('tool_revision_id',$4::text)),'sha256:tenancyseed')`,
			[]any{setRevision, org, project, toolRevision}},
		// The E26 T1 background task (000047), PROJECT-scoped like the tool_calls row it hangs off. It needs
		// that row first: `UNIQUE (tool_call_id)` makes the FK the identity of the pair, so the seed writes
		// both rather than pointing at a call that never existed.
		//
		// WHAT MAKES THIS CANARY WORTH A ROW rather than only the catalogue sweep: `handle` names a LIVE
		// OPERATING-SYSTEM OBJECT — a container id, or a `pgid:starttime` on the machine the control plane
		// runs on. A cross-tenant read of this column would hand one tenant a handle the reaper signals, so
		// the leak this seed exists to fail on is not a disclosure, it is a kill.
		{`INSERT INTO tool_calls (id, organization_id, project_id, run_id, name) VALUES ($1,$2,$3,$4,'palai.workspace.shell')`,
			[]any{toolCall, org, project, run}},
		{`INSERT INTO background_tasks (id, organization_id, project_id, run_id, session_id, response_id, tool_call_id,
		    posture, handle, state, output_path)
		  VALUES ($1,$2,$3,$4,$5,$6,$7,'unsandboxed-host','4242:2026-07-30T12:00:00Z','running','.palai-session/bg/'||$1||'.log')`,
			[]any{backgroundTask, org, project, run, session, response, toolCall}},
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
		{`INSERT INTO runner_pools (id, organization_id, project_id, name, posture, os, arch, strict_enrollment)
		  VALUES ($1,$2,$3,'mac-pool','unsandboxed-host','darwin','arm64',true)`,
			[]any{runnerPool, org, project}},
	}
	for _, stmt := range stmts {
		if _, err := s.owner.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed %s: %v", org, err)
		}
	}
}

// asOrg runs fn inside one transaction whose palai.org_id GUC names org. It mirrors the per-request scope
// the auth middleware resolves, though the mechanism differs on purpose: this test sets the GUC
// transaction-locally (set_config is_local=true) for isolation between subtests, whereas production sets it
// session-level once per pool acquisition (storage.OpenPool, is_local=false). Both reach the same policy.
// An empty org leaves the GUC unset — the "connection that never declared a tenant" case.
func (s *suite) asOrg(t *testing.T, org string, fn func(tx pgx.Tx)) {
	t.Helper()
	ctx := context.Background()
	tx, err := s.app.Begin(ctx)
	if err != nil {
		t.Fatalf("begin app tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if org != "" {
		if _, err := tx.Exec(ctx, "SELECT set_config('palai.org_id', $1, true)", org); err != nil {
			t.Fatalf("set tenant GUC: %v", err)
		}
	}
	fn(tx)
}

// TestWhereLessQueryIsRejectedByTheDatabase is the essence of TEN-002: the fixture query is
// deliberately unscoped — no organization_id predicate at all, the shape an application bug would
// produce — and the database still returns only the caller's tenant.
func TestWhereLessQueryIsRejectedByTheDatabase(t *testing.T) {
	s := newSuite(t)
	for _, table := range []string{"runs", "responses", "sessions", "projects", "artifacts", "environments", "environment_values",
		"tools", "tool_revisions", "tool_set_revisions", "background_tasks", "runner_pools"} {
		s.asOrg(t, s.orgA, func(tx pgx.Tx) {
			var foreign int
			// The only predicate names the OTHER tenant: the query asks for exactly the rows the
			// caller must never see, so a non-zero count is a leak and nothing else.
			query := fmt.Sprintf(`SELECT count(*) FROM %s WHERE organization_id = $1`, table)
			if err := tx.QueryRow(context.Background(), query, s.orgB).Scan(&foreign); err != nil {
				t.Fatalf("%s: count foreign rows: %v", table, err)
			}
			if foreign != 0 {
				t.Fatalf("%s: WHERE-less query saw %d row(s) of the foreign tenant; RLS did not deny", table, foreign)
			}
			var visible int
			if err := tx.QueryRow(context.Background(), fmt.Sprintf(`SELECT count(*) FROM %s`, table)).Scan(&visible); err != nil {
				t.Fatalf("%s: count visible rows: %v", table, err)
			}
			if visible != 1 {
				t.Fatalf("%s: own tenant sees %d row(s), want exactly the 1 seeded", table, visible)
			}
		})
	}
}

// TestConnectionWithoutTenantContextSeesNoTenantRows proves the deny-by-default half: a runtime
// connection that never set palai.org_id — a forgotten scope, a background path that skipped the
// context — reads zero rows from every tenant-scoped table, rather than everything.
func TestConnectionWithoutTenantContextSeesNoTenantRows(t *testing.T) {
	s := newSuite(t)
	for _, table := range tenantTables(t, s.owner) {
		s.asOrg(t, "", func(tx pgx.Tx) {
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
	s.asOrg(t, s.orgA, func(tx pgx.Tx) {
		_, err := tx.Exec(context.Background(),
			`INSERT INTO sessions (id, organization_id, project_id) VALUES ($1, $2, $3)`,
			newID("ses"), s.orgB, newID("prj"))
		if err == nil {
			t.Fatal("insert into the foreign organization succeeded; the WITH CHECK policy did not deny it")
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
	s.asOrg(t, s.orgA, func(tx pgx.Tx) {
		var foreignProject string
		if err := s.owner.QueryRow(ctx, `SELECT id FROM projects WHERE organization_id = $1`, s.orgB).Scan(&foreignProject); err != nil {
			t.Fatalf("read the foreign tenant's project: %v", err)
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO runner_pools (id, organization_id, project_id, name, posture) VALUES ($1,$2,$3,'planted','unsandboxed-host')`,
			newID("pool"), s.orgB, foreignProject)
		if err == nil {
			t.Fatal("a pool was planted in the foreign organization: a create that lands in another tenant places their runs on hardware they did not choose")
		}
		if code := sqlState(err); code != "42501" {
			t.Fatalf("the foreign pool insert failed with SQLSTATE %q, want 42501: %v", code, err)
		}
	})
	// A SECOND TRANSACTION, because the refused INSERT above aborted the first one: a follow-up statement in
	// an aborted transaction comes back 25P02, which would have been an "error" this test could have read as
	// a refusal it never actually made.
	s.asOrg(t, s.orgA, func(tx pgx.Tx) {
		tag, err := tx.Exec(ctx, `UPDATE runner_pools SET strict_enrollment = false WHERE organization_id = $1`, s.orgB)
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
	if err := s.owner.QueryRow(ctx, `SELECT strict_enrollment FROM runner_pools WHERE organization_id = $1`, s.orgB).Scan(&strict); err != nil {
		t.Fatalf("re-read the foreign tenant's pool: %v", err)
	}
	if !strict {
		t.Fatal("the foreign tenant's pool is no longer strict; the UPDATE matched nothing and the row changed anyway")
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
