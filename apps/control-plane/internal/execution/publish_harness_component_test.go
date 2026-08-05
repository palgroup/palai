//go:build component

package execution

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	"github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
	"github.com/palgroup/palai/storage"
)

// The publication-approval harness: a real spine carrying an active session with a repository binding, a
// prepared workspace holding a real commit, and a recording publisher — everything a test needs to drive a
// publication from the tool call to the publisher against REAL PostgreSQL.
//
// IT WAS EXTRACTED FROM slack_publish_component_test.go ON 2026-08-05 AND DE-SLACKED IN THE SAME MOVE. That
// file is gone with the rest of the in-process Slack bridge, and the harness had to survive it: three tests
// that have nothing to do with Slack were built on it — merge_component_test.go,
// publication_precheck_component_test.go and (through seedRegisteredGatedRun)
// http_tool_approval_component_test.go. Deleting the file would have taken the generic half of this epic's
// evidence with the transport half.
//
// WHAT CHANGED IS THE DECISION MECHANISM AND NOTHING ELSE. The harness used to register a Slack connection,
// correlate a thread to the session, stand up a local slack.com, and decide through SlackAdmitter.Decide —
// so `h.click("Uapprover", "approve", hash)` carried the approver allow-list, AcceptCommand and
// ApplyApprovalDecision in one call. `h.approve` now mints the SAME durable command through the SAME
// AcceptCommand and applies the SAME ApplyApprovalDecision, with a bearer principal instead of a Slack user
// id. Every publication claim downstream of the decision — the destination came from the binding, the
// publisher was untouched until a human decided, a deny publishes nothing, the run parks and wakes — is
// unchanged, because none of them was ever about Slack.
//
// WHAT IS NOT HERE, named rather than left to be discovered: the two legs that WERE about Slack. A question
// posted into a correlated thread, and SLK-006's "a workspace that refuses the message costs the button and
// never the approval, its deadline or the run". Both measured the delivery of a question over a transport
// this deployment no longer serves; apps/slack-bot asks the question now, off approval.requested.v1.

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

// publishHarness is a real spine carrying an active session: a repository binding whose default_branch is
// `dev`, a prepared workspace with a real git repo and a real commit, and a recording publisher.
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
	publisher *recordingPublisher
}

// publishBaseBranch is the value the publication chain turns on: `dev` is a BINDING value an operator set in
// .env.local (PALAI_GIT_BASE_BRANCH), never a constant in this tree. It is spelled once here so a test that
// stopped reading it off the binding would stop compiling rather than quietly assert a literal.
const publishBaseBranch = "dev"

// publishApprover is the principal the harness decides as. A bearer principal rather than a Slack user id:
// nothing links a Slack account to an API key (coordinator.publication.go says so at length), and with no
// config_policy.approvers list configured any tenant key may decide — which is the posture
// approver_component_test.go pins separately and this harness deliberately does not re-litigate.
const publishApprover = "key_publish_harness"

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
		tenant:    coordinator.Tenant{Project: redeliveryID("prj")},
		sessionID: redeliveryID("ses"), runID: redeliveryID("run"), respID: redeliveryID("resp"),
		bindingID: redeliveryID("bnd"), remote: "https://github.test/acme/widgets.git",
	}
	h.branch = "agent/" + h.sessionID + "/" + h.runID

	pool := cs.Pool()
	execSQL(t, pool, `INSERT INTO projects (id) VALUES ($1)`, h.tenant.Project)
	execSQL(t, pool, `INSERT INTO sessions (id, project_id, state) VALUES ($1,$2,'active')`,
		h.sessionID, h.tenant.Project)
	execSQL(t, pool, `INSERT INTO responses (id, project_id, session_id, state) VALUES ($1,$2,$3,'in_progress')`,
		h.respID, h.tenant.Project, h.sessionID)
	execSQL(t, pool, `INSERT INTO runs (id, project_id, session_id, response_id, state) VALUES ($1,$2,$3,$4,'running')`,
		h.runID, h.tenant.Project, h.sessionID, h.respID)

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

// execEnv is the per-call environment the orchestrator hands a tool: this run's scope, this attempt's
// workspace, and the REAL publication registry.
//
// IT CARRIES ITS OWN WorkspaceOps, and the pair is what a bound workspace IS since A.3 T5: a root AND
// something that can reach it. The production seam picks the second from the attempt's connection
// (execEnv -> workspaceOpsFor), and this harness holds no connection, so it states the local answer
// explicitly — the same substitution tools/*_test.go make, and the rule this tree wrote down after four
// proofs turned out to be properties of their harness. A root alone made all three publication tests
// here refuse for the WORKSPACE before they reached the claim they exist to make.
func (h *publishHarness) execEnv() toolbroker.ExecEnv {
	return toolbroker.ExecEnv{
		WorkspaceRoot: h.root,
		Workspace:     tools.LocalWorkspace(h.root),
		Publications:  newPublicationRegistry(h.spine, nil),
		Scope: toolbroker.TaskScope{
			Project:   h.tenant.Project,
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

// runState reads the durable run state — `waiting` is the claim the park exists to make true.
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

// approve drives the production decision path for one decision: the durable command first, then the
// transition — the same two steps in the same order every decision surface takes (internal/store's
// approvals.go states the rule, and it is why the check lives in approverAuthorizedTx rather than being
// written twice). `decision` is "approve" or "deny"; it returns how many rows the transition applied, so a
// caller can assert a refusal as 0 rather than reading it off an error string.
func (h *publishHarness) approve(decision, requestHash string) int64 {
	return h.approveAs(publishApprover, decision, requestHash)
}

// setApprovers writes the project's approver allow-list. Written straight to config_policy because the
// admission route that normally writes it is a different surface's claim; what matters here is that
// approverAuthorizedTx reads the list LIVE, so a narrowing takes a pending approval with it.
func (h *publishHarness) setApprovers(policy string) {
	h.t.Helper()
	execSQL(h.t, h.spine.Pool(), `UPDATE projects SET config_policy=$2 WHERE id=$1`, h.tenant.Project, policy)
}

// approveAs is approve for a NAMED principal, so a test can drive a decision by somebody the project's
// approver list does not name. It returns 0 applied rows for a refusal rather than an error: a principal
// who may not decide decides nothing, which is a count and not an exception.
//
// AND THE COUNT IS NOT THE PROPERTY, WHICH IS WHY EVERY CALL ALSO CHECKS THE TWO DURABLE FACTS IT STANDS
// FOR. On 2026-08-05 nine publication/merge tests turned red at once and the reason is the whole argument
// for the lines below: they asserted a real property (`0` = nothing was applied, `1` = exactly one row
// moved) against a production function that was returning the session's next EVENT-JOURNAL SEQUENCE, and
// the two agreed by COINCIDENCE until the Slack cutover added an event to that stream. The return value was
// corrected — but the assertion stayed anchored to a value only this file reads, so the next refactor that
// shifts the number reproduces the same nine reds and they look like fixture rot again.
//
// So the count stays, because it is the cheap first line, and the facts it is a proxy FOR are asserted
// beside it: the `publications` row's own state, and the journalled approval.approved/denied.v1. Those are
// what "an approve applied" MEANS — a row a human can no longer decide, and an audit record naming the
// decision — and neither can drift into agreement with a counter by accident.
func (h *publishHarness) approveAs(principal, decision, requestHash string) int64 {
	h.t.Helper()
	ctx := context.Background()
	commandID := redeliveryID("cmd")
	if _, err := h.spine.AcceptCommand(ctx, h.tenant, h.sessionID, coordinator.CommandInput{
		CommandID: commandID, Kind: decision, Payload: []byte(`{"request_hash":"` + requestHash + `"}`),
	}); err != nil {
		h.t.Fatalf("AcceptCommand(%s): %v", decision, err)
	}

	// READ BEFORE THE DECISION, because both facts are DIFFERENCES. The pending row has to be identified
	// before it stops being pending, and an event count means nothing without the count it rose from — a
	// suite that seeds several decisions into one session would otherwise read another decision's event and
	// call it this one's.
	event, wantState := "approval.approved.v1", "approved"
	if decision == "deny" {
		event, wantState = "approval.denied.v1", "denied"
	}
	pending, hadPending, err := h.spine.PendingApprovalForSession(ctx, h.tenant, h.sessionID)
	if err != nil {
		h.t.Fatalf("read the session's pending approval before the decision: %v", err)
	}
	eventsBefore := h.decisionEvents(event)

	applied, err := h.spine.ApplyApprovalDecision(ctx, h.tenant, h.sessionID, h.respID, h.runID,
		commandID, decision, requestHash, principal)
	// A REFUSAL IS THE VALUE THIS HELPER PROMISES, NOT AN ERROR TO REPORT — and until 2026-08-05 the three
	// sentences above claimed that while the line below did the opposite. ErrApproverNotAuthorized is a
	// typed outcome whose own declaration says callers "treat it as 'the receiver worked and the click/POST
	// authorized nothing', never as a failure"; the command is settled before it is returned. The Slack
	// helper this one replaced never had to say so, because SlackAdmitter.Decide converted it into
	// SlackDecisionOutcome.Rejected and returned a nil error — so de-Slacking the harness moved the
	// conversion's only home without carrying it, and the sole caller that drives an unnamed principal
	// (TestMergePullRequestWithoutAnApprovalPublishesNothing) died at this Fatalf reading
	// "ApplyApprovalDecision(approve): approver_not_authorized" — an error, at the exact line whose claim is
	// that a refusal is not one — instead of reaching the `applied != 0` assertion it exists to make.
	// Every OTHER error still fails: this widens the helper by exactly one documented outcome.
	if err != nil && !errors.Is(err, coordinator.ErrApproverNotAuthorized) {
		h.t.Fatalf("ApplyApprovalDecision(%s): %v", decision, err)
	}
	h.assertDecisionLanded(decision, applied, hadPending, pending.ID, wantState, event, eventsBefore)
	return applied
}

// assertDecisionLanded is the half of the count that is not a number. It runs on EVERY decision this harness
// drives rather than in the tests that happened to think of it — which is the actual finding behind it:
// publish_approval_component_test.go asserts the publication's state after both its refusals, and
// merge_component_test.go's unauthorized MERGE asserts only the count, on the least reversible operation
// this system performs.
//
// The `default` arm is the one that would have caught 2026-08-05 outright. A journal position is a number
// like any other and agrees with `1` roughly whenever a session is one event old; it cannot agree with the
// statement "a transition moves at most one row", so that is the statement made here.
func (h *publishHarness) assertDecisionLanded(decision string, applied int64, hadPending bool, pubID, wantState, event string, eventsBefore int) {
	h.t.Helper()
	eventsAfter := h.decisionEvents(event)
	switch applied {
	case 0:
		if hadPending {
			if got := h.publicationState(pubID); got != "pending_approval" {
				h.t.Fatalf("%s applied 0 row(s) and left publication %s in %q — a decision that authorized "+
					"nothing must leave a row a human can still decide", decision, pubID, got)
			}
		}
		if eventsAfter != eventsBefore {
			h.t.Fatalf("%s applied 0 row(s) and journalled %d new %s — the count says nobody decided anything "+
				"and the audit stream says somebody did, and the audit stream is the record",
				decision, eventsAfter-eventsBefore, event)
		}
	case 1:
		if !hadPending {
			h.t.Fatalf("%s applied 1 row with no pending approval for it to have moved", decision)
		}
		if got := h.publicationState(pubID); got != wantState {
			h.t.Fatalf("%s applied 1 row and publication %s is %q, want %q — the number is the count of rows "+
				"ONE transition moved, so the row it counts has to be the row that moved",
				decision, pubID, got, wantState)
		}
		if eventsAfter != eventsBefore+1 {
			h.t.Fatalf("%s applied 1 row and journalled %d new %s, want exactly 1 — the transition and its "+
				"audit record commit in one transaction or the ledger is not the record",
				decision, eventsAfter-eventsBefore, event)
		}
	default:
		h.t.Fatalf("%s applied %d row(s) — ApplyApprovalDecision counts the rows one publication's transition "+
			"moved, so any value outside {0,1} is a journal position wearing a row count's name", decision, applied)
	}
}

// publicationState reads ONE publication by id. publicationRow reads by operation, which is the wrong handle
// here: the row this helper judges is the one that was pending when the decision was made, and naming it by
// id is what keeps that true for a session holding more than one publication.
func (h *publishHarness) publicationState(pubID string) string {
	h.t.Helper()
	var state string
	if err := h.spine.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT state FROM publications WHERE id = $1`, pubID).Scan(&state); err != nil {
		h.t.Fatalf("read publication %s: %v", pubID, err)
	}
	return state
}

// decisionEvents counts one event type in this session's journal.
func (h *publishHarness) decisionEvents(eventType string) int {
	h.t.Helper()
	var n int
	if err := h.spine.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(*) FROM events WHERE session_id = $1 AND type = $2`, h.sessionID, eventType).Scan(&n); err != nil {
		h.t.Fatalf("count %s events: %v", eventType, err)
	}
	return n
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

// requestHash is the one-shot binding a decision must carry to apply.
func (h *publishHarness) requestHash() string {
	h.t.Helper()
	pub, found, err := h.spine.PendingApprovalForSession(context.Background(), h.tenant, h.sessionID)
	if err != nil || !found {
		h.t.Fatalf("read the session's pending approval: (%v,%v)", found, err)
	}
	return pub.RequestHash
}
