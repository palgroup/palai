//go:build component

package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/palgroup/palai/adapters/integrations/slack"
	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/internal/extensions"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	statemachines "github.com/palgroup/palai/packages/state-machines"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
	"github.com/palgroup/palai/storage"
)

// E23 T8 — THE GENERIC APPROVAL GETS THE DECISION SURFACE THE PUBLICATION ONE ALREADY HAD.
//
// WHAT THE EXIT GATE MEASURED AND FILED AS `HIL-P8`, and what this file exists to make false: T1 built the
// gate, T4 built the three-button argument table, T5 advertised MCP write tools as approvable — and NOTHING
// JOINED THEM. `slack.ToolApprovalMessage` had no production caller; `coordinator.DecideToolApproval` had no
// production caller. So a gated non-publication call parked its run and was released by the EXPIRY REAPER,
// half an hour later, having asked nobody. It failed CLOSED, which is exactly why it survived six tasks.
//
// The publication half is the template and it is followed statement for statement:
//
//	RequestToolApproval (the tx that parks the run)
//	  → EnqueueApprovalMessage        — the SAME durable order-to-ask, the SAME table, one queue
//	  → SlackApprovalPump.Tick        — claims it, and the claim that matched says WHICH SCREEN
//	  → slack.ToolApprovalMessage     — its FIRST production caller
//	  → SlackAdmitter.Decide          — the click, through the SAME five bindings a publication click passes
//	  → coordinator.DecideToolApproval — its FIRST production caller; the wake is inside it
//
// EVERY LINK BELOW IS THE PRODUCTION ONE. The dispatcher is the real `dispatchTool`, the ask is the real
// `SlackApprovalPump`, the click is the real `SlackAdmitter.Decide`, and the decision is the real
// `DecideToolApproval`. Two things are doubles and both are named where they are built: slack.com (a local
// server, as every Slack component test in this tree uses) and the gated tool's executor (a counter — "did
// anything run" must not be a matter of interpretation).
//
// THE ORDER OF THE ASSERTIONS IS THE ORDER THE DANGER RUNS, the discipline slack_publish_component_test.go
// set: the counter is read while nobody has decided, and only then does a click arrive.

// toolApprovalSlackHarness is a real spine carrying a Slack-born session parked on a GATED TOOL CALL: a
// registered connection whose only authorized approver is Uapprover, a thread correlated to the session,
// and a counting executor for the tool nobody may run without a human.
//
// It does NOT reuse publishHarness: that one prepares a git workspace, a repository binding and a
// preparation receipt because a publication needs a destination. A gated tool call needs none of it, and a
// harness that skips on missing git for a test about Jira would be a test that quietly does not run.
type toolApprovalSlackHarness struct {
	t         *testing.T
	spine     *coordinator.Store
	tenant    coordinator.Tenant
	sessionID string
	runID     string
	callID    string
	executed  *int32
	broker    func() *toolbroker.Broker
	bridge    *extensions.SlackAdmitter
	conn      api.SlackConnectionRef
	calls     *[]slackCall

	team, channel, thread, botMessageTS string
}

// gatedToolArgs are the bytes the whole chain is bound to: the ledger row's `arguments`, the screen's
// table, and the request hash the button carries are all derived from THIS map.
var gatedToolArgs = map[string]any{"issue": "PAL-42", "status": "Done"}

func newToolApprovalSlackHarness(t *testing.T) *toolApprovalSlackHarness {
	t.Helper()
	cs, tenant, sessionID, runID := openLedgerSpine(t)

	var executed int32
	tool := gatedTool
	tool.InputSchema = map[string]any{"type": "object"}
	tool.OutputSchema = map[string]any{"type": "object"}
	tool.Exec = func(context.Context, toolbroker.ExecEnv, map[string]any) (map[string]any, error) {
		atomic.AddInt32(&executed, 1)
		return map[string]any{"ok": true}, nil
	}

	h := &toolApprovalSlackHarness{
		t: t, spine: cs, tenant: tenant, sessionID: sessionID, runID: runID,
		callID: redeliveryID("tc"), executed: &executed,
		team:    strings.ToUpper(redeliveryID("T")),
		channel: "C" + redeliveryID("chan"),
		thread:  "1700000700.000100",
	}
	h.botMessageTS = h.thread
	h.broker = func() *toolbroker.Broker { return toolbroker.New(tool) }
	h.wireSlack()
	return h
}

// wireSlack registers a real Slack connection whose ONLY authorized approver is Uapprover, correlates this
// harness's thread to its session, and builds the production SlackAdmitter with its decision half.
func (h *toolApprovalSlackHarness) wireSlack() {
	h.t.Helper()
	h.bridge, h.conn, h.calls = wireSlackForSession(h.t, h.spine, h.tenant, h.sessionID, h.team, h.channel, h.thread)
}

// wireSlackForSession registers a Slack connection whose ONLY authorized approver is Uapprover, correlates
// one thread to one session, and builds the production SlackAdmitter with its decision half against a local
// stand-in for slack.com. It returns the bridge, the resolved connection, and the recorded outbound calls.
//
// The admitter is nil: neither caller admits anything (they decide an approval on a session that already
// exists), and passing a real one would import the whole admission path into a test about approval.
func wireSlackForSession(t *testing.T, spine *coordinator.Store, tenant coordinator.Tenant,
	sessionID, team, channel, thread string) (*extensions.SlackAdmitter, api.SlackConnectionRef, *[]slackCall) {

	t.Helper()
	ctx := context.Background()
	ext := extensions.New(spine.Pool())
	const signingRef, botRef = "slack/e23t8/signing", "slack/e23t8/bot"
	botToken := []byte("xoxb-e23t8-component-fake-not-a-credential")
	conn, err := ext.CreateSlackConnection(ctx, tenant.Organization, tenant.Project, []byte(fmt.Sprintf(
		`{"team_id":%q,"bot_user_id":"Ubot","signing_secret_ref":%q,"bot_token_ref":%q,"allowed_users":["Uapprover"]}`,
		team, signingRef, botRef)))
	if err != nil {
		t.Fatalf("register the Slack workspace: %v", err)
	}
	// The thread↔session correlation an app_mention writes (SLK-003). It is what makes a click able to
	// decide THIS session's approval and no other.
	execSQL(t, spine.Pool(), `INSERT INTO slack_thread_sessions
	                            (id, organization_id, project_id, connection_id, team_id, channel_id, thread_ts, session_id)
	                          VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		redeliveryID("sts"), tenant.Organization, tenant.Project, conn.ID, team, channel, thread, sessionID)

	var calls []slackCall
	slackAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls = append(calls, slackCall{path: r.URL.Path, auth: r.Header.Get("Authorization"), body: string(body)})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"ts":"1700000799.000200"}`))
	}))
	t.Cleanup(slackAPI.Close)

	secrets := func(org, ref string) ([]byte, error) {
		if org != tenant.Organization {
			return nil, fmt.Errorf("no secret bridge for %q/%q", org, ref)
		}
		switch ref {
		case botRef:
			return botToken, nil
		case signingRef:
			return []byte("component-signing-secret-not-a-credential"), nil
		}
		return nil, fmt.Errorf("no secret bridge for %q", ref)
	}
	bridge := extensions.NewSlackAdmitter(ext, nil, secrets, api.AdmissionLimits{}).
		WithDecisions(spine, http.DefaultClient, slackAPI.URL)
	ref, found, err := bridge.ResolveConnection(ctx, team, "")
	if err != nil || !found {
		t.Fatalf("resolve the registered connection: (%v,%v)", found, err)
	}
	return bridge, ref, &calls
}

// dispatch drives the REAL orchestrator tool dispatcher for one attempt, with a FRESH broker — a woken
// attempt is a new process and only the durable ledger may carry anything between them.
func (h *toolApprovalSlackHarness) dispatch(fence uint64, args map[string]any) (*recordingChannel, error) {
	h.t.Helper()
	orch, st, ch := ledgerAttempt(h.spine, h.broker(), h.tenant, h.sessionID, h.runID, fence)
	return ch, orch.dispatchTool(context.Background(), st, toolRequestFrame(h.callID, gatedTool.Name, args))
}

// post drives the REAL approval-message pump — the production loop main.go supervises. Its return is
// DISCARDED for slack_publish_component_test.go's reason: the pump claims across every tenant, so on a
// shared component database its count includes whatever another test left due. Every assertion below
// re-derives from asks(), which is scoped to THIS harness's channel.
func (h *toolApprovalSlackHarness) post() {
	h.t.Helper()
	if _, err := extensions.NewSlackApprovalPump(h.bridge).Tick(context.Background()); err != nil {
		h.t.Fatalf("SlackApprovalPump.Tick: %v", err)
	}
}

// asks returns the questions posted into THIS harness's channel.
func (h *toolApprovalSlackHarness) asks() []map[string]any {
	h.t.Helper()
	return asksIn(h.t, h.calls, h.channel)
}

// asksIn returns the decoded bodies of every chat.postMessage one channel received that carries an Approve
// button — the questions, as opposed to the repairs. DECODED, because encoding/json escapes < > & and a raw
// substring sweep over marshalled Block Kit can never fail (E20 T4's lesson).
//
// The channel filter is not decoration: the pump serves every project, so on a shared component database a
// delivery another test left due would otherwise be counted as this one's.
func asksIn(t *testing.T, calls *[]slackCall, channel string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, c := range *calls {
		if c.path != "/chat.postMessage" {
			continue
		}
		var body map[string]any
		if err := json.Unmarshal([]byte(c.body), &body); err != nil {
			t.Fatalf("an outbound Slack body is not JSON: %v", err)
		}
		if got, _ := body["channel"].(string); got != channel {
			continue
		}
		if containsString(texts(body), slack.ActionApprove) {
			out = append(out, body)
		}
	}
	return out
}

// repairs returns every chat.update this harness's channel received — the SLK-006 in-place edit.
func (h *toolApprovalSlackHarness) repairs() []map[string]any {
	h.t.Helper()
	var out []map[string]any
	for _, c := range *h.calls {
		if c.path != "/chat.update" {
			continue
		}
		var body map[string]any
		if err := json.Unmarshal([]byte(c.body), &body); err != nil {
			h.t.Fatalf("an outbound Slack body is not JSON: %v", err)
		}
		if channel, _ := body["channel"].(string); channel == h.channel {
			out = append(out, body)
		}
	}
	return out
}

// click drives the production Slack decision path for a button press by this user.
func (h *toolApprovalSlackHarness) click(user, decision, requestHash string) api.SlackDecisionOutcome {
	h.t.Helper()
	actionID := slack.ActionApprove
	if decision == "deny" {
		actionID = slack.ActionDeny
	}
	out, err := h.bridge.Decide(context.Background(), h.conn, slack.ApprovalIntent{
		TeamID: h.team, UserID: user, RequestHash: requestHash, Decision: decision, ActionID: actionID,
		ChannelID: h.channel, ThreadTS: h.thread, MessageTS: h.botMessageTS,
	})
	if err != nil {
		h.t.Fatalf("Decide(%s by %s): %v", decision, user, err)
	}
	return out
}

func (h *toolApprovalSlackHarness) runState() string {
	h.t.Helper()
	var state string
	if err := h.spine.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT state FROM runs WHERE id = $1`, h.runID).Scan(&state); err != nil {
		h.t.Fatalf("read run state: %v", err)
	}
	return state
}

func (h *toolApprovalSlackHarness) callState() string {
	h.t.Helper()
	var state string
	if err := h.spine.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT state FROM tool_calls WHERE id = $1`, h.callID).Scan(&state); err != nil {
		h.t.Fatalf("read tool_call state: %v", err)
	}
	return state
}

func (h *toolApprovalSlackHarness) ran() int32 { return atomic.LoadInt32(h.executed) }

// texts flattens one decoded Slack body to every string in it.
func texts(body map[string]any) []string {
	var out []string
	collectStrings(body, &out)
	return out
}

// carries reports whether any DECODED string in a Slack body contains want. Substring rather than equality
// because Block Kit composes: the tool identity arrives inside "**Approval requested**\n`name`\nlabel" and
// an argument's value arrives JSON-quoted. Decoding first is the E20 T4 lesson — encoding/json escapes
// < > & so a sweep over the marshalled bytes could never fail — and collectStrings is what decodes.
func carries(body map[string]any, want string) bool {
	for _, s := range texts(body) {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}

// TestSlackToolApprovalAsksInTheThreadAndAnAuthorizedClickReleasesTheRun is THE HEADLINE, and the sentence
// it proves is the one `HIL-P8` said was unreachable: a gated NON-PUBLICATION tool call produces a real ask
// in the thread, and an authorized click releases the run.
//
// At the fork point (1221956) it fails at the first ask: `RequestToolApproval` committed no order to post,
// the pump's only claim joined `publications`, and so the thread received nothing at all.
func TestSlackToolApprovalAsksInTheThreadAndAnAuthorizedClickReleasesTheRun(t *testing.T) {
	h := newToolApprovalSlackHarness(t)

	// 1. THE MODEL CALLS THE GATED TOOL. The dispatcher parks the run and hands the model nothing.
	ch, err := h.dispatch(1, gatedToolArgs)
	if !errors.Is(err, errRunAwaitingApproval) {
		t.Fatalf("dispatchTool error = %v, want the park", err)
	}
	if n := h.ran(); n != 0 {
		t.Fatalf("the gated tool executed %d time(s) with NO human decision, want 0", n)
	}
	if got := toolResults(ch); len(got) != 0 {
		t.Fatalf("the engine was sent %d tool.result frame(s) while a human owes an answer: %+v", len(got), got)
	}
	if got := h.runState(); got != string(statemachines.RunWaiting) {
		t.Fatalf("run state = %q while a human owes an answer, want waiting", got)
	}
	// The ORDER is durable, the message is not yet: that split is the loss-lessness claim, and it is the
	// same one the publication path makes. A pump that is down delays the question and cannot lose it.
	if got := len(h.asks()); got != 0 {
		t.Fatalf("%d approval message(s) went out before the pump ran", got)
	}

	// 2. THE ASK. This is the assertion that was unreachable: the pump posts into the correlated thread.
	h.post()
	msgs := h.asks()
	if len(msgs) != 1 {
		t.Fatalf("the thread received %d ask(s) for a gated tool call, want 1 — this is the half HIL-P8 filed "+
			"as missing: a gated call that nobody can approve parks its run until the expiry reaper", len(msgs))
	}
	if got, _ := msgs[0]["channel"].(string); got != h.channel {
		t.Fatalf("the question was posted to channel %q, want the correlated %q", got, h.channel)
	}
	if got, _ := msgs[0]["thread_ts"].(string); got != h.thread {
		t.Fatalf("the question was posted to thread %q, want the correlated %q", got, h.thread)
	}

	// 3. AND IT IS THE GENERIC SCREEN, chosen from the approvals ROW rather than from anything the model
	// said. A publication row gets E19's two-button `ApprovalMessage`; this row names a tool call, so it
	// gets T4's three-button argument table — the third button is the discriminator that cannot be faked.
	ask := msgs[0]
	if !containsString(texts(ask), slack.ActionShowArguments) {
		t.Fatalf("the ask carries no Show-arguments button, so it is the PUBLICATION screen: %v", texts(ask))
	}
	if !carries(ask, gatedTool.Name) {
		t.Fatalf("the ask does not name the tool a human is authorizing (%s): %v", gatedTool.Name, texts(ask))
	}
	// The arguments a human reads are the LEDGER row's committed bytes, one table row each.
	for _, want := range []string{"issue", "PAL-42", "status", "Done"} {
		if !carries(ask, want) {
			t.Fatalf("the argument table does not carry %q — a human is being asked about bytes they cannot see: %v", want, texts(ask))
		}
	}
	// And every button is bound to the BYTES: the one-shot request hash on the ledger row.
	hash := requestHashFor(gatedToolArgs)
	if !containsString(texts(ask), hash) {
		t.Fatalf("no button carries the call's one-shot request hash %q: %v", hash, texts(ask))
	}
	// THE OPERATOR'S LABEL SLOT IS RENDERED OUT LOUD. `gatedTool` is a Go-declared BUILT-IN, and LookupTool
	// is the per-tenant REGISTRY chain — so a built-in's `ApprovalLabel` reaches neither this screen nor
	// T4's shipped modal, and both say so in the same words rather than silently dropping the line. The
	// registered case, which is the one HIL-P8 names, is proven on a real revision by
	// TestSlackToolApprovalCarriesTheOperatorsOwnLabelForARegisteredTool.
	if !carries(ask, slack.NoOperatorLabel) {
		t.Fatalf("the ask omits the operator-label line entirely; a missing sentence must be VISIBLY missing: %v", texts(ask))
	}

	// EXACTLY ONCE: a second tick asks nothing. UNIQUE (approval_id) plus a claim that IS the update.
	h.post()
	if got := len(h.asks()); got != 1 {
		t.Fatalf("a second pump tick asked the same question again (%d asks in the thread)", got)
	}

	// 4. THE CLICK RELEASES THE RUN. `DecideToolApproval`'s first production caller.
	out := h.click("Uapprover", "approve", hash)
	if out.Rejected != "" {
		t.Fatalf("an authorized approver's click on a gated tool call was refused: %q", out.Rejected)
	}
	if out.ToolCallID != h.callID {
		t.Fatalf("the outcome names tool call %q, want %q — a decision that cannot say what it decided is "+
			"not a decision an operator can read", out.ToolCallID, h.callID)
	}
	if out.PublicationID != "" {
		t.Fatalf("a gated tool call's decision reported publication %q — the two are mutually exclusive", out.PublicationID)
	}
	if got := h.callState(); got != "ready" {
		t.Fatalf("tool_call state after the approve = %q, want ready", got)
	}
	if got := h.runState(); got != "running" {
		t.Fatalf("run state after the approve = %q, want running — the wake did not fire, so the human's "+
			"click released nothing", got)
	}
	// The wake enqueues the response.run job in the SAME transaction, so a worker opens a fresh attempt.
	var jobs int
	if err := h.spine.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(*) FROM durable_jobs WHERE kind='response.run' AND payload->>'run_id' = $1`, h.runID).Scan(&jobs); err != nil {
		t.Fatalf("count the wake jobs: %v", err)
	}
	if jobs != 1 {
		t.Fatalf("the approve enqueued %d response.run job(s) for this run, want 1", jobs)
	}

	// 5. THE MESSAGE IS REPAIRED IN PLACE (SLK-006) — the same handle, edited, never a second post.
	reps := h.repairs()
	if len(reps) != 1 {
		t.Fatalf("the decision produced %d in-place repair(s), want 1", len(reps))
	}
	if got, _ := reps[0]["ts"].(string); got != h.botMessageTS {
		t.Fatalf("the repair edited message %q, want the clicked %q", got, h.botMessageTS)
	}
	if !carries(reps[0], "Approved: "+gatedTool.Name) {
		t.Fatalf("the repaired message does not say what was decided: %v", texts(reps[0]))
	}

	// 6. AND THE RUN ACTUALLY RUNS IT, once, on the bytes that were shown. The woken attempt replays to the
	// same tool.request and the ledger consult now finds `ready`.
	if _, err := h.dispatch(2, gatedToolArgs); err != nil {
		t.Fatalf("the woken attempt failed: %v", err)
	}
	if n := h.ran(); n != 1 {
		t.Fatalf("the approved tool ran %d time(s), want exactly 1", n)
	}

	// No credential is on any outbound body; the bot token rides the Authorization header alone.
	for _, c := range *h.calls {
		if strings.Contains(c.body, "xoxb-") {
			t.Fatalf("an outbound Slack body carried a bot token: %s", c.path)
		}
		if c.auth != "" && !strings.HasPrefix(c.auth, "Bearer ") {
			t.Fatalf("outbound Authorization on %s is %q", c.path, c.auth)
		}
	}
}

// TestSlackToolApprovalRefusesAnUnauthorizedClicker is THE NEGATIVE, and it is not a formality: T2's
// approver policy was written and proven for PUBLICATIONS, in ApplyApprovalDecision, which a tool-call
// decision does not pass through. A tool path that reached DecideToolApproval directly would have been a
// second decision surface with no approver check at all — a widening committed by accident.
//
// Two independent lists refuse here, and the test presses both: the CONNECTION's allowed_users (checked in
// Decide, so no decision function is even reached) and the PROJECT's config_policy.approvers (checked
// inside the one throat both kinds pass through).
func TestSlackToolApprovalRefusesAnUnauthorizedClicker(t *testing.T) {
	ctx := context.Background()
	h := newToolApprovalSlackHarness(t)
	if _, err := h.dispatch(1, gatedToolArgs); !errors.Is(err, errRunAwaitingApproval) {
		t.Fatalf("dispatchTool error = %v, want the park", err)
	}
	h.post()
	hash := requestHashFor(gatedToolArgs)

	// 1. A clicker outside the CONNECTION's allow-list decides nothing.
	if got := h.click("Uintruder", "approve", hash); got.Rejected == "" {
		t.Fatalf("an unmapped user's click on a gated tool call was accepted: %+v", got)
	}
	if got := h.callState(); got != "approval_pending" {
		t.Fatalf("an unauthorized click moved the gated call to %q", got)
	}
	if got := h.runState(); got != string(statemachines.RunWaiting) {
		t.Fatalf("an unauthorized click woke the parked run (%q)", got)
	}
	if n := h.ran(); n != 0 {
		t.Fatalf("the gated tool executed %d time(s) after an unauthorized click, want 0", n)
	}

	// 2. Now the PROJECT's approver list names somebody else. Uapprover is still in the connection's
	// allowed_users, so this refusal can only come from config_policy.approvers — the T2 list, applied to a
	// tool call and not only to a publication.
	execSQL(t, h.spine.Pool(), `UPDATE projects SET config_policy = $3::jsonb WHERE organization_id = $1 AND id = $2`,
		h.tenant.Organization, h.tenant.Project, `{"approvers":["slack:OTHERTEAM:Usomebodyelse"]}`)
	if got := h.click("Uapprover", "approve", hash); got.Rejected == "" {
		t.Fatalf("a clicker outside the PROJECT's approver list decided a gated tool call: %+v", got)
	}
	if got := h.callState(); got != "approval_pending" {
		t.Fatalf("a click refused by the project's approver list still moved the gated call to %q", got)
	}
	if n := h.ran(); n != 0 {
		t.Fatalf("the gated tool executed %d time(s) after a refused click, want 0", n)
	}

	// 3. And the same principal, once the list names them, decides. Without this leg the two refusals above
	// would also pass on a build where NOTHING can ever decide — which is precisely the bug being fixed.
	execSQL(t, h.spine.Pool(), `UPDATE projects SET config_policy = $3::jsonb WHERE organization_id = $1 AND id = $2`,
		h.tenant.Organization, h.tenant.Project,
		fmt.Sprintf(`{"approvers":["slack:%s:Uapprover"]}`, h.team))
	if got := h.click("Uapprover", "approve", hash); got.Rejected != "" {
		t.Fatalf("the named approver's click was refused: %q", got.Rejected)
	}
	if got := h.callState(); got != "ready" {
		t.Fatalf("tool_call state after the named approver decided = %q, want ready", got)
	}
	_ = ctx
}

// registeredGatedToolLabel is the operator's own sentence — written at publish time, beside the per-tool
// approval decision they already make. It is the ONLY human sentence on the approval screen, and the whole
// reason 000044 R3 gave it a column of its own instead of reusing a discovered revision's `description`:
// the description is written by the SERVER being authorized.
const registeredGatedToolLabel = "the shared service account may move tickets, and only in PAL"

// seedRegisteredGatedRun seeds a REGISTERED, PUBLISHED, gated tool revision and a run granted it — the
// shape an operator actually produces, and the shape HIL-P8 names. It returns the session and run.
func seedRegisteredGatedRun(t *testing.T, cs *coordinator.Store, tenant coordinator.Tenant, modelName string) (sessionID, runID string) {
	t.Helper()
	pool := cs.Pool()
	org, project := tenant.Organization, tenant.Project
	toolID, trevID := redeliveryID("tool"), redeliveryID("trev")
	setID, profileID, arevID := redeliveryID("tsrev"), redeliveryID("aprof"), redeliveryID("arev")
	sessionID, runID = redeliveryID("ses"), redeliveryID("run")

	execSQL(t, pool, `INSERT INTO tools (id, organization_id, project_id, canonical_name, model_visible_name)
	                  VALUES ($1,$2,$3,$4,$5)`, toolID, org, project, "reg."+modelName, modelName)
	// approval_required + approval_label are 000044 R3's columns, set exactly where the operator sets them.
	execSQL(t, pool, `INSERT INTO tool_revisions (id, organization_id, project_id, tool_id, revision_number,
	                      executor, description, input_schema, replay_class, digest, published_at,
	                      approval_required, approval_label)
	                  VALUES ($1,$2,$3,$4,1,'control_plane',$5,'{"type":"object"}'::jsonb,'irreversible',$6,
	                          clock_timestamp(), true, $7)`,
		trevID, org, project, toolID, "moves a ticket", "sha256:"+trevID, registeredGatedToolLabel)
	execSQL(t, pool, `INSERT INTO tool_set_revisions (id, organization_id, project_id, set_name, revision_number,
	                      tool_pins, digest, published_at)
	                  VALUES ($1,$2,$3,$4,1,$5::jsonb,'d',clock_timestamp())`,
		setID, org, project, "set-"+modelName, `[{"tool_revision_id":"`+trevID+`"}]`)
	execSQL(t, pool, `INSERT INTO agent_profiles (id, organization_id, project_id, name) VALUES ($1,$2,$3,$4)`,
		profileID, org, project, profileID)
	execSQL(t, pool, `INSERT INTO agent_revisions (id, organization_id, project_id, profile_id, revision_number,
	                      model, published_at, tool_sets, mcp_connections)
	                  VALUES ($1,$2,$3,$4,1,'model-x',clock_timestamp(),$5::jsonb,'[]'::jsonb)`,
		arevID, org, project, profileID, `["`+setID+`"]`)
	execSQL(t, pool, `INSERT INTO sessions (id, organization_id, project_id) VALUES ($1,$2,$3)`, sessionID, org, project)
	execSQL(t, pool, `INSERT INTO runs (id, organization_id, project_id, session_id, agent_revision_id, state)
	                  VALUES ($1,$2,$3,$4,$5,'running')`, runID, org, project, sessionID, arevID)
	return sessionID, runID
}

// TestSlackToolApprovalCarriesTheOperatorsOwnLabelForARegisteredTool closes the loop the previous test
// leaves open, on the case that actually ships: a tool REGISTERED and PUBLISHED with `approval_required`,
// which is the only way a deployment produces a gated non-publication call at all (a built-in declares its
// gate in Go and is not what HIL-P5/HIL-P8 are about).
//
// WHAT IT PROVES BEYOND THE ASK ITSELF: the poster resolves the operator's sentence through the SAME
// LookupTool the modal and the executor resolve the call through. That is not tidiness — it is why the
// message in the channel, the modal opened from it, and the tool that finally runs cannot disagree about
// what a human authorized. A poster that had frozen a copy of the label at enqueue would show a stale
// sentence after a re-publish, and the sentence is the whole human-readable half of the screen.
func TestSlackToolApprovalCarriesTheOperatorsOwnLabelForARegisteredTool(t *testing.T) {
	cs, tenant, _, _ := openLedgerSpine(t)
	sessionID, runID := seedRegisteredGatedRun(t, cs, tenant, "transitionIssue")
	team := strings.ToUpper(redeliveryID("T"))
	channel, thread := "C"+redeliveryID("chan"), "1700000701.000100"
	bridge, _, calls := wireSlackForSession(t, cs, tenant, sessionID, team, channel, thread)

	// The REAL registry lookup behind the broker, exactly as main.go wires it: nothing about this tool is
	// declared in Go, so `approval_required` is read off the published revision and nowhere else.
	registry := extensions.New(cs.Pool())
	broker := toolbroker.New()
	broker.SetLookup(registryLookup(registry))
	orch := &Orchestrator{spine: cs, tools: broker}
	st := &attemptState{
		attempt:   AttemptDescriptor{RunID: contracts.RunID(runID), AttemptID: contracts.AttemptID(redeliveryID("att")), Fence: 1},
		tenant:    tenant,
		sessionID: sessionID,
		ch:        &recordingChannel{},
	}
	callID := redeliveryID("tc")
	args := map[string]any{"issue": "PAL-77"}
	if err := orch.dispatchTool(context.Background(), st, toolRequestFrame(callID, "transitionIssue", args)); !errors.Is(err, errRunAwaitingApproval) {
		t.Fatalf("dispatchTool error = %v, want the park — a REGISTERED approval_required revision must gate", err)
	}

	if _, err := extensions.NewSlackApprovalPump(bridge).Tick(context.Background()); err != nil {
		t.Fatalf("SlackApprovalPump.Tick: %v", err)
	}

	// The one question this channel was asked.
	asks := asksIn(t, calls, channel)
	if len(asks) != 1 {
		t.Fatalf("the thread received %d ask(s) for a registered gated tool, want 1", len(asks))
	}
	ask := asks[0]
	if !carries(ask, registeredGatedToolLabel) {
		t.Fatalf("the operator's own sentence is not on the screen a human decides from: %v", texts(ask))
	}
	if carries(ask, slack.NoOperatorLabel) {
		t.Fatalf("the screen says there is no operator label while the revision carries one: %v", texts(ask))
	}
}

// TestSlackToolApprovalDenyReleasesTheRunAndRunsNothing is the other half of an answer. A deny must not be
// silence: the call is canceled with a reason the model can act on, the run is released, and the effect
// never happens.
func TestSlackToolApprovalDenyReleasesTheRunAndRunsNothing(t *testing.T) {
	h := newToolApprovalSlackHarness(t)
	if _, err := h.dispatch(1, gatedToolArgs); !errors.Is(err, errRunAwaitingApproval) {
		t.Fatalf("dispatchTool error = %v, want the park", err)
	}
	h.post()

	out := h.click("Uapprover", "deny", requestHashFor(gatedToolArgs))
	if out.Rejected != "" {
		t.Fatalf("an authorized approver's deny was refused: %q", out.Rejected)
	}
	if got := h.callState(); got != "canceled" {
		t.Fatalf("tool_call state after the deny = %q, want canceled", got)
	}
	if got := h.runState(); got != "running" {
		t.Fatalf("run state after the deny = %q, want running — a deny releases the run just as an approve does", got)
	}
	// THE ANSWER IS ON THE ROW, durable before it is spoken: the model continues on a fact rather than
	// stalling on an absence.
	var result string
	if err := h.spine.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT coalesce(result::text, '') FROM tool_calls WHERE id = $1`, h.callID).Scan(&result); err != nil {
		t.Fatalf("read the denied call's answer: %v", err)
	}
	var answer map[string]any
	if err := json.Unmarshal([]byte(result), &answer); err != nil {
		t.Fatalf("the denied call's answer is not JSON: %q", result)
	}
	if answer["status"] != "denied" {
		t.Fatalf("the denied call's answer is %v, want status=denied", answer)
	}
	if reason, _ := answer["reason"].(string); reason == "" {
		t.Fatalf("the deny carries no reason: %v — a denial with no reason is a wall, and the whole point of "+
			"handing one back is that the agent can act on it", answer)
	}
	if n := h.ran(); n != 0 {
		t.Fatalf("the denied tool executed %d time(s), want 0", n)
	}
	// The visible message says so.
	reps := h.repairs()
	if len(reps) != 1 || !carries(reps[0], "Denied: "+gatedTool.Name) {
		t.Fatalf("the deny did not repair the visible message: %+v", reps)
	}
}
