package tools

import (
	"context"
	"fmt"

	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// CommitTool is the built-in workspace commit tool (spec §30.7). It records a Git commit of the
// worktree under a FIXED, configured author identity — it needs NO credential and it grants NO push
// permission: a commit is a local Git operation, and pushing is a separate approved capability (T8).
// It runs against the workspace repo dir directly (not the sandbox shell), mirroring the merge seam,
// so the commit is a control-plane Git operation the model cannot smuggle a credential into.
func CommitTool() toolbroker.Tool {
	return toolbroker.Tool{
		Name:        "palai.workspace.commit",
		Description: "Commit the current workspace changes to the run's Git repository with the given message.",
		ReplayClass: toolbroker.ClassReversible, // a workspace git commit is revertible (§26.6)
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"message": map[string]any{"type": "string"},
			},
			"required":             []any{"message"},
			"additionalProperties": false,
		},
		OutputSchema: map[string]any{"type": "object"},
		Exec:         commitExec,
	}
}

// commitExec commits the workspace repo. The repo lives at <allocation>/repo (spec §29.9); a
// workspace-less or read-only attempt fails cleanly rather than touching anything.
func commitExec(ctx context.Context, env toolbroker.ExecEnv, args map[string]any) (map[string]any, error) {
	// THE THREE PRE-FLIGHT REFUSALS ARE ANSWERS; the Commit below is not. The line is the one
	// answer.go draws: everything above env.Workspace.Commit is a statement about a write that did not
	// happen, so the model is told and the run continues, while Commit's own error may have left a
	// half-landed effect and still aborts the attempt. Each code is the one answer.go already names for
	// its condition — "no workspace bound" under AnswerUnavailable, "a write to a read-only workspace"
	// under AnswerRefused (the same spelling file.go:111 uses), a missing argument under
	// AnswerInvalidArguments, which the model can fix on its next turn and could not before.
	if env.WorkspaceRoot == "" || env.Workspace == nil {
		return nil, toolbroker.Answerf(toolbroker.AnswerUnavailable,
			"commit tool: no workspace bound for this run")
	}
	if env.ReadOnly {
		return nil, toolbroker.Answerf(toolbroker.AnswerRefused,
			"commit tool: workspace is read-only for this run")
	}
	message, _ := args["message"].(string)
	if message == "" {
		return nil, toolbroker.Answerf(toolbroker.AnswerInvalidArguments,
			"commit tool: a commit message is required")
	}
	// The commit runs WHERE THE REPOSITORY IS (A.3 T5), which is the machine holding this attempt's
	// lease whenever the allocation was realized there. It is still a control-plane-DIRECTED git
	// operation under the platform's fixed author identity — the model supplies a message and nothing
	// else — so the property this tool's doc comment claims is unchanged: no credential is involved
	// and no push permission is granted.
	sha, err := env.Workspace.Commit(ctx, message)
	if err != nil {
		return nil, fmt.Errorf("commit tool: %w", err)
	}
	// The result carries the commit sha ONLY — no push token, no credential, no publication handle.
	// Committing does not imply the ability to push (spec §30.7, TestCommitDoesNotImplyPush).
	return map[string]any{"commit": sha}, nil
}
