//go:build e2e

package responses

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/palgroup/palai/packages/contracts"
)

// TestLiveLoopCompletesOneResponseThroughSubprocessEngine drives the full kernel:
// admission → queued run job → orchestrator → live subprocess engine → one model
// request that asks for the add tool → committed tool result → a second model request
// that returns the final output → exactly one terminal → a committed terminal response
// with contiguous events and no duplicate model/tool dispatch.
func TestLiveLoopCompletesOneResponseThroughSubprocessEngine(t *testing.T) {
	h := newHarness(t)
	stop := h.runWorker(h.newOrchestrator(subprocessDialer{engineDir: h.engineDir}))
	defer stop()

	responseID, sessionID, runID := h.admit()
	h.awaitResponseState(responseID, "completed", 60*time.Second)

	// One transient session and one root run.
	if n := h.count(`SELECT count(*) FROM sessions WHERE organization_id=$1 AND project_id=$2`, h.tenant.Organization, h.tenant.Project); n != 1 {
		t.Fatalf("session count = %d, want 1", n)
	}
	if n := h.count(`SELECT count(*) FROM runs WHERE organization_id=$1 AND project_id=$2`, h.tenant.Organization, h.tenant.Project); n != 1 {
		t.Fatalf("run count = %d, want 1", n)
	}

	// Contiguous events with exactly one terminal.
	events := h.events(sessionID)
	assertContiguous(t, events)
	terminals := 0
	for _, e := range events {
		if e.typ == "run.completed.v1" {
			terminals++
		}
	}
	if terminals != 1 {
		t.Fatalf("terminal events = %d, want exactly 1: %+v", terminals, events)
	}

	// Terminal response projection: completed status, the model's output, and usage.
	state, projection := h.response(responseID)
	if state != "completed" {
		t.Fatalf("response state = %q, want completed", state)
	}
	if len(projection.Output) != 1 || projection.Output[0]["content"] != "12" {
		t.Fatalf("response output = %+v, want a single message with content 12", projection.Output)
	}
	if projection.Usage.TotalTokens == 0 {
		t.Fatalf("response usage total_tokens = 0, want the accumulated model usage")
	}

	// No duplicate model/tool dispatch: two model requests, one tool execution.
	if calls := atomic.LoadInt32(&h.provider.calls); calls != 2 {
		t.Fatalf("model provider calls = %d, want 2", calls)
	}
	if n := h.count(`SELECT count(*) FROM tool_calls WHERE run_id=$1`, runID); n != 1 {
		t.Fatalf("tool_call rows = %d, want 1", n)
	}
}

// TestModelRequestPersistedBeforeRouteAndReplayed proves two things brief Step 4
// requires: the model request is journaled BEFORE the provider is called, and a
// committed result for a stable model_request_id is replayed rather than re-routed
// (the DB half of cross-attempt dedup, spec §53.4). The scripted channel dispatches
// the same model_request_id twice — a live engine would reject a second model.result.
func TestModelRequestPersistedBeforeRouteAndReplayed(t *testing.T) {
	h := newHarness(t)
	responseID, sessionID, runID := h.admit()

	mreq := newID("mreq")
	frames := []contracts.EngineFrame{
		scriptFrame("engine.ready", runID, map[string]any{
			"selected_protocol": "engine.v1",
			"engine":            map[string]any{"name": "fake", "version": "0"},
			"max_frame_bytes":   1024, "nonce": "n",
		}),
		scriptFrame("model.request", runID, map[string]any{"model_request_id": mreq}),
		scriptFrame("model.request", runID, map[string]any{"model_request_id": mreq}),
		scriptFrame("output.item", runID, map[string]any{"type": "message", "content": "done"}),
		scriptFrame("run.terminal", runID, map[string]any{"outcome": "completed", "output": "done"}),
	}
	orch := h.newOrchestrator(scriptedDialer{&scriptedChannel{frames: frames}})
	if err := orch.ExecuteAttempt(context.Background(), h.descriptor(runID, 1)); err != nil {
		t.Fatalf("ExecuteAttempt error = %v", err)
	}

	if calls := atomic.LoadInt32(&h.provider.calls); calls != 1 {
		t.Fatalf("provider calls = %d, want 1 (a committed result must replay, not re-route)", calls)
	}

	events := h.events(sessionID)
	reqSeq, resSeq := -1, -1
	for _, e := range events {
		if e.typ == "run.model_request.v1" && reqSeq < 0 {
			reqSeq = e.seq
		}
		if e.typ == "run.model_result.v1" && resSeq < 0 {
			resSeq = e.seq
		}
	}
	if reqSeq < 1 {
		t.Fatalf("no run.model_request.v1 event journaled: %+v", events)
	}
	if resSeq < 1 || reqSeq >= resSeq {
		t.Fatalf("request event (seq %d) must precede result event (seq %d)", reqSeq, resSeq)
	}
	if state, _ := h.response(responseID); state != "completed" {
		t.Fatalf("response state = %q, want completed", state)
	}
}

// TestCommitBeforeDeliverOnToolResult proves the tool_call row and its journal event
// are committed to PostgreSQL before the tool.result frame ever reaches the engine.
func TestCommitBeforeDeliverOnToolResult(t *testing.T) {
	h := newHarness(t)
	responseID, sessionID, runID := h.admit()

	var checked int32
	dialer := subprocessDialer{engineDir: h.engineDir, onSend: func(frame contracts.EngineFrame) {
		if frame.Type != "tool.result" {
			return
		}
		callID, _ := frame.Data["tool_call_id"].(string)
		var state string
		if err := h.spine.Pool().QueryRow(context.Background(),
			`SELECT state FROM tool_calls WHERE id=$1`, callID).Scan(&state); err != nil {
			t.Errorf("tool_call %s not committed before tool.result delivery: %v", callID, err)
			return
		}
		if state != "completed" {
			t.Errorf("tool_call state = %q before delivery, want completed", state)
		}
		if n := h.count(`SELECT count(*) FROM events WHERE session_id=$1 AND type='run.tool_result.v1'`, sessionID); n < 1 {
			t.Errorf("tool result event not committed before delivery")
		}
		atomic.AddInt32(&checked, 1)
	}}

	if err := h.newOrchestrator(dialer).ExecuteAttempt(context.Background(), h.descriptor(runID, 1)); err != nil {
		t.Fatalf("ExecuteAttempt error = %v", err)
	}
	if atomic.LoadInt32(&checked) == 0 {
		t.Fatal("no tool.result was delivered; commit-before-deliver was never exercised")
	}
	if state, _ := h.response(responseID); state != "completed" {
		t.Fatalf("response state = %q, want completed", state)
	}
}

// TestDuplicateEngineFrameDoesNotDoubleDispatch proves the controller-side frame
// ledger: a re-delivered frame with the same id and hash is an idempotent replay that
// dispatches nothing new, while a re-delivered id with a different hash is a protocol
// violation that fails the attempt.
func TestDuplicateEngineFrameDoesNotDoubleDispatch(t *testing.T) {
	t.Run("same_hash_replays", func(t *testing.T) {
		h := newHarness(t)
		responseID, _, runID := h.admit()
		dialer := subprocessDialer{engineDir: h.engineDir, dupType: "model.request"}
		if err := h.newOrchestrator(dialer).ExecuteAttempt(context.Background(), h.descriptor(runID, 1)); err != nil {
			t.Fatalf("ExecuteAttempt error = %v", err)
		}
		if state, _ := h.response(responseID); state != "completed" {
			t.Fatalf("response state = %q, want completed", state)
		}
		if calls := atomic.LoadInt32(&h.provider.calls); calls != 2 {
			t.Fatalf("model provider calls = %d, want 2 (the duplicate frame must not dispatch)", calls)
		}
	})

	t.Run("different_hash_violates", func(t *testing.T) {
		h := newHarness(t)
		_, _, runID := h.admit()
		dialer := subprocessDialer{engineDir: h.engineDir, dupType: "model.request", mutateDup: true}
		err := h.newOrchestrator(dialer).ExecuteAttempt(context.Background(), h.descriptor(runID, 1))
		if err == nil {
			t.Fatal("a re-delivered frame id with a different hash did not fail the attempt")
		}
		if calls := atomic.LoadInt32(&h.provider.calls); calls != 1 {
			t.Fatalf("model provider calls = %d, want 1 before the protocol violation", calls)
		}
	})
}
