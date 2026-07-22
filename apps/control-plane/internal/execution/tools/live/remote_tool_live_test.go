//go:build live

// CASE=remote-tool-async-roundtrip (E12 Task 4, TOL-016/017 signed half): a REGISTERED remote_http tool,
// pinned into a published set a run's AgentRevision names, is resolved through the broker's per-tenant
// lookup and executed as a SIGNED HTTP invoke — driven by a REAL provider-one tool call. The CP signs the
// tool-http.v1 invoke (HMAC over the raw body), a REAL local harness server VERIFIES the signature
// (server-side, USING the T9 Extension SDK — extsdk.Verify), answers 202, and POSTs a REAL signed callback
// to the REAL
// callback endpoint, which one-use-consumes the token and completes the durable operation under the live
// fence — the invoke -> 202 -> signed-callback -> completed round-trip proven end-to-end with a real
// model driving it and real HMAC on both directions.
//
// HONEST CEILINGS (mandatory, spec §10.2; brief §6):
//  1. FORCED, NOT SPONTANEOUS (pre-T1 seam): T1's advertising of REGISTRY tools to the real provider is
//     not exercised here, so the call is FORCED (tool_choice:required), the E09 T4 broker-seam pattern —
//     the same ceiling CASE=registry-tool-roundtrip carries. The advertised-effective-set half is proven
//     deterministically (TestRegistryToolsLoadIntoBrokerEffectiveSet). Re-run for the spontaneous half
//     once T1 advertises registry tools.
//  2. LEDGER-COMMIT-UNDER-FENCE + MODEL CONTINUATION ARE GATED ON T1b: this smoke drives the SIGNED
//     ROUND-TRIP through the broker (tb.Execute), proving invoke -> 202 -> callback -> completed with real
//     HMAC. The dispatchTool ledger commit under a live attempt fence AND the model CONTINUING on the
//     callback result are proven DETERMINISTICALLY in the component tier (TestAsync202CallbackAcceptedOnce
//     UnderFence, TestLateCallbackAfterDeadlineEntersReconciliationNotSilentCommit, the ledger fill test).
//     The multi-step continuation needs the engine-wire tool_call id (T1b), which is NOT merged; when it
//     lands, this case upgrades to assert the model continues on the callback result (a one-line assert).
//  3. THE REMOTE SERVER IS OUR HARNESS, not a real customer service — the proof is the signature + one-use
//     token + operation round-trip, with the real provider driving the tool CHOICE.
//
// The HMAC signing secret is an in-process []byte, never logged; the provider credential is an opaque
// env-resolved needle for the leak scan and is never printed.

package live

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	remotehttp "github.com/palgroup/palai/adapters/tools/http"
	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/internal/extensions"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	extsdk "github.com/palgroup/palai/packages/extension-sdk"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"

	"github.com/palgroup/palai/storage"
)

const remoteToolShortName = "lookup"

// TestLiveRemoteToolAsyncRoundtrip is CASE=remote-tool-async-roundtrip (see the file ceilings).
func TestLiveRemoteToolAsyncRoundtrip(t *testing.T) {
	if os.Getenv(credentialEnv) == "" {
		t.Fatalf("%s is unset; the live tier loads it from .env.local at runtime", credentialEnv)
	}
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Fatalf("PALAI_COMPONENT_POSTGRES_URL is unset; the CASE runs under run_live_with_postgres")
	}
	ctx := context.Background()

	cs, err := coordinator.Open(ctx, url)
	if err != nil {
		t.Fatalf("coordinator.Open: %v", err)
	}
	t.Cleanup(cs.Close)
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool := cs.Pool()

	org, project := liveID("org"), liveID("prj")
	sessionID, runID := liveID("ses"), liveID("run")
	profileID, arevID := liveID("aprof"), liveID("arev")
	execLive(t, pool, `INSERT INTO organizations (id) VALUES ($1)`, org)
	execLive(t, pool, `INSERT INTO projects (id, organization_id) VALUES ($1,$2)`, project, org)
	execLive(t, pool, `INSERT INTO sessions (id, organization_id, project_id) VALUES ($1,$2,$3)`, sessionID, org, project)
	execLive(t, pool, `INSERT INTO agent_profiles (id, organization_id, project_id, name) VALUES ($1,$2,$3,'reviewer')`, profileID, org, project)

	// The shared HMAC secret (in-process only; never logged). The org-scoped resolver hands it to BOTH the
	// outbound invoke signer and the inbound callback verifier — the same secret, both directions.
	secret := []byte("live-remote-tool-hmac-secret")
	ops := remotehttp.NewOperations(pool)
	resolver := func(o, ref string) ([]byte, error) {
		if o == org && ref == "sig-ref" {
			return secret, nil
		}
		return nil, nil
	}

	// The REAL callback endpoint over the real operation ledger (the production handler).
	muxA := http.NewServeMux()
	muxA.Handle("POST /v1/tool-callbacks/{operation_id}", api.NewToolCallbackHandler(ops, resolver))
	callbackServer := httptest.NewServer(muxA)
	defer callbackServer.Close()

	// The REAL remote tool harness: it VERIFIES the invoke HMAC server-side with the T9 Extension SDK
	// (extsdk.Verify — the customer SDK's job, now dogfooded), answers 202, and POSTs a REAL signed
	// callback back to the callback endpoint SIGNED with the SDK (extsdk.Callback + extsdk.CallbackHeaders).
	var execCount int32
	toolServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		id := r.Header.Get(extsdk.HeaderID)
		unix, err := strconv.ParseInt(r.Header.Get(extsdk.HeaderTimestamp), 10, 64)
		if err != nil || !extsdk.Verify(secret, id, time.Unix(unix, 0), raw, r.Header.Get(extsdk.HeaderSignature), time.Now(), 5*time.Minute) {
			w.WriteHeader(http.StatusUnauthorized) // a bad/unsigned invoke never executes
			return
		}
		atomic.AddInt32(&execCount, 1)
		var inv struct {
			ToolCallID string `json:"tool_call_id"`
			Callback   struct {
				URL   string `json:"url"`
				Token string `json:"token"`
			} `json:"callback"`
		}
		_ = json.Unmarshal(raw, &inv)
		w.WriteHeader(http.StatusAccepted) // 202: the result comes later via the signed callback
		go postSignedCallbackFromHarness(secret, inv.ToolCallID, inv.Callback.URL, inv.Callback.Token)
	}))
	defer toolServer.Close()

	// Register the remote_http tool: executor_config carries only the harness URL + self-host flag; the
	// credential is a secret_ref handle. Pin it into a published set the run's agent revision names.
	reg := extensions.New(pool)
	tool, err := reg.CreateTool(ctx, org, project, "acme.remote."+remoteToolShortName)
	if err != nil {
		t.Fatalf("create tool: %v", err)
	}
	body := `{"executor":"remote_http","input_schema":{"type":"object"},"output_schema":{"type":"object"},"replay_class":"idempotent","executor_config":{"url":"` + toolServer.URL + `","allow_private":true},"secret_ref":"sig-ref","timeout_ms":15000}`
	rev, err := reg.CreateToolRevision(ctx, org, project, tool.ID, []byte(body))
	if err != nil {
		t.Fatalf("create remote_http revision: %v", err)
	}
	if _, _, err := reg.PublishToolRevision(ctx, org, project, rev.ID); err != nil {
		t.Fatalf("publish revision: %v", err)
	}
	set, err := reg.CreateToolSetRevision(ctx, org, project, "reviewers", []byte(`{"tools":[{"tool_revision_id":"`+rev.ID+`"}]}`))
	if err != nil {
		t.Fatalf("create set: %v", err)
	}
	if _, _, err := reg.PublishToolSetRevision(ctx, org, project, set.ID); err != nil {
		t.Fatalf("publish set: %v", err)
	}
	execLive(t, pool, `INSERT INTO agent_revisions (id, organization_id, project_id, profile_id, revision_number, model, published_at, tool_sets)
	                   VALUES ($1,$2,$3,$4,1,$5,clock_timestamp(),$6::jsonb)`, arevID, org, project, profileID, liveModel(), `["`+set.ID+`"]`)
	execLive(t, pool, `INSERT INTO runs (id, organization_id, project_id, session_id, agent_revision_id) VALUES ($1,$2,$3,$4,$5)`, runID, org, project, sessionID, arevID)

	// A REAL provider forced tool call for the registered remote tool (the live element — the model CHOOSES
	// the tool; forcing is the pre-T1 seam, ceiling 1).
	req := modelbroker.Request{
		ModelRequestID: contracts.ModelRequestID("mreq_remote_tool_roundtrip"),
		RouteRevision:  1, ModelStepID: "step-remote", Model: liveModel(),
		Messages:      []modelbroker.Message{{Role: "user", Content: "Call the lookup tool with query \"weather\"."}},
		Tools:         []modelbroker.ToolSchema{remoteToolSchema()},
		ForceToolCall: true,
		Deadline:      time.Now().Add(60 * time.Second),
		Reservation:   modelbroker.Reservation{MaxTotalTokens: 2000},
		Secret:        modelbroker.SecretRef("provider-one"),
	}
	mb := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"provider-one": providerone.Adapter{}},
		Secrets:  modelbroker.EnvResolver{"provider-one": credentialEnv},
	})
	res, err := mb.Route(ctx, "provider-one", req, func(modelbroker.Delta) {})
	if err != nil {
		t.Fatalf("route remote-tool turn: %v", err)
	}
	assertRealCompletion(t, res, "remote-tool-async-roundtrip (forced, pre-T1)")
	call := requireToolCall(t, res, "remote-tool-async-roundtrip (forced, pre-T1)")
	if call.Name != remoteToolShortName {
		t.Fatalf("forced call = %q, want the registered remote tool %q", call.Name, remoteToolShortName)
	}

	// Execute the forced call through the broker's per-tenant lookup + the wired remote executor: the CP
	// signs the invoke, the harness verifies + 202s, its signed callback completes the operation, and the
	// executor returns the result — the signed async round-trip end to end.
	executor := remotehttp.NewExecutor(ops, remotehttp.WithCallbackBaseURL(callbackServer.URL))
	reg.SetRemoteInvoker(executor, resolver)
	tb := toolbroker.New()
	tb.SetLookup(func(ctx context.Context, env toolbroker.ExecEnv, name string) (toolbroker.Tool, bool, error) {
		return reg.LookupTool(ctx, env.Scope.Org, env.Scope.Project, env.Scope.RunID, name)
	})
	callID := call.ID
	if callID == "" {
		callID = "tc_live_remote"
	}
	env := toolbroker.ExecEnv{Scope: toolbroker.TaskScope{Org: org, Project: project, RunID: runID}}
	out, err := tb.Execute(ctx, contracts.ToolCallID(callID), remoteToolShortName, decodeArgs(t, call.Arguments), 1, env)
	if err != nil {
		t.Fatalf("execute remote tool signed round-trip: %v", err)
	}
	if out.State != "completed" {
		t.Fatalf("remote tool state = %q, want completed (signed invoke -> 202 -> callback -> completed)", out.State)
	}
	if n := atomic.LoadInt32(&execCount); n != 1 {
		t.Fatalf("harness verified+executed invokes = %d, want exactly 1 (real HMAC round-trip)", n)
	}
	// The durable operation completed via the one-use signed callback (not a silent commit).
	var opState string
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT state FROM remote_tool_operations WHERE tool_call_id=$1`, callID).Scan(&opState); err != nil {
		t.Fatalf("read operation error = %v", err)
	}
	if opState != "completed" {
		t.Fatalf("operation state = %q, want completed", opState)
	}
	t.Logf("remote-tool-async-roundtrip PASS: real model chose the remote tool; the CP signed the invoke, the harness verified the HMAC + answered 202, its signed callback one-use-completed the operation, and the executor returned the result. Ledger-commit-under-fence + model continuation are proven deterministically (component tier) and upgrade live once T1b (engine-wire tool_call id) merges.")
}

// postSignedCallbackFromHarness is the harness side of the async round-trip: it signs a result callback
// (id = the operation id, the callback URL's last path segment — the CP's verify id) and POSTs it with the
// one-use token. The signing secret never leaves the process.
func postSignedCallbackFromHarness(secret []byte, toolCallID, callbackURL, token string) {
	if callbackURL == "" {
		return
	}
	operationID := path.Base(callbackURL)
	raw, err := extsdk.Callback(operationID, toolCallID, map[string]any{"answer": "sunny"})
	if err != nil {
		return
	}
	headers := extsdk.CallbackHeaders(operationID, time.Now(), raw, secret)
	req, err := http.NewRequest(http.MethodPost, callbackURL, bytes.NewReader(raw))
	if err != nil {
		return
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set(extsdk.HeaderCallbackToken, token)
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

// remoteToolSchema advertises the registered remote tool's short name so the forced provider call names it.
func remoteToolSchema() modelbroker.ToolSchema {
	return modelbroker.ToolSchema{
		Name:        remoteToolShortName,
		Description: "Look up an answer via a remote service. Registered remote_http tool.",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"query": map[string]any{"type": "string"}},
			"required":   []any{"query"},
		},
	}
}
