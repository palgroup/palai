//go:build uat

package uat

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// uatIdem mints a fresh Idempotency-Key per POST so two responses in one session are two distinct
// admissions, not an idempotent replay. Fresh per process; the stack's DB is fresh per run.
var uatIdem atomic.Int64

// TestLocalLiveConfigSwitch is the T3 live smoke (spec §9.3, B): one session, two chained REAL
// responses with a change_config to a different real model id between them, proving consecutive
// real provider calls carry old→new model id with two genuine chatcmpl request ids. It reuses the
// provider-one UAT stack; skipped without PALAI_UAT_PROVIDER=provider-one + OPENAI_API_KEY (the
// operator entry loads it from .env.local). The credential rides stdin/env only and is asserted
// absent from the evidence bundle.
func TestLocalLiveConfigSwitch(t *testing.T) {
	requireDocker(t)
	if os.Getenv("PALAI_UAT_PROVIDER") != "provider-one" {
		t.Skip("live config-switch smoke needs PALAI_UAT_PROVIDER=provider-one and OPENAI_API_KEY (run make uat-local-live PROVIDER=provider-one)")
	}
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		t.Fatal("OPENAI_API_KEY is unset; the operator entry loads it from .env.local")
	}
	oldModel := envOr("PALAI_MODEL", "gpt-4o-mini")
	newModel := envOr("PALAI_UAT_SWITCH_MODEL", "gpt-4o")
	if newModel == oldModel {
		t.Fatalf("switch target %q must differ from the deployment model %q", newModel, oldModel)
	}

	// newUATStack registers its own teardown (t.Cleanup), so no defer reset here.
	s := newUATStack(t, "provider-one", key)

	// One session, two chained real responses with a change_config between them.
	sessionID := s.createSession()
	resp1, run1 := s.createResponseInSession(sessionID, "Reply with the single word: ping.")
	if st := s.awaitTerminal(resp1, 120*time.Second); st.Status != "completed" {
		t.Fatalf("response 1 status = %q, want completed", st.Status)
	}
	if status := s.submitChangeConfig(sessionID, newModel); status != "queued" {
		t.Fatalf("change_config status = %q, want queued (accepted on the live session)", status)
	}
	resp2, run2 := s.createResponseInSession(sessionID, "Reply with the single word: pong.")
	if st := s.awaitTerminal(resp2, 120*time.Second); st.Status != "completed" {
		t.Fatalf("response 2 status = %q, want completed", st.Status)
	}

	// The two consecutive REAL calls carry old→new model id with distinct genuine chatcmpl ids.
	model1, chat1 := s.modelCall(run1)
	model2, chat2 := s.modelCall(run2)
	if !strings.HasPrefix(model1, oldModel) {
		t.Fatalf("run1 model = %q, want the deployment default %q…", model1, oldModel)
	}
	if !strings.HasPrefix(model2, newModel) || strings.HasPrefix(model2, oldModel) {
		t.Fatalf("run2 model = %q, want the switched %q… (not the old %q)", model2, newModel, oldModel)
	}
	if model1 == model2 {
		t.Fatalf("run1 and run2 both recorded %q — the switch did not take on the wire", model1)
	}
	for _, id := range []string{chat1, chat2} {
		if !liveProviderIDPattern.MatchString(id) {
			t.Fatalf("provider request id %q is not a genuine chatcmpl id", id)
		}
	}
	if chat1 == chat2 {
		t.Fatalf("both calls reported chatcmpl id %q — not two distinct real round-trips", chat1)
	}
	fmt.Printf("T3 live switch: %s (%s) -> %s (%s)\n", model1, chat1, model2, chat2)

	// Evidence bundle: two live-provider receipts carrying the real chatcmpl ids, verified by the
	// ^chatcmpl- rule with the credential proven absent (0 secret findings).
	s.writeAndVerifySwitchEvidence(t, key, run1, model1, chat1, run2, model2, chat2)
}

// post issues an authenticated JSON POST against the running stack and asserts the status. A fresh
// Idempotency-Key rides every call (ignored by sessions/commands; required by responses).
func (s *uatStack) post(path, body string, want int) []byte {
	s.t.Helper()
	req, err := http.NewRequest(http.MethodPost, s.cfg.BaseURL+path, strings.NewReader(body))
	if err != nil {
		s.t.Fatalf("build POST %s: %v", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+s.apiKey())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", fmt.Sprintf("uat-%d", uatIdem.Add(1)))
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		s.t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		s.t.Fatalf("POST %s status = %d, want %d: %s", path, resp.StatusCode, want, redactBytes(string(raw), s.secret))
	}
	return raw
}

// createSession opens a standalone session and returns its id.
func (s *uatStack) createSession() string {
	var r struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(s.post("/v1/sessions", `{}`, http.StatusCreated), &r)
	if r.ID == "" {
		s.t.Fatal("session create returned no id")
	}
	return r.ID
}

// createResponseInSession admits a response chained into the given session and returns its id and
// run id.
func (s *uatStack) createResponseInSession(sessionID, input string) (respID, runID string) {
	body := fmt.Sprintf(`{"input":%q,"session_id":%q}`, input, sessionID)
	var r struct {
		ID    string `json:"id"`
		RunID string `json:"run_id"`
	}
	_ = json.Unmarshal(s.post("/v1/responses", body, http.StatusAccepted), &r)
	if r.ID == "" || r.RunID == "" {
		s.t.Fatalf("response create returned no id/run_id for session %s", sessionID)
	}
	return r.ID, r.RunID
}

// submitChangeConfig posts a change_config to a different model id and returns the command status.
func (s *uatStack) submitChangeConfig(sessionID, model string) string {
	body := fmt.Sprintf(`{"command_id":%q,"kind":"change_config","model":%q}`, fmt.Sprintf("cmd-%d", uatIdem.Add(1)), model)
	var r struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(s.post("/v1/sessions/"+sessionID+"/commands", body, http.StatusAccepted), &r)
	return r.Status
}

// modelCall reads a single-step run's committed model result: the effective model and the genuine
// provider (chatcmpl) request id.
func (s *uatStack) modelCall(runID string) (model, chatcmpl string) {
	s.t.Helper()
	model = s.query(fmt.Sprintf(
		"SELECT result->>'model' FROM model_requests WHERE run_id='%s' AND result IS NOT NULL ORDER BY created_at LIMIT 1", runID))
	chatcmpl = s.query(fmt.Sprintf(
		"SELECT result->>'provider_request_id' FROM model_requests WHERE run_id='%s' AND result IS NOT NULL ORDER BY created_at LIMIT 1", runID))
	if model == "" || chatcmpl == "" {
		s.t.Fatalf("run %s has no completed model call (model=%q chatcmpl=%q)", runID, model, chatcmpl)
	}
	return model, chatcmpl
}

// writeAndVerifySwitchEvidence writes the config-switch evidence bundle (two live-provider case
// receipts carrying the real chatcmpl ids) and verifies it clean: the ^chatcmpl- rule holds and
// the credential appears nowhere (0 secret findings). It reuses the shared manifest + verifier.
func (s *uatStack) writeAndVerifySwitchEvidence(t *testing.T, key, run1, model1, chat1, run2, model2, chat2 string) {
	t.Helper()
	release := envOr("PALAI_UAT_RELEASE", "local-live-0.1.0") + "-config-switch"
	digest, enroll := s.engineImageDigest(), redactBytes(s.enrollRecord(), s.secret)
	receipt := func(runID, model, chat string) caseReceipt {
		return caseReceipt{
			RunID: runID, ImageDigest: digest, ProviderRequestID: redactBytes(chat, s.secret), MTLSEnroll: enroll,
			TerminalType: "run.completed", TerminalCount: 1, Usage: map[string]int{},
			DBAssertions: []string{"runs.state=completed", "model_requests.result.model=" + model, "model_requests.provider_request_id present (chatcmpl)"},
			Checksum:     hashBundle(runID, model, chat),
		}
	}
	specs := []caseSpec{
		{ID: "T3-SWITCH-OLD", Name: "live-switch-old-model", ProofClass: "live-provider", Provider: "provider-one", ExpectStatus: "completed"},
		{ID: "T3-SWITCH-NEW", Name: "live-switch-new-model", ProofClass: "live-provider", Provider: "provider-one", ExpectStatus: "completed"},
	}
	receipts := map[string]caseReceipt{"T3-SWITCH-OLD": receipt(run1, model1, chat1), "T3-SWITCH-NEW": receipt(run2, model2, chat2)}

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
		t.Fatalf("verify switch bundle: %v", err)
	}
	fmt.Printf("evidence (config-switch): %s\n", summary.String())
	if !summary.OK() || summary.SecretFindings != 0 {
		t.Fatalf("config-switch evidence did not verify clean: %v", summary.Findings)
	}
}
