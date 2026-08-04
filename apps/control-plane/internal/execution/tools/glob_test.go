package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// globEnv binds a real local workspace over a temp tree, so these tests exercise the same path a run
// takes when its allocation lives on this host.
func globEnv(t *testing.T, files ...string) toolbroker.ExecEnv {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	for _, rel := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(abs, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	return toolbroker.ExecEnv{WorkspaceRoot: root, Workspace: LocalWorkspace(root)}
}

// TestGlobToolReturnsPathsAndTruncation pins the result shape a model reads. `truncated` is not
// decoration: without it a capped answer is indistinguishable from a complete one, and a model
// concludes the files it did not see are absent.
func TestGlobToolReturnsPathsAndTruncation(t *testing.T) {
	env := globEnv(t, "a.go", "b.go", "src/c.go")

	out, err := GlobTool().Exec(context.Background(), env, map[string]any{"pattern": "**/*.go"})
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	paths, _ := out["paths"].([]string)
	if len(paths) != 3 {
		t.Errorf("matched %v, want three .go files", paths)
	}
	if truncated, _ := out["truncated"].(bool); truncated {
		t.Error("an uncapped glob reported truncation")
	}

	capped, err := GlobTool().Exec(context.Background(), env, map[string]any{"pattern": "**/*.go", "limit": float64(2)})
	if err != nil {
		t.Fatalf("glob with a limit: %v", err)
	}
	if paths, _ := capped["paths"].([]string); len(paths) != 2 {
		t.Errorf("limit 2 returned %v", paths)
	}
	if truncated, _ := capped["truncated"].(bool); !truncated {
		t.Error("a capped glob did not report truncation")
	}
}

// TestAWorkspacelessGlobAnswersInsteadOfReachingLocalDisk mirrors the guard the file tool already
// carries. A nil Workspace means this attempt's allocation is not reachable — the answer is to say
// so, never to fall back to whatever this process happens to have on its own filesystem.
func TestAWorkspacelessGlobAnswersInsteadOfReachingLocalDisk(t *testing.T) {
	_, err := GlobTool().Exec(context.Background(), toolbroker.ExecEnv{}, map[string]any{"pattern": "**/*"})
	var answer *toolbroker.AnswerError
	if !errors.As(err, &answer) {
		t.Fatalf("error is %T (%v), want a toolbroker answer", err, err)
	}
	if answer.Code != toolbroker.AnswerUnavailable {
		t.Errorf("answer code = %q, want %q", answer.Code, toolbroker.AnswerUnavailable)
	}
}

// TestAGlobRefusalIsAnAnswer — a search that was refused changed nothing, so the model may adjust and
// try again. Returning a raw error would end the attempt over a fixable mistake, which is the defect
// the file tool's own classification rule was written to stop.
func TestAGlobRefusalIsAnAnswer(t *testing.T) {
	env := globEnv(t, "a.go")
	_, err := GlobTool().Exec(context.Background(), env, map[string]any{"pattern": "../*"})
	var answer *toolbroker.AnswerError
	if !errors.As(err, &answer) {
		t.Fatalf("error is %T (%v), want a toolbroker answer", err, err)
	}
	if answer.Code != toolbroker.AnswerRefused {
		t.Errorf("answer code = %q, want %q", answer.Code, toolbroker.AnswerRefused)
	}
}

// TestGlobDeclaresItsSchemaAndClass — the schema types were undeclarable until the validator learned
// `integer`-alongside-`enum`; declaring them is what makes a malformed call fail at validation rather
// than inside the tool. ClassPure is the honest class: a glob has no external side effect at all.
func TestGlobDeclaresItsSchemaAndClass(t *testing.T) {
	tool := GlobTool()
	if tool.ReplayClass != toolbroker.ClassPure {
		t.Errorf("ReplayClass = %q, want %q — a glob changes nothing", tool.ReplayClass, toolbroker.ClassPure)
	}
	props, ok := tool.InputSchema["properties"].(map[string]any)
	if !ok {
		t.Fatal("no properties in the glob schema")
	}
	if pattern, _ := props["pattern"].(map[string]any); pattern["type"] != "string" {
		t.Errorf("pattern declares type %v, want string", pattern["type"])
	}
	if limit, _ := props["limit"].(map[string]any); limit["type"] != "integer" {
		t.Errorf("limit declares type %v, want integer", limit["type"])
	}
}
