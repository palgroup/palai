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
// IT CARRIES NO SCHEMA AND NO DESCRIPTION, and that is the point rather than an omission. The shape
// of its arguments and the discipline for using them are trained into the model; the request sends
// {type, name} and the model supplies calls it already knows how to make. Writing our own schema
// would describe a slightly different tool than the one it learned.
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
		Name:         "str_replace_based_edit_tool",
		Type:         "text_editor_20250728",
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
