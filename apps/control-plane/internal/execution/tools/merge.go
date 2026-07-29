package tools

import (
	"context"
	"fmt"

	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// MergeTool is the built-in merge-pull-request publication tool (E23 T6), and its size is the epic's
// claim rather than an accident: once a tool call can wait on a human, the third side effect the owner
// asked for — "kod yazdırır, PR açtırır, merge ettirir" — is a switch case and a CHECK value.
//
// It takes NO ARGUMENTS AT ALL, which is stronger than "no destination". The pull request to merge is the
// one THIS RUN opened, read back from its own published receipt; the head sent to GitHub is the head that
// publication carried; the merge method is the binding's policy. There is no field for a model to name a
// number, a repository or a method in, because a destination the model can name is a destination the
// approver did not approve — and unlike the push and pull-request tools, this one does not even have prose
// to propose.
//
// Like its two siblings it does not act: it records a PENDING publication and answers pending_approval,
// and the dispatcher parks the run on it (E23 T1/T3). The merge happens at a boundary after a human
// pressed Approve, through the same pump that pushes a branch.
//
// THE CEILING, and it is stated here because this is where somebody will look for it (known-gaps HIL-P6):
// this tree opens DRAFT pull requests only (§30.8), and GitHub does not merge a draft until a human marks
// it ready for review. So an approved merge of a pull request Palai itself opened arrives at the documented
// 405 and becomes a visible warning — one human gate more than the button, provided by GitHub. Nothing
// here waits for CI, requests a review, or asks about required checks; every refusal is the provider's and
// comes back honestly rather than being pre-empted by a weaker copy of a rule it already enforces.
func MergeTool() toolbroker.Tool {
	return toolbroker.Tool{
		Name: "palai.publish.merge_pull_request",
		Description: "Request that the pull request this run opened be merged. The request is recorded for approval " +
			"and nothing merges until approved; which pull request, at which commit, and by which merge method are all " +
			"resolved from the run's own publication and its binding, not model-supplied.",
		ReplayClass: toolbroker.ClassIdempotent, // records a pending publication under a stable idempotency key (§26.6)
		InputSchema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{},
			"additionalProperties": false,
		},
		OutputSchema: map[string]any{"type": "object"},
		Exec:         mergeExec,
	}
}

// mergeExec records the pending merge. It reads NOTHING from args — not even to ignore it — and needs no
// workspace: what is merged lives at the provider, not in this attempt's checkout, so the tool is callable
// from a woken attempt whose workspace was rebuilt.
func mergeExec(ctx context.Context, env toolbroker.ExecEnv, _ map[string]any) (map[string]any, error) {
	if env.Publications == nil {
		return nil, fmt.Errorf("merge tool: no publication registry wired for this run")
	}
	return env.Publications.RequestPublication(ctx, env.Scope, map[string]any{"operation": "merge_pull_request"})
}
