package tools

import (
	"context"
	"errors"
	"strings"

	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// defaultGrepLimit caps an unasked-for search. Fifty entries is enough to see the shape of an answer
// and few enough to leave room for the work the search was for; past that the useful move is a
// narrower pattern, and `truncated` is what says so.
const defaultGrepLimit = 50

// GrepTool is the built-in content search — the tool that answers "where is this written", which is
// the question a model asks before it can change anything it did not write itself.
//
// WITHOUT IT THE WORK FALLS TO THE SHELL, and that is worse in three ways a description cannot fix: a
// shell command is classified irreversible, so it carries replay semantics meant for things with side
// effects; it is budgeted for a command rather than a lookup; and its output arrives as an unparsed
// blob the model has to read as text. A search is none of those things.
func GrepTool() toolbroker.Tool {
	return toolbroker.Tool{
		Name: "palai.workspace.grep",
		Description: "Search file CONTENTS within the run's workspace. Patterns are RE2 regular " +
			"expressions — the same syntax ripgrep uses, NOT POSIX grep — so metacharacters need " +
			"escaping: finding `interface{}` in Go takes the pattern `interface\\{\\}`. Three output " +
			"modes: `files_with_matches` (the default, cheapest — which files are worth opening), " +
			"`content` (matching lines with file and line number), and `count` (matches per file plus " +
			"a total). Scope a search with `path` for a subtree or `glob` for a filename filter. " +
			"Results are capped and set `truncated` when the cap dropped entries — narrow the pattern " +
			"rather than raising the cap. Use this when you know something about a file's CONTENTS; " +
			"to find files by NAME, use the glob tool.",
		ReplayClass:  toolbroker.ClassPure,
		ParallelSafe: true, // reads contents, changes nothing
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{
					"type":        "string",
					"description": "RE2 regular expression, e.g. `func \\w+Handler` or `interface\\{\\}`",
				},
				"path": map[string]any{
					"type":        "string",
					"description": "workspace-relative subtree to search; omit to search everything",
				},
				"glob": map[string]any{
					"type":        "string",
					"description": "filename filter, e.g. `**/*.go` — `**` crosses directories, a single `*` does not",
				},
				"output_mode": map[string]any{
					"type":        "string",
					"enum":        []any{"files_with_matches", "content", "count"},
					"description": "files_with_matches (default): paths only. content: matching lines with line numbers. count: matches per file plus a total",
				},
				"-B": map[string]any{
					"type":        "integer",
					"description": "lines of context BEFORE each match (content mode only)",
				},
				"-A": map[string]any{
					"type":        "integer",
					"description": "lines of context AFTER each match (content mode only)",
				},
				"multiline": map[string]any{
					"type":        "boolean",
					"description": "let the pattern match across line boundaries (default false)",
				},
				"head_limit": map[string]any{
					"type":        "integer",
					"description": "maximum entries to list (default 50). In count mode the total still covers every match, including files this cap did not list",
				},
			},
			"required":             []any{"pattern"},
			"additionalProperties": false,
		},
		OutputSchema: map[string]any{"type": "object"},
		Exec:         grepExec,
	}
}

func grepExec(ctx context.Context, env toolbroker.ExecEnv, args map[string]any) (map[string]any, error) {
	if env.Workspace == nil {
		return nil, toolbroker.Answerf(toolbroker.AnswerUnavailable, "grep tool: no workspace bound for this run")
	}
	req := toolbroker.GrepRequest{Limit: defaultGrepLimit}
	req.Pattern, _ = args["pattern"].(string)
	req.Path, _ = args["path"].(string)
	req.Glob, _ = args["glob"].(string)
	req.OutputMode, _ = args["output_mode"].(string)
	req.Multiline, _ = args["multiline"].(bool)
	req.Before = intArg(args, "-B", 0)
	req.After = intArg(args, "-A", 0)
	req.Limit = intArg(args, "head_limit", defaultGrepLimit)

	res, err := env.Workspace.Grep(ctx, req)
	if err != nil {
		// A SEARCH THAT FAILED CHANGED NOTHING, so each failure is an answer the model can act on. The
		// classification matters more here than for most tools because the likeliest failure — a
		// mistyped regex — is entirely recoverable, and ending an attempt over it would be the outcome
		// this whole answer seam exists to prevent.
		code := toolbroker.AnswerFailed
		switch {
		case errors.Is(err, workspace.ErrPathEscape):
			code = toolbroker.AnswerRefused
		case strings.Contains(err.Error(), "pattern"), strings.Contains(err.Error(), "output mode"):
			code = toolbroker.AnswerInvalidArguments
		}
		return nil, toolbroker.Answerf(code, "grep tool: %s", err.Error())
	}

	out := map[string]any{"mode": res.Mode, "truncated": res.Truncated}
	switch res.Mode {
	case "content":
		matches := make([]map[string]any, 0, len(res.Matches))
		for _, m := range res.Matches {
			entry := map[string]any{"path": m.Path, "line": m.Line, "text": m.Text}
			if len(m.Before) > 0 {
				entry["before"] = m.Before
			}
			if len(m.After) > 0 {
				entry["after"] = m.After
			}
			matches = append(matches, entry)
		}
		out["matches"] = matches
	case "count":
		out["counts"] = res.Counts
		out["total"] = res.Total
	default:
		out["files"] = res.Files
	}
	return out, nil
}

// intArg reads a JSON number argument, which decodes as float64.
func intArg(args map[string]any, name string, fallback int) int {
	if raw, present := args[name]; present {
		if n, ok := raw.(float64); ok && n >= 0 {
			return int(n)
		}
	}
	return fallback
}
