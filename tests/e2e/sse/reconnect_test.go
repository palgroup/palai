//go:build e2e

package sse

import (
	"testing"

	statemachines "github.com/palgroup/palai/packages/state-machines"

	"github.com/palgroup/palai/storage"
)

// TestReconnectResumesAfterLastEventID reads three events, drops the connection,
// proves the disconnect did not cancel the run, then reconnects with Last-Event-ID
// and collects the remaining events through the terminal. The union of both
// connections must be the unique, gap-free canonical sequence.
func TestReconnectResumesAfterLastEventID(t *testing.T) {
	h := newHarness(t)
	sessionID, runID := h.createSession()

	// queued (seq 1) already exists; advance to running so three events are journaled.
	h.apply(runID, statemachines.RunCmdProvision) // seq 2: run.provisioning.v1
	h.apply(runID, statemachines.RunCmdStart)     // seq 3: run.running.v1

	first := h.openStream(sessionID, nil)
	var collected []sseEvent
	for i := 0; i < 3; i++ {
		ev, ok := first.next(t)
		if !ok {
			t.Fatalf("stream closed after %d events, want 3", i)
		}
		collected = append(collected, ev)
	}
	lastSeen := collected[2]
	if lastSeen.event != "run.running.v1" {
		t.Fatalf("third event = %q, want run.running.v1", lastSeen.event)
	}

	// Drop the TCP stream mid-run.
	first.close()

	// A client disconnect must not cancel the run: the journal read never writes.
	if state := h.runState(runID); state != "running" {
		t.Fatalf("run state after disconnect = %q, want running (disconnect must not cancel)", state)
	}

	// Reconnect from the last confirmed event id; the server replays from seq 4 on.
	second := h.openStream(sessionID, map[string]string{"Last-Event-ID": lastSeen.id})
	defer second.close()

	// Drive the run to a terminal state; the tailing stream delivers it, then closes.
	h.apply(runID, statemachines.RunCmdComplete) // seq 4: run.completed.v1

	for {
		ev, ok := second.next(t)
		if !ok {
			break // clean close after the terminal event
		}
		collected = append(collected, ev)
	}

	if len(collected) != 4 {
		t.Fatalf("collected %d events, want 4", len(collected))
	}
	assertContiguous(t, collected, 1)
	if last := collected[3]; last.event != "run.completed.v1" {
		t.Fatalf("final event = %q, want run.completed.v1 (terminal)", last.event)
	}
}

// TestForeignSessionIsNotFound proves the tenant boundary: a session id owned by
// another tenant is a 404, never a readable stream.
func TestForeignSessionIsNotFound(t *testing.T) {
	h := newHarness(t)

	// A session that exists for a *different* tenant. It is planted under that
	// tenant's OWN scope (000029's WITH CHECK rejects an undeclared writer), so the
	// row is genuinely foreign; the assertion below still reads over HTTP with the
	// harness credential, i.e. under the harness tenant's scope.
	other := seedTenantWithKey(t, h.spine.Pool(), newID("other-tok"))
	otherSession := newID("ses")
	if _, err := h.spine.Pool().Exec(storage.WithTenant(t.Context(), other.Organization, other.Project),
		`INSERT INTO sessions (id, organization_id, project_id) VALUES ($1, $2, $3)`,
		otherSession, other.Organization, other.Project); err != nil {
		t.Fatalf("seed foreign session error = %v", err)
	}

	resp := h.getEvents(otherSession, nil)
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("foreign session status = %d, want 404", resp.StatusCode)
	}
}
