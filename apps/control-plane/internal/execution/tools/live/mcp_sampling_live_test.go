//go:build live

// CASE=mcp-sampling-budgeted (E12 Task 6, TOL-010): a sampling-ENABLED MCP connection to a REAL stdio server
// (the fixture, in a hardened, network-less OCI container) triggers a server sampling/createMessage during a
// tools/call, and the platform routes it as a SEPARATE budgeted model step through a REAL provider-one Route.
// A deliberately tiny budget makes the REAL completion exceed the reservation, so the sampling step is CUT
// OFF at Admit — asserted LIVE from the journalled step (a real provider request id + reason budget_exceeded).
//
// HONEST CEILING (mandatory, spec §10.2): the MCP server is OUR fixture (a third-party live server is the T10
// journey) — that is the honest ceiling here. What is REAL: the sampling step is a genuine provider-one Route
// (a real chatcmpl id), and the SEPARATE Reservation genuinely cuts it off (budget_exceeded), proving the
// TOL-010 budget is enforced against a live provider, not a fake. The denial is a JSON-RPC error to the server
// (the tools/call still completes) and NEVER trips the breaker. The credential reaches only the broker's Route
// call frame — never argv, a log, or the sampling event payload.

package live

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	mcpclient "github.com/palgroup/palai/adapters/integrations/mcp"
	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	"github.com/palgroup/palai/adapters/sandboxes/oci"
	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// TestLiveMCPSamplingBudgetedCutoff drives a real fixture MCP server's `sample` tool through a sampling-enabled
// connection; the server's sampling request is routed to a REAL provider under a 1-token budget and cut off.
func TestLiveMCPSamplingBudgetedCutoff(t *testing.T) {
	if os.Getenv(credentialEnv) == "" {
		t.Fatalf("%s is unset; the live tier loads it from .env.local at runtime", credentialEnv)
	}
	fixtureImage := os.Getenv("PALAI_MCP_FIXTURE_IMAGE_ID")
	if fixtureImage == "" {
		t.Skip("PALAI_MCP_FIXTURE_IMAGE_ID is required; run make test-live-provider PROVIDER=provider-one CASE=mcp-sampling-budgeted")
	}
	ctx := context.Background()

	driver, err := oci.NewDockerInteractiveDriver()
	if err != nil {
		t.Fatalf("docker interactive driver: %v", err)
	}

	// A REAL broker + route to provider-one (the same wiring the exec path uses); the credential is redeemed
	// only inside Route, from the environment, never on a request or in a log.
	broker := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"provider-one": providerone.Adapter{}},
		Secrets:  modelbroker.EnvResolver{"provider-one": credentialEnv},
	})
	route := execution.ModelRoute{Provider: "provider-one", Model: liveModel(), Secret: modelbroker.SecretRef("provider-one")}

	// Capture the sampling step's journalled events in memory (no Postgres needed for this smoke).
	var mu sync.Mutex
	var steps []map[string]any
	emit := func(_ context.Context, _ mcpclient.CallScope, eventType string, payload []byte) error {
		var p map[string]any
		_ = json.Unmarshal(payload, &p)
		p["_event"] = eventType
		mu.Lock()
		steps = append(steps, p)
		mu.Unlock()
		return nil
	}

	manager := mcpclient.NewManager(mcpclient.Config{
		Driver:         driver,
		Sampling:       execution.NewMCPSamplingRouter(broker, route, emit),
		DefaultTimeout: 60 * time.Second,
		Limits:         oci.Limits{WallTime: 60 * time.Second, MaxMemoryBytes: 256 << 20, MaxProcessCount: 32, NanoCPUs: 1_000_000_000},
	})

	// A sampling-ENABLED connection with a 1-token budget: any real completion exceeds it, forcing the cutoff.
	conn := mcpclient.ConnConfig{
		ID:                "mcpc_live_sampling",
		Name:              "fixture",
		Transport:         "stdio",
		ImageDigest:       fixtureImage,
		Cmd:               []string{"/mcp"},
		SamplingEnabled:   true,
		SamplingMaxTokens: 1,
		TimeoutMS:         60_000,
	}
	scope := mcpclient.CallScope{Org: liveID("org"), Project: liveID("prj"), SessionID: liveID("ses"), ResponseID: liveID("rsp"), RunID: liveID("run"), CallID: liveID("cal")}

	out, err := manager.Call(ctx, scope, conn, "sample", map[string]any{"message": "Say hello in one short sentence."})
	if err != nil {
		t.Fatalf("tools/call sample (a sampling denial must NOT fail the call / trip the breaker): %v", err)
	}
	// The tool completed despite the sampling denial, and reports the denial the client returned.
	if out["sampling_denied"] != true {
		t.Fatalf("sample tool result = %v, want sampling_denied:true (the 1-token budget cut the sampling off)", out)
	}

	// The journalled sampling step is the live evidence: a real provider request id proves a REAL provider
	// Route happened, and reason budget_exceeded proves the SEPARATE Reservation cut it off.
	mu.Lock()
	defer mu.Unlock()
	var completed map[string]any
	for _, s := range steps {
		if s["_event"] == "model_step.completed.v1" && s["source"] == "mcp_sampling" {
			completed = s
		}
	}
	if completed == nil {
		t.Fatalf("no completed mcp_sampling model step journalled; steps=%v", steps)
	}
	if completed["denied"] != true || completed["reason"] != "budget_exceeded" {
		t.Fatalf("sampling step = %v, want denied:true reason:budget_exceeded (live Admit cutoff)", completed)
	}
	providerReqID, _ := completed["provider_request_id"].(string)
	if providerReqID == "" {
		t.Fatalf("sampling step carries no provider_request_id; a REAL provider Route did not happen: %v", completed)
	}
	t.Logf("live mcp-sampling-budgeted PASS (REAL provider Route cut off by a 1-token budget; MCP server is our fixture): "+
		"provider_request=%s… reason=%s tokens=%v model=%s", safePrefix(providerReqID), completed["reason"], completed["total_tokens"], route.Model)
}
