//go:build uat

package uat

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// lifecycleAnchorPrompt keeps every lifecycle case's real anchor call short and deterministic —
// one word — so the fork/close proofs spend a single cheap real generation each and assert on the
// committed output rather than the model's prose.
const lifecycleAnchorPrompt = "Reply with exactly one word and nothing else: LIGHTHOUSE."

// TestLocalLiveLifecycle is the E08 Task 4 live smoke (spec §22.1, §22.3, §22.8): fork_session,
// close_session, and cancel driven against real infrastructure and a real provider, each carrying a
// genuine chatcmpl receipt.
//
// On the real provider every run is single step (no tools → no boundary), so the lifecycle
// operations are proven AROUND real calls, never a fabricated multi-step run:
//   - close_session acts at a COMPLETED-response boundary — the real anchor call gives the case its
//     chatcmpl, and close is then applied and asserted (a closed session rejects new work).
//   - fork_session forks at a completed-response boundary, then the FORK runs its OWN real response:
//     the case's chatcmpl is the fork's own call, and a response written to the parent AFTER the
//     fork is proven absent from the child's history and journal (the future is isolated, §22.8).
//   - cancel acts on a real IN-FLIGHT run: a long generation is canceled after its model call
//     starts, so the aborted run commits no result of its own. Its receipt carries the session's
//     prior completed call's chatcmpl (the real-wire proof), disclosed in the assertions.
//
// pause/resume are NOT here: a cooperative pause on a single-step real run is structurally
// un-observable (its only boundary is the terminal), so they are proven deterministically only —
// the honest naming the T6 journey also keeps.
//
// Skipped without PALAI_UAT_PROVIDER=provider-one + OPENAI_API_KEY (loaded from .env.local by the
// operator entry); the credential rides env only and is asserted absent from the evidence bundle.
func TestLocalLiveLifecycle(t *testing.T) {
	requireDocker(t)
	if os.Getenv("PALAI_UAT_PROVIDER") != "provider-one" {
		t.Skip("live lifecycle smoke needs PALAI_UAT_PROVIDER=provider-one and OPENAI_API_KEY (run make uat-local-live PROVIDER=provider-one)")
	}
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		t.Fatal("OPENAI_API_KEY is unset; the operator entry loads it from .env.local")
	}

	// newUATStack registers its own teardown (t.Cleanup), so no defer reset here.
	s := newUATStack(t, "provider-one", key)

	proofs := []liveProof{
		s.proveForkSession(t),
		s.proveCloseSession(t),
		s.proveCancelInFlight(t),
	}

	// Evidence bundle: three live-provider receipts carrying the real chatcmpl ids, credential
	// proven absent (0 secret findings), verified by the ^chatcmpl- rule.
	s.writeAndVerifyLiveProviderEvidence(t, key, "-lifecycle", proofs)
}

// proveForkSession drives the fork_session proof (spec §22.8): a parent session runs a real
// completed response-1, then fork_session opens a fresh active child that reference-copies the
// parent's pre-fork history byte-for-byte. The fork then runs its OWN real response to completion
// (its own genuine chatcmpl — the receipt's real-wire proof that the child is a LIVE session, not a
// dead copy), and a response written to the PARENT after the fork is proven absent from the child's
// history and journal: the fork's future is isolated. Returns the fork's own run, model, chatcmpl.
func (s *uatStack) proveForkSession(t *testing.T) liveProof {
	t.Helper()
	// Parent runs a real completed response-1 — the pre-fork history the child will inherit.
	parentID := s.createSession()
	anchorResp, anchorRun := s.createResponseInSession(parentID, lifecycleAnchorPrompt)
	if final := s.awaitTerminal(anchorResp, 120*time.Second); final.Status != "completed" {
		t.Fatalf("fork: parent response-1 %s status = %q, want completed", anchorResp, final.Status)
	}
	_, anchorChat := s.modelCall(anchorRun)
	if !liveProviderIDPattern.MatchString(anchorChat) {
		t.Fatalf("fork: parent response-1 provider request id %q is not a genuine chatcmpl id", anchorChat)
	}

	// Fork the session at the completed-response boundary; the command result carries the child id.
	status, result := s.submitLifecycleCommand(parentID, "fork_session")
	if status != "applied" {
		t.Fatalf("fork_session status = %q, want applied", status)
	}
	childID, _ := result["session_id"].(string)
	if childID == "" || childID == parentID {
		t.Fatalf("fork_session result session_id = %q, want a fresh child id (parent %q)", childID, parentID)
	}
	// The child is a fresh ACTIVE session that reference-copied exactly the parent's one pre-fork
	// response, byte-for-byte (faithful to the REAL call, not a placeholder).
	if st := s.query(fmt.Sprintf("SELECT state FROM sessions WHERE id='%s'", childID)); st != "active" {
		t.Fatalf("fork child session %s state = %q, want active", childID, st)
	}
	if n := s.query(fmt.Sprintf("SELECT count(*) FROM responses WHERE session_id='%s'", childID)); n != "1" {
		t.Fatalf("fork child response count = %q, want 1 (the pre-fork history copy)", n)
	}
	parentOut := s.query(fmt.Sprintf("SELECT output::text FROM responses WHERE id='%s'", anchorResp))
	childOut := s.query(fmt.Sprintf("SELECT output::text FROM responses WHERE session_id='%s'", childID))
	if childOut == "" || childOut != parentOut {
		t.Fatalf("fork child copied output = %q, want the parent's real output %q", childOut, parentOut)
	}

	// The fork is a LIVE session: it runs its OWN real response to completion with its own chatcmpl.
	forkResp, forkRun := s.createResponseInSession(childID, "Reply with exactly one word and nothing else: BEACON.")
	forkFinal := s.awaitTerminal(forkResp, 120*time.Second)
	if forkFinal.Status != "completed" {
		t.Fatalf("fork: child response %s status = %q, want completed", forkResp, forkFinal.Status)
	}
	// The fork's own real call produced real OUTPUT, not just a chatcmpl id: its committed response
	// carries a non-empty generated content item. The exact word is model-chosen and steered by the
	// inherited history (the child carries the parent's LIGHTHOUSE turn, so the real answer here is
	// "LIGHT."-ish, not the asked BEACON) — assert real content, not a fixed token.
	var forkContent string
	if len(forkFinal.Output) > 0 {
		forkContent, _ = forkFinal.Output[0]["content"].(string)
	}
	if strings.TrimSpace(forkContent) == "" {
		t.Fatalf("fork: child response %s carried no real output token, only a chatcmpl id — got %+v", forkResp, forkFinal.Output)
	}
	model, chat := s.modelCall(forkRun)
	if !liveProviderIDPattern.MatchString(chat) {
		t.Fatalf("fork: child provider request id %q is not a genuine chatcmpl id", chat)
	}
	// The fork made its OWN real generation: its chatcmpl is distinct from the parent anchor's call.
	if chat == anchorChat {
		t.Fatalf("fork: child chatcmpl %s equals the parent anchor's — the fork did not make its own real generation", chat)
	}

	// Isolated future: a response written to the PARENT after the fork must never reach the child.
	postForkResp, _ := s.createResponseInSession(parentID, "Reply with exactly one word and nothing else: DIVERGE.")
	if final := s.awaitTerminal(postForkResp, 120*time.Second); final.Status != "completed" {
		t.Fatalf("fork: parent post-fork response %s status = %q, want completed", postForkResp, final.Status)
	}
	// The post-fork parent write is absent from the child's history AND its journal, and the child
	// holds only its pre-fork copy + its own run (2) — the copy happened once, at the fork boundary.
	if n := s.query(fmt.Sprintf("SELECT count(*) FROM responses WHERE session_id='%s' AND id='%s'", childID, postForkResp)); n != "0" {
		t.Fatalf("fork child leaked the post-fork parent response %s — the future is not isolated", postForkResp)
	}
	if n := s.query(fmt.Sprintf("SELECT count(*) FROM events WHERE session_id='%s' AND response_id='%s'", childID, postForkResp)); n != "0" {
		t.Fatalf("fork child journal contains the post-fork parent response %s — the future is not isolated", postForkResp)
	}
	if n := s.query(fmt.Sprintf("SELECT count(*) FROM responses WHERE session_id='%s'", childID)); n != "2" {
		t.Fatalf("fork child response count = %q after its own run, want 2 (pre-fork copy + the fork's own run)", n)
	}

	fmt.Printf("T4-FORK: forked %s -> child %s; fork ran its own real response (%s); post-fork parent write %s stayed isolated\n", parentID, childID, chat, postForkResp)
	return liveProof{"T4-FORK", "live-fork-session", forkRun, model, chat, []string{
		"parent ran a real completed response-1 (the pre-fork history)",
		"fork_session applied; child session active, reference-copied the parent's real pre-fork output (byte-equal, §22.8)",
		"the fork ran its OWN real response to completion; provider_request_id is the fork's own chatcmpl, distinct from the parent anchor's call (live child session)",
		"the fork's committed response carried a real output token, not just a chatcmpl id (its own live, history-steered generation)",
		"a response written to the parent AFTER the fork is absent from the child's history and journal (isolated future)",
	}, "run.completed"}
}

// proveCloseSession drives the close_session proof (spec §22.1): a session runs a real completed
// response, then close_session drives it to closed and a closed session rejects new work — a new
// response is a 409 and a new command is a typed rejection. Returns the anchor call's identity.
func (s *uatStack) proveCloseSession(t *testing.T) liveProof {
	t.Helper()
	sessionID := s.createSession()
	respID, runID := s.createResponseInSession(sessionID, lifecycleAnchorPrompt)
	if final := s.awaitTerminal(respID, 120*time.Second); final.Status != "completed" {
		t.Fatalf("close: anchor response %s status = %q, want completed", respID, final.Status)
	}
	model, chat := s.modelCall(runID)
	if !liveProviderIDPattern.MatchString(chat) {
		t.Fatalf("close: anchor provider request id %q is not a genuine chatcmpl id", chat)
	}

	// Close the idle session (its anchor run is terminal, so it goes straight to closed).
	if status, _ := s.submitLifecycleCommand(sessionID, "close_session"); status != "applied" {
		t.Fatalf("close_session status = %q, want applied", status)
	}
	if st := s.query(fmt.Sprintf("SELECT state FROM sessions WHERE id='%s'", sessionID)); st != "closed" {
		t.Fatalf("session state after close = %q, want closed", st)
	}

	// A new response on the closed session is a 409 (admission gate, spec §22.1).
	rejectBody := fmt.Sprintf(`{"input":"after close","session_id":%q}`, sessionID)
	var problem struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(s.post("/v1/responses", rejectBody, http.StatusConflict), &problem)
	if problem.Code != "session_not_active" {
		t.Fatalf("new response on closed session code = %q, want session_not_active", problem.Code)
	}
	// A new command on the closed session is a typed rejection, not a silently-queued command.
	cmdID, status := s.submitSendMessage(sessionID, "queue", "after close")
	if status != "rejected" {
		t.Fatalf("send_message on closed session status = %q, want rejected", status)
	}
	if code := s.query(fmt.Sprintf("SELECT result->>'code' FROM commands WHERE id='%s'", cmdID)); code != "session_not_active" {
		t.Fatalf("rejection code = %q, want session_not_active", code)
	}

	fmt.Printf("T4-CLOSE: closed %s; closed session rejected a new response (409) and a new command (%s)\n", sessionID, chat)
	return liveProof{"T4-CLOSE", "live-close-session", runID, model, chat, []string{
		"runs.state=completed (real anchor call)",
		"model_requests.provider_request_id present (chatcmpl)",
		"close_session applied; session state=closed (§22.1)",
		"closed session rejects a new response (409 session_not_active) and a new command (typed rejection)",
	}, "run.completed"}
}

// proveCancelInFlight drives the cancel proof (spec §22.3): a session runs a real completed anchor
// call (its chatcmpl is the receipt's real-wire proof), then a second chained long generation is
// canceled after its real model call starts. The aborted run reaches the canonical canceled
// terminal, and a repeated cancel is a monotonic no-op. The canceled run commits no result of its
// own, so the receipt carries the anchor call's chatcmpl (disclosed in the assertions).
func (s *uatStack) proveCancelInFlight(t *testing.T) liveProof {
	t.Helper()
	sessionID := s.createSession()
	anchorResp, anchorRun := s.createResponseInSession(sessionID, lifecycleAnchorPrompt)
	if final := s.awaitTerminal(anchorResp, 120*time.Second); final.Status != "completed" {
		t.Fatalf("cancel: anchor response %s status = %q, want completed", anchorResp, final.Status)
	}
	model, chat := s.modelCall(anchorRun)
	if !liveProviderIDPattern.MatchString(chat) {
		t.Fatalf("cancel: anchor provider request id %q is not a genuine chatcmpl id", chat)
	}

	// A second, chained long generation: cancel it once its real model call is in flight.
	respID, runID := s.createResponseInSession(sessionID, longGenerationPrompt)
	if !s.awaitModelRequest(runID, 60*time.Second) {
		t.Fatalf("cancel: run %s never started a model call — no in-flight window to cancel", runID)
	}
	s.post("/v1/responses/"+respID+"/cancel", "", http.StatusAccepted)

	// The run reaches the canonical canceled terminal: run row canceled, GET canceled with the
	// canceled problem, run.canceled.v1 the last journaled event for this response.
	final := s.awaitTerminal(respID, 60*time.Second)
	if final.Status != "canceled" {
		t.Fatalf("cancel: response %s status = %q, want canceled", respID, final.Status)
	}
	if st := s.query(fmt.Sprintf("SELECT state FROM runs WHERE id='%s'", runID)); st != "canceled" {
		t.Fatalf("cancel: run %s state = %q, want canceled", runID, st)
	}
	if n := s.query(fmt.Sprintf(
		"SELECT count(*) FROM events WHERE response_id='%s' AND type='run.canceled.v1'", respID)); n != "1" {
		t.Fatalf("cancel: run.canceled.v1 count for response %s = %q, want 1 (the single terminal)", respID, n)
	}

	// Cancel is monotonic: a repeated cancel is a no-op — still 202, still canceled, no second
	// terminal event.
	s.post("/v1/responses/"+respID+"/cancel", "", http.StatusAccepted)
	if n := s.query(fmt.Sprintf(
		"SELECT count(*) FROM events WHERE response_id='%s' AND type='run.canceled.v1'", respID)); n != "1" {
		t.Fatalf("cancel: run.canceled.v1 count after re-cancel = %q, want 1 (monotonic, no second terminal)", n)
	}

	fmt.Printf("T4-CANCEL: canceled in-flight run %s (session anchor call %s)\n", runID, chat)
	return liveProof{"T4-CANCEL", "live-cancel-response", runID, model, chat, []string{
		"session anchor response completed on the real provider; provider_request_id is that call's chatcmpl (real-wire proof)",
		"the second chained long generation was canceled after its real model call started",
		"run.state=canceled; GET=canceled with the canonical canceled problem; run.canceled.v1 the single terminal (§22.3)",
		"repeated cancel is a monotonic no-op (no second terminal event)",
	}, "run.canceled"}
}

// submitLifecycleCommand posts a bare session-lifecycle command (fork_session|close_session) and
// returns its accepted status and result payload (fork_session carries the child session id).
func (s *uatStack) submitLifecycleCommand(sessionID, kind string) (status string, result map[string]any) {
	body := fmt.Sprintf(`{"command_id":%q,"kind":%q}`, fmt.Sprintf("cmd-%d", uatIdem.Add(1)), kind)
	var r struct {
		Status string         `json:"status"`
		Result map[string]any `json:"result"`
	}
	_ = json.Unmarshal(s.post("/v1/sessions/"+sessionID+"/commands", body, http.StatusAccepted), &r)
	return r.Status, r.Result
}

// awaitModelRequest polls until a run has a model_requests row — its real provider call has been
// persisted and is in flight — so a caller can cancel within the call's window. Returns false on
// timeout.
func (s *uatStack) awaitModelRequest(runID string, within time.Duration) bool {
	s.t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if s.query(fmt.Sprintf("SELECT count(*) FROM model_requests WHERE run_id='%s'", runID)) != "0" {
			return true
		}
		time.Sleep(150 * time.Millisecond)
	}
	return false
}
