package tools

import (
	"context"
	"errors"

	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// defaultGlobLimit caps an unasked-for glob. A hundred paths is what fits in a model's working
// attention without crowding out the reason it searched; beyond that the useful move is a narrower
// pattern, and the truncation flag is what tells it to make one.
const defaultGlobLimit = 100

// GlobTool is the built-in filename search. It answers "which files are called this" — the question a
// model asks before it can ask anything else about a codebase — and it answers it through the
// workspace surface, so an allocation that lives on a machine is searched on that machine.
//
// IT IS A SEPARATE TOOL FROM CONTENT SEARCH, not a mode of one. The two take different inputs, return
// different shapes, and are reached for at different moments: glob when the name is the clue, grep
// when the contents are. Folding them together would make every call start with a mode decision the
// model has to get right before it can express what it actually wants.
func GlobTool() toolbroker.Tool {
	return toolbroker.Tool{
		Name: "palai.workspace.glob",
		Description: "Find files by name pattern within the run's workspace. `**` matches any depth " +
			"(`**/*.go` finds Go files anywhere, `src/**/*.ts` only under src), a single `*` matches " +
			"within one path segment (`*.go` finds only top-level Go files). Results come back " +
			"newest-modified first and capped, with a `truncated` flag when the cap dropped matches — " +
			"narrow the pattern rather than raising the cap. Use this when you know something about a " +
			"file's NAME; to search file CONTENTS, use the grep tool.",
		ReplayClass:  toolbroker.ClassPure,
		ParallelSafe: true, // reads names, changes nothing, safe to re-run on replay
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{
					"type":        "string",
					"description": "glob pattern relative to the workspace root, e.g. `**/*.go` or `src/**/*.ts`",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "maximum paths to return (default 100); results are newest-modified first, so a truncated answer still carries the most recently touched files",
				},
			},
			"required":             []any{"pattern"},
			"additionalProperties": false,
		},
		OutputSchema: map[string]any{"type": "object"},
		Exec:         globExec,
	}
}

func globExec(ctx context.Context, env toolbroker.ExecEnv, args map[string]any) (map[string]any, error) {
	if env.Workspace == nil {
		// Same rule the file tool states at length: no workspace bound means this attempt's allocation
		// is not reachable, and saying so lets the run continue. Reaching for this process's own disk
		// would answer for the wrong machine.
		return nil, toolbroker.Answerf(toolbroker.AnswerUnavailable, "glob tool: no workspace bound for this run")
	}
	pattern, _ := args["pattern"].(string)
	limit := defaultGlobLimit
	if raw, present := args["limit"]; present {
		if n, ok := raw.(float64); ok && n > 0 { // JSON numbers decode as float64
			limit = int(n)
		}
	}

	paths, truncated, err := env.Workspace.Glob(ctx, pattern, limit)
	if err != nil {
		// A SEARCH THAT FAILED CHANGED NOTHING, so every failure here is an answer the model can act
		// on: a refused pattern is `refused`, and anything else is `failed`. Ending the attempt over a
		// mistyped pattern is the outcome this classification exists to prevent.
		code := toolbroker.AnswerFailed
		if errors.Is(err, workspace.ErrPathEscape) {
			code = toolbroker.AnswerRefused
		}
		return nil, toolbroker.Answerf(code, "glob tool: %s", err.Error())
	}
	return map[string]any{"paths": paths, "truncated": truncated}, nil
}
