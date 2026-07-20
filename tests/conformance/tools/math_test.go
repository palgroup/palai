// Package tools_test is the pure in-process tool-broker conformance suite. It
// proves palai.conformance.math.add validates its input against a strict schema,
// produces the strict output, is idempotent on a repeated tool_call_id (the same
// call never re-executes), and emits exactly one usage event per real execution
// (spec §26.7 fenced tool-call rows). No network or credential is involved.
package tools_test

import (
	"context"
	"errors"
	"testing"

	"github.com/palgroup/palai/packages/contracts"
	statemachines "github.com/palgroup/palai/packages/state-machines"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

func TestMathAddProducesStrictSum(t *testing.T) {
	b := toolbroker.New(toolbroker.ConformanceMathAdd())
	out, err := b.Execute(context.Background(), contracts.ToolCallID("tcall_add1"), "palai.conformance.math.add",
		map[string]any{"a": 7, "b": 5}, 1, toolbroker.ExecEnv{})
	if err != nil {
		t.Fatalf("execute add: %v", err)
	}
	if sum, _ := out.Result["sum"]; sum != 12 {
		t.Errorf("result = %v, want {sum: 12}", out.Result)
	}
	if out.State != statemachines.ToolCallCompleted {
		t.Errorf("state = %s, want completed", out.State)
	}
	if out.Hash == "" {
		t.Error("completed tool call carries no request hash")
	}
	if out.Usage.ToolCalls != 1 {
		t.Errorf("usage.tool_calls = %d, want one usage event", out.Usage.ToolCalls)
	}
	if out.Cached {
		t.Error("first execution must not be served from cache")
	}
}

// TestSameToolCallIDDoesNotReExecute proves the fenced row caches the completed
// result: a second call with the same tool_call_id returns the stored result
// without invoking the tool again and without emitting a second usage event.
func TestSameToolCallIDDoesNotReExecute(t *testing.T) {
	var runs int
	add := toolbroker.ConformanceMathAdd()
	inner := add.Invoke
	add.Invoke = func(args map[string]any) (map[string]any, error) {
		runs++
		return inner(args)
	}
	b := toolbroker.New(add)

	id := contracts.ToolCallID("tcall_dup1")
	first, err := b.Execute(context.Background(), id, "palai.conformance.math.add", map[string]any{"a": 7, "b": 5}, 1, toolbroker.ExecEnv{})
	if err != nil {
		t.Fatalf("first execute: %v", err)
	}
	// A higher fence on the same completed call still replays the cache: completion
	// wins over a re-lease, so the tool never runs twice.
	second, err := b.Execute(context.Background(), id, "palai.conformance.math.add", map[string]any{"a": 7, "b": 5}, 2, toolbroker.ExecEnv{})
	if err != nil {
		t.Fatalf("duplicate execute: %v", err)
	}
	if runs != 1 {
		t.Errorf("tool invoked %d times, want exactly 1 for a duplicate tool_call_id", runs)
	}
	if !second.Cached {
		t.Error("duplicate call was not served from cache")
	}
	if second.Result["sum"] != first.Result["sum"] {
		t.Errorf("cached result %v differs from first %v", second.Result, first.Result)
	}
	if second.Usage.ToolCalls != 0 {
		t.Errorf("duplicate call emitted a second usage event: %+v", second.Usage)
	}
}

// TestStrictSchemaRejectsBadInput proves the input schema is enforced strictly:
// a wrong-typed or missing argument fails before the tool runs.
func TestStrictSchemaRejectsBadInput(t *testing.T) {
	var runs int
	add := toolbroker.ConformanceMathAdd()
	inner := add.Invoke
	add.Invoke = func(args map[string]any) (map[string]any, error) {
		runs++
		return inner(args)
	}
	b := toolbroker.New(add)

	cases := map[string]map[string]any{
		"missing b":   {"a": 7},
		"wrong type":  {"a": 7, "b": "five"},
		"extra field": {"a": 7, "b": 5, "c": 9},
	}
	for name, args := range cases {
		if _, err := b.Execute(context.Background(), contracts.ToolCallID("tcall_bad"), "palai.conformance.math.add", args, 1, toolbroker.ExecEnv{}); err == nil {
			t.Errorf("%s: strict schema accepted invalid input %v", name, args)
		}
	}
	if runs != 0 {
		t.Errorf("tool ran %d times on invalid input, want 0", runs)
	}
}

// TestOnlyExplicitConformanceToolsAreDiscoverable proves the broker exposes only
// the tools it was constructed with; an unregistered name is not discoverable and
// cannot be executed.
func TestOnlyExplicitConformanceToolsAreDiscoverable(t *testing.T) {
	b := toolbroker.New(toolbroker.ConformanceMathAdd())
	if !b.Discoverable("palai.conformance.math.add") {
		t.Error("the explicit conformance tool is not discoverable")
	}
	if b.Discoverable("palai.conformance.fs.delete") {
		t.Error("an unregistered tool is discoverable")
	}
	if _, err := b.Execute(context.Background(), contracts.ToolCallID("tcall_x"), "palai.conformance.fs.delete", nil, 1, toolbroker.ExecEnv{}); !errors.Is(err, toolbroker.ErrUnknownTool) {
		t.Errorf("executing an unknown tool: got %v, want ErrUnknownTool", err)
	}

	empty := toolbroker.New()
	if empty.Discoverable("palai.conformance.math.add") {
		t.Error("math.add is discoverable without being registered")
	}
}
