//go:build live

// This file is CASE=run-history-list, the E13 Task 4 live smoke: over the REAL router + real store, a
// tenant lists its run history (a run produced by a REAL provider-one completion) with a plain HTTP client
// (no SDK), and a SECOND tenant presenting the first tenant's pagination cursor is an EXPLICIT 400
// invalid_cursor — never a silently-empty page. It is the live half of MCI-003 + the TEN-001 cursor-fuzz
// contract: the deterministic e2e proves the list keyset + cross-tenant reject against seeded/fake-provider
// rows; this proves the same surface against a row a REAL model produced, end to end through the router.
//
// HONEST CEILINGS:
//   - SINGLE PROVIDER: the completed run is provider-one only; the list surface is provider-agnostic.
//   - The cursor is a process-random-keyed HMAC (single-replica compose ceiling, matches T7's in-process
//     limiter): the cross-tenant reject holds within the process serving both keys, which is exactly the
//     self-host stack this phase targets.
//
// GATED: serialized with every LIVE smoke on the shared stack; NOT part of make verify / CI. Skips cleanly
// without creds. The credential is an opaque env-resolved secret, never printed.
package live

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"

	"github.com/palgroup/palai/storage"
)

func TestLiveRunHistoryListCrossTenantCursor(t *testing.T) {
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

	// Org-A owns the real run; a second bare response gives A a 2-row history so a limit=1 page mints a
	// cursor. Org-B is a second tenant on the SAME stack with its own key.
	tokenA, tenantA, sessionA, respA, runA := seedTenantWithRun(t, pool, "org-A: reply with the single word done.")
	respA2 := newID("resp")
	if _, err := pool.Exec(storage.WithSystemScope(ctx),
		`INSERT INTO responses (id, organization_id, project_id, session_id, state, input) VALUES ($1,$2,$3,$4,'queued','{}'::jsonb)`,
		respA2, tenantA.Organization, tenantA.Project, sessionA); err != nil {
		t.Fatalf("seed org-A second response: %v", err)
	}
	tokenB, _ := seedTenantWithKey(t, pool, "org-B")

	// Drive org-A's run to a terminal completion on the REAL provider — the listed run is genuine.
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

	// The REAL router over the real store (no SDK — a plain HTTP client stands in for curl).
	srv := httptest.NewServer(api.NewRouter(repo, repo, repo, repo, repo, repo, nil, nil, nil, nil, nil, nil, nil, nil, api.SSEConfig{}, nil, nil))
	t.Cleanup(srv.Close)

	// Org-A lists its run history: a limit=1 page returns one row + a next_cursor over the rest.
	pageA := listRunHistory(t, srv.URL, tokenA, "limit=1")
	if len(pageA.Data) != 1 || !pageA.HasMore || pageA.NextCursor == nil {
		t.Fatalf("org-A page: len=%d has_more=%v cursor=%v, want 1 row + a further page", len(pageA.Data), pageA.HasMore, pageA.NextCursor)
	}

	// Org-A's full history contains the completed run the real provider produced.
	full := listRunHistory(t, srv.URL, tokenA, "limit=10")
	var sawCompletedRun bool
	for _, raw := range full.Data {
		blob, _ := json.Marshal(raw)
		var r contracts.Response
		if err := json.Unmarshal(blob, &r); err != nil {
			t.Fatalf("decode org-A history row: %v (row=%s)", err, blob)
		}
		if string(r.ID) == respA && r.Status == "completed" {
			sawCompletedRun = true
		}
	}
	if !sawCompletedRun {
		t.Fatalf("org-A history did not carry the completed real run %s: %+v", respA, full.Data)
	}

	// Org-B presenting org-A's cursor is an EXPLICIT 400 invalid_cursor (TEN-001 cursor-fuzz), not an empty 200.
	status, body := getRaw(t, srv.URL, tokenB, "limit=1&after="+url.QueryEscape(*pageA.NextCursor))
	if status != http.StatusBadRequest {
		t.Fatalf("org-B with org-A's cursor: status=%d body=%s, want 400", status, body)
	}
	var prob contracts.Problem
	if err := json.Unmarshal(body, &prob); err != nil || prob.Code != "invalid_cursor" {
		t.Fatalf("org-B foreign-cursor problem code = %q (err=%v), want invalid_cursor", prob.Code, err)
	}

	// And org-B's OWN history is empty — RLS confines the list to the caller's tenant.
	pageB := listRunHistory(t, srv.URL, tokenB, "")
	if len(pageB.Data) != 0 {
		t.Fatalf("org-B sees %d row(s) of history, want 0 (RLS confines the list)", len(pageB.Data))
	}

	t.Logf("run-history-list PASS: org-A (%s) lists a REAL provider run over the router; org-B's key is rejected with 400 invalid_cursor on org-A's cursor and sees an empty history.", runA)
}

// listRunHistory GETs /v1/responses with a bearer token and returns the decoded page.
func listRunHistory(t *testing.T, base, token, query string) contracts.Page {
	t.Helper()
	status, body := getRaw(t, base, token, query)
	if status != http.StatusOK {
		t.Fatalf("list status = %d, want 200 (body=%s)", status, body)
	}
	var p contracts.Page
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("decode page: %v (body=%s)", err, body)
	}
	return p
}

// getRaw GETs /v1/responses[?query] with a bearer token and returns the status + raw body.
func getRaw(t *testing.T, base, token, query string) (int, []byte) {
	t.Helper()
	u := base + "/v1/responses"
	if query != "" {
		u += "?" + query
	}
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, body
}
