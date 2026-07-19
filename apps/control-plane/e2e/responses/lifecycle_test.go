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

// awaitRunState polls a run's durable state until it reaches want, so a test can wait for a
// cooperative pause to land the run in waiting before asserting on it.
func (h *harness) awaitRunState(runID, want string, within time.Duration) {
	h.t.Helper()
	deadline := time.Now().Add(within)
	var last string
	for time.Now().Before(deadline) {
		if last = h.runState(runID); last == want {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	h.t.Fatalf("run %s state = %q after %s, want %q", runID, last, within, want)
}

// responseRunJobCount counts the durable response.run jobs enqueued for a run. Each attempt
// generation is one job, so a resume that opens a fresh attempt on the same run adds a second.
func (h *harness) responseRunJobCount(runID string) int {
	h.t.Helper()
	return h.count(`SELECT count(*) FROM durable_jobs WHERE payload->>'run_id'=$1 AND kind='response.run' AND organization_id=$2 AND project_id=$3`,
		runID, h.tenant.Organization, h.tenant.Project)
}

// TestPauseReleasesComputeResumeSameRunNewAttempt proves the pause/resume lifecycle (SES-009,
// the deterministic command/state half) and faithful resume (spec §22.3). pause is a cooperative
// boundary stop: at a safe loop boundary the run goes to waiting, the attempt ends, and its
// compute is released (the durable job settles) — but a message queued before the pause is
// PRE-EMPTED, left queued for resume rather than delivered. resume opens a NEW attempt on the SAME
// run (waiting -> running, a second response.run job): it replays the committed first step from
// the journal (the provider is NOT re-called for it) and re-delivers the queued message, which
// folds into the resumed run's next model step. The current response's committed transcript
// survives the pause intact — the load-bearing faithful-resume invariant.
func TestPauseReleasesComputeResumeSameRunNewAttempt(t *testing.T) {
	h := newHarness(t)
	gp, rec := newGatedProvider(), &deliverRecorder{}
	dialer := subprocessDialer{engineDir: h.engineDir, onSend: rec.onSend}
	stop := h.runWorker(h.newOrchestratorWithAdapter(dialer, gp))
	defer stop()

	respID, sessionID, runID := h.admitWith(`{"input":"first turn"}`, newID("idem"))

	// Wait for the first (pre-tool) model step to start, then park it at the gate.
	select {
	case <-gp.started:
	case <-time.After(30 * time.Second):
		t.Fatal("first model step never started")
	}

	// Queue a message (to survive the pause), then pause — both while the first step is gated.
	msgID := newID("cmd")
	if cmd := h.submitCommand(sessionID, `{"command_id":"`+msgID+`","kind":"send_message","delivery":"queue","message":"RESUME-FOLD"}`); cmd.Status != "queued" {
		t.Fatalf("send_message status = %q, want queued", cmd.Status)
	}
	pauseID := newID("cmd")
	if cmd := h.submitCommand(sessionID, `{"command_id":"`+pauseID+`","kind":"pause"}`); cmd.Status != "queued" {
		t.Fatalf("pause status = %q, want queued (a live run should accept it)", cmd.Status)
	}

	// Release the first step: it completes (a tool call), the loop reaches a safe boundary, and
	// the pending pause pre-empts it — the run goes to waiting and the attempt ends.
	close(gp.release)

	h.awaitRunState(runID, "waiting", 30*time.Second)
	// Compute released: the paused attempt's durable job settled rather than holding its lease.
	h.awaitJobStatus(runID, "completed", 30*time.Second)
	// The pause applied; the queued message is PRE-EMPTED — still queued, never delivered.
	if st, seq := h.commandRow(pauseID); st != "applied" || seq == nil {
		t.Fatalf("pause state = %q applied_sequence = %v, want applied with a sequence", st, seq)
	}
	if st, _ := h.commandRow(msgID); st != "queued" {
		t.Fatalf("queued message state after pause = %q, want queued (pre-empted, survives for resume)", st)
	}
	if count, _ := rec.snapshot(); count != 0 {
		t.Fatalf("message.deliver frames before resume = %d, want 0 (the pause pre-empted delivery)", count)
	}
	if n := h.responseRunJobCount(runID); n != 1 {
		t.Fatalf("response.run jobs after pause = %d, want 1 (one attempt so far)", n)
	}

	// Resume: the same run re-enters running under a NEW attempt (a second response.run job).
	if cmd := h.submitCommand(sessionID, `{"command_id":"`+newID("cmd")+`","kind":"resume"}`); cmd.Status != "applied" {
		t.Fatalf("resume status = %q, want applied", cmd.Status)
	}
	h.awaitResponseState(respID, "completed", 60*time.Second)

	// A new attempt drove the resume (a second response.run job); the run id held throughout.
	if n := h.responseRunJobCount(runID); n != 2 {
		t.Fatalf("response.run jobs after resume = %d, want 2 (resume opened a new attempt on the same run)", n)
	}
	// Faithful resume: the first step was REPLAYED from the journal, so the provider was called
	// exactly twice across both attempts (step 1 once in attempt 1, step 2 once in attempt 2) —
	// never a third time re-running the already-committed first step.
	if calls := gp.callCount(); calls != 2 {
		t.Fatalf("provider calls across pause/resume = %d, want 2 (the committed first step is replayed, not re-run)", calls)
	}
	// The pre-empted message survived the pause, was re-delivered exactly once on resume, and
	// folded into the resumed run's model context.
	if count, last := rec.snapshot(); count != 1 || last != "RESUME-FOLD" {
		t.Fatalf("message.deliver frames after resume = %d (last %q), want exactly 1 of RESUME-FOLD", count, last)
	}
	if !gp.sawUserMessage("RESUME-FOLD") {
		t.Fatal("the queued message did not survive resume / never folded into the resumed run")
	}
	if st, seq := h.commandRow(msgID); st != "applied" || seq == nil {
		t.Fatalf("queued message state after resume = %q applied_sequence = %v, want applied", st, seq)
	}
}

// TestRedeliveredJobOnWaitingRunIsBailedNotDriven closes the pause/settle race window (the T4
// R2 finding): in the ms between PauseRun committing the run to waiting and the paused attempt's
// durable job settling, a worker crash redelivers the response.run job. Without the entry guard
// the fresh attempt skips Provision/Start on ErrInvalidState (waiting is non-terminal), drives the
// waiting run, delivers the PRE-EMPTED message, and finalizes an illegal waiting→completed —
// dead-lettering the job and FAILING a resumable run. The guard bails the attempt cleanly, leaving
// the run waiting and the pre-empted message queued for resume — faithful resume survives the race.
func TestRedeliveredJobOnWaitingRunIsBailedNotDriven(t *testing.T) {
	h := newHarness(t)
	gp, rec := newGatedProvider(), &deliverRecorder{}
	dialer := subprocessDialer{engineDir: h.engineDir, onSend: rec.onSend}
	orch := h.newOrchestratorWithAdapter(dialer, gp)
	stop := h.runWorker(orch)
	defer stop()

	_, sessionID, runID := h.admitWith(`{"input":"first turn"}`, newID("idem"))

	// Park the first step at the gate, queue a message to survive the pause, then pause.
	select {
	case <-gp.started:
	case <-time.After(30 * time.Second):
		t.Fatal("first model step never started")
	}
	msgID := newID("cmd")
	if cmd := h.submitCommand(sessionID, `{"command_id":"`+msgID+`","kind":"send_message","delivery":"queue","message":"SURVIVE-REDELIVERY"}`); cmd.Status != "queued" {
		t.Fatalf("send_message status = %q, want queued", cmd.Status)
	}
	pauseID := newID("cmd")
	if cmd := h.submitCommand(sessionID, `{"command_id":"`+pauseID+`","kind":"pause"}`); cmd.Status != "queued" {
		t.Fatalf("pause status = %q, want queued", cmd.Status)
	}
	close(gp.release)

	h.awaitRunState(runID, "waiting", 30*time.Second)
	h.awaitJobStatus(runID, "completed", 30*time.Second) // the paused attempt's job settled

	// The race: redeliver the response.run job while the run is waiting, by driving a fresh attempt
	// directly (a redelivery whose paused predecessor had not yet acked would land here identically).
	if err := orch.ExecuteAttempt(context.Background(), h.descriptor(runID, 2)); err != nil {
		t.Fatalf("redelivered attempt on a waiting run error = %v, want a clean bail (nil)", err)
	}

	// The guard held: the run is STILL waiting (never driven or failed) and the pre-empted message
	// is STILL queued — the doomed attempt neither delivered it nor finalized the run.
	if st := h.runState(runID); st != "waiting" {
		t.Fatalf("run state after redelivered attempt = %q, want waiting (the guard must not drive/fail it)", st)
	}
	if st, _ := h.commandRow(msgID); st != "queued" {
		t.Fatalf("pre-empted message state after redelivered attempt = %q, want queued (never delivered)", st)
	}
	if count, _ := rec.snapshot(); count != 0 {
		t.Fatalf("message.deliver frames from the redelivered attempt = %d, want 0", count)
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
