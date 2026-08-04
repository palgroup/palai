//go:build live

// This file is CASE=tenancy-isolation, the E13 Task 1 live smoke: a two-organization stack on ONE real
// Postgres proves cross-tenant isolation at BOTH layers the plan names. A REAL provider-one completion
// produces org-A's run (so the run being protected is genuinely real, not a fixture), and then:
//
//  1. API SURFACE: org-B's real API key resolves to org-B's scope and retrieving org-A's run through the
//     production credential→retrieval path returns NOT FOUND — the same 404 the router emits, leaking no
//     cross-tenant existence. org-A's own key retrieves it.
//  2. DATABASE: an org-B-scoped connection issues a deliberately WHERE-clause-free count over responses
//     and sees ZERO of org-A's rows — migration 000029's row-level-security policy denies it at Postgres,
//     not in application code. org-A's scope sees exactly its one row.
//
// This is the live half of TEN-001/TEN-002: the deterministic corpus (tests/security/tenancy) proves the
// DB deny against seeded rows; this proves it against a row a REAL model produced, end to end.
//
// HONEST CEILINGS:
//   - SINGLE DATABASE, SINGLE RUNTIME ROLE (spec E13 ceiling): the boundary is RLS on the non-owner
//     palai_app role. This makes a missing WHERE clause unexploitable; it is NOT a defence against a
//     compromised control-plane process (which can RESET ROLE) or a hostile DBA, and nothing is encrypted
//     at rest — those are E13-H/E15.
//   - SINGLE PROVIDER: the real completion is provider-one only; the isolation proof is provider-agnostic.
//   - The 404 is proven through the store's real VerifyAPIKey→GetResponse path (exactly what the auth
//     middleware and retrieval handler call); the HTTP transport around it adds nothing to the isolation.
//
// GATED: serialized with every LIVE smoke on the shared :local Docker stack; NOT part of make verify / CI.
// Skips cleanly without creds. The credential is an opaque env-resolved secret, never printed.
package live

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
	"github.com/palgroup/palai/packages/coordinator"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"

	"github.com/palgroup/palai/storage"
)

// TestLiveTenancyIsolationCrossOrgDeny is CASE=tenancy-isolation (see the file ceilings).
func TestLiveTenancyIsolationCrossOrgDeny(t *testing.T) {
	secret := requireEnv(t, credentialEnv)
	engineDir := requireEnv(t, "PALAI_ENGINE_DIR")
	pgURL := requireEnv(t, "PALAI_COMPONENT_POSTGRES_URL")
	_ = secret // resolved through the env secret resolver; never referenced directly

	ctx := context.Background()
	repo, err := store.Open(ctx, pgURL)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(repo.Close)
	if err := repo.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	pool := repo.Spine().Pool()

	// Org-A owns the run; org-B is a second tenant on the SAME stack with its own key. Each key's plaintext
	// exists only here; only its hash reaches the database (coordinator.HashAPIKey).
	tokenA, tenantA, sessionA, respA, runA := seedTenantWithRun(t, pool, "org-A: reply with the single word done.")
	tokenB, _ := seedTenantWithKey(t, pool, "org-B")

	// Drive org-A's run to a terminal completion on the REAL provider — the run being isolated is genuine.
	broker := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"provider-one": providerone.Adapter{}},
		Secrets:  modelbroker.EnvResolver{"provider-one": credentialEnv},
	})
	orch := execution.NewOrchestrator(repo, &subprocessDialer{engineDir: engineDir}, broker, toolbroker.New(tools.FileTool()))
	orch.SetModelRoute(execution.ModelRoute{Provider: "provider-one", Model: liveModel(), Secret: modelbroker.SecretRef("provider-one")})
	if err := orch.ExecuteAttempt(ctx, descriptor(runA, 1)); err != nil {
		t.Fatalf("execute org-A run on the real provider: %v", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM model_requests WHERE run_id=$1 AND state='completed'`, runA); n < 1 {
		t.Fatalf("completed model_requests for org-A = %d, want >=1 (the real provider must have answered)", n)
	}
	_ = sessionA

	// Both keys resolve to their own verified scope through the production credential path.
	scopeA, err := repo.VerifyAPIKey(ctx, tokenA)
	if err != nil {
		t.Fatalf("VerifyAPIKey(org-A) error = %v", err)
	}
	scopeB, err := repo.VerifyAPIKey(ctx, tokenB)
	if err != nil {
		t.Fatalf("VerifyAPIKey(org-B) error = %v", err)
	}
	if scopeA.Project == scopeB.Project {
		t.Fatalf("the two keys resolved to the SAME project %q — the fixture is not two tenants", scopeA.Project)
	}

	// LAYER 1 — API surface: org-A retrieves its own run; org-B's key gets a clean 404 (Found=false), the
	// same not-found the retrieval handler renders, with no cross-tenant existence disclosure.
	own, err := repo.GetResponse(ctx, scopeA, respA)
	if err != nil {
		t.Fatalf("GetResponse(org-A, own run) error = %v", err)
	}
	if !own.Found {
		t.Fatal("org-A could not retrieve its own completed run")
	}
	foreign, err := repo.GetResponse(ctx, scopeB, respA)
	if err != nil {
		t.Fatalf("GetResponse(org-B, org-A run) error = %v", err)
	}
	if foreign.Found {
		t.Fatal("org-B's key retrieved org-A's run — cross-tenant isolation FAILED at the API surface (404 expected)")
	}

	// LAYER 2 — database: a WHERE-less-on-tenant count under org-B's scope must not include org-A's row.
	// This is the RLS deny at Postgres: the query asks for exactly the first tenant's response id and the
	// policy hides it, so even a query that forgot to scope sees nothing across the boundary. Migration
	// 000062 keys this table's RLS on project_id alone, and A.2 removed the organization parameter that used
	// to sit beside it — project_id is the whole of the isolation now.
	//
	// AND DROPPING THAT PARAMETER FIXED A CALL SITE THAT WAS ALREADY WRONG. Under the old
	// (t, pool, org, project, query, args...) signature the second call below read
	// `countUnderTenant(t, pool, tenantA.Project, "SELECT ...", respA)` — five arguments where six were
	// meant, so `tenantA.Project` landed in `org`, the SQL landed in `project`, and `respA` became the
	// QUERY. It compiled because all three are strings; it would have failed at runtime trying to execute a
	// run id as SQL, and nothing caught it because this leg only runs against a real provider. With the org
	// parameter gone the same call site is correct, and it is written out here rather than left to luck.
	if n := countUnderTenant(t, pool, scopeB.Project,
		`SELECT count(*) FROM responses WHERE id=$1`, respA); n != 0 {
		t.Fatalf("the second tenant's scoped DB read saw %d of the first tenant's response rows, want 0 (migration 000062 RLS deny)", n)
	}
	if n := countUnderTenant(t, pool, tenantA.Project,
		`SELECT count(*) FROM responses WHERE id=$1`, respA); n != 1 {
		t.Fatalf("the first tenant's scoped DB read saw %d of its own response rows, want exactly 1", n)
	}

	t.Logf("tenancy-isolation PASS: a REAL provider run in org-A (%s) is invisible to org-B's key at the API surface (404) AND denied at the database by RLS (000029); org-A sees its own row at both layers.", runA)
}

// seedTenantWithRun creates an organization → project → session → response(queued) → run(queued) plus a
// principal and API key, and returns the key plaintext, tenant, session, response, and run ids. The run
// starts queued so the orchestrator drives it; prompt is the input the model answers.
func seedTenantWithRun(t *testing.T, pool *pgxpool.Pool, prompt string) (token string, tenant coordinator.Tenant, session, response, runID string) {
	t.Helper()
	tenant = coordinator.Tenant{Project: newID("prj")}
	session, response, runID = newID("ses"), newID("resp"), newID("run")
	principal, keyID := newID("prin"), newID("key")
	token = newID("sk")
	do := func(sql string, args ...any) {
		if _, err := pool.Exec(storage.WithSystemScope(context.Background()), sql, args...); err != nil {
			t.Fatalf("seed exec %q: %v", sql, err)
		}
	}
	do(`INSERT INTO projects (id) VALUES ($1)`, tenant.Project)
	do(`INSERT INTO principals (id, project_id, kind) VALUES ($1, $2, 'service')`,
		principal, tenant.Project)
	do(`INSERT INTO api_keys (id, project_id, principal_id, key_hash) VALUES ($1, $2, $3, $4)`,
		keyID, tenant.Project, principal, coordinator.HashAPIKey(token))
	do(`INSERT INTO sessions (id, project_id) VALUES ($1, $2)`, session, tenant.Project)
	do(`INSERT INTO responses (id, project_id, session_id, state, input) VALUES ($1,$2,$3,'queued',$4)`,
		response, tenant.Project, session, encodeJSONString(prompt))
	do(`INSERT INTO runs (id, project_id, session_id, response_id, state) VALUES ($1,$2,$3,$4,'queued')`,
		runID, tenant.Project, session, response)
	return token, tenant, session, response, runID
}

// seedTenantWithKey creates a bare second tenant (organization → project → principal → api_key) with no
// run, returning the key plaintext and its tenant — the "other org" whose key must be denied.
func seedTenantWithKey(t *testing.T, pool *pgxpool.Pool, label string) (token string, tenant coordinator.Tenant) {
	t.Helper()
	_ = label
	tenant = coordinator.Tenant{Project: newID("prj")}
	principal, keyID := newID("prin"), newID("key")
	token = newID("sk")
	do := func(sql string, args ...any) {
		if _, err := pool.Exec(storage.WithSystemScope(context.Background()), sql, args...); err != nil {
			t.Fatalf("seed exec %q: %v", sql, err)
		}
	}
	do(`INSERT INTO projects (id) VALUES ($1)`, tenant.Project)
	do(`INSERT INTO principals (id, project_id, kind) VALUES ($1, $2, 'service')`,
		principal, tenant.Project)
	do(`INSERT INTO api_keys (id, project_id, principal_id, key_hash) VALUES ($1, $2, $3, $4)`,
		keyID, tenant.Project, principal, coordinator.HashAPIKey(token))
	return token, tenant
}

// seedSystemKey creates a bearer key that carries ONLY middleware.ScopeSystem — the platform capability
// Task 2 gates organization creation on. It is deliberately NOT a variant of seedTenantWithKey: a system
// key is not a tenant admin key with extra privileges, it answers a different question (see
// provisioning.go's authorizeSystem), and every OTHER seedTenantWithKey caller in this package must keep
// getting an ordinary tenant key with no platform capability. It still needs a throwaway tenant to satisfy
// api_keys' foreign keys, but that tenant is never the caller's.
func seedSystemKey(t *testing.T, pool *pgxpool.Pool) (token string) {
	t.Helper()
	tenant := coordinator.Tenant{Project: newID("prj")}
	principal, keyID := newID("prin"), newID("key")
	token = newID("sk")
	do := func(sql string, args ...any) {
		if _, err := pool.Exec(storage.WithSystemScope(context.Background()), sql, args...); err != nil {
			t.Fatalf("seed exec %q: %v", sql, err)
		}
	}
	do(`INSERT INTO projects (id) VALUES ($1)`, tenant.Project)
	do(`INSERT INTO principals (id, project_id, kind) VALUES ($1, $2, 'service')`,
		principal, tenant.Project)
	do(`INSERT INTO api_keys (id, project_id, principal_id, key_hash, scopes) VALUES ($1, $2, $3, $4, $5)`,
		keyID, tenant.Project, principal, coordinator.HashAPIKey(token), []string{middleware.ScopeSystem})
	return token
}

// countUnderTenant runs query inside ONE transaction scoped to the given tenant via the same
// palai.org_id / palai.project_id GUCs the auth middleware publishes, so the count is subject to
// migration 000029's row-level-security policies exactly as a request would be.
func countUnderTenant(t *testing.T, pool *pgxpool.Pool, project, query string, args ...any) int {
	t.Helper()
	ctx := storage.WithTenant(context.Background(), project)
	var n int
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, query, args...).Scan(&n)
	})
	if err != nil {
		t.Fatalf("scoped count %q: %v", query, err)
	}
	return n
}

// encodeJSONString renders s as a JSON string literal for the response.input jsonb column.
func encodeJSONString(s string) []byte {
	out := make([]byte, 0, len(s)+2)
	out = append(out, '"')
	for _, r := range []byte(s) {
		if r == '"' || r == '\\' {
			out = append(out, '\\')
		}
		out = append(out, r)
	}
	return append(out, '"')
}
