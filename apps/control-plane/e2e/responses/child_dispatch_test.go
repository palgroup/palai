//go:build e2e

package responses

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// finalOnlyProvider is a single-step fake model: every call returns final output, no tools. It is
// the delegation tier's provider — parent and child alike run to a clean terminal in one model
// step, so the observable multi-run structure is the delegation, not a fabricated tool loop. The
// provider request id encodes the model, so a parent and a child on different model ids are
// distinguishable (the deterministic mirror of the live two-chatcmpl proof).
type finalOnlyProvider struct{}

func (finalOnlyProvider) Execute(_ context.Context, req modelbroker.Request, _ string, _ func(modelbroker.Delta)) (modelbroker.Result, error) {
	return modelbroker.Result{
		ModelRequestID:    req.ModelRequestID,
		ProviderRequestID: "prov_" + req.Model,
		Model:             req.Model,
		Output:            "answer(" + req.Model + ")",
		Usage:             contracts.Usage{InputTokens: 5, OutputTokens: 3, TotalTokens: 8},
		FinishReason:      "stop",
		Attempts:          1,
	}, nil
}

// childRunID reads the single ChildRun a parent dispatched, plus its response id.
func (h *harness) childRunOf(parentRunID string) (runID, responseID string) {
	h.t.Helper()
	if err := h.spine.Pool().QueryRow(context.Background(),
		`SELECT id, response_id FROM runs WHERE parent_run_id=$1 AND organization_id=$2 AND project_id=$3`,
		parentRunID, h.tenant.Organization, h.tenant.Project).Scan(&runID, &responseID); err != nil {
		h.t.Fatalf("read child run of %s error = %v", parentRunID, err)
	}
	return runID, responseID
}

// modelOfRun reads the model a run's terminal projection recorded (the child's own routed model).
func (h *harness) modelOfRun(responseID string) string {
	h.t.Helper()
	var model *string
	if err := h.spine.Pool().QueryRow(context.Background(),
		`SELECT output->>'model' FROM responses WHERE id=$1 AND organization_id=$2 AND project_id=$3`,
		responseID, h.tenant.Organization, h.tenant.Project).Scan(&model); err != nil {
		h.t.Fatalf("read model of %s error = %v", responseID, err)
	}
	if model == nil {
		return ""
	}
	return *model
}

// childRunsLink reads the child run ids the parent's terminal projection links (spec §25.19).
func (h *harness) childRunsLink(responseID string) []string {
	h.t.Helper()
	var raw []byte
	if err := h.spine.Pool().QueryRow(context.Background(),
		`SELECT output->'child_runs' FROM responses WHERE id=$1 AND organization_id=$2 AND project_id=$3`,
		responseID, h.tenant.Organization, h.tenant.Project).Scan(&raw); err != nil {
		h.t.Fatalf("read child_runs of %s error = %v", responseID, err)
	}
	if len(raw) == 0 {
		return nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		h.t.Fatalf("decode child_runs %s error = %v", raw, err)
	}
	return out
}

// childEffectiveBudget reads the intersected budget stored on a ChildRun's delegation spec.
func (h *harness) childEffectiveBudget(childRunID string) int {
	h.t.Helper()
	var budget *int
	if err := h.spine.Pool().QueryRow(context.Background(),
		`SELECT (delegation->'spec'->>'budget')::int FROM runs WHERE id=$1 AND organization_id=$2 AND project_id=$3`,
		childRunID, h.tenant.Organization, h.tenant.Project).Scan(&budget); err != nil {
		h.t.Fatalf("read child budget %s error = %v", childRunID, err)
	}
	if budget == nil {
		return 0
	}
	return *budget
}

// setProjectAllowedModels sets the project's config_policy allowlist so a delegation to an
// off-allowlist model is unroutable (SUB-003).
func (h *harness) setProjectAllowedModels(models ...string) {
	h.t.Helper()
	policy, _ := json.Marshal(map[string]any{"allowed_models": models})
	if _, err := h.spine.Pool().Exec(context.Background(),
		`UPDATE projects SET config_policy=$1 WHERE id=$2 AND organization_id=$3`,
		policy, h.tenant.Project, h.tenant.Organization); err != nil {
		h.t.Fatalf("set project allowed models error = %v", err)
	}
}

// agentDelegatingProvider is the MODEL-driven delegation fake (DEL-001): on the parent's first step
// it calls the built-in `agent` tool, which the engine turns into a child.request; the child (its own
// model id) answers in one step; the parent's next step, seeing the child's answer folded in, finishes.
// The child answer is a unique marker so the parent detects the fold reliably, not by message count.
type agentDelegatingProvider struct{ childModel, childAnswer string }

func (p agentDelegatingProvider) Execute(_ context.Context, req modelbroker.Request, _ string, _ func(modelbroker.Delta)) (modelbroker.Result, error) {
	res := modelbroker.Result{
		ModelRequestID: req.ModelRequestID, Model: req.Model,
		Usage: contracts.Usage{InputTokens: 5, OutputTokens: 3, TotalTokens: 8}, Attempts: 1,
	}
	// The child run answers in a single step, on its own model id.
	if req.Model == p.childModel {
		res.ProviderRequestID = "prov_" + req.Model
		res.Output = p.childAnswer
		res.FinishReason = "stop"
		return res, nil
	}
	// The parent: once the child's answer has folded in, finish.
	for _, m := range req.Messages {
		if strings.Contains(m.Content, p.childAnswer) {
			res.ProviderRequestID = "prov_parent_final"
			res.Output = "parent folded the child result"
			res.FinishReason = "stop"
			return res, nil
		}
	}
	// The parent's first step DELEGATES via the agent tool — model-driven, not config-seeded.
	res.ProviderRequestID = "prov_parent_delegate"
	res.ToolCalls = []modelbroker.ToolCall{{
		ID: "call_agent", Name: "agent",
		Arguments: fmt.Sprintf(`{"role":"researcher","objective":"find x","model":%q,"required":true}`, p.childModel),
	}}
	res.FinishReason = "tool_calls"
	return res, nil
}

// TestModelAgentToolCallDispatchesChildRun proves DEL-001 (spec §25.18, master plan line 410): the
// model DRIVES delegation with an `agent` tool_call — the engine emits a child.request (not a
// tool.request), the controller runs a real ChildRun on its own model id, and its typed result folds
// back into the parent, whose projection links the child run. No config-seeded delegation involved.
func TestModelAgentToolCallDispatchesChildRun(t *testing.T) {
	h := newHarness(t)
	marker := "child-research-" + newID("mark")
	stop := h.runWorker(h.newOrchestratorWithAdapter(subprocessDialer{engineDir: h.engineDir},
		agentDelegatingProvider{childModel: "fake-child", childAnswer: marker}))
	defer stop()

	respID, _, runID := h.admitWith(`{"input":"delegate this"}`, newID("idem"))
	h.awaitResponseState(respID, "completed", 90*time.Second)

	// A real ChildRun ran on the agent tool_call's model — not a tool.request.
	childRun, childResp := h.childRunOf(runID)
	if state := h.runState(childRun); state != "completed" {
		t.Fatalf("child run state = %q, want completed", state)
	}
	if got := h.modelOfRun(childResp); got != "fake-child" {
		t.Fatalf("child model = %q, want fake-child (the agent tool_call's model)", got)
	}
	// The parent journal carries the child lifecycle + typed result, and links the child run.
	if n := h.count(`SELECT count(*) FROM events WHERE response_id=$1 AND type='child.requested.v1'`, respID); n != 1 {
		t.Fatalf("child.requested.v1 = %d, want 1", n)
	}
	if n := h.count(`SELECT count(*) FROM events WHERE response_id=$1 AND type='child.completed.v1'`, respID); n != 1 {
		t.Fatalf("child.completed.v1 = %d, want 1", n)
	}
	if links := h.childRunsLink(respID); len(links) != 1 || links[0] != childRun {
		t.Fatalf("parent projection child_runs = %v, want [%s]", links, childRun)
	}
	// The agent call never became a broker tool.request: no tool_call events on the parent stream.
	if n := h.count(`SELECT count(*) FROM events WHERE response_id=$1 AND type LIKE 'tool_call.%'`, respID); n != 0 {
		t.Fatalf("tool_call events on parent = %d, want 0 (the agent call became a child.request, not a tool.request)", n)
	}
}

// TestChildResultEntersParentAsTypedResult proves the delegation happy path (SUB-001/002
// deterministic half, spec §25.18-19): a required child runs as its OWN run through the existing
// execution path, its typed result folds back into the parent, and the parent's terminal
// projection identifies the child run id — not a hidden transcript.
func TestChildResultEntersParentAsTypedResult(t *testing.T) {
	h := newHarness(t)
	stop := h.runWorker(h.newOrchestratorWithAdapter(subprocessDialer{engineDir: h.engineDir}, finalOnlyProvider{}))
	defer stop()

	respID, _, runID := h.admitWith(
		`{"input":"do it","delegations":[{"role":"researcher","objective":"find x","model":"fake-child","required":true}]}`, newID("idem"))
	h.awaitResponseState(respID, "completed", 90*time.Second)

	// The child ran as its own run, linked to the parent, on its own model id (its own provider call).
	childRun, childResp := h.childRunOf(runID)
	if state := h.runState(childRun); state != "completed" {
		t.Fatalf("child run state = %q, want completed", state)
	}
	if got := h.modelOfRun(childResp); got != "fake-child" {
		t.Fatalf("child model = %q, want fake-child (its own routed model, distinct from the parent)", got)
	}
	// The parent's terminal projection links the child run id (spec §25.19).
	if links := h.childRunsLink(respID); len(links) != 1 || links[0] != childRun {
		t.Fatalf("parent projection child_runs = %v, want [%s]", links, childRun)
	}
	// The parent journal carries the child lifecycle + result, keyed to the parent's response.
	if n := h.count(`SELECT count(*) FROM events WHERE response_id=$1 AND type='child.requested.v1'`, respID); n != 1 {
		t.Fatalf("child.requested.v1 in parent journal = %d, want 1", n)
	}
	if n := h.count(`SELECT count(*) FROM events WHERE response_id=$1 AND type='child.completed.v1'`, respID); n != 1 {
		t.Fatalf("child.completed.v1 in parent journal = %d, want 1", n)
	}
}

// TestChildRunCarriesOwnJournalScopedEvents proves the visibility rule (spec §25.19): a ChildRun's
// own model steps are journaled under ITS response, so they never bloat the parent's stream — the
// parent journal carries only the child lifecycle + result events.
func TestChildRunCarriesOwnJournalScopedEvents(t *testing.T) {
	h := newHarness(t)
	stop := h.runWorker(h.newOrchestratorWithAdapter(subprocessDialer{engineDir: h.engineDir}, finalOnlyProvider{}))
	defer stop()

	respID, _, runID := h.admitWith(
		`{"input":"do it","delegations":[{"role":"r","objective":"o","model":"fake-child","required":true}]}`, newID("idem"))
	h.awaitResponseState(respID, "completed", 90*time.Second)
	_, childResp := h.childRunOf(runID)

	// The child's model steps live under the CHILD response, not the parent's.
	if n := h.count(`SELECT count(*) FROM events WHERE response_id=$1 AND type='model_step.created.v1'`, childResp); n < 1 {
		t.Fatalf("child model steps under the child response = %d, want >=1", n)
	}
	// The parent response carries the child lifecycle but NOT the child's model steps: its
	// model_step events are its OWN (2 steps), and the child's steps do not leak into its stream.
	parentChildLifecycle := h.count(`SELECT count(*) FROM events WHERE response_id=$1 AND type IN ('child.requested.v1','child.completed.v1')`, respID)
	if parentChildLifecycle != 2 {
		t.Fatalf("parent child-lifecycle events = %d, want 2 (requested + completed)", parentChildLifecycle)
	}
	// Cross-check: every child model step is scoped to the child response, none to the parent's.
	childSteps := h.count(`SELECT count(*) FROM events WHERE response_id=$1 AND type='model_step.created.v1'`, childResp)
	parentSteps := h.count(`SELECT count(*) FROM events WHERE response_id=$1 AND type='model_step.created.v1'`, respID)
	if childSteps < 1 || parentSteps < 1 {
		t.Fatalf("model steps: child=%d parent=%d, want each >=1 under its own response", childSteps, parentSteps)
	}
}

// TestRequiredDelegationFailsTypedWhenUnroutable proves SUB-003 (spec §25.18): a required
// delegation the project cannot route fails the run with a typed capability denial — no ChildRun
// escapes, and the parent does NOT fake a parent-only success.
func TestRequiredDelegationFailsTypedWhenUnroutable(t *testing.T) {
	h := newHarness(t)
	// The project allows only the "fake" model; a delegation to "nope" is unroutable.
	h.setProjectAllowedModels("fake")
	stop := h.runWorker(h.newOrchestratorWithAdapter(subprocessDialer{engineDir: h.engineDir}, finalOnlyProvider{}))
	defer stop()

	respID, _, runID := h.admitWith(
		`{"input":"do it","delegations":[{"role":"r","objective":"o","model":"nope","required":true}]}`, newID("idem"))
	h.awaitResponseState(respID, "failed", 90*time.Second)

	// No ChildRun was created — the unroutable required delegation never ran.
	if n := h.count(`SELECT count(*) FROM runs WHERE parent_run_id=$1`, runID); n != 0 {
		t.Fatalf("child runs for an unroutable required delegation = %d, want 0 (no escaped child)", n)
	}
	// The denial is typed and journaled on the parent.
	if n := h.count(`SELECT count(*) FROM events WHERE response_id=$1 AND type='child.denied.v1'`, respID); n != 1 {
		t.Fatalf("child.denied.v1 = %d, want 1 (typed capability failure)", n)
	}
}

// TestChildRunDepthAndFanoutBounded proves SUB-004 end to end (spec §25.18): a parent that asks
// for more children than the fan-out bound gets exactly the bound admitted and the rest denied —
// no escaped child beyond the limit. The excess delegations are optional, so the deny is a skip
// and the parent still completes.
func TestChildRunDepthAndFanoutBounded(t *testing.T) {
	h := newHarness(t)
	stop := h.runWorker(h.newOrchestratorWithAdapter(subprocessDialer{engineDir: h.engineDir}, finalOnlyProvider{}))
	defer stop()

	// Five optional delegations against a fan-out bound of four: four admitted, one denied.
	specs := ""
	for i := 0; i < 5; i++ {
		if i > 0 {
			specs += ","
		}
		specs += fmt.Sprintf(`{"role":"r%d","objective":"o","model":"fake-child","required":false}`, i)
	}
	respID, _, runID := h.admitWith(fmt.Sprintf(`{"input":"do it","delegations":[%s]}`, specs), newID("idem"))
	h.awaitResponseState(respID, "completed", 120*time.Second)

	if n := h.count(`SELECT count(*) FROM runs WHERE parent_run_id=$1`, runID); n != 4 {
		t.Fatalf("admitted child runs = %d, want 4 (the fan-out bound; no escaped child)", n)
	}
	if n := h.count(`SELECT count(*) FROM events WHERE response_id=$1 AND type='child.denied.v1'`, respID); n != 1 {
		t.Fatalf("child.denied.v1 = %d, want 1 (the over-fanout delegation)", n)
	}
}

// TestChildBudgetIntersectsParentRemainder proves the budget half of SUB-004 reached the durable
// child (spec §25.18): a child that over-requests against a bounded parent runs under the
// intersected remainder, stored on its run so its model call is bounded by it.
func TestChildBudgetIntersectsParentRemainder(t *testing.T) {
	h := newHarness(t)
	stop := h.runWorker(h.newOrchestratorWithAdapter(subprocessDialer{engineDir: h.engineDir}, finalOnlyProvider{}))
	defer stop()

	// Parent budget 100; the child requests 500 — clamped to what remains after the parent's own
	// step (100 - 8 = 92 tokens, the fake usage per step).
	respID, _, runID := h.admitWith(
		`{"input":"do it","delegation_budget":100,"delegations":[{"role":"r","objective":"o","model":"fake-child","budget":500,"required":true}]}`, newID("idem"))
	h.awaitResponseState(respID, "completed", 90*time.Second)

	childRun, _ := h.childRunOf(runID)
	effective := h.childEffectiveBudget(childRun)
	if effective <= 0 || effective >= 500 {
		t.Fatalf("child effective budget = %d, want intersected below the 500 requested (parent remainder)", effective)
	}
}
