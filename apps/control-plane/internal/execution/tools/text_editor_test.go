package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

const editorFixture = `package main

func Alpha() int {
	return 1
}

func Beta() int {
	return 1
}
`

func editorEnv(t *testing.T) (toolbroker.ExecEnv, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte(editorFixture), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return toolbroker.ExecEnv{WorkspaceRoot: root, Workspace: LocalWorkspace(root)}, root
}

func runEditor(t *testing.T, env toolbroker.ExecEnv, args map[string]any) (map[string]any, error) {
	t.Helper()
	return TextEditorTool().Exec(context.Background(), env, args)
}

// TestStrReplaceDemandsExactlyOneMatch is the whole reason for adopting this tool. Zero matches and
// several matches are BOTH errors, and the messages must differ: a model told "not found" widens its
// string, one told "3 matches" narrows it. Collapsing them into one message makes it guess.
func TestStrReplaceDemandsExactlyOneMatch(t *testing.T) {
	env, root := editorEnv(t)

	// Zero matches.
	_, err := runEditor(t, env, map[string]any{
		"command": "str_replace", "path": "a.go",
		"old_str": "func Gamma() int {", "new_str": "func Gamma() int {",
	})
	var answer *toolbroker.AnswerError
	if !errors.As(err, &answer) {
		t.Fatalf("zero-match error is %T (%v), want an answer", err, err)
	}
	if !strings.Contains(strings.ToLower(answer.Error()), "no match") {
		t.Errorf("zero-match message = %q, want it to say the string was not found", answer.Error())
	}

	// Several matches: `return 1` appears twice.
	_, err = runEditor(t, env, map[string]any{
		"command": "str_replace", "path": "a.go",
		"old_str": "\treturn 1\n", "new_str": "\treturn 2\n",
	})
	if !errors.As(err, &answer) {
		t.Fatalf("multi-match error is %T (%v), want an answer", err, err)
	}
	if !strings.Contains(answer.Error(), "2") {
		t.Errorf("multi-match message = %q, want it to say HOW MANY were found", answer.Error())
	}

	// The file must be untouched by either refusal.
	after, _ := os.ReadFile(filepath.Join(root, "a.go"))
	if string(after) != editorFixture {
		t.Error("a refused str_replace changed the file")
	}

	// Exactly one: unique because of the surrounding context.
	if _, err := runEditor(t, env, map[string]any{
		"command": "str_replace", "path": "a.go",
		"old_str": "func Alpha() int {\n\treturn 1\n}", "new_str": "func Alpha() int {\n\treturn 42\n}",
	}); err != nil {
		t.Fatalf("a unique replacement failed: %v", err)
	}
	edited, _ := os.ReadFile(filepath.Join(root, "a.go"))
	if !strings.Contains(string(edited), "return 42") {
		t.Error("the replacement did not land")
	}
	if !strings.Contains(string(edited), "func Beta() int {\n\treturn 1\n}") {
		t.Error("the replacement touched Beta, which it was not aimed at")
	}
}

// TestViewReturnsNumberedLines — the model reads a numbered view and then sends `old_str` back from
// it. If the numbering is absent or off, every subsequent replacement is aimed at the wrong place.
func TestViewReturnsNumberedLines(t *testing.T) {
	env, _ := editorEnv(t)
	out, err := runEditor(t, env, map[string]any{"command": "view", "path": "a.go"})
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	body, _ := out["content"].(string)
	if !strings.Contains(body, "1\tpackage main") {
		t.Errorf("view output does not start with a numbered line 1:\n%s", body)
	}
	if !strings.Contains(body, "3\tfunc Alpha() int {") {
		t.Errorf("line 3 is not numbered as 3:\n%s", body)
	}
}

// TestViewRangeSelectsLines covers the parameter that keeps a large file out of the context window.
func TestViewRangeSelectsLines(t *testing.T) {
	env, _ := editorEnv(t)
	out, err := runEditor(t, env, map[string]any{
		"command": "path", "path": "a.go", "view_range": []any{float64(3), float64(5)},
	})
	if err == nil {
		t.Fatal("an unknown command was accepted")
	}

	out, err = runEditor(t, env, map[string]any{
		"command": "view", "path": "a.go", "view_range": []any{float64(3), float64(5)},
	})
	if err != nil {
		t.Fatalf("view with a range: %v", err)
	}
	body, _ := out["content"].(string)
	if strings.Contains(body, "package main") {
		t.Error("a ranged view returned lines outside the range")
	}
	if !strings.Contains(body, "func Alpha") {
		t.Errorf("the range did not include line 3:\n%s", body)
	}
}

// TestCreateWritesTheWholeFile — `create` is the one command that legitimately replaces everything,
// and it must also make a new file.
func TestCreateWritesTheWholeFile(t *testing.T) {
	env, root := editorEnv(t)
	if _, err := runEditor(t, env, map[string]any{
		"command": "create", "path": "new/b.go", "file_text": "package b\n",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "new", "b.go"))
	if err != nil {
		t.Fatalf("read created file: %v", err)
	}
	if string(got) != "package b\n" {
		t.Errorf("created content = %q", got)
	}
}

// TestInsertPlacesTextAfterTheGivenLine, including insert_line 0 meaning the start of the file.
func TestInsertPlacesTextAfterTheGivenLine(t *testing.T) {
	env, root := editorEnv(t)
	if _, err := runEditor(t, env, map[string]any{
		"command": "insert", "path": "a.go", "insert_line": float64(0), "insert_text": "// top\n",
	}); err != nil {
		t.Fatalf("insert at 0: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(root, "a.go"))
	if !strings.HasPrefix(string(got), "// top\npackage main") {
		t.Errorf("insert_line 0 did not place the text at the start:\n%s", got)
	}
}

// TestAWorkspacelessEditorAnswersInsteadOfTouchingLocalDisk mirrors the guard every workspace tool
// carries: a nil Workspace is `unavailable`, never a fallback to this process's own filesystem.
func TestAWorkspacelessEditorAnswersInsteadOfTouchingLocalDisk(t *testing.T) {
	_, err := runEditor(t, toolbroker.ExecEnv{}, map[string]any{"command": "view", "path": "a.go"})
	var answer *toolbroker.AnswerError
	if !errors.As(err, &answer) || answer.Code != toolbroker.AnswerUnavailable {
		t.Fatalf("error = %v, want an unavailable answer", err)
	}
}

// TestTheEditorDeclaresATypeAndNoSchema pins the shape provider_two's typed branch depends on. A
// schema here would be refused by that adapter — deliberately, because the schema this tool works to
// lives in the model.
func TestTheEditorDeclaresATypeAndNoSchema(t *testing.T) {
	tool := TextEditorTool()
	if tool.Type != "text_editor_20250728" {
		t.Errorf("Type = %q", tool.Type)
	}
	if tool.Name != "str_replace_based_edit_tool" {
		t.Errorf("Name = %q — this literal is what the model was trained on", tool.Name)
	}
	if len(tool.InputSchema) != 0 {
		t.Errorf("InputSchema is non-empty (%v); an Anthropic-defined tool carries no schema", tool.InputSchema)
	}
	if tool.Description != "" {
		t.Errorf("Description is set (%q); the platform does not restate what this tool does", tool.Description)
	}
}
