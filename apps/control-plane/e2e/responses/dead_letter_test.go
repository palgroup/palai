//go:build e2e

package responses

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"

	"github.com/palgroup/palai/storage"
)

// violatingDialer returns a fresh channel on every Dial whose stream violates the intake
// contract identically — engine.ready at sequence 1 then a frame at sequence 3, a gap the
// controller intake rejects before any dispatch. So every attempt of a run fails the same
// way, exhausts the ceiling, and dead-letters the durable job.
type violatingDialer struct{ runID string }

func (d violatingDialer) Dial(_ context.Context, _ execution.AttemptDescriptor) (execution.EngineChannel, error) {
	return &scriptedChannel{frames: []contracts.EngineFrame{
		scriptFrame("engine.ready", d.runID, 1, map[string]any{
			"selected_protocol": "engine.v1",
			"engine":            map[string]any{"name": "fake", "version": "0"},
			"max_frame_bytes":   1024, "nonce": "n",
		}),
		scriptFrame("model.request", d.runID, 3, map[string]any{"model_request_id": newID("mreq")}),
	}}, nil
}

// TestDeadLetteredRunJobIsDrivenToFailedTerminal proves the reconciler bridge closes the
// plan-wide hung-run gap (spec §24.4 -> §22.3): a run whose every attempt deterministically
// violates the protocol dead-letters its durable job but never self-reports terminal. The
// bridge drives it to a failed run terminal — the run row is failed, run.failed.v1 is
// journaled, the response projects a failed terminal, and the SSE stream delivers the
// terminal event and closes rather than hanging in running forever.
func TestDeadLetteredRunJobIsDrivenToFailedTerminal(t *testing.T) {
	h := newHarness(t)
	responseID, sessionID, runID := h.admit()

	// Every attempt fails identically; a low ceiling dead-letters the job after a few.
	stop := h.runWorkerWithRetry(
		h.newOrchestrator(violatingDialer{runID: runID}),
		coordinator.RetryPolicy{MaxAttempts: 3, BaseBackoff: 5 * time.Millisecond, MaxBackoff: 20 * time.Millisecond},
	)
	h.awaitJobStatus(runID, "dead", 30*time.Second)
	stop()

	// Before the bridge the run is hung: non-terminal, no failed projection.
	if state := h.runState(runID); state == "failed" {
		t.Fatalf("run reached failed without the reconciler bridge (state = %q)", state)
	}

	// The reconciler sweep drives the dead-lettered run to a failed terminal.
	rec := execution.NewReconciler(h.spine, time.Hour, 3)
	if _, err := rec.Sweep(context.Background()); err != nil {
		t.Fatalf("reconciler Sweep error = %v", err)
	}

	// The run row is failed.
	if state := h.runState(runID); state != "failed" {
		t.Fatalf("run state after sweep = %q, want failed", state)
	}

	// run.failed.v1 is journaled, contiguous, and terminal (last).
	events := h.events(sessionID)
	assertContiguous(t, events)
	if len(events) == 0 || events[len(events)-1].typ != "run.failed.v1" {
		t.Fatalf("last journaled event = %+v, want run.failed.v1 terminal", events)
	}
	failures := 0
	for _, e := range events {
		if e.typ == "run.failed.v1" {
			failures++
		}
	}
	if failures != 1 {
		t.Fatalf("run.failed.v1 events = %d, want exactly 1 (terminal monotonicity)", failures)
	}

	// The response projects a failed terminal (a retrieval reads failed).
	if state, _ := h.response(responseID); state != "failed" {
		t.Fatalf("response projection state = %q, want failed", state)
	}

	// The SSE stream replays the journal, delivers the terminal event, and closes cleanly
	// — the run does not hang with an open stream.
	types, closed := h.readSSEUntilClose(sessionID, 10*time.Second)
	if !closed {
		t.Fatal("SSE stream did not close after the terminal event (hung run)")
	}
	if len(types) == 0 || types[len(types)-1] != "run.failed.v1" {
		t.Fatalf("SSE event types = %v, want run.failed.v1 last then a clean close", types)
	}

	// A later sweep is a no-op: the terminal run is not reopened.
	if _, err := rec.Sweep(context.Background()); err != nil {
		t.Fatalf("second reconciler Sweep error = %v", err)
	}
	if after := h.events(sessionID); len(after) != len(events) {
		t.Fatalf("a second sweep journaled %d new events; the terminal run must not reopen", len(after)-len(events))
	}
}

// TestRetrieveDeadLetteredResponseCarriesProblemError proves the terminal projection
// completeness follow-up on the failed path: a dead-lettered run the reconciler bridge
// drives to failed retrieves with a sanitized problem-shaped error (code + human title
// filled), and the error's request_id is stamped by the retrieval request. The dead
// letter never reached a model step, so model may be empty — only the completed path
// asserts a non-empty model.
func TestRetrieveDeadLetteredResponseCarriesProblemError(t *testing.T) {
	h := newHarness(t)
	responseID, _, runID := h.admit()

	stop := h.runWorkerWithRetry(
		h.newOrchestrator(violatingDialer{runID: runID}),
		coordinator.RetryPolicy{MaxAttempts: 3, BaseBackoff: 5 * time.Millisecond, MaxBackoff: 20 * time.Millisecond},
	)
	h.awaitJobStatus(runID, "dead", 30*time.Second)
	stop()

	// The reconciler bridge drives the dead-lettered run to a failed terminal projection.
	rec := execution.NewReconciler(h.spine, time.Hour, 3)
	if _, err := rec.Sweep(context.Background()); err != nil {
		t.Fatalf("reconciler Sweep error = %v", err)
	}

	resp := h.getResponse(responseID, h.token)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", resp.StatusCode)
	}
	var got struct {
		Status string             `json:"status"`
		Error  *contracts.Problem `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode retrieve body error = %v", err)
	}
	if got.Status != "failed" {
		t.Fatalf("retrieved status = %q, want failed", got.Status)
	}
	if got.Error == nil {
		t.Fatal("failed response carried no error; want a problem-shaped error object")
	}
	if got.Error.Code == "" || got.Error.Title == "" {
		t.Fatalf("error is not problem-shaped (code/title empty): %+v", got.Error)
	}
	// request_id is stamped at retrieval time, not stored, so it is a valid req_ id.
	if !strings.HasPrefix(string(got.Error.RequestID), "req_") {
		t.Fatalf("error.request_id = %q, want a req_ id stamped at retrieval", got.Error.RequestID)
	}
}

// runState reads a run's durable state.
func (h *harness) runState(runID string) string {
	h.t.Helper()
	var state string
	if err := h.spine.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT state FROM runs WHERE id=$1 AND organization_id=$2 AND project_id=$3`,
		runID, h.tenant.Organization, h.tenant.Project).Scan(&state); err != nil {
		h.t.Fatalf("read run state error = %v", err)
	}
	return state
}

// awaitJobStatus polls the durable job (found by its run_id payload) until it reaches want.
func (h *harness) awaitJobStatus(runID, want string, within time.Duration) {
	h.t.Helper()
	deadline := time.Now().Add(within)
	var last string
	for time.Now().Before(deadline) {
		if err := h.spine.Pool().QueryRow(storage.WithSystemScope(context.Background()),
			`SELECT status FROM durable_jobs WHERE payload->>'run_id'=$1 AND organization_id=$2 AND project_id=$3`,
			runID, h.tenant.Organization, h.tenant.Project).Scan(&last); err == nil && last == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatalf("job for run %s status = %q after %s, want %q", runID, last, within, want)
}

// readSSEUntilClose opens the resumable stream, reads every event type from the journal
// replay, and reports whether the server closed the stream (EOF) within the deadline —
// proving the terminal event closed it rather than a hang.
func (h *harness) readSSEUntilClose(sessionID string, within time.Duration) (types []string, closed bool) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.base+"/v1/sessions/"+sessionID+"/events", nil)
	if err != nil {
		h.t.Fatalf("build SSE GET error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("SSE GET error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("SSE GET status = %d, want 200", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		if line := sc.Text(); strings.HasPrefix(line, "event: ") {
			types = append(types, strings.TrimPrefix(line, "event: "))
		}
	}
	// The scan ended: either the server closed the stream after the terminal (clean) or
	// the deadline cancelled a hung read. A nil scanner error means a clean server EOF.
	return types, sc.Err() == nil && ctx.Err() == nil
}
