package tools

import (
	"context"
	"fmt"

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
	// BOTH REFUSALS ARE ANSWERS, and neither was until 2026-08-04. They are the case answer.go names
	// under AnswerUnavailable word for word — "the tool is not wired on this deployment (no shell
	// posture, no workspace bound, no registry)" — and this tool returned a bare error for both, so an
	// attempt that reached either one died with its tool_call row still `executing`, was classified
	// uncertain by the next attempt's ledger consult, and spent the retry ladder reproducing a refusal
	// that cannot change. Nothing has happened here: no publication is recorded, no ref moves.
	//
	// The configuration is not hypothetical — it is the one `palai up` warns about in so many words:
	// "a run carrying this binding still ANSWERS \"no workspace bound for this run\" and cannot clone,
	// edit or commit" (cmd/cli/internal/stack/up.go:765). That sentence was true of file/shell/glob/
	// grep/text_editor and false of exactly the three tools it names.
	if env.Publications == nil {
		return nil, toolbroker.Answerf(toolbroker.AnswerUnavailable,
			"push tool: no publication registry wired for this run")
	}
	if env.WorkspaceRoot == "" || env.Workspace == nil {
		return nil, toolbroker.Answerf(toolbroker.AnswerUnavailable,
			"push tool: no workspace bound for this run")
	}
	// The head is read WHERE THE REPOSITORY IS (A.3 T5). It is the one thing this tool touches: the
	// push itself is still a gated side effect that happens after an approval, at a boundary, through
	// the approval pump, to a destination resolved from the binding.
	head, _, err := env.Workspace.Head(ctx)
	if err != nil {
		return nil, fmt.Errorf("push tool: read workspace head: %w", err)
	}
	return env.Publications.RequestPublication(ctx, env.Scope, map[string]any{
		"operation": "push_branch",
		"head_sha":  head,
	})
}
