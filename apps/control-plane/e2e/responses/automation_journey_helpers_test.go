//go:build e2e

package responses

// Helpers for TestScheduledInvestigationJourneyDeterministic: the local callback receiver, the inbound
// HMAC POST, the deterministic occurrence/callback drivers, the strict-report reader, the attach + fork
// sub-proofs, and the automation-0.1.0 evidence writer. Kept beside the journey so the test body reads as
// the §63.4 step list.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/integrations/webhook"
	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/apps/control-plane/internal/automation"
	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/packages/coordinator/recovery"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
	"github.com/palgroup/palai/tests/uat"

	"github.com/palgroup/palai/storage"
)

// --- callback receiver -----------------------------------------------------------------------------

// callbackReceiver is a real local HTTP receiver that verifies the callback HMAC server-side, dedupes on
// Webhook-Id, and forces exactly ONE retry (5xx on the first hit of an id, 2xx after) so the journey proves
// "delivered exactly once despite a retry". A sham verifier would not prove the signing contract.
type callbackReceiver struct {
	server *httptest.Server
	mu     sync.Mutex
	secret []byte
	hits   int            // total HTTP attempts
	ok     int            // successful (2xx) deliveries
	seen   map[string]int // per Webhook-Id hit count → distinct ids = semantic callbacks
	typ    string
}

func newCallbackReceiver(t *testing.T) *callbackReceiver {
	r := &callbackReceiver{seen: map[string]int{}}
	r.server = httptest.NewServer(http.HandlerFunc(r.handle))
	t.Cleanup(r.server.Close)
	return r
}

func (r *callbackReceiver) url() string { return r.server.URL }

func (r *callbackReceiver) handle(w http.ResponseWriter, req *http.Request) {
	raw, _ := io.ReadAll(req.Body)
	id := req.Header.Get(webhook.HeaderID)
	ts, _ := strconv.ParseInt(req.Header.Get(webhook.HeaderTimestamp), 10, 64)
	r.mu.Lock()
	secret := r.secret
	r.mu.Unlock()
	if !webhook.Verify(secret, id, time.Unix(ts, 0), raw, req.Header.Get(webhook.HeaderSignature), time.Now(), 5*time.Minute) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var env struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &env)
	r.mu.Lock()
	r.hits++
	r.seen[id]++
	firstHitForID := r.seen[id] == 1
	r.typ = env.Type
	r.mu.Unlock()
	if firstHitForID {
		w.WriteHeader(http.StatusInternalServerError) // force one retry
		return
	}
	r.mu.Lock()
	r.ok++
	r.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (r *callbackReceiver) attempts() int      { r.mu.Lock(); defer r.mu.Unlock(); return r.hits }
func (r *callbackReceiver) delivered() int     { r.mu.Lock(); defer r.mu.Unlock(); return r.ok }
func (r *callbackReceiver) semanticCount() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.seen) }
func (r *callbackReceiver) gotType() string    { r.mu.Lock(); defer r.mu.Unlock(); return r.typ }

// --- inbound + delivery ----------------------------------------------------------------------------

// postInbound wraps the mapping payload in the §21.7 source envelope (source + opaque data; the
// source_event_id is derived from the signed Webhook-Id), signs the whole body with the harness inbound
// secret, and POSTs it to the UNAUTHENTICATED inbound route (the top mux, no bearer). Two POSTs with the
// same eventID share a source_event_id, so the second dedupes. It returns the accepted delivery id.
func (h *harness) postInbound(t *testing.T, triggerID, eventID string, attempt int, payload []byte) string {
	t.Helper()
	body := []byte(`{"source":"harness","data":` + string(payload) + `}`)
	headers := webhook.NewSigner(h.inboundSecret).Headers(eventID, time.Now(), attempt, body)
	req, err := http.NewRequest(http.MethodPost, h.base+"/v1/inbound/"+triggerID, strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("build inbound POST: %v", err)
	}
	req.Close = true
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/inbound: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("inbound POST status = %d, want 202: %s", resp.StatusCode, raw)
	}
	var acc map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&acc)
	id, _ := acc["id"].(string)
	if id == "" {
		t.Fatalf("inbound 202 carried no delivery id: %v", acc)
	}
	return id
}

// deliveryView reads a trigger delivery over the real router with the harness bearer.
func (h *harness) deliveryView(t *testing.T, deliveryID string) map[string]any {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, h.base+"/v1/trigger-deliveries/"+deliveryID, nil)
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET trigger-delivery: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}

// --- schedule occurrence ---------------------------------------------------------------------------

// driveOccurrence hand-triggers the ticker until the forced-due schedule has admitted exactly one
// occurrence, then re-ticks to prove the UNIQUE key collapses a re-fire (still one). It returns the single
// canonical occurrence's id + instants.
func (h *harness) driveOccurrence(t *testing.T, ctx context.Context, ticker *automation.ScheduleTicker, scheduleID string) (occID string, plannedAt, admittedAt time.Time) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := ticker.Tick(ctx); err != nil {
			t.Fatalf("schedule ticker tick: %v", err)
		}
		occID, plannedAt, admittedAt = h.admittedOccurrence(t, ctx, scheduleID)
		if occID != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("schedule never admitted an occurrence via the hand-triggered ticker")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// A re-tick must not create a second occurrence for the same (schedule, revision, planned_at).
	if err := ticker.Tick(ctx); err != nil {
		t.Fatalf("schedule ticker re-tick: %v", err)
	}
	if n := h.count(`SELECT count(*) FROM schedule_occurrences WHERE schedule_id=$1`, scheduleID); n != 1 {
		t.Fatalf("schedule has %d occurrences after a re-tick, want exactly 1 (single canonical occurrence)", n)
	}
	return occID, plannedAt, admittedAt
}

// admittedOccurrence returns the single admitted occurrence's id + instants, or "" if none yet.
func (h *harness) admittedOccurrence(t *testing.T, ctx context.Context, scheduleID string) (string, time.Time, time.Time) {
	t.Helper()
	var occID string
	var planned time.Time
	var admitted *time.Time
	err := h.spine.Pool().QueryRow(storage.WithSystemScope(ctx),
		`SELECT occurrence_id, planned_at, admitted_at FROM schedule_occurrences
		 WHERE schedule_id=$1 AND state='admitted' ORDER BY planned_at LIMIT 1`, scheduleID).Scan(&occID, &planned, &admitted)
	if err != nil {
		return "", time.Time{}, time.Time{} // not admitted yet (or none) — poll again
	}
	if admitted == nil {
		return "", time.Time{}, time.Time{}
	}
	return occID, planned, *admitted
}

// --- callback --------------------------------------------------------------------------------------

// driveCallback hand-triggers the pump until the receiver has accepted the callback (a 2xx after the forced
// 5xx retry), then returns the callback's webhook_delivery id.
func (h *harness) driveCallback(t *testing.T, ctx context.Context, pump *automation.WebhookPump, deliveryID string, receiver *callbackReceiver) string {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for receiver.delivered() == 0 {
		if err := pump.Tick(ctx); err != nil {
			t.Fatalf("webhook pump tick: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("callback never delivered via the hand-triggered pump: attempts=%d", receiver.attempts())
		}
		time.Sleep(10 * time.Millisecond)
	}
	var whdID string
	if err := h.spine.Pool().QueryRow(storage.WithSystemScope(ctx),
		`SELECT id FROM webhook_deliveries WHERE event_id=$1`, "cb:"+deliveryID).Scan(&whdID); err != nil {
		t.Fatalf("read callback webhook_delivery id: %v", err)
	}
	return whdID
}

// --- strict report ---------------------------------------------------------------------------------

// finalReport reads the completed run's final output text and decodes it against the strict harness schema.
func (h *harness) finalReport(t *testing.T, responseID string) investigationReport {
	t.Helper()
	_, proj := h.response(responseID)
	if len(proj.Output) == 0 {
		t.Fatal("completed response carried no output")
	}
	content, _ := proj.Output[0]["content"].(string)
	var r investigationReport
	if err := json.Unmarshal([]byte(content), &r); err != nil {
		t.Fatalf("final output is not a valid InvestigationReport JSON: %v (content=%q)", err, content)
	}
	return r
}

// --- step 8: attach + follow-up --------------------------------------------------------------------

// assertAttachFoldsReport chains a follow-up response onto the investigation session and proves the
// folded history carries the prior report turn (E08 attach path; the full multi-client journal is
// attach_test.go's job). It reuses this package's capturingProvider (chaining_test.go), which records the
// messages of every model call, so the assertion reads the folded history the follow-up run was dispatched
// with.
func (h *harness) assertAttachFoldsReport(t *testing.T, ctx context.Context, sessionID, needle string) {
	t.Helper()
	cap := &capturingProvider{}
	stop := h.runWorker(h.newOrchestratorWithAdapter(subprocessDialer{engineDir: h.engineDir}, cap))
	respID, session2, _ := h.admitWith(`{"input":"summarize the finding","session_id":"`+sessionID+`"}`, newID("idem"))
	if session2 != sessionID {
		t.Fatalf("follow-up landed on session %q, want the investigation session %q", session2, sessionID)
	}
	h.awaitResponseState(respID, "completed", 90*time.Second)
	stop()
	cap.mu.Lock()
	defer cap.mu.Unlock()
	for _, call := range cap.calls {
		for _, m := range call {
			if strings.Contains(m.Content, needle) {
				return
			}
		}
	}
	t.Fatalf("follow-up folded history did not carry the prior report turn (summary %q absent)", needle)
}

// --- step 9: separate authorized coding fork -------------------------------------------------------

// forkFileProvider writes a single file then finishes — the minimal coding turn the separate-authority
// fork runs (the full coding cycle is TestCodingJourneyDeterministic's job, ponytail).
type forkFileProvider struct{ marker string }

func (p *forkFileProvider) Execute(_ context.Context, req modelbroker.Request, _ string, _ func(modelbroker.Delta)) (modelbroker.Result, error) {
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
	if toolResults == 0 {
		res.ProviderRequestID = "prov_fork_file"
		res.ToolCalls = []modelbroker.ToolCall{{
			ID: "call_file", Name: "palai.workspace.file",
			Arguments: `{"op":"write","path":"repo/fork.txt","content":"` + p.marker + `\n"}`,
		}}
		res.FinishReason = "tool_calls"
		return res, nil
	}
	res.ProviderRequestID = "prov_fork_final"
	res.Output = "wrote fork.txt"
	res.FinishReason = "stop"
	return res, nil
}

// forkPushSecret is the throwaway brokered token the fork's clone runs behind — asserted absent from the
// evidence bundle as a needle (the separate authority's credential, self-checked).
const forkPushSecret = "palai-FORK-clone-secret"

// assertSeparateCodingFork proves a SECOND principal can run an authorized coding fork whose authority is
// independent of the automation revision's {file,shell} ceiling: it binds a repository, prepares an
// isolated workspace under a brokered credential, and runs a one-file coding turn. Returns the fork's
// brokered token so the journey needles it in the evidence redaction scan.
func (h *harness) assertSeparateCodingFork(t *testing.T, ctx context.Context) string {
	t.Helper()
	token2 := newID("e2e-fork-tok")
	tenant2 := seedTenantWithKey(t, h.spine.Pool(), token2)

	remote := newCodingRemote(t)
	bindingID := newID("bnd-fork")
	if err := h.spine.CreateRepositoryBinding(ctx, tenant2, coordinator.RepositoryBindingInput{
		BindingID: bindingID, Provider: "local", RepositoryIdentity: "acme/fork",
		CloneURL: remote.url, DefaultBranch: "main",
		AllowedOperations: []string{"push_branch"},
	}); err != nil {
		t.Fatalf("fork: create repository binding: %v", err)
	}

	// Admit a run as the SECOND principal (its own token), then prepare + execute — a distinct authority.
	resp := h.postResponse(`{"input":"fork the repo and add a file"}`, newID("idem"), token2)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("fork admit status = %d, want 202", resp.StatusCode)
	}
	var r contracts.Response
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("fork decode response: %v", err)
	}
	responseID, sessionID, runID := string(r.ID), string(r.SessionID), string(r.RunID)

	alloc := newAllocationRoot(t)
	if _, err := execution.PrepareRepository(ctx, h.spine, repositories.NewLocalBrokerWithToken(forkPushSecret), tenant2, execution.PrepareRepositoryInput{
		BindingID: bindingID, RunID: runID, RequestedRef: remote.head,
		WorkBranch: "agent/" + sessionID + "/" + runID,
		TargetDir:  filepath.Join(alloc, "repo"), SecretsDir: filepath.Join(alloc, "secrets"),
		AttemptFence: 1, ToolCall: "prepare",
	}); err != nil {
		t.Fatalf("fork: prepare repository: %v", err)
	}

	orch := h.newOrchestratorWithTools(subprocessDialer{engineDir: h.engineDir},
		&forkFileProvider{marker: "FORK-" + newID("m")}, tools.FileTool())
	if err := orch.ExecuteAttempt(ctx, h.workspaceDescriptor(runID, 1, alloc)); err != nil {
		t.Fatalf("fork: execute coding attempt: %v", err)
	}
	// The fork's response is under the SECOND tenant, so read its state in that scope (h.response is scoped
	// to the harness tenant — a distinct authority boundary, the point of this step).
	var st string
	if err := h.spine.Pool().QueryRow(storage.WithSystemScope(ctx),
		`SELECT state FROM responses WHERE id=$1 AND organization_id=$2 AND project_id=$3`,
		responseID, tenant2.Organization, tenant2.Project).Scan(&st); err != nil {
		t.Fatalf("fork: read response state: %v", err)
	}
	if st != "completed" {
		t.Fatalf("fork run state = %q, want completed (separate authority coding run)", st)
	}
	if _, err := os.Stat(filepath.Join(alloc, "repo", "fork.txt")); err != nil {
		t.Fatalf("fork: file tool did not write fork.txt: %v", err)
	}
	return forkPushSecret
}

// --- step 10: evidence -----------------------------------------------------------------------------

// automationReceipt is the journey's captured evidence for the automation-0.1.0 bundle.
type automationReceipt struct {
	dedupeOriginal, dedupeDuplicate string
	occurrenceID                    string
	plannedAt, admittedAt           time.Time
	callbackDelivery                string
	webhookDeliveryID               string
	callbackAttempts                int
	recoveryRunID                   string
	recoveryProof                   recovery.RecoveryProof
	secrets                         []string
}

// writeAndVerifyAutomationEvidence builds an automation-0.1.0-shaped manifest from the journey's REAL rows —
// the dedupe linkage, the single canonical occurrence, the once-delivered callback, and the run's §26.12
// RecoveryProof — and verifies it clean through the shared verifier with all four automation rules active
// (0 findings, 0 secret findings including the inbound + callback + fork-push secrets as needles). It
// writes to a TEMP dir, not the tracked release path: the manifest carries fresh ids every run, so writing
// the tracked file would dirty the tree; the tracked automation-0.1.0 snapshot is the committed
// deterministic bundle.
func (h *harness) writeAndVerifyAutomationEvidence(t *testing.T, r automationReceipt) {
	t.Helper()
	root := strings.TrimSpace(mustGit(t, "rev-parse", "--show-toplevel"))
	modelRun := func(id string, extra map[string]any) map[string]any {
		c := map[string]any{
			"id": id, "status": "PASS", "proof_class": "e2e-deterministic",
			"run_id": r.recoveryRunID, "image_digest": "sha256:" + strings.Repeat("a", 64),
			"provider_request_id": "prov_report", "mtls_enroll": "runner-local cn=controller",
			"terminal": map[string]any{"type": "response.completed", "count": 1},
			"usage":    map[string]int{"input_tokens": 5, "output_tokens": 3, "total_tokens": 8},
		}
		for k, v := range extra {
			c[k] = v
		}
		return c
	}
	manifest := map[string]any{
		"release": "automation-0.1.0", "api_version": "v1",
		"git_sha":     strings.TrimSpace(mustGit(t, "-C", root, "rev-parse", "--short", "HEAD")),
		"migration":   latestMigrationName(t, root),
		"captured_at": time.Now().UTC().Format(time.RFC3339),
		"cases": []any{
			modelRun("AUT-001", map[string]any{
				"dedupe_claim": "deduplicated",
				"dedupe_proof": map[string]any{
					"original_delivery_id": r.dedupeOriginal, "duplicate_delivery_id": r.dedupeDuplicate, "canonical_action_count": 1,
				},
				"db_assertions": []string{
					"a duplicated inbound event (same source_event_id) produced ONE canonical delivery + one run; the duplicate row links the original (original linkage)",
					"single-step E08 run pin: the automation run is a single-step real run, no model->tool advertising claim",
				},
				"checksum": hashCoding(r.recoveryRunID, r.dedupeOriginal, "dedupe"),
			}),
			modelRun("AUT-007", map[string]any{
				"occurrence_claim": "single_canonical",
				"occurrence_proof": map[string]any{
					"occurrence_id": r.occurrenceID, "planned_at": r.plannedAt.UTC().Format(time.RFC3339),
					"admitted_at": r.admittedAt.UTC().Format(time.RFC3339), "canonical_count": 1,
				},
				"db_assertions": []string{
					"occurrence unique by (schedule, revision, planned_at); a re-tick added no second occurrence; lateness (planned_at vs admitted_at) visible",
					"single-PG replicas ceiling: the two-replica race is the AUT-007 component proof; here one hand-triggered ticker fires the canonical occurrence",
				},
				"checksum": hashCoding(r.recoveryRunID, r.occurrenceID, "occurrence"),
			}),
			modelRun("AUT-013", map[string]any{
				"callback_claim": "delivered_once",
				"callback_proof": map[string]any{
					"delivery_id": r.callbackDelivery, "webhook_delivery_id": r.webhookDeliveryID,
					"attempts": r.callbackAttempts, "receiver_receipt_count": 1, "run_terminal_intact": true,
				},
				"db_assertions": []string{
					"a run-terminal callback was delivered exactly once (the receiver deduped a 5xx retry to a single semantic receipt); callback_state=delivered",
					"the callback delivery did NOT disturb the run terminal; E13 encryption-at-rest is a named future, no 'encrypted' claim here",
				},
				"checksum": hashCoding(r.recoveryRunID, r.callbackDelivery, "callback"),
			}),
			modelRun("ENG-004", map[string]any{
				"recovery_claim": "continued",
				"recovery_proof": r.recoveryProof,
				"db_assertions": []string{
					"a real SIGKILL at the report boundary recovered via the compatible_checkpoint ladder to a SINGLE report; the completed file/shell tools were not replayed",
					"every checkpoint object was scanned for the inbound signing secret and carried no credential (checkpoint-byte secret scan); harness-source deterministic tier, not a live provider",
				},
				"checksum": hashCoding(r.recoveryRunID, "recovery"),
			}),
		},
	}
	dir := t.TempDir()
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatalf("marshal automation manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), append(raw, '\n'), 0o644); err != nil {
		t.Fatalf("write automation manifest: %v", err)
	}
	summary, err := uat.VerifyRelease(dir, r.secrets)
	if err != nil {
		t.Fatalf("verify automation bundle: %v", err)
	}
	if !summary.OK() || summary.SecretFindings != 0 {
		t.Fatalf("automation-0.1.0 evidence did not verify clean: %v", summary.Findings)
	}
	t.Logf("evidence (automation-0.1.0): %s", summary.String())
}
