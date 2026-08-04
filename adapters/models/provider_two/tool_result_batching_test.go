package providertwo_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	providertwo "github.com/palgroup/palai/adapters/models/provider_two"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// captureMessages runs one request against a stub and returns the `messages` array the adapter built.
func captureMessages(t *testing.T, messages []modelbroker.Message) []map[string]any {
	t.Helper()
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, cannedStream)
	}))
	defer srv.Close()

	req := toolSchemaRequest()
	req.ForceToolCall = false
	req.Tools = nil
	req.Messages = messages
	if _, err := (providertwo.Adapter{BaseURL: srv.URL}).Execute(context.Background(), req, sentinelSecret, nil); err != nil {
		t.Fatalf("execute: %v", err)
	}
	raw, ok := captured["messages"].([]any)
	if !ok {
		t.Fatalf("the request body carried no messages array: %v", captured)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, entry := range raw {
		if m, ok := entry.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// TestATurnsToolResultsRideOneUserMessage is a PROVIDER CONTRACT, not a preference. Anthropic's
// parallel-tool-use documentation requires every tool_result for a turn to come back in a single user
// message, and states the consequence of splitting them: it "silently trains Claude to stop making
// parallel calls".
//
// SO THIS IS THE FIRST HALF OF PARALLELISM, AND IT COMES BEFORE ANY CONCURRENCY WORK. A control plane
// that executed three tools at once would gain nothing while the model was being taught, one turn at a
// time, never to ask for three. Splitting was what this adapter did: one user message per tool result.
func TestATurnsToolResultsRideOneUserMessage(t *testing.T) {
	messages := captureMessages(t, []modelbroker.Message{
		{Role: "user", Content: "find the handler"},
		{Role: "assistant", ToolCalls: []modelbroker.ToolCall{
			{ID: "tc_1", Name: "palai.workspace.glob", Arguments: `{"pattern":"**/*.go"}`},
			{ID: "tc_2", Name: "palai.workspace.grep", Arguments: `{"pattern":"Handler"}`},
			{ID: "tc_3", Name: "palai.knowledge.retrieve", Arguments: `{"q":"handler"}`},
		}},
		{Role: "tool", ToolCallID: "tc_1", Content: "a.go\nb.go"},
		{Role: "tool", ToolCallID: "tc_2", Content: "a.go:12: func Handler"},
		{Role: "tool", ToolCallID: "tc_3", Content: "nothing relevant"},
	})

	var userTurns []map[string]any
	for _, m := range messages {
		if m["role"] == "user" {
			userTurns = append(userTurns, m)
		}
	}
	// The opening question is one user turn; the three results must be a SECOND one, not three more.
	if len(userTurns) != 2 {
		t.Fatalf("got %d user messages, want 2 (the question, then one carrying all three results)", len(userTurns))
	}

	blocks, _ := userTurns[1]["content"].([]any)
	if len(blocks) != 3 {
		t.Fatalf("the results turn carries %d blocks, want the three tool_results", len(blocks))
	}
	seen := map[string]bool{}
	for _, entry := range blocks {
		block, _ := entry.(map[string]any)
		if block["type"] != "tool_result" {
			t.Errorf("block type = %v, want tool_result", block["type"])
		}
		id, _ := block["tool_use_id"].(string)
		seen[id] = true
	}
	for _, want := range []string{"tc_1", "tc_2", "tc_3"} {
		if !seen[want] {
			t.Errorf("tool_use_id %q is missing from the batched turn", want)
		}
	}
}

// TestToolResultsKeepTheirOrder — the blocks must arrive in the order the model called them, so a
// model correlating by position rather than by id reads the right pairing.
func TestToolResultsKeepTheirOrder(t *testing.T) {
	messages := captureMessages(t, []modelbroker.Message{
		{Role: "user", Content: "go"},
		{Role: "tool", ToolCallID: "tc_1", Content: "first"},
		{Role: "tool", ToolCallID: "tc_2", Content: "second"},
	})
	for _, m := range messages {
		if m["role"] != "user" {
			continue
		}
		blocks, _ := m["content"].([]any)
		if len(blocks) != 2 {
			continue
		}
		first, _ := blocks[0].(map[string]any)
		second, _ := blocks[1].(map[string]any)
		if first["tool_use_id"] != "tc_1" || second["tool_use_id"] != "tc_2" {
			t.Errorf("order = %v then %v, want tc_1 then tc_2", first["tool_use_id"], second["tool_use_id"])
		}
		return
	}
	t.Fatal("no user turn carried two tool_result blocks")
}

// TestASingleToolResultIsUnchanged — the batching must not perturb the ordinary one-call turn, which
// is every turn this tree has produced so far.
func TestASingleToolResultIsUnchanged(t *testing.T) {
	messages := captureMessages(t, []modelbroker.Message{
		{Role: "user", Content: "go"},
		{Role: "tool", ToolCallID: "tc_1", Content: "only"},
	})
	for _, m := range messages {
		blocks, _ := m["content"].([]any)
		if len(blocks) != 1 {
			continue
		}
		block, _ := blocks[0].(map[string]any)
		if block["type"] == "tool_result" && block["tool_use_id"] == "tc_1" && block["content"] == "only" {
			return
		}
	}
	t.Fatal("a lone tool result did not survive as its own user turn")
}

// TestAnInterveningTurnBreaksTheBatch — only ADJACENT results belong together. A user turn between
// two tool results means they answered different model turns, and merging them would rewrite the
// conversation's shape.
func TestAnInterveningTurnBreaksTheBatch(t *testing.T) {
	messages := captureMessages(t, []modelbroker.Message{
		{Role: "tool", ToolCallID: "tc_1", Content: "first"},
		{Role: "user", Content: "actually, also check this"},
		{Role: "tool", ToolCallID: "tc_2", Content: "second"},
	})
	batched := 0
	for _, m := range messages {
		if blocks, _ := m["content"].([]any); len(blocks) > 1 {
			batched++
		}
	}
	if batched != 0 {
		t.Errorf("%d turns were batched across an intervening user message", batched)
	}
}
