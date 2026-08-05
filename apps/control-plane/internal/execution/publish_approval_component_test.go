//go:build component

package execution

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	statemachines "github.com/palgroup/palai/packages/state-machines"
	"github.com/palgroup/palai/storage"
)

// A publication from the tool call to the publisher, against REAL PostgreSQL.
//
// WHAT THIS FILE EXISTS TO REFUSE, and it is the only failure in this chain that is silent: a publication
// that reaches the publisher WITHOUT a human. Everything else in the chain announces itself — a refused
// decision answers, a failed push warns, an unwired publisher logs. An unapproved push would simply be a
// branch on someone's remote.
//
// So the assertion is made in the order the danger runs: the tool is called and the pump is driven while
// the publication is still PENDING, and the publisher must not have been touched. Only then does a decision
// arrive. A test that approved first and then checked the publisher would have proven the approve works and
// nothing at all about what happens without one.
//
// THESE ARE E22 T4's CLAIMS, CARRIED OFF SLACK ON 2026-08-05. They were written as
// TestPublicationFromSlack* against SlackAdmitter.Decide, because in E22 a button in a thread was the only
// decision surface this tree had. It is not any more, and it is not the one that survived: the in-process
// Slack bridge is deleted and apps/slack-bot decides through the same public surface any other client uses.
// The claims did not move an inch — none of them was ever about Slack — so what changed is `h.click(user,
// decision, hash)` becoming `h.approve(decision, hash)`, which mints the same durable command and applies
// the same ApplyApprovalDecision with a bearer principal instead of a Slack user id.
//
// TWO LEGS DID NOT COME ACROSS, because they were about the transport rather than the gate, and both are
// named here rather than quietly dropped:
//
//   - the question POSTED into a correlated thread (E23 T3's RED #1), and
//   - SLK-006 — a workspace that REFUSES the message costs a human the button and never the approval, its
//     deadline, or the run.
//
// The second is the one worth reading twice, because the property it protects is still true and is now
// proven elsewhere: an approval nobody was shown still expires, and the expiry reaper still wakes the run
// parked on it. tool_approval_component_test.go's TestExpiredToolApprovalCancelsTheCallAndWakesTheParkedRun
// holds that half. What is no longer proven is the DELIVERY failure specifically, because there is no
// in-process delivery left to fail.
//
// EVERY OTHER LINK IS THE PRODUCTION ONE. The push and pull-request tools are the shipped tools.PushTool() /
// PullRequestTool(); the destination is resolved by the real publicationRegistry through
// RunPublicationTarget (so remote/branch/base come from the BINDING and the model cannot name them); the
// decision runs AcceptCommand + ApplyApprovalDecision; and the publish half is publishApproved, the same
// body the orchestrator's boundary pump calls. One thing is a double and it is named where it is built: the
// Publisher is a recorder — what a REAL push does to a REAL remote is the e2e coding journey and the live
// leg in tests/live/repository.

// TestPublicationWaitsForAnApproveAndThenPublishes is E22 T4's whole claim in one run, asserted in the order
// the danger runs.
func TestPublicationWaitsForAnApproveAndThenPublishes(t *testing.T) {
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

	// 3. A decision carrying the WRONG one-shot hash moves nothing either. This is the binding the whole
	// gate rests on: an approval id alone must not authorize anything, or a decision could be replayed onto
	// a publication whose arguments changed after a human read them.
	hash := h.requestHash()
	if applied := h.approve("approve", "sha256:not-the-hash-a-human-was-shown"); applied != 0 {
		t.Fatalf("a decision carrying a stale request hash applied %d row(s), want 0", applied)
	}
	if _, state, _, _, _, _ := h.publicationRow("push_branch"); state != "pending_approval" {
		t.Fatalf("a stale-hash decision left the publication %q", state)
	}
	if n := h.pump(); n != 0 {
		t.Fatalf("the publisher was called after a STALE-HASH decision (%v)", h.publisher.operations())
	}

	// 4. The approver decides, and the whole production chain runs.
	if applied := h.approve("approve", hash); applied != 1 {
		t.Fatalf("the approve applied %d row(s), want 1", applied)
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

// TestPublicationDenialPreventsThePushEntirely is the half a decision surface is worth nothing without.
// Recording a "denied" verdict is not the guarantee; the guarantee is that the side effect does not happen —
// so this drives the pump AFTER the deny, and again after the run is CANCELLED, and the publisher must never
// have been asked for anything at all.
func TestPublicationDenialPreventsThePushEntirely(t *testing.T) {
	ctx := context.Background()
	h := newPublishHarness(t)
	h.propose(tools.PushTool(), nil)

	if applied := h.approve("deny", h.requestHash()); applied != 1 {
		t.Fatalf("the deny applied %d row(s), want 1", applied)
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

// TestPublicationTargetsTheBindingsBaseBranch is plan §3.5 X17, and the reason "open the PR against dev"
// needed no code change: `dev` is this binding's default_branch, resolved server-side by
// RunPublicationTarget. The model is never asked and — per the schema guard in tools/publish_test.go —
// cannot answer.
//
// IT IS THE ONLY END-TO-END PROOF OF THE RESOLUTION HALF, which is why it was worth carrying across the
// cutover rather than leaving to the schema guard. That guard proves the model cannot NAME a destination;
// this proves the destination the operator DID name is the one that reaches the publisher. The two fail in
// different directions and a tree with only the first would ship a base nobody chose.
//
// It also pins what the human SEES. publicationDisplay builds the approval detail from the resolved
// destination; the model's proposed title and body are RECORDED on the publication's args for a later
// policy-filtered pass and reach the display nowhere. The model's prose here is a lie about the
// destination, and it must appear nowhere on the surface a human reads before deciding.
func TestPublicationTargetsTheBindingsBaseBranch(t *testing.T) {
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

	// And approving it publishes a DRAFT pull request to that same base — the destination the human read.
	if applied := h.approve("approve", h.requestHash()); applied != 1 {
		t.Fatalf("the approve applied %d row(s), want 1", applied)
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
}

// TestPublicationParksTheRunSoTheApproveLands is E23 T3's second half, and it is a REPAIR rather than a
// feature. E22 shipped a publication tool that answered `pending_approval` and let the model carry on, so
// the run reached `completed` before anybody looked — and the decision that arrived afterwards hit a run
// that had already ended.
//
// The run goes running -> waiting through the pause/detach choreography, and the attempt ends with NO
// tool.result, so the model is given nothing to continue on and a parked run costs no compute while a human
// reads. THE PARK IS DECIDED BY THE DATABASE, NEVER BY THE RESULT BYTES: a pending approvals row THIS RUN
// owns is what parks it, not a tool answering `{"status":"pending_approval"}` — an MCP server can write that
// string and tool output is untrusted data.
func TestPublicationParksTheRunSoTheApproveLands(t *testing.T) {
	h := newPublishHarness(t)

	// A PARKED ATTEMPT ENDS ON errRunParked, NOT ON nil: the dispatcher's contract for "the model is given
	// nothing to continue on" is an error the attempt loop recognises and swallows (orchestrator.go's two
	// errors.Is(err, errRunParked) arms), never a clean return — ExecuteAttempt only reads it as "not a
	// failure", it does not disappear. A caller here that expected nil would never observe a park at all.
	ch, err := h.dispatch(tools.PushTool(), "call_park", 1, nil)
	if !errors.Is(err, errRunParked) {
		t.Fatalf("dispatchTool error = %v, want the park — a run that answers here is a run whose approval "+
			"can no longer be applied", err)
	}
	for _, f := range ch.sent {
		if f.Type == "tool.result" {
			t.Fatalf("the dispatcher answered the parked call with a tool.result (%+v): the model must be given "+
				"nothing to continue on, or it carries on past the gate", f)
		}
	}
	if got := h.runState(); got != "waiting" {
		t.Fatalf("the run is %q after a gated publication, want waiting — a run that stays running answers "+
			"before a human looks, which is the defect this repairs", got)
	}

	// The decision lands on the PARKED run and wakes it.
	if applied := h.approve("approve", h.requestHash()); applied != 1 {
		t.Fatalf("the approve applied %d row(s), want 1", applied)
	}
	if got := h.runState(); got == "waiting" {
		t.Fatal("the run is still waiting after its approval was decided: the wake is what keeps a decided " +
			"question from holding a run open forever")
	}
	if n := h.pump(); n != 1 {
		t.Fatalf("the publisher was called %d time(s) after the approve on the woken run, want 1: %v",
			n, h.publisher.operations())
	}
}

// TestPublicationDenyWakesTheParkedRunAndPublishesNothing is the deny half of the wake, and it exists
// because a gate that refuses the operation and leaves the run waiting forever has replaced one failure
// with a worse one.
func TestPublicationDenyWakesTheParkedRunAndPublishesNothing(t *testing.T) {
	h := newPublishHarness(t)

	// Same contract as the approve half above: a parked attempt ends on errRunParked, not on nil.
	if _, err := h.dispatch(tools.PushTool(), "call_deny", 1, nil); !errors.Is(err, errRunParked) {
		t.Fatalf("dispatchTool error = %v, want the park", err)
	}
	if got := h.runState(); got != "waiting" {
		t.Fatalf("the run is %q before the deny, want waiting", got)
	}
	if applied := h.approve("deny", h.requestHash()); applied != 1 {
		t.Fatalf("the deny applied %d row(s), want 1", applied)
	}
	if got := h.runState(); got == "waiting" {
		t.Fatal("the run is still waiting after a DENY: a refused operation must release the run it parked")
	}
	if n := h.pump(); n != 0 {
		t.Fatalf("the publisher was called %d time(s) after a deny: %v", n, h.publisher.operations())
	}
}
