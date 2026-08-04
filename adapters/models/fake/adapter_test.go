package fake

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// TestAdapterDedupsByIdempotencyKey proves the idempotent fake settles exactly one effect
// across two calls carrying the same key — the local, no-spend proof that a reclaimed,
// re-routed request does not double-charge the provider (spec §53.4, §35.3).
func TestAdapterDedupsByIdempotencyKey(t *testing.T) {
	ledger := NewIdempotencyLedger()
	adapter := Adapter{
		Script:      Script{ProviderRequestID: "prov_1", Model: "fake", Output: "hi", Usage: contracts.Usage{InputTokens: 5, OutputTokens: 3, TotalTokens: 8}},
		Idempotency: ledger,
	}
	req := modelbroker.Request{ModelRequestID: "mreq_dedup1", IdempotencyKey: "run_dedup1/mreq_dedup1"}

	first, err := adapter.Execute(context.Background(), req, "secret", nil)
	if err != nil {
		t.Fatalf("first Execute error = %v", err)
	}
	second, err := adapter.Execute(context.Background(), req, "secret", nil)
	if err != nil {
		t.Fatalf("second Execute error = %v", err)
	}

	if ledger.Effects() != 1 {
		t.Fatalf("provider effects = %d, want 1 (the repeated key must not re-run)", ledger.Effects())
	}
	if keys := ledger.Keys(); len(keys) != 2 || keys[0] != req.IdempotencyKey || keys[1] != req.IdempotencyKey {
		t.Fatalf("recorded keys = %v, want the same key twice", keys)
	}
	if first.Output != second.Output || second.Output != "hi" {
		t.Fatalf("replayed result = %q, want the stored %q", second.Output, first.Output)
	}
}

// TestAdapterFaultsOnUnadvertisedToolCall proves the fake honors advertising parity (plan §109):
// when the request advertises a tool set, a scripted tool call to a name outside it is a provider
// fault — the fake never fabricates a call to a tool it was not offered. With no advertised tools
// the check is inert and the script replays unchanged (bit-for-bit the pre-advertising behavior).
func TestAdapterFaultsOnUnadvertisedToolCall(t *testing.T) {
	adapter := Adapter{Script: Script{
		ProviderRequestID: "prov_1", Model: "fake",
		ToolCalls: []modelbroker.ToolCall{{ID: "c1", Name: "palai.workspace.shell", Arguments: "{}"}},
	}}

	// Advertised set offers only file; a scripted shell call is outside it → provider fault.
	offered := modelbroker.Request{ModelRequestID: "mreq_adv1", Tools: []modelbroker.ToolSchema{{Name: "palai.workspace.file"}}}
	if _, err := adapter.Execute(context.Background(), offered, "secret", nil); err == nil {
		t.Fatal("advertised only file but scripted a shell call; want a provider fault, got nil")
	}

	// The SAME script with no advertised tools → the check is inert, the script replays unchanged.
	unadvertised := modelbroker.Request{ModelRequestID: "mreq_adv2"}
	if _, err := adapter.Execute(context.Background(), unadvertised, "secret", nil); err != nil {
		t.Fatalf("no advertised tools should replay the script unchanged, got %v", err)
	}

	// A call to the advertised tool passes.
	adapter.Script.ToolCalls[0].Name = "palai.workspace.file"
	if _, err := adapter.Execute(context.Background(), offered, "secret", nil); err != nil {
		t.Fatalf("calling the advertised tool should pass, got %v", err)
	}
}

// A SCRIPT WITH NO FOLLOW-UP TURNS IS THE ONE THIS TREE ALREADY HAD, and every call replays it
// whatever the conversation carries. Written down because turnFor now runs on every Execute: the
// single-turn script is not merely expected to be unchanged, it is asserted to be — including on a
// request whose messages hold tool results, which is where a turn selector would move if it moved.
func TestASingleTurnScriptIsUnaffectedByTheConversation(t *testing.T) {
	adapter := Adapter{Script: Script{ProviderRequestID: "fake-local", Model: "fake", Output: "ok"}}
	conversations := map[string][]modelbroker.Message{
		"empty":        nil,
		"user only":    {{Role: "user", Content: "hello"}},
		"one result":   {{Role: "user"}, {Role: "assistant"}, {Role: "tool", ToolCallID: "c1", Content: "Darwin"}},
		"four results": {{Role: "tool"}, {Role: "tool"}, {Role: "user"}, {Role: "tool"}, {Role: "tool"}},
	}
	for name, msgs := range conversations {
		res, err := adapter.Execute(context.Background(), modelbroker.Request{ModelRequestID: "mreq_1", Messages: msgs}, "secret", nil)
		if err != nil {
			t.Fatalf("%s: Execute error = %v", name, err)
		}
		if res.Output != "ok" || res.ProviderRequestID != "fake-local" || len(res.ToolCalls) != 0 || res.FinishReason != "stop" {
			t.Fatalf("%s: replayed output=%q provider_request_id=%q tool_calls=%d finish=%q, want the one scripted turn",
				name, res.Output, res.ProviderRequestID, len(res.ToolCalls), res.FinishReason)
		}
	}
}

// THE MULTI-STEP REPLAY, and the reason the seam exists: turn one asks for a tool, turn two answers
// with what it learned. The turn is chosen by the CONVERSATION — one turn per contiguous group of
// tool results — so this test drives it the way the engine does, by appending the results of the
// previous turn and calling again.
func TestAScriptWithFollowUpTurnsAdvancesOnToolResults(t *testing.T) {
	adapter := Adapter{Script: Script{
		ProviderRequestID: "scripted_1", Model: "fake",
		ToolCalls: []modelbroker.ToolCall{{ID: "c1", Name: "palai.workspace.shell", Arguments: `{"command":"uname"}`}},
		Then: []Script{
			{ProviderRequestID: "scripted_2", Model: "fake", Output: "Darwin"},
		},
	}}

	first, err := adapter.Execute(context.Background(), modelbroker.Request{
		ModelRequestID: "mreq_1",
		Messages:       []modelbroker.Message{{Role: "user", Content: "which kernel?"}},
	}, "secret", nil)
	if err != nil {
		t.Fatalf("first turn error = %v", err)
	}
	if len(first.ToolCalls) != 1 || first.ToolCalls[0].Name != "palai.workspace.shell" || first.FinishReason != "tool_calls" {
		t.Fatalf("first turn = %+v finish=%q, want the scripted shell call", first.ToolCalls, first.FinishReason)
	}
	if first.Output != "" || first.ProviderRequestID != "scripted_1" {
		t.Fatalf("first turn output=%q id=%q, want the first turn's own", first.Output, first.ProviderRequestID)
	}

	second, err := adapter.Execute(context.Background(), modelbroker.Request{
		ModelRequestID: "mreq_2",
		Messages: []modelbroker.Message{
			{Role: "user", Content: "which kernel?"},
			{Role: "assistant", ToolCalls: first.ToolCalls},
			{Role: "tool", ToolCallID: "c1", Content: "Darwin"},
		},
	}, "secret", nil)
	if err != nil {
		t.Fatalf("second turn error = %v", err)
	}
	if second.Output != "Darwin" || len(second.ToolCalls) != 0 || second.FinishReason != "stop" {
		t.Fatalf("second turn output=%q tool_calls=%d finish=%q, want the follow-up turn's answer",
			second.Output, len(second.ToolCalls), second.FinishReason)
	}
	if second.ProviderRequestID != "scripted_2" {
		t.Fatalf("second turn provider_request_id = %q, want the follow-up turn's own", second.ProviderRequestID)
	}
}

// A TURN THAT CALLS TWO TOOLS COSTS ONE TURN, NOT TWO. The engine emits a frame per tool call and
// answers all of them before the next model step, so the results arrive as one contiguous group —
// counting messages instead of groups would skip the follow-up turn entirely and land on whatever
// came after it. There is nothing after it here, so the skip would have shown up as the last turn
// repeating: the assertion is that the SECOND turn answers, on a conversation carrying two results.
func TestATurnCallingTwoToolsAdvancesByOneTurn(t *testing.T) {
	adapter := Adapter{Script: Script{
		Model:     "fake",
		ToolCalls: []modelbroker.ToolCall{{ID: "c1", Name: "a"}, {ID: "c2", Name: "b"}},
		Then: []Script{
			{Model: "fake", Output: "after one round", ToolCalls: []modelbroker.ToolCall{{ID: "c3", Name: "c"}}},
			{Model: "fake", Output: "after two rounds"},
		},
	}}
	res, err := adapter.Execute(context.Background(), modelbroker.Request{Messages: []modelbroker.Message{
		{Role: "user"},
		{Role: "assistant", ToolCalls: []modelbroker.ToolCall{{ID: "c1"}, {ID: "c2"}}},
		{Role: "tool", ToolCallID: "c1"},
		{Role: "tool", ToolCallID: "c2"},
	}}, "secret", nil)
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if res.Output != "after one round" {
		t.Fatalf("output = %q, want the FIRST follow-up turn: two results from one turn are one round", res.Output)
	}
}

// Past the last turn the last turn repeats — the documented tail, asserted rather than assumed,
// because it is what makes a script that outlives its turns answer instead of panicking.
func TestPastTheLastTurnTheLastTurnRepeats(t *testing.T) {
	adapter := Adapter{Script: Script{Model: "fake", ToolCalls: []modelbroker.ToolCall{{ID: "c1", Name: "a"}},
		Then: []Script{{Model: "fake", Output: "done"}}}}
	for _, groups := range []int{1, 2, 5} {
		var msgs []modelbroker.Message
		for i := 0; i < groups; i++ {
			msgs = append(msgs, modelbroker.Message{Role: "assistant"}, modelbroker.Message{Role: "tool"})
		}
		res, err := adapter.Execute(context.Background(), modelbroker.Request{Messages: msgs}, "secret", nil)
		if err != nil {
			t.Fatalf("%d groups: Execute error = %v", groups, err)
		}
		if res.Output != "done" {
			t.Fatalf("%d groups: output = %q, want the last turn repeated", groups, res.Output)
		}
	}
}

// TestLoadScriptReadsAMultiStepExchange proves the deployment seam's happy path end to end: the JSON
// field names an operator writes are the ones that arrive, and what arrives replays as two turns.
func TestLoadScriptReadsAMultiStepExchange(t *testing.T) {
	path := writeScript(t, `{
	  "provider_request_id": "scripted-uname",
	  "model": "fake",
	  "tool_calls": [{"id": "call_uname", "name": "palai.workspace.shell", "arguments": "{\"command\":\"uname\"}"}],
	  "then": [
	    {"provider_request_id": "scripted-answer", "model": "fake", "output": "Darwin",
	     "usage": {"input_tokens": 5, "output_tokens": 3, "total_tokens": 8}}
	  ]
	}`)

	script, err := LoadScript(path)
	if err != nil {
		t.Fatalf("LoadScript = %v, want the scripted exchange", err)
	}
	if script.ProviderRequestID != "scripted-uname" || len(script.ToolCalls) != 1 {
		t.Fatalf("first turn = %+v, want the shell call", script)
	}
	if got := script.ToolCalls[0]; got.ID != "call_uname" || got.Name != "palai.workspace.shell" || got.Arguments != `{"command":"uname"}` {
		t.Fatalf("tool call = %+v, want the id, name and verbatim arguments string from the file", got)
	}
	if len(script.Then) != 1 || script.Then[0].Output != "Darwin" || script.Then[0].Usage.TotalTokens != 8 {
		t.Fatalf("follow-up turns = %+v, want one answering turn carrying its usage", script.Then)
	}
}

// EVERY WAY A ROUTED SCRIPT CAN BE WRONG IS AN ERROR, NEVER THE BUILT-IN SCRIPT. This is the claim
// the seam is FOR: a silent fallback here is a proof that reports "my script ran" while driving a run
// that called no tool at all. Each case names a real mistake an operator makes with a JSON file.
func TestLoadScriptRefusesRatherThanFallingBack(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"malformed json", `{"model": "fake", "output": `, "parse"},
		{"trailing content", `{"model":"fake","output":"ok"} {"model":"fake"}`, "trailing content"},
		{"misspelled field", `{"model": "fake", "outputs": "Darwin"}`, "outputs"},
		{"empty object", `{}`, "answers nothing"},
		{"empty follow-up turn", `{"output":"a","then":[{}]}`, "answers nothing"},
		{"nested then", `{"output":"a","then":[{"output":"b","then":[{"output":"c"}]}]}`, "nests its own"},
		{"last turn calls a tool", `{"output":"a","then":[{"tool_calls":[{"id":"c1","name":"t"}]}]}`, "calls a tool"},
		{"single turn that only calls a tool", `{"tool_calls":[{"id":"c1","name":"t"}]}`, "calls a tool"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			script, err := LoadScript(writeScript(t, c.body))
			if err == nil {
				t.Fatalf("LoadScript accepted %s and returned %+v; a run driven by it would call no tool "+
					"while its operator believes the file is driving it", c.name, script)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("LoadScript error = %v, want it to name %q", err, c.want)
			}
		})
	}
}

// A path that names no file is the most ordinary mistake of all — a typo, or a file the control plane
// cannot read because it runs as another user — and it is the one a fallback would hide best.
func TestLoadScriptRefusesAPathItCannotRead(t *testing.T) {
	if _, err := LoadScript(filepath.Join(t.TempDir(), "no-such-script.json")); err == nil {
		t.Fatal("LoadScript accepted a path with no file behind it; want an error")
	}
}

func writeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "script.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return path
}

// TestAdapterWithoutLedgerReplaysEveryCall proves the default fake (no ledger) is
// unchanged: it replays its script on every call, deduping nothing.
func TestAdapterWithoutLedgerReplaysEveryCall(t *testing.T) {
	adapter := Adapter{Script: Script{ProviderRequestID: "prov_1", Model: "fake", Output: "hi"}}
	req := modelbroker.Request{ModelRequestID: "mreq_plain1", IdempotencyKey: "run_plain1/mreq_plain1"}

	for i := 0; i < 2; i++ {
		res, err := adapter.Execute(context.Background(), req, "secret", nil)
		if err != nil || res.Output != "hi" {
			t.Fatalf("Execute #%d = %q, %v, want the scripted output", i, res.Output, err)
		}
	}
}
