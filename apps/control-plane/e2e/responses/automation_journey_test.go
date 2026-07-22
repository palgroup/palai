//go:build e2e

package responses

// TestScheduledInvestigationJourneyDeterministic is the E11 Task 7 deterministic half of the mandatory
// automation journey (spec §63.4). It composes the automation spine end to end in CI with NO network and
// NO real credential: the FAKE provider drives the REAL router + trigger/schedule/inbound/callback stores +
// orchestrator against a throwaway Postgres + the reference engine subprocess. It proves the NEW invariants
// this epic owns and NAMES the ceilings it does not claim.
//
// It lives in package responses (not the plan's literal tests/e2e/automation) because the journey drives
// the control plane's internal automation + execution packages, which Go's internal rule forbids importing
// from tests/ — the same constraint that put the E08 newHarness here (T5 recorded the same deviation for
// the fault suite). The supervised loops (delivery-reconciler / webhook-pump / schedule-ticker) do NOT run
// in this harness (e2e "no background worker races" discipline): the journey hand-triggers Tick
// deterministically. The supervised proof is the *WiredIntoRunningBinary component tests — the journey
// NAMES that ceiling, it does not claim it.
//
// Steps (§63.4): publish an agent revision → two trigger variants (webhook + cron) + a schedule → a
// duplicated inbound event yields ONE canonical action (AUT-001) → a schedule occurrence fires a single
// canonical occurrence (AUT-007) → the run's capability never expands to repository-write (63.4 ceiling) →
// a real engine SIGKILL recovers via the ladder to a SINGLE report (recovery) → a SINGLE idempotent
// callback lands despite a retry (AUT-011/013) → an attach follow-up folds the report turn (E08 path) → a
// separate authorized coding fork proves the automation ceiling is session-scoped (E09 seam) → pass +
// self-verified evidence.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/integrations/webhook"
	"github.com/palgroup/palai/apps/control-plane/internal/automation"
	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator/recovery"
	modelbroker "github.com/palgroup/palai/packages/model-broker"

	"github.com/palgroup/palai/storage"
)

// investigationReport is the strict schema the investigation run's final content must satisfy. It lives in
// the JOURNEY HARNESS, not the agent revision (E11's revision enforce-subset rejects an output_schema
// field, agents.go); the evidence therefore says "schema validated by harness, not a revision-carried
// field".
type investigationReport struct {
	Summary           string `json:"summary"`
	Severity          string `json:"severity"`
	RecommendedAction string `json:"recommended_action"`
}

func (r investigationReport) complete() bool {
	return r.Summary != "" && r.Severity != "" && r.RecommendedAction != ""
}

// investigationKillProvider is the deterministic investigation agent: forced (by script) to read a
// research note via the file tool, analyse it via the shell tool, then emit a strict-schema report. On the
// FIRST time it reaches the report step (two tool results folded — file + shell complete, the boundary
// checkpoint durable), it SIGKILLs the live engine and errors; attempt-2, restored past the completed
// tools via the ladder, resumes at the report step and finishes — the tools never re-run. It captures the
// tool set the model was offered so the journey can assert the capability ceiling.
type investigationKillProvider struct {
	report  string
	kill    func()
	mu      sync.Mutex
	crashed bool
	tools   []string // the tool names offered on the LAST call (capability-ceiling capture)
}

func (p *investigationKillProvider) Execute(_ context.Context, req modelbroker.Request, _ string, _ func(modelbroker.Delta)) (modelbroker.Result, error) {
	p.mu.Lock()
	p.tools = p.tools[:0]
	for _, t := range req.Tools {
		p.tools = append(p.tools, t.Name)
	}
	p.mu.Unlock()

	toolResults := 0
	for _, m := range req.Messages {
		if m.Role == "tool" {
			toolResults++
		}
	}
	res := modelbroker.Result{
		ModelRequestID: req.ModelRequestID, Model: "fake",
		Usage: contracts.Usage{InputTokens: 5, OutputTokens: 3, TotalTokens: 8}, Attempts: 1,
	}
	switch toolResults {
	case 0: // write a research note into the scratch workspace
		res.ProviderRequestID = "prov_file"
		res.ToolCalls = []modelbroker.ToolCall{{
			ID: "call_file", Name: "palai.workspace.file",
			Arguments: `{"op":"write","path":"notes.txt","content":"outage: db pool exhausted\n"}`,
		}}
		res.FinishReason = "tool_calls"
	case 1: // analyse it through the shell tool (the file->shell round-trip)
		res.ProviderRequestID = "prov_shell"
		res.ToolCalls = []modelbroker.ToolCall{{
			ID: "call_shell", Name: "palai.workspace.shell",
			Arguments: `{"argv":["sh","-c","grep -q outage notes.txt && echo ANALYSED"],"shell":true}`,
		}}
		res.FinishReason = "tool_calls"
	default: // emit the strict-schema report — but SIGKILL on the FIRST arrival at this boundary
		p.mu.Lock()
		first := !p.crashed
		p.crashed = true
		p.mu.Unlock()
		if first {
			p.kill() // SIGKILL after the file+shell boundary checkpoints are durable
			return modelbroker.Result{}, errRecoveryCrash
		}
		res.ProviderRequestID = "prov_report"
		res.Output = p.report
		res.FinishReason = "stop"
	}
	return res, nil
}

func (p *investigationKillProvider) offeredTools() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.tools))
	copy(out, p.tools)
	return out
}

func TestScheduledInvestigationJourneyDeterministic(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	org, proj := h.tenant.Organization, h.tenant.Project

	// --- Step 1: publish an AgentRevision pinning the model + the {file, shell} tool ceiling. The strict
	// InvestigationReport schema is a HARNESS concern (output_schema is not an E11 revision field). ---
	_, profile := h.postAgent("/v1/agents", `{"name":"investigator"}`)
	profileID, _ := profile["id"].(string)
	_, rev := h.postAgent("/v1/agents/"+profileID+"/revisions",
		`{"model":"fake","tools":["file","shell"],"instructions":"investigate the incident and report"}`)
	revID, _ := rev["id"].(string)
	if profileID == "" || revID == "" {
		t.Fatalf("agent revision not created: profile=%v rev=%v", profile, rev)
	}
	if st, _ := h.postAgent("/v1/agents/"+profileID+"/revisions/"+revID+"/publish", ``); st != http.StatusOK {
		t.Fatalf("publish revision status = %d, want 200", st)
	}

	// --- Step 2: two trigger variants pinned to the revision. (a) a webhook trigger with an inbound secret
	// ref + a callback endpoint (this is the run the journey executes end to end); (b) a cron trigger + a
	// schedule (the occurrence half). ---
	receiver := newCallbackReceiver(t)
	endpointID, err := h.webhooks.CreateEndpoint(ctx, org, proj, automation.EndpointCreate{
		URL: receiver.url(), EventFilter: []string{"trigger.callback.v1"}, SigningSecretRef: "cbref",
		TimeoutMS: 3000, MaxAttempts: 20, RetryWindowSeconds: 3600, AllowPrivateDestination: true,
	})
	if err != nil {
		t.Fatalf("create callback endpoint: %v", err)
	}

	_, wt := h.postAgent("/v1/triggers", `{"name":"inbound-investigate","type":"webhook"}`)
	whTrigger, _ := wt["id"].(string)
	if st, _ := h.postAgent("/v1/triggers/"+whTrigger+"/revisions",
		`{"agent_revision_id":"`+revID+`","input_mapping":{"fields":{"input":{"select":"task"}},"required":["input"]},`+
			`"output_mapping":{"fields":{"result":{"select":"status"}}},"callback_endpoint_id":"`+endpointID+`"}`); st != http.StatusCreated {
		t.Fatalf("revise webhook trigger status = %d, want 201", st)
	}
	if err := h.triggers.SetInboundSecretRefs(ctx, org, proj, whTrigger, "journeyref", ""); err != nil {
		t.Fatalf("set inbound secret ref: %v", err)
	}

	_, ct := h.postAgent("/v1/triggers", `{"name":"cron-investigate","type":"cron"}`)
	cronTrigger, _ := ct["id"].(string)
	if st, _ := h.postAgent("/v1/triggers/"+cronTrigger+"/revisions",
		`{"agent_revision_id":"`+revID+`","input_mapping":{"fields":{"input":{"const":"scheduled investigation"}}}}`); st != http.StatusCreated {
		t.Fatalf("revise cron trigger status = %d, want 201", st)
	}
	// A one_time schedule fires EXACTLY ONE occurrence then exhausts (next_fire_at NULL) — the deterministic
	// shape for the single-canonical-occurrence proof (no per-minute cron fan-out). jitter 0 so the
	// occurrence admits on the same tick the clock makes it due.
	oneTimeAt := time.Now().UTC().Add(2 * time.Second).Format(time.RFC3339)
	_, sch := h.postAgent("/v1/schedules",
		`{"name":"once-`+newID("s")+`","trigger_id":"`+cronTrigger+`","kind":"one_time","one_time_at":"`+oneTimeAt+`","timezone":"UTC","jitter_seconds":0}`)
	scheduleID, _ := sch["id"].(string)
	if whTrigger == "" || cronTrigger == "" || scheduleID == "" {
		t.Fatalf("triggers/schedule not created: wh=%v cron=%v sch=%v", wt, ct, sch)
	}

	// --- Step 3: a duplicated inbound event yields ONE canonical action (AUT-001). Two real HMAC-signed
	// POSTs with the SAME source event id → one canonical delivery + one run; the second is a duplicate
	// linked to the original and bears no run. ---
	payload := []byte(`{"task":"investigate the outage","status":"needs-review"}`)
	const eventID = "evt-journey-dup"
	firstDeliveryID := h.postInbound(t, whTrigger, eventID, 1, payload)
	dupDeliveryID := h.postInbound(t, whTrigger, eventID, 2, payload)
	if firstDeliveryID == dupDeliveryID {
		t.Fatalf("duplicate POST reused the canonical delivery id %q — a duplicate must be its own row", firstDeliveryID)
	}
	firstView := h.deliveryView(t, firstDeliveryID)
	dupView := h.deliveryView(t, dupDeliveryID)
	if dupView["state"] != "duplicate" || dupView["duplicate_of"] != firstDeliveryID {
		t.Fatalf("second inbound event = %v, want state=duplicate duplicate_of=%s", dupView, firstDeliveryID)
	}
	responseID, _ := firstView["response_id"].(string)
	runID, _ := firstView["run_id"].(string)
	if responseID == "" || runID == "" {
		t.Fatalf("canonical inbound delivery bore no run: %v", firstView)
	}
	var canonical, withRun int
	if err := h.spine.Pool().QueryRow(storage.WithSystemScope(ctx),
		`SELECT count(*) FILTER (WHERE duplicate_of IS NULL), count(*) FILTER (WHERE run_id <> '')
		 FROM trigger_deliveries WHERE trigger_id=$1 AND source_event_id=$2`, whTrigger, eventID).Scan(&canonical, &withRun); err != nil {
		t.Fatalf("count canonical deliveries: %v", err)
	}
	if canonical != 1 || withRun != 1 {
		t.Fatalf("duplicated event fanned out: canonical=%d withRun=%d, want 1/1 (one action, original linkage)", canonical, withRun)
	}
	var sessionID string
	if err := h.spine.Pool().QueryRow(storage.WithSystemScope(ctx), `SELECT session_id FROM runs WHERE id=$1`, runID).Scan(&sessionID); err != nil {
		t.Fatalf("read run session: %v", err)
	}

	// --- Step 4: the schedule fires its trigger — exactly ONE canonical occurrence for the (schedule,
	// revision, planned_at) triple, with lateness (planned_at vs admitted_at) visible (AUT-007). The clock
	// is advanced past one_time_at so the occurrence is due AND its (zero) jitter is cleared (accelerate the
	// clock, not the machinery); a second Tick admits nothing (the one_time schedule is exhausted). ---
	ticker := automation.NewScheduleTicker(h.schedules, time.Second, 100, nil).
		WithClock(func() time.Time { return time.Now().Add(2 * time.Minute) })
	occID, plannedAt, admittedAt := h.driveOccurrence(t, ctx, ticker, scheduleID)

	// --- Step 5: capability ceiling — the revision ceiling {file, shell} never expands to repository/store
	// write. Proven two ways: the pure resolver the run uses excludes a publication tool even when the
	// project baseline carries it (the TestRevisionToolCeilingIntersectsRunTools invariant, on THIS
	// revision's tool set), and (after step 6) the executed run creates ZERO publications. ---
	snap := execution.Resolve(execution.ResolveInput{
		DeploymentModel:    "fake",
		ProjectTools:       []string{"file", "shell", "push", "pull_request"},
		AgentRevisionID:    revID,
		AgentRevisionTools: []string{"file", "shell"},
	})
	for _, tool := range snap.Tools {
		if tool == "push" || tool == "pull_request" {
			t.Fatalf("capability ceiling breached: revision {file,shell} resolved a publication tool %q in %v", tool, snap.Tools)
		}
	}

	// --- Step 6: execute the canonical inbound run with a killable real engine + a checkpoint sink. The
	// provider SIGKILLs the engine at the report boundary (after file+shell checkpoints are durable) on
	// attempt-1; attempt-2 restores via the ladder and emits the SINGLE strict-schema report. ---
	reportJSON := `{"summary":"db pool exhausted under the retry storm","severity":"high","recommended_action":"cap the pool and add backoff"}`
	ckStore := newMemCheckpointStore()
	dialer := &killableDialer{inner: subprocessDialer{engineDir: h.engineDir}}
	provider := &investigationKillProvider{report: reportJSON, kill: dialer.killLatest}
	orch := h.newOrchestratorWithTools(dialer, provider, tools.FileTool(), tools.ShellTool())
	orch.SetShellRunner(hostShellRunner{})
	orch.SetCheckpointSink(h.checkpointSink(ckStore))

	alloc := newAllocationRoot(t)
	if err := orch.ExecuteAttempt(ctx, h.workspaceDescriptor(runID, 1, alloc)); err == nil {
		t.Fatal("attempt-1 must fail after the engine is SIGKILLed at the report boundary")
	}
	if dialer.killCount() == 0 {
		t.Fatal("attempt-1 failed WITHOUT the SIGKILL firing: the recovery would not exercise a real kill")
	}
	if ckStore.objectCount() == 0 {
		t.Fatal("no checkpoint persisted before the kill: the ladder has nothing to restore")
	}
	if err := orch.ExecuteAttempt(ctx, h.workspaceDescriptor(runID, 2, alloc)); err != nil {
		t.Fatalf("attempt-2 (ladder restore after kill) error = %v", err)
	}
	if st, proj := h.response(responseID); st != "completed" {
		t.Fatalf("investigation run state = %q, want completed (projection %v)", st, proj)
	}
	if !contains(h.recoveryEventLevels(sessionID), string(recovery.LevelCompatibleCheckpoint)) {
		t.Fatalf("no compatible_checkpoint rung after the kill; levels = %v", h.recoveryEventLevels(sessionID))
	}
	recProof, ok := h.recoveryProof(sessionID)
	if !ok || !recProof.Complete() {
		t.Fatalf("recovery proof missing/incomplete after kill+restore: %+v (ok=%v)", recProof, ok)
	}
	// The file + shell tools ran exactly ONCE across both attempts (a completed tool is not replayed).
	for _, name := range []string{"palai.workspace.file", "palai.workspace.shell"} {
		if n := h.count(`SELECT count(*) FROM tool_calls WHERE run_id=$1 AND name=$2`, runID, name); n != 1 {
			t.Fatalf("%s tool_call rows = %d, want 1 (a completed tool must not replay after a kill)", name, n)
		}
	}
	// The report validates against the strict harness schema — the model's final content is a real report.
	report := h.finalReport(t, responseID)
	if !report.complete() {
		t.Fatalf("investigation report is not schema-complete: %+v", report)
	}
	// Capability never expanded to repository-write: the run created ZERO publications, and no publication
	// tool was ever offered to the model.
	if n := h.count(`SELECT count(*) FROM publications WHERE run_id=$1`, runID); n != 0 {
		t.Fatalf("investigation run created %d publications, want 0 (capability must not expand to repository-write)", n)
	}
	for _, name := range provider.offeredTools() {
		if strings.Contains(name, "publish") {
			t.Fatalf("a publication tool %q was offered to the investigation model (capability ceiling)", name)
		}
	}
	// No checkpoint object carries the inbound signing secret (the §26.2 byte scan on checkpoint objects).
	for _, obj := range ckStore.objects() {
		if strings.Contains(string(obj), string(h.inboundSecret)) {
			t.Fatal("a checkpoint object leaked the inbound signing secret (checkpoint-byte secret scan)")
		}
	}

	// --- Step 7: a SINGLE idempotent callback lands despite a retry. The reconciler arms the callback once
	// the run is terminal; the pump signs + delivers it to the local receiver under a 5xx-then-2xx sequence;
	// the receiver dedupes on Webhook-Id to ONE semantic callback. A second Tick pair adds nothing. ---
	callbackSecret := []byte("whsec_journey_cb_" + newID("s"))
	receiver.secret = callbackSecret
	rec := automation.NewDeliveryReconciler(h.triggers, time.Second, time.Hour, 100, nil)
	if err := rec.Tick(ctx); err != nil {
		t.Fatalf("reconciler tick (arm callback): %v", err)
	}
	pump := automation.NewWebhookPump(h.webhooks, webhook.NewSender(),
		func(_, ref string) ([]byte, error) {
			if ref == "cbref" {
				return callbackSecret, nil
			}
			return nil, fmt.Errorf("unknown callback ref %q", ref)
		},
		automation.PumpConfig{BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond, Tick: time.Second, BatchSize: 50}, nil)
	// Drive attempts until the receiver has accepted the callback (5xx forces a retry, 2xx delivers).
	webhookDeliveryID := h.driveCallback(t, ctx, pump, firstDeliveryID, receiver)
	attempts := receiver.attempts()
	if attempts < 2 {
		t.Fatalf("callback delivered in %d attempts, want >=2 (the 5xx retry must be exercised)", attempts)
	}
	if got := receiver.semanticCount(); got != 1 {
		t.Fatalf("receiver counted %d semantic callbacks, want exactly 1 (Webhook-Id dedupe)", got)
	}
	if got := receiver.gotType(); got != "trigger.callback.v1" {
		t.Fatalf("receiver got envelope type %q, want trigger.callback.v1", got)
	}
	// A second Tick pair republishes nothing — the callback is delivered.
	before := receiver.semanticCount()
	_ = rec.Tick(ctx)
	_ = pump.Tick(ctx)
	if receiver.semanticCount() != before {
		t.Fatalf("a re-driven reconciler+pump added a second semantic callback (idempotency broken)")
	}
	if h.deliveryView(t, firstDeliveryID)["callback_state"] != "delivered" {
		t.Fatalf("callback_state = %v, want delivered", h.deliveryView(t, firstDeliveryID)["callback_state"])
	}
	// The run terminal is intact — the callback delivery did not disturb the completed response.
	if st, _ := h.response(responseID); st != "completed" {
		t.Fatalf("run terminal after callback = %q, want completed (a callback must not mutate the run)", st)
	}

	// --- Step 8: attach + follow-up (E08 path). A chained response onto the investigation session folds the
	// report turn into its history — the follow-up model sees the prior report. ---
	h.assertAttachFoldsReport(t, ctx, sessionID, report.Summary)

	// --- Step 9: a SEPARATE authorized coding fork (E09 seam). A second principal binds a repository and
	// runs a mini one-file coding run — the automation revision's ceiling is session-scoped and does not
	// reach this authority. The FULL coding cycle is TestCodingJourneyDeterministic's job (ponytail). ---
	forkPushSecret := h.assertSeparateCodingFork(t, ctx)

	// --- Step 10: pass + self-verified evidence. The four rules (dedupe/occurrence/callback/recovery) are
	// exercised on the journey's REAL rows; the inbound + callback + fork-push secrets are needles. ---
	h.writeAndVerifyAutomationEvidence(t, automationReceipt{
		dedupeOriginal: firstDeliveryID, dedupeDuplicate: dupDeliveryID,
		occurrenceID: occID, plannedAt: plannedAt, admittedAt: admittedAt,
		callbackDelivery: firstDeliveryID, webhookDeliveryID: webhookDeliveryID, callbackAttempts: attempts,
		recoveryRunID: runID, recoveryProof: recProof,
		secrets: []string{string(h.inboundSecret), string(callbackSecret), forkPushSecret},
	})
}
