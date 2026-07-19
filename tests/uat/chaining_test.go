//go:build uat

package uat

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLocalLiveChaining is the retroactive E08 Task 1 live smoke (spec §9, §22.2): two REAL
// responses chained in one session prove session history genuinely carries on the provider wire.
// Turn 1 puts a non-guessable codename in its OUTPUT — chaining carries the prior response's
// output as an assistant turn (history.go historyMessages), so the fact must live in the reply,
// not the prompt. Turn 2, chained by session_id, asks for that codename and can answer ONLY if
// turn 1's output folded into its context. A fresh-session control asks the same question with no
// history and must NOT be able to name it, so the follow-up assertion is a genuine history-carry
// discriminator, not a coincidence (the RED half, baked in). Two genuine, DISTINCT chatcmpl ids
// prove two real round-trips. Skipped without PALAI_UAT_PROVIDER=provider-one + OPENAI_API_KEY
// (the operator entry loads it from .env.local); the credential rides env only and is asserted
// absent from the evidence bundle.
func TestLocalLiveChaining(t *testing.T) {
	requireDocker(t)
	if os.Getenv("PALAI_UAT_PROVIDER") != "provider-one" {
		t.Skip("live chaining smoke needs PALAI_UAT_PROVIDER=provider-one and OPENAI_API_KEY (run make uat-local-live PROVIDER=provider-one)")
	}
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		t.Fatal("OPENAI_API_KEY is unset; the operator entry loads it from .env.local")
	}

	// newUATStack registers its own teardown (t.Cleanup), so no defer reset here.
	s := newUATStack(t, "provider-one", key)

	// Turn 1 establishes a non-guessable fact IN ITS OUTPUT (history carries the prior response's
	// output, not the prompt), so the codename must land in the reply the model returns.
	const codeword = "Quokka-4417"
	sessionID := s.createSession()
	resp1, run1 := s.createResponseInSession(sessionID, "Reply with exactly this sentence and nothing else: The project codename is "+codeword+".")
	if st := s.awaitTerminal(resp1, 120*time.Second); st.Status != "completed" {
		t.Fatalf("response 1 status = %q, want completed", st.Status)
	}

	// Turn 2 chains into the same session and asks for the codename. Without the carried history it
	// cannot know it; a correct answer is the semantic proof that session history carried on the wire.
	resp2, run2 := s.createResponseInSession(sessionID, "What is the project codename I told you? Reply with only the codename and nothing else.")
	st2 := s.awaitTerminal(resp2, 120*time.Second)
	if st2.Status != "completed" {
		t.Fatalf("response 2 status = %q, want completed", st2.Status)
	}
	answer, _ := json.Marshal(st2.Output)
	if !strings.Contains(string(answer), codeword) {
		t.Fatalf("response 2 output %s does not carry the turn-1 codename %q — session history did not carry on the wire",
			redactBytes(string(answer), s.secret), codeword)
	}

	// Negative control: the SAME question in a FRESH session (no chaining) must NOT be able to name
	// the codename. This proves the follow-up assertion is a genuine history-carry discriminator
	// rather than something the model would satisfy anyway (the RED half, baked into every run).
	control := s.createSession()
	resp3, _ := s.createResponseInSession(control, "What is the project codename I told you? Reply with only the codename and nothing else.")
	st3 := s.awaitTerminal(resp3, 120*time.Second)
	if st3.Status != "completed" {
		t.Fatalf("control response status = %q, want completed", st3.Status)
	}
	if unchained, _ := json.Marshal(st3.Output); strings.Contains(string(unchained), codeword) {
		t.Fatalf("control (unchained) output named the codename %q with no history — the follow-up assertion is not a genuine chaining discriminator", codeword)
	}

	// Two genuine, DISTINCT real round-trips carry the chained pair.
	model1, chat1 := s.modelCall(run1)
	model2, chat2 := s.modelCall(run2)
	for _, id := range []string{chat1, chat2} {
		if !liveProviderIDPattern.MatchString(id) {
			t.Fatalf("provider request id %q is not a genuine chatcmpl id", id)
		}
	}
	if chat1 == chat2 {
		t.Fatalf("both responses reported chatcmpl id %q — not two distinct real round-trips", chat1)
	}
	fmt.Printf("T1 live chaining: turn1 %s (%s) -> turn2 %s (%s) carried %q\n", model1, chat1, model2, chat2, codeword)

	// Evidence bundle: two live-provider receipts carrying the real chatcmpl ids, credential proven
	// absent (0 secret findings), verified by the ^chatcmpl- rule.
	s.writeAndVerifyChainingEvidence(t, key, run1, model1, chat1, run2, model2, chat2)
}

// writeAndVerifyChainingEvidence writes the chaining evidence bundle (two live-provider case
// receipts carrying the real chatcmpl ids) and verifies it clean: the ^chatcmpl- rule holds and
// the credential appears nowhere (0 secret findings). It reuses the shared manifest + verifier.
func (s *uatStack) writeAndVerifyChainingEvidence(t *testing.T, key, run1, model1, chat1, run2, model2, chat2 string) {
	t.Helper()
	release := envOr("PALAI_UAT_RELEASE", "local-live-0.1.0") + "-chaining"
	digest, enroll := s.engineImageDigest(), redactBytes(s.enrollRecord(), s.secret)
	receipt := func(runID, model, chat string, assertions []string) caseReceipt {
		return caseReceipt{
			RunID: runID, ImageDigest: digest, ProviderRequestID: redactBytes(chat, s.secret), MTLSEnroll: enroll,
			TerminalType: "run.completed", TerminalCount: 1, Usage: map[string]int{},
			DBAssertions: assertions,
			Checksum:     hashBundle(runID, model, chat),
		}
	}
	specs := []caseSpec{
		{ID: "T1-CHAIN-ROOT", Name: "live-chaining-root-turn", ProofClass: "live-provider", Provider: "provider-one", ExpectStatus: "completed"},
		{ID: "T1-CHAIN-FOLLOWUP", Name: "live-chaining-history-carry", ProofClass: "live-provider", Provider: "provider-one", ExpectStatus: "completed"},
	}
	receipts := map[string]caseReceipt{
		"T1-CHAIN-ROOT": receipt(run1, model1, chat1, []string{
			"runs.state=completed",
			"model_requests.result.model=" + model1,
			"model_requests.provider_request_id present (chatcmpl)",
		}),
		"T1-CHAIN-FOLLOWUP": receipt(run2, model2, chat2, []string{
			"runs.state=completed",
			"model_requests.result.model=" + model2,
			"model_requests.provider_request_id present (chatcmpl)",
			"response 2 output carried turn-1 session history (codename echoed)",
		}),
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
		t.Fatalf("verify chaining bundle: %v", err)
	}
	fmt.Printf("evidence (chaining): %s\n", summary.String())
	if !summary.OK() || summary.SecretFindings != 0 {
		t.Fatalf("chaining evidence did not verify clean: %v", summary.Findings)
	}
}
