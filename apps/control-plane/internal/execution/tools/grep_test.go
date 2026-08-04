package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

func grepToolEnv(t *testing.T) toolbroker.ExecEnv {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	for rel, body := range map[string]string{
		"a.go":     "package main\nfunc Alpha() {}\n// Alpha again\n",
		"src/c.go": "package src\nfunc Alpha() {}\n",
		"notes.md": "nothing here\n",
	} {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	return toolbroker.ExecEnv{WorkspaceRoot: root, Workspace: LocalWorkspace(root)}
}

// TestGrepToolReturnsEachModesShape — the three modes answer three different questions, and a caller
// that got the wrong shape back would have to guess which one it asked.
func TestGrepToolReturnsEachModesShape(t *testing.T) {
	env := grepToolEnv(t)
	ctx := context.Background()

	files, err := GrepTool().Exec(ctx, env, map[string]any{"pattern": "Alpha"})
	if err != nil {
		t.Fatalf("default mode: %v", err)
	}
	if mode, _ := files["mode"].(string); mode != "files_with_matches" {
		t.Errorf("default mode = %q, want files_with_matches", mode)
	}
	if paths, _ := files["files"].([]string); len(paths) != 2 {
		t.Errorf("files = %v, want a.go and src/c.go", paths)
	}

	content, err := GrepTool().Exec(ctx, env, map[string]any{"pattern": "func Alpha", "output_mode": "content"})
	if err != nil {
		t.Fatalf("content mode: %v", err)
	}
	matches, _ := content["matches"].([]map[string]any)
	if len(matches) != 2 {
		t.Fatalf("content matches = %v, want two", matches)
	}
	for _, m := range matches {
		if m["path"] == "" || m["line"] == 0 || m["text"] == "" {
			t.Errorf("a match reached the model incomplete: %v", m)
		}
	}

	counted, err := GrepTool().Exec(ctx, env, map[string]any{"pattern": "Alpha", "output_mode": "count"})
	if err != nil {
		t.Fatalf("count mode: %v", err)
	}
	if total, _ := counted["total"].(int); total < 3 {
		t.Errorf("total = %v, want every Alpha", counted["total"])
	}
}

// TestAWorkspacelessGrepAnswersInsteadOfReachingLocalDisk — same guard the file and glob tools carry.
func TestAWorkspacelessGrepAnswersInsteadOfReachingLocalDisk(t *testing.T) {
	_, err := GrepTool().Exec(context.Background(), toolbroker.ExecEnv{}, map[string]any{"pattern": "x"})
	var answer *toolbroker.AnswerError
	if !errors.As(err, &answer) || answer.Code != toolbroker.AnswerUnavailable {
		t.Fatalf("error = %v, want an unavailable answer", err)
	}
}

// TestABadPatternIsAnAnswerNotAFailedAttempt — a mistyped regex is the single most likely thing to go
// wrong with this tool, and it is entirely recoverable: the model reads the diagnostic and tries
// again. Ending the attempt over it would be the defect the answer classification exists to prevent.
func TestABadPatternIsAnAnswerNotAFailedAttempt(t *testing.T) {
	env := grepToolEnv(t)
	_, err := GrepTool().Exec(context.Background(), env, map[string]any{"pattern": "func Alpha("})
	var answer *toolbroker.AnswerError
	if !errors.As(err, &answer) {
		t.Fatalf("error is %T (%v), want a toolbroker answer", err, err)
	}
	if answer.Code != toolbroker.AnswerInvalidArguments {
		t.Errorf("answer code = %q, want %q", answer.Code, toolbroker.AnswerInvalidArguments)
	}
}

// TestGrepDeclaresTheSchemaTheValidatorNowSupports is the payoff of teaching the validator arrays,
// booleans and enums: output_mode can say which three values it accepts, so a near-miss like
// "contents" is refused at validation instead of reaching the tool as an unknown mode.
func TestGrepDeclaresTheSchemaTheValidatorNowSupports(t *testing.T) {
	tool := GrepTool()
	if tool.ReplayClass != toolbroker.ClassPure {
		t.Errorf("ReplayClass = %q, want %q", tool.ReplayClass, toolbroker.ClassPure)
	}
	props, ok := tool.InputSchema["properties"].(map[string]any)
	if !ok {
		t.Fatal("no properties in the grep schema")
	}
	mode, _ := props["output_mode"].(map[string]any)
	allowed, _ := mode["enum"].([]any)
	if len(allowed) != 3 {
		t.Errorf("output_mode declares enum %v, want the three modes", allowed)
	}
	if multiline, _ := props["multiline"].(map[string]any); multiline["type"] != "boolean" {
		t.Errorf("multiline declares type %v, want boolean", multiline["type"])
	}
	if limit, _ := props["head_limit"].(map[string]any); limit["type"] != "integer" {
		t.Errorf("head_limit declares type %v, want integer", limit["type"])
	}
}
