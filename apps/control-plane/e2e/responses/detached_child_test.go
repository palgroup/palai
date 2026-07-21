//go:build e2e

package responses

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/packages/contracts"
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

// detachGatedChildProvider drives the DET-002 conversation deterministically: the parent runs a single
// final step (a config-seeded delegation dispatches the detached child), and the child runs TWO steps —
// its first GATES (blocks until the test has durably queued a spine message to it) and requests a tool
// so there is a boundary to fold at; its second, seeing the queued message folded into context, echoes
// the marker and finishes. The gate makes the fold-order deterministic — no wall-clock race.
type detachGatedChildProvider struct {
	childModel string
	marker     string
	started    chan struct{}
	once       sync.Once
	release    chan struct{}
	mu         sync.Mutex
	childFolds int
}

func newDetachGatedChildProvider(childModel, marker string) *detachGatedChildProvider {
	return &detachGatedChildProvider{childModel: childModel, marker: marker, started: make(chan struct{}), release: make(chan struct{})}
}

func (p *detachGatedChildProvider) Execute(ctx context.Context, req modelbroker.Request, _ string, _ func(modelbroker.Delta)) (modelbroker.Result, error) {
	res := modelbroker.Result{
		ModelRequestID: req.ModelRequestID, Model: req.Model,
		Usage: contracts.Usage{InputTokens: 5, OutputTokens: 3, TotalTokens: 8}, Attempts: 1,
	}
	if req.Model != p.childModel {
		// The parent: a single final step. The config-seeded delegation (with detach) dispatches the child.
		res.ProviderRequestID = "prov_parent"
		res.Output = "parent done"
		res.FinishReason = "stop"
		return res, nil
	}
	sawMarker := false
	for _, m := range req.Messages {
		if m.Role == "user" && strings.Contains(m.Content, p.marker) {
			sawMarker = true
		}
	}
	if sawMarker {
		p.mu.Lock()
		p.childFolds++
		p.mu.Unlock()
		res.ProviderRequestID = "prov_child_final"
		res.Output = "child folded [" + p.marker + "]"
		res.FinishReason = "stop"
		return res, nil
	}
	// The child's first step: signal it is running (idle at its own boundary), block until the test has
	// durably queued the spine message, then request a tool so the fold happens on the resumed step.
	p.once.Do(func() { close(p.started) })
	select {
	case <-p.release:
	case <-ctx.Done():
		return modelbroker.Result{}, ctx.Err()
	}
	res.ProviderRequestID = "prov_child_tool"
	res.ToolCalls = []modelbroker.ToolCall{{ID: "call_add", Name: "palai.conformance.math.add", Arguments: `{"a":7,"b":5}`}}
	res.FinishReason = "tool_calls"
	return res, nil
}

func (p *detachGatedChildProvider) folds() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.childFolds
}

// TestDetachedChildIdleReceivesSpineMessage proves DET-002 (spec §25.18-19, E10 T8): while a detached
// child runs (its parent released, WAITING), the child is the session's live run, so a send_message
// accepted on the parent's session reaches the CHILD, which folds it EXACTLY ONCE at its next boundary —
// the existing send_message spine + delivered_messages (E10 T2), applied run-generically to the child.
func TestDetachedChildIdleReceivesSpineMessage(t *testing.T) {
	h := newHarness(t)
	store := newMemCheckpointStore()
	marker := "spine-to-child-" + newID("mark")
	gp := newDetachGatedChildProvider("fake-child", marker)
	stop := h.runWorker(h.newDetachOrchestrator(store, gp))
	defer stop()

	respID, sessionID, runID := h.admitWith(
		`{"input":"do it","delegations":[{"role":"r","objective":"o","model":"fake-child","required":true,"detach":true}]}`, newID("idem"))

	// Wait for the child to be running and idle at its own first boundary.
	select {
	case <-gp.started:
	case <-time.After(60 * time.Second):
		t.Fatal("detached child never started its first model step")
	}
	// The child is now the session's live run (the parent is WAITING). Address it through the parent's
	// session — no new command kind, no distinct child address.
	childRun, _ := h.childRunOf(runID)
	commandID := newID("cmd")
	cmd := h.submitCommand(sessionID, `{"command_id":"`+commandID+`","kind":"send_message","delivery":"queue","message":"`+marker+`"}`)
	if cmd.Status != "queued" {
		t.Fatalf("send_message status = %q, want queued (the live detached child accepts it)", cmd.Status)
	}
	// The command bound to the CHILD run, not the waiting parent.
	if n := h.count(`SELECT count(*) FROM commands WHERE id=$1 AND run_id=$2`, commandID, childRun); n != 1 {
		t.Fatalf("send_message did not bind to the detached child run %s", childRun)
	}
	close(gp.release) // let the child continue: it folds the queued message on its resumed step

	h.awaitResponseState(respID, "completed", 120*time.Second)

	// The child folded the spine message EXACTLY once (the delivered_messages exactly-once, T2, applied
	// run-generically to the child), and the command settled applied against the child run.
	if got := gp.folds(); got != 1 {
		t.Fatalf("child folded the spine message %d times, want exactly 1", got)
	}
	if n := h.count(`SELECT count(*) FROM commands WHERE id=$1 AND state='applied' AND run_id=$2`, commandID, childRun); n != 1 {
		t.Fatalf("send_message did not settle as exactly one applied delivery to the child run")
	}
	if n := h.count(`SELECT count(*) FROM delivered_messages WHERE command_id=$1 AND run_id=$2`, commandID, childRun); n != 1 {
		t.Fatalf("no durable delivered-message row for the spine message on the child run")
	}
}
