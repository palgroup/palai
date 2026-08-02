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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/integrations/slack"
	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	"github.com/palgroup/palai/apps/control-plane/internal/extensions"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	statemachines "github.com/palgroup/palai/packages/state-machines"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
	"github.com/palgroup/palai/storage"
)

// E22 T4 — a push from Slack, from the tool call to the publisher, against REAL PostgreSQL.
//
// WHAT THIS FILE EXISTS TO REFUSE, and it is the only failure in this chain that is silent: a publication
// that reaches the publisher WITHOUT a human. Everything else in the chain announces itself — a refused
// click answers, a failed push warns, an unwired publisher logs. An unapproved push would simply be a
// branch on someone's remote.
//
// So the assertion is made in the order the danger runs: the tool is called and the pump is driven while
// the publication is still PENDING, and the publisher must not have been touched. Only then does a click
// arrive. A test that approved first and then checked the publisher would have proven the approve works and
// nothing at all about what happens without one.
//
// EVERY LINK IS THE PRODUCTION ONE. The push and pull-request tools are the shipped tools.PushTool() /
// PullRequestTool(); the destination is resolved by the real publicationRegistry through
// RunPublicationTarget (so remote/branch/base come from the BINDING and the model cannot name them); the
// click runs the real SlackAdmitter.Decide — thread correlation, the connection's approver allow-list,
// AcceptCommand, ApplyApprovalDecision; and the publish half is publishApproved, the same body the
// orchestrator's boundary pump calls. Two things are doubles and both are named where they are built: the
// Publisher (a recorder — what a REAL push does to a REAL remote is the e2e coding journey and the live
// leg), and slack.com itself (a local server; the outbound half is proven in internal/store).
//
// THE APPROVAL SURFACE IS NOT NEW (plan §3.6 D9). ApprovalMessage has minted Approve/Deny since E19 T2 and
// only SLACK_APPROVER_IDS may press them; blocks_test.go:165 pins interactions.go as the sole minter. E22
// touches none of it. What was missing was a publication for those buttons to decide, and that is what a
// repository binding on a Slack connection (T3) plus the publish tools (T4) supply.

// recordingPublisher stands in for RepositoryPublisher: it records every target it was asked to publish and
// answers a receipt. It never touches a remote — what a real push does to a real remote is proven by the
// deterministic coding journey (a faithful local git remote) and by tests/live/repository against a real
// GitHub App. What matters HERE is the count: zero while a human has not decided, one after they approve.
type recordingPublisher struct{ targets []PublishTarget }

func (p *recordingPublisher) Publish(_ context.Context, target PublishTarget) (map[string]any, error) {
	p.targets = append(p.targets, target)
	switch target.Publication.Operation {
	case "open_pull_request":
		return map[string]any{"pull_request_id": "PR_1", "number": 1, "draft": true,
			"url": "https://github.test/acme/widgets/pull/1"}, nil
	default:
		return map[string]any{"remote": target.Publication.Remote, "branch": target.Publication.Branch,
			"remote_sha": target.Publication.HeadSHA}, nil
	}
}

func (p *recordingPublisher) operations() []string {
	var ops []string
	for _, t := range p.targets {
		ops = append(ops, t.Publication.Operation)
	}
	return ops
}

// slackCall is one outbound request the local stand-in for slack.com received.
type slackCall struct{ path, auth, body string }

// publishHarness is a real spine carrying an active Slack-born session: a repository binding whose
// default_branch is `dev`, a prepared workspace with a real git repo and a real commit, a thread correlated
// to the session, and a connection whose only authorized approver is Uapprover.
type publishHarness struct {
	t         *testing.T
	spine     *coordinator.Store
	tenant    coordinator.Tenant
	sessionID string
	runID     string
	respID    string
	bindingID string
	branch    string
	head      string
	root      string // the workspace allocation root; the repo lives at root/repo
	remote    string
	bridge    *extensions.SlackAdmitter
	conn      api.SlackConnectionRef
	publisher *recordingPublisher
	calls     *[]slackCall
	// slackRefuses turns the stand-in workspace into one that answers every call with a documented API
	// error, so SLK-006 can be measured rather than asserted: a question Slack will not take must not cost
	// the approval, its deadline, or the run.
	slackRefuses bool

	team, channel, thread, botMessageTS string
}

// publishBaseBranch is the value the whole task turns on: `dev` is a BINDING value an operator set in
// .env.local (PALAI_GIT_BASE_BRANCH), never a constant in this tree. It is spelled once here so a test that
// stopped reading it off the binding would stop compiling rather than quietly assert a literal.
const publishBaseBranch = "dev"

func newPublishHarness(t *testing.T) *publishHarness {
	t.Helper()
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run TEST=postgres scripts/test/component")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not found: %v", err)
	}
	ctx := context.Background()
	cs, err := coordinator.Open(ctx, url)
	if err != nil {
		t.Fatalf("coordinator.Open() error = %v", err)
	}
	t.Cleanup(cs.Close)
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}

	h := &publishHarness{
		t: t, spine: cs, publisher: &recordingPublisher{},
		tenant:    coordinator.Tenant{Organization: redeliveryID("org"), Project: redeliveryID("prj")},
		sessionID: redeliveryID("ses"), runID: redeliveryID("run"), respID: redeliveryID("resp"),
		bindingID: redeliveryID("bnd"), remote: "https://github.test/acme/widgets.git",
		team: strings.ToUpper(redeliveryID("T")), channel: "C" + redeliveryID("chan"),
		thread: "1700000900.000100",
	}
	h.botMessageTS = h.thread
	h.branch = "agent/" + h.sessionID + "/" + h.runID

	pool := cs.Pool()
	execSQL(t, pool, `INSERT INTO organizations (id) VALUES ($1)`, h.tenant.Organization)
	execSQL(t, pool, `INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, h.tenant.Project, h.tenant.Organization)
	execSQL(t, pool, `INSERT INTO sessions (id, organization_id, project_id, state) VALUES ($1,$2,$3,'active')`,
		h.sessionID, h.tenant.Organization, h.tenant.Project)
	execSQL(t, pool, `INSERT INTO responses (id, organization_id, project_id, session_id, state) VALUES ($1,$2,$3,$4,'in_progress')`,
		h.respID, h.tenant.Organization, h.tenant.Project, h.sessionID)
	execSQL(t, pool, `INSERT INTO runs (id, organization_id, project_id, session_id, response_id, state) VALUES ($1,$2,$3,$4,$5,'running')`,
		h.runID, h.tenant.Organization, h.tenant.Project, h.sessionID, h.respID)

	// The binding. default_branch is what a pull request will target, and NOTHING else decides it.
	if err := cs.CreateRepositoryBinding(ctx, h.tenant, coordinator.RepositoryBindingInput{
		BindingID: h.bindingID, Provider: "github", RepositoryIdentity: "acme/widgets",
		CloneURL: h.remote, DefaultBranch: publishBaseBranch,
	}); err != nil {
		t.Fatalf("create the repository binding: %v", err)
	}
	// The preparation receipt is what RunPublicationTarget joins the binding to; PrepareRepository writes
	// one after a real clone. Written directly here because the clone is not what is under test — the
	// deterministic coding journey drives the real PrepareRepository against a faithful git remote.
	if err := cs.RecordPreparationReceipt(ctx, h.tenant, coordinator.PreparationReceiptInput{
		ReceiptID: redeliveryID("prep"), BindingID: h.bindingID, RunID: h.runID,
		RequestedRef: publishBaseBranch, BaseCommit: "0000000", TreeHash: "0000000", Branch: h.branch,
	}); err != nil {
		t.Fatalf("record the preparation receipt: %v", err)
	}

	h.root, h.head = newWorkspaceWithACommit(t)
	h.bridge, h.conn, h.calls = h.wireSlack(t, cs)
	return h
}

// newWorkspaceWithACommit prepares a workspace allocation with a real git repository holding one commit —
// the head the push tool reads. Returns the allocation root and that head.
func newWorkspaceWithACommit(t *testing.T) (root, head string) {
	t.Helper()
	root = t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	if err := workspace.Prepare(root); err != nil {
		t.Fatalf("prepare the workspace allocation: %v", err)
	}
	repoDir := filepath.Join(root, workspace.RepoDir)
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repoDir, "feature.txt"), []byte("agent edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "the change a human is about to approve")
	return root, git("rev-parse", "HEAD")
}

// wireSlack registers a real Slack connection whose ONLY authorized approver is Uapprover, correlates the
// harness thread to the harness session, and builds the production SlackAdmitter with its decision half.
//
// The admitter is nil: Decide never admits anything (it decides an approval on a session that already
// exists), and passing a real one would import the whole admission path into a test about publication.
func (h *publishHarness) wireSlack(t *testing.T, cs *coordinator.Store) (*extensions.SlackAdmitter, api.SlackConnectionRef, *[]slackCall) {
	t.Helper()
	ctx := context.Background()
	ext := extensions.New(cs.Pool())
	const signingRef, botRef = "slack/e22t4/signing", "slack/e22t4/bot"
	botToken := []byte("xoxb-e22t4-component-fake-not-a-credential")
	conn, err := ext.CreateSlackConnection(ctx, h.tenant.Organization, h.tenant.Project, []byte(fmt.Sprintf(
		`{"team_id":%q,"bot_user_id":"Ubot","signing_secret_ref":%q,"bot_token_ref":%q,"allowed_users":["Uapprover"]}`,
		h.team, signingRef, botRef)))
	if err != nil {
		t.Fatalf("register the Slack workspace: %v", err)
	}
	// The thread↔session correlation an app_mention writes (SLK-003). The events route that writes it is
	// proven in internal/store; what is needed here is that the click's thread owns THIS session, because
	// that correlation is what makes a click able to decide this publication and no other.
	execSQL(t, cs.Pool(), `INSERT INTO slack_thread_sessions
	                         (id, organization_id, project_id, connection_id, team_id, channel_id, thread_ts, session_id)
	                       VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		redeliveryID("sts"), h.tenant.Organization, h.tenant.Project, conn.ID, h.team, h.channel, h.thread, h.sessionID)

	var calls []slackCall
	slackAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls = append(calls, slackCall{path: r.URL.Path, auth: r.Header.Get("Authorization"), body: string(body)})
		w.Header().Set("Content-Type", "application/json")
		if h.slackRefuses {
			// A workspace that will not take the message (E23 T3, SLK-006). `not_in_channel` is a real
			// documented error and the shape decodeChatResponse reads.
			_, _ = w.Write([]byte(`{"ok":false,"error":"not_in_channel"}`))
			return
		}
		// A ts, so a posted question has the handle a later chat.update repairs in place.
		_, _ = w.Write([]byte(`{"ok":true,"ts":"1700000999.000200"}`))
	}))
	t.Cleanup(slackAPI.Close)

	secrets := func(org, ref string) ([]byte, error) {
		if org != h.tenant.Organization {
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
		WithDecisions(cs, http.DefaultClient, slackAPI.URL)
	ref, found, err := bridge.ResolveConnection(ctx, h.team, "")
	if err != nil || !found {
		t.Fatalf("resolve the registered connection: (%v,%v)", found, err)
	}
	return bridge, ref, &calls
}

// execEnv is the per-call environment the orchestrator hands a tool: this run's scope, this attempt's
// workspace, and the REAL publication registry.
func (h *publishHarness) execEnv() toolbroker.ExecEnv {
	return toolbroker.ExecEnv{
		WorkspaceRoot: h.root,
		Publications:  newPublicationRegistry(h.spine, nil),
		Scope: toolbroker.TaskScope{
			Org: h.tenant.Organization, Project: h.tenant.Project,
			SessionID: h.sessionID, RunID: h.runID, ResponseID: h.respID,
		},
	}
}

// propose calls a shipped publication tool the way a model's tool_call would and returns its result.
func (h *publishHarness) propose(tool toolbroker.Tool, args map[string]any) map[string]any {
	h.t.Helper()
	out, err := tool.Exec(context.Background(), h.execEnv(), args)
	if err != nil {
		h.t.Fatalf("%s: %v", tool.Name, err)
	}
	if status, _ := out["status"].(string); status != "pending_approval" {
		h.t.Fatalf("%s answered status %q, want pending_approval — a publication tool that acts is the one thing "+
			"the approval boundary cannot survive", tool.Name, status)
	}
	return out
}

// dispatch drives the REAL orchestrator tool dispatcher for one attempt — not the tool in isolation, which
// is what propose() does. That difference is the whole of E23 T3: what changed is not the tool (it records
// a pending publication exactly as it did) but what the dispatcher does with the receipt.
//
// A fresh broker and a fresh attempt state each time, because a woken attempt is a NEW process and only
// the durable ledger may carry anything between them.
func (h *publishHarness) dispatch(tool toolbroker.Tool, callID string, fence uint64, args map[string]any) (*recordingChannel, error) {
	h.t.Helper()
	ch := &recordingChannel{}
	orch := &Orchestrator{
		spine: h.spine, tools: toolbroker.New(tool), publications: newPublicationRegistry(h.spine, nil),
	}
	st := &attemptState{
		attempt: AttemptDescriptor{
			RunID: contracts.RunID(h.runID), AttemptID: contracts.AttemptID(redeliveryID("att")),
			Fence: fence, WorkspaceHostPath: h.root,
		},
		tenant: h.tenant, sessionID: h.sessionID, responseID: h.respID, ch: ch,
	}
	return ch, orch.dispatchTool(context.Background(), st, toolRequestFrame(callID, tool.Name, args))
}

// post drives the REAL approval-message pump — the production loop main.go supervises. Its return value is
// deliberately DISCARDED: the pump is a system loop that claims across every tenant, so on a shared
// component database its count includes whatever other tests left due. Every assertion below re-derives
// from approvalMessages(), which is scoped to THIS harness's channel.
func (h *publishHarness) post() {
	h.t.Helper()
	if _, err := extensions.NewSlackApprovalPump(h.bridge).Tick(context.Background()); err != nil {
		h.t.Fatalf("SlackApprovalPump.Tick: %v", err)
	}
}

// approvalMessages returns the decoded bodies of every chat.postMessage THIS harness's channel received
// that carries an actionable element — the questions, as opposed to the repairs and the answers. The
// channel filter is not decoration: the pump serves every project, so a delivery another test left behind
// would otherwise be counted as this one's.
func (h *publishHarness) approvalMessages() []map[string]any {
	h.t.Helper()
	var out []map[string]any
	for _, c := range *h.calls {
		if c.path != "/chat.postMessage" {
			continue
		}
		var body map[string]any
		if err := json.Unmarshal([]byte(c.body), &body); err != nil {
			h.t.Fatalf("an outbound Slack body is not JSON: %v", err)
		}
		if channel, _ := body["channel"].(string); channel != h.channel {
			continue
		}
		var texts []string
		collectStrings(body, &texts)
		if containsString(texts, slack.ActionApprove) {
			out = append(out, body)
		}
	}
	return out
}

// runState reads the durable run state — `waiting` is the claim this task exists to make true.
func (h *publishHarness) runState() string {
	h.t.Helper()
	var state string
	if err := h.spine.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT state FROM runs WHERE id = $1`, h.runID).Scan(&state); err != nil {
		h.t.Fatalf("read run state: %v", err)
	}
	return state
}

// pump drives the REAL approval pump — the body Orchestrator.pumpApprovedPublications calls at every
// model-loop boundary — and returns how many publications the publisher was asked to perform in total.
func (h *publishHarness) pump() int {
	h.t.Helper()
	if err := publishApproved(context.Background(), h.spine, h.publisher, h.tenant,
		h.runID, h.sessionID, h.respID, h.root, 1, publicationCredential{}); err != nil {
		h.t.Fatalf("publishApproved: %v", err)
	}
	return len(h.publisher.targets)
}

// click drives the production Slack decision path for a button press by this user.
func (h *publishHarness) click(user, decision, requestHash string) api.SlackDecisionOutcome {
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

// publicationRow reads one durable publication of this run by operation.
func (h *publishHarness) publicationRow(operation string) (id, state, remote, branch, base, display string) {
	h.t.Helper()
	if err := h.spine.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT id, state, remote, branch, base, display FROM publications WHERE run_id=$1 AND operation=$2`,
		h.runID, operation).Scan(&id, &state, &remote, &branch, &base, &display); err != nil {
		h.t.Fatalf("read the %s publication: %v", operation, err)
	}
	return id, state, remote, branch, base, display
}

// requestHash is the one-shot binding minted into the approval button's value.
func (h *publishHarness) requestHash() string {
	h.t.Helper()
	pub, found, err := h.spine.PendingApprovalForSession(context.Background(), h.tenant, h.sessionID)
	if err != nil || !found {
		h.t.Fatalf("read the session's pending approval: (%v,%v)", found, err)
	}
	return pub.RequestHash
}

// TestPublicationFromSlackWaitsForAnApproveAndThenPublishes is E22 T4's whole claim in one run, asserted in
// the order the danger runs.
func TestPublicationFromSlackWaitsForAnApproveAndThenPublishes(t *testing.T) {
	h := newPublishHarness(t)

	// 1. The model asks. The tool does not push — it records a pending publication and says so.
	out := h.propose(tools.PushTool(), nil)
	pubID, state, remote, branch, _, display := h.publicationRow("push_branch")
	if state != "pending_approval" {
		t.Fatalf("the publication is %q straight out of the tool call, want pending_approval", state)
	}
	if remote != h.remote || branch != h.branch {
		t.Fatalf("the pending push targets %s/%s, want the BINDING's %s and the run's work branch %s — the "+
			"destination is resolved from the binding, never supplied by the model", remote, branch, h.remote, h.branch)
	}
	if got, _ := out["display"].(string); got != display || !strings.Contains(display, h.head) {
		t.Fatalf("the display the model was shown (%q) and the one the row carries (%q) must be the same string, "+
			"and it must name the exact head %s that will be pushed", got, display, h.head)
	}

	// 2. THE RED HALF. The pump runs — as it does at every model-loop boundary, whether or not anybody has
	// decided anything — and the publisher must not be touched.
	if n := h.pump(); n != 0 {
		t.Fatalf("the publisher was called %d time(s) for a publication nobody approved: %v. Everything else in "+
			"this chain announces itself; an unapproved push is just a branch appearing on a remote",
			n, h.publisher.operations())
	}
	if _, state, _, _, _, _ := h.publicationRow("push_branch"); state != "pending_approval" {
		t.Fatalf("a boundary moved an undecided publication to %q", state)
	}

	// 3. A click from somebody who is not an approver moves nothing either (SLK-004, deny by default). The
	// connection's allow-list is ["Uapprover"], and this is the same button carrying the same hash.
	hash := h.requestHash()
	if got := h.click("Uintruder", "approve", hash); got.Rejected == "" {
		t.Fatalf("an unmapped user's click was accepted: %+v", got)
	}
	if _, state, _, _, _, _ := h.publicationRow("push_branch"); state != "pending_approval" {
		t.Fatalf("an unauthorized click left the publication %q", state)
	}
	if n := h.pump(); n != 0 {
		t.Fatalf("the publisher was called after an UNAUTHORIZED click (%v)", h.publisher.operations())
	}

	// 4. The approver presses Approve, and the whole production chain runs.
	decision := h.click("Uapprover", "approve", hash)
	if decision.Rejected != "" {
		t.Fatalf("the authorized approver's click was refused: %q", decision.Rejected)
	}
	if _, state, _, _, _, _ := h.publicationRow("push_branch"); state != "approved" {
		t.Fatalf("the publication is %q after an approve, want approved", state)
	}

	// 5. The next boundary publishes it — exactly once, to exactly the approved destination.
	if n := h.pump(); n != 1 {
		t.Fatalf("the publisher was called %d time(s) after the approve, want exactly 1: %v", n, h.publisher.operations())
	}
	target := h.publisher.targets[0]
	if target.Publication.ID != pubID || target.Publication.Remote != h.remote ||
		target.Publication.Branch != h.branch || target.Publication.HeadSHA != h.head {
		t.Fatalf("published %+v, want the approved row: remote %s, branch %s, head %s",
			target.Publication, h.remote, h.branch, h.head)
	}
	if target.WorkspaceRoot != h.root {
		t.Fatalf("the publish target's workspace root is %q, want the attempt's allocation %q", target.WorkspaceRoot, h.root)
	}
	if _, state, _, _, _, _ := h.publicationRow("push_branch"); state != "published" {
		t.Fatalf("the publication is %q after a successful publish, want published", state)
	}
	// And a re-driven boundary (a lost ack, E10's detached execution) publishes NOTHING a second time.
	if n := h.pump(); n != 1 {
		t.Fatalf("a second boundary published again (%d calls): a re-drive must find the row already published", n)
	}
}

// TestPublicationFromSlackDenialPreventsThePushEntirely is the half a decision surface is worth nothing
// without. Recording a "denied" verdict is not the guarantee; the guarantee is that the side effect does not
// happen — so this drives the pump AFTER the deny, and again after the run is CANCELLED, and the publisher
// must never have been asked for anything at all.
func TestPublicationFromSlackDenialPreventsThePushEntirely(t *testing.T) {
	ctx := context.Background()
	h := newPublishHarness(t)
	h.propose(tools.PushTool(), nil)

	if got := h.click("Uapprover", "deny", h.requestHash()); got.Rejected != "" {
		t.Fatalf("the deny click was refused: %q", got.Rejected)
	}
	if _, state, _, _, _, _ := h.publicationRow("push_branch"); state != "denied" {
		t.Fatalf("the publication is %q after a deny, want denied", state)
	}
	approved, err := h.spine.ApprovedPublicationsForRun(ctx, h.tenant, h.runID)
	if err != nil {
		t.Fatalf("ApprovedPublicationsForRun: %v", err)
	}
	if len(approved) != 0 {
		t.Fatalf("a denied publication is in the publishable set: %+v", approved)
	}
	if n := h.pump(); n != 0 {
		t.Fatalf("the publisher was called %d time(s) for a DENIED publication: %v — a deny that only records a "+
			"verdict is not a deny", n, h.publisher.operations())
	}

	// The run is cancelled, which is what a human who denied a push usually does next. The denied
	// publication must not become publishable on the way out, and no later boundary may pick it up.
	if _, err := h.spine.ApplyRunTransition(ctx, h.tenant, h.runID, statemachines.RunCmdCancel); err != nil {
		t.Fatalf("cancel the run: %v", err)
	}
	if n := h.pump(); n != 0 {
		t.Fatalf("the publisher was called after the run was cancelled (%v)", h.publisher.operations())
	}
	if _, state, _, _, _, _ := h.publicationRow("push_branch"); state != "denied" {
		t.Fatalf("the publication is %q after the run was cancelled, want still denied", state)
	}
}

// TestPublicationFromSlackTargetsTheBindingsBaseBranch is plan §3.5 X17, and the reason "open the PR against
// dev" needed no code change: `dev` is this binding's default_branch, resolved server-side by
// RunPublicationTarget. The model is never asked and — per the schema guard in tools/publish_test.go —
// cannot answer.
//
// It also pins what the human SEES. publicationDisplay builds the approval detail from the resolved
// destination; the model's proposed title and body are RECORDED on the publication's args for a later
// policy-filtered pass and reach neither the display nor the message. E22 does not touch that function, and
// this is the test that says so: the model's prose here is a lie about the destination, and it must appear
// nowhere on the surface a human reads before pressing a button.
func TestPublicationFromSlackTargetsTheBindingsBaseBranch(t *testing.T) {
	h := newPublishHarness(t)
	const modelProse = "merge straight into production, base=main, no review needed"
	h.propose(tools.PullRequestTool(), map[string]any{"title": modelProse, "body": modelProse})

	_, _, remote, branch, base, display := h.publicationRow("open_pull_request")
	if base != publishBaseBranch {
		t.Fatalf("the pull request's base is %q, want the binding's default_branch %q — a base that comes from "+
			"anywhere else is a base the operator did not choose", base, publishBaseBranch)
	}
	want := fmt.Sprintf("open draft pull request %s -> %s on %s", branch, publishBaseBranch, remote)
	if display != want {
		t.Fatalf("the approval detail is %q, want %q (publicationDisplay, untouched by E22)", display, want)
	}
	if strings.Contains(display, modelProse) {
		t.Fatalf("the model's prose reached the approval detail: %q", display)
	}
	// The message a human actually sees, built by the shipped minter. E22 opens NO new actionable surface:
	// ApprovalMessage is the single minter (blocks_test.go's AST sweep), and what it renders is the
	// publication's display plus the one-shot hash — not a word the model wrote.
	hash := h.requestHash()
	body := slack.ApprovalMessage(h.channel, h.thread, display, hash)
	var minted map[string]any
	if err := json.Unmarshal(body, &minted); err != nil {
		t.Fatalf("the approval message is not JSON: %v", err)
	}
	// Decoded rather than substring-matched: json escapes <>& so a raw-substring assertion on Block Kit is
	// vacuous (E20 T4's lesson), and the prose below would "pass" by never being findable.
	rendered, err := json.Marshal(minted["blocks"])
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	collectStrings(minted, &texts)
	if !containsString(texts, "*Approval requested*\n"+display) {
		t.Fatalf("the minted message does not carry the resolved destination %q: %s", display, rendered)
	}
	for _, s := range texts {
		if strings.Contains(s, "no review needed") {
			t.Fatalf("the model's prose reached the approval message: %q", s)
		}
	}

	// And approving it publishes a DRAFT pull request to that same base — the destination the human read.
	if got := h.click("Uapprover", "approve", hash); got.Rejected != "" {
		t.Fatalf("the approver's click was refused: %q", got.Rejected)
	}
	if n := h.pump(); n != 1 {
		t.Fatalf("the publisher was called %d time(s) after the approve, want 1: %v", n, h.publisher.operations())
	}
	if got := h.publisher.targets[0].Publication.Base; got != publishBaseBranch {
		t.Fatalf("the publisher was handed base %q, want %q", got, publishBaseBranch)
	}
	// The model's title/body survive as ARGS on the row (a later policy-filtered pass), which is the honest
	// ceiling: E09 opens the PR with a deterministic title/body, so the prose is recorded, never published.
	var args string
	if err := h.spine.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT COALESCE(args::text,'{}') FROM publications WHERE run_id=$1 AND operation='open_pull_request'`,
		h.runID).Scan(&args); err != nil {
		t.Fatalf("read the publication args: %v", err)
	}
	if !strings.Contains(args, "merge straight into production") {
		t.Fatalf("the model's proposal was dropped rather than recorded: %s", args)
	}
	// The credential claim for this surface: nothing the harness holds as a secret is on any outbound Slack
	// body, and the bot token rides the Authorization header alone.
	for _, c := range *h.calls {
		if strings.Contains(c.body, "xoxb-") {
			t.Fatalf("an outbound Slack body carried a bot token: %s", c.path)
		}
		if c.auth != "" && !strings.HasPrefix(c.auth, "Bearer ") {
			t.Fatalf("outbound Authorization on %s is %q", c.path, c.auth)
		}
	}
}

// TestPublicationFromSlackPostsAnApprovalMessageIntoTheThread is E23 T3's RED #1, and it was born red for
// the plainest possible reason: no code in this tree posted an approval message. slack.ApprovalMessage has
// minted Approve and Deny since E19 T2 and every caller of it was a test or a credential-gated live smoke —
// which is how E22 could close with "the agent publishes behind a human's button" while no human in a real
// workspace had ever been shown one. The tree said so itself, in two places, and nobody read either.
//
// So this drives the production path end to end: the real dispatcher runs the shipped push tool, the real
// coordinator records the publication AND the order to ask about it in one transaction, and the real pump
// posts it. What lands in the thread is asserted DECODED, because encoding/json escapes < > & and a raw
// substring sweep over marshalled Block Kit can never fail (E20 T4's lesson).
func TestPublicationFromSlackPostsAnApprovalMessageIntoTheThread(t *testing.T) {
	h := newPublishHarness(t)

	if _, err := h.dispatch(tools.PushTool(), redeliveryID("tc"), 1, nil); !errors.Is(err, errRunParked) {
		t.Fatalf("dispatchTool error = %v, want the park", err)
	}
	// Nothing has been asked yet — the ORDER is durable, the message is not. That split is the loss-lessness
	// claim: the pump can be down for the whole window and still deliver what the approval committed.
	if got := len(h.approvalMessages()); got != 0 {
		t.Fatalf("%d approval message(s) went out before the pump ran", got)
	}

	h.post()
	msgs := h.approvalMessages()
	if len(msgs) != 1 {
		t.Fatalf("the thread received %d approval message(s), want 1 — this is the half E22 did not have", len(msgs))
	}
	_, _, _, _, _, display := h.publicationRow("push_branch")
	var texts []string
	collectStrings(msgs[0], &texts)
	if !containsString(texts, "*Approval requested*\n"+display) {
		t.Fatalf("the posted question does not carry the resolved destination %q: %v", display, texts)
	}
	if !strings.Contains(display, h.head) || !strings.Contains(display, h.remote) {
		t.Fatalf("the destination a human was shown (%q) does not name the exact head %s and the binding's remote %s",
			display, h.head, h.remote)
	}
	// It went to the THREAD the run was born in, not to a channel of the poster's choosing.
	if got, _ := msgs[0]["channel"].(string); got != h.channel {
		t.Fatalf("the question was posted to channel %q, want the correlated %q", got, h.channel)
	}
	if got, _ := msgs[0]["thread_ts"].(string); got != h.thread {
		t.Fatalf("the question was posted to thread %q, want the correlated %q", got, h.thread)
	}
	// The button is bound to the BYTES: its value is the one-shot request hash, which is what makes the
	// click decidable at all (Decide compares it against the session's pending approval).
	if !containsString(texts, h.requestHash()) {
		t.Fatalf("no button carries the pending approval's one-shot hash: %v", texts)
	}
	// EXACTLY ONCE: a second tick asks nothing, because UNIQUE (approval_id) + a claim that IS the update
	// leave nothing due. One question, one message, however many posters run.
	h.post()
	if got := len(h.approvalMessages()); got != 1 {
		t.Fatalf("a second pump tick asked the same question again (%d messages in the thread)", got)
	}
	// And no credential is on any outbound body; the bot token rides the Authorization header alone.
	for _, c := range *h.calls {
		if strings.Contains(c.body, "xoxb-") {
			t.Fatalf("an outbound Slack body carried a bot token: %s", c.path)
		}
		if c.auth != "" && !strings.HasPrefix(c.auth, "Bearer ") {
			t.Fatalf("outbound Authorization on %s is %q", c.path, c.auth)
		}
	}
}

// TestPublicationFromSlackParksTheRunSoTheApproveLands is E23 T3's RED #2, and it is the UPDATE of E22's
// TestPublicationFromSlackCeilingAnApproveAfterTheRunEndsDecidesNothing rather than a new test beside it —
// that test carried an instruction in its body ("an approve after the run terminated now SUCCEEDS … update
// it, and the honest ceiling in CAS-002, rather than deleting it") and this is the instruction being
// followed. The record it kept is preserved here in full:
//
//	WHAT E22 MEASURED. A publication tool returned pending_approval and the model KEPT GOING, because
//	nothing in this tree parked a run on its own pending approval — the only routes to `waiting` were an
//	explicit pause and detach. So the ordinary Slack shape was that the run finished its answer before any
//	human looked at Slack; ApplyApprovalDecision runs under guardRunActive; and the click that arrived
//	afterwards did not merely refuse — Decide returned an ERROR, the interactivity route answered 503, the
//	human saw Slack report a failure, and the publication stayed pending forever.
//
// WHAT IS TRUE NOW, and the assertions below are that sentence: the run PARKS instead of answering, so the
// click lands on an ACTIVE run and is a 200. Nothing about guardRunActive changed — a genuinely terminal
// run still refuses, and the last leg here still proves it. What changed is that the ordinary flow no
// longer produces one.
func TestPublicationFromSlackParksTheRunSoTheApproveLands(t *testing.T) {
	ctx := context.Background()
	h := newPublishHarness(t)

	ch, err := h.dispatch(tools.PushTool(), redeliveryID("tc"), 1, nil)
	if !errors.Is(err, errRunParked) {
		t.Fatalf("dispatchTool error = %v, want the park — a run that answers here is a run whose approval "+
			"can no longer be applied", err)
	}
	// THE MODEL WAS GIVEN NOTHING TO CONTINUE ON. That is the difference from the shipped behaviour, where
	// the tool's "pending_approval" receipt was delivered and read as an answer.
	if got := toolResults(ch); len(got) != 0 {
		t.Fatalf("the engine was sent %d tool.result frame(s) while a human owes an answer: %+v", len(got), got)
	}
	if got := h.runState(); got != string(statemachines.RunWaiting) {
		t.Fatalf("run state = %q while a human owes an answer, want waiting", got)
	}
	// The approval carries a DEADLINE. 000013 declared expires_at in 2023 and nothing ever wrote a value
	// into it; harmless while nothing waited, and a run parked forever the moment something does.
	var expiresAt *time.Time
	if err := h.spine.Pool().QueryRow(storage.WithSystemScope(ctx),
		`SELECT a.expires_at FROM approvals a JOIN publications p ON p.id = a.publication_id WHERE p.run_id = $1`,
		h.runID).Scan(&expiresAt); err != nil {
		t.Fatalf("read the approval deadline: %v", err)
	}
	if expiresAt == nil {
		t.Fatal("the publication approval has no expires_at: nothing would ever release a run parked on it")
	}

	// A click from somebody who is not an approver still moves nothing — the park does not widen who decides.
	hash := h.requestHash()
	if got := h.click("Uintruder", "approve", hash); got.Rejected == "" {
		t.Fatalf("an unmapped user's click was accepted on a parked run: %+v", got)
	}
	if got := h.runState(); got != string(statemachines.RunWaiting) {
		t.Fatalf("an unauthorized click moved the parked run to %q", got)
	}

	// THE CLICK THAT USED TO 503. Decide returns no error, so the interactivity route answers 200.
	decision, derr := h.bridge.Decide(ctx, h.conn, slack.ApprovalIntent{
		TeamID: h.team, UserID: "Uapprover", RequestHash: hash, Decision: "approve",
		ActionID: slack.ActionApprove, ChannelID: h.channel, ThreadTS: h.thread, MessageTS: h.botMessageTS,
	})
	if derr != nil {
		t.Fatalf("the approver's click on a PARKED run failed with %v — that is the 503 E22 measured, and "+
			"closing it is what this task exists for", derr)
	}
	if decision.Rejected != "" {
		t.Fatalf("the authorized approver's click was refused: %q", decision.Rejected)
	}
	if _, state, _, _, _, _ := h.publicationRow("push_branch"); state != "approved" {
		t.Fatalf("the publication is %q after an approve, want approved", state)
	}

	// THE WAKE: waiting -> running, with the response.run job enqueued in the SAME transaction as the
	// decision, so no worker can pick it up before the approval is durable.
	if got := h.runState(); got != "running" {
		t.Fatalf("run state after the approve = %q, want running (the decision did not re-enter the run)", got)
	}
	var jobs int
	if err := h.spine.Pool().QueryRow(storage.WithSystemScope(ctx),
		`SELECT count(*) FROM durable_jobs WHERE kind='response.run' AND payload->>'run_id' = $1`, h.runID).Scan(&jobs); err != nil {
		t.Fatalf("count wake jobs: %v", err)
	}
	if jobs != 1 {
		t.Fatalf("the wake enqueued %d response.run job(s), want exactly 1", jobs)
	}
	// And the boundary the woken attempt reaches publishes it — once, to the approved destination.
	if n := h.pump(); n != 1 {
		t.Fatalf("the publisher was called %d time(s) after the approve, want exactly 1: %v", n, h.publisher.operations())
	}
}

// TestPublicationFromSlackCeilingATerminalRunStillRefusesTheClick is the OTHER half of E22's record, and it
// is unchanged: guardRunActive still refuses a decision on a terminal run, and the route still answers 503.
//
// What E23 T3 changed is not that refusal — it is that the ORDINARY flow no longer walks into it. Before,
// every publication reached it, because the run answered and finished while the human was still reading.
// Now the run parks, so reaching this refusal takes a run that terminalized some OTHER way while a question
// was open: a cancel, a timeout, an exhausted budget. That is a real and much rarer window, it is left open
// deliberately (a decision applied to a dead run would publish from a workspace nobody holds), and it is
// asserted here so that closing it later is a deliberate edit rather than a surprise.
func TestPublicationFromSlackCeilingATerminalRunStillRefusesTheClick(t *testing.T) {
	ctx := context.Background()
	h := newPublishHarness(t)
	if _, err := h.dispatch(tools.PushTool(), redeliveryID("tc"), 1, nil); !errors.Is(err, errRunParked) {
		t.Fatalf("dispatchTool error = %v, want the park", err)
	}
	hash := h.requestHash()
	if _, err := h.spine.ApplyRunTransition(ctx, h.tenant, h.runID, statemachines.RunCmdCancel); err != nil {
		t.Fatalf("cancel the parked run: %v", err)
	}
	_, err := h.bridge.Decide(ctx, h.conn, slack.ApprovalIntent{
		TeamID: h.team, UserID: "Uapprover", RequestHash: hash, Decision: "approve",
		ActionID: slack.ActionApprove, ChannelID: h.channel, ThreadTS: h.thread, MessageTS: h.botMessageTS,
	})
	if err == nil || !strings.Contains(err.Error(), "run_terminal") {
		t.Fatalf("a click on a TERMINAL run failed with %v, want the guardRunActive refusal — if that changed "+
			"shape, the route's answer to the human changed with it", err)
	}
	// The refusal is SAFE, which is why it is tolerable: nothing was decided and nothing was published.
	if _, state, _, _, _, _ := h.publicationRow("push_branch"); state != "pending_approval" {
		t.Fatalf("the publication is %q after a refused click on a dead run, want still pending_approval", state)
	}
	if n := h.pump(); n != 0 {
		t.Fatalf("the publisher was called %d time(s) for a publication no live run could approve", n)
	}
}

// TestPublicationFromSlackDenyWakesTheParkedRunAndPublishesNothing is the half a decision surface is worth
// nothing without, asserted on the PARKED path: a deny has to both prevent the side effect and release the
// run. A gate that refuses the operation and leaves the run waiting forever has replaced one failure with a
// worse one.
func TestPublicationFromSlackDenyWakesTheParkedRunAndPublishesNothing(t *testing.T) {
	h := newPublishHarness(t)
	if _, err := h.dispatch(tools.PushTool(), redeliveryID("tc"), 1, nil); !errors.Is(err, errRunParked) {
		t.Fatalf("dispatchTool error = %v, want the park", err)
	}
	if got := h.click("Uapprover", "deny", h.requestHash()); got.Rejected != "" {
		t.Fatalf("the deny click was refused: %q", got.Rejected)
	}
	if _, state, _, _, _, _ := h.publicationRow("push_branch"); state != "denied" {
		t.Fatalf("the publication is %q after a deny, want denied", state)
	}
	if got := h.runState(); got == string(statemachines.RunWaiting) {
		t.Fatal("the run is STILL waiting after its publication was denied: a deny that parks a run forever " +
			"is a worse outcome than the push")
	}
	if n := h.pump(); n != 0 {
		t.Fatalf("the publisher was called %d time(s) for a DENIED publication: %v", n, h.publisher.operations())
	}
}

// TestPublicationFromSlackDeliveryFailureKeepsTheApprovalAndReleasesTheRun is SLK-006 applied to the new
// surface, and it is the failure this task could most easily have introduced: a run now PARKS on a question,
// so a workspace that will not take the message could wedge the run forever.
//
// It cannot. The canonical result is untouched by a delivery failure — the approval is recorded, its
// deadline stands, the publication is still pending and still decidable — and the reaper releases the run
// whether or not anybody was ever shown a button.
func TestPublicationFromSlackDeliveryFailureKeepsTheApprovalAndReleasesTheRun(t *testing.T) {
	ctx := context.Background()
	h := newPublishHarness(t)
	h.slackRefuses = true

	if _, err := h.dispatch(tools.PushTool(), redeliveryID("tc"), 1, nil); !errors.Is(err, errRunParked) {
		t.Fatalf("dispatchTool error = %v, want the park", err)
	}
	h.post()
	// The question was attempted and Slack said no. Nothing about the run's state moved because of it.
	if got := len(h.approvalMessages()); got != 1 {
		t.Fatalf("the pump made %d attempt(s) at the question, want 1", got)
	}
	if _, state, _, _, _, _ := h.publicationRow("push_branch"); state != "pending_approval" {
		t.Fatalf("a failed delivery moved the publication to %q", state)
	}
	if got := h.runState(); got != string(statemachines.RunWaiting) {
		t.Fatalf("a failed delivery moved the parked run to %q", got)
	}

	// THE RELEASE. Move the clock rather than wait out the TTL: the deadline is a column, so the component
	// test edits the column. (A real thirty-minute expiry is an operator leg, §6.)
	execSQL(t, h.spine.Pool(),
		`UPDATE approvals a SET expires_at = clock_timestamp() - interval '1 minute'
		   FROM publications p WHERE p.id = a.publication_id AND p.run_id = $1`, h.runID)
	swept, err := h.spine.SweepExpiredApprovals(ctx)
	if err != nil {
		t.Fatalf("SweepExpiredApprovals error = %v", err)
	}
	if swept < 1 {
		t.Fatalf("the reaper swept %d elapsed approvals, want at least this one", swept)
	}
	if _, state, _, _, _, _ := h.publicationRow("push_branch"); state != "expired" {
		t.Fatalf("the publication is %q after its deadline passed, want expired", state)
	}
	if got := h.runState(); got == string(statemachines.RunWaiting) {
		t.Fatal("the run is STILL waiting after its approval expired: the gate closed something forever, " +
			"which is the one failure mode worse than having no gate")
	}
	if n := h.pump(); n != 0 {
		t.Fatalf("the publisher was called %d time(s) for an EXPIRED publication", n)
	}
}

// collectStrings walks a decoded JSON document and gathers every string leaf, so an assertion about what a
// message says is made against the DECODED text rather than the escaped wire bytes.
func collectStrings(v any, out *[]string) {
	switch t := v.(type) {
	case string:
		*out = append(*out, t)
	case []any:
		for _, e := range t {
			collectStrings(e, out)
		}
	case map[string]any:
		for _, e := range t {
			collectStrings(e, out)
		}
	}
}

func containsString(all []string, want string) bool {
	for _, s := range all {
		if s == want {
			return true
		}
	}
	return false
}
