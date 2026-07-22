//go:build live

// This file is CASE=edge-admission, the E13 Task 7 live smoke: §20.12 basic-tier edge admission
// control on a REAL control-plane router over a real Postgres, ending in a REAL provider-one run.
// It proves three things a burst of clients exercises:
//
//  1. REQUEST-RATE: with a token bucket of depth 2, a rapid burst of 5 POST /v1/responses on ONE API
//     key admits the first 2 and sheds the rest as 429 rate_limited + Retry-After — before any run is
//     created.
//  2. PER-PROJECT QUEUED-RUN BOUND: with MaxQueuedRuns=3 and no worker draining the queue, a burst of
//     8 admits exactly 3 (each a real queued run) and rejects 5 as 429 concurrency_exceeded (the §20.10
//     stable admission-capacity code) — and NO run is lost or duplicated: the project holds exactly 3
//     runs and 3 responses, one per 202, none per 429.
//  3. THE ACCEPTED QUEUE IS GENUINE: one accepted run DRAINS on the REAL provider (a completed
//     model_request), and draining it creates no duplicate run — the caps shed load without corrupting
//     the admitted work.
//
// HONEST CEILINGS (named here, matching the plan):
//   - The request-rate limiter is an IN-PROCESS token bucket — correct for the single-replica compose
//     deployment (every request for a key hits one process). A multi-replica distributed limiter +
//     weighted per-tenant fairness (QUO-002) is SaaS scope, not this gate.
//   - The queued-run bound is enforced at admission against a ReadCommitted count, which can overshoot
//     by the number of admissions racing past it — a soft basic-tier cap, not an exact semaphore.
//   - SINGLE PROVIDER: the real drain is provider-one only; the admission logic is provider-agnostic.
//   - SURFACE: the request-rate limiter governs the PUBLIC API only. Automation-born runs (trigger/
//     webhook/schedule/inbound deliveries) mount outside it and are bounded by their own AUT-010
//     backpressure — but they consume the SAME per-project run caps, so those still bound them.
//
// GATED: serialized with every LIVE smoke on the shared :local Docker stack; NOT part of make verify.
// Skips cleanly without creds. The credential is an opaque env-resolved secret, never printed.
package live

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// TestLiveEdgeAdmissionBurstRateLimited is CASE=edge-admission (see the file ceilings).
func TestLiveEdgeAdmissionBurstRateLimited(t *testing.T) {
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

	// --- Part 1: the per-API-key request-rate token bucket sheds a rapid burst ---
	rateToken, _ := seedTenantWithKey(t, pool, "edge-rate")
	rateSrv := httptest.NewServer(api.NewRouter(repo, repo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		api.SSEConfig{}, nil, nil, api.WithEdgeLimits(api.EdgeLimits{RequestRatePerSec: 0.001, RequestBurst: 2})))
	t.Cleanup(rateSrv.Close)

	rateAccepted, rateLimited := 0, 0
	for i := 0; i < 5; i++ {
		code, body := postResponse(t, rateSrv.URL, rateToken, fmt.Sprintf("edge-rate-%d", i))
		switch code {
		case http.StatusAccepted:
			rateAccepted++
		case http.StatusTooManyRequests:
			rateLimited++
			assertProblemCode(t, body, "rate_limited")
		default:
			t.Fatalf("rate burst %d: status %d, body %s", i, code, body)
		}
	}
	if rateAccepted != 2 || rateLimited != 3 {
		t.Fatalf("rate burst: accepted=%d limited=%d, want 2 accepted / 3 limited (bucket depth 2)", rateAccepted, rateLimited)
	}

	// --- Part 2: the per-project queued-run bound sheds a burst with no run lost or duplicated ---
	// No worker drains the queue here, so every accepted run stays queued and the bound bites deterministically.
	capToken, capTenant := seedTenantWithKey(t, pool, "edge-cap")
	capSrv := httptest.NewServer(api.NewRouter(repo, repo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		api.SSEConfig{}, nil, nil, api.WithEdgeLimits(api.EdgeLimits{MaxQueuedRuns: 3})))
	t.Cleanup(capSrv.Close)

	acceptedRuns := map[string]struct{}{}
	capLimited := 0
	for i := 0; i < 8; i++ {
		code, body := postResponse(t, capSrv.URL, capToken, fmt.Sprintf("edge-cap-%d", i))
		switch code {
		case http.StatusAccepted:
			acceptedRuns[runIDFromBody(t, body)] = struct{}{}
		case http.StatusTooManyRequests:
			capLimited++
			assertProblemCode(t, body, "concurrency_exceeded")
		default:
			t.Fatalf("cap burst %d: status %d, body %s", i, code, body)
		}
	}
	if len(acceptedRuns) != 3 || capLimited != 5 {
		t.Fatalf("cap burst: accepted=%d limited=%d, want 3 distinct accepted / 5 limited (queued bound 3)", len(acceptedRuns), capLimited)
	}
	// No run lost or duplicated: exactly one run + one response per 202, and nothing for a 429.
	if n := countRows(t, pool, `SELECT count(*) FROM runs WHERE organization_id=$1 AND project_id=$2`, capTenant.Organization, capTenant.Project); n != 3 {
		t.Fatalf("runs in project = %d, want exactly 3 (one per accepted admission, none per 429)", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM responses WHERE organization_id=$1 AND project_id=$2`, capTenant.Organization, capTenant.Project); n != 3 {
		t.Fatalf("responses in project = %d, want exactly 3", n)
	}

	// --- Part 3: an accepted run DRAINS on the REAL provider; draining it duplicates no run ---
	var drainRun string
	for id := range acceptedRuns {
		drainRun = id
		break
	}
	broker := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"provider-one": providerone.Adapter{}},
		Secrets:  modelbroker.EnvResolver{"provider-one": credentialEnv},
	})
	orch := execution.NewOrchestrator(repo, &subprocessDialer{engineDir: engineDir}, broker, toolbroker.New(tools.FileTool()))
	orch.SetModelRoute(execution.ModelRoute{Provider: "provider-one", Model: liveModel(), Secret: modelbroker.SecretRef("provider-one")})
	if err := orch.ExecuteAttempt(ctx, descriptor(drainRun, 1)); err != nil {
		t.Fatalf("drain accepted run on the real provider: %v", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM model_requests WHERE run_id=$1 AND state='completed'`, drainRun); n < 1 {
		t.Fatalf("completed model_requests for drained run = %d, want >=1 (the real provider must have answered)", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM runs WHERE organization_id=$1 AND project_id=$2`, capTenant.Organization, capTenant.Project); n != 3 {
		t.Fatalf("runs in project after draining one = %d, want still exactly 3 (no duplicate)", n)
	}

	t.Logf("edge-admission PASS: request-rate bucket shed 3/5, queued bound shed 5/8 as 429 with zero run loss/duplication, and an accepted run drained on the real provider.")
}

// postResponse POSTs one create over real HTTP with the bearer key and idempotency key, returning the
// status and body. The input is a plain string prompt — the proven-good shape the reference engine
// wraps as a user message.
func postResponse(t *testing.T, baseURL, token, idemKey string) (int, []byte) {
	t.Helper()
	body := fmt.Sprintf(`{"model":%q,"input":"reply with the single word done."}`, liveModel())
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/responses", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Idempotency-Key", idemKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/responses: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func runIDFromBody(t *testing.T, body []byte) string {
	t.Helper()
	var env struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.RunID == "" {
		t.Fatalf("no run_id in 202 body %s (err %v)", body, err)
	}
	return env.RunID
}

func assertProblemCode(t *testing.T, body []byte, want string) {
	t.Helper()
	var p struct {
		Code   string `json:"code"`
		Status int    `json:"status"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("decode problem %s: %v", body, err)
	}
	if p.Code != want || p.Status != http.StatusTooManyRequests {
		t.Fatalf("problem code=%q status=%d, want %q / 429", p.Code, p.Status, want)
	}
}
