package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// maxEditorReadBytes bounds one view, mirroring the file tool's own ceiling.
const maxEditorReadBytes = 1 << 20

// TextEditorTool is Anthropic's client-side text editor (`text_editor_20250728`), executed here
// against the run's workspace.
//
// ON AN ANTHROPIC ROUTE IT CARRIES NEITHER SCHEMA NOR DESCRIPTION, and that is the point rather than an
// omission: the shape of its arguments and the discipline for using them are trained into the model, the
// request sends {type, name}, and the model supplies calls it already knows how to make.
//
// ‼️ IT NOW ALSO CARRIES A SCHEMA, FOR EVERY OTHER PROVIDER, and the reason is not portability for its
// own sake. Measured on a live stack 2026-08-06: `palai up` grants the canonical tool baseline — which
// contains this tool — to every project, AND routes the deployment to provider-one when
// PALAI_MODEL_PROVIDER says so. Those two halves of one bring-up were mutually exclusive: the OpenAI
// adapter refused a typed tool, so every run on an OpenAI-routed stack died at dispatch with "route this
// agent to an Anthropic model or drop the tool from its set" — advice nobody could act on, because the
// stack's own bring-up had chosen both. An agent that cannot reach this tool cannot change a line of
// code, which is the whole product.
//
// THE SCHEMA IS A RESTATEMENT AND THE ANTHROPIC PATH DOES NOT SEE IT. provider_two takes the type and
// leaves the schema unsent, so the trained shape is unchanged there and nothing about an Anthropic run
// moves. Everywhere else the model reads this description instead — a slightly different tool than the
// one Anthropic learned, which is the honest trade against having no editor at all.
//
// WHAT IT REPLACES IS WHOLE-FILE REWRITING. Before it, the only way to change a line was to send the
// entire file back through the file tool's write — every untouched line re-generated, every one of
// them a chance to drop a function or reorder an import, with nothing checking that the result
// differed only where it was meant to. `str_replace` narrows that to the span the model names, and
// REFUSES when the span is ambiguous rather than picking one.
//
// `undo_edit` IS NOT PART OF THIS VERSION and is deliberately not implemented.
func TextEditorTool() toolbroker.Tool {
	return toolbroker.Tool{
		Name: "str_replace_based_edit_tool",
		Type: "text_editor_20250728",
		// ‼️ THE DESCRIPTION NAMES THE LAYOUT, and the sentence it replaces cost a whole publish. It said
		// only "relative to the workspace root", which is true and useless: the workspace root is the
		// ALLOCATION root, and a bound repository is checked out one level down at `repo/`. Measured
		// 2026-08-06 on a live run against a private GitHub repository — the agent wrote
		// PALAI_AGENT.md at the allocation root, `palai.workspace.commit` operates on the repository and
		// therefore saw nothing new, and the branch that reached GitHub did not contain the file while the
		// commit message said it did.
		//
		// The shell tool's description already names this for the same reason. Two tools, one layout, and
		// an agent that can only learn it from what it is told.
		Description: "View and edit files in the run's workspace. `view` reads a file (optionally a line " +
			"range) or lists a directory; `create` writes a file, replacing it if it exists; `str_replace` " +
			"replaces ONE exact occurrence of old_str and refuses when the span is ambiguous or absent; " +
			"`insert` inserts text after a line number. Paths are relative to the workspace root, and when " +
			"the run has a repository bound it is checked out at ./repo — so a file that belongs to the " +
			"repository is written under repo/ (e.g. repo/README.md), and only a path under repo/ is " +
			"committed or published.",
		// The four commands textEditorExec implements, and nothing it does not: `undo_edit` is absent
		// here for the same reason it is absent from the switch below.
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type": "string",
					"enum": []string{"view", "create", "str_replace", "insert"},
				},
				"path":        map[string]any{"type": "string", "description": "path relative to the workspace root; repository files live under repo/"},
				"file_text":   map[string]any{"type": "string", "description": "the whole file body (create)"},
				"old_str":     map[string]any{"type": "string", "description": "the exact text to replace, once (str_replace)"},
				"new_str":     map[string]any{"type": "string", "description": "the replacement text (str_replace)"},
				"insert_line": map[string]any{"type": "integer", "description": "insert after this 1-based line (insert)"},
				"view_range": map[string]any{
					"type": "array", "items": map[string]any{"type": "integer"},
					"description": "[start, end] 1-based inclusive line range (view)",
				},
			},
			"required": []string{"command", "path"},
		},
		ReplayClass:  toolbroker.ClassReversible, // a workspace edit is revertible via the snapshot (§26.6)
		OutputSchema: map[string]any{"type": "object"},
		Exec:         textEditorExec,
	}
}

func textEditorExec(ctx context.Context, env toolbroker.ExecEnv, args map[string]any) (map[string]any, error) {
	if env.Workspace == nil {
		return nil, toolbroker.Answerf(toolbroker.AnswerUnavailable, "text editor: no workspace bound for this run")
	}
	command, _ := args["command"].(string)
	path, _ := args["path"].(string)

	switch command {
	case "view":
		return editorView(ctx, env, path, args["view_range"])
	case "create":
		text, _ := args["file_text"].(string)
		return editorWrite(ctx, env, path, text)
	case "str_replace":
		oldStr, _ := args["old_str"].(string)
		newStr, _ := args["new_str"].(string)
		return editorStrReplace(ctx, env, path, oldStr, newStr)
	case "insert":
		text, _ := args["insert_text"].(string)
		return editorInsert(ctx, env, path, intArg(args, "insert_line", 0), text)
	default:
		return nil, toolbroker.Answerf(toolbroker.AnswerInvalidArguments,
			"text editor: unknown command %q (view, create, str_replace, insert)", command)
	}
}

// editorView returns the file with line numbers, optionally narrowed to a range.
//
// THE NUMBERS ARE NOT DECORATION: the model reads this output and then sends a span of it back as
// `old_str`. Numbering is what lets it quote a line it can see, and a range is what keeps a large
// file from crowding out the reason it was opened.
func editorView(ctx context.Context, env toolbroker.ExecEnv, path string, rawRange any) (map[string]any, error) {
	data, truncated, err := env.Workspace.Read(ctx, path, maxEditorReadBytes)
	if err != nil {
		return nil, editorAnswer(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	first, last := 1, len(lines)
	if span, ok := rawRange.([]any); ok && len(span) == 2 {
		if n, ok := span[0].(float64); ok && int(n) >= 1 {
			first = int(n)
		}
		if n, ok := span[1].(float64); ok && int(n) >= first {
			last = min(int(n), len(lines))
		}
	}
	if first > len(lines) {
		return nil, toolbroker.Answerf(toolbroker.AnswerInvalidArguments,
			"text editor: view_range starts at %d but %q has %d lines", first, path, len(lines))
	}
	var b strings.Builder
	for i := first; i <= last; i++ {
		fmt.Fprintf(&b, "%d\t%s\n", i, lines[i-1])
	}
	return map[string]any{"path": path, "content": b.String(), "truncated": truncated}, nil
}

func editorWrite(ctx context.Context, env toolbroker.ExecEnv, path, text string) (map[string]any, error) {
	report, err := env.Workspace.Write(ctx, path, []byte(text))
	if err != nil {
		return nil, editorAnswer(err)
	}
	return map[string]any{
		"path": report.Path, "before_hash": report.BeforeHash,
		"after_hash": report.AfterHash, "created": report.Created,
	}, nil
}

// editorStrReplace is the command this tool exists for, and its arity rule is the whole discipline:
// the old string must appear EXACTLY ONCE.
//
// THE TWO REFUSALS SAY DIFFERENT THINGS ON PURPOSE. A model told "no match" widens its string; one
// told "found 3" narrows it. One shared message would leave it guessing which way to move, and
// guessing is what a first-match editor does silently — which is the failure this rule prevents.
//
// COUNTING IS ON RAW BYTES: no trimming, no whitespace normalisation. A single space of difference
// must miss, because that is the contract the model is working to when it quotes a line it saw.
func editorStrReplace(ctx context.Context, env toolbroker.ExecEnv, path, oldStr, newStr string) (map[string]any, error) {
	if oldStr == "" {
		return nil, toolbroker.Answerf(toolbroker.AnswerInvalidArguments, "text editor: old_str is empty")
	}
	data, _, err := env.Workspace.Read(ctx, path, maxEditorReadBytes)
	if err != nil {
		return nil, editorAnswer(err)
	}
	body := string(data)
	switch n := strings.Count(body, oldStr); {
	case n == 0:
		return nil, toolbroker.Answerf(toolbroker.AnswerNotFound,
			"text editor: no match for old_str in %q — quote the text exactly as `view` showed it, including indentation", path)
	case n > 1:
		return nil, toolbroker.Answerf(toolbroker.AnswerInvalidArguments,
			"text editor: old_str appears %d times in %q — include enough surrounding context to name exactly one", n, path)
	}
	return editorWrite(ctx, env, path, strings.Replace(body, oldStr, newStr, 1))
}

// editorInsert places text after a given line; line 0 means the start of the file.
func editorInsert(ctx context.Context, env toolbroker.ExecEnv, path string, after int, text string) (map[string]any, error) {
	data, _, err := env.Workspace.Read(ctx, path, maxEditorReadBytes)
	if err != nil {
		return nil, editorAnswer(err)
	}
	body := string(data)
	trailing := strings.HasSuffix(body, "\n")
	lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
	if after < 0 || after > len(lines) {
		return nil, toolbroker.Answerf(toolbroker.AnswerInvalidArguments,
			"text editor: insert_line %d is outside %q, which has %d lines", after, path, len(lines))
	}
	inserted := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	out := make([]string, 0, len(lines)+len(inserted))
	out = append(out, lines[:after]...)
	out = append(out, inserted...)
	out = append(out, lines[after:]...)
	joined := strings.Join(out, "\n")
	if trailing {
		joined += "\n"
	}
	return editorWrite(ctx, env, path, joined)
}

// editorAnswer classifies a workspace failure for the model, following the rule file.go states at
// length: a read that failed changed nothing, so it is answerable; a refused path is `refused`
// rather than an error code that sounds like a bug.
func editorAnswer(err error) error {
	switch {
	case errors.Is(err, workspace.ErrPathEscape), errors.Is(err, workspace.ErrNotRegular):
		return toolbroker.Answerf(toolbroker.AnswerRefused, "text editor: %s", err.Error())
	default:
		return toolbroker.Answerf(toolbroker.AnswerNotFound, "text editor: %s", err.Error())
	}
}
