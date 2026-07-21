//go:build e2e

package responses

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/packages/coordinator/recovery"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// mustJSON marshals v for a byte-stable projection comparison.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal projection: %v", err)
	}
	return string(b)
}

// killableDialer wraps the real subprocess dialer and records every engine process it dials, so a
// test can send a REAL SIGKILL to the live engine mid-loop (ENG-004) rather than a graceful,
// injected error. It is the fault-class kill the recovery ladder must survive: an abrupt process
// death that EOFs the channel, not a clean error return.
type killableDialer struct {
	inner  subprocessDialer
	mu     sync.Mutex
	procs  []*os.Process
	killed int
}

func (d *killableDialer) Dial(ctx context.Context, a execution.AttemptDescriptor) (execution.EngineChannel, error) {
	ch, err := d.inner.Dial(ctx, a)
	if err != nil {
		return nil, err
	}
	if sc, ok := ch.(*subprocessChannel); ok && sc.cmd != nil && sc.cmd.Process != nil {
		d.mu.Lock()
		d.procs = append(d.procs, sc.cmd.Process)
		d.mu.Unlock()
	}
	return ch, err
}

// killLatest SIGKILLs the most recently dialed engine process — the live attempt's engine — and
// counts the kill, so the test can prove the SIGKILL actually fired (not a silent no-op that would
// let ENG-004 green off the provider error alone).
func (d *killableDialer) killLatest() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.procs) == 0 {
		return
	}
	if err := d.procs[len(d.procs)-1].Kill(); err == nil {
		d.killed++
	}
}

func (d *killableDialer) dialed() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.procs)
}

func (d *killableDialer) killCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.killed
}

// killThenFinishProvider drives one tool step (recovery.count) then a final answer, but the FIRST
// time it reaches the second (post-tool) step it SIGKILLs the live engine — a real mid-loop process
// kill AFTER the tool boundary's checkpoint was persisted — and returns an error so attempt-1 ends
// deterministically. The next attempt, restored past the tool via the ladder, reaches the second
// step live and finishes.
type killThenFinishProvider struct {
	mu        sync.Mutex
	toolSteps int
	kill      func()
}

func (p *killThenFinishProvider) Execute(_ context.Context, req modelbroker.Request, _ string, _ func(modelbroker.Delta)) (modelbroker.Result, error) {
	sawTool := false
	for _, m := range req.Messages {
		if m.Role == "tool" {
			sawTool = true
		}
	}
	res := modelbroker.Result{ModelRequestID: req.ModelRequestID, Model: "fake", Usage: contracts.Usage{InputTokens: 5, OutputTokens: 3, TotalTokens: 8}, Attempts: 1}
	if !sawTool {
		res.ProviderRequestID = "prov_tool"
		res.ToolCalls = []modelbroker.ToolCall{{ID: "call_k", Name: "recovery.count", Arguments: "{}"}}
		res.FinishReason = "tool_calls"
		return res, nil
	}
	p.mu.Lock()
	p.toolSteps++
	n := p.toolSteps
	p.mu.Unlock()
	if n == 1 {
		p.kill() // the checkpoint is persisted; SIGKILL the live engine now
		return modelbroker.Result{}, errRecoveryCrash
	}
	res.ProviderRequestID = "prov_final"
	res.Output = "12"
	res.FinishReason = "stop"
	return res, nil
}

// TestEngineProcessKillRecoversViaLadder proves ENG-004 (spec §26.8, §26.3): a REAL SIGKILL of the
// engine process mid-tool-loop — after the tool boundary's checkpoint is durable — is recovered by
// the T4 ladder. The next attempt restores from the checkpoint, the completed tool is NOT replayed,
// the run finishes, and a complete §26.12 RecoveryProof is journaled.
func TestEngineProcessKillRecoversViaLadder(t *testing.T) {
	h := newHarness(t)
	store := newMemCheckpointStore()
	tool := &countingTool{}
	dialer := &killableDialer{inner: subprocessDialer{engineDir: h.engineDir}}
	provider := &killThenFinishProvider{kill: dialer.killLatest}
	orch := h.newOrchestratorWithTools(dialer, provider, tool.tool())
	orch.SetCheckpointSink(h.checkpointSink(store))

	respID, sessionID, runID := h.admit()

	// attempt-1: reaches the tool boundary (persisting a checkpoint), then the engine is SIGKILLed.
	if err := orch.ExecuteAttempt(context.Background(), h.descriptor(runID, 1)); err == nil {
		t.Fatal("attempt-1 must fail after the engine process is killed")
	}
	// Prove the SIGKILL actually fired against a real engine — otherwise ENG-004 would green off the
	// provider error alone with no kill, a false proof.
	if dialer.dialed() == 0 {
		t.Fatal("attempt-1 dialed no engine process: the kill had no target")
	}
	if dialer.killCount() == 0 {
		t.Fatal("attempt-1 failed WITHOUT the SIGKILL firing: the recovery would not be exercising a real kill")
	}
	if store.objectCount() == 0 {
		t.Fatal("no checkpoint persisted before the process kill: the ladder has nothing to restore")
	}
	if tool.runs() != 1 {
		t.Fatalf("tool ran %d times on attempt-1, want 1", tool.runs())
	}

	// attempt-2: the ladder restores from the checkpoint and resumes past the completed tool.
	if err := orch.ExecuteAttempt(context.Background(), h.descriptor(runID, 2)); err != nil {
		t.Fatalf("attempt-2 (ladder restore after kill) error = %v", err)
	}
	if tool.runs() != 1 {
		t.Fatalf("tool ran %d times total, want 1 (a completed tool must NOT replay after a kill+restore)", tool.runs())
	}
	if !contains(h.recoveryEventLevels(sessionID), string(recovery.LevelCompatibleCheckpoint)) {
		t.Fatalf("no compatible_checkpoint rung after process kill; levels = %v", h.recoveryEventLevels(sessionID))
	}
	proof, ok := h.recoveryProof(sessionID)
	if !ok || !proof.Complete() {
		t.Fatalf("recovery proof missing/incomplete after process kill: %+v (ok=%v)", proof, ok)
	}
	if got, _ := h.response(respID); got != "completed" {
		t.Fatalf("run state after kill+restore = %q, want completed", got)
	}
}

// terminalLessDialer returns a fresh channel on every Dial whose engine hands back engine.ready and
// one output item, then EOFs WITHOUT ever emitting run.terminal — a terminal-less exit that must
// fail every attempt identically (never a false success).
type terminalLessDialer struct{ runID string }

func (d terminalLessDialer) Dial(_ context.Context, _ execution.AttemptDescriptor) (execution.EngineChannel, error) {
	return &scriptedChannel{frames: []contracts.EngineFrame{
		scriptFrame("engine.ready", d.runID, 1, map[string]any{
			"selected_protocol": "engine.v1",
			"engine":            map[string]any{"name": "fake", "version": "0"},
			"max_frame_bytes":   1024, "nonce": "n",
		}),
		scriptFrame("output.item", d.runID, 2, map[string]any{"type": "message", "content": "partial"}),
	}}, nil
}

// TestExitWithoutTerminalNeverFalseSuccess proves ENG-014 (spec §26.8): an engine that exits — the
// channel EOFs — WITHOUT ever emitting run.terminal must NEVER read as a completed run. Every
// attempt fails the same way; with no checkpoint to restore, the durable job dead-letters and the
// reconciler bridge drives the run to an explicit failed terminal — never a silent success.
func TestExitWithoutTerminalNeverFalseSuccess(t *testing.T) {
	h := newHarness(t)
	respID, sessionID, runID := h.admit()

	stop := h.runWorkerWithRetry(
		h.newOrchestrator(terminalLessDialer{runID: runID}),
		coordinator.RetryPolicy{MaxAttempts: 3, BaseBackoff: 5 * time.Millisecond, MaxBackoff: 20 * time.Millisecond},
	)
	h.awaitJobStatus(runID, "dead", 30*time.Second)
	stop()

	// Before the bridge the run is hung — but crucially NEVER completed.
	if got, _ := h.response(respID); got == "completed" {
		t.Fatal("a terminal-less engine exit produced a completed run: false success")
	}

	// The reconciler sweep drives the dead-lettered run to an explicit failed terminal.
	rec := execution.NewReconciler(h.spine, time.Hour, 3)
	if _, err := rec.Sweep(context.Background()); err != nil {
		t.Fatalf("reconciler Sweep error = %v", err)
	}
	if state := h.runState(runID); state != "failed" {
		t.Fatalf("run state after sweep = %q, want failed (explicit failure, never a silent success)", state)
	}
	events := h.events(sessionID)
	if len(events) == 0 || events[len(events)-1].typ != "run.failed.v1" {
		t.Fatalf("last journaled event = %+v, want run.failed.v1 terminal", events)
	}
}

// TestRedeliveredTerminalStaysSingleByMonotonicity proves ENG-013 (spec §26.8, §22.3): once a run's
// terminal is persisted, a crash + redelivery of the same run's job (a duplicate terminal) is
// rejected by the finalize MONOTONICITY guard (first-terminal-write-wins by state) — the run keeps
// EXACTLY ONE terminal event and its projection is byte-unchanged. This is the durable half of the
// tail-frame fix: the OCI drain guarantees the terminal is delivered once, and finalize guarantees a
// redelivery never doubles it.
func TestRedeliveredTerminalStaysSingleByMonotonicity(t *testing.T) {
	h := newHarness(t)
	respID, sessionID, runID := h.admit()

	terminal := func() []contracts.EngineFrame {
		return []contracts.EngineFrame{
			scriptFrame("engine.ready", runID, 1, map[string]any{
				"selected_protocol": "engine.v1",
				"engine":            map[string]any{"name": "fake", "version": "0"},
				"max_frame_bytes":   1024, "nonce": "n",
			}),
			scriptFrame("output.item", runID, 2, map[string]any{"type": "message", "content": "done"}),
			scriptFrame("run.terminal", runID, 3, map[string]any{"outcome": "completed", "output": "done"}),
		}
	}

	// attempt-1: run reaches its persisted terminal.
	orch1 := h.newOrchestrator(scriptedDialer{&scriptedChannel{frames: terminal()}})
	if err := orch1.ExecuteAttempt(context.Background(), h.descriptor(runID, 1)); err != nil {
		t.Fatalf("attempt-1 error = %v", err)
	}
	if got, _ := h.response(respID); got != "completed" {
		t.Fatalf("run state after attempt-1 = %q, want completed", got)
	}
	_, projectionBefore := h.response(respID)
	terminalsBefore := h.count(`SELECT count(*) FROM events WHERE session_id=$1 AND organization_id=$2 AND project_id=$3 AND type='run.completed.v1'`,
		sessionID, h.tenant.Organization, h.tenant.Project)
	if terminalsBefore != 1 {
		t.Fatalf("run.terminal events after attempt-1 = %d, want 1", terminalsBefore)
	}

	// A crash + job redelivery: a second attempt on the same run replays the same terminal frame.
	// The finalize monotonicity guard (ApplyRunTransition -> ErrRunTerminal) absorbs it cleanly.
	orch2 := h.newOrchestrator(scriptedDialer{&scriptedChannel{frames: terminal()}})
	if err := orch2.ExecuteAttempt(context.Background(), h.descriptor(runID, 2)); err != nil {
		t.Fatalf("redelivered terminal must be absorbed cleanly (finalize monotonicity), got %v", err)
	}

	terminalsAfter := h.count(`SELECT count(*) FROM events WHERE session_id=$1 AND organization_id=$2 AND project_id=$3 AND type='run.completed.v1'`,
		sessionID, h.tenant.Organization, h.tenant.Project)
	if terminalsAfter != 1 {
		t.Fatalf("run.terminal events after redelivery = %d, want 1 (a redelivered terminal must not double under the fence)", terminalsAfter)
	}
	_, projectionAfter := h.response(respID)
	if before, after := mustJSON(t, projectionBefore.Output), mustJSON(t, projectionAfter.Output); before != after {
		t.Fatalf("projection output changed after redelivery: %s -> %s", before, after)
	}
}
