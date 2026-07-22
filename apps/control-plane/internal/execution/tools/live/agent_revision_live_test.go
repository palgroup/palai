//go:build live

// CASE=agent-revision-run (E11 Task 1, AGT-001): a real provider-one run whose ExecutionSpec is
// resolved FROM a published AgentRevision — the revision pins the model, and its tool set is a
// capability CEILING. This confirms live what the deterministic tiers prove at the resolver: the
// pinned-revision config is what actually reaches the real provider request.
//
// HONEST CEILINGS (spec §10.2):
//  1. dispatchModel now advertises the effective tool set (E12 T1). The request built here from the
//     resolved ExecutionSpec carries ONLY the ceiling tool's schema — the SAME set dispatchModel would
//     advertise from this pinned revision (EXT-002's deterministic test proves the orchestrator path
//     assembles exactly this from the pinned revision's effective set). The web tool, outside the
//     ceiling, never reaches the request.
//  2. SPONTANEOUS: the call is NOT forced (no tool_choice:required) — the real provider, offered only
//     the file tool, elects to call it. Spontaneity is probabilistic: a run where the model declines
//     produces no tool call and the smoke re-runs; a green run is the proof. What is proven live:
//     model-pin + tool-ceiling reach the real provider request and a real spontaneous file-tool call
//     (with a genuine chatcmpl id) comes back.

package live

import (
	"context"
	"os"
	"testing"
	"time"

	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

const (
	fileToolName = "palai.workspace.file"
	webToolName  = "palai.web.search" // a tool the revision does NOT declare — outside the ceiling
)

// TestLiveAgentRevisionPinnedRun resolves a published AgentRevision's ExecutionSpec (pinned model +
// tool ceiling), builds the real provider request FROM that spec, and confirms the pinned config runs
// live: the request advertises only the ceiling tool, the model is the revision's pin, and the real
// provider SPONTANEOUSLY calls that tool (not forced), coming back with a genuine completion.
func TestLiveAgentRevisionPinnedRun(t *testing.T) {
	secret := os.Getenv(credentialEnv)
	if secret == "" {
		t.Fatalf("%s is unset; the live tier loads it from .env.local at runtime", credentialEnv)
	}

	// A published AgentRevision pins the model and declares a tool ceiling of just the file tool; the
	// project baseline ALSO offers a web tool the revision does not declare. Resolution is the exact
	// pure function the control plane runs (execution.Resolve — checkpoint.go/model_dispatch feed it).
	snap := execution.Resolve(execution.ResolveInput{
		DeploymentModel:    "deployment-default-not-used",
		ProjectTools:       []string{fileToolName, webToolName},
		AgentRevisionID:    "arev_live_ceiling",
		AgentRevisionModel: liveModel(),
		AgentRevisionTools: []string{fileToolName}, // the ceiling — excludes the web tool
	})
	if snap.Model != liveModel() {
		t.Fatalf("resolved model = %q, want the revision-pinned %q", snap.Model, liveModel())
	}
	if snap.Provenance["model"] != "agent_revision" || snap.Provenance["agent_revision"] != "arev_live_ceiling" {
		t.Fatalf("provenance = %v, want the model pinned by the agent revision", snap.Provenance)
	}
	for _, tool := range snap.Tools {
		if tool == webToolName {
			t.Fatalf("resolved tools = %v; the ceiling must exclude %q", snap.Tools, webToolName)
		}
	}

	// Build the REAL provider request from the resolved ExecutionSpec — the SAME advertised set
	// dispatchModel produces from this pinned revision. Only the ceiling tool's schema is advertised;
	// the web tool never reaches the request. The call is SPONTANEOUS (no ForceToolCall).
	req := modelbroker.Request{
		ModelRequestID: contracts.ModelRequestID("mreq_agent_revision_pin"),
		RouteRevision:  1, ModelStepID: "step-pin", Model: snap.Model,
		Messages:    []modelbroker.Message{{Role: "user", Content: "Use the file tool to write repo/hello.txt with the content OK."}},
		Tools:       schemasForCeiling(snap.Tools),
		Deadline:    time.Now().Add(60 * time.Second),
		Reservation: modelbroker.Reservation{MaxTotalTokens: 2000},
		Secret:      modelbroker.SecretRef("provider-one"),
	}
	for _, ts := range req.Tools {
		if ts.Name == webToolName {
			t.Fatalf("the provider request advertised %q, which is outside the revision ceiling", webToolName)
		}
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != fileToolName {
		t.Fatalf("advertised tools = %v, want only the ceiling tool %q", req.Tools, fileToolName)
	}

	broker := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"provider-one": providerone.Adapter{}},
		Secrets:  modelbroker.EnvResolver{"provider-one": credentialEnv},
	})
	res, err := broker.Route(context.Background(), "provider-one", req, func(modelbroker.Delta) {})
	if err != nil {
		t.Fatalf("route pinned-revision turn: %v", err)
	}
	assertRealCompletion(t, res, "agent-revision pinned run")
	call := requireToolCall(t, res, "agent-revision pinned run")
	if call.Name != fileToolName {
		t.Fatalf("spontaneous call = %q, want the ceiling tool %q", call.Name, fileToolName)
	}
}

// schemasForCeiling advertises a schema only for the file tool (the ceiling in this smoke). A tool
// outside the ceiling has no schema, so it can never reach the request — the request-construction
// ceiling this smoke asserts.
func schemasForCeiling(tools []string) []modelbroker.ToolSchema {
	out := make([]modelbroker.ToolSchema, 0, len(tools))
	for _, tool := range tools {
		if tool == fileToolName {
			out = append(out, fileToolSchema())
		}
	}
	return out
}
