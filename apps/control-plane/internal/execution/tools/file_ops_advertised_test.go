package tools

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// A TOOL THAT ACCEPTS ANY STRING FOR `op` TELLS A MODEL THAT EVERY STRING IS WORTH TRYING.
//
// ‼️ MEASURED ON A LIVE RUN, 2026-08-06. Asked to write one file into a cloned repository, the model made
// ELEVEN tool calls and NINE were refused `invalid_arguments: file tool: unknown op`. It tried "create",
// "str_replace", "view" and "insert" — the text-editor vocabulary — because the advertisement declared
// `op` as a bare `{"type": "string"}`. The description names the operations in prose, and prose is not a
// constraint: it is read by a model that has already decided what it wants to call.
//
// The cost is not politeness. A refused call spends the attempt's tool-error budget, and the run answered
// "I am unable to create or modify files in the workspace" — which is the product failing at the one thing
// this whole workspace layer exists for, with every layer reporting success.
//
// These two tests hold the fix from both sides, because an enum is a claim about the dispatch:
//
//	(1) every advertised op is one fileExec really answers, driven rather than read; and
//	(2) every op fileExec answers is advertised — the direction a sweep over the schema alone cannot see,
//	    which this tree records as the one-directional-sweep failure.

// advertisedFileOps reads the enum out of the shipped advertisement. It is deliberately taken from
// FileTool() and never from a copy: a list written twice is a list that disagrees once.
func advertisedFileOps(t *testing.T) []string {
	t.Helper()
	schema, ok := FileTool().InputSchema["properties"].(map[string]any)
	if !ok {
		t.Fatal("the file tool's InputSchema has no properties object")
	}
	op, ok := schema["op"].(map[string]any)
	if !ok {
		t.Fatal("the file tool's InputSchema declares no `op` property")
	}
	raw, ok := op["enum"].([]any)
	if !ok || len(raw) == 0 {
		t.Fatal("the file tool advertises `op` with no enum: a model has no way to learn which words this " +
			"tool knows, and a live run spent nine of eleven calls guessing text-editor verbs")
	}
	ops := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("the `op` enum carries a non-string value %#v", v)
		}
		ops = append(ops, s)
	}
	sort.Strings(ops)
	return ops
}

func TestEveryAdvertisedFileOpIsOneTheToolAnswers(t *testing.T) {
	root := realTempDir(t)
	if err := os.WriteFile(filepath.Join(root, "seed.txt"), []byte("seed\n"), 0o600); err != nil {
		t.Fatalf("seed the workspace: %v", err)
	}
	tool := FileTool()
	env := toolbroker.ExecEnv{WorkspaceRoot: root, Workspace: LocalWorkspace(root)}

	// DRIVEN, NOT READ. A test that compared the enum against a list of strings in this file would pass
	// on an op the dispatch dropped — the advertisement is only true if the call it invites is answered.
	for _, op := range advertisedFileOps(t) {
		args := map[string]any{"op": op, "path": "seed.txt"}
		if op == "write" {
			args["path"], args["content"] = "written.txt", "x"
		}
		if op == "list" {
			args["path"] = "."
		}
		if _, err := tool.Exec(context.Background(), env, args); err != nil {
			if strings.Contains(err.Error(), "unknown op") {
				t.Errorf("the tool ADVERTISES op %q and refuses it as unknown: this is the defect the enum "+
					"was added to end, moved from the model's guess to our own list", op)
				continue
			}
			t.Errorf("advertised op %q failed on a workspace that has what it asked for: %v", op, err)
		}
	}
}

func TestEveryFileOpTheToolAnswersIsAdvertised(t *testing.T) {
	// THE OTHER DIRECTION, AND IT NEEDS THE SOURCE. An op the dispatch answers and the enum omits is
	// unreachable through a schema-respecting model — the tool can do it and nothing can ask. No
	// reflection reaches a switch, so this reads the file, which is what this tree already does to pin a
	// sole minter or a protocol's shape.
	dispatched := dispatchedFileOps(t)
	advertised := map[string]bool{}
	for _, op := range advertisedFileOps(t) {
		advertised[op] = true
	}
	for _, op := range dispatched {
		if !advertised[op] {
			t.Errorf("fileExec answers op %q and the advertisement does not offer it: a model that respects "+
				"the schema can never reach it, so the capability exists and nothing can ask for it", op)
		}
	}
	if len(dispatched) == 0 {
		t.Fatal("no `case` was parsed out of fileExec, so this test asserts nothing — the function's shape " +
			"changed and the matcher below did not")
	}
}

// dispatchedFileOps extracts the case labels of fileExec's switch from file.go.
//
// It is scoped to the function rather than the file: `case "read":` appears in other switches, and a
// matcher that swept the whole file would report ops from a different question as missing.
func dispatchedFileOps(t *testing.T) []string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test's own directory")
	}
	body, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "file.go"))
	if err != nil {
		t.Fatalf("read file.go: %v", err)
	}
	src := string(body)
	start := strings.Index(src, "func fileExec(")
	if start < 0 {
		t.Fatal("file.go has no fileExec function: the matcher is reading the wrong tree")
	}
	var ops []string
	for _, m := range regexp.MustCompile(`(?m)^\t*case "([a-z_]+)":`).FindAllStringSubmatch(src[start:], -1) {
		ops = append(ops, m[1])
	}
	sort.Strings(ops)
	return ops
}
