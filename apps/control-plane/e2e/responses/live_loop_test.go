//go:build e2e

package responses

import (
	"context"
	"encoding/json"
	"net/http"
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

// TestRetrieveCompletedResponseCarriesUsedModel proves the terminal projection
// completeness follow-up: after the live loop reaches a completed terminal, GET
// /v1/responses/{id} carries the actually-used model from the committed model result
// (non-empty), and a completed response carries no error field (spec §8.3 retrieval,
// response.json requires model and models error as oneOf[problem, null]).
func TestRetrieveCompletedResponseCarriesUsedModel(t *testing.T) {
	h := newHarness(t)
	stop := h.runWorker(h.newOrchestrator(subprocessDialer{engineDir: h.engineDir}))
	defer stop()

	responseID, _, _ := h.admit()
	h.awaitResponseState(responseID, "completed", 60*time.Second)

	resp := h.getResponse(responseID, h.token)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", resp.StatusCode)
	}
	var got struct {
		Model string             `json:"model"`
		Error *contracts.Problem `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode retrieve body error = %v", err)
	}
	// The scripted provider commits "fake" as the used model on every model result.
	if got.Model != "fake" {
		t.Fatalf("retrieved model = %q, want the used model %q", got.Model, "fake")
	}
	if got.Error != nil {
		t.Fatalf("completed response carried an error = %+v, want none", got.Error)
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
		scriptFrame("engine.ready", runID, 1, map[string]any{
			"selected_protocol": "engine.v1",
			"engine":            map[string]any{"name": "fake", "version": "0"},
			"max_frame_bytes":   1024, "nonce": "n",
		}),
		scriptFrame("model.request", runID, 2, map[string]any{"model_request_id": mreq}),
		scriptFrame("model.request", runID, 3, map[string]any{"model_request_id": mreq}),
		scriptFrame("output.item", runID, 4, map[string]any{"type": "message", "content": "done"}),
		scriptFrame("run.terminal", runID, 5, map[string]any{"outcome": "completed", "output": "done"}),
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
		if e.typ == "model_step.created.v1" && reqSeq < 0 {
			reqSeq = e.seq
		}
		if e.typ == "model_step.completed.v1" && resSeq < 0 {
			resSeq = e.seq
		}
	}
	if reqSeq < 1 {
		t.Fatalf("no model_step.created.v1 event journaled: %+v", events)
	}
	if resSeq < 1 || reqSeq >= resSeq {
		t.Fatalf("request event (seq %d) must precede result event (seq %d)", reqSeq, resSeq)
	}
	if state, _ := h.response(responseID); state != "completed" {
		t.Fatalf("response state = %q, want completed", state)
	}
}

// TestIntakeRejectsNonMonotonicEngineSequence proves the controller intake enforces the
// engine frame sequence: engine.ready at sequence 1 followed by a frame at sequence 3 is
// a gap the intake rejects as a protocol violation, failing the attempt before any model
// or tool dispatch. It closes the batch/streaming parity — the batch supervisor already
// required a contiguous index+1 sequence.
//
// The attempt fails as a protocol violation (ExecuteAttempt returns an error) with zero
// dispatch, matching the orchestrator's uniform intake-violation contract (see
// different_hash_violates): a violation returns an error rather than finalizing the run,
// so no false completion is committed.
func TestIntakeRejectsNonMonotonicEngineSequence(t *testing.T) {
	h := newHarness(t)
	_, _, runID := h.admit()

	frames := []contracts.EngineFrame{
		scriptFrame("engine.ready", runID, 1, map[string]any{
			"selected_protocol": "engine.v1",
			"engine":            map[string]any{"name": "fake", "version": "0"},
			"max_frame_bytes":   1024, "nonce": "n",
		}),
		scriptFrame("model.request", runID, 3, map[string]any{"model_request_id": newID("mreq")}),
	}
	orch := h.newOrchestrator(scriptedDialer{&scriptedChannel{frames: frames}})

	err := orch.ExecuteAttempt(context.Background(), h.descriptor(runID, 1))
	if err == nil {
		t.Fatal("intake accepted a non-monotonic engine sequence (gap at 1->3)")
	}
	if calls := atomic.LoadInt32(&h.provider.calls); calls != 0 {
		t.Fatalf("model provider calls = %d, want 0 (a gapped frame must not dispatch)", calls)
	}
	if n := h.count(`SELECT count(*) FROM tool_calls WHERE run_id=$1`, runID); n != 0 {
		t.Fatalf("tool_call rows = %d, want 0 (a gapped frame must not dispatch)", n)
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
		if n := h.count(`SELECT count(*) FROM events WHERE session_id=$1 AND type='tool_call.completed.v1'`, sessionID); n < 1 {
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
