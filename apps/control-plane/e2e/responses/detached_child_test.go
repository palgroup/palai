//go:build e2e

package responses

import (
	"context"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/packages/coordinator/recovery"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// newDetachOrchestrator builds a kernel with a checkpoint sink wired (detach REQUIRES a durable
// boundary, §26.5) over an in-memory checkpoint store, so the E10 T8 detached flow can release the
// parent and restore it. The store lets a test prove a checkpoint was persisted at the release.
func (h *harness) newDetachOrchestrator(store *memCheckpointStore, adapter modelbroker.ModelAdapter) *execution.Orchestrator {
	orch := h.newOrchestratorWithAdapter(subprocessDialer{engineDir: h.engineDir}, adapter)
	orch.SetCheckpointSink(h.checkpointSink(store))
	return orch
}

// runDetachedDelegation drives the full DET-001 cycle through the real worker: a parent with one
// DETACHED config-seeded delegation releases at the awaiting-children boundary; the worker then claims
// the child job, runs it, and the child terminal wakes the parent, which the worker claims, restores,
// rebinds the existing child, folds its result, and completes. It returns the ids for slice assertions.
func (h *harness) runDetachedDelegation(t *testing.T) (store *memCheckpointStore, respID, sessionID, runID string) {
	t.Helper()
	store = newMemCheckpointStore()
	stop := h.runWorker(h.newDetachOrchestrator(store, finalOnlyProvider{}))
	t.Cleanup(stop)
	respID, sessionID, runID = h.admitWith(
		`{"input":"do it","delegations":[{"role":"r","objective":"o","model":"fake-child","required":true,"detach":true}]}`, newID("idem"))
	h.awaitResponseState(respID, "completed", 120*time.Second)
	return store, respID, sessionID, runID
}

// TestParentReleasesComputeWhileChildRuns proves DET-001 (spec §25.18-19, §26.5): a detached child
// runs as a durable job while the parent RELEASES its compute — the parent attempt ends at a persisted
// checkpoint (run.waiting), the child runs on its own model, and the parent later RESTORES (ladder rung
// 2) to fold the typed result and complete. No inline hold: the release is observable as run.waiting +
// a durable checkpoint + a compatible-checkpoint restore rung.
func TestParentReleasesComputeWhileChildRuns(t *testing.T) {
	h := newHarness(t)
	store, respID, sessionID, runID := h.runDetachedDelegation(t)

	// The child ran as its OWN run, on its own routed model, to completion.
	childRun, childResp := h.childRunOf(runID)
	if state := h.runState(childRun); state != "completed" {
		t.Fatalf("child run state = %q, want completed", state)
	}
	if got := h.modelOfRun(childResp); got != "fake-child" {
		t.Fatalf("child model = %q, want fake-child (its own routed model)", got)
	}

	// The parent RELEASED its compute: run.waiting.v1 journaled and a durable checkpoint persisted.
	if n := h.count(`SELECT count(*) FROM events WHERE response_id=$1 AND type='run.waiting.v1'`, respID); n != 1 {
		t.Fatalf("parent run.waiting.v1 = %d, want 1 (the release)", n)
	}
	if store.objectCount() == 0 {
		t.Fatal("no checkpoint persisted at the release — the parent could not have released safely (§26.5)")
	}
	// The parent RESUMED via the recovery ladder's compatible-checkpoint rung — proving it restored a
	// fresh process rather than holding the engine inline while the child ran.
	if !contains(h.recoveryEventLevels(sessionID), string(recovery.LevelCompatibleCheckpoint)) {
		t.Fatalf("no compatible_checkpoint restore rung; levels = %v (the parent did not release+restore)", h.recoveryEventLevels(sessionID))
	}
	// The parent's terminal projection links the child run (spec §25.19).
	if links := h.childRunsLink(respID); len(links) != 1 || links[0] != childRun {
		t.Fatalf("parent projection child_runs = %v, want [%s]", links, childRun)
	}
}

// TestReemittedChildRequestNotRespawned proves the DET-001 keystone (spec §25.18): the parent restore
// re-emits the SAME deterministic child.request, and the controller REBINDS the existing child rather
// than cloning it. Across the release → child → wake → restore → rebind cycle, EXACTLY ONE child run
// exists for the parent, and the typed result folds exactly once.
func TestReemittedChildRequestNotRespawned(t *testing.T) {
	h := newHarness(t)
	_, respID, _, runID := h.runDetachedDelegation(t)

	// The re-emit is rebound, never cloned: exactly one child run for this parent.
	if n := h.count(`SELECT count(*) FROM runs WHERE parent_run_id=$1`, runID); n != 1 {
		t.Fatalf("child runs for the parent = %d, want 1 (the re-emitted child.request must REBIND, not clone)", n)
	}
	// The child.requested.v1 (spawn) is journaled once and the child.completed.v1 (fold) exactly once —
	// even though the parent restored and re-emitted the child.request.
	if n := h.count(`SELECT count(*) FROM events WHERE response_id=$1 AND type='child.requested.v1'`, respID); n != 1 {
		t.Fatalf("child.requested.v1 = %d, want 1", n)
	}
	if n := h.count(`SELECT count(*) FROM events WHERE response_id=$1 AND type='child.completed.v1'`, respID); n != 1 {
		t.Fatalf("child.completed.v1 = %d, want 1 (the fold is exactly-once across the restore)", n)
	}
}

// TestChildTerminalWakesParentExactlyOnce proves the wake is single-winner (DET-001): the parent is
// re-entered from waiting EXACTLY once. run.running.v1 appears exactly twice on the parent's response —
// the initial start and the one wake — so a doubled/redelivered child terminal never wakes it twice.
func TestChildTerminalWakesParentExactlyOnce(t *testing.T) {
	h := newHarness(t)
	_, respID, _, _ := h.runDetachedDelegation(t)

	// run.running.v1: one at the initial provisioning→running, one at the single waiting→running wake.
	if n := h.count(`SELECT count(*) FROM events WHERE response_id=$1 AND type='run.running.v1'`, respID); n != 2 {
		t.Fatalf("parent run.running.v1 = %d, want 2 (initial start + exactly one wake)", n)
	}
}

// TestChildEventReachesParentResumeHistory proves the child→parent lifecycle events land in the
// parent's journal in canonical sequence order across the release/restore (spec §25.19, DET-002): the
// spawn (child.requested.v1) precedes the fold (child.completed.v1), both under the parent response,
// and the parent's own model steps stay intact — no child event injected into a reconstructed step.
func TestChildEventReachesParentResumeHistory(t *testing.T) {
	h := newHarness(t)
	_, respID, _, _ := h.runDetachedDelegation(t)

	var requestedSeq, completedSeq int
	if err := h.spine.Pool().QueryRow(context.Background(),
		`SELECT seq FROM events WHERE response_id=$1 AND type='child.requested.v1'`, respID).Scan(&requestedSeq); err != nil {
		t.Fatalf("read child.requested.v1 seq error = %v", err)
	}
	if err := h.spine.Pool().QueryRow(context.Background(),
		`SELECT seq FROM events WHERE response_id=$1 AND type='child.completed.v1'`, respID).Scan(&completedSeq); err != nil {
		t.Fatalf("read child.completed.v1 seq error = %v", err)
	}
	if !(requestedSeq < completedSeq) {
		t.Fatalf("child lifecycle out of order: requested seq=%d, completed seq=%d (want requested < completed)", requestedSeq, completedSeq)
	}
	// The parent still ran its own model steps (its first step delegated, its final step folded the
	// child result), scoped to its own response — the child's steps never leaked in.
	if n := h.count(`SELECT count(*) FROM events WHERE response_id=$1 AND type='model_step.created.v1'`, respID); n < 2 {
		t.Fatalf("parent model steps = %d, want >=2 (delegate step + fold step)", n)
	}
}
