//go:build uat

package uat

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestInteractiveLiveJourney is the E08 Task 6 Milestone 2 live journey (spec §9, §22, §25): ONE
// real-OpenAI run through a single durable session that chains every T1-T5 feature live and captures
// the interactive-0.1.0 evidence bundle. It is NOT named TestLocalLive* so make uat-local-live stays
// untouched (the make uat-interactive target is Milestone 3).
//
// HONEST NAMING (E08 exit-gate discipline; T2 finding, command_spine_test.go:40-41): on the real
// provider every run is single step (no tools → no continues boundary), so a mid-run STEER can never
// reach pumpCommands and its applied_sequence never lands live — the same structurally-un-observable
// class as pause/resume (lifecycle_test.go). This journey therefore does NOT claim a live steer.
// The "a mid-run command's applied_sequence lands BETWEEN two real model steps" proof is carried by
// INTERRUPT, which genuinely does it: handleInterrupt records model_step.interrupted.v1 then applies
// the command (command.applied.v1 with applied_sequence, coordinator/commands.go), and the run
// resumes into a second real model step. The steer boundary stays deterministic-only (M1 tier).
//
// The legs, all on one session, each with a genuine chatcmpl receipt:
//   - response-1 on the OLD model (chaining root; the codename lives in its OUTPUT so history carry
//     is genuine, not prompt-fed);
//   - NORMAL model switch (change_config queued) then response-2 via previous_response_id: two
//     consecutive model_requests rows prove old→new, and response-2 echoes the codename (history
//     carried across the chain);
//   - INTERRUPT of a long real generation: applied_sequence lands between the interrupted and the
//     resumed real model step; the resumed call carries the redirect token;
//   - REQUIRED child (SUB-002) on the cheaper OLD model id while the session runs on the switched
//     NEW model: parent and child reach real terminals on two distinct chatcmpl ids and two distinct
//     model ids; the parent's projection links the child run;
//   - two authorized clients attach and observe the identical ordered journal (equal journal hash),
//     SES-001 live;
//   - CANCEL of an in-flight long generation → the canonical canceled terminal;
//   - FORK the session, run a short real response-3 in the fork, then CLOSE the session (rejects new
//     work).
//
// Skipped without PALAI_UAT_PROVIDER=provider-one + OPENAI_API_KEY (the operator entry loads it from
// .env.local); the credential rides env only and is asserted absent from the evidence bundle.
func TestInteractiveLiveJourney(t *testing.T) {
	requireDocker(t)
	if os.Getenv("PALAI_UAT_PROVIDER") != "provider-one" {
		t.Skip("live interactive journey needs PALAI_UAT_PROVIDER=provider-one and OPENAI_API_KEY (run the operator entry that loads .env.local)")
	}
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		t.Fatal("OPENAI_API_KEY is unset; the operator entry loads it from .env.local")
	}

	// The switch goes OLD→NEW so the session ends on NEW (gpt-4o); the required child then routes the
	// cheaper OLD id (gpt-4o-mini), a genuinely distinct model — one switch sets up both legs.
	oldModel := envOr("PALAI_MODEL", "gpt-4o-mini")
	newModel := envOr("PALAI_UAT_SWITCH_MODEL", "gpt-4o")
	childModel := oldModel
	if newModel == oldModel {
		t.Fatalf("switch target %q must differ from the deployment model %q", newModel, oldModel)
	}
	// Inline delegation holds the parent's engine while the child dials its own, so the runner needs
	// 2 concurrent lease slots (T5's option A; the default stays 1 for every non-delegating stack).
	t.Setenv("PALAI_RUNNER_CONCURRENCY", envOr("PALAI_RUNNER_CONCURRENCY", "2"))

	// newUATStack registers its own teardown (t.Cleanup), so no defer reset here.
	s := newUATStack(t, "provider-one", key)
	var proofs []liveProof

	sessionID := s.createSession()

	// --- response-1: chaining root on the OLD model. The codename lives in the OUTPUT (history
	// carries the prior response's output, not the prompt), so a later echo is genuine proof.
	const codeword = "Quokka-4417"
	r1, run1 := s.createResponseInSession(sessionID, "Reply with exactly this sentence and nothing else: The project codename is "+codeword+".")
	if st := s.awaitTerminal(r1, 120*time.Second); st.Status != "completed" {
		t.Fatalf("response-1 status = %q, want completed", st.Status)
	}
	model1, chat1 := s.modelCall(run1)
	if !strings.HasPrefix(model1, oldModel) {
		t.Fatalf("response-1 model = %q, want the deployment default %q…", model1, oldModel)
	}
	proofs = append(proofs, liveProof{"INT-CHAIN-OLD", "live-chaining-root-old-model", run1, model1, chat1, []string{
		"runs.state=completed on the real provider (chaining root)",
		"model_requests.result.model=" + model1 + " (deployment default, the OLD model)",
		"model_requests.provider_request_id present (chatcmpl)",
		"the codename lives in the response OUTPUT, so the response-2 echo is a genuine history-carry proof",
	}, "run.completed"})

	// --- NORMAL model switch (queued), then response-2 via previous_response_id proves BOTH the
	// old→new switch (consecutive model_requests rows) and history carry (the codename echoes).
	if status := s.submitChangeConfig(sessionID, newModel); status != "queued" {
		t.Fatalf("change_config status = %q, want queued (accepted on the live session)", status)
	}
	r2, run2 := s.createResponseWithPrevious(r1, "What is the project codename I told you? Reply with only the codename and nothing else.")
	st2 := s.awaitTerminal(r2, 120*time.Second)
	if st2.Status != "completed" {
		t.Fatalf("response-2 status = %q, want completed", st2.Status)
	}
	if sess := s.query(fmt.Sprintf("SELECT session_id FROM responses WHERE id='%s'", r2)); sess != sessionID {
		t.Fatalf("response-2 session = %q, want the same session %q (previous_response_id chaining)", sess, sessionID)
	}
	if answer, _ := json.Marshal(st2.Output); !strings.Contains(string(answer), codeword) {
		t.Fatalf("response-2 output %s does not carry the response-1 codename %q — session history did not carry on the wire",
			redactBytes(string(answer), s.secret), codeword)
	}
	model2, chat2 := s.modelCall(run2)
	if !strings.HasPrefix(model2, newModel) || strings.HasPrefix(model2, oldModel) {
		t.Fatalf("response-2 model = %q, want the switched %q… (not the old %q)", model2, newModel, oldModel)
	}
	if chat1 == chat2 {
		t.Fatalf("response-1 and response-2 share chatcmpl %s — not two distinct real round-trips", chat1)
	}
	fmt.Printf("JOURNEY switch+history: %s (%s) -> %s (%s) carried %q\n", model1, chat1, model2, chat2, codeword)
	proofs = append(proofs, liveProof{"INT-SWITCH-HISTORY", "live-normal-switch-and-history-carry", run2, model2, chat2, []string{
		"runs.state=completed on the switched NEW model",
		"model_requests.result.model=" + model2 + " — the consecutive row after " + model1 + " proves the normal switch old→new (§9.3)",
		"model_requests.provider_request_id present (chatcmpl), distinct from response-1",
		"response-2 was created via previous_response_id and its OUTPUT echoed the response-1 codename (history carried, §22.2)",
	}, "run.completed"})

	// --- INTERRUPT: a long real generation, interrupted mid-flight; its applied_sequence lands
	// between the interrupted and the resumed real model step (the honest live "mid-run command
	// between real model steps" proof — via interrupt, not steer).
	r3, run3 := s.createResponseWithPrevious(r2, longGenerationPrompt)
	if !s.awaitModelRequest(run3, 60*time.Second) {
		t.Fatalf("interrupt: run %s never started its real model call — no in-flight window", run3)
	}
	time.Sleep(2 * time.Second) // let real tokens stream so the partial assistant turn carries content
	const redirect = "Disregard the essay. Reply with exactly one word and nothing else: PINEAPPLE."
	cmdID, status := s.submitSendMessage(sessionID, "interrupt", redirect)
	if status != "queued" {
		t.Fatalf("interrupt command status = %q, want queued (accepted against the live run)", status)
	}
	if final := s.awaitTerminal(r3, 120*time.Second); final.Status != "completed" {
		t.Fatalf("interrupt: response %s status = %q, want completed — real interrupt/resume did not complete", r3, final.Status)
	}
	if n := s.query(fmt.Sprintf("SELECT count(*) FROM events WHERE response_id='%s' AND type='model_step.interrupted.v1'", r3)); n == "0" {
		t.Fatalf("interrupt: no model_step.interrupted.v1 journaled — the interrupt did not abort the in-flight call")
	}
	if cs := s.query(fmt.Sprintf("SELECT state FROM commands WHERE id='%s' AND session_id='%s'", cmdID, sessionID)); cs != "applied" {
		t.Fatalf("interrupt command state = %q, want applied", cs)
	}
	appliedSeq := s.query(fmt.Sprintf("SELECT applied_sequence FROM commands WHERE id='%s'", cmdID))
	if appliedSeq == "" || appliedSeq == "null" {
		t.Fatalf("interrupt applied_sequence is empty — the effect boundary was not journaled")
	}
	// The applied_sequence sits strictly between two REAL model steps: step 1 (interrupted) started
	// before it, step 2 (resumed) started after it.
	stepsBefore := s.query(fmt.Sprintf("SELECT count(*) FROM events WHERE response_id='%s' AND type='model_step.created.v1' AND seq < %s", r3, appliedSeq))
	stepsAfter := s.query(fmt.Sprintf("SELECT count(*) FROM events WHERE response_id='%s' AND type='model_step.created.v1' AND seq > %s", r3, appliedSeq))
	if stepsBefore == "0" || stepsAfter == "0" {
		t.Fatalf("interrupt applied_sequence=%s is not between two real model steps (before=%s after=%s)", appliedSeq, stepsBefore, stepsAfter)
	}
	if ans, _ := json.Marshal(s.awaitTerminal(r3, 120*time.Second).Output); !strings.Contains(string(ans), "PINEAPPLE") {
		t.Fatalf("interrupt: resumed output %s does not carry the redirect token — the delivered message did not steer the resumed real call", redactBytes(string(ans), s.secret))
	}
	model3, chat3 := s.modelCall(run3)
	if !liveProviderIDPattern.MatchString(chat3) {
		t.Fatalf("interrupt: resumed provider request id %q is not a genuine chatcmpl id", chat3)
	}
	fmt.Printf("JOURNEY interrupt: applied_sequence=%s between real steps (before=%s after=%s); resumed %s (%s)\n", appliedSeq, stepsBefore, stepsAfter, model3, chat3)
	proofs = append(proofs, liveProof{"INT-INTERRUPT", "live-interrupt-applied-sequence-between-real-steps", run3, model3, chat3, []string{
		"runs.state=completed after a real in-flight abort (model_step.interrupted.v1 journaled)",
		"command.applied.v1 applied_sequence=" + appliedSeq + " lands BETWEEN two real model_step.created.v1 events (§22.4, §25.11)",
		"model_requests.provider_request_id present (chatcmpl) — the resumed real call",
		"the delivered interrupt message steered the resumed call (output carried the redirect token)",
	}, "run.completed"})

	// --- REQUIRED child (SUB-002): the parent runs on the switched NEW model (gpt-4o via the session
	// config revision) and required-delegates a child on the cheaper OLD model id (gpt-4o-mini).
	delegations := fmt.Sprintf(`[{"role":"researcher","objective":"Reply with exactly one word and nothing else: ATLAS.","model":%q,"required":true}]`, childModel)
	r4, parentRun := s.createResponseWithDelegation(sessionID, "Reply with exactly one word and nothing else: SUMMIT.", delegations)
	if final := s.awaitTerminal(r4, 180*time.Second); final.Status != "completed" {
		s.dumpDelegationFailure(r4, parentRun)
		t.Fatalf("child: parent response %s status = %q, want completed", r4, final.Status)
	}
	childRun := s.childRunOf(parentRun)
	if childRun == "" {
		t.Fatal("child: no ChildRun was dispatched for the required delegation")
	}
	if cst := s.query(fmt.Sprintf("SELECT state FROM runs WHERE id='%s'", childRun)); cst != "completed" {
		t.Fatalf("child: child run %s state = %q, want completed", childRun, cst)
	}
	parentModel, parentChat := s.modelCall(parentRun)
	childModelUsed, childChat := s.modelCall(childRun)
	if !liveProviderIDPattern.MatchString(parentChat) || !liveProviderIDPattern.MatchString(childChat) {
		t.Fatalf("child: provider request ids are not genuine chatcmpl ids (parent=%q child=%q)", parentChat, childChat)
	}
	if parentChat == childChat {
		t.Fatalf("child: parent and child share chatcmpl %s — the child did not make its own real call", parentChat)
	}
	if !strings.HasPrefix(parentModel, newModel) || strings.HasPrefix(parentModel, childModel) {
		t.Fatalf("child: parent model = %q, want the switched NEW %q… (not the child's %q)", parentModel, newModel, childModel)
	}
	// Distinctness is anchored on the more-specific child id: gpt-4o is a prefix of gpt-4o-mini, so
	// the honest check is "child carries the child id AND the parent does NOT" (subagents_test.go).
	if !strings.HasPrefix(childModelUsed, childModel) || strings.HasPrefix(parentModel, childModel) {
		t.Fatalf("child: child model = %q, want the cheaper %q… (distinct from parent %q)", childModelUsed, childModel, parentModel)
	}
	if links := s.query(fmt.Sprintf("SELECT output->'child_runs' FROM responses WHERE id='%s'", r4)); links == "" || links == "null" {
		t.Fatalf("child: parent projection does not link the child run id (child_runs=%q)", links)
	}
	fmt.Printf("JOURNEY child: parent %s (%s) delegated to child %s (%s)\n", parentModel, parentChat, childModelUsed, childChat)
	proofs = append(proofs,
		liveProof{"INT-CHILD-PARENT", "live-required-delegation-parent", parentRun, parentModel, parentChat, []string{
			"parent runs.state=completed on the switched NEW model; provider_request_id is a chatcmpl",
			"the parent required-delegated a child and its terminal links the child run id (§25.19)",
			"parent model " + parentModel + " is distinct from the child's cheaper id (independent routing)",
		}, "run.completed"},
		liveProof{"INT-CHILD", "live-required-delegation-child", childRun, childModelUsed, childChat, []string{
			"child runs.state=completed as its OWN run on the real provider (SUB-002)",
			"child provider_request_id is a genuine chatcmpl, DISTINCT from the parent's",
			"child routed its own cheaper model id (" + childModel + "), not the parent's",
		}, "run.completed"})

	// --- SES-001 live: two authorized clients attach and observe the identical ordered journal.
	// Both replay from the start and close at the first run terminal (events.go ceiling: the stream
	// closes after a run terminal), so both see response-1's real journal byte-for-byte.
	hashA, framesA := s.attachJournalHash(t, sessionID)
	hashB, framesB := s.attachJournalHash(t, sessionID)
	if len(framesA) == 0 {
		t.Fatal("SES-001: client A saw an empty journal")
	}
	if last := framesA[len(framesA)-1]; !strings.HasPrefix(last.typ, "run.") || !strings.HasSuffix(last.typ, ".v1") {
		t.Fatalf("SES-001: client A last frame type = %q, want a run terminal (stream must drain through the terminal)", last.typ)
	}
	if len(framesA) != len(framesB) {
		t.Fatalf("SES-001: client A saw %d frames, client B saw %d — the journals diverge", len(framesA), len(framesB))
	}
	for i := range framesA {
		if framesA[i].id != framesB[i].id || framesA[i].payloadHash != framesB[i].payloadHash {
			t.Fatalf("SES-001: frame %d diverges: A=(%s,%s) B=(%s,%s)", i, framesA[i].id, framesA[i].payloadHash, framesB[i].id, framesB[i].payloadHash)
		}
	}
	if hashA != hashB {
		t.Fatalf("SES-001: two clients computed different journal hashes A=%s B=%s", hashA, hashB)
	}
	fmt.Printf("JOURNEY attach: two clients saw %d identical frames, journal hash %s\n", len(framesA), hashA)
	proofs = append(proofs, liveProof{"INT-ATTACH", "live-two-client-identical-journal", run1, model1, chat1, []string{
		"two authorized clients attached to the session and saw the identical ordered journal",
		fmt.Sprintf("both computed the identical journal hash sha256:%s over %d frames (SES-001)", hashA, len(framesA)),
		"the attached journal is response-1's real engine-driven run (its chatcmpl is the real-wire proof); the stream closes at the first run terminal",
	}, "run.completed"})

	// --- CANCEL an in-flight long generation → the canonical canceled terminal.
	r5, run5 := s.createResponseInSession(sessionID, longGenerationPrompt)
	if !s.awaitModelRequest(run5, 60*time.Second) {
		t.Fatalf("cancel: run %s never started a model call — no in-flight window to cancel", run5)
	}
	s.post("/v1/responses/"+r5+"/cancel", "", http.StatusAccepted)
	if final := s.awaitTerminal(r5, 60*time.Second); final.Status != "canceled" {
		t.Fatalf("cancel: response %s status = %q, want canceled", r5, final.Status)
	}
	if rs := s.query(fmt.Sprintf("SELECT state FROM runs WHERE id='%s'", run5)); rs != "canceled" {
		t.Fatalf("cancel: run %s state = %q, want canceled", run5, rs)
	}
	if n := s.query(fmt.Sprintf("SELECT count(*) FROM events WHERE response_id='%s' AND type='run.canceled.v1'", r5)); n != "1" {
		t.Fatalf("cancel: run.canceled.v1 count for response %s = %q, want 1 (the single terminal)", r5, n)
	}
	fmt.Printf("JOURNEY cancel: in-flight run %s reached the canceled terminal\n", run5)
	// The canceled run commits no result of its own, so the receipt carries a prior real same-session
	// call's chatcmpl (response-2's), disclosed in the assertions (the T4 cancel-receipt discipline).
	proofs = append(proofs, liveProof{"INT-CANCEL", "live-cancel-in-flight", run5, model2, chat2, []string{
		"a chained long generation was canceled after its real model call started",
		"run.state=canceled; run.canceled.v1 is the single terminal for the response (§22.3)",
		"the canceled run commits no result of its own; provider_request_id is a prior real same-session call (real-wire proof)",
	}, "run.canceled"})

	// --- FORK the session, then run a short real response-3 in the fork.
	fstatus, fresult := s.submitLifecycleCommand(sessionID, "fork_session")
	if fstatus != "applied" {
		t.Fatalf("fork_session status = %q, want applied", fstatus)
	}
	forkID, _ := fresult["session_id"].(string)
	if forkID == "" || forkID == sessionID {
		t.Fatalf("fork_session result session_id = %q, want a fresh child id (parent %q)", forkID, sessionID)
	}
	if fst := s.query(fmt.Sprintf("SELECT state FROM sessions WHERE id='%s'", forkID)); fst != "active" {
		t.Fatalf("fork child session %s state = %q, want active", forkID, fst)
	}
	r6, run6 := s.createResponseInSession(forkID, "Reply with exactly one word and nothing else: BEACON.")
	f6 := s.awaitTerminal(r6, 120*time.Second)
	if f6.Status != "completed" {
		t.Fatalf("fork: response-3 %s status = %q, want completed", r6, f6.Status)
	}
	var forkContent string
	if len(f6.Output) > 0 {
		forkContent, _ = f6.Output[0]["content"].(string)
	}
	if strings.TrimSpace(forkContent) == "" {
		t.Fatalf("fork: response-3 %s carried no real output token, only a chatcmpl id — got %+v", r6, f6.Output)
	}
	model6, chat6 := s.modelCall(run6)
	if !liveProviderIDPattern.MatchString(chat6) {
		t.Fatalf("fork: response-3 provider request id %q is not a genuine chatcmpl id", chat6)
	}
	fmt.Printf("JOURNEY fork: forked %s -> %s; the fork ran its own real response-3 (%s)\n", sessionID, forkID, chat6)
	proofs = append(proofs, liveProof{"INT-FORK", "live-fork-runs-own-response", run6, model6, chat6, []string{
		"fork_session applied; the child session is active (a reference copy of the pre-fork history, §22.8)",
		"the fork ran its OWN real response-3 to completion; provider_request_id is its own chatcmpl",
		"the fork's committed response carried a real output token, not just a chatcmpl id (a live child session)",
	}, "run.completed"})

	// --- CLOSE the session → a closed session rejects new work.
	if cstatus, _ := s.submitLifecycleCommand(sessionID, "close_session"); cstatus != "applied" {
		t.Fatalf("close_session status = %q, want applied", cstatus)
	}
	if cst := s.query(fmt.Sprintf("SELECT state FROM sessions WHERE id='%s'", sessionID)); cst != "closed" {
		t.Fatalf("session state after close = %q, want closed", cst)
	}
	var problem struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(s.post("/v1/responses", fmt.Sprintf(`{"input":"after close","session_id":%q}`, sessionID), http.StatusConflict), &problem)
	if problem.Code != "session_not_active" {
		t.Fatalf("new response on closed session code = %q, want session_not_active", problem.Code)
	}
	rcmdID, rstatus := s.submitSendMessage(sessionID, "queue", "after close")
	if rstatus != "rejected" {
		t.Fatalf("send_message on closed session status = %q, want rejected", rstatus)
	}
	if code := s.query(fmt.Sprintf("SELECT result->>'code' FROM commands WHERE id='%s'", rcmdID)); code != "session_not_active" {
		t.Fatalf("rejection code = %q, want session_not_active", code)
	}
	fmt.Printf("JOURNEY close: closed %s; it rejected a new response (409) and a new command (typed rejection)\n", sessionID)
	// close makes no model call; the receipt carries the session's interrupt-resumed call (chat3).
	proofs = append(proofs, liveProof{"INT-CLOSE", "live-close-rejects-new-work", run3, model3, chat3, []string{
		"close_session applied; session state=closed (§22.1)",
		"the closed session rejects a new response (409 session_not_active) and a new command (typed rejection)",
		"close makes no model call; provider_request_id is a prior real same-session call (real-wire proof)",
	}, "run.completed"})

	// Evidence bundle interactive-0.1.0: every leg's chatcmpl, the interrupt applied_sequence, the
	// two-client journal hash, the child run link — verified clean with the credential proven absent.
	s.writeAndVerifyInteractiveEvidence(t, key, proofs)
}

// createResponseWithPrevious admits a response chained by previous_response_id (continuing the prior
// response's session and history) and returns its id and run id.
func (s *uatStack) createResponseWithPrevious(prevRespID, input string) (respID, runID string) {
	s.t.Helper()
	body := fmt.Sprintf(`{"input":%q,"previous_response_id":%q}`, input, prevRespID)
	var r struct {
		ID    string `json:"id"`
		RunID string `json:"run_id"`
	}
	_ = json.Unmarshal(s.post("/v1/responses", body, http.StatusAccepted), &r)
	if r.ID == "" || r.RunID == "" {
		s.t.Fatalf("response create returned no id/run_id for previous_response_id %s", prevRespID)
	}
	return r.ID, r.RunID
}

// journalFrame is one SSE frame reduced to what the two-client equality proof compares: the durable
// per-session event id, its type, and a hash of the raw CloudEvents data line. The same durable rows
// serialize identically for every client, so two authorized readers hash to the same value.
type journalFrame struct {
	id, typ, payloadHash string
}

// attachJournalHash opens the session's SSE stream with the stack's own API key, drains it through
// the clean close the server emits after a run terminal (events.go), and returns the ordered frames
// plus a single sha256 over the (id, type, payloadHash) sequence — the journal hash SES-001 compares.
func (s *uatStack) attachJournalHash(t *testing.T, sessionID string) (string, []journalFrame) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, s.cfg.BaseURL+"/v1/sessions/"+sessionID+"/events", nil)
	if err != nil {
		t.Fatalf("build attach GET: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.apiKey())
	// A safety net only: response-1 is already terminal, so the replay-from-start hits its
	// run.completed.v1 and the server closes the stream well within this.
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("attach GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("attach status = %d, want 200", resp.StatusCode)
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var frames []journalFrame
	var f journalFrame
	var dataLine string
	got := false
	flush := func() {
		if !got {
			return
		}
		sum := sha256.Sum256([]byte(dataLine))
		f.payloadHash = hex.EncodeToString(sum[:])
		frames = append(frames, f)
		f, dataLine, got = journalFrame{}, "", false
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, ":"):
			// heartbeat comment — ignore
		case strings.HasPrefix(line, "id: "):
			f.id = strings.TrimPrefix(line, "id: ")
			got = true
		case strings.HasPrefix(line, "event: "):
			f.typ = strings.TrimPrefix(line, "event: ")
			got = true
		case strings.HasPrefix(line, "data: "):
			dataLine = strings.TrimPrefix(line, "data: ")
			got = true
		}
	}
	flush() // in case the terminal frame is not followed by a trailing blank line before EOF

	h := sha256.New()
	for _, fr := range frames {
		h.Write([]byte(fr.id))
		h.Write([]byte{0})
		h.Write([]byte(fr.payloadHash))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), frames
}

// writeAndVerifyInteractiveEvidence writes the interactive-0.1.0 bundle from the journey proofs and
// verifies it clean: the ^chatcmpl- rule holds and the credential appears nowhere (0 secret
// findings). It reuses the shared manifest + verifier; the release name is fixed so make
// evidence-verify RELEASE=interactive-0.1.0 (Milestone 3) reads it.
func (s *uatStack) writeAndVerifyInteractiveEvidence(t *testing.T, key string, proofs []liveProof) {
	t.Helper()
	release := envOr("PALAI_UAT_INTERACTIVE_RELEASE", "interactive-0.1.0")
	digest, enroll := s.engineImageDigest(), redactBytes(s.enrollRecord(), s.secret)
	specs := make([]caseSpec, 0, len(proofs))
	receipts := map[string]caseReceipt{}
	for _, p := range proofs {
		specs = append(specs, caseSpec{ID: p.id, Name: p.name, ProofClass: "live-provider", Provider: "provider-one", ExpectStatus: "completed"})
		receipts[p.id] = caseReceipt{
			RunID: p.runID, ImageDigest: digest, ProviderRequestID: redactBytes(p.chat, s.secret), MTLSEnroll: enroll,
			TerminalType: p.terminalType, TerminalCount: 1, Usage: map[string]int{},
			DBAssertions: p.assertions, Checksum: hashBundle(p.runID, p.model, p.chat),
		}
	}

	manifest := buildManifest(t, release, specs, receipts)
	dir := writeManifest(t, release, manifest)
	summary, err := VerifyRelease(dir, []string{key})
	if err != nil {
		t.Fatalf("verify %s bundle: %v", release, err)
	}
	fmt.Printf("evidence (%s): %s\n", release, summary.String())
	if !summary.OK() || summary.SecretFindings != 0 {
		t.Fatalf("%s evidence did not verify clean: %v", release, summary.Findings)
	}
}

// writeManifest marshals a manifest to evidence/releases/<release>/manifest.json and returns the
// directory. ponytail: the four earlier live-smoke writers inline this same block; this is the fifth
// call site, factored rather than copied — the earlier ones are left untouched (they are green).
func writeManifest(t *testing.T, release string, manifest map[string]any) string {
	t.Helper()
	dir := filepath.Join(repoRoot(t), "evidence", "releases", release)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("make release dir: %v", err)
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), append(raw, '\n'), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return dir
}
