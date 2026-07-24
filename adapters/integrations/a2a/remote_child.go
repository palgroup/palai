package a2a

// remote_child.go materializes SUB-007: a registered remote A2A agent acting as an external CHILD-RUN executor
// (the remote counterpart of an inline E08 ChildRun). RemoteChildRun is what the orchestrator's dispatchChild
// would call for a delegation targeting a remote agent instead of a local engine: it negotiates the card
// (version + extension allowlist), dispatches the child's OBJECTIVE as the minimum context, and folds the
// remote's UNTRUSTED reply into the child.result shape the engine folds.
//
// SECURITY (the crown RED-first asserts, A2A-005/SUB-007): the child receives the MINIMUM context (its
// objective only — no parent artifacts by default, per the data_policy 'minimum' pin) and NEVER inherits the
// parent credential: the sole outbound Authorization is the remote connection's OWN redeemed secret, which the
// Client resolves — there is no field or parameter through which a parent/platform token could reach it. The
// remote reply is UNTRUSTED tool-result data; a completed child yields a typed result the engine folds, any
// other terminal state a failure the parent treats per the delegation's required flag.
//
// HONEST CEILING (§6, E08): this is driven by FAKE-ENGINE runs — the engine opens no tool to a real provider,
// so the dispatch is deterministic. It is the materialized seam, NOT live-wired into orchestrator.dispatchChild
// (which needs a real remote peer = §6 leg 2). The capability stays "preview".

import "context"

// RemoteChildRequest is one delegated child dispatched to a remote agent (the E08 childSpec, remote flavor).
// ChildRequestID is the deterministic delegation id the engine minted (the parent journals + folds by it);
// RunID is the CANONICAL parent/child run the dispatch belongs to; Objective is the minimum context sent.
type RemoteChildRequest struct {
	ChildRequestID string
	RunID          string
	Objective      string
}

// RemoteChildResult is the child.result the engine folds (spec §25.19), remote flavor. Status is "completed"
// or "failed" (the same mapping an inline ChildRun uses); Output is the remote's UNTRUSTED text; TrustClass is
// fixed "untrusted"; RemoteTaskID is connection-scoped (meaningful only against the same agent's endpoint).
type RemoteChildResult struct {
	ChildRequestID string
	Status         string
	Output         string
	RemoteTaskID   string
	TrustClass     string
}

// RemoteChildRun dispatches a delegated child objective to a remote agent and folds the result. It negotiates
// the card FIRST (so a version-unsupported or extension-poisoned remote is refused before any objective is
// sent), then dispatches the objective as the minimum context, redeeming ONLY the remote connection's own
// credential (never the parent's). The returned Output is untrusted tool-result data.
func (c *Client) RemoteChildRun(ctx context.Context, agent RemoteAgent, req RemoteChildRequest) (RemoteChildResult, error) {
	if _, err := c.FetchCard(ctx, agent); err != nil {
		return RemoteChildResult{}, err
	}
	res, err := c.SendMessage(ctx, agent, RemoteRequest{RunID: req.RunID, Objective: req.Objective})
	if err != nil {
		return RemoteChildResult{}, err
	}
	return RemoteChildResult{
		ChildRequestID: req.ChildRequestID,
		Status:         childStatusFor(res.State),
		Output:         res.Output,
		RemoteTaskID:   res.RemoteTaskID,
		TrustClass:     trustUntrusted,
	}, nil
}

// childStatusFor maps a remote task's terminal state onto the child.result status the engine folds: a
// completed remote child is a typed result, any other outcome a non-completion the parent treats per required.
func childStatusFor(state TaskState) string {
	if state == TaskStateCompleted {
		return "completed"
	}
	return "failed"
}
