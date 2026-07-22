//go:build e2e

package responses

import (
	"context"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/internal/store"

	"github.com/palgroup/palai/storage"
)

// TestRestartPreservesTerminalResponseAndEvents proves the terminal outcome is durable
// (LP-008): after a run completes and the executing worker stops, a fresh process view
// of the same database reads the identical terminal response and the identical
// gap-free canonical event sequence. Nothing load-bearing lived only in memory.
func TestRestartPreservesTerminalResponseAndEvents(t *testing.T) {
	h := newHarness(t)
	stop := h.runWorker(h.newOrchestrator(subprocessDialer{engineDir: h.engineDir}))

	responseID, sessionID, _ := h.admit()
	h.awaitResponseState(responseID, "completed", 60*time.Second)

	stateBefore, projectionBefore := h.response(responseID)
	eventsBefore := h.events(sessionID)

	// Stop the executing worker (and its in-memory ledger/usage) — the run is done.
	stop()

	// "Restart": a brand-new store connection, as a fresh process would open, reading
	// the same durable database.
	ctx := context.Background()
	restarted, err := store.Open(ctx, requireEnv(t, "PALAI_E2E_POSTGRES_URL"))
	if err != nil {
		t.Fatalf("reopen store error = %v", err)
	}
	defer restarted.Close()

	var stateAfter string
	var outputAfter []byte
	if err := restarted.Spine().Pool().QueryRow(storage.WithSystemScope(ctx),
		`SELECT state, output FROM responses WHERE id=$1 AND organization_id=$2 AND project_id=$3`,
		responseID, h.tenant.Organization, h.tenant.Project).Scan(&stateAfter, &outputAfter); err != nil {
		t.Fatalf("read response after restart error = %v", err)
	}
	if stateAfter != stateBefore || stateAfter != "completed" {
		t.Fatalf("response state after restart = %q, want %q", stateAfter, stateBefore)
	}

	// The event sequence is byte-for-byte the same and still contiguous.
	eventsAfter := h.events(sessionID) // same tenant scope; the journal is durable
	assertContiguous(t, eventsAfter)
	if len(eventsAfter) != len(eventsBefore) {
		t.Fatalf("event count after restart = %d, want %d", len(eventsAfter), len(eventsBefore))
	}
	for i := range eventsAfter {
		if eventsAfter[i] != eventsBefore[i] {
			t.Fatalf("event %d after restart = %+v, want %+v", i, eventsAfter[i], eventsBefore[i])
		}
	}
	if len(projectionBefore.Output) == 0 {
		t.Fatal("terminal projection had no output to preserve")
	}
}
