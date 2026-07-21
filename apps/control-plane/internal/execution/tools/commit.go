package tools

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
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
	if env.WorkspaceRoot == "" {
		return nil, fmt.Errorf("commit tool: no workspace bound for this run")
	}
	if env.ReadOnly {
		return nil, fmt.Errorf("commit tool: workspace is read-only for this run")
	}
	message, _ := args["message"].(string)
	if message == "" {
		return nil, fmt.Errorf("commit tool: a commit message is required")
	}
	repoDir := filepath.Join(env.WorkspaceRoot, workspace.RepoDir)
	sha, err := repositories.Commit(ctx, repoDir, message)
	if err != nil {
		return nil, fmt.Errorf("commit tool: %w", err)
	}
	// The result carries the commit sha ONLY — no push token, no credential, no publication handle.
	// Committing does not imply the ability to push (spec §30.7, TestCommitDoesNotImplyPush).
	return map[string]any{"commit": sha}, nil
}
