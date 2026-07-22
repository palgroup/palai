//go:build e2e

package responses

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"

	"github.com/palgroup/palai/storage"
)

// capturingProvider is the deterministic scripted provider (tool call, then final "12")
// that also records the messages of every call, so a chaining test can prove a later
// response's first model request carries the earlier response's output as history.
type capturingProvider struct {
	mu    sync.Mutex
	calls [][]modelbroker.Message
}

func (p *capturingProvider) Execute(_ context.Context, req modelbroker.Request, _ string, _ func(modelbroker.Delta)) (modelbroker.Result, error) {
	p.mu.Lock()
	msgs := make([]modelbroker.Message, len(req.Messages))
	copy(msgs, req.Messages)
	p.calls = append(p.calls, msgs)
	p.mu.Unlock()

	sawTool := false
	for _, m := range req.Messages {
		if m.Role == "tool" {
			sawTool = true
		}
	}
	res := modelbroker.Result{
		ModelRequestID: req.ModelRequestID,
		Model:          "fake",
		Usage:          contracts.Usage{InputTokens: 5, OutputTokens: 3, TotalTokens: 8},
		Attempts:       1,
	}
	if sawTool {
		res.ProviderRequestID = "prov_final"
		res.Output = "12"
		res.FinishReason = "stop"
		return res, nil
	}
	res.ProviderRequestID = "prov_tool"
	res.ToolCalls = []modelbroker.ToolCall{{ID: "call_add", Name: "palai.conformance.math.add", Arguments: `{"a":7,"b":5}`}}
	res.FinishReason = "tool_calls"
	return res, nil
}

func (p *capturingProvider) snapshot() [][]modelbroker.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([][]modelbroker.Message, len(p.calls))
	copy(out, p.calls)
	return out
}

// hasAssistantContaining reports whether any assistant message carries substr in its
// content, so a history assertion does not depend on the exact content-item shape.
func hasAssistantContaining(msgs []modelbroker.Message, substr string) bool {
	for _, m := range msgs {
		if m.Role == "assistant" && strings.Contains(m.Content, substr) {
			return true
		}
	}
	return false
}

// assertProblem decodes a problem body and asserts its status and stable code.
func assertProblem(t *testing.T, resp *http.Response, wantStatus int, wantCode string) {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		t.Fatalf("status = %d, want %d", resp.StatusCode, wantStatus)
	}
	var problem contracts.Problem
	if err := json.NewDecoder(resp.Body).Decode(&problem); err != nil {
		t.Fatalf("decode problem error = %v", err)
	}
	if problem.Code != wantCode {
		t.Fatalf("code = %q, want %q", problem.Code, wantCode)
	}
}

// TestCreateWithSessionIDAppendsToExistingSession proves a lone session_id opens a new
// response on the existing active session (one journal, one monotonic sequence) and that
// the chained run's engine input carries the prior response's output as history (spec §9,
// §22.2; LP Task 5 minor-a closure).
func TestCreateWithSessionIDAppendsToExistingSession(t *testing.T) {
	h := newHarness(t)
	cap := &capturingProvider{}
	stop := h.runWorker(h.newOrchestratorWithAdapter(subprocessDialer{engineDir: h.engineDir}, cap))
	defer stop()

	resp1, session1, _ := h.admitWith(`{"input":"first turn"}`, newID("idem"))
	h.awaitResponseState(resp1, "completed", 60*time.Second)
	callsAfterResp1 := len(cap.snapshot())

	resp2, session2, _ := h.admitWith(`{"input":"second turn","session_id":"`+session1+`"}`, newID("idem"))
	if session2 != session1 {
		t.Fatalf("chained response session = %q, want the existing %q", session2, session1)
	}
	h.awaitResponseState(resp2, "completed", 60*time.Second)

	// One session, one strictly-increasing gap-free sequence across both responses.
	events := h.events(session1)
	assertContiguous(t, events)
	if len(events) < 4 {
		t.Fatalf("chained journal has %d events, want both responses on one sequence", len(events))
	}

	// The second run's first model call carries the first response's output ("12").
	resp2Calls := cap.snapshot()[callsAfterResp1:]
	if len(resp2Calls) == 0 {
		t.Fatal("response 2 made no model call")
	}
	if !hasAssistantContaining(resp2Calls[0], "12") {
		t.Fatalf("response 2 first model call missing prior output as history: %+v", resp2Calls[0])
	}
}

// TestCreateWithPreviousResponseIDContinuesSameSession proves a lone previous_response_id
// continues in that response's session and links its output into the new run's history
// (spec §9; LP Task 5 minor-a closure).
func TestCreateWithPreviousResponseIDContinuesSameSession(t *testing.T) {
	h := newHarness(t)
	cap := &capturingProvider{}
	stop := h.runWorker(h.newOrchestratorWithAdapter(subprocessDialer{engineDir: h.engineDir}, cap))
	defer stop()

	resp1, session1, _ := h.admitWith(`{"input":"first turn"}`, newID("idem"))
	h.awaitResponseState(resp1, "completed", 60*time.Second)
	callsAfterResp1 := len(cap.snapshot())

	resp2, session2, _ := h.admitWith(`{"input":"second turn","previous_response_id":"`+resp1+`"}`, newID("idem"))
	if session2 != session1 {
		t.Fatalf("continued response session = %q, want the previous response's session %q", session2, session1)
	}
	h.awaitResponseState(resp2, "completed", 60*time.Second)

	resp2Calls := cap.snapshot()[callsAfterResp1:]
	if len(resp2Calls) == 0 {
		t.Fatal("response 2 made no model call")
	}
	if !hasAssistantContaining(resp2Calls[0], "12") {
		t.Fatalf("continued run's first model call missing prior output as history: %+v", resp2Calls[0])
	}
}

// TestLoneSessionIDUnknownIs404TenantScoped proves an unknown session_id is a tenant-scoped
// 404 (no existence disclosure), including the cross-tenant negative: a second tenant cannot
// reach the first tenant's session (spec §39.2; LP Task 5 pattern).
func TestLoneSessionIDUnknownIs404TenantScoped(t *testing.T) {
	h := newHarness(t)

	// Unknown id in the caller's own tenant → 404, never a 202 that silently mints a session.
	resp := h.postResponse(`{"input":"x","session_id":"ses_does_not_exist"}`, newID("idem"), h.token)
	assertProblem(t, resp, http.StatusNotFound, "not_found")

	// Cross-tenant: tenant A owns a real session; tenant B's key cannot reach it.
	_, sessionA, _ := h.admitWith(`{"input":"a"}`, newID("idem"))
	otherToken := newID("e2e-tok")
	seedTenantWithKey(t, h.spine.Pool(), otherToken)
	respB := h.postResponse(`{"input":"x","session_id":"`+sessionA+`"}`, newID("idem"), otherToken)
	assertProblem(t, respB, http.StatusNotFound, "not_found")
}

// TestCreateOnClosedSessionConflicts proves a create against a non-active session is rejected
// with a 409 conflict rather than silently appended (spec §22.1). close_session is a T4
// command; here the session is closed directly to exercise the admission guard.
func TestCreateOnClosedSessionConflicts(t *testing.T) {
	h := newHarness(t)

	_, session1, _ := h.admitWith(`{"input":"first"}`, newID("idem"))
	if _, err := h.spine.Pool().Exec(storage.WithSystemScope(context.Background()),
		`UPDATE sessions SET state='closed' WHERE id=$1`, session1); err != nil {
		t.Fatalf("close session error = %v", err)
	}

	resp := h.postResponse(`{"input":"second","session_id":"`+session1+`"}`, newID("idem"), h.token)
	assertProblem(t, resp, http.StatusConflict, "session_not_active")
}
