package execution

import (
	"context"
	"errors"
	"fmt"

	statemachines "github.com/palgroup/palai/packages/state-machines"
)

// errRunReleased signals that a detached child was dispatched at this boundary and the parent RELEASED
// its compute (spec §25.18-19, §26.5, E10 T8 DET-001): the run is now waiting, the attempt ends cleanly
// (no engine process held), and a fresh attempt resumes it when the child wakes it. Like errRunPaused it
// is not a failure — ExecuteAttempt returns nil on it, freeing the worker even in a single-runner stack.
var errRunReleased = errors.New("run_released")

// releaseParentForDetach releases a parent's compute at the awaiting-children boundary after a detached
// child was enqueued (spec §26.5, E10 T8): it mirrors the pause choreography — capture a durable
// checkpoint of THIS boundary (so resume RESTORES and re-emits the pending child.request, ladder rung 2),
// then drive the run running→waiting, then hand back errRunReleased so the attempt ends. §26.5 forbids a
// release with no recoverable boundary, so a checkpoint-persist failure fails the attempt (retry) rather
// than releasing blind — the caller only reaches here with a checkpoint sink wired.
//
// The post-release self-wake closes the child-finished-before-release race: if the child already
// terminated while we were checkpointing, its own wake found the parent still running and no-op'd, so
// the parent would wait forever. WakeDetachedParent is single-winner, so a genuine terminal wake racing
// this self-wake still re-enters the parent exactly once.
func (o *Orchestrator) releaseParentForDetach(ctx context.Context, st *attemptState) error {
	// §26.5: never release without a durable boundary. The fresh detach path already gates on the sink,
	// but the rebind path routes here too (MF-2) — so the single choke point refuses a sink-less release
	// rather than letting checkpointBeforePause no-op and the parent wait on an unrecoverable boundary.
	if o.checkpoints == nil {
		return fmt.Errorf("cannot release a detached parent with no checkpoint sink (§26.5): no durable boundary")
	}
	// Checkpoint the awaiting-children boundary and drain any in-flight child.requests without dispatch
	// (the same discipline the pause path uses); a persist failure surfaces here.
	if err := o.checkpointBeforePause(ctx, st); err != nil {
		return err
	}
	if _, err := o.spine.ApplyRunTransition(ctx, st.tenant, string(st.attempt.RunID), statemachines.RunCmdWait); err != nil {
		return err
	}
	if _, err := o.spine.WakeDetachedParent(ctx, st.tenant, string(st.attempt.RunID)); err != nil {
		return err
	}
	return errRunReleased
}
