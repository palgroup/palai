//go:build component

package store_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/integrations/slack"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/storage"
)

// E23 T2, the Slack half — TWO gates, and both have to be passed.
//
// THE MEASUREMENT: "SLACK_APPROVER_IDS is just an env var on one integration" is half true and misleading
// in both directions. Its durable home is slack_connections.allowed_users (000035_slack.up.sql:29), read
// by SlackAuthorizationPolicyFor and enforced deny-by-default by ApproverAuthorized; the env var is only
// what `palai up` WRITES there (up.go:861). So it is sturdier than an env var — tenant-scoped, durable
// policy — and still connection-shaped: that same connection also carries CHANNEL scope, which a
// platform-wide list cannot express.
//
// Hence two gates rather than one replacing the other. Slack's own list runs FIRST (it is the one that can
// refuse before any command is enqueued at all), and the project list runs inside ApplyApprovalDecision,
// the single throat both surfaces pass through. A narrowing on either side is a narrowing.
//
// Everything below drives the SHIPPED route: a signed form body → slack.MapInteractiveApproval →
// SlackAuthorizationPolicyFor → ApproverAuthorized → AcceptCommand → ApplyApprovalDecision.

// setProjectApprovers writes the project-level allow-list the coordinator reads live.
func (f *slackFixture) setProjectApprovers(t *testing.T, policy string) {
	t.Helper()
	exec(t, f.pool, `UPDATE projects SET config_policy=$2 WHERE id=$1`, f.project, policy)
}

// slackPrincipalOf is the canonical principal for the fixture's mapped clicker.
func (f *slackFixture) slackPrincipalOf(user string) string {
	return coordinator.ApproverPrincipal(coordinator.ApproverSurfaceSlack, f.team, user)
}

// TestSlackApproverBothListsMustBePassed is the crown of the pair. "Umapped" is in the CONNECTION's
// allowed_users, so Slack's own gate lets the click through — and the PROJECT names somebody else. The
// click must decide nothing.
//
// This is the case the connection list alone cannot express and the project list alone cannot express:
// authorized on the workspace, unauthorized on the platform.
func TestSlackApproverBothListsMustBePassed(t *testing.T) {
	f := newSlackFixture(t)
	thread := f.seedApproval(t, "C90", "1700000090.000100")
	f.setProjectApprovers(t, `{"approvers":["slack:TOTHER:Usomebody","key_someone"]}`)

	resp := f.click(t, "Umapped", thread.channel, thread.root, slack.ActionApprove, thread.requestHash, time.Now())
	defer resp.Body.Close()

	if state := f.publicationState(t, thread.publicationID); state != "pending_approval" {
		t.Fatalf("the publication is %q after a click by a workspace-authorized but project-UNLISTED user, want pending_approval", state)
	}
	// The command still SETTLES. A refusal that leaves a queued command behind is re-read by the boundary
	// pump at every boundary for the life of the run.
	if state := f.commandState(t, "approve"); state != "applied" {
		t.Fatalf("the approve command is %q, want applied — a refusal must settle", state)
	}
	// And nothing was drawn as approved: SLK-006's repair must not tell a human their click landed.
	for _, call := range f.callsTo("/chat.update") {
		t.Fatalf("a refused click repaired the message anyway: %+v", call)
	}
}

// TestSlackApproverListedOnBothPasses: a principal in BOTH lists decides, and the canonical form is the one
// ApproverPrincipal renders — slack:<team_id>:<user_id>, workspace-qualified, because a Slack user id is
// unique only within its workspace.
func TestSlackApproverListedOnBothPasses(t *testing.T) {
	f := newSlackFixture(t)
	thread := f.seedApproval(t, "C91", "1700000091.000100")
	f.setProjectApprovers(t, `{"approvers":["`+f.slackPrincipalOf("Umapped")+`"]}`)

	resp := f.click(t, "Umapped", thread.channel, thread.root, slack.ActionApprove, thread.requestHash, time.Now())
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("an authorized click = %d, want 200", resp.StatusCode)
	}
	if state := f.publicationState(t, thread.publicationID); state != "approved" {
		t.Fatalf("the publication is %q after a click by a doubly-listed approver, want approved", state)
	}
}

// TestSlackApproverAUserIdAloneIsNotAPrincipal is the workspace-qualification proof, and it is a real
// security property rather than a formatting preference: Slack user ids are unique only within a workspace,
// so a bare "Umapped" in a project list must not admit the Umapped of some other workspace — or of this one.
func TestSlackApproverAUserIdAloneIsNotAPrincipal(t *testing.T) {
	f := newSlackFixture(t)
	thread := f.seedApproval(t, "C92", "1700000092.000100")
	f.setProjectApprovers(t, `{"approvers":["Umapped","slack:TWRONGTEAM:Umapped"]}`)

	resp := f.click(t, "Umapped", thread.channel, thread.root, slack.ActionApprove, thread.requestHash, time.Now())
	defer resp.Body.Close()

	if state := f.publicationState(t, thread.publicationID); state != "pending_approval" {
		t.Fatalf("the publication is %q, want pending_approval — a bare user id, and the same id in another workspace, are not this principal", state)
	}
}

// TestSlackApproverWithNoProjectListIsBitUnchanged: the half that is not negotiable. A deployment that has
// configured no project approver list must behave EXACTLY as it does today — the connection's own
// allowed_users is the only gate, and a mapped clicker approves.
func TestSlackApproverWithNoProjectListIsBitUnchanged(t *testing.T) {
	f := newSlackFixture(t)
	thread := f.seedApproval(t, "C93", "1700000093.000100")
	// No setProjectApprovers call: config_policy stays NULL, which is every existing deployment.

	resp := f.click(t, "Umapped", thread.channel, thread.root, slack.ActionApprove, thread.requestHash, time.Now())
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a mapped click with no project list = %d, want 200", resp.StatusCode)
	}
	if state := f.publicationState(t, thread.publicationID); state != "approved" {
		t.Fatalf("the publication is %q with NO project approver list, want approved (bit-unchanged)", state)
	}
	if state := f.commandState(t, "approve"); state != "applied" {
		t.Fatalf("the approve command is %q, want applied", state)
	}
}

// TestSlackApproverConnectionListStillRunsFirst: the project list does not retire the connection's. An
// UNMAPPED clicker is refused before any command is enqueued at all, even when the project list would have
// named them — because the connection list is the one carrying channel scope and the one that can refuse
// without leaving a durable command behind.
func TestSlackApproverConnectionListStillRunsFirst(t *testing.T) {
	f := newSlackFixture(t)
	thread := f.seedApproval(t, "C94", "1700000094.000100")
	f.setProjectApprovers(t, `{"approvers":["`+f.slackPrincipalOf("Uunmapped")+`"]}`)

	resp := f.click(t, "Uunmapped", thread.channel, thread.root, slack.ActionApprove, thread.requestHash, time.Now())
	defer resp.Body.Close()

	if n := f.commandCount(t, "approve"); n != 0 {
		t.Fatalf("an unmapped clicker enqueued %d approve commands, want 0 — allowed_users must refuse BEFORE a command exists", n)
	}
	if state := f.publicationState(t, thread.publicationID); state != "pending_approval" {
		t.Fatalf("the publication is %q, want pending_approval", state)
	}
}

// TestSlackApproverNarrowingTheProjectListTakesAPendingApprovalWithIt: the list is read at DECISION time,
// never frozen when the approval was created. The argument is the tree's own, written for allowed_channels
// at slack_decision.go:104-107 — an operator narrowing an allow-list is what containing an incident looks
// like, and it must reach the buttons already posted.
func TestSlackApproverNarrowingTheProjectListTakesAPendingApprovalWithIt(t *testing.T) {
	f := newSlackFixture(t)
	f.setProjectApprovers(t, `{"approvers":["`+f.slackPrincipalOf("Umapped")+`"]}`)
	// The approval is born while this user IS an approver, and its button is posted.
	thread := f.seedApproval(t, "C95", "1700000095.000100")

	// The incident is contained: the operator narrows the list.
	f.setProjectApprovers(t, `{"approvers":["slack:`+f.team+`:Uincidentcommander"]}`)

	resp := f.click(t, "Umapped", thread.channel, thread.root, slack.ActionApprove, thread.requestHash, time.Now())
	defer resp.Body.Close()
	if state := f.publicationState(t, thread.publicationID); state != "pending_approval" {
		t.Fatalf("the publication is %q after a narrowing, want pending_approval — an already-posted button must go dead", state)
	}
}

// TestSlackApproverTheRefusalRecordsNoDecider: a refused click leaves no attribution behind. approvals
// records WHO decided (the SLK-007 audit half); a decision that did not happen must not name anyone.
func TestSlackApproverTheRefusalRecordsNoDecider(t *testing.T) {
	f := newSlackFixture(t)
	thread := f.seedApproval(t, "C96", "1700000096.000100")
	f.setProjectApprovers(t, `{"approvers":["slack:TOTHER:Usomebody"]}`)

	resp := f.click(t, "Umapped", thread.channel, thread.root, slack.ActionApprove, thread.requestHash, time.Now())
	defer resp.Body.Close()

	var decidedBy string
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT COALESCE(decided_by,'') FROM approvals WHERE publication_id=$1`, thread.publicationID).Scan(&decidedBy); err != nil {
		t.Fatalf("read the approval decision: %v", err)
	}
	if decidedBy != "" {
		t.Fatalf("a refused click recorded %q as the decider; nothing was decided", decidedBy)
	}
}
