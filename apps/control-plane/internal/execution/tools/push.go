package tools

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// PushTool is the built-in push-branch publication tool (spec §30.9, REP-004/006). It does NOT push:
// pushing is a gated side effect. The tool computes the workspace repo's current head and records a
// PENDING publication + approval through the registry, returning "pending_approval" and the exact
// operation display to the model. The push itself happens only after an approve, at a safe boundary,
// through the approval pump — and the destination (remote/branch) is resolved from the run's binding,
// never model-supplied, so the model cannot redirect a push.
func PushTool() toolbroker.Tool {
	return toolbroker.Tool{
		Name:        "palai.publish.push",
		Description: "Request a push of the run's work branch to its bound remote. The push is recorded for approval and happens only after approval; the destination is resolved from the run's binding, not model-supplied.",
		ReplayClass: toolbroker.ClassIdempotent, // records a pending publication under a stable idempotency key (§26.6, TOL-002)
		InputSchema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{},
			"additionalProperties": false,
		},
		OutputSchema: map[string]any{"type": "object"},
		Exec:         pushExec,
	}
}

func pushExec(ctx context.Context, env toolbroker.ExecEnv, _ map[string]any) (map[string]any, error) {
	if env.Publications == nil {
		return nil, fmt.Errorf("push tool: no publication registry wired for this run")
	}
	if env.WorkspaceRoot == "" {
		return nil, fmt.Errorf("push tool: no workspace bound for this run")
	}
	head, _, err := repositories.Head(ctx, filepath.Join(env.WorkspaceRoot, workspace.RepoDir))
	if err != nil {
		return nil, fmt.Errorf("push tool: read workspace head: %w", err)
	}
	return env.Publications.RequestPublication(ctx, env.Scope, map[string]any{
		"operation": "push_branch",
		"head_sha":  head,
	})
}
