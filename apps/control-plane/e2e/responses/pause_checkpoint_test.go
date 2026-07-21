//go:build e2e

package responses

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator/recovery"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// pauseResumeProvider drives: step 1 (gated) requests a counting tool; step 2 requests a second
// (built-in) tool so the resumed run has a live pump boundary; step 3 finishes and records whether
// the pre-empted queued message folded in. Only step 1 gates, so a resume (which restores PAST step
// 1) never blocks. It backs the SES-009 round-trip: pause captures a checkpoint at step 1's tool
// boundary; resume restores it, runs the drained tool for the first time, and folds the message.
type pauseResumeProvider struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once

	mu           sync.Mutex
	sawFoldFinal bool
}

func newPauseResumeProvider() *pauseResumeProvider {
	return &pauseResumeProvider{started: make(chan struct{}), release: make(chan struct{})}
}

func (p *pauseResumeProvider) Execute(ctx context.Context, req modelbroker.Request, _ string, _ func(modelbroker.Delta)) (modelbroker.Result, error) {
	toolResults := 0
	for _, m := range req.Messages {
		if m.Role == "tool" {
			toolResults++
		}
	}
	res := modelbroker.Result{ModelRequestID: req.ModelRequestID, Model: "fake", Usage: contracts.Usage{InputTokens: 5, OutputTokens: 3, TotalTokens: 8}, Attempts: 1}
	switch {
	case toolResults == 0: // step 1: gate, then request the counting tool
		p.once.Do(func() { close(p.started) })
		select {
		case <-p.release:
		case <-ctx.Done():
			return modelbroker.Result{}, ctx.Err()
		}
		res.ProviderRequestID = "prov_tool1"
		res.ToolCalls = []modelbroker.ToolCall{{ID: "c1", Name: "recovery.count", Arguments: "{}"}}
		res.FinishReason = "tool_calls"
		return res, nil
	case toolResults == 1: // step 2: continue with the built-in add tool -> a live pump boundary
		res.ProviderRequestID = "prov_tool2"
		res.ToolCalls = []modelbroker.ToolCall{{ID: "c2", Name: "palai.conformance.math.add", Arguments: `{"a":7,"b":5}`}}
		res.FinishReason = "tool_calls"
		return res, nil
	default: // step 3: final; record whether the queued message folded into the resumed run
		p.mu.Lock()
		for _, m := range req.Messages {
			if strings.Contains(m.Content, "PAUSE-FOLD") {
				p.sawFoldFinal = true
			}
		}
		p.mu.Unlock()
		res.ProviderRequestID = "prov_final"
		res.Output = "done"
		res.FinishReason = "stop"
		return res, nil
	}
}

func (p *pauseResumeProvider) foldedMessage() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sawFoldFinal
}

type pauseFixture struct {
	stop      func()
	provider  *pauseResumeProvider
	tool      *countingTool
	rec       *deliverRecorder
	store     *memCheckpointStore
	respID    string
	sessionID string
	runID     string
	msgID     string
	pauseID   string
}

// pauseAtToolBoundaryWithSink runs the gated-provider flow with a checkpoint sink WIRED up to the
// pause pre-emption: it queues a message, pauses at step 1's tool boundary, and returns once the run
// is waiting with its compute released. Shared by the create and validity halves of SES-009. The
// worker keeps running (stop tears it down) so the validity test can resume the same run.
func (h *harness) pauseAtToolBoundaryWithSink() pauseFixture {
	h.t.Helper()
	f := pauseFixture{provider: newPauseResumeProvider(), tool: &countingTool{}, rec: &deliverRecorder{}, store: newMemCheckpointStore()}
	dialer := subprocessDialer{engineDir: h.engineDir, onSend: f.rec.onSend}
	orch := h.newOrchestratorWithTools(dialer, f.provider, f.tool.tool())
	orch.SetCheckpointSink(h.checkpointSink(f.store))
	f.stop = h.runWorker(orch)

	f.respID, f.sessionID, f.runID = h.admit()
	select {
	case <-f.provider.started:
	case <-time.After(30 * time.Second):
		h.t.Fatal("first model step never started")
	}
	f.msgID = newID("cmd")
	if cmd := h.submitCommand(f.sessionID, `{"command_id":"`+f.msgID+`","kind":"send_message","delivery":"queue","message":"PAUSE-FOLD"}`); cmd.Status != "queued" {
		h.t.Fatalf("send_message status = %q, want queued", cmd.Status)
	}
	f.pauseID = newID("cmd")
	if cmd := h.submitCommand(f.sessionID, `{"command_id":"`+f.pauseID+`","kind":"pause"}`); cmd.Status != "queued" {
		h.t.Fatalf("pause status = %q, want queued", cmd.Status)
	}
	close(f.provider.release)
	h.awaitRunState(f.runID, "waiting", 30*time.Second)
	h.awaitJobStatus(f.runID, "completed", 30*time.Second)
	return f
}

// TestPauseProducesValidCheckpointBeforeComputeRelease proves the SES-009 CREATE half (spec §26.5):
// on a pause, the controller captures a valid checkpoint of the pause boundary BEFORE releasing
// compute. The engine's in-flight tool.request is drain-received (admitted + seq-tracked) but NOT
// dispatched — the tool never runs, no tool_call is committed — and the checkpoint (row + bytes +
// checksum) is persisted at a journal boundary that precedes the pause event.
func TestPauseProducesValidCheckpointBeforeComputeRelease(t *testing.T) {
	h := newHarness(t)
	f := h.pauseAtToolBoundaryWithSink()
	defer f.stop()

	// A valid checkpoint persisted: bytes in the store + an immutable row with a checksum.
	if f.store.objectCount() != 1 {
		t.Fatalf("checkpoint objects = %d, want 1 (a checkpoint persisted at the pause boundary)", f.store.objectCount())
	}
	cp, found, err := h.spine.LatestRunCheckpoint(context.Background(), h.tenant, f.runID)
	if err != nil || !found {
		t.Fatalf("LatestRunCheckpoint found=%v err=%v, want a persisted checkpoint", found, err)
	}
	if cp.ContentChecksum == "" || cp.ObjectKey == "" {
		t.Fatalf("checkpoint row missing integrity fields: checksum=%q key=%q", cp.ContentChecksum, cp.ObjectKey)
	}

	// The in-flight tool.request was DRAINED, not dispatched: the tool never ran, no tool_call row.
	if f.tool.runs() != 0 {
		t.Fatalf("tool ran %d times during the pause drain, want 0 (drained, not dispatched)", f.tool.runs())
	}
	if n := h.count(`SELECT count(*) FROM tool_calls WHERE run_id=$1 AND organization_id=$2 AND project_id=$3`, f.runID, h.tenant.Organization, h.tenant.Project); n != 0 {
		t.Fatalf("tool_calls committed during pause = %d, want 0 (the drained request never commits)", n)
	}

	// Ordering: the checkpoint's transcript boundary was cut BEFORE the pause event was journaled.
	_, pauseSeq := h.commandRow(f.pauseID)
	if pauseSeq == nil {
		t.Fatal("pause command has no applied_sequence")
	}
	if cp.TranscriptSequence >= *pauseSeq {
		t.Fatalf("checkpoint transcript seq %d is not < pause event seq %d (persist must precede PauseRun)", cp.TranscriptSequence, *pauseSeq)
	}

	// The queued message is pre-empted (still queued), and nothing was delivered before resume.
	if st, _ := h.commandRow(f.msgID); st != "queued" {
		t.Fatalf("queued message state = %q, want queued (pre-empted for resume)", st)
	}
	if count, _ := f.rec.snapshot(); count != 0 {
		t.Fatalf("message.deliver frames before resume = %d, want 0", count)
	}
}

// TestResumeRestoresFromValidCheckpoint proves the SES-009 VALIDITY half: resuming the paused run
// RESTORES from the checkpoint (ladder rung 2, run.restore) — NOT a transcript reconstruction. The
// engine re-derives the pending tool.request, so the drained tool runs for the FIRST time, the
// pre-empted message folds at a live boundary exactly once, and the proof is compatible_checkpoint.
func TestResumeRestoresFromValidCheckpoint(t *testing.T) {
	h := newHarness(t)
	f := h.pauseAtToolBoundaryWithSink()
	defer f.stop()

	if cmd := h.submitCommand(f.sessionID, `{"command_id":"`+newID("cmd")+`","kind":"resume"}`); cmd.Status != "applied" {
		t.Fatalf("resume status = %q, want applied", cmd.Status)
	}
	h.awaitResponseState(f.respID, "completed", 60*time.Second)

	// Restored from the checkpoint, never transcript-only.
	levels := h.recoveryEventLevels(f.sessionID)
	if !contains(levels, string(recovery.LevelCompatibleCheckpoint)) {
		t.Fatalf("resume did not restore from the checkpoint; levels = %v", levels)
	}
	if contains(levels, string(recovery.LevelTranscriptReconstruction)) {
		t.Fatalf("resume fell to transcript reconstruction instead of the valid checkpoint; levels = %v", levels)
	}

	// The engine re-derived the pending tool.request: the drained tool ran for the FIRST time.
	if f.tool.runs() != 1 {
		t.Fatalf("tool ran %d times, want 1 (first execution on resume, re-derived from the restored checkpoint)", f.tool.runs())
	}

	// The pre-empted message folded at a live boundary, exactly once, into the resumed run.
	if !f.provider.foldedMessage() {
		t.Fatal("the pre-empted queued message did not fold into the resumed run")
	}
	if count, last := f.rec.snapshot(); count != 1 || last != "PAUSE-FOLD" {
		t.Fatalf("message.deliver frames after resume = %d (last %q), want exactly 1 of PAUSE-FOLD", count, last)
	}
	if st, seq := h.commandRow(f.msgID); st != "applied" || seq == nil {
		t.Fatalf("queued message state after resume = %q seq=%v, want applied", st, seq)
	}

	proof, ok := h.recoveryProof(f.sessionID)
	if !ok || !proof.Complete() || proof.Level != recovery.LevelCompatibleCheckpoint {
		t.Fatalf("recovery proof missing/incomplete: %+v (ok=%v)", proof, ok)
	}
}
