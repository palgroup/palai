package tools

import (
	"context"
	"testing"

	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

type fakeRegistry struct {
	lastScope toolbroker.TaskScope
	lastOp    map[string]any
}

func (f *fakeRegistry) ApplyTask(_ context.Context, scope toolbroker.TaskScope, op map[string]any) (map[string]any, error) {
	f.lastScope, f.lastOp = scope, op
	return map[string]any{"tasks": []any{}}, nil
}

// TestTaskToolInjectsKindAndForwardsArgs proves the task tool tags its operation with kind "task",
// forwards the model's args, and passes the attempt scope to the registry.
func TestTaskToolInjectsKindAndForwardsArgs(t *testing.T) {
	reg := &fakeRegistry{}
	env := toolbroker.ExecEnv{Tasks: reg, Scope: toolbroker.TaskScope{SessionID: "ses1", RunID: "run1"}}
	if _, err := TaskTool().Exec(context.Background(), env, map[string]any{"key": "a", "title": "do A", "status": "open"}); err != nil {
		t.Fatalf("Exec() error = %v", err)
	}
	if reg.lastOp["kind"] != "task" {
		t.Fatalf("kind = %v, want task", reg.lastOp["kind"])
	}
	if reg.lastOp["key"] != "a" || reg.lastOp["title"] != "do A" || reg.lastOp["status"] != "open" {
		t.Fatalf("args not forwarded: %v", reg.lastOp)
	}
	if reg.lastScope.SessionID != "ses1" || reg.lastScope.RunID != "run1" {
		t.Fatalf("scope not passed through: %+v", reg.lastScope)
	}
}

// TestTodoToolInjectsTodoKind proves the todo tool tags its operation with kind "todo".
func TestTodoToolInjectsTodoKind(t *testing.T) {
	reg := &fakeRegistry{}
	if _, err := TodoTool().Exec(context.Background(), toolbroker.ExecEnv{Tasks: reg}, map[string]any{"key": "t1"}); err != nil {
		t.Fatalf("Exec() error = %v", err)
	}
	if reg.lastOp["kind"] != "todo" {
		t.Fatalf("kind = %v, want todo", reg.lastOp["kind"])
	}
}

// TestToolKindOverridesModelSuppliedKind proves the tool's kind is authoritative: a model that
// passes kind:"task" to the TODO tool still writes a todo (no cross-kind clobber).
func TestToolKindOverridesModelSuppliedKind(t *testing.T) {
	reg := &fakeRegistry{}
	if _, err := TodoTool().Exec(context.Background(), toolbroker.ExecEnv{Tasks: reg},
		map[string]any{"key": "t1", "kind": "task"}); err != nil {
		t.Fatalf("Exec() error = %v", err)
	}
	if reg.lastOp["kind"] != "todo" {
		t.Fatalf("kind = %v, want todo (model-supplied kind must not override the tool's kind)", reg.lastOp["kind"])
	}
}

// TestTaskToolWithoutRegistryFailsCleanly proves a task tool call on an attempt with no registry
// wired fails cleanly rather than panicking or touching the control plane's own state.
func TestTaskToolWithoutRegistryFailsCleanly(t *testing.T) {
	if _, err := TaskTool().Exec(context.Background(), toolbroker.ExecEnv{}, map[string]any{"key": "a"}); err == nil {
		t.Fatal("Exec() with no registry wired: want error, got nil")
	}
}
