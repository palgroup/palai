//go:build uat

package uat

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// longGenerationPrompt keeps the single real model step outstanding for many seconds, so a command
// injected mid-run is durably queued while the call is in flight (the window the fast real wire
// needs).
const longGenerationPrompt = "Write a detailed 600-word essay about the history of lighthouses. Number every sentence."

// TestLocalLiveCommandSpine is the retroactive E08 Task 2 live smoke (spec §9.2, §22.4, §25.11):
// three proofs that the durable command spine runs against real infrastructure.
//
//  1. INTERRUPT (mid-generation) — delivery on the wire. On the real provider every run is single
//     step (no tools → no tool call → no `continues` boundary), so queue/steer never reach
//     pumpCommands and cannot be delivered live; only interrupt can, because handleInterrupt
//     manufactures its own second step by aborting the first in-flight real call (the 25ms watcher
//     cancels the model ctx → the engine resumes in a new model.request folding the delivered
//     message). A long real generation is interrupted after some tokens stream: a partial step is
//     journaled (model_step.interrupted.v1), the command applies, and the redirect folds into the
//     RESUMED real call whose output carries the redirect token.
//  2. INTERRUPT-FAST (pre-token) — the empty-partial fix on the wire. An interrupt injected before
//     the first token streams (empty partial) must complete cleanly, not fail: it used to break the
//     resumed real call (a content-less assistant turn OpenAI rejects) — the regression this smoke
//     surfaced and the empty-partial engine fix (d6321ac) closes.
//  3. QUEUE-EXPIRED — the §22.4 lifecycle on a live run. A queued send_message accepted against a
//     live real run is swept to `expired` when the single-step run terminalizes (accept-on-a-live-
//     run + the M2 sweep against real infrastructure; additive, not delivery-reflected).
//
// ponytail: steer/queue delivery-fold stays deterministic-only — single-step live delivery is
// impossible by design (pumpCommands needs a tool boundary the real provider never emits).
//
// Skipped without PALAI_UAT_PROVIDER=provider-one + OPENAI_API_KEY (the operator entry loads it
// from .env.local); the credential rides env only and is asserted absent from the evidence bundle.
func TestLocalLiveCommandSpine(t *testing.T) {
	requireDocker(t)
	if os.Getenv("PALAI_UAT_PROVIDER") != "provider-one" {
		t.Skip("live command-spine smoke needs PALAI_UAT_PROVIDER=provider-one and OPENAI_API_KEY (run make uat-local-live PROVIDER=provider-one)")
	}
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		t.Fatal("OPENAI_API_KEY is unset; the operator entry loads it from .env.local")
	}

	// newUATStack registers its own teardown (t.Cleanup), so no defer reset here.
	s := newUATStack(t, "provider-one", key)

	proofs := []liveProof{
		s.proveInterrupt(t, "T2-INTERRUPT", "live-interrupt-delivery",
			"Disregard the essay. Reply with exactly one word and nothing else: PINEAPPLE.", "PINEAPPLE", true),
		s.proveInterrupt(t, "T2-INTERRUPT-FAST", "live-interrupt-pre-token",
			"Stop. Ignore that. Reply with exactly one word and nothing else: MANGO.", "MANGO", false),
		s.proveQueueExpiryLifecycle(t),
	}

	// Evidence bundle: three live-provider receipts carrying the real chatcmpl ids, credential
	// proven absent (0 secret findings), verified by the ^chatcmpl- rule.
	s.writeAndVerifyLiveProviderEvidence(t, key, "-command-spine", proofs)
}

// liveProof is one live-provider case's captured identity and DB assertions for the evidence bundle.
type liveProof struct {
	id, name, runID, model, chat string
	assertions                   []string
}

// proveInterrupt drives one real interrupt proof: it admits a long generation and injects an
// interrupt once the model step has started — after ~2s of streaming when waitForStream is true (a
// mid-generation, non-empty partial), or immediately when false (a pre-token, empty partial) — then
// asserts the run COMPLETES with the redirect token folded into the resumed real call. It returns
// the resumed step's run id, model, and genuine chatcmpl id.
func (s *uatStack) proveInterrupt(t *testing.T, id, name, redirect, token string, waitForStream bool) liveProof {
	t.Helper()
	sessionID := s.createSession()
	respID, runID := s.createResponseInSession(sessionID, longGenerationPrompt)

	if !s.awaitEvent(sessionID, "model_step.created.v1", 60*time.Second) {
		t.Fatalf("%s: run %s never started a model step — could not inject a mid-run interrupt", id, runID)
	}
	if waitForStream {
		// Let real tokens stream so the partial assistant turn carries content (mid-generation).
		// ponytail: fixed delay; gpt-4o-mini TTFT is well under 2s. Widen it, not a token-poll, if
		// it ever proves tight.
		time.Sleep(2 * time.Second)
	}
	cmdID, status := s.submitSendMessage(sessionID, "interrupt", redirect)
	if status != "queued" {
		t.Fatalf("%s command status = %q, want queued (accepted against the live run)", id, status)
	}

	final := s.awaitTerminal(respID, 120*time.Second)
	if final.Status != "completed" {
		// Interrupt fired but the run did not complete — a real-provider break the deterministic
		// test cannot catch (the pre-token case is exactly the empty-partial regression). Report.
		t.Fatalf("%s: response %s status = %q, want completed — real interrupt/resume did not complete", id, respID, final.Status)
	}

	// A partial step was journaled for the aborted in-flight call (interrupted, not failed —
	// spec §25.11): proof a REAL provider call was canceled mid-flight, not a clean finish.
	if n := s.query(fmt.Sprintf(
		"SELECT count(*) FROM events WHERE session_id='%s' AND type='model_step.interrupted.v1'", sessionID)); n == "0" {
		t.Fatalf("%s: no model_step.interrupted.v1 journaled — the interrupt did not abort the in-flight call", id)
	}
	// The command actually took effect — applied, not expired/degraded.
	if st := s.query(fmt.Sprintf(
		"SELECT state FROM commands WHERE id='%s' AND session_id='%s'", cmdID, sessionID)); st != "applied" {
		t.Fatalf("%s command state = %q, want applied (the delivered message was consumed)", id, st)
	}
	// The delivered content steered the resumed REAL call: its output carries the redirect token.
	if answer, _ := json.Marshal(final.Output); !strings.Contains(string(answer), token) {
		t.Fatalf("%s: resumed output %s does not carry the redirect token %q — the delivered message did not steer the resumed real call", id, redactBytes(string(answer), s.secret), token)
	}

	// result IS NOT NULL LIMIT 1 → the resumed step (the aborted step committed no result).
	model, chat := s.modelCall(runID)
	if !liveProviderIDPattern.MatchString(chat) {
		t.Fatalf("%s: resumed provider request id %q is not a genuine chatcmpl id", id, chat)
	}
	fmt.Printf("%s: interrupt applied, resumed %s (%s) steered to %s\n", id, model, chat, token)
	return liveProof{id, name, runID, model, chat, []string{
		"runs.state=completed",
		"model_requests.result.model=" + model,
		"model_requests.provider_request_id present (chatcmpl)",
		"model_step.interrupted.v1 journaled (real in-flight abort)",
		"command applied; redirect folded into the resumed call (output carried the token)",
	}}
}

// proveQueueExpiryLifecycle drives the queue→expired proof: a send_message queued on a live real
// run is swept to expired when the single-step run terminalizes (§22.4). Returns the completed run.
func (s *uatStack) proveQueueExpiryLifecycle(t *testing.T) liveProof {
	t.Helper()
	sessionID := s.createSession()
	respID, runID := s.createResponseInSession(sessionID, longGenerationPrompt)

	// Queue a send_message while the real run is live (a model step has started), so it is accepted
	// (a live root run exists) rather than rejected no_active_run.
	if !s.awaitEvent(sessionID, "model_step.created.v1", 60*time.Second) {
		t.Fatalf("queue: run %s never started a model step — could not queue against a live run", runID)
	}
	cmdID, status := s.submitSendMessage(sessionID, "queue", "this message is never delivered on a single-step run")
	if status != "queued" {
		t.Fatalf("queue command status = %q, want queued (accepted against the live run)", status)
	}

	if final := s.awaitTerminal(respID, 120*time.Second); final.Status != "completed" {
		t.Fatalf("queue: response %s status = %q, want completed", respID, final.Status)
	}

	// The single-step run had no delivery boundary, so §22.4 sweeps the queued command to expired.
	if st := s.query(fmt.Sprintf(
		"SELECT state FROM commands WHERE id='%s' AND session_id='%s'", cmdID, sessionID)); st != "expired" {
		t.Fatalf("queued command state = %q, want expired (§22.4 sweep on the terminalized single-step run)", st)
	}
	if n := s.query(fmt.Sprintf(
		"SELECT count(*) FROM events WHERE session_id='%s' AND type='command.expired.v1'", sessionID)); n == "0" {
		t.Fatalf("queue: no command.expired.v1 journaled for the swept command")
	}

	model, chat := s.modelCall(runID)
	if !liveProviderIDPattern.MatchString(chat) {
		t.Fatalf("queue: provider request id %q is not a genuine chatcmpl id", chat)
	}
	fmt.Printf("T2-QUEUE-EXPIRED: queued on live run, swept to expired; run %s (%s)\n", model, chat)
	return liveProof{"T2-QUEUE-EXPIRED", "live-queue-expiry-lifecycle", runID, model, chat, []string{
		"runs.state=completed",
		"model_requests.provider_request_id present (chatcmpl)",
		"send_message queued on the live run, swept to expired (§22.4)",
		"command.expired.v1 journaled",
	}}
}

// submitSendMessage posts a send_message command with the given delivery (queue|steer|interrupt)
// and returns the command id and its accepted status.
func (s *uatStack) submitSendMessage(sessionID, delivery, message string) (commandID, status string) {
	commandID = fmt.Sprintf("cmd-%d", uatIdem.Add(1))
	body := fmt.Sprintf(`{"command_id":%q,"kind":"send_message","delivery":%q,"message":%q}`, commandID, delivery, message)
	var r struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(s.post("/v1/sessions/"+sessionID+"/commands", body, http.StatusAccepted), &r)
	return commandID, r.Status
}

// awaitEvent polls the journal until an event of the given type appears for the session, so a
// caller can wait for a durable execution milestone (e.g. the model step starting). Returns false
// on timeout.
func (s *uatStack) awaitEvent(sessionID, eventType string, within time.Duration) bool {
	s.t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if s.query(fmt.Sprintf("SELECT count(*) FROM events WHERE session_id='%s' AND type='%s'", sessionID, eventType)) != "0" {
			return true
		}
		time.Sleep(150 * time.Millisecond)
	}
	return false
}

// writeAndVerifyLiveProviderEvidence writes a live-provider evidence bundle from the captured
// proofs and verifies it clean: the ^chatcmpl- rule holds and the credential appears nowhere
// (0 secret findings). It reuses the shared manifest + verifier.
func (s *uatStack) writeAndVerifyLiveProviderEvidence(t *testing.T, key, suffix string, proofs []liveProof) {
	t.Helper()
	release := envOr("PALAI_UAT_RELEASE", "local-live-0.1.0") + suffix
	digest, enroll := s.engineImageDigest(), redactBytes(s.enrollRecord(), s.secret)
	specs := make([]caseSpec, 0, len(proofs))
	receipts := map[string]caseReceipt{}
	for _, p := range proofs {
		specs = append(specs, caseSpec{ID: p.id, Name: p.name, ProofClass: "live-provider", Provider: "provider-one", ExpectStatus: "completed"})
		receipts[p.id] = caseReceipt{
			RunID: p.runID, ImageDigest: digest, ProviderRequestID: redactBytes(p.chat, s.secret), MTLSEnroll: enroll,
			TerminalType: "run.completed", TerminalCount: 1, Usage: map[string]int{},
			DBAssertions: p.assertions, Checksum: hashBundle(p.runID, p.model, p.chat),
		}
	}

	manifest := buildManifest(t, release, specs, receipts)
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
	summary, err := VerifyRelease(dir, []string{key})
	if err != nil {
		t.Fatalf("verify %s bundle: %v", release, err)
	}
	fmt.Printf("evidence (%s): %s\n", release, summary.String())
	if !summary.OK() || summary.SecretFindings != 0 {
		t.Fatalf("%s evidence did not verify clean: %v", release, summary.Findings)
	}
}
