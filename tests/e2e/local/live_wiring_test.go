//go:build e2e

package local

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// TestShippedBinaryCompletesOneResponseThroughLiveTopology proves main.go's own production
// exec-path wiring — not an in-process test harness — drives a created response to a terminal
// completed state through the real four-service topology: the shipped control-plane binary
// dispatches the run, the orchestrator routes the model.request through the deterministic fake
// adapter, dials the engine over the runner gateway (compose mTLS enrollment), and the runner
// supervises the reference engine as a real OCI container. Proof class e2e-deterministic: the
// fake provider means no network and no credential, so it does NOT stand in for the live
// provider LP suite. RED before the wiring: with only AdvanceRun the run hangs in running and
// the reconciler/dead-letter bridge drives it to failed, never completed.
func TestShippedBinaryCompletesOneResponseThroughLiveTopology(t *testing.T) {
	// Turn the exec-path on with the deterministic provider; the compose default is
	// dispatch-off/queued-only, so the browser and clean-boot proofs stay unaffected.
	t.Setenv("PALAI_DISPATCH_WORKERS", "1")
	t.Setenv("PALAI_MODEL_PROVIDER", "fake")

	s := newStack(t)
	s.run("init")
	s.run("local", "up")

	// The runner must have enrolled over compose mTLS for a live engine channel to exist.
	report := s.doctor()
	if !report.OK {
		t.Fatalf("doctor not green: %+v", report.Checks)
	}
	if got := report.Checks["runner"].Status; got != "ok" {
		t.Fatalf("runner check = %q, want ok (mTLS enrollment): %s", got, report.Checks["runner"].Detail)
	}

	id := createResponse(t, s, "hello")
	final := awaitTerminal(t, s, id, 120*time.Second)

	if final.Status != "completed" {
		t.Fatalf("response %s terminal status = %q, want completed (the exec-path did not run to a terminal)", id, final.Status)
	}
	if len(final.Output) == 0 {
		t.Fatalf("completed response %s carries no output projection", id)
	}
}

// terminalResponse is the slice of the GET /v1/responses/{id} projection the wiring proof
// reads: the terminal status and the output items the finalize step committed.
type terminalResponse struct {
	Status string           `json:"status"`
	Output []map[string]any `json:"output"`
}

// awaitTerminal polls the public API until the response reaches a terminal status or the
// deadline elapses. A failed/canceled status ends the wait immediately — a failed run is the
// pre-wiring RED signal (assignment-only leaves the run to the dead-letter bridge), so the
// test names it rather than waiting out the whole timeout.
func awaitTerminal(t *testing.T, s *stack, id string, within time.Duration) terminalResponse {
	t.Helper()
	deadline := time.Now().Add(within)
	var last terminalResponse
	for time.Now().Before(deadline) {
		resp := s.getResponse(id)
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			t.Fatalf("GET /v1/responses/%s = %d, want 200", id, resp.StatusCode)
		}
		if err := json.NewDecoder(resp.Body).Decode(&last); err != nil {
			resp.Body.Close()
			t.Fatalf("decode response %s: %v", id, err)
		}
		resp.Body.Close()
		switch last.Status {
		case "completed", "failed", "canceled":
			return last
		}
		time.Sleep(1 * time.Second)
	}
	t.Fatalf("response %s never reached a terminal status (last=%q)", id, last.Status)
	return last
}
