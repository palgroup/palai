//go:build component

package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/internal/extensions"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// E23 T9 — THE SLACK-LESS DECISION SURFACE, against a REAL spine and the REAL router.
//
// THE HOLE THIS FILE WAS BORN FROM, measured on main at cb99e3d: E23 T8 wired a decision surface and
// wired it to SLACK ONLY. `coordinator.DecideToolApproval` — the one throat every decision passes
// through — had exactly ONE production caller (extensions/slack_decision.go:283), reachable only through
// POST /v1/slack/interactions (router.go:367), and there was no /v1/approvals route of any kind. So an
// operator who self-hosts WITHOUT Slack had a gate that parked the run, asked nobody, and let the expiry
// reaper release it half an hour later. It fails CLOSED, which is why it was invisible.
//
// EVERY TEST HERE DRIVES THE SHIPPED ROUTER over a REAL Postgres: the run is parked by the REAL
// dispatcher against a REGISTERED, PUBLISHED, gated revision (the only shape a deployment actually
// produces), the key is resolved by the REAL VerifyAPIKey, and the decision goes through the production
// handler into DecideToolApproval. Nothing is hand-composed.

// httpApprovalFixture is one parked gated call plus the HTTP surface a human would decide it on.
type httpApprovalFixture struct {
	t          *testing.T
	spine      *coordinator.Store
	tenant     coordinator.Tenant
	repo       *store.Store
	srv        *httptest.Server
	sessionID  string
	runID      string
	callID     string
	approvalID string
	args       map[string]any
	hash       string
	key        httpApprover
}

// httpApprovalToolName is the model-visible name seedRegisteredGatedRun publishes the gated revision under.
const httpApprovalToolName = "transitionIssue"

// newHTTPApprovalFixture parks a run on a REGISTERED gated tool call and mints a verified key for it.
//
// It uses seedRegisteredGatedRun rather than a Go-declared built-in deliberately: a built-in declares its
// gate in source and its ApprovalLabel reaches no registry, so a screen built from one could not prove the
// operator's registration-time sentence travels. HIL-P8 is about the registered shape.
func newHTTPApprovalFixture(t *testing.T, scopes []string) *httpApprovalFixture {
	t.Helper()
	cs, tenant, _, _ := openLedgerSpine(t)
	sessionID, runID := seedRegisteredGatedRun(t, cs, tenant, httpApprovalToolName)

	f := &httpApprovalFixture{
		t: t, spine: cs, tenant: tenant, sessionID: sessionID, runID: runID,
		callID: redeliveryID("tc"), args: map[string]any{"issue": "PAL-77", "status": "Done"},
	}
	f.hash = toolbroker.RequestHash(httpApprovalToolName, f.args)

	parkGatedCall(t, cs, tenant, sessionID, runID, f.callID, httpApprovalToolName, f.args)
	f.approvalID = scalar(t, cs.Pool(), `SELECT id FROM approvals WHERE tool_call_id = $1`, f.callID)
	if f.approvalID == "" {
		t.Fatal("the park opened no approvals row")
	}

	f.repo = approverHTTP(t)
	f.key = mintScopedKey(t, f.repo, cs, tenant, scopes)
	f.srv = httptest.NewServer(approvalRouter(f.repo))
	t.Cleanup(f.srv.Close)
	return f
}

// parkGatedCall drives ONE call through the REAL dispatcher and the REAL registry lookup — exactly as
// main.go wires it, so `approval_required` is read off the published revision and nowhere else. It fails
// the test unless the run actually PARKS: a gated call that answered would make every assertion below
// about a question nobody was asked.
func parkGatedCall(t *testing.T, cs *coordinator.Store, tenant coordinator.Tenant, sessionID, runID, callID, name string, args map[string]any) {
	t.Helper()
	broker := toolbroker.New()
	broker.SetLookup(registryLookup(extensions.New(cs.Pool())))
	orch := &Orchestrator{spine: cs, tools: broker}
	st := &attemptState{
		attempt:   AttemptDescriptor{RunID: contracts.RunID(runID), AttemptID: contracts.AttemptID(redeliveryID("att")), Fence: 1},
		tenant:    tenant,
		sessionID: sessionID,
		ch:        &recordingChannel{},
	}
	if err := orch.dispatchTool(context.Background(), st, toolRequestFrame(callID, name, args)); !errors.Is(err, errRunAwaitingApproval) {
		t.Fatalf("dispatchTool(%s) error = %v, want the park — a REGISTERED approval_required revision must gate", name, err)
	}
}

// parkAnother parks a SECOND gated call in the SAME project, on its own run and its own registered tool.
// The tool name has to differ: two published revisions sharing one model-visible name is the ambiguous
// grant E23 T5 made LookupTool REFUSE, so reusing the name would test the refusal rather than the page.
func (f *httpApprovalFixture) parkAnother(t *testing.T, name string) string {
	t.Helper()
	sessionID, runID := seedRegisteredGatedRun(t, f.spine, f.tenant, name)
	callID := redeliveryID("tc")
	parkGatedCall(t, f.spine, f.tenant, sessionID, runID, callID, name, map[string]any{"issue": "PAL-78"})
	return callID
}

// approvalRouter builds the SHIPPED router carrying the approval surface. Only the verifier is wired: this
// file is about one surface, and a router that mounts nothing else proves the routes are that surface's
// own rather than something another mount happens to answer.
func approvalRouter(repo *store.Store) http.Handler {
	return api.NewRouter(repo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		api.SSEConfig{}, nil, nil, api.WithApprovals(repo))
}

// httpApprover is a minted API key as an HTTP caller: the bearer token, and the canonical principal a
// project's approver list would have to name to admit it.
type httpApprover struct {
	token     string
	principal string
}

// mintScopedKey creates a real api_keys row with the given capability scopes and resolves it through the
// REAL VerifyAPIKey, so the principal an approver list matches is the one authentication established —
// never a struct literal.
func mintScopedKey(t *testing.T, repo *store.Store, cs *coordinator.Store, tenant coordinator.Tenant, scopes []string) httpApprover {
	t.Helper()
	if scopes == nil {
		scopes = []string{}
	}
	principalID, keyID := redeliveryID("prin"), redeliveryID("key")
	token := "palai_" + redeliveryID("tok")
	execSQL(t, cs.Pool(), `INSERT INTO principals (id, organization_id, project_id, kind) VALUES ($1,$2,$3,'service')`,
		principalID, tenant.Organization, tenant.Project)
	execSQL(t, cs.Pool(), `INSERT INTO api_keys (id, organization_id, project_id, principal_id, key_hash, scopes)
	                       VALUES ($1,$2,$3,$4,$5,$6)`,
		keyID, tenant.Organization, tenant.Project, principalID, coordinator.HashAPIKey(token), scopes)

	scope, err := repo.VerifyAPIKey(context.Background(), token)
	if err != nil {
		t.Fatalf("VerifyAPIKey() error = %v", err)
	}
	if scope.Organization != tenant.Organization || scope.Project != tenant.Project {
		t.Fatalf("the minted key verified into %s/%s, want %s/%s",
			scope.Organization, scope.Project, tenant.Organization, tenant.Project)
	}
	return httpApprover{
		token:     token,
		principal: coordinator.ApproverPrincipal(coordinator.ApproverSurfaceKey, "", scope.APIKeyID),
	}
}

// seedForeignTenantKey mints a key in a DIFFERENT organization and project on the same database — the
// cross-tenant negative's other side.
func seedForeignTenantKey(t *testing.T, repo *store.Store, cs *coordinator.Store) httpApprover {
	t.Helper()
	other := coordinator.Tenant{Organization: redeliveryID("org"), Project: redeliveryID("prj")}
	execSQL(t, cs.Pool(), `INSERT INTO organizations (id) VALUES ($1)`, other.Organization)
	execSQL(t, cs.Pool(), `INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, other.Project, other.Organization)
	return mintScopedKey(t, repo, cs, other, nil)
}

// get issues an authenticated GET and returns the status plus the decoded body.
func (f *httpApprovalFixture) get(t *testing.T, k httpApprover, path string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, f.srv.URL+path, nil)
	if err != nil {
		t.Fatalf("build GET %s: %v", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+k.token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer res.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(res.Body).Decode(&body)
	return res.StatusCode, body
}

// post issues an authenticated POST with a JSON body.
func (f *httpApprovalFixture) post(t *testing.T, k httpApprover, path string, body any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, f.srv.URL+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("build POST %s: %v", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+k.token)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer res.Body.Close()
	var decoded map[string]any
	_ = json.NewDecoder(res.Body).Decode(&decoded)
	return res.StatusCode, decoded
}

// item finds this fixture's parked call in a list body, or reports that the page does not carry it.
func (f *httpApprovalFixture) item(body map[string]any) (map[string]any, bool) {
	rows, _ := body["data"].([]any)
	for _, row := range rows {
		m, ok := row.(map[string]any)
		if ok && m["tool_call_id"] == f.callID {
			return m, true
		}
	}
	return nil, false
}

func (f *httpApprovalFixture) callState(t *testing.T) string {
	t.Helper()
	return scalar(t, f.spine.Pool(), `SELECT state FROM tool_calls WHERE id = $1`, f.callID)
}

func (f *httpApprovalFixture) runState(t *testing.T) string {
	t.Helper()
	return scalar(t, f.spine.Pool(), `SELECT state FROM runs WHERE id = $1`, f.runID)
}

func (f *httpApprovalFixture) setApprovers(t *testing.T, policy string) {
	t.Helper()
	execSQL(t, f.spine.Pool(), `UPDATE projects SET config_policy = $3::jsonb WHERE organization_id = $1 AND id = $2`,
		f.tenant.Organization, f.tenant.Project, policy)
}

// TestHTTPToolApprovalListShowsTheParkedCallWithItsArgumentsVerbatim is RED #1: a human cannot decide what
// they cannot see, and "enough to decide on" is this epic's own definition — the identity resolved
// SERVER-side by the lookup that will execute, the operator's registration-time sentence, and the
// arguments verbatim.
func TestHTTPToolApprovalListShowsTheParkedCallWithItsArgumentsVerbatim(t *testing.T) {
	f := newHTTPApprovalFixture(t, nil)

	status, body := f.get(t, f.key, "/v1/approvals")
	if status != http.StatusOK {
		t.Fatalf("GET /v1/approvals = %d, want 200 — a Slack-less operator has no other way to see the question", status)
	}
	got, found := f.item(body)
	if !found {
		t.Fatalf("the parked call %s is not in the list a human decides from: %v", f.callID, body)
	}
	if got["identity"] != httpApprovalToolName {
		t.Fatalf("identity = %v, want the name the EXECUTOR resolved (%q)", got["identity"], httpApprovalToolName)
	}
	if got["operator_label"] != registeredGatedToolLabel {
		t.Fatalf("operator_label = %v, want the operator's own registration-time sentence %q", got["operator_label"], registeredGatedToolLabel)
	}
	if got["request_hash"] != f.hash {
		t.Fatalf("request_hash = %v, want the one-shot binding %q", got["request_hash"], f.hash)
	}
	// THE ARGUMENTS VERBATIM. Decoded and compared as a value, never as a substring: encoding/json escapes
	// < > and &, so a raw substring sweep over rendered JSON can never fail (E20 T4's lesson).
	rendered, _ := got["arguments"].(string)
	var shown map[string]any
	if err := json.Unmarshal([]byte(rendered), &shown); err != nil {
		t.Fatalf("the screen's arguments are not decodable JSON (%q): %v", rendered, err)
	}
	if fmt.Sprint(shown) != fmt.Sprint(f.args) {
		t.Fatalf("the screen shows %v, want the ledger row's own arguments %v", shown, f.args)
	}
}

// TestHTTPToolApprovalTheListPagesForwardRatherThanRepeatingItself: with two questions open and a page of
// one, the second page holds the OTHER question. A list that mints a next_cursor and then resumes from
// nothing hands a client page 1 forever — with has_more still true, so a script draining the queue never
// terminates and never sees the second approval. This surface shipped that bug for one draft.
func TestHTTPToolApprovalTheListPagesForwardRatherThanRepeatingItself(t *testing.T) {
	f := newHTTPApprovalFixture(t, nil)
	second := f.parkAnother(t, httpApprovalToolName+"Two")

	status, body := f.get(t, f.key, "/v1/approvals?limit=1")
	if status != http.StatusOK {
		t.Fatalf("GET page 1 = %d, want 200", status)
	}
	firstPage := callIDsIn(body)
	if len(firstPage) != 1 || firstPage[0] != f.callID {
		t.Fatalf("page 1 = %v, want exactly the OLDEST question (%s)", firstPage, f.callID)
	}
	cursor, _ := body["next_cursor"].(string)
	if has, _ := body["has_more"].(bool); !has || cursor == "" {
		t.Fatalf("page 1 reports has_more=%v cursor=%q with a second question open", body["has_more"], cursor)
	}

	status, body = f.get(t, f.key, "/v1/approvals?limit=1&after="+url.QueryEscape(cursor))
	if status != http.StatusOK {
		t.Fatalf("GET page 2 = %d, want 200", status)
	}
	secondPage := callIDsIn(body)
	if len(secondPage) != 1 || secondPage[0] != second {
		t.Fatalf("page 2 = %v, want the SECOND question (%s) — a repeated page 1 never terminates", secondPage, second)
	}
	if has, _ := body["has_more"].(bool); has {
		t.Fatalf("page 2 still reports has_more with only two questions open: %v", body)
	}
}

// callIDsIn lists the tool_call_ids a page carries, in order.
func callIDsIn(body map[string]any) []string {
	rows, _ := body["data"].([]any)
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		if m, ok := row.(map[string]any); ok {
			id, _ := m["tool_call_id"].(string)
			out = append(out, id)
		}
	}
	return out
}

// TestHTTPToolApprovalApproveWithTheBoundHashReleasesTheParkedRun is RED #2: the decision goes through
// coordinator.DecideToolApproval and nothing else, so the call transitions, the journal records it, and
// the parked run is WOKEN — in one transaction.
func TestHTTPToolApprovalApproveWithTheBoundHashReleasesTheParkedRun(t *testing.T) {
	f := newHTTPApprovalFixture(t, nil)
	if got := f.runState(t); got != "waiting" {
		t.Fatalf("run state before the decision = %q, want waiting", got)
	}

	status, body := f.post(t, f.key, "/v1/approvals/"+f.approvalID+"/approve",
		map[string]string{"request_hash": f.hash})
	if status != http.StatusOK {
		t.Fatalf("POST approve = %d (%v), want 200", status, body)
	}
	if got := f.callState(t); got != "ready" {
		t.Fatalf("tool_call state after an authorized approve = %q, want ready", got)
	}
	if got := f.runState(t); got != "running" {
		t.Fatalf("run state after the approve = %q, want running (the wake did not fire)", got)
	}
	// The wake enqueues the response.run job in the SAME transaction as the decision, so nothing dispatches
	// before the decision is durable.
	jobs := scalar(t, f.spine.Pool(),
		`SELECT count(*)::text FROM durable_jobs WHERE kind = 'response.run' AND payload->>'run_id' = $1`, f.runID)
	if jobs != "1" {
		t.Fatalf("response.run jobs after the approve = %s, want 1", jobs)
	}
	if n := eventCountFor(t, f.spine, f.sessionID, "approval.approved.v1"); n != 1 {
		t.Fatalf("approval.approved.v1 events = %d, want exactly 1", n)
	}
	// WHO decided is the VERIFIED key, stamped server-side — never a body field.
	decided := scalar(t, f.spine.Pool(), `SELECT decided_by FROM approvals WHERE id = $1`, f.approvalID)
	if decided != f.key.principal {
		t.Fatalf("decided_by = %q, want the verified key's own principal %q", decided, f.key.principal)
	}
}

// TestHTTPToolApprovalAStaleRequestHashDecidesNothing: the Slack button carries tool_calls.request_hash so
// that arguments changed after the approval leave no approval. The HTTP caller supplies the SAME hash and
// a mismatch must refuse — a decision keyed only by an approval id would be a weaker second path.
func TestHTTPToolApprovalAStaleRequestHashDecidesNothing(t *testing.T) {
	f := newHTTPApprovalFixture(t, nil)
	stale := toolbroker.RequestHash(httpApprovalToolName, map[string]any{"issue": "PAL-77", "status": "Rejected"})
	if stale == f.hash {
		t.Fatal("the stale hash equals the bound one; the test would be vacuous")
	}

	status, body := f.post(t, f.key, "/v1/approvals/"+f.approvalID+"/approve",
		map[string]string{"request_hash": stale})
	if status != http.StatusConflict {
		t.Fatalf("POST approve with a stale hash = %d (%v), want 409", status, body)
	}
	if got := f.callState(t); got != "approval_pending" {
		t.Fatalf("tool_call state after a stale-hash approve = %q, want approval_pending", got)
	}
	if got := f.runState(t); got != "waiting" {
		t.Fatalf("run state after a stale-hash approve = %q, want waiting", got)
	}
}

// TestHTTPToolApprovalADecisionWithNoRequestHashIsRefused is the structural half of the binding: an
// approval id ALONE must not authorize anything. DecideToolApproval skips the hash comparison when the
// field is empty (the native command surface never carried one), so the refusal has to be at this edge or
// the HTTP surface would be the one place the binding does not hold.
func TestHTTPToolApprovalADecisionWithNoRequestHashIsRefused(t *testing.T) {
	f := newHTTPApprovalFixture(t, nil)

	status, _ := f.post(t, f.key, "/v1/approvals/"+f.approvalID+"/approve", map[string]string{})
	if status != http.StatusBadRequest {
		t.Fatalf("POST approve with no request_hash = %d, want 400 — an approval id alone authorizes nothing", status)
	}
	if got := f.callState(t); got != "approval_pending" {
		t.Fatalf("tool_call state = %q, want approval_pending", got)
	}
}

// TestHTTPToolApprovalAnotherTenantsKeyReadsNothingAndDecidesNothing is the cross-tenant negative. A
// pending approval belongs to a project; another tenant's key must see no trace of it and be unable to
// decide it — and the refusal must be a non-disclosing 404, never a 403 that confirms the id exists.
func TestHTTPToolApprovalAnotherTenantsKeyReadsNothingAndDecidesNothing(t *testing.T) {
	f := newHTTPApprovalFixture(t, nil)
	outsider := seedForeignTenantKey(t, f.repo, f.spine)

	status, body := f.get(t, outsider, "/v1/approvals")
	if status != http.StatusOK {
		t.Fatalf("GET /v1/approvals for the outsider = %d, want 200 (its OWN empty page)", status)
	}
	if _, found := f.item(body); found {
		t.Fatalf("another tenant's pending approval is visible to an outsider: %v", body)
	}

	status, _ = f.post(t, outsider, "/v1/approvals/"+f.approvalID+"/approve",
		map[string]string{"request_hash": f.hash})
	if status != http.StatusNotFound {
		t.Fatalf("an outsider's decision on another tenant's approval = %d, want 404", status)
	}
	if got := f.callState(t); got != "approval_pending" {
		t.Fatalf("tool_call state after a foreign decision = %q, want approval_pending", got)
	}
	if got := f.runState(t); got != "waiting" {
		t.Fatalf("run state after a foreign decision = %q, want waiting", got)
	}
}

// TestHTTPToolApprovalAKeyOutsideTheProjectApproverListDecidesNothing: E23 T2 made the approver a PROJECT
// policy (config_policy.approvers), not a Slack list, and DecideToolApproval applies it to every surface.
// The HTTP principal is the API KEY (`key:<api_key_id>`) — the only identity this platform has for a
// bearer caller (HIL-P2: there is no user identity), and the form ApproverPrincipal already renders.
//
// The third leg is not decoration: without it the two refusals would also pass on a build where NOTHING
// can ever decide, which is precisely the bug this task fixes.
func TestHTTPToolApprovalAKeyOutsideTheProjectApproverListDecidesNothing(t *testing.T) {
	f := newHTTPApprovalFixture(t, nil)
	f.setApprovers(t, `{"approvers":["key_somebody_else","slack:T1:U1"]}`)

	status, _ := f.post(t, f.key, "/v1/approvals/"+f.approvalID+"/approve",
		map[string]string{"request_hash": f.hash})
	if status != http.StatusForbidden {
		t.Fatalf("an unlisted key's approve = %d, want 403", status)
	}
	if got := f.callState(t); got != "approval_pending" {
		t.Fatalf("tool_call state after an unlisted approve = %q, want approval_pending", got)
	}
	// A DENY is a decision too: an unlisted key must not be able to BLOCK the call either.
	status, _ = f.post(t, f.key, "/v1/approvals/"+f.approvalID+"/deny",
		map[string]string{"request_hash": f.hash})
	if status != http.StatusForbidden {
		t.Fatalf("an unlisted key's deny = %d, want 403", status)
	}
	if got := f.callState(t); got != "approval_pending" {
		t.Fatalf("tool_call state after an unlisted deny = %q, want approval_pending", got)
	}

	// And the same key, once the list names it, decides.
	f.setApprovers(t, `{"approvers":["`+f.key.principal+`"]}`)
	if status, body := f.post(t, f.key, "/v1/approvals/"+f.approvalID+"/approve",
		map[string]string{"request_hash": f.hash}); status != http.StatusOK {
		t.Fatalf("the LISTED key's approve = %d (%v), want 200", status, body)
	}
	if got := f.callState(t); got != "ready" {
		t.Fatalf("tool_call state after the listed key decided = %q, want ready", got)
	}
}

// TestHTTPToolApprovalAKeyWithoutTheApproveCapabilityDecidesNothing is the route guard. The capability
// scope and the approver policy are two different gates — exactly as a Slack connection's allowed_users is
// not config_policy.approvers — and both have to be passed. This one is what lets an operator mint a key
// that can ONLY decide approvals: it holds `approve` and nothing else, so it cannot rewrite the approver
// list it is checked against.
func TestHTTPToolApprovalAKeyWithoutTheApproveCapabilityDecidesNothing(t *testing.T) {
	f := newHTTPApprovalFixture(t, []string{"provision"})

	if status, _ := f.get(t, f.key, "/v1/approvals"); status != http.StatusForbidden {
		t.Fatalf("GET /v1/approvals without the approve capability = %d, want 403", status)
	}
	status, _ := f.post(t, f.key, "/v1/approvals/"+f.approvalID+"/approve",
		map[string]string{"request_hash": f.hash})
	if status != http.StatusForbidden {
		t.Fatalf("POST approve without the approve capability = %d, want 403", status)
	}
	if got := f.callState(t); got != "approval_pending" {
		t.Fatalf("tool_call state = %q, want approval_pending", got)
	}
}

// TestHTTPToolApprovalDenyReleasesTheRunWithAnAnswer: a deny is an ANSWER, not silence. The call is
// canceled with a reason the model can act on and the run is released — the same shape the Slack Deny
// produces, because it is the same function.
func TestHTTPToolApprovalDenyReleasesTheRunWithAnAnswer(t *testing.T) {
	f := newHTTPApprovalFixture(t, nil)

	const reason = "not during the freeze; ask again on Monday"
	status, _ := f.post(t, f.key, "/v1/approvals/"+f.approvalID+"/deny",
		map[string]string{"request_hash": f.hash, "reason": reason})
	if status != http.StatusOK {
		t.Fatalf("POST deny = %d, want 200", status)
	}
	if got := f.callState(t); got != "canceled" {
		t.Fatalf("tool_call state after a deny = %q, want canceled", got)
	}
	if got := f.runState(t); got != "running" {
		t.Fatalf("run state after a deny = %q, want running (a denied run must be released)", got)
	}
	result := scalar(t, f.spine.Pool(), `SELECT result->>'reason' FROM tool_calls WHERE id = $1`, f.callID)
	if result != reason {
		t.Fatalf("the model is handed reason %q, want the human's own %q", result, reason)
	}
}

// eventCountFor counts one event type on a session's journal.
func eventCountFor(t *testing.T, cs *coordinator.Store, sessionID, eventType string) int {
	t.Helper()
	n := scalar(t, cs.Pool(), `SELECT count(*)::text FROM events WHERE session_id = $1 AND type = $2`, sessionID, eventType)
	var out int
	_, _ = fmt.Sscanf(n, "%d", &out)
	return out
}
