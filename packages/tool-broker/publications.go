package toolbroker

import "context"

// This file is the publication seam behind the in-process broker (spec §30.8, §22.4). A side-effect
// tool (push branch / open pull request) does NOT act — it records a pending publication + approval
// through this seam and returns "pending_approval" to the model. The concrete DB-backed registry (which
// resolves the run's binding, forms the idempotency key + one-shot request hash, and records the
// pending row) lives in the control plane and is injected per attempt through ExecEnv.

// PublicationRegistry records a side-effect operation awaiting approval and returns the pending-approval
// result the model sees (spec §30.8-30.10). The op map carries the operation ("push_branch" /
// "open_pull_request"), the workspace head the tool computed, and any model-proposed title/body; the
// registry resolves the destination from the run's binding — the model never supplies a remote. A nil
// registry makes a publication tool fail cleanly rather than act.
type PublicationRegistry interface {
	RequestPublication(ctx context.Context, scope TaskScope, op map[string]any) (map[string]any, error)
}
