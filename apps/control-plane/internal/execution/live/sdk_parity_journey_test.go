//go:build live

// This file is the E16 T8 EXIT-gate live journey (plan §T8, journey 63.1) — the capstone. On ONE running
// in-process control-plane (httptest over the shipped api.NewRouter + WithModelRoutes) over a throwaway
// Postgres, it proves the load-bearing exit sentence LIVE and end to end:
//
//  1. FOUR-CLIENT MECHANICAL PARITY (the crown, API-012): a model connection is provisioned via the T1 API, a
//     REAL provider-one run completes, and the SAME response is retrieved + normalized by ALL FOUR clients —
//     the TypeScript, Python, and Go SDKs plus the palai CLI — each as its own real subprocess against the live
//     server. Their normalized decodes are asserted byte-identical by ONE canonical-bytes diff (the T2 harness
//     discipline applied to the JOURNEY outputs, not four hand-written assertions), and a uat.ThreeLanguageEqualityProof
//     is built from the four real outputs and must pass Complete() (which RE-CANONICALIZES them and recomputes
//     the equality digest — a divergent client fails).
//  2. RETAINED RETRIEVAL ACROSS A RESTART: the server is torn down and rebuilt over the SAME store; the retained
//     run still retrieves 200 (the store outlives the process).
//  3. GATEWAY-OFF DIRECT PATH (MOD-003, the exit sentence's second clause): the openai-compatible route points at
//     a local STAND-IN proxy; the proxy is KILLED and a run on the gateway route typed-fails, while the DIRECT
//     provider-one route keeps serving a REAL run (real chatcmpl id). A uat.GatewayOffProof is built and asserted.
//  4. SECOND REAL PROVIDER (MOD-001): a provider-two (Anthropic) route serves a REAL completion (a msg_ id) on
//     the same stack — two independent provider families, two real live smokes.
//
// HONEST CEILINGS (named, per the live-tier convention):
//   - The stand-in gateway is a LOCAL httptest proxy explicitly named a stand-in; a real LiteLLM/private-server
//     gateway drill is the §6 operator leg (the openai-compatible adapter's BaseURL is parametric).
//   - The typed-410 SDK surface + the store:false purge are proven DETERMINISTICALLY (the corpus gone-410 across
//     the three SDK runners + the API-015 server test TestAdmitPurgedReplayRenders410Tombstone); the SDK live
//     clients surface a typed error on a missing id here, but a full retention-purge 410 is the deterministic tier.
//   - SINGLE DATABASE / runtime role (the E13 RLS ceiling). The provisioning is REAL HTTP against this process's
//     own in-proc router — the HTTP contract is identical to the packaged compose stack.
//
// GATED: needs OPENAI_API_KEY + ANTROPHIC_API_KEY + a throwaway Postgres + the reference engine + node + uv +
// go on PATH; NOT part of make verify / CI. Credentials are opaque env-resolved secrets, never printed, never
// on argv, never in a log or the evidence bundle.
package live

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	openaicompatible "github.com/palgroup/palai/adapters/models/openai_compatible"
	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	providertwo "github.com/palgroup/palai/adapters/models/provider_two"
	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	"github.com/palgroup/palai/apps/control-plane/internal/identity"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
	"github.com/palgroup/palai/packages/coordinator"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
	"github.com/palgroup/palai/storage"
	"github.com/palgroup/palai/tests/uat"

	"github.com/jackc/pgx/v5/pgxpool"
)

const anthropicCredentialEnv = "ANTROPHIC_API_KEY"

// TestSDKParityJourney is the E16 T8 EXIT gate live journey (see the file header).
func TestSDKParityJourney(t *testing.T) {
	openaiKey := requireEnv(t, credentialEnv)
	anthropicKey := requireEnv(t, anthropicCredentialEnv)
	engineDir := requireEnv(t, "PALAI_ENGINE_DIR")
	pgURL := requireEnv(t, "PALAI_COMPONENT_POSTGRES_URL")
	root := repoRootLive(t)
	// The four client runtimes must be present — this is a workstation gate.
	for _, bin := range []string{"go", "node", "uv"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Fatalf("%s not on PATH: the four-client parity journey needs go + node + uv on the workstation", bin)
		}
	}

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

	var rawKey [32]byte
	if _, err := rand.Read(rawKey[:]); err != nil {
		t.Fatalf("mint master key: %v", err)
	}
	key, err := identity.ParseMasterKey(hex.EncodeToString(rawKey[:]))
	if err != nil {
		t.Fatalf("ParseMasterKey: %v", err)
	}
	idstore := identity.New(pool)
	secretStore := identity.NewSecretStore(pool, key)

	// The stand-in gateway upstream: a local proxy that speaks a minimal OpenAI ChatCompletions SSE for every
	// POST (both the openai-compatible probe and the execute). It is EXPLICITLY a stand-in — killed below to
	// prove the direct routes are load-bearing (a real LiteLLM/private-server gateway is the §6 operator leg).
	standIn := httptest.NewServer(http.HandlerFunc(standInChatCompletions))

	// The broker carries all THREE adapter families on one registry (main.go's shape). The openai-compatible
	// adapter's BaseURL is the stand-in; provider-one and provider-two ignore it and dial their REAL endpoints.
	broker := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{
			"provider-one":      providerone.Adapter{},
			"provider-two":      providertwo.Adapter{},
			"openai-compatible": openaicompatible.Adapter{Adapter: providerone.Adapter{BaseURL: standIn.URL}, Prober: openaicompatible.NewProber()},
		},
		Secrets: execution.RouteSecretResolver{
			Lookup: func(org, name string) ([]byte, bool, error) { return secretStore.Resolve(ctx, org, name) },
			Fallback: modelbroker.EnvResolver{
				"provider-one": credentialEnv,
				"provider-two": anthropicCredentialEnv,
			},
		},
	})
	orch := execution.NewOrchestrator(repo, &subprocessDialer{engineDir: engineDir}, broker, toolbroker.New(tools.FileTool()))
	// A deliberately-unusable env default: a run that fell back to it instead of routing could not complete, so
	// per-project routing is load-bearing for every leg below.
	orch.SetModelRoute(execution.ModelRoute{Provider: "provider-one", Model: "palai-no-such-deployment-model", Secret: modelbroker.SecretRef("provider-one")})

	srv := httptest.NewServer(api.NewRouter(repo, repo, repo, repo, repo, repo, nil, nil, nil, nil, nil, nil, nil, idstore, nil, api.SSEConfig{}, nil, nil, api.WithModelRoutes(repo)))
	t.Cleanup(srv.Close)

	// --- Provision tenant A over the API + a provider-one route; execute the SHARED run ---
	bootstrapToken, _ := seedTenantWithKey(t, pool, "bootstrap")
	tenantA, tokenA := provisionAPITenant(t, srv.URL, bootstrapToken, "sdk-parity-A")
	publishRoute(t, ctx, repo, secretStore, tenantA, "provider-one", liveModel(), openaiKey)

	sharedRun, sharedResp := seedParityRun(t, pool, tenantA, "reply with the single word done.")
	if err := orch.ExecuteAttempt(ctx, descriptor(sharedRun, 1)); err != nil {
		t.Fatalf("execute the shared parity run on the real provider: %v", err)
	}
	sharedChatID := lastProviderRequestID(t, pool, tenantA, sharedRun)
	if sharedChatID == "" {
		t.Fatal("the shared parity run produced no provider request id (the real provider must have answered)")
	}

	// --- THE CROWN: four clients retrieve + normalize the SAME response; the decodes must be byte-identical ---
	clientEnv := []string{"PALAI_BASE_URL=" + srv.URL, "PALAI_API_KEY=" + tokenA, "PALAI_LIVE_RESPONSE_ID=" + sharedResp}
	outputs := map[string]json.RawMessage{
		"go":         runParityClient(t, root, filepath.Join(root, "sdks", "go"), clientEnv, "go", "run", "./runner"),
		"python":     runParityClient(t, root, root, clientEnv, "uv", "run", "--locked", "--project", "sdks/python", "python", "sdks/python/conformance/live_retrieve.py"),
		"typescript": runParityClient(t, root, root, clientEnv, "node", "--experimental-strip-types", "sdks/typescript/test/live-retrieve.ts"),
		"cli":        runParityClient(t, root, root, clientEnv, "go", "run", "./cmd/cli", "response", "get", sharedResp),
	}
	agreed, ok := canonicalizeJSON(outputs["go"])
	if !ok {
		t.Fatalf("go client emitted non-JSON: %s", outputs["go"])
	}
	for _, client := range uat.EqualityClients {
		got, ok := canonicalizeJSON(outputs[client])
		if !ok {
			t.Fatalf("%s client emitted non-JSON: %s", client, outputs[client])
		}
		if got != agreed {
			t.Fatalf("SDK PARITY FAILED: %s client diverged\n  %s = %s\n  go = %s", client, client, got, agreed)
		}
	}
	// Build the crown proof from the four REAL outputs and assert it passes Complete() (it re-canonicalizes them
	// and recomputes the equality digest — the mechanical diff hoisted into the evidence proof).
	equalityProof := uat.ThreeLanguageEqualityProof{
		RunID:          sharedRun,
		ClientOutputs:  outputs,
		EqualityDigest: hashPartsLive(agreed),
	}
	if !equalityProof.Complete() {
		t.Fatal("the ThreeLanguageEqualityProof built from the four live client outputs did not pass Complete()")
	}
	t.Logf("PARITY PASS (crown): all four clients (TS/Python/Go SDKs + CLI) decoded the shared run %s identically: %s (real provider run %s)", sharedResp, agreed, sharedChatID)

	// --- RESTART: rebuild the server over the SAME store; the retained run still retrieves 200 ---
	srv.Close()
	srv2 := httptest.NewServer(api.NewRouter(repo, repo, repo, repo, repo, repo, nil, nil, nil, nil, nil, nil, nil, idstore, nil, api.SSEConfig{}, nil, nil, api.WithModelRoutes(repo)))
	t.Cleanup(srv2.Close)
	if code, body := getResponseByID(t, srv2.URL, tokenA, sharedResp); code != http.StatusOK {
		t.Fatalf("after restart, retained retrieval of %s: status=%d body=%s, want 200", sharedResp, code, body)
	}
	t.Logf("RESTART PASS: the retained run %s retrieved 200 on a fresh server over the same store", sharedResp)

	// --- GATEWAY-OFF: kill the stand-in; the gateway route fails, the direct provider-one route still serves ---
	tenantG, _ := provisionAPITenant(t, srv2.URL, bootstrapToken, "sdk-parity-gateway")
	publishRoute(t, ctx, repo, secretStore, tenantG, "openai-compatible", "gpt-4.1-mini", openaiKey)
	standIn.Close() // the kill
	gwRun, _ := seedParityRun(t, pool, tenantG, "hello through the gateway")
	gwErr := orch.ExecuteAttempt(ctx, descriptor(gwRun, 1))
	if gwErr == nil {
		t.Fatal("the gateway-route run completed after the stand-in proxy was killed — the gateway is not actually off")
	}
	// A DIRECT provider-one run on tenant A still completes on the real provider (a fresh run row).
	directRun, _ := seedParityRun(t, pool, tenantA, "reply with the single word ok.")
	if err := orch.ExecuteAttempt(ctx, descriptor(directRun, 1)); err != nil {
		t.Fatalf("direct provider-one run failed with the gateway off: %v", err)
	}
	directChatID := lastProviderRequestID(t, pool, tenantA, directRun)
	gatewayOff := uat.GatewayOffProof{
		ConfigDigest: hashPartsLive("gateway=openai-compatible", "direct=provider-one", "direct=provider-two"),
		GatewayRoute: "gpt-4.1-mini@openai-compatible", ProxyKilled: true, GatewayRunFailed: true,
		DirectRunID: directRun, DirectProviderRequestID: directChatID, DirectCompleted: true,
	}
	if !gatewayOff.Complete() {
		t.Fatalf("the GatewayOffProof built from the live run did not pass Complete() (direct chatcmpl id %q)", directChatID)
	}
	t.Logf("GATEWAY-OFF PASS: the gateway route failed after the stand-in was killed (%v); the direct provider-one route still served a real run %s", gwErr, directChatID)

	// --- SECOND REAL PROVIDER: a provider-two (Anthropic) route serves a real completion (a msg_ id) ---
	tenantT, _ := provisionAPITenant(t, srv2.URL, bootstrapToken, "sdk-parity-provider-two")
	publishRoute(t, ctx, repo, secretStore, tenantT, "provider-two", liveModelTwo(), anthropicKey)
	p2Run, _ := seedParityRun(t, pool, tenantT, "reply with the single word done.")
	if err := orch.ExecuteAttempt(ctx, descriptor(p2Run, 1)); err != nil {
		t.Fatalf("execute the provider-two run on the real Anthropic provider: %v", err)
	}
	p2ID := lastProviderRequestID(t, pool, tenantT, p2Run)
	if !strings.HasPrefix(p2ID, "msg") {
		t.Fatalf("provider-two run produced request id %q, want an Anthropic msg_ id", p2ID)
	}
	t.Logf("PROVIDER-TWO PASS: a real Anthropic completion on the same stack (%s)", p2ID)

	t.Logf("SDK-PARITY JOURNEY PASS: four-client mechanical equality on a real provider-one run (%s); retained retrieval across a restart; gateway-off direct path (%s) with the stand-in killed; a real provider-two run (%s). LOCAL seam only — the real LiteLLM gateway + published-registry legs are §6/E18, named not claimed.", sharedChatID, directChatID, p2ID)
}

// liveModelTwo is the real provider-two (Anthropic) model id for the second-provider live smoke.
func liveModelTwo() string {
	if m := os.Getenv("PALAI_LIVE_MODEL_TWO"); m != "" {
		return m
	}
	return "claude-3-5-haiku-20241022"
}

// standInChatCompletions is the local stand-in gateway upstream: it answers every POST (the openai-compatible
// probe AND the execute) with a minimal OpenAI ChatCompletions SSE, so a run through the gateway completes WHILE
// the stand-in is alive and fails once it is killed. It is explicitly a stand-in, not a real gateway.
func standInChatCompletions(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "data: {\"id\":\"chatcmpl-standin\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"}}]}\n\n")
	fmt.Fprint(w, "data: {\"id\":\"chatcmpl-standin\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// provisionAPITenant provisions an org + project + project API key entirely over the public API (the managed-cloud
// pattern) and returns the tenant + the project API key the SDK clients authenticate with.
func provisionAPITenant(t *testing.T, base, bootstrapToken, name string) (coordinator.Tenant, string) {
	t.Helper()
	var org struct {
		ID               string `json:"id"`
		DefaultProjectID string `json:"default_project_id"`
		AdminAPIKey      struct {
			Key string `json:"key"`
		} `json:"admin_api_key"`
	}
	decodeJSON(t, expectJSON(t, http.MethodPost, base, bootstrapToken, "/v1/organizations", `{"display_name":"`+name+`"}`, http.StatusCreated), &org)
	if org.ID == "" || org.DefaultProjectID == "" || org.AdminAPIKey.Key == "" {
		t.Fatalf("provisioned organization %s is incomplete: %+v", name, org)
	}
	var apiKey struct {
		Key string `json:"key"`
	}
	decodeJSON(t, expectJSON(t, http.MethodPost, base, org.AdminAPIKey.Key, "/v1/api-keys", `{"project_id":"`+org.DefaultProjectID+`"}`, http.StatusCreated), &apiKey)
	if apiKey.Key == "" {
		t.Fatalf("provisioned api key for %s has no plaintext", name)
	}
	return coordinator.Tenant{Organization: org.ID, Project: org.DefaultProjectID}, apiKey.Key
}

// publishRoute stores the credential under the tenant's own org and publishes a default-alias model route bound
// to (provider, model, connection) — the production write surface, the credential sealed AES-256-GCM.
func publishRoute(t *testing.T, ctx context.Context, repo *store.Store, secrets *identity.SecretStore, tenant coordinator.Tenant, provider, model, credential string) {
	t.Helper()
	scope := middleware.Scope{Organization: tenant.Organization, Project: tenant.Project}
	secretRef := provider + "-credential"
	body, err := json.Marshal(map[string]string{"name": secretRef, "value": credential})
	if err != nil {
		t.Fatalf("encode secret body: %v", err)
	}
	if _, err := secrets.CreateSecretRef(ctx, scope, body); err != nil {
		t.Fatalf("CreateSecretRef(%s) error = %v", provider, err)
	}
	conn, err := repo.CreateModelConnection(ctx, scope, []byte(`{"provider":"`+provider+`","secret_ref":"`+secretRef+`"}`))
	connID := createdID(t, conn, err)
	route, err := repo.CreateModelRoute(ctx, scope, []byte(`{"name":"`+coordinator.DefaultModelRouteAlias+`"}`))
	routeID := createdID(t, route, err)
	rev, err := repo.CreateModelRouteRevision(ctx, scope, routeID, []byte(`{"model":"`+model+`","connection_id":"`+connID+`"}`))
	revID := createdID(t, rev, err)
	if _, err := repo.PublishModelRouteRevision(ctx, scope, routeID, revID); err != nil {
		t.Fatalf("PublishModelRouteRevision(%s) error = %v", provider, err)
	}
}

// seedParityRun seeds a queued session/response/run for a tenant and returns (runID, responseID). The run
// configures no tools, so it is a single real model step.
func seedParityRun(t *testing.T, pool *pgxpool.Pool, tenant coordinator.Tenant, prompt string) (runID, responseID string) {
	t.Helper()
	ctx := storage.WithSystemScope(context.Background())
	session, response, run := newID("ses"), newID("resp"), newID("run")
	do := func(sql string, args ...any) {
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed exec %q: %v", sql, err)
		}
	}
	do(`INSERT INTO sessions (id, organization_id, project_id) VALUES ($1,$2,$3)`, session, tenant.Organization, tenant.Project)
	do(`INSERT INTO responses (id, organization_id, project_id, session_id, state, input) VALUES ($1,$2,$3,$4,'queued',$5)`,
		response, tenant.Organization, tenant.Project, session, encodeJSONString(prompt))
	do(`INSERT INTO runs (id, organization_id, project_id, session_id, response_id, state) VALUES ($1,$2,$3,$4,$5,'queued')`,
		run, tenant.Organization, tenant.Project, session, response)
	return run, response
}

// runParityClient runs one client subprocess against the live server and returns its stdout. A non-zero exit is
// a journey failure (the client must retrieve + normalize the shared run). The credential is never on argv — the
// clients read PALAI_API_KEY from the environment.
func runParityClient(t *testing.T, root, dir string, extraEnv []string, name string, args ...string) json.RawMessage {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), extraEnv...)
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("parity client %q failed: %v\nstderr: %s", name+" "+strings.Join(args, " "), err, stderr)
	}
	return json.RawMessage(strings.TrimSpace(string(out)))
}

// canonicalizeJSON renders a raw JSON value in canonical form (sorted keys) — the T2 harness's canon(); ok is
// false for non-JSON.
func canonicalizeJSON(raw json.RawMessage) (string, bool) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", false
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// hashPartsLive reproduces tests/uat hashParts (sha256 of each part + NUL, hex, sha256:-prefixed) so the journey
// can build the same equality/config digests uat.*Proof.Complete() recompute.
func hashPartsLive(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// repoRootLive walks up to the main module root (the dir holding go.mod).
func repoRootLive(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}
