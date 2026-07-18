//go:build e2e

package responses

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/packages/contracts"
)

// gatedChannel replays a scripted frame sequence but blocks before frame index gateAt
// until gate is closed, so a test can cancel a run while an attempt is in flight — the run
// is running, the first model step is committed, and the next model.request has not yet
// been delivered. Send is discarded; the orchestrator's model.result never needs a reader.
type gatedChannel struct {
	frames []contracts.EngineFrame
	i      int
	gateAt int
	gate   chan struct{}
}

func (c *gatedChannel) Send(context.Context, contracts.EngineFrame) error { return nil }
func (c *gatedChannel) Close() error                                      { return nil }
func (c *gatedChannel) Receive(ctx context.Context) (contracts.EngineFrame, error) {
	if c.i == c.gateAt {
		select {
		case <-c.gate:
		case <-ctx.Done():
			return contracts.EngineFrame{}, ctx.Err()
		}
	}
	if c.i >= len(c.frames) {
		return contracts.EngineFrame{}, io.EOF
	}
	f := c.frames[c.i]
	c.i++
	return f, nil
}

type gatedDialer struct{ ch *gatedChannel }

func (d gatedDialer) Dial(context.Context, execution.AttemptDescriptor) (execution.EngineChannel, error) {
	return d.ch, nil
}

// TestCancelRunningResponseReachesCanceledTerminal proves the contracted cancel endpoint
// drives a running response to a canonical canceled terminal, and that the commit-after-
// terminal guard holds the "exactly one terminal, contiguous events" invariant under a
// cancel that races an in-flight attempt (spec §22.3 monotonic terminality).
//
// A gated engine delivers engine.ready + one model.request, so the attempt drives the run
// to running, commits one model step, and then parks before a second model.request. With
// the attempt parked, POST /cancel returns 202 + a canceled projection, the run row is
// canceled, run.canceled.v1 is the journal's last (terminal) event, GET reads the canceled
// terminal with a problem-shaped error, and the SSE stream closes cleanly after the
// terminal. Releasing the gate then delivers the second model.request: its commit is
// rejected by the terminal-run guard, so no event is appended after the terminal, the fake
// provider is not called again, the attempt ends cleanly, and the durable job settles
// instead of dead-lettering.
func TestCancelRunningResponseReachesCanceledTerminal(t *testing.T) {
	h := newHarness(t)
	responseID, sessionID, runID := h.admit()

	ch := &gatedChannel{
		gate:   make(chan struct{}),
		gateAt: 2, // block before delivering the third frame (the second model.request)
		frames: []contracts.EngineFrame{
			scriptFrame("engine.ready", runID, 1, map[string]any{
				"selected_protocol": "engine.v1",
				"engine":            map[string]any{"name": "fake", "version": "0"},
				"max_frame_bytes":   1024, "nonce": "n",
			}),
			scriptFrame("model.request", runID, 2, map[string]any{"model_request_id": newID("mreq")}),
			scriptFrame("model.request", runID, 3, map[string]any{"model_request_id": newID("mreq")}),
		},
	}
	stop := h.runWorker(h.newOrchestrator(gatedDialer{ch: ch}))
	defer stop()

	// The attempt drives the run to running and commits the first model step, then parks at
	// the gate. Wait until that first step is journaled so the run is provably in flight.
	h.awaitLastEvent(sessionID, "model_step.completed.v1", 20*time.Second)
	if state := h.runState(runID); state != "running" {
		t.Fatalf("run state before cancel = %q, want running", state)
	}
	if calls := atomic.LoadInt32(&h.provider.calls); calls != 1 {
		t.Fatalf("provider calls before cancel = %d, want 1", calls)
	}
	preCancel := h.events(sessionID)

	// Cancel the running response: 202 + a canceled projection.
	resp := h.cancelResponse(responseID, h.token)
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("cancel status = %d, want 202 (body=%s)", resp.StatusCode, body)
	}
	canceled := decodeResp(t, body)
	if canceled.Status != "canceled" {
		t.Fatalf("cancel body status = %q, want canceled", canceled.Status)
	}

	// The run row is canceled and run.canceled.v1 is the journal's last (terminal) event.
	if state := h.runState(runID); state != "canceled" {
		t.Fatalf("run state after cancel = %q, want canceled", state)
	}
	afterCancel := h.events(sessionID)
	assertContiguous(t, afterCancel)
	if len(afterCancel) != len(preCancel)+1 {
		t.Fatalf("cancel journaled %d events, want exactly 1 (run.canceled.v1)", len(afterCancel)-len(preCancel))
	}
	if last := afterCancel[len(afterCancel)-1].typ; last != "run.canceled.v1" {
		t.Fatalf("last journaled event = %q, want run.canceled.v1", last)
	}

	// GET reads the canceled terminal with a problem-shaped error.
	get := h.getResponse(responseID, h.token)
	getBody := readAll(t, get)
	if get.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200 (body=%s)", get.StatusCode, getBody)
	}
	var retrieved struct {
		Status string             `json:"status"`
		Error  *contracts.Problem `json:"error"`
	}
	if err := json.Unmarshal(getBody, &retrieved); err != nil {
		t.Fatalf("decode GET body error = %v (body=%s)", err, getBody)
	}
	if retrieved.Status != "canceled" {
		t.Fatalf("GET status = %q, want canceled", retrieved.Status)
	}
	if retrieved.Error == nil || retrieved.Error.Code != "canceled" {
		t.Fatalf("GET error = %+v, want a canceled problem shape", retrieved.Error)
	}

	// The SSE stream replays the journal, delivers the terminal, and closes cleanly.
	types, closed := h.readSSEUntilClose(sessionID, 10*time.Second)
	if !closed {
		t.Fatal("SSE stream did not close after the cancel terminal (hung run)")
	}
	if len(types) == 0 || types[len(types)-1] != "run.canceled.v1" {
		t.Fatalf("SSE event types = %v, want run.canceled.v1 last then a clean close", types)
	}

	// Release the gate: the in-flight attempt delivers a second model.request. Its commit is
	// rejected by the terminal-run guard — no event is appended after the terminal, the fake
	// provider is not called again, and the attempt ends cleanly so the job settles.
	close(ch.gate)
	h.awaitJobStatus(runID, "completed", 15*time.Second)

	if calls := atomic.LoadInt32(&h.provider.calls); calls != 1 {
		t.Fatalf("provider calls after cancel = %d, want 1 (the post-terminal commit must abort before the provider)", calls)
	}
	postGuard := h.events(sessionID)
	if len(postGuard) != len(afterCancel) {
		t.Fatalf("a post-terminal commit leaked %d events after the terminal", len(postGuard)-len(afterCancel))
	}
	if last := postGuard[len(postGuard)-1].typ; last != "run.canceled.v1" {
		t.Fatalf("last event after the guard = %q, want run.canceled.v1 (terminal is the journal's end)", last)
	}

	// Cancel is retry-safe: re-canceling a canceled terminal is the same no-op, no second
	// terminal (§22.3), still 202 + the canceled projection.
	retry := h.cancelResponse(responseID, h.token)
	retryBody := readAll(t, retry)
	if retry.StatusCode != http.StatusAccepted {
		t.Fatalf("re-cancel status = %d, want 202", retry.StatusCode)
	}
	if again := decodeResp(t, retryBody); again.Status != "canceled" {
		t.Fatalf("re-cancel body status = %q, want canceled", again.Status)
	}
	if after := h.events(sessionID); len(after) != len(postGuard) {
		t.Fatalf("re-cancel journaled %d new events; a canceled terminal must not reopen", len(after)-len(postGuard))
	}
}

// TestCancelIsScopedToProject proves cancel is tenant-scoped: a second project's key cannot
// cancel the first project's response. The foreign id is a 404 that leaks no existence (LP5
// scope immunity), and the run is not canceled — no transition, no event.
func TestCancelIsScopedToProject(t *testing.T) {
	h := newHarness(t)
	responseID, sessionID, runID := h.admit()
	before := h.events(sessionID)

	// A second, fully independent tenant with its own key.
	otherToken := newID("e2e-tok")
	seedTenantWithKey(t, h.spine.Pool(), otherToken)

	resp := h.cancelResponse(responseID, otherToken)
	problem := decodeProblemBody(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-tenant cancel status = %d, want 404", resp.StatusCode)
	}
	if problem.Code != "not_found" {
		t.Fatalf("code = %q, want not_found", problem.Code)
	}

	// No cancel happened: the run is untouched and no event was journaled.
	if state := h.runState(runID); state == "canceled" {
		t.Fatalf("cross-tenant cancel canceled the run (state = %q)", state)
	}
	if after := h.events(sessionID); len(after) != len(before) {
		t.Fatalf("cross-tenant cancel journaled %d events, want 0", len(after)-len(before))
	}
}

// TestCancelAfterTerminalIsSafe proves monotonic terminality: canceling an already-terminal
// (completed) response is a safe no-op — 202 with the existing completed projection, no
// transition, no new event, no second terminal (spec §22.3). GET still reads completed.
func TestCancelAfterTerminalIsSafe(t *testing.T) {
	h := newHarness(t)
	responseID, sessionID, runID := h.admit()

	// Drive the run to a completed terminal through the real engine.
	if err := h.newOrchestrator(subprocessDialer{engineDir: h.engineDir}).
		ExecuteAttempt(context.Background(), h.descriptor(runID, 1)); err != nil {
		t.Fatalf("ExecuteAttempt error = %v", err)
	}
	if state, _ := h.response(responseID); state != "completed" {
		t.Fatalf("setup: response state = %q, want completed", state)
	}
	before := h.events(sessionID)

	resp := h.cancelResponse(responseID, h.token)
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("cancel-after-terminal status = %d, want 202 (body=%s)", resp.StatusCode, body)
	}
	// The existing completed terminal is returned unchanged — not a canceled projection.
	if got := decodeResp(t, body); got.Status != "completed" {
		t.Fatalf("cancel-after-terminal body status = %q, want completed (no double terminal)", got.Status)
	}
	if state := h.runState(runID); state != "completed" {
		t.Fatalf("run state after cancel-after-terminal = %q, want completed", state)
	}
	if after := h.events(sessionID); len(after) != len(before) {
		t.Fatalf("cancel-after-terminal journaled %d events, want 0 (monotonic terminality)", len(after)-len(before))
	}

	get := h.getResponse(responseID, h.token)
	getBody := readAll(t, get)
	if got := decodeResp(t, getBody); got.Status != "completed" {
		t.Fatalf("GET after cancel-after-terminal = %q, want completed", got.Status)
	}
}

// TestLateTerminalDoesNotOverwriteCanceled proves the second terminal at the response surface
// is closed (spec §22.3): once a cancel commits the canceled terminal, an in-flight engine
// that finishes recovery and sends run.terminal outcome=completed must NOT overwrite the
// canceled projection. finalize finds the run already terminal (ErrRunTerminal) and skips the
// projection write, so the run row, GET, and the journal all stay canceled — the late
// completed terminal loses the race and is dropped.
//
// A gated engine delivers engine.ready + one model.request, parking before a run.terminal
// completed frame. With the attempt parked, POST /cancel commits run.canceled.v1. Releasing
// the gate delivers the late run.terminal: without the guard, its unconditional projection
// write would flip GET to completed (a second terminal); with it, GET stays canceled.
func TestLateTerminalDoesNotOverwriteCanceled(t *testing.T) {
	h := newHarness(t)
	responseID, sessionID, runID := h.admit()

	ch := &gatedChannel{
		gate:   make(chan struct{}),
		gateAt: 2, // block before delivering the run.terminal completed frame
		frames: []contracts.EngineFrame{
			scriptFrame("engine.ready", runID, 1, map[string]any{
				"selected_protocol": "engine.v1",
				"engine":            map[string]any{"name": "fake", "version": "0"},
				"max_frame_bytes":   1024, "nonce": "n",
			}),
			scriptFrame("model.request", runID, 2, map[string]any{"model_request_id": newID("mreq")}),
			scriptFrame("run.terminal", runID, 3, map[string]any{"outcome": "completed"}),
		},
	}
	// Drive this run's attempt directly rather than through the shared worker: the worker is
	// not run-scoped, so it would also claim sibling tests' stale queued jobs and consume this
	// single gated channel on the wrong run. A direct ExecuteAttempt dials the gated channel
	// only for this run, so the park is deterministic and order-independent.
	orch := h.newOrchestrator(gatedDialer{ch: ch})
	done := make(chan error, 1)
	go func() { done <- orch.ExecuteAttempt(context.Background(), h.descriptor(runID, 1)) }()

	// The attempt drives the run to running and commits the first model step, then parks at
	// the gate before the late terminal.
	h.awaitLastEvent(sessionID, "model_step.completed.v1", 20*time.Second)

	// Cancel the parked run: run.canceled.v1 is the terminal.
	resp := h.cancelResponse(responseID, h.token)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("cancel status = %d, want 202 (body=%s)", resp.StatusCode, readAll(t, resp))
	}
	resp.Body.Close()
	if state := h.runState(runID); state != "canceled" {
		t.Fatalf("run state after cancel = %q, want canceled", state)
	}
	afterCancel := h.events(sessionID)

	// Release the gate: the in-flight engine delivers the late run.terminal completed. The run
	// is already terminal, so finalize must skip the projection write and end the attempt cleanly.
	close(ch.gate)
	if err := <-done; err != nil {
		t.Fatalf("attempt after late terminal error = %v", err)
	}

	// The canceled terminal stands: the run row and GET both stay canceled. GET is the
	// load-bearing check — the overwrite lands on the responses projection, not the journal.
	if state := h.runState(runID); state != "canceled" {
		t.Fatalf("run state after late terminal = %q, want canceled", state)
	}
	get := h.getResponse(responseID, h.token)
	if got := decodeResp(t, readAll(t, get)); got.Status != "canceled" {
		t.Fatalf("GET after late terminal = %q, want canceled (a late completed overwrote the canceled response, §22.3)", got.Status)
	}
	// The journal is unchanged: the rejected transition appends nothing after the terminal.
	after := h.events(sessionID)
	if len(after) != len(afterCancel) {
		t.Fatalf("late terminal journaled %d events after the cancel, want 0", len(after)-len(afterCancel))
	}
	if last := after[len(after)-1].typ; last != "run.canceled.v1" {
		t.Fatalf("last journaled event = %q, want run.canceled.v1", last)
	}
}

// awaitLastEvent polls the journal until its last event is want, so a test can wait for an
// in-flight attempt to reach a known committed step before acting.
func (h *harness) awaitLastEvent(sessionID, want string, within time.Duration) {
	h.t.Helper()
	deadline := time.Now().Add(within)
	var last string
	for time.Now().Before(deadline) {
		if evts := h.events(sessionID); len(evts) > 0 {
			if last = evts[len(evts)-1].typ; last == want {
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	h.t.Fatalf("journal last event = %q after %s, want %q", last, within, want)
}

// cancelResponse issues POST /v1/responses/{id}/cancel with the given bearer token.
func (h *harness) cancelResponse(responseID, token string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.base+"/v1/responses/"+responseID+"/cancel", nil)
	if err != nil {
		h.t.Fatalf("build cancel error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("POST /cancel error = %v", err)
	}
	return resp
}

func readAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body error = %v", err)
	}
	return data
}

func decodeResp(t *testing.T, body []byte) contracts.Response {
	t.Helper()
	var r contracts.Response
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("decode response error = %v (body=%s)", err, body)
	}
	return r
}

func decodeProblemBody(t *testing.T, resp *http.Response) contracts.Problem {
	t.Helper()
	var p contracts.Problem
	if err := json.Unmarshal(readAll(t, resp), &p); err != nil {
		t.Fatalf("decode problem error = %v", err)
	}
	return p
}
