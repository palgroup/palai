//go:build e2e

package responses

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"

	"github.com/palgroup/palai/storage"
)

// switchProvider echoes the requested model as the result model — the discipline a real
// provider follows — so consecutive model_requests rows record the per-step effective model
// (spec §9.3). Its first (pre-tool) step gates on a release channel, so a test can queue a
// config change mid-step and prove the in-flight step kept the old model; the resuming step
// answers with whatever model the config resolved for it.
type switchProvider struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once

	mu     sync.Mutex
	models []string
}

func newSwitchProvider() *switchProvider {
	return &switchProvider{started: make(chan struct{}), release: make(chan struct{})}
}

func (p *switchProvider) Execute(ctx context.Context, req modelbroker.Request, _ string, _ func(modelbroker.Delta)) (modelbroker.Result, error) {
	sawTool := false
	for _, m := range req.Messages {
		if m.Role == "tool" {
			sawTool = true
		}
	}
	p.mu.Lock()
	p.models = append(p.models, req.Model)
	p.mu.Unlock()

	if !sawTool {
		p.once.Do(func() { close(p.started) })
		select {
		case <-p.release:
		case <-ctx.Done():
			return modelbroker.Result{}, ctx.Err()
		}
	}
	res := modelbroker.Result{
		ModelRequestID: req.ModelRequestID, Model: req.Model,
		Usage: contracts.Usage{InputTokens: 5, OutputTokens: 3, TotalTokens: 8}, Attempts: 1,
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

// immediateSwitchProvider streams a partial then blocks until the in-flight call is canceled (an
// immediate switch), and answers on the resumed step once the new model is in effect. It echoes
// the requested model and records every call's messages, so a test can prove the resumed step
// routed under the new model AND saw the captured partial as an explicit assistant turn (§25.16).
type immediateSwitchProvider struct {
	started  chan struct{}
	once     sync.Once
	newModel string
	partial  string

	mu     sync.Mutex
	calls  [][]modelbroker.Message
	models []string
}

func (p *immediateSwitchProvider) Execute(ctx context.Context, req modelbroker.Request, _ string, onDelta func(modelbroker.Delta)) (modelbroker.Result, error) {
	p.mu.Lock()
	p.calls = append(p.calls, append([]modelbroker.Message(nil), req.Messages...))
	p.models = append(p.models, req.Model)
	p.mu.Unlock()

	// The resumed step routes under the new model and answers without blocking.
	if req.Model == p.newModel {
		return modelbroker.Result{
			ModelRequestID: req.ModelRequestID, Model: req.Model, ProviderRequestID: "prov_resumed",
			Output: "done", FinishReason: "stop",
			Usage: contracts.Usage{InputTokens: 5, OutputTokens: 3, TotalTokens: 8}, Attempts: 1,
		}, nil
	}
	// The first step (old model) streams a partial, then blocks until the controller aborts it.
	p.once.Do(func() { close(p.started) })
	if onDelta != nil {
		onDelta(modelbroker.Delta{Text: p.partial})
	}
	<-ctx.Done()
	return modelbroker.Result{}, ctx.Err()
}

func (p *immediateSwitchProvider) sawAssistantContent(substr string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, msgs := range p.calls {
		for _, m := range msgs {
			if m.Role == "assistant" && strings.Contains(m.Content, substr) {
				return true
			}
		}
	}
	return false
}

// setProjectPolicy writes the project's config allowlist so a change_config can be denied
// against it (spec §9.3). A NULL policy — the default — is unrestricted.
func (h *harness) setProjectPolicy(policy string) {
	h.t.Helper()
	if _, err := h.spine.Pool().Exec(storage.WithSystemScope(context.Background()),
		`UPDATE projects SET config_policy = $1::jsonb WHERE id = $2 AND organization_id = $3`,
		policy, h.tenant.Project, h.tenant.Organization); err != nil {
		h.t.Fatalf("set project policy error = %v", err)
	}
}

// completedStepModels returns the effective model of each COMPLETED model step for a run, in
// step order — the per-step effective-model evidence the config switch is proven by.
func (h *harness) completedStepModels(runID string) []string {
	h.t.Helper()
	rows, err := h.spine.Pool().Query(storage.WithSystemScope(context.Background()),
		`SELECT result->>'model' FROM model_requests
		 WHERE run_id=$1 AND organization_id=$2 AND project_id=$3 AND state='completed'
		 ORDER BY created_at`,
		runID, h.tenant.Organization, h.tenant.Project)
	if err != nil {
		h.t.Fatalf("read step models error = %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var m *string
		if err := rows.Scan(&m); err != nil {
			h.t.Fatalf("scan step model error = %v", err)
		}
		if m != nil {
			out = append(out, *m)
		}
	}
	return out
}

// eventPayloadOf returns the decoded payload of the first event of the given type, or nil.
func (h *harness) eventPayloadOf(sessionID, typ string) map[string]any {
	h.t.Helper()
	var payload []byte
	err := h.spine.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT payload FROM events WHERE session_id=$1 AND organization_id=$2 AND project_id=$3 AND type=$4 ORDER BY seq LIMIT 1`,
		sessionID, h.tenant.Organization, h.tenant.Project, typ).Scan(&payload)
	if err != nil {
		return nil
	}
	var out map[string]any
	_ = json.Unmarshal(payload, &out)
	return out
}

// TestNormalModelSwitchAppliesNextStep proves a normal model switch lets the active step finish
// on the old model and the NEXT step route under the new one: consecutive model_requests rows
// record old→new (spec §9.3; SES-006 deterministic half). The switch is submitted while the
// first step is gated, so it can only take effect at the boundary after it.
func TestNormalModelSwitchAppliesNextStep(t *testing.T) {
	h := newHarness(t)
	gp := newSwitchProvider()
	stop := h.runWorker(h.newOrchestratorWithAdapter(subprocessDialer{engineDir: h.engineDir}, gp))
	defer stop()

	respID, sessionID, runID := h.admitWith(`{"input":"first turn"}`, newID("idem"))
	select {
	case <-gp.started:
	case <-time.After(30 * time.Second):
		t.Fatal("first model step never started")
	}

	commandID := newID("cmd")
	cmd := h.submitCommand(sessionID, `{"command_id":"`+commandID+`","kind":"change_config","model":"model-beta"}`)
	if cmd.Status != "queued" {
		t.Fatalf("change_config status = %q, want queued (a live run should accept it)", cmd.Status)
	}
	close(gp.release)
	h.awaitResponseState(respID, "completed", 60*time.Second)

	// The two completed steps carry the old (deployment default) then the new model.
	models := h.completedStepModels(runID)
	if len(models) != 2 || models[0] != "fake" || models[1] != "model-beta" {
		t.Fatalf("completed step models = %v, want [fake model-beta] (old then new)", models)
	}
	if state, appliedSeq := h.commandRow(commandID); state != "applied" || appliedSeq == nil {
		t.Fatalf("change_config state = %q applied_sequence = %v, want applied with a sequence", state, appliedSeq)
	}
	if eventSeq(h.events(sessionID), "config.revised.v1") < 0 {
		t.Fatalf("no config.revised.v1 in the journal: %+v", h.events(sessionID))
	}
}

// TestConfigChangeAppliesOnlyAtStepBoundary proves the exit gate's second clause: a config
// change submitted mid-step does NOT leak into the in-flight step, and applies only at the safe
// boundary (spec §9.3). The negative proof is on the journal ledger — the first step COMPLETED
// on the old model before config.revised.v1's sequence, and the next step was CREATED after it.
func TestConfigChangeAppliesOnlyAtStepBoundary(t *testing.T) {
	h := newHarness(t)
	gp := newSwitchProvider()
	stop := h.runWorker(h.newOrchestratorWithAdapter(subprocessDialer{engineDir: h.engineDir}, gp))
	defer stop()

	respID, sessionID, runID := h.admitWith(`{"input":"first turn"}`, newID("idem"))
	select {
	case <-gp.started:
	case <-time.After(30 * time.Second):
		t.Fatal("first model step never started")
	}

	// Submitted while the first step is still gated (in flight): it must not affect that step.
	h.submitCommand(sessionID, `{"command_id":"`+newID("cmd")+`","kind":"change_config","model":"model-beta"}`)
	close(gp.release)
	h.awaitResponseState(respID, "completed", 60*time.Second)

	// The in-flight step kept the old model — the mid-step change did not leak into it.
	models := h.completedStepModels(runID)
	if len(models) == 0 || models[0] != "fake" {
		t.Fatalf("first (in-flight) step model = %v, want it to keep the old model fake", models)
	}

	// The change applied strictly at the boundary: config.revised.v1 sits AFTER the first step
	// completed and BEFORE the next step was created (a negative-leak ledger assertion).
	events := h.events(sessionID)
	revised := eventSeq(events, "config.revised.v1")
	firstCompleted := eventSeq(events, "model_step.completed.v1")
	if revised < 0 || firstCompleted < 0 || firstCompleted >= revised {
		t.Fatalf("config.revised.v1 seq %d must follow the first model_step.completed.v1 seq %d", revised, firstCompleted)
	}
	var nextCreated int
	for _, e := range events {
		if e.typ == "model_step.created.v1" && e.seq > revised {
			nextCreated = e.seq
			break
		}
	}
	if nextCreated == 0 {
		t.Fatalf("no model step created after config.revised.v1 seq %d: %+v", revised, events)
	}
}

// TestImmediateModelSwitchInterruptsStep proves an immediate switch aborts the in-flight step,
// records the streamed-so-far output as an explicit partial (spec §25.16), raises a warning, and
// resumes on the NEW model (spec §9.3; SES-007). The partial content survives into the resumed
// step's context — the streamed-partial capture the T2 interrupt deferred.
func TestImmediateModelSwitchInterruptsStep(t *testing.T) {
	h := newHarness(t)
	gp := &immediateSwitchProvider{started: make(chan struct{}), newModel: "model-beta", partial: "PARTIAL-STREAMED"}
	stop := h.runWorker(h.newOrchestratorWithAdapter(subprocessDialer{engineDir: h.engineDir}, gp))
	defer stop()

	respID, sessionID, runID := h.admitWith(`{"input":"long task"}`, newID("idem"))
	select {
	case <-gp.started:
	case <-time.After(30 * time.Second):
		t.Fatal("first model step never started")
	}

	commandID := newID("cmd")
	cmd := h.submitCommand(sessionID, `{"command_id":"`+commandID+`","kind":"change_config","model":"model-beta","immediate":true}`)
	if cmd.Status != "queued" {
		t.Fatalf("immediate change_config status = %q, want queued", cmd.Status)
	}
	h.awaitResponseState(respID, "completed", 60*time.Second)

	// The aborted step was recorded as an interrupted partial carrying the streamed text (§25.16),
	// not None — the streamed-partial capture the T2 interrupt deferred.
	events := h.events(sessionID)
	if eventSeq(events, "model_step.interrupted.v1") < 0 {
		t.Fatalf("no model_step.interrupted.v1 after the immediate switch: %+v", events)
	}
	if payload := h.eventPayloadOf(sessionID, "model_step.interrupted.v1"); payload["output"] != "PARTIAL-STREAMED" {
		t.Fatalf("interrupted partial output = %v, want the streamed PARTIAL-STREAMED (not None)", payload["output"])
	}
	if !gp.sawAssistantContent("PARTIAL-STREAMED") {
		t.Fatal("the captured partial never reached the resumed step's context as an assistant turn")
	}

	// A warning was raised (spec §9.3, §25.16), the config revised, and the resumed step routed
	// under the new model.
	if eventSeq(events, "warning.raised.v1") < 0 {
		t.Fatalf("no warning.raised.v1 for the immediate switch: %+v", events)
	}
	if eventSeq(events, "config.revised.v1") < 0 {
		t.Fatalf("no config.revised.v1 for the immediate switch: %+v", events)
	}
	models := h.completedStepModels(runID)
	if len(models) == 0 || models[len(models)-1] != "model-beta" {
		t.Fatalf("resumed step model = %v, want the new model-beta", models)
	}
	if state, _ := h.commandRow(commandID); state != "applied" {
		t.Fatalf("immediate change_config state = %q, want applied", state)
	}
}

// TestDeniedToolChangeIsTypedRejection proves a config change requesting a model or tool outside
// the project allowlist is a typed rejection with no silent fallback or broadening, and no
// revision is created (spec §9.3; SES-008 component-real half).
func TestDeniedToolChangeIsTypedRejection(t *testing.T) {
	h := newHarness(t)
	h.setProjectPolicy(`{"allowed_models":["fake"],"allowed_tools":["palai.conformance.math.add"]}`)
	sessionID := h.createSession()

	// A tool outside the allowlist is denied by name — never narrowed to an allowed tool.
	denied := h.submitCommand(sessionID, `{"command_id":"`+newID("cmd")+`","kind":"change_config","tools":["palai.fs.write"]}`)
	if denied.Status != "rejected" {
		t.Fatalf("out-of-allowlist tool change status = %q, want rejected", denied.Status)
	}
	if code, _ := denied.Result["code"].(string); code != "tool_not_allowed" {
		t.Fatalf("rejection code = %q, want tool_not_allowed", code)
	}

	// A model outside the allowlist is likewise denied outright.
	deniedModel := h.submitCommand(sessionID, `{"command_id":"`+newID("cmd")+`","kind":"change_config","model":"model-forbidden"}`)
	if code, _ := deniedModel.Result["code"].(string); code != "model_not_allowed" {
		t.Fatalf("out-of-allowlist model rejection code = %q, want model_not_allowed", code)
	}

	// No silent fallback: the denial created no config revision.
	if n := h.count(`SELECT count(*) FROM config_revisions WHERE session_id=$1 AND organization_id=$2 AND project_id=$3`,
		sessionID, h.tenant.Organization, h.tenant.Project); n != 0 {
		t.Fatalf("config revisions after a denied change = %d, want 0 (no silent fallback)", n)
	}
}

// echoModelProvider answers every step with a final result echoing the requested model — a
// genuine single-step run per response, no fabricated tool call. A non-nil started/release lets a
// test gate the FIRST step so it can submit a command while that run is in flight; nil runs free.
type echoModelProvider struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *echoModelProvider) Execute(ctx context.Context, req modelbroker.Request, _ string, _ func(modelbroker.Delta)) (modelbroker.Result, error) {
	if p.started != nil {
		p.once.Do(func() { close(p.started) })
		select {
		case <-p.release:
		case <-ctx.Done():
			return modelbroker.Result{}, ctx.Err()
		}
	}
	return modelbroker.Result{
		ModelRequestID: req.ModelRequestID, Model: req.Model, ProviderRequestID: "prov_" + req.Model,
		Output: "ok", FinishReason: "stop",
		Usage: contracts.Usage{InputTokens: 5, OutputTokens: 3, TotalTokens: 8}, Attempts: 1,
	}, nil
}

// TestConfigSwitchAppliesAcrossRuns proves the cross-run config carry (spec §9.3, B): a
// change_config submitted on an IDLE session (between two responses) is accepted — not rejected —
// and applies at the NEXT run's start, so consecutive single-step runs record old→new effective
// model WITHOUT any fabricated tool_call. This is the deterministic half of the T3 live smoke.
func TestConfigSwitchAppliesAcrossRuns(t *testing.T) {
	h := newHarness(t)
	stop := h.runWorker(h.newOrchestratorWithAdapter(subprocessDialer{engineDir: h.engineDir}, &echoModelProvider{}))
	defer stop()

	// Response 1 runs to completion on the deployment default model (single step).
	resp1, sessionID, run1 := h.admitWith(`{"input":"first turn"}`, newID("idem"))
	h.awaitResponseState(resp1, "completed", 60*time.Second)

	// change_config on the now-idle session is ACCEPTED (queued), not the old no_active_run reject.
	commandID := newID("cmd")
	cmd := h.submitCommand(sessionID, `{"command_id":"`+commandID+`","kind":"change_config","model":"model-beta"}`)
	if cmd.Status != "queued" {
		t.Fatalf("idle-session change_config status = %q, want queued (accepted, carried to next run)", cmd.Status)
	}

	// Response 2 chained into the same session applies the pending revision at run start.
	resp2, session2, run2 := h.admitWith(`{"input":"second turn","session_id":"`+sessionID+`"}`, newID("idem"))
	if session2 != sessionID {
		t.Fatalf("chained response session = %q, want the same session %q", session2, sessionID)
	}
	h.awaitResponseState(resp2, "completed", 60*time.Second)

	// The two single-step runs recorded old (deployment default) then new (the switched model).
	if m := h.completedStepModels(run1); len(m) != 1 || m[0] != "fake" {
		t.Fatalf("run1 step models = %v, want [fake] (deployment default)", m)
	}
	if m := h.completedStepModels(run2); len(m) != 1 || m[0] != "model-beta" {
		t.Fatalf("run2 step models = %v, want [model-beta] (the switched model)", m)
	}
	if state, appliedSeq := h.commandRow(commandID); state != "applied" || appliedSeq == nil {
		t.Fatalf("change_config state = %q applied_sequence = %v, want applied with a sequence", state, appliedSeq)
	}
	if eventSeq(h.events(sessionID), "config.revised.v1") < 0 {
		t.Fatalf("no config.revised.v1 in the journal: %+v", h.events(sessionID))
	}
}

// TestConfigSwitchMidSingleStepRunCarriesForward proves a change_config submitted DURING a live
// single-step run — which has no step boundary to pump at — is not lost (not expired at run end)
// and applies at the NEXT run's start (spec §9.3, B). It guards the expiry carve-out that keeps a
// boundary-less change_config queued for the cross-run drain.
func TestConfigSwitchMidSingleStepRunCarriesForward(t *testing.T) {
	h := newHarness(t)
	gp := &echoModelProvider{started: make(chan struct{}), release: make(chan struct{})}
	stop := h.runWorker(h.newOrchestratorWithAdapter(subprocessDialer{engineDir: h.engineDir}, gp))
	defer stop()

	resp1, sessionID, run1 := h.admitWith(`{"input":"first turn"}`, newID("idem"))
	select {
	case <-gp.started:
	case <-time.After(30 * time.Second):
		t.Fatal("first (single) model step never started")
	}

	// Submitted while run1's only step is in flight: run1 has no boundary, so it must carry forward.
	commandID := newID("cmd")
	cmd := h.submitCommand(sessionID, `{"command_id":"`+commandID+`","kind":"change_config","model":"model-beta"}`)
	if cmd.Status != "queued" {
		t.Fatalf("mid-run change_config status = %q, want queued", cmd.Status)
	}
	close(gp.release)
	h.awaitResponseState(resp1, "completed", 60*time.Second)

	// It was NOT expired when run1 terminalized — it survives queued (or already applied at run2).
	resp2, _, run2 := h.admitWith(`{"input":"second turn","session_id":"`+sessionID+`"}`, newID("idem"))
	h.awaitResponseState(resp2, "completed", 60*time.Second)

	if m := h.completedStepModels(run1); len(m) != 1 || m[0] != "fake" {
		t.Fatalf("run1 step models = %v, want [fake] (kept the old model — no boundary to switch at)", m)
	}
	if m := h.completedStepModels(run2); len(m) != 1 || m[0] != "model-beta" {
		t.Fatalf("run2 step models = %v, want [model-beta] (the carried-forward switch)", m)
	}
	if state, _ := h.commandRow(commandID); state != "applied" {
		t.Fatalf("carried-forward change_config state = %q, want applied (not expired)", state)
	}
}
