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
)

// gatedProvider is the scripted provider (tool call, then final "12") whose FIRST model call
// blocks on a release channel, so a test can inject a command mid-run at a deterministic point:
// it signals started when the first call is entered, holds until the test releases it, and
// records every call's messages so a test can prove a delivered message folded into a later
// step's context.
type gatedProvider struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once

	mu    sync.Mutex
	calls [][]modelbroker.Message
}

func newGatedProvider() *gatedProvider {
	return &gatedProvider{started: make(chan struct{}), release: make(chan struct{})}
}

func (p *gatedProvider) Execute(ctx context.Context, req modelbroker.Request, _ string, _ func(modelbroker.Delta)) (modelbroker.Result, error) {
	sawTool := false
	for _, m := range req.Messages {
		if m.Role == "tool" {
			sawTool = true
		}
	}
	p.mu.Lock()
	p.calls = append(p.calls, append([]modelbroker.Message(nil), req.Messages...))
	p.mu.Unlock()

	// Only the first (pre-tool) step gates: the test releases it once the command is durably
	// queued. The resuming step returns the final answer without blocking.
	if !sawTool {
		p.once.Do(func() { close(p.started) })
		select {
		case <-p.release:
		case <-ctx.Done():
			return modelbroker.Result{}, ctx.Err()
		}
	}
	res := modelbroker.Result{
		ModelRequestID: req.ModelRequestID, Model: "fake",
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

// callCount reports how many times the provider was invoked, so a test can prove a resumed
// attempt replays a committed step (no re-call) rather than re-running it.
func (p *gatedProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

func (p *gatedProvider) sawUserMessage(substr string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, msgs := range p.calls {
		for _, m := range msgs {
			if m.Role == "user" && strings.Contains(m.Content, substr) {
				return true
			}
		}
	}
	return false
}

// interruptProvider blocks its first model step until the in-flight call is canceled (an
// interrupt), then, on the resumed step, sees the interrupt message in context and returns a
// final answer. It records every call's messages so a test can prove the message reached the
// new step.
type interruptProvider struct {
	started       chan struct{}
	once          sync.Once
	interruptText string

	mu    sync.Mutex
	calls [][]modelbroker.Message
}

func (p *interruptProvider) Execute(ctx context.Context, req modelbroker.Request, _ string, _ func(modelbroker.Delta)) (modelbroker.Result, error) {
	p.mu.Lock()
	p.calls = append(p.calls, append([]modelbroker.Message(nil), req.Messages...))
	p.mu.Unlock()

	resumed := false
	for _, m := range req.Messages {
		if m.Role == "user" && strings.Contains(m.Content, p.interruptText) {
			resumed = true
		}
	}
	// The first step (no interrupt message yet) blocks until the controller aborts it; the
	// resumed step (interrupt message folded in) answers without blocking.
	if !resumed {
		p.once.Do(func() { close(p.started) })
		<-ctx.Done()
		return modelbroker.Result{}, ctx.Err()
	}
	return modelbroker.Result{
		ModelRequestID: req.ModelRequestID, Model: "fake", ProviderRequestID: "prov_resumed",
		Output: "resumed", FinishReason: "stop",
		Usage: contracts.Usage{InputTokens: 5, OutputTokens: 3, TotalTokens: 8}, Attempts: 1,
	}, nil
}

func (p *interruptProvider) sawUserMessage(substr string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, msgs := range p.calls {
		for _, m := range msgs {
			if m.Role == "user" && strings.Contains(m.Content, substr) {
				return true
			}
		}
	}
	return false
}

// deliverRecorder counts the message.deliver frames the orchestrator sends to the engine
// (through the subprocessDialer onSend hook), so a test can prove deliver-once at the wire.
type deliverRecorder struct {
	mu    sync.Mutex
	count int
	last  string
}

func (r *deliverRecorder) onSend(f contracts.EngineFrame) {
	if f.Type != "message.deliver" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.count++
	if msg, ok := f.Data["message"].(string); ok {
		r.last = msg
	}
}

func (r *deliverRecorder) snapshot() (int, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count, r.last
}

// postCommand issues POST /v1/sessions/{id}/commands and returns the raw response.
func (h *harness) postCommand(sessionID, body, token string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.base+"/v1/sessions/"+sessionID+"/commands", strings.NewReader(body))
	if err != nil {
		h.t.Fatalf("build command POST error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("POST command error = %v", err)
	}
	return resp
}

// submitCommand posts a command and decodes the 202 command projection.
func (h *harness) submitCommand(sessionID, body string) contracts.Command {
	h.t.Helper()
	resp := h.postCommand(sessionID, body, h.token)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		h.t.Fatalf("command POST status = %d, want 202", resp.StatusCode)
	}
	var cmd contracts.Command
	if err := json.NewDecoder(resp.Body).Decode(&cmd); err != nil {
		h.t.Fatalf("decode command error = %v", err)
	}
	return cmd
}

// commandRow reads a command's durable state and applied_sequence straight from the table.
func (h *harness) commandRow(commandID string) (state string, appliedSeq *int64) {
	h.t.Helper()
	if err := h.spine.Pool().QueryRow(context.Background(),
		`SELECT state, applied_sequence FROM commands WHERE id=$1 AND organization_id=$2 AND project_id=$3`,
		commandID, h.tenant.Organization, h.tenant.Project).Scan(&state, &appliedSeq); err != nil {
		h.t.Fatalf("read command %s error = %v", commandID, err)
	}
	return state, appliedSeq
}

// eventSeq returns the sequence of the first event of the given type, or -1 if absent.
func eventSeq(events []event, typ string) int {
	for _, e := range events {
		if e.typ == typ {
			return e.seq
		}
	}
	return -1
}

// gatedSteerFlow admits a response, blocks its first model step, submits one send_message
// command mid-run, releases the step, and waits for completion. releaseAfter delays the
// release so a caller can prove the engine outlives the dial deadline. It returns the session
// id, the command id, and the recorder.
func (h *harness) gatedSteerFlow(t *testing.T, gp *gatedProvider, rec *deliverRecorder, delivery, message string, releaseAfter time.Duration) (sessionID, commandID string) {
	t.Helper()
	dialer := subprocessDialer{engineDir: h.engineDir, onSend: rec.onSend}
	stop := h.runWorker(h.newOrchestratorWithAdapter(dialer, gp))
	defer stop()

	respID, sessionID, _ := h.admitWith(`{"input":"first turn"}`, newID("idem"))

	select {
	case <-gp.started:
	case <-time.After(30 * time.Second):
		t.Fatal("first model step never started")
	}

	commandID = newID("cmd")
	cmd := h.submitCommand(sessionID, `{"command_id":"`+commandID+`","kind":"send_message","delivery":"`+delivery+`","message":"`+message+`"}`)
	if cmd.Status != "queued" {
		t.Fatalf("command status = %q, want queued (a live run should accept it)", cmd.Status)
	}

	if releaseAfter > 0 {
		time.Sleep(releaseAfter)
	}
	close(gp.release)

	h.awaitResponseState(respID, "completed", 60*time.Second)
	return sessionID, commandID
}

// TestSteerAppliesAtNextLoopBoundaryWithSequence proves a steer is delivered once at the next
// safe loop boundary and its applied_sequence lands in the journal BETWEEN the two model steps'
// event sequences (spec §9.2, §22.4; SES-004).
func TestSteerAppliesAtNextLoopBoundaryWithSequence(t *testing.T) {
	h := newHarness(t)
	gp, rec := newGatedProvider(), &deliverRecorder{}
	sessionID, commandID := h.gatedSteerFlow(t, gp, rec, "steer", "STEER-PAYLOAD", 0)

	// Delivered exactly once, and the steered text reached a later model call's context.
	if count, last := rec.snapshot(); count != 1 || last != "STEER-PAYLOAD" {
		t.Fatalf("message.deliver frames = %d (last %q), want exactly 1 of STEER-PAYLOAD", count, last)
	}
	if !gp.sawUserMessage("STEER-PAYLOAD") {
		t.Fatal("the steered message never folded into a model call's context")
	}

	// The command is applied and its applied_sequence sits between the two model steps.
	state, appliedSeq := h.commandRow(commandID)
	if state != "applied" || appliedSeq == nil {
		t.Fatalf("command state = %q applied_sequence = %v, want applied with a sequence", state, appliedSeq)
	}
	events := h.events(sessionID)
	// The applied event's own sequence is the applied_sequence it carries.
	if got := eventSeq(events, "command.applied.v1"); int64(got) != *appliedSeq {
		t.Fatalf("command.applied.v1 seq = %d, want applied_sequence %d", got, *appliedSeq)
	}
	firstStep := eventSeq(events, "model_step.completed.v1") // step 1 completed, before the boundary
	if firstStep < 0 || int64(firstStep) >= *appliedSeq {
		t.Fatalf("no model step completed before applied_sequence %d (first completed seq %d)", *appliedSeq, firstStep)
	}
	var laterStep int
	for _, e := range events {
		if e.typ == "model_step.created.v1" && int64(e.seq) > *appliedSeq {
			laterStep = e.seq
			break
		}
	}
	if laterStep == 0 {
		t.Fatalf("no model step created after applied_sequence %d: %+v", *appliedSeq, events)
	}
}

// TestQueuedMessageDeliversOnceAtInputBoundary proves a queued message is delivered exactly
// once, at the input boundary, and folds into the next model request (spec §9.2; SES-003).
func TestQueuedMessageDeliversOnceAtInputBoundary(t *testing.T) {
	h := newHarness(t)
	gp, rec := newGatedProvider(), &deliverRecorder{}
	_, commandID := h.gatedSteerFlow(t, gp, rec, "queue", "QUEUED-PAYLOAD", 0)

	count, last := rec.snapshot()
	if count != 1 {
		t.Fatalf("message.deliver frames = %d, want exactly 1 (deliver-once)", count)
	}
	if last != "QUEUED-PAYLOAD" {
		t.Fatalf("delivered message = %q, want QUEUED-PAYLOAD", last)
	}
	if !gp.sawUserMessage("QUEUED-PAYLOAD") {
		t.Fatal("the queued message never folded into the next model request")
	}
	if state, appliedSeq := h.commandRow(commandID); state != "applied" || appliedSeq == nil {
		t.Fatalf("command state = %q applied_sequence = %v, want applied with a sequence", state, appliedSeq)
	}
}

// TestInterruptEndsCancelableStepPartial proves an interrupt aborts the in-flight model step
// (adapter cancel), records a partial step event, and resumes the run in a NEW step carrying
// the interrupt message (spec §9.2, §25.11; SES-005).
func TestInterruptEndsCancelableStepPartial(t *testing.T) {
	h := newHarness(t)
	gp := &interruptProvider{started: make(chan struct{}), interruptText: "INTERRUPT-MSG"}
	stop := h.runWorker(h.newOrchestratorWithAdapter(subprocessDialer{engineDir: h.engineDir}, gp))
	defer stop()

	respID, sessionID, _ := h.admitWith(`{"input":"long task"}`, newID("idem"))
	select {
	case <-gp.started:
	case <-time.After(30 * time.Second):
		t.Fatal("first model step never started")
	}

	commandID := newID("cmd")
	cmd := h.submitCommand(sessionID, `{"command_id":"`+commandID+`","kind":"send_message","delivery":"interrupt","message":"INTERRUPT-MSG"}`)
	if cmd.Status != "queued" {
		t.Fatalf("interrupt status = %q, want queued (a live run should accept it)", cmd.Status)
	}

	// The abort + resume drives the run to completion (the resumed step answers).
	h.awaitResponseState(respID, "completed", 60*time.Second)

	// A partial step was recorded for the aborted call (interrupted, not failed — spec §25.11),
	// the command applied, and the interrupt message reached the resumed step's context.
	events := h.events(sessionID)
	if eventSeq(events, "model_step.interrupted.v1") < 0 {
		t.Fatalf("no partial (model_step.interrupted.v1) event after interrupt: %+v", events)
	}
	if state, appliedSeq := h.commandRow(commandID); state != "applied" || appliedSeq == nil {
		t.Fatalf("interrupt command state = %q applied_sequence = %v, want applied", state, appliedSeq)
	}
	if !gp.sawUserMessage("INTERRUPT-MSG") {
		t.Fatal("the interrupt message never reached the resumed step's context")
	}
}

// TestMidRunSteerOutlivesDialDeadline is the Step-0 proof: a mid-run steer whose model step
// keeps the engine busy PAST the dial-handshake deadline must still complete — the engine
// process outlives the deadline (plain exec.Command + ctx-aware Receive), never "signal:
// killed". The deadline is the default 20s; the step blocks just over it, so a regression that
// re-ties the process to the dial ctx would kill it mid-run and fail this test.
func TestMidRunSteerOutlivesDialDeadline(t *testing.T) {
	h := newHarness(t)
	gp, rec := newGatedProvider(), &deliverRecorder{}
	// The default DialHandshakeDeadline is 20s; block the first step ~22s to exceed it. The
	// worker heartbeat renews the 30s lease across the block, so the run is not reclaimed.
	_, commandID := h.gatedSteerFlow(t, gp, rec, "steer", "SLOW-STEER", 22*time.Second)

	if count, _ := rec.snapshot(); count != 1 {
		t.Fatalf("message.deliver frames = %d, want exactly 1", count)
	}
	if state, _ := h.commandRow(commandID); state != "applied" {
		t.Fatalf("command state = %q, want applied (the long run completed cleanly)", state)
	}
}

// TestDuplicateCommandIDReturnsOriginalResult proves a duplicate command_id returns the
// original command resource and never creates a second row or a second effect (spec §22.4).
func TestDuplicateCommandIDReturnsOriginalResult(t *testing.T) {
	h := newHarness(t)
	gp, rec := newGatedProvider(), &deliverRecorder{}
	dialer := subprocessDialer{engineDir: h.engineDir, onSend: rec.onSend}
	stop := h.runWorker(h.newOrchestratorWithAdapter(dialer, gp))
	defer stop()

	respID, sessionID, _ := h.admitWith(`{"input":"first turn"}`, newID("idem"))
	select {
	case <-gp.started:
	case <-time.After(30 * time.Second):
		t.Fatal("first model step never started")
	}

	commandID := newID("cmd")
	body := `{"command_id":"` + commandID + `","kind":"send_message","delivery":"steer","message":"DUP"}`
	first := h.submitCommand(sessionID, body)
	second := h.submitCommand(sessionID, body) // duplicate command_id
	if first.ID != second.ID || first.Status != second.Status {
		t.Fatalf("duplicate command diverged: first %+v, second %+v", first, second)
	}
	// Exactly one durable row — the table's own unique deduped, not a second insert.
	if n := h.count(`SELECT count(*) FROM commands WHERE id=$1 AND organization_id=$2 AND project_id=$3`,
		commandID, h.tenant.Organization, h.tenant.Project); n != 1 {
		t.Fatalf("commands rows for %s = %d, want 1", commandID, n)
	}
	close(gp.release)
	h.awaitResponseState(respID, "completed", 60*time.Second)

	// And the effect landed once: a single message.deliver frame despite the duplicate accept.
	if count, _ := rec.snapshot(); count != 1 {
		t.Fatalf("message.deliver frames = %d, want exactly 1 despite the duplicate command", count)
	}
}

// TestQueuedCommandExpiresWhenRunTerminalizes proves the §22.4 lifecycle: a command accepted
// mid-run that never reaches a delivery boundary before the run terminalizes is swept to
// expired (with command.expired.v1), not left queued forever.
func TestQueuedCommandExpiresWhenRunTerminalizes(t *testing.T) {
	h := newHarness(t)
	gp, rec := newGatedProvider(), &deliverRecorder{}
	dialer := subprocessDialer{engineDir: h.engineDir, onSend: rec.onSend}
	stop := h.runWorker(h.newOrchestratorWithAdapter(dialer, gp))
	defer stop()

	respID, sessionID, _ := h.admitWith(`{"input":"work"}`, newID("idem"))
	select {
	case <-gp.started:
	case <-time.After(30 * time.Second):
		t.Fatal("first model step never started")
	}

	commandID := newID("cmd")
	cmd := h.submitCommand(sessionID, `{"command_id":"`+commandID+`","kind":"send_message","delivery":"queue","message":"never delivered"}`)
	if cmd.Status != "queued" {
		t.Fatalf("command status = %q, want queued", cmd.Status)
	}

	// Cancel while the command is queued and the step is still gated: it never reaches a delivery
	// boundary, so terminalization must sweep it to expired.
	h.cancelResponse(respID, h.token).Body.Close()
	close(gp.release) // let the gated attempt unwind against the now-terminal run
	h.awaitResponseState(respID, "canceled", 60*time.Second)

	if state, _ := h.commandRow(commandID); state != "expired" {
		t.Fatalf("queued command on a canceled run state = %q, want expired", state)
	}
	if count, _ := rec.snapshot(); count != 0 {
		t.Fatalf("message.deliver frames = %d, want 0 (never delivered)", count)
	}
	if eventSeq(h.events(sessionID), "command.expired.v1") < 0 {
		t.Fatalf("no command.expired.v1 in the journal: %+v", h.events(sessionID))
	}
}

// TestCommandOnTerminalRunRejected proves a steer on a session whose run has already reached a
// terminal state is a typed rejection — there is no live loop to steer (spec §9.2).
func TestCommandOnTerminalRunRejected(t *testing.T) {
	h := newHarness(t)
	stop := h.runWorker(h.newOrchestrator(subprocessDialer{engineDir: h.engineDir}))
	defer stop()

	respID, sessionID, _ := h.admitWith(`{"input":"do the work"}`, newID("idem"))
	h.awaitResponseState(respID, "completed", 60*time.Second)

	cmd := h.submitCommand(sessionID, `{"command_id":"`+newID("cmd")+`","kind":"send_message","delivery":"steer","message":"too late"}`)
	if cmd.Status != "rejected" {
		t.Fatalf("command on a terminal run status = %q, want rejected", cmd.Status)
	}
	if code, _ := cmd.Result["code"].(string); code != "no_active_run" {
		t.Fatalf("rejection code = %q, want no_active_run", code)
	}
}

// TestApproveWithoutPendingApprovalRejected proves an approve command is accepted but rejected
// (no pending-approval source until E09), rather than silently doing nothing (spec §22.4;
// plan E09 deferral).
func TestApproveWithoutPendingApprovalRejected(t *testing.T) {
	h := newHarness(t)
	sessionID := h.createSession()

	cmd := h.submitCommand(sessionID, `{"command_id":"`+newID("cmd")+`","kind":"approve"}`)
	if cmd.Status != "rejected" {
		t.Fatalf("approve status = %q, want rejected", cmd.Status)
	}
	if code, _ := cmd.Result["code"].(string); code != "no_pending_approval" {
		t.Fatalf("rejection code = %q, want no_pending_approval", code)
	}
}

// createSession opens a session via POST /v1/sessions and returns its id.
func (h *harness) createSession() string {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.base+"/v1/sessions", strings.NewReader(`{}`))
	if err != nil {
		h.t.Fatalf("build session POST error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("POST session error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		h.t.Fatalf("POST session status = %d, want 201", resp.StatusCode)
	}
	var s contracts.Session
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		h.t.Fatalf("decode session error = %v", err)
	}
	return string(s.ID)
}

// TestCreateAndGetSession proves the standalone session resource: POST mints an active session,
// GET returns it, and an unknown id is a tenant-scoped 404 (spec §9.1).
func TestCreateAndGetSession(t *testing.T) {
	h := newHarness(t)
	sessionID := h.createSession()

	req, _ := http.NewRequest(http.MethodGet, h.base+"/v1/sessions/"+sessionID, nil)
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET session error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET session status = %d, want 200", resp.StatusCode)
	}
	var s contracts.Session
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		t.Fatalf("decode session error = %v", err)
	}
	if string(s.ID) != sessionID || s.Status != "active" || s.Object != "session" {
		t.Fatalf("session projection = %+v, want id %s active session", s, sessionID)
	}

	// Unknown id is a 404, never a signal that the id exists elsewhere.
	missing, _ := http.NewRequest(http.MethodGet, h.base+"/v1/sessions/ses_does_not_exist", nil)
	missing.Header.Set("Authorization", "Bearer "+h.token)
	miss, err := http.DefaultClient.Do(missing)
	if err != nil {
		t.Fatalf("GET missing session error = %v", err)
	}
	defer miss.Body.Close()
	if miss.StatusCode != http.StatusNotFound {
		t.Fatalf("GET unknown session status = %d, want 404", miss.StatusCode)
	}
}
