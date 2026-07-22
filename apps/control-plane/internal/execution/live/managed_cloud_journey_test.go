//go:build live

// This file is CASE=managed-cloud-journey, the E13 Task 11 (EXIT gate) live journey: the WHOLE managed-cloud
// spine on ONE running process with NO restart, ending in a REAL provider-one run. Where the per-task live
// smokes (second-tenant-provisioning, secret-rotation-restartless, run-history-list, artifact-retrieval,
// model-route-per-project, budget/edge-admission, tenancy-isolation) each boot their OWN throwaway process,
// this composes the restart-less SPINE the plan §T11 names — a real provider run, an SDK-surface steer, the
// run-history list, and the cross-tenant negative — on a SINGLE store + a SINGLE router, and proves the
// database process never restarted across every step (pg_postmaster_start_time is identical start-to-end).
//
// The restart-less property is itself the load-bearing claim (MCI-001 ProvisioningProof.restart_count=0): the
// SAME live process resolves each step. scripts/uat/managed-cloud runs this journey AND the per-task MCI
// smokes inline, so every MCI-00N step is proven live and the spine is proven restart-less here.
//
// HONEST CEILINGS:
//   - The steer is proven DURABLY ACCEPTED (202, queued onto the session's command stream — the E08 command
//     spine reached from the public API); its APPLICATION at the next loop boundary is proven by the e2e
//     TestSteerAppliesAtNextLoopBoundaryWithSequence (MCI-008's catalog proof), not re-driven here.
//   - The finer steps (secret-resolve, artifact download, budget/rate refusal, per-project model route,
//     binding connection_ref) are proven LIVE by their own CASE smokes (run inline by scripts/uat/
//     managed-cloud); this journey proves the restart-less SPINE + the real provider run + the cross-tenant
//     deny end to end on ONE process.
//   - SINGLE PROVIDER (provider-one); SINGLE DATABASE / runtime role (the E13 RLS ceiling, TEN-001/002).
//
// GATED: needs the credential + a throwaway Postgres + the reference engine; NOT part of make verify / CI.
// The credential is an opaque env-resolved secret, never printed.
package live

import (
	"bytes"
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

func TestManagedCloudJourney(t *testing.T) {
	secret := requireEnv(t, credentialEnv)
	engineDir := requireEnv(t, "PALAI_ENGINE_DIR")
	pgURL := requireEnv(t, "PALAI_COMPONENT_POSTGRES_URL")
	_ = secret // resolved through the env secret resolver; never referenced directly

	ctx := context.Background()

	// ONE store, opened ONCE: the restart-less anchor. Every step below runs on this store + the single
	// router built from it; the process is never restarted.
	repo, err := store.Open(ctx, pgURL)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(repo.Close)
	if err := repo.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	pool := repo.Spine().Pool()

	// The database process boot time at the START of the journey — asserted identical at the END, so a restart
	// anywhere in the spine would fail the journey (the concrete restart_count=0 proof).
	var bootStart string
	if err := pool.QueryRow(context.Background(), `SELECT pg_postmaster_start_time()::text`).Scan(&bootStart); err != nil {
		t.Fatalf("read pg boot time (start): %v", err)
	}

	// Two tenants on the SAME stack: A owns a real run; B is a second tenant with its own key.
	tokenA, tenantA, sessionA, respA, runA := seedTenantWithRun(t, pool, "managed-cloud journey: reply with the single word done.")
	tokenB, _ := seedTenantWithKey(t, pool, "org-B")

	// A second bare response gives tenant A a 2-row history, so a limit=1 page mints a next_cursor for the
	// cross-tenant cursor-reject step below (matches the run-history-list smoke).
	respA2 := newID("resp")
	if _, err := pool.Exec(storage.WithSystemScope(ctx),
		`INSERT INTO responses (id, organization_id, project_id, session_id, state, input) VALUES ($1,$2,$3,$4,'queued','{}'::jsonb)`,
		respA2, tenantA.Organization, tenantA.Project, sessionA); err != nil {
		t.Fatalf("seed tenant-A second response: %v", err)
	}

	// Step (real provider run): drive tenant A's run to a terminal completion on the REAL provider — the run
	// the rest of the journey lists, steers, and isolates is genuine (a real chatcmpl id).
	broker := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"provider-one": providerone.Adapter{}},
		Secrets:  modelbroker.EnvResolver{"provider-one": credentialEnv},
	})
	orch := execution.NewOrchestrator(repo, &subprocessDialer{engineDir: engineDir}, broker, toolbroker.New(tools.FileTool()))
	orch.SetModelRoute(execution.ModelRoute{Provider: "provider-one", Model: liveModel(), Secret: modelbroker.SecretRef("provider-one")})
	if err := orch.ExecuteAttempt(ctx, descriptor(runA, 1)); err != nil {
		t.Fatalf("execute tenant-A run on the real provider: %v", err)
	}
	chatID := lastProviderRequestID(t, pool, tenantA, runA)
	if chatID == "" {
		t.Fatal("tenant-A run produced no provider request id (the real provider must have answered)")
	}

	// The single router over the single store — the one process serving every HTTP step below.
	srv := httptest.NewServer(api.NewRouter(repo, repo, repo, repo, repo, repo, nil, nil, nil, nil, nil, nil, nil, nil, nil, api.SSEConfig{}, nil, nil))
	t.Cleanup(srv.Close)

	// Step (steer): the SDK-surface steer — POST a send_message/steer command to tenant A's session over the
	// public API. Acceptance is 202 (durably queued onto the command stream — the E08 spine reached from the
	// API); a duplicate command_id returns the original. The command carries a durable id in the response.
	cmdID := newID("cmd")
	status, body := postCommand(t, srv.URL, tokenA, sessionA, contracts.CommandCreateRequest{
		CommandID: contracts.CommandID(cmdID), Kind: "send_message", Delivery: "steer", Message: "focus on the summary",
	})
	if status != http.StatusAccepted {
		t.Fatalf("steer command: status=%d body=%s, want 202 accepted", status, body)
	}
	if !bytes.Contains(body, []byte(cmdID)) {
		t.Fatalf("steer command response did not carry the durable command id %s: %s", cmdID, body)
	}

	// Step (list): tenant A lists its run history over the router; the completed real run is present.
	full := listRunHistory(t, srv.URL, tokenA, "limit=10")
	var sawCompleted bool
	for _, raw := range full.Data {
		blob, _ := json.Marshal(raw)
		var r contracts.Response
		if err := json.Unmarshal(blob, &r); err != nil {
			t.Fatalf("decode tenant-A history row: %v (row=%s)", err, blob)
		}
		if string(r.ID) == respA && r.Status == "completed" {
			sawCompleted = true
		}
	}
	if !sawCompleted {
		t.Fatalf("tenant-A history did not carry the completed real run %s: %+v", respA, full.Data)
	}

	// Step (cross-tenant negative): tenant B is denied tenant A's run at every surface —
	//  1. GET the run by id  -> 404 (no existence disclosure);
	//  2. reuse A's cursor   -> 400 invalid_cursor (not a silent empty page);
	//  3. list its own       -> empty (RLS confines the list to the caller).
	pageA := listRunHistory(t, srv.URL, tokenA, "limit=1")
	if pageA.NextCursor == nil {
		t.Fatal("tenant-A limit=1 page minted no cursor (need a 2nd row to page)")
	}
	if code, gb := getResponseByID(t, srv.URL, tokenB, respA); code != http.StatusNotFound {
		t.Fatalf("tenant-B GET of tenant-A run %s: status=%d body=%s, want 404 (cross-tenant deny)", respA, code, gb)
	}
	if code, gb := getResponseByID(t, srv.URL, tokenA, respA); code != http.StatusOK {
		t.Fatalf("tenant-A GET of its own run %s: status=%d body=%s, want 200", respA, code, gb)
	}
	if code, fb := getRaw(t, srv.URL, tokenB, "limit=1&after="+url.QueryEscape(*pageA.NextCursor)); code != http.StatusBadRequest {
		t.Fatalf("tenant-B with tenant-A's cursor: status=%d body=%s, want 400 invalid_cursor", code, fb)
	}
	if pageB := listRunHistory(t, srv.URL, tokenB, ""); len(pageB.Data) != 0 {
		t.Fatalf("tenant-B sees %d row(s) of history, want 0 (RLS confines the list)", len(pageB.Data))
	}

	// Restart-less proof: the database process that served the FIRST step is the SAME one serving the LAST —
	// pg_postmaster_start_time is unchanged, so no restart happened anywhere across the spine (restart_count=0).
	var bootEnd string
	if err := pool.QueryRow(context.Background(), `SELECT pg_postmaster_start_time()::text`).Scan(&bootEnd); err != nil {
		t.Fatalf("read pg boot time (end): %v", err)
	}
	if bootStart != bootEnd {
		t.Fatalf("the stack RESTARTED mid-journey: pg boot %q -> %q (restart-less spine broken)", bootStart, bootEnd)
	}

	t.Logf("managed-cloud journey PASS (restart-less spine): one process (pg boot %s) provisioned two tenants, drove a REAL provider run (%s), accepted an SDK-surface steer (%s), listed tenant-A history, and denied tenant-B the run at every surface (404 / 400 invalid_cursor / empty).",
		bootStart, chatID, cmdID)
}

// postCommand POSTs a durable command to /v1/sessions/{id}/commands with a bearer token and returns the
// status + raw body. No Idempotency-Key: the command_id carries idempotency (see commands.go).
func postCommand(t *testing.T, base, token, sessionID string, req contracts.CommandCreateRequest) (int, []byte) {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal command: %v", err)
	}
	httpReq, err := http.NewRequest(http.MethodPost, base+"/v1/sessions/"+sessionID+"/commands", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("build command request: %v", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("do command request: %v", err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read command body: %v", err)
	}
	return resp.StatusCode, out
}

// getResponseByID GETs /v1/responses/{id} with a bearer token and returns the status + raw body (the
// cross-tenant run retrieval — 404 for a foreign id, 200 for the owner).
func getResponseByID(t *testing.T, base, token, id string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+"/v1/responses/"+id, nil)
	if err != nil {
		t.Fatalf("build response-get request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do response-get request: %v", err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response-get body: %v", err)
	}
	return resp.StatusCode, out
}
