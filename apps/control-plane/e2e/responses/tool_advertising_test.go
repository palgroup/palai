//go:build e2e

package responses

// Tool-schema advertising end to end (E12 T1): dispatchModel resolves the run's effective tool set
// from the SAME three config layers a checkpoint hashes (session override, project baseline, pinned
// revision) and offers each registered name to the provider as a ToolSchema. A real provider tool
// call then rides the existing engine multi-step loop to a terminal run. These prove: the advertised
// set is exactly the registered effective schemas (unregistered names dropped), a revision ceiling
// keeps an out-of-set tool off the wire, an unconfigured run advertises nothing (bit-unchanged), and
// production advertises without ever forcing a call.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// advertisingProvider records the tool schemas (and the force flag) of every model call, so a test
// reads exactly what dispatchModel offered. When toolCall is set the first step (no tool result in
// the conversation yet) fabricates it and the next step finishes; when nil the run is single-step.
type advertisingProvider struct {
	mu       sync.Mutex
	tools    [][]modelbroker.ToolSchema
	force    []bool
	toolCall *modelbroker.ToolCall
}

func (p *advertisingProvider) Execute(_ context.Context, req modelbroker.Request, _ string, _ func(modelbroker.Delta)) (modelbroker.Result, error) {
	p.mu.Lock()
	p.tools = append(p.tools, append([]modelbroker.ToolSchema(nil), req.Tools...))
	p.force = append(p.force, req.ForceToolCall)
	p.mu.Unlock()

	sawTool := false
	for _, m := range req.Messages {
		if m.Role == "tool" {
			sawTool = true
		}
	}
	res := modelbroker.Result{
		ModelRequestID: req.ModelRequestID, Model: "fake",
		Usage: contracts.Usage{InputTokens: 5, OutputTokens: 3, TotalTokens: 8}, Attempts: 1,
	}
	if p.toolCall != nil && !sawTool {
		res.ProviderRequestID = "prov_tool"
		res.ToolCalls = []modelbroker.ToolCall{*p.toolCall}
		res.FinishReason = "tool_calls"
		return res, nil
	}
	res.ProviderRequestID = "prov_final"
	res.Output = "done"
	res.FinishReason = "stop"
	return res, nil
}

func (p *advertisingProvider) snapshot() ([][]modelbroker.ToolSchema, []bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tools, p.force
}

// TestDispatchModelAdvertisesEffectiveToolSchemas is the key proof: a project baseline of three tool
// names (two registered, one not) advertises exactly the two registered schemas — name, platform
// description, and JSON-schema parameters — on every model step, and a real provider tool call to one
// of them rides the multi-step loop to a completed run with a real workspace write and a fenced
// tool_calls ledger row. The unregistered name is never offered.
func TestDispatchModelAdvertisesEffectiveToolSchemas(t *testing.T) {
	h := newHarness(t)
	h.setProjectPolicy(`{"default_tools":["palai.workspace.file","palai.workspace.shell","not.registered.tool"]}`)

	prov := &advertisingProvider{toolCall: &modelbroker.ToolCall{
		ID: "call_file", Name: "palai.workspace.file",
		Arguments: `{"op":"write","path":"hello.txt","content":"advertised\n"}`,
	}}
	orch := h.newOrchestratorWithTools(subprocessDialer{engineDir: h.engineDir}, prov, tools.FileTool(), tools.ShellTool())

	respID, _, runID := h.admit()
	alloc := t.TempDir()
	if err := orch.ExecuteAttempt(context.Background(), h.workspaceDescriptor(runID, 1, alloc)); err != nil {
		t.Fatalf("execute advertising attempt: %v", err)
	}

	if state, _ := h.response(respID); state != "completed" {
		t.Fatalf("response state = %q, want completed (multi-step tool round-trip)", state)
	}

	advertised, _ := prov.snapshot()
	if len(advertised) != 2 {
		t.Fatalf("provider calls = %d, want 2 (tool step then final)", len(advertised))
	}
	wantDesc := map[string]string{
		"palai.workspace.file":  tools.FileTool().Description,
		"palai.workspace.shell": tools.ShellTool().Description,
	}
	wantParams := map[string]map[string]any{
		"palai.workspace.file":  tools.FileTool().InputSchema,
		"palai.workspace.shell": tools.ShellTool().InputSchema,
	}
	for i, call := range advertised {
		if len(call) != 2 {
			t.Fatalf("call %d advertised %d tools, want 2 (file, shell; not.registered.tool dropped): %+v", i, len(call), call)
		}
		// Effective order is preserved: file before shell, the project-baseline order minus the
		// unregistered name.
		if call[0].Name != "palai.workspace.file" || call[1].Name != "palai.workspace.shell" {
			t.Fatalf("call %d advertised order = [%s, %s], want [file, shell]", i, call[0].Name, call[1].Name)
		}
		for _, s := range call {
			if s.Description != wantDesc[s.Name] {
				t.Fatalf("call %d tool %s description = %q, want the built-in platform text", i, s.Name, s.Description)
			}
			if !reflect.DeepEqual(s.Parameters, wantParams[s.Name]) {
				t.Fatalf("call %d tool %s parameters = %v, want the broker InputSchema", i, s.Name, s.Parameters)
			}
		}
	}

	if _, err := os.Stat(filepath.Join(alloc, "hello.txt")); err != nil {
		t.Fatalf("file tool did not write hello.txt: %v", err)
	}
	if n := h.count(`SELECT count(*) FROM tool_calls WHERE run_id=$1 AND name='palai.workspace.file' AND state='completed'`, runID); n != 1 {
		t.Fatalf("completed palai.workspace.file tool_calls rows = %d, want 1", n)
	}
}

// TestToolOutsideEffectiveSetNeverAdvertised (EXT-002) proves the revision capability ceiling gates
// advertising: the project baseline carries both file and shell, but a pinned revision whose ceiling
// is only file drops shell from the effective set, so shell is NEVER offered on the wire.
func TestToolOutsideEffectiveSetNeverAdvertised(t *testing.T) {
	h := newHarness(t)
	h.setProjectPolicy(`{"default_tools":["palai.workspace.file","palai.workspace.shell"]}`)

	_, profile := h.postAgent("/v1/agents", `{"name":"restricted"}`)
	profileID, _ := profile["id"].(string)
	if profileID == "" {
		t.Fatalf("create profile returned no id: %v", profile)
	}
	// The ceiling uses the CANONICAL broker name — T1 seeds canonical; short-name normalization is T2.
	_, rev := h.postAgent("/v1/agents/"+profileID+"/revisions", `{"model":"fake","tools":["palai.workspace.file"],"instructions":"only file"}`)
	revID, _ := rev["id"].(string)
	if revID == "" {
		t.Fatalf("create revision returned no id: %v", rev)
	}
	if st, _ := h.postAgent("/v1/agents/"+profileID+"/revisions/"+revID+"/publish", ``); st != http.StatusOK {
		t.Fatalf("publish revision status = %d, want 200", st)
	}

	prov := &advertisingProvider{} // single-step; advertising happens whether or not a tool is called
	orch := h.newOrchestratorWithTools(subprocessDialer{engineDir: h.engineDir}, prov, tools.FileTool(), tools.ShellTool())

	resp := h.postResponse(`{"input":"go","agent_revision_id":"`+revID+`"}`, newID("idem"), h.token)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("pin published revision status = %d, want 202", resp.StatusCode)
	}
	var r contracts.Response
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("decode pinned response: %v", err)
	}
	if err := orch.ExecuteAttempt(context.Background(), h.descriptor(string(r.RunID), 1)); err != nil {
		t.Fatalf("execute pinned attempt: %v", err)
	}

	if state, _ := h.response(string(r.ID)); state != "completed" {
		t.Fatalf("response state = %q, want completed", state)
	}
	advertised, _ := prov.snapshot()
	if len(advertised) == 0 {
		t.Fatal("provider was never called")
	}
	for i, call := range advertised {
		if len(call) != 1 || call[0].Name != "palai.workspace.file" {
			t.Fatalf("call %d advertised %+v, want only [palai.workspace.file] — shell is outside the revision ceiling", i, call)
		}
	}
}

// TestAdvertisingPreservesDeterministicFakeRuns is the regression anchor: a run that configures no
// tools (no project policy) advertises nothing, so req.Tools stays nil and the provider request is
// bit-for-bit the pre-advertising one, while the deterministic tool round-trip still completes.
func TestAdvertisingPreservesDeterministicFakeRuns(t *testing.T) {
	h := newHarness(t)
	// No setProjectPolicy → the effective tool set is empty.
	prov := &advertisingProvider{toolCall: &modelbroker.ToolCall{
		ID: "call_add", Name: "palai.conformance.math.add", Arguments: `{"a":7,"b":5}`,
	}}
	// The default set registers only the conformance math add tool the script calls; no project policy
	// means the effective set is empty, so nothing is advertised.
	orch := h.newOrchestratorWithTools(subprocessDialer{engineDir: h.engineDir}, prov)

	respID, _, runID := h.admit()
	if err := orch.ExecuteAttempt(context.Background(), h.descriptor(runID, 1)); err != nil {
		t.Fatalf("execute unconfigured attempt: %v", err)
	}

	if state, _ := h.response(respID); state != "completed" {
		t.Fatalf("response state = %q, want completed", state)
	}
	advertised, _ := prov.snapshot()
	if len(advertised) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(advertised))
	}
	for i, call := range advertised {
		if call != nil {
			t.Fatalf("call %d advertised %+v, want nil (an unconfigured run offers no tools)", i, call)
		}
	}
}

// TestForceToolCallStillWorksWithAdvertising proves production advertises but never forces: a run
// with a non-empty effective set offers tools yet leaves ForceToolCall false on every call. The
// forced (tool_choice:required) pattern stays a test-only seam (conformance provider_one, tools/live).
func TestForceToolCallStillWorksWithAdvertising(t *testing.T) {
	h := newHarness(t)
	h.setProjectPolicy(`{"default_tools":["palai.workspace.file"]}`)

	prov := &advertisingProvider{} // single-step; the point is the advertised set + the force flag
	orch := h.newOrchestratorWithTools(subprocessDialer{engineDir: h.engineDir}, prov, tools.FileTool())

	respID, _, runID := h.admit()
	if err := orch.ExecuteAttempt(context.Background(), h.descriptor(runID, 1)); err != nil {
		t.Fatalf("execute attempt: %v", err)
	}
	if state, _ := h.response(respID); state != "completed" {
		t.Fatalf("response state = %q, want completed", state)
	}

	advertised, force := prov.snapshot()
	if len(advertised) == 0 || len(advertised[0]) == 0 {
		t.Fatal("expected a non-empty advertised tool set")
	}
	for i, f := range force {
		if f {
			t.Fatalf("call %d set ForceToolCall=true; production advertises but never forces", i)
		}
	}
}
