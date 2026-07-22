//go:build e2e

package responses

// Tool_call-id threading end to end (E12 T1b): a real provider tool call carries an id; after the
// tool executes, the NEXT model step's conversation must thread that id through both the assistant
// tool-call turn and the following tool message, so the conversation is well-formed for the real
// OpenAI chat API (assistant.tool_calls[].id == the tool message's tool_call_id). This is the
// deterministic proof of the seam modelbroker.Request.Messages — the exact input wireMessages
// serializes to the wire — so the live restore/continuation cases can rely on it.

import (
	"context"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// TestDispatchModelThreadsToolCallIDToProvider drives a spontaneous-style tool round-trip with a
// provider tool call id (call_file) and asserts the id survives the whole engine boundary: the
// inbound drop (toEngineToolCalls) must carry it onto the assistant turn, and the engine must
// translate its synthetic tcall_ id back to it on the tool message. Both were broken before T1b.
func TestDispatchModelThreadsToolCallIDToProvider(t *testing.T) {
	h := newHarness(t)
	h.setProjectPolicy(`{"default_tools":["palai.workspace.file"]}`)

	prov := &advertisingProvider{toolCall: &modelbroker.ToolCall{
		ID: "call_file", Name: "palai.workspace.file",
		Arguments: `{"op":"write","path":"hello.txt","content":"threaded\n"}`,
	}}
	orch := h.newOrchestratorWithTools(subprocessDialer{engineDir: h.engineDir}, prov, tools.FileTool())

	respID, _, runID := h.admit()
	alloc := t.TempDir()
	if err := orch.ExecuteAttempt(context.Background(), h.workspaceDescriptor(runID, 1, alloc)); err != nil {
		t.Fatalf("execute tool-call-id attempt: %v", err)
	}
	if state, _ := h.response(respID); state != "completed" {
		t.Fatalf("response state = %q, want completed (multi-step tool round-trip)", state)
	}

	calls := prov.messagesSnapshot()
	if len(calls) != 2 {
		t.Fatalf("provider calls = %d, want 2 (tool step then final)", len(calls))
	}
	// The 2nd call is the continuation after the tool executed: its conversation must thread the id.
	second := calls[1]
	var assistant, toolMsg *modelbroker.Message
	for i := range second {
		if m := &second[i]; m.Role == "assistant" && len(m.ToolCalls) > 0 {
			assistant = m
		} else if m.Role == "tool" {
			toolMsg = m
		}
	}
	if assistant == nil {
		t.Fatalf("2nd call carries no assistant tool-call turn: %+v", second)
	}
	if assistant.ToolCalls[0].ID != "call_file" {
		t.Fatalf("assistant tool_call id = %q, want call_file (the inbound drop at toEngineToolCalls)", assistant.ToolCalls[0].ID)
	}
	if toolMsg == nil {
		t.Fatalf("2nd call carries no tool message: %+v", second)
	}
	if toolMsg.ToolCallID != "call_file" {
		t.Fatalf("tool message tool_call_id = %q, want call_file (the engine kept the synthetic tcall_ id)", toolMsg.ToolCallID)
	}
}
