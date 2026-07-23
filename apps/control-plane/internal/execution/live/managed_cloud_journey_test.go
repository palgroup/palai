//go:build live

// This file is CASE=managed-cloud-journey, the E13 Task 11 (EXIT gate) live journey: the restart-less SPINE
// the plan §T11 names, on ONE running process with NO restart, ending in a REAL provider-one run. On a SINGLE
// store + a SINGLE router it PROVISIONS a tenant entirely over the public API (POST /v1/organizations,
// /v1/projects, PATCH config_policy, POST /v1/api-keys), then drives that tenant's run on the real provider,
// steers it, lists its history, and denies the cross-tenant read — and proves the database process never
// restarted across the spine (pg_postmaster_start_time is identical start-to-end).
//
// The restart-less property is the load-bearing claim (MCI-001 ProvisioningProof.restart_count=0): the SAME
// live process resolves each SPINE step. scripts/uat/managed-cloud runs this journey AND the per-task MCI
// smokes inline, so every MCI-00N step is proven live and the spine is proven restart-less here.
//
// HONEST CEILINGS:
//   - The provisioning is REAL HTTP against this process's own in-proc router (httptest.NewServer over the
//     shipped api.NewRouter) — NOT the packaged Docker compose stack; the HTTP contract is identical.
//   - The steer is proven DURABLY ACCEPTED (202 onto the command stream — the E08 command spine reached over
//     the public API); its APPLICATION at the next loop boundary is the e2e proof
//     TestSteerAppliesAtNextLoopBoundaryWithSequence (MCI-008's catalog proof), not re-driven here. This
//     journey drives raw HTTP (the SAME surface @palai/sdk targets); the SDK client shape is proven by its
//     own vitest suite, not connected to a live server.
//   - The finer steps (secret-resolve, artifact download, budget/rate refusal, per-project model route,
//     binding connection_ref) are proven LIVE by their own CASE smokes (run inline by scripts/uat/
//     managed-cloud); this journey proves the restart-less SPINE + provisioning + real run + cross-tenant deny.
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
	"github.com/palgroup/palai/apps/control-plane/internal/identity"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
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

	// The single router over the single store, with the provisioning surface (internal/identity) mounted —
	// the one process serving every HTTP step below, including the tenant-provisioning POSTs.
	idstore := identity.New(pool)
	srv := httptest.NewServer(api.NewRouter(repo, repo, repo, repo, repo, repo, nil, nil, nil, nil, nil, nil, nil, idstore, nil, api.SSEConfig{}, nil, nil))
	t.Cleanup(srv.Close)

	// A bootstrap admin key (empty scope = unrestricted) authenticates the first cross-tenant POST. Its
	// plaintext exists only here; only its hash reaches the database.
	bootstrapToken, _ := seedTenantWithKey(t, pool, "bootstrap")

	// Step (provision-org): POST /v1/organizations mints a brand-new tenant — org + default project + admin
	// key — through the public API on THIS running process (no restart). The admin key plaintext is in the
	// create response only.
	var org struct {
		ID               string `json:"id"`
		DefaultProjectID string `json:"default_project_id"`
		AdminAPIKey      struct {
			ID  string `json:"id"`
			Key string `json:"key"`
		} `json:"admin_api_key"`
	}
	decodeJSON(t, expectJSON(t, http.MethodPost, srv.URL, bootstrapToken, "/v1/organizations", `{"display_name":"managed-cloud-journey"}`, http.StatusCreated), &org)
	if org.ID == "" || org.DefaultProjectID == "" || org.AdminAPIKey.Key == "" {
		t.Fatalf("provisioned organization is incomplete: %+v", org)
	}
	adminA := org.AdminAPIKey.Key

	// Step (provision-project): POST /v1/projects registers a project in the new tenant's org.
	var project struct {
		ID string `json:"id"`
	}
	decodeJSON(t, expectJSON(t, http.MethodPost, srv.URL, adminA, "/v1/projects", `{"display_name":"journey-project"}`, http.StatusCreated), &project)
	if project.ID == "" {
		t.Fatal("provisioned project has no id")
	}

	// Step (config-policy): PATCH /v1/projects/{id} writes a config_policy (allowed_models includes the run's
	// model, so it is causally consumed without advertising a tool), and GET reads it back — the §14 project
	// layer is API-reachable and applied.
	expectJSON(t, http.MethodPatch, srv.URL, adminA, "/v1/projects/"+project.ID,
		`{"config_policy":{"allowed_models":["`+liveModel()+`"]}}`, http.StatusOK)
	var readBack struct {
		ConfigPolicy json.RawMessage `json:"config_policy"`
	}
	decodeJSON(t, expectJSON(t, http.MethodGet, srv.URL, adminA, "/v1/projects/"+project.ID, "", http.StatusOK), &readBack)
	if !bytes.Contains(readBack.ConfigPolicy, []byte("allowed_models")) {
		t.Fatalf("config_policy did not take on the resolver: %s", readBack.ConfigPolicy)
	}

	// Step (provision-api-key): POST /v1/api-keys mints the project's own key — the token the rest of the
	// journey (run, steer, list) acts under.
	var apiKey struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	decodeJSON(t, expectJSON(t, http.MethodPost, srv.URL, adminA, "/v1/api-keys", `{"project_id":"`+project.ID+`"}`, http.StatusCreated), &apiKey)
	if apiKey.Key == "" {
		t.Fatal("provisioned api key has no plaintext")
	}
	tokenA := apiKey.Key
	tenantA := coordinator.Tenant{Organization: org.ID, Project: project.ID}

	// Seed a queued run + a second bare response for the provisioned tenant (the run itself is orchestrator-
	// driven, not an HTTP claim; the 2nd response gives A a paginable history for the cross-tenant cursor step).
	sessionA, respA, runA := newID("ses"), newID("resp"), newID("run")
	do := func(sql string, args ...any) {
		if _, err := pool.Exec(storage.WithSystemScope(ctx), sql, args...); err != nil {
			t.Fatalf("seed exec %q: %v", sql, err)
		}
	}
	do(`INSERT INTO sessions (id, organization_id, project_id) VALUES ($1,$2,$3)`, sessionA, tenantA.Organization, tenantA.Project)
	do(`INSERT INTO responses (id, organization_id, project_id, session_id, state, input) VALUES ($1,$2,$3,$4,'queued',$5)`,
		respA, tenantA.Organization, tenantA.Project, sessionA, encodeJSONString("managed-cloud journey: reply with the single word done."))
	do(`INSERT INTO runs (id, organization_id, project_id, session_id, response_id, state) VALUES ($1,$2,$3,$4,$5,'queued')`,
		runA, tenantA.Organization, tenantA.Project, sessionA, respA)
	respA2 := newID("resp")
	do(`INSERT INTO responses (id, organization_id, project_id, session_id, state, input) VALUES ($1,$2,$3,$4,'queued','{}'::jsonb)`,
		respA2, tenantA.Organization, tenantA.Project, sessionA)

	// A second seeded tenant with its own key — the outsider whose key must be denied A's resources.
	tokenB, _ := seedTenantWithKey(t, pool, "org-B")

	// Step (real-run): drive the provisioned tenant's run to a terminal completion on the REAL provider —
	// the run the rest of the journey lists, steers, and isolates is genuine (a real chatcmpl id).
	broker := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"provider-one": providerone.Adapter{}},
		Secrets:  modelbroker.EnvResolver{"provider-one": credentialEnv},
	})
	orch := execution.NewOrchestrator(repo, &subprocessDialer{engineDir: engineDir}, broker, toolbroker.New(tools.FileTool()))
	orch.SetModelRoute(execution.ModelRoute{Provider: "provider-one", Model: liveModel(), Secret: modelbroker.SecretRef("provider-one")})
	if err := orch.ExecuteAttempt(ctx, descriptor(runA, 1)); err != nil {
		t.Fatalf("execute the provisioned tenant's run on the real provider: %v", err)
	}
	chatID := lastProviderRequestID(t, pool, tenantA, runA)
	if chatID == "" {
		t.Fatal("the provisioned tenant's run produced no provider request id (the real provider must have answered)")
	}

	// Step (steer): POST a send_message/steer command to the run's session over the public API. Acceptance is
	// 202 (durably queued onto the command stream — the E08 spine reached over the API); the response carries
	// the durable command id.
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

	// Step (list-history): tenant A lists its run history over the router; the completed real run is present.
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

	// Step (cross-tenant-deny): tenant B is denied tenant A's run at every surface —
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

	t.Logf("managed-cloud journey PASS (restart-less spine): one process (pg boot %s) PROVISIONED a tenant over the public API (org %s, project %s, key %s), applied its config_policy, drove a REAL provider run (%s), accepted an API steer (%s), listed tenant-A history, and denied tenant-B the run at every surface (404 / 400 invalid_cursor / empty).",
		bootStart, org.ID, project.ID, apiKey.ID, chatID, cmdID)
}

// sendJSON issues an authenticated JSON request (POST/PATCH/GET) against the running router and returns the
// status + raw body. No Idempotency-Key: the provisioning + command routes carry their own idempotency.
func sendJSON(t *testing.T, method, base, token, path, body string) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequest(method, base+path, rdr)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s %s body: %v", method, path, err)
	}
	return resp.StatusCode, out
}

// expectJSON issues a request and fails unless it carried the wanted status, returning the body for decoding.
func expectJSON(t *testing.T, method, base, token, path, body string, want int) []byte {
	t.Helper()
	status, out := sendJSON(t, method, base, token, path, body)
	if status != want {
		t.Fatalf("%s %s status = %d, want %d: %s", method, path, status, want, out)
	}
	return out
}

func decodeJSON(t *testing.T, body []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("decode %T: %v (body=%s)", v, err, body)
	}
}

// postCommand POSTs a durable command to /v1/sessions/{id}/commands with a bearer token and returns the
// status + raw body. No Idempotency-Key: the command_id carries idempotency (see commands.go).
func postCommand(t *testing.T, base, token, sessionID string, req contracts.CommandCreateRequest) (int, []byte) {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal command: %v", err)
	}
	return sendJSON(t, http.MethodPost, base, token, "/v1/sessions/"+sessionID+"/commands", string(raw))
}

// getResponseByID GETs /v1/responses/{id} with a bearer token and returns the status + raw body (the
// cross-tenant run retrieval — 404 for a foreign id, 200 for the owner).
func getResponseByID(t *testing.T, base, token, id string) (int, []byte) {
	t.Helper()
	return sendJSON(t, http.MethodGet, base, token, "/v1/responses/"+id, "")
}
