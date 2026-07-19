//go:build e2e

package responses

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// sessionState reads a session's lifecycle state straight from the table.
func (h *harness) sessionState(sessionID string) string {
	h.t.Helper()
	var state string
	if err := h.spine.Pool().QueryRow(context.Background(),
		`SELECT state FROM sessions WHERE id=$1 AND organization_id=$2 AND project_id=$3`,
		sessionID, h.tenant.Organization, h.tenant.Project).Scan(&state); err != nil {
		h.t.Fatalf("read session state %s error = %v", sessionID, err)
	}
	return state
}

// responseCount counts a session's response rows within the tenant scope.
func (h *harness) responseCount(sessionID string) int {
	h.t.Helper()
	return h.count(`SELECT count(*) FROM responses WHERE session_id=$1 AND organization_id=$2 AND project_id=$3`,
		sessionID, h.tenant.Organization, h.tenant.Project)
}

// TestClosedSessionRejectsNewWork proves the close_session lifecycle exit (spec §22.1, SES-012):
// close drives an idle session to closed, and a closed session then rejects new work — a new
// response is a 409, and a new command is a typed rejection. No new run or steer can start.
func TestClosedSessionRejectsNewWork(t *testing.T) {
	h := newHarness(t)
	sessionID := h.createSession()

	// Close the idle session: no live root run, so it goes straight to closed and the command applies.
	closeCmd := h.submitCommand(sessionID, `{"command_id":"`+newID("cmd")+`","kind":"close_session"}`)
	if closeCmd.Status != "applied" {
		t.Fatalf("close_session status = %q, want applied", closeCmd.Status)
	}
	if st := h.sessionState(sessionID); st != "closed" {
		t.Fatalf("session state after close = %q, want closed", st)
	}

	// A new response on the closed session is a 409 (admission gate, spec §22.1).
	resp := h.postResponse(`{"input":"more","session_id":"`+sessionID+`"}`, newID("idem"), h.token)
	assertProblem(t, resp, http.StatusConflict, "session_not_active")

	// A new command on the closed session is a typed rejection, not a silently-queued command.
	rejected := h.submitCommand(sessionID, `{"command_id":"`+newID("cmd")+`","kind":"send_message","delivery":"queue","message":"hi"}`)
	if rejected.Status != "rejected" {
		t.Fatalf("send_message on closed session status = %q, want rejected", rejected.Status)
	}
	if code, _ := rejected.Result["code"].(string); code != "session_not_active" {
		t.Fatalf("rejection code = %v, want session_not_active", rejected.Result["code"])
	}
}

// TestCloseSessionSweepsQueuedSessionCommands proves F1 (the T3 finding): close_session sweeps a
// session's still-queued commands to expired, so a change_config queued for the cross-run carry
// on a session that then never runs again does not orphan (queued forever). close is its
// lifecycle exit (spec §22.1, §22.4).
func TestCloseSessionSweepsQueuedSessionCommands(t *testing.T) {
	h := newHarness(t)
	sessionID := h.createSession()

	// A change_config on the idle session is accepted and queued for the cross-run carry (T3):
	// ExpireQueuedCommandsForRun excludes change_config, so only the session's close can reap it.
	changeID := newID("cmd")
	cc := h.submitCommand(sessionID, `{"command_id":"`+changeID+`","kind":"change_config","model":"model-beta"}`)
	if cc.Status != "queued" {
		t.Fatalf("idle-session change_config status = %q, want queued", cc.Status)
	}

	// Closing the session sweeps the orphaned change_config to expired.
	h.submitCommand(sessionID, `{"command_id":"`+newID("cmd")+`","kind":"close_session"}`)

	if st, _ := h.commandRow(changeID); st != "expired" {
		t.Fatalf("queued change_config state after close = %q, want expired (F1 close-sweep)", st)
	}
	// The close command itself is not swept — it applied.
	if st := h.sessionState(sessionID); st != "closed" {
		t.Fatalf("session state after close = %q, want closed", st)
	}
}

// TestForkCopiesHistoryBoundaryIsolatesFuture proves fork_session (spec §22.8, SES-011): the fork
// child is a new active session that reference-copies the parent's history up to the fork
// boundary, and a response written to the PARENT after the fork is NOT in the child — the fork's
// future is isolated.
func TestForkCopiesHistoryBoundaryIsolatesFuture(t *testing.T) {
	h := newHarness(t)
	stop := h.runWorker(h.newOrchestrator(subprocessDialer{engineDir: h.engineDir}))
	defer stop()

	// Parent: one completed response — the immutable history up to the fork boundary.
	resp1, sessionID, _ := h.admit()
	h.awaitResponseState(resp1, "completed", 60*time.Second)

	// Fork the parent: the command result carries the fresh child session id.
	forkCmd := h.submitCommand(sessionID, `{"command_id":"`+newID("cmd")+`","kind":"fork_session"}`)
	if forkCmd.Status != "applied" {
		t.Fatalf("fork_session status = %q, want applied", forkCmd.Status)
	}
	childID, _ := forkCmd.Result["session_id"].(string)
	if childID == "" || childID == sessionID {
		t.Fatalf("fork result session_id = %q, want a fresh child id (parent %q)", childID, sessionID)
	}
	if st := h.sessionState(childID); st != "active" {
		t.Fatalf("child session state = %q, want active", st)
	}

	// The child copied the parent's pre-fork history: exactly one response, carrying resp1's output.
	if n := h.responseCount(childID); n != 1 {
		t.Fatalf("child response count = %d, want 1 (the pre-fork history copy)", n)
	}
	var childOutput string
	if err := h.spine.Pool().QueryRow(context.Background(),
		`SELECT output::text FROM responses WHERE session_id=$1`, childID).Scan(&childOutput); err != nil {
		t.Fatalf("read child copied response error = %v", err)
	}
	if !strings.Contains(childOutput, "12") {
		t.Fatalf("child copied response output = %s, want the parent's completed output (12)", childOutput)
	}

	// A response written to the PARENT after the fork — the isolated future.
	resp2, session2, _ := h.admitWith(`{"input":"after the fork","session_id":"`+sessionID+`"}`, newID("idem"))
	if session2 != sessionID {
		t.Fatalf("chained response session = %q, want the parent %q", session2, sessionID)
	}
	h.awaitResponseState(resp2, "completed", 60*time.Second)

	// The parent grew to two responses; the child is unchanged — the future is NOT in the fork.
	if n := h.responseCount(sessionID); n != 2 {
		t.Fatalf("parent response count = %d, want 2 (its own future grew)", n)
	}
	if n := h.responseCount(childID); n != 1 {
		t.Fatalf("child response count = %d after the parent's post-fork response, want 1 (future isolated, §22.8)", n)
	}
}
