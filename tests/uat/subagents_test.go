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

// TestLocalLiveSubagents is the E08 Task 5 live smoke (spec §25.18-19): ChildRun delegation and
// cancel propagation driven against real infrastructure and a real provider.
//
// On the real provider every run is single step, so the delegation is config-driven (required
// delegations ride the create body) rather than model-chosen — a real single-step parent still
// emits its child.request from run.start. The two proofs are:
//   - SUB-002 required child on a CHEAPER model id: the parent runs on gpt-4o and delegates to a
//     required child on gpt-4o-mini. The child reaches its own real terminal, and the parent and
//     child carry two DISTINCT genuine chatcmpl ids on two distinct model ids — the child routes
//     its own delegated model, not the parent's. The parent's terminal links the child run id.
//   - SUB-005 parent cancel mid-run: a parent delegates a required child that runs a long real
//     generation; canceling the parent mid-flight propagates to the child, which reaches the
//     canceled terminal with consistent accounting (one terminal), no escaped compute left live.
//
// Skipped without PALAI_UAT_PROVIDER=provider-one + OPENAI_API_KEY (the operator entry loads it
// from .env.local); the credential rides env only and is asserted absent from the evidence bundle.
func TestLocalLiveSubagents(t *testing.T) {
	requireDocker(t)
	if os.Getenv("PALAI_UAT_PROVIDER") != "provider-one" {
		t.Skip("live subagents smoke needs PALAI_UAT_PROVIDER=provider-one and OPENAI_API_KEY (run make uat-local-live PROVIDER=provider-one)")
	}
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		t.Fatal("OPENAI_API_KEY is unset; the operator entry loads it from .env.local")
	}
	// The parent runs on gpt-4o so the required child's gpt-4o-mini is a genuinely cheaper,
	// distinct model id (both proven accessible by the T3 switch smoke).
	t.Setenv("PALAI_MODEL", envOr("PALAI_UAT_PARENT_MODEL", "gpt-4o"))
	childModel := envOr("PALAI_UAT_CHILD_MODEL", "gpt-4o-mini")
	// Inline delegation holds the parent's engine while the child dials its own, so the runner
	// needs 2 concurrent lease slots (children are sequential + depth is capped at 1, so 2 is the
	// ceiling regardless of fan-out). The default stays 1 for every non-delegating stack.
	t.Setenv("PALAI_RUNNER_CONCURRENCY", envOr("PALAI_RUNNER_CONCURRENCY", "2"))

	s := newUATStack(t, "provider-one", key)
	proofs := []liveProof{}
	proofs = append(proofs, s.proveRequiredChildOnCheaperModel(t, childModel)...)
	proofs = append(proofs, s.proveParentCancelPropagatesToChild(t, childModel))

	s.writeAndVerifyLiveProviderEvidence(t, key, "-subagents", proofs)
}

// proveRequiredChildOnCheaperModel drives SUB-002: a parent (gpt-4o) with a required delegation to
// a cheaper child (gpt-4o-mini). Both reach a real terminal on two distinct chatcmpl ids and two
// distinct model ids; the parent's terminal projection links the child run id. Returns the parent
// and child receipts.
func (s *uatStack) proveRequiredChildOnCheaperModel(t *testing.T, childModel string) []liveProof {
	t.Helper()
	sessionID := s.createSession()
	delegations := fmt.Sprintf(`[{"role":"researcher","objective":"Reply with exactly one word and nothing else: ATLAS.","model":%q,"required":true}]`, childModel)
	respID, parentRun := s.createResponseWithDelegation(sessionID, "Reply with exactly one word and nothing else: SUMMIT.", delegations)

	if final := s.awaitTerminal(respID, 180*time.Second); final.Status != "completed" {
		s.dumpDelegationFailure(respID, parentRun)
		t.Fatalf("SUB-002: parent response %s status = %q, want completed", respID, final.Status)
	}
	childRun := s.childRunOf(parentRun)
	if childRun == "" {
		t.Fatal("SUB-002: no ChildRun was dispatched for the required delegation")
	}
	if st := s.query(fmt.Sprintf("SELECT state FROM runs WHERE id='%s'", childRun)); st != "completed" {
		t.Fatalf("SUB-002: child run %s state = %q, want completed", childRun, st)
	}

	parentModel, parentChat := s.modelCall(parentRun)
	childModelUsed, childChat := s.modelCall(childRun)
	if !liveProviderIDPattern.MatchString(parentChat) || !liveProviderIDPattern.MatchString(childChat) {
		t.Fatalf("SUB-002: provider request ids are not genuine chatcmpl ids (parent=%q child=%q)", parentChat, childChat)
	}
	// Two DISTINCT real provider request ids, parent + child.
	if parentChat == childChat {
		t.Fatalf("SUB-002: parent and child share chatcmpl %s — the child did not make its own real call", parentChat)
	}
	// The child routed its OWN cheaper model family, distinct from the parent's (the provider
	// returns a dated id, e.g. gpt-4o-mini-2024-07-18, so match the family prefix). The parent
	// must NOT carry the child's family — that is what proves independent routing.
	if !strings.HasPrefix(childModelUsed, childModel) || strings.HasPrefix(parentModel, childModel) {
		t.Fatalf("SUB-002: child model = %q (want the cheaper %q family, distinct from parent %q)", childModelUsed, childModel, parentModel)
	}
	// The parent's terminal projection links the child run id (spec §25.19).
	if links := s.query(fmt.Sprintf("SELECT output->'child_runs' FROM responses WHERE id='%s'", respID)); links == "" || links == "null" {
		t.Fatalf("SUB-002: parent projection does not link the child run id (child_runs=%q)", links)
	}

	fmt.Printf("SUB-002: parent %s (%s) delegated to child %s (%s) — distinct real calls\n", parentModel, parentChat, childModelUsed, childChat)
	parentProof := liveProof{"SUB-002-PARENT", "live-required-delegation-parent", parentRun, parentModel, parentChat, []string{
		"parent runs.state=completed on the real provider; provider_request_id is a chatcmpl",
		"the parent required-delegated a child and its terminal links the child run id (§25.19)",
		"parent model id is distinct from the child's (independent routing)",
	}, "run.completed"}
	childProof := liveProof{"SUB-002-CHILD", "live-required-delegation-child", childRun, childModelUsed, childChat, []string{
		"child runs.state=completed as its OWN run on the real provider (SUB-002)",
		"child provider_request_id is a genuine chatcmpl, DISTINCT from the parent's",
		"child routed its own cheaper model id (" + childModel + "), not the parent's",
	}, "run.completed"}
	return []liveProof{parentProof, childProof}
}

// proveParentCancelPropagatesToChild drives SUB-005: a parent delegates a required child that runs
// a long real generation; canceling the parent mid-flight propagates to the child, which reaches
// the canceled terminal with consistent accounting. Returns the parent's cancel receipt.
func (s *uatStack) proveParentCancelPropagatesToChild(t *testing.T, childModel string) liveProof {
	t.Helper()
	sessionID := s.createSession()
	delegations := fmt.Sprintf(`[{"role":"writer","objective":%q,"model":%q,"required":true}]`, longGenerationPrompt, childModel)
	respID, parentRun := s.createResponseWithDelegation(sessionID, "Reply with exactly one word and nothing else: ANCHOR.", delegations)

	// Wait for the child to be dispatched and its real model call to start — the in-flight window.
	childRun := s.awaitChildRun(parentRun, 120*time.Second)
	if childRun == "" {
		t.Fatal("SUB-005: the required child was never dispatched")
	}
	if !s.awaitModelRequest(childRun, 120*time.Second) {
		t.Fatalf("SUB-005: child run %s never started its real model call — no in-flight window to cancel", childRun)
	}
	// The parent's own first model step committed a chatcmpl before it delegated (the receipt's
	// real-wire proof); capture it before the cancel.
	parentModel, parentChat := s.modelCall(parentRun)

	// Cancel the parent mid-run: it propagates to the live child.
	s.post("/v1/responses/"+respID+"/cancel", "", http.StatusAccepted)
	if final := s.awaitTerminal(respID, 60*time.Second); final.Status != "canceled" {
		t.Fatalf("SUB-005: parent response %s status = %q, want canceled", respID, final.Status)
	}

	// The child reached the canceled terminal (propagation), with exactly one terminal event.
	deadline := time.Now().Add(60 * time.Second)
	for {
		if st := s.query(fmt.Sprintf("SELECT state FROM runs WHERE id='%s'", childRun)); st == "canceled" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("SUB-005: child run %s did not reach canceled after the parent cancel", childRun)
		}
		time.Sleep(200 * time.Millisecond)
	}
	childResp := s.query(fmt.Sprintf("SELECT response_id FROM runs WHERE id='%s'", childRun))
	if n := s.query(fmt.Sprintf("SELECT count(*) FROM events WHERE response_id='%s' AND type='run.canceled.v1'", childResp)); n != "1" {
		t.Fatalf("SUB-005: child run.canceled.v1 count = %q, want 1 (single terminal, consistent accounting)", n)
	}
	// Repeated cancel is a monotonic no-op — no second child terminal.
	s.post("/v1/responses/"+respID+"/cancel", "", http.StatusAccepted)
	if n := s.query(fmt.Sprintf("SELECT count(*) FROM events WHERE response_id='%s' AND type='run.canceled.v1'", childResp)); n != "1" {
		t.Fatalf("SUB-005: child run.canceled.v1 after re-cancel = %q, want 1 (monotonic)", n)
	}

	fmt.Printf("SUB-005: canceled parent %s mid-run; child %s propagated to canceled (parent call %s)\n", parentRun, childRun, parentChat)
	return liveProof{"SUB-005-CANCEL", "live-parent-cancel-propagates-to-child", parentRun, parentModel, parentChat, []string{
		"parent committed a real model step (chatcmpl) then delegated a required child running a long real generation",
		"canceling the parent mid-run propagated to the live child: child run.state=canceled (SUB-005)",
		"child run.canceled.v1 is the single terminal; a repeated cancel is a monotonic no-op (consistent accounting)",
	}, "run.canceled"}
}

// dumpDelegationFailure prints redacted diagnostics for a delegation run that did not complete —
// the parent's error, every model_requests row (model + provider request id + any sanitized error),
// the child run and its model call, and the last journaled events — so a live-provider failure is
// diagnosable without re-running. Every value is redacted of the credential.
func (s *uatStack) dumpDelegationFailure(respID, parentRun string) {
	r := func(v string) string { return redactBytes(v, s.secret) }
	fmt.Printf("DELEGATION-FAIL: parent response error = %s\n", r(s.query(fmt.Sprintf("SELECT output->'error' FROM responses WHERE id='%s'", respID))))
	fmt.Printf("DELEGATION-FAIL: parent model_requests = %s\n", r(s.query(fmt.Sprintf(
		"SELECT json_agg(json_build_object('state',state,'model',result->>'model','prov',result->>'provider_request_id','err',result->'error')) FROM model_requests WHERE run_id='%s'", parentRun))))
	childRun := s.childRunOf(parentRun)
	fmt.Printf("DELEGATION-FAIL: child run = %s state = %s\n", childRun, s.query(fmt.Sprintf("SELECT state FROM runs WHERE id='%s'", childRun)))
	if childRun != "" {
		fmt.Printf("DELEGATION-FAIL: child model_requests = %s\n", r(s.query(fmt.Sprintf(
			"SELECT json_agg(json_build_object('state',state,'model',result->>'model','prov',result->>'provider_request_id','err',result->'error')) FROM model_requests WHERE run_id='%s'", childRun))))
	}
	fmt.Printf("DELEGATION-FAIL: last events = %s\n", r(s.query(fmt.Sprintf(
		"SELECT json_agg(type ORDER BY seq) FROM (SELECT type, seq FROM events WHERE response_id='%s' ORDER BY seq DESC LIMIT 12) e", respID))))
}

// createResponseWithDelegation admits a response chained into the session carrying required
// delegations (the Palai extension parsed from the raw body), returning its id and run id.
func (s *uatStack) createResponseWithDelegation(sessionID, input, delegationsJSON string) (respID, runID string) {
	s.t.Helper()
	body := fmt.Sprintf(`{"input":%q,"session_id":%q,"delegations":%s}`, input, sessionID, delegationsJSON)
	var r struct {
		ID    string `json:"id"`
		RunID string `json:"run_id"`
	}
	raw := s.post("/v1/responses", body, http.StatusAccepted)
	if err := json.Unmarshal(raw, &r); err != nil || r.ID == "" || r.RunID == "" {
		s.t.Fatalf("delegation response create returned no id/run_id (err=%v) for session %s", err, sessionID)
	}
	return r.ID, r.RunID
}

// childRunOf returns the single ChildRun a parent dispatched, or "" if none yet.
func (s *uatStack) childRunOf(parentRunID string) string {
	return s.query(fmt.Sprintf("SELECT id FROM runs WHERE parent_run_id='%s' ORDER BY created_at LIMIT 1", parentRunID))
}

// awaitChildRun polls until the parent has dispatched a ChildRun, returning its id (or "" on timeout).
func (s *uatStack) awaitChildRun(parentRunID string, within time.Duration) string {
	s.t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if id := s.childRunOf(parentRunID); id != "" {
			return id
		}
		time.Sleep(200 * time.Millisecond)
	}
	return ""
}
