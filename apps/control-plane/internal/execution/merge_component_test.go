//go:build component

package execution

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	"github.com/palgroup/palai/packages/coordinator"
	statemachines "github.com/palgroup/palai/packages/state-machines"
	"github.com/palgroup/palai/storage"
)

// E23 T6 — MERGE, against REAL PostgreSQL, and the point of this file is how little is in it.
//
// The owner's sentence was "kod yazdırır, PR açtırır, merge ettirir". E22 delivered the first two and named
// the third as its ceiling. What closed it was a CHECK value (000044 R2), a switch case in the publisher,
// a tool with an empty input schema, and a display branch. Nothing here builds an approval path, a park, a
// button, a one-shot hash, a deadline or a reaper — all of that was built by T1/T3 for the general case and
// a merge simply arrives at it. THAT is this task's claim, and a file this size is the evidence for it.
//
// Everything measured below rides the production links: the shipped tools.MergeTool(), the real
// publicationRegistry (which resolves WHICH pull request from the run's own published receipt), the real
// dispatcher (which parks), the real Slack Decide (which authorizes), and publishApproved (the same body
// the boundary pump calls). Two doubles, both named where they are built: the Publisher, and slack.com.

// mergingPublisher is a RepositoryPublisher standing in front of a GitHub double: it records every target
// and answers a merge receipt, but it refuses exactly the way the API documents when the approved sha is no
// longer the branch head. That refusal is not this fake's invention — the transport-level proof against a
// server built to the published contract is adapters/repositories (TestGitHubMergeAlwaysSendsTheApprovedSHA).
// What is under test HERE is which sha the pump sends and what it does with a refusal.
type mergingPublisher struct {
	targets []PublishTarget
	// remoteHead is what the pull request branch currently points at. Moving it after the approval is the
	// race, and it is the only knob this fake has.
	remoteHead string
	prNumber   int
}

func (p *mergingPublisher) Publish(_ context.Context, target PublishTarget) (map[string]any, error) {
	p.targets = append(p.targets, target)
	pub := target.Publication
	switch pub.Operation {
	case "open_pull_request":
		return map[string]any{"pull_request_id": "PR_1", "number": p.prNumber, "draft": true,
			"url": "https://github.test/acme/widgets/pull/1"}, nil
	case "merge_pull_request":
		number, _ := pub.Args["pull_request_number"].(float64)
		method, _ := pub.Args["merge_method"].(string)
		receipt, err := repositories.MergePullRequest(context.Background(), p, repositories.MergeInput{
			Number: int(number), HeadSHA: pub.HeadSHA, Method: method,
		})
		if err != nil {
			return nil, err
		}
		return map[string]any{"merged": receipt.Merged, "sha": receipt.SHA, "number": int(number),
			"merge_method": method}, nil
	default:
		return map[string]any{"remote": pub.Remote, "branch": pub.Branch, "remote_sha": pub.HeadSHA}, nil
	}
}

// The PullRequestClient half: only Merge is ever reached, and it answers the documented 409 for the
// documented reason.
func (p *mergingPublisher) Find(context.Context, string, string) (repositories.PullRequest, bool, error) {
	return repositories.PullRequest{}, false, nil
}

func (p *mergingPublisher) Open(context.Context, repositories.OpenPRInput) (repositories.PullRequest, error) {
	return repositories.PullRequest{}, fmt.Errorf("mergingPublisher: this test opens no pull request through the client")
}

func (p *mergingPublisher) Merge(_ context.Context, in repositories.MergeInput) (repositories.MergeReceipt, error) {
	if in.HeadSHA != p.remoteHead {
		return repositories.MergeReceipt{}, fmt.Errorf("%w: head is now %s", repositories.ErrHeadMoved, p.remoteHead)
	}
	return repositories.MergeReceipt{Merged: true, SHA: "merged_" + in.HeadSHA, Message: "Pull Request successfully merged"}, nil
}

func (p *mergingPublisher) operations() []string {
	var ops []string
	for _, t := range p.targets {
		ops = append(ops, t.Publication.Operation)
	}
	return ops
}

// mergeHarness is the publish harness with the merging publisher swapped in, plus the one thing a merge
// needs that a push does not: a pull request this run already PUBLISHED, because that receipt is where the
// number comes from.
type mergeHarness struct {
	*publishHarness
	merger *mergingPublisher
}

func newMergeHarness(t *testing.T) *mergeHarness {
	t.Helper()
	h := newPublishHarness(t)
	m := &mergingPublisher{remoteHead: h.head, prNumber: 12}
	return &mergeHarness{publishHarness: h, merger: m}
}

// pumpMerge drives the REAL boundary pump through the merging publisher and returns the total number of
// publish attempts it has made. It is the publishHarness's pump() with the other double.
func (h *mergeHarness) pumpMerge() int {
	h.t.Helper()
	if err := publishApproved(context.Background(), h.spine, h.merger, h.tenant,
		h.runID, h.sessionID, h.respID, h.root, 1, publicationCredential{}); err != nil {
		h.t.Fatalf("publishApproved: %v", err)
	}
	return len(h.merger.targets)
}

// openAndPublishAPullRequest walks the run through the FIRST approval so there is something to merge: the
// model proposes a pull request, the run parks, the approver presses Approve, and the boundary publishes it.
// Only then does a merge have a destination, and that ordering is the design — the number in the receipt is
// the provider's own answer to an operation a human already authorized.
func (h *mergeHarness) openAndPublishAPullRequest() {
	h.t.Helper()
	if _, err := h.dispatch(tools.PullRequestTool(), redeliveryID("tc"), 1, map[string]any{"title": "t", "body": "b"}); !errors.Is(err, errRunParked) {
		h.t.Fatalf("the pull-request dispatch did not park: %v", err)
	}
	if got := h.click("Uapprover", "approve", h.requestHash()); got.Rejected != "" {
		h.t.Fatalf("the approver's click on the pull request was refused: %q", got.Rejected)
	}
	if n := h.pumpMerge(); n != 1 {
		h.t.Fatalf("the pull request published %d time(s), want 1: %v", n, h.merger.operations())
	}
}

// TestMergePullRequestWithoutAnApprovalPublishesNothing is E23 T6's RED #1, and it is deliberately the same
// shape as E22 T4's push assertion: the pump is driven WHILE the merge is still pending, because that is the
// order the danger runs in. Approving first and then checking the publisher would prove the approve works
// and nothing at all about what happens without one.
//
// A merge is the least reversible of the three publication operations — a push adds a branch, a pull request
// adds a conversation, a merge changes what the repository IS — so it is also the one where "the publisher
// was never called" has to be measured rather than assumed.
func TestMergePullRequestWithoutAnApprovalPublishesNothing(t *testing.T) {
	h := newMergeHarness(t)
	h.openAndPublishAPullRequest()

	// The model asks for a merge. The tool records a pending publication and the run PARKS — no answer goes
	// back, so the model cannot carry on as if the merge had happened.
	if _, err := h.dispatch(tools.MergeTool(), redeliveryID("tc"), 2, nil); !errors.Is(err, errRunParked) {
		t.Fatalf("the merge dispatch returned %v, want the park — a run that answers here is a run whose "+
			"approval can no longer be applied", err)
	}
	if got := h.runState(); got != string(statemachines.RunWaiting) {
		t.Fatalf("run state = %q while a human owes an answer about a MERGE, want waiting", got)
	}
	pubID, state, _, _, _, display := h.publicationRow("merge_pull_request")
	if state != "pending_approval" {
		t.Fatalf("the merge publication is %q straight out of the tool call, want pending_approval", state)
	}

	// THE RED HALF. A boundary runs — as it does whether or not anybody has decided anything — and the
	// publisher must not have been asked to merge.
	if n := h.pumpMerge(); n != 1 {
		t.Fatalf("the publisher performed %d operation(s) with a merge nobody approved: %v", n, h.merger.operations())
	}
	if _, state, _, _, _, _ := h.publicationRow("merge_pull_request"); state != "pending_approval" {
		t.Fatalf("a boundary moved an undecided merge to %q", state)
	}
	// approved=false is the row-level form of the same claim, asserted separately because the count above
	// would also be satisfied by a publisher that was asked and quietly did nothing.
	approved, err := h.spine.ApprovedPublicationsForRun(context.Background(), h.tenant, h.runID)
	if err != nil {
		t.Fatalf("ApprovedPublicationsForRun: %v", err)
	}
	for _, p := range approved {
		if p.ID == pubID {
			t.Fatalf("the undecided merge %s is in the publishable set", pubID)
		}
	}

	// An unauthorized click decides nothing either — the merge does not widen who may approve.
	hash := h.requestHash()
	if got := h.click("Uintruder", "approve", hash); got.Rejected == "" {
		t.Fatalf("an unmapped user's click on a MERGE was accepted: %+v", got)
	}
	if n := h.pumpMerge(); n != 1 {
		t.Fatalf("the publisher merged after an UNAUTHORIZED click: %v", h.merger.operations())
	}

	// And now the approve, which is the other half of the same claim: the gate is a gate, not a wall.
	if got := h.click("Uapprover", "approve", hash); got.Rejected != "" {
		t.Fatalf("the authorized approver's click was refused: %q", got.Rejected)
	}
	if n := h.pumpMerge(); n != 2 {
		t.Fatalf("the publisher performed %d operation(s) after the approve, want 2 (the PR and the merge): %v",
			n, h.merger.operations())
	}
	merged := h.merger.targets[1]
	if merged.Publication.Operation != "merge_pull_request" {
		t.Fatalf("the second publish was %q, want merge_pull_request", merged.Publication.Operation)
	}
	// WHAT WAS MERGED IS WHAT THE HUMAN READ. The number came from the run's OWN published receipt, the head
	// from the approved row, and both are named in the sentence the button carried.
	if got, _ := merged.Publication.Args["pull_request_number"].(float64); int(got) != h.merger.prNumber {
		t.Fatalf("the publisher merged pull request #%v, want the one THIS RUN published (#%d)", got, h.merger.prNumber)
	}
	if merged.Publication.HeadSHA != h.head {
		t.Fatalf("the merge was sent head %q, want the approved head %q", merged.Publication.HeadSHA, h.head)
	}
	want := fmt.Sprintf("merge pull request #%d (%s -> %s) on %s at %s",
		h.merger.prNumber, h.branch, publishBaseBranch, h.remote, h.head)
	if display != want {
		t.Fatalf("the approval detail is %q, want %q", display, want)
	}
	if _, state, _, _, _, _ := h.publicationRow("merge_pull_request"); state != "published" {
		t.Fatalf("the merge publication is %q after a successful merge, want published", state)
	}
	// A re-driven boundary (a lost ack, E10's detached execution) merges NOTHING a second time.
	if n := h.pumpMerge(); n != 2 {
		t.Fatalf("a second boundary merged again (%d publishes): a re-drive must find the row already published", n)
	}
	// And no credential is anywhere near the receipt a human or the model can read.
	var receipt string
	if err := h.spine.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT coalesce(receipt::text,'') FROM publications WHERE id = $1`, pubID).Scan(&receipt); err != nil {
		t.Fatalf("read the merge receipt: %v", err)
	}
	if !strings.Contains(receipt, "merged_"+h.head) {
		t.Fatalf("the merge receipt does not carry the provider's merge sha: %s", receipt)
	}
	for _, secret := range []string{"xoxb-", "ghs_", "x-access-token"} {
		if strings.Contains(receipt, secret) {
			t.Fatalf("the merge receipt carries something credential-shaped (%q): %s", secret, receipt)
		}
	}
}

// TestMergePullRequestRefusesWhenTheHeadMovedAfterTheApproval is E23 T6's RED #2 — the race, at the level
// that matters: not "does the client send sha" (that is proven against a server built to the published
// contract in adapters/repositories) but "does the PUMP send the head the human approved".
//
// The window is real and ordinary. A human reads a message, thinks about it, and presses a button; the merge
// happens at the next boundary. Anything can push to that branch in between — the agent itself, on a woken
// attempt. GitHub documents `sha` as OPTIONAL and documents 409 as its answer when the head does not match,
// which means the vendor hands this guard over for free and using it was a decision available to anyone who
// read the page. What this test pins is that we took it: the merge does NOT happen, the publication stays
// approved for a later re-drive, and a warning says so where a human can see it (REP-010).
func TestMergePullRequestRefusesWhenTheHeadMovedAfterTheApproval(t *testing.T) {
	ctx := context.Background()
	h := newMergeHarness(t)
	h.openAndPublishAPullRequest()

	if _, err := h.dispatch(tools.MergeTool(), redeliveryID("tc"), 2, nil); !errors.Is(err, errRunParked) {
		t.Fatalf("the merge dispatch did not park: %v", err)
	}
	pubID, _, _, _, _, display := h.publicationRow("merge_pull_request")
	if !strings.Contains(display, h.head) {
		t.Fatalf("the approval detail %q does not name the exact head %s the approve will authorize", display, h.head)
	}
	if got := h.click("Uapprover", "approve", h.requestHash()); got.Rejected != "" {
		t.Fatalf("the approver's click was refused: %q", got.Rejected)
	}

	// THE RACE: the branch advances between the button and the boundary. The approved row still carries the
	// approved head, so that is what is sent — and the provider refuses.
	h.merger.remoteHead = "0123456789012345678901234567890123456789"
	if n := h.pumpMerge(); n != 2 {
		t.Fatalf("the pump attempted %d publish(es), want 2 — the attempt SHOULD be made; what must not happen "+
			"is the merge", n)
	}
	if got := h.merger.targets[1].Publication.HeadSHA; got != h.head {
		t.Fatalf("the pump sent head %q after the branch moved, want the APPROVED head %q — sending the current "+
			"head would merge content nobody read", got, h.head)
	}
	// Nothing merged, and the row is still approved: a re-drive after somebody re-approves (or after the
	// branch is put back) is safe, and the operation was not silently dropped.
	if _, state, _, _, _, _ := h.publicationRow("merge_pull_request"); state != "approved" {
		t.Fatalf("the merge publication is %q after a refused merge, want still approved", state)
	}
	// THE WARNING (REP-010). A refusal a human never sees is a merge that mysteriously did not happen.
	var warnings int
	if err := h.spine.Pool().QueryRow(storage.WithSystemScope(ctx),
		`SELECT count(*) FROM events WHERE session_id = $1 AND type = 'warning.raised.v1'
		   AND payload->>'publication_id' = $2 AND payload->>'detail' LIKE '%pull_request_head_moved%'`,
		h.sessionID, pubID).Scan(&warnings); err != nil {
		t.Fatalf("count publication warnings: %v", err)
	}
	if warnings < 1 {
		t.Fatalf("a refused merge journaled %d warnings naming the moved head, want at least 1", warnings)
	}

	// And when the branch is back at the approved head, the SAME approved row merges — the refusal was a
	// refusal, not a terminal failure.
	h.merger.remoteHead = h.head
	if n := h.pumpMerge(); n != 3 {
		t.Fatalf("the recovered boundary attempted %d publishes, want 3", n)
	}
	if _, state, _, _, _, _ := h.publicationRow("merge_pull_request"); state != "published" {
		t.Fatalf("the merge publication is %q after the head came back, want published", state)
	}
}

// TestMergePullRequestWithNoPublishedPullRequestRefuses is the destination guarantee from the other side.
// RED #3 (tools/publish_test.go) proves the model has no FIELD to name a pull request in; this proves there
// is no pull request to merge unless this run published one — so a merge tool call on a run that never
// opened a PR cannot borrow somebody else's, it simply has nothing to point at.
func TestMergePullRequestWithNoPublishedPullRequestRefuses(t *testing.T) {
	h := newMergeHarness(t)
	_, err := tools.MergeTool().Exec(context.Background(), h.execEnv(), nil)
	if err == nil {
		t.Fatal("a merge on a run with no published pull request was accepted")
	}
	if !strings.Contains(err.Error(), "nothing to merge") {
		t.Fatalf("the refusal reads %q; it must say what is missing, because the model has no other way to "+
			"learn that the pull request has to be published first", err)
	}
	// Nothing was recorded: no pending row, so no human is asked about an operation with no destination.
	var rows int
	if err := h.spine.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(*) FROM publications WHERE run_id = $1 AND operation = 'merge_pull_request'`,
		h.runID).Scan(&rows); err != nil {
		t.Fatalf("count merge publications: %v", err)
	}
	if rows != 0 {
		t.Fatalf("%d merge publication(s) were recorded with nothing to merge", rows)
	}
}

// TestMergePullRequestDeniedPreventsTheMergeAndReleasesTheRun is the half a decision surface is worth
// nothing without, on the operation where it matters most. A deny has to do BOTH things: stop the merge, and
// let the run go. A gate that refuses the merge and leaves the run parked forever has replaced one problem
// with a worse one.
func TestMergePullRequestDeniedPreventsTheMergeAndReleasesTheRun(t *testing.T) {
	h := newMergeHarness(t)
	h.openAndPublishAPullRequest()
	if _, err := h.dispatch(tools.MergeTool(), redeliveryID("tc"), 2, nil); !errors.Is(err, errRunParked) {
		t.Fatalf("the merge dispatch did not park: %v", err)
	}
	if got := h.click("Uapprover", "deny", h.requestHash()); got.Rejected != "" {
		t.Fatalf("the deny click was refused: %q", got.Rejected)
	}
	if _, state, _, _, _, _ := h.publicationRow("merge_pull_request"); state != "denied" {
		t.Fatalf("the merge publication is %q after a deny, want denied", state)
	}
	if got := h.runState(); got == string(statemachines.RunWaiting) {
		t.Fatal("the run is STILL waiting after its merge was denied")
	}
	if n := h.pumpMerge(); n != 1 {
		t.Fatalf("the publisher merged a DENIED publication: %v", h.merger.operations())
	}
}

// TestMergePullRequestMethodComesFromTheBindingNotTheModel is the third destination field, and it is the one
// most easily mistaken for a preference. Squash versus merge versus rebase decides what a repository's
// history LOOKS like afterwards, which is a decision a team makes once — so it lives on the binding's policy
// beside the base branch, and the model has no field for it (RED #3).
//
// The honest ceiling is stated by this test rather than beside it: merge_method is DEPLOYMENT policy. A
// deployment cannot choose squash for one pull request and merge for the next, and E23 does not build that.
func TestMergePullRequestMethodComesFromTheBindingNotTheModel(t *testing.T) {
	ctx := context.Background()
	h := newMergeHarness(t)
	// The operator's decision, written where every other destination decision lives.
	execSQL(t, h.spine.Pool(), `UPDATE repository_bindings SET policy = '{"merge_method":"squash"}'::jsonb WHERE id = $1`, h.bindingID)
	target, found, err := h.spine.RunPublicationTarget(ctx, h.tenant, h.runID)
	if err != nil || !found {
		t.Fatalf("RunPublicationTarget: (%v,%v)", found, err)
	}
	if target.MergeMethod != "squash" {
		t.Fatalf("the resolved merge method is %q, want the binding policy's squash", target.MergeMethod)
	}

	h.openAndPublishAPullRequest()
	if _, err := h.dispatch(tools.MergeTool(), redeliveryID("tc"), 2, nil); !errors.Is(err, errRunParked) {
		t.Fatalf("the merge dispatch did not park: %v", err)
	}
	if got := h.click("Uapprover", "approve", h.requestHash()); got.Rejected != "" {
		t.Fatalf("the approver's click was refused: %q", got.Rejected)
	}
	if n := h.pumpMerge(); n != 2 {
		t.Fatalf("the pump published %d time(s), want 2: %v", n, h.merger.operations())
	}
	if got, _ := h.merger.targets[1].Publication.Args["merge_method"].(string); got != "squash" {
		t.Fatalf("the merge carried method %q, want the binding's squash", got)
	}
	// An unset policy defaults to `merge` at the adapter, and it is NOT decided by the model either.
	var pubs []coordinator.Publication
	if pubs, err = h.spine.ApprovedPublicationsForRun(ctx, h.tenant, h.runID); err != nil {
		t.Fatalf("ApprovedPublicationsForRun: %v", err)
	}
	if len(pubs) != 0 {
		t.Fatalf("%d publication(s) remain approved-but-unpublished after a clean merge", len(pubs))
	}
}
