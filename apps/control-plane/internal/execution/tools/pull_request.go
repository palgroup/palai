package tools

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// PullRequestTool is the built-in open-pull-request publication tool (spec §30.10, REP-008). Like the
// push tool it does NOT act: it records a PENDING publication + approval for a DRAFT pull request from
// the run's work branch to the binding's base, returning "pending_approval" to the model. The model's
// proposed title/body are RECORDED on the publication (args) for a later policy-filtered pass — E09
// publishes with a deterministic default title/body. The head/base are resolved from the binding, not
// model-supplied. The idempotency key excludes the head, so a duplicate request dedupes to one PR once
// approved (REP-008).
func PullRequestTool() toolbroker.Tool {
	return toolbroker.Tool{
		Name:        "palai.publish.pull_request",
		Description: "Propose a pull request from the run's work branch to its bound base. The request is recorded for approval; no PR is opened until approved, and the destination is resolved from the run's binding, not model-supplied.",
		ReplayClass: toolbroker.ClassIdempotent, // one PR per branch under a stable idempotency key (§26.6, REP-008)
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"title": map[string]any{"description": "proposed pull request title"},
				"body":  map[string]any{"description": "proposed pull request description"},
			},
			"additionalProperties": false,
		},
		OutputSchema: map[string]any{"type": "object"},
		Exec:         pullRequestExec,
	}
}

func pullRequestExec(ctx context.Context, env toolbroker.ExecEnv, args map[string]any) (map[string]any, error) {
	if env.Publications == nil {
		return nil, fmt.Errorf("pull request tool: no publication registry wired for this run")
	}
	if env.WorkspaceRoot == "" {
		return nil, fmt.Errorf("pull request tool: no workspace bound for this run")
	}
	head, _, err := repositories.Head(ctx, filepath.Join(env.WorkspaceRoot, workspace.RepoDir))
	if err != nil {
		return nil, fmt.Errorf("pull request tool: read workspace head: %w", err)
	}
	op := map[string]any{"operation": "open_pull_request", "head_sha": head}
	if title, ok := args["title"].(string); ok {
		op["title"] = title
	}
	if body, ok := args["body"].(string); ok {
		op["body"] = body
	}
	return env.Publications.RequestPublication(ctx, env.Scope, op)
}
