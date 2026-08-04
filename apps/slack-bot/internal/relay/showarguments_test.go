package relay

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/integrations/slack"
	palai "github.com/palgroup/palai/sdks/go"
)

// nestedApproval is the shape that makes the third button worth having at all: a call whose arguments
// NEST. The channel table renders `fields` as one compacted cell; the modal renders `fields.assignee` and
// `fields.labels[0]` as their own rows. A test over a flat document could not tell the two apart, and the
// whole justification for wiring this button rather than deleting it is that they differ.
func nestedApproval() palai.Approval {
	return palai.Approval{
		ID: "apr_1", RequestHash: "rh_1", SessionID: "ses_1", Kind: "tool",
		Identity: "jira.transitionIssue", OperatorLabel: "Move a Jira issue",
		Arguments: `{"issue":"PAL-42","fields":{"assignee":"aylin","labels":["urgent","ios"]},"notify":true}`,
		ExpiresAt: time.Now().Add(11 * time.Minute).UTC().Format(time.RFC3339),
	}
}

func showArgumentsDeps(t *testing.T, approvals ...palai.Approval) ApprovalDeps {
	t.Helper()
	deps := newTestApprovalDeps(t)
	deps.Palai = &fakeApprovalsPalai{pages: []palai.Page[palai.Approval]{{Data: approvals}}}
	return deps
}

func showArgumentsClick(user, value, trigger string) slack.ShowArgumentsIntent {
	return slack.ShowArgumentsIntent{
		TeamID: "T1", UserID: user, RequestHash: value, TriggerID: trigger,
		ChannelID: "C1", ThreadTS: "100.001", MessageTS: "ts_approval",
	}
}

// openedView decodes the single views.open body the fake recorded.
func openedView(t *testing.T, deps ApprovalDeps) (triggerID string, blocks []map[string]any, raw []byte) {
	t.Helper()
	fake := deps.Slack.(*fakeApprovalSlack)
	if len(fake.opened) != 1 {
		t.Fatalf("views.open was called %d time(s), want exactly one", len(fake.opened))
	}
	var body struct {
		TriggerID string `json:"trigger_id"`
		View      struct {
			Blocks []map[string]any `json:"blocks"`
		} `json:"view"`
	}
	if err := json.Unmarshal(fake.opened[0], &body); err != nil {
		t.Fatalf("decode the views.open body: %v (%s)", err, fake.opened[0])
	}
	return body.TriggerID, body.View.Blocks, fake.opened[0]
}

// TestAShowArgumentsClickOpensTheCallInFull is the affordance, closed.
func TestAShowArgumentsClickOpensTheCallInFull(t *testing.T) {
	deps := showArgumentsDeps(t, nestedApproval())
	if err := OnShowArguments(context.Background(), deps,
		showArgumentsClick("U_allowed", approvalActionValue("apr_1", "rh_1"), "trg_1")); err != nil {
		t.Fatalf("OnShowArguments: %v", err)
	}

	trigger, blocks, raw := openedView(t, deps)
	if trigger != "trg_1" {
		t.Fatalf("views.open carried trigger %q, want the click's own", trigger)
	}
	// THE LEAVES, which is the thing the channel message cannot show. Asserted by PATH rather than by
	// value: `aylin` appears in both renderings, `fields.assignee` only in this one.
	for _, path := range []string{"fields.assignee", "fields.labels[0]", "fields.labels[1]"} {
		if !strings.Contains(string(raw), path) {
			t.Fatalf("the modal does not address the leaf %q; a modal that shows no more than the message "+
				"it was opened from is a button with nothing to open", path)
		}
	}
	// The expiry alert: a human deciding is entitled to know the deadline they are deciding against.
	var sawAlert bool
	for _, block := range blocks {
		if block["type"] == "alert" {
			sawAlert = true
		}
	}
	if !sawAlert {
		t.Fatal("the approval expires and the modal says nothing about it")
	}
}

// TestTheModalAndTheMessageCannotDisagree. Both surfaces are rendered from ONE ledger row through the same
// DeriveApprovalDisplay, and the claim their design rests on is that neither can say something the other
// does not — a description that leaked into only the modal would be as much of a breach as one in both.
func TestTheModalAndTheMessageCannotDisagree(t *testing.T) {
	approval := nestedApproval()
	deps := showArgumentsDeps(t, approval)
	if err := OnShowArguments(context.Background(), deps,
		showArgumentsClick("U_allowed", approvalActionValue("apr_1", "rh_1"), "trg_1")); err != nil {
		t.Fatalf("OnShowArguments: %v", err)
	}
	_, _, modal := openedView(t, deps)
	message := buildApprovalMessage("C1", "100.001", approval)

	for _, shared := range []string{approval.Identity, "Move a Jira issue", "PAL-42"} {
		if !strings.Contains(string(message), shared) {
			t.Fatalf("the channel message does not carry %q, so the comparison below measures nothing", shared)
		}
		if !strings.Contains(string(modal), shared) {
			t.Fatalf("the modal does not carry %q, which the message it was opened from does", shared)
		}
	}
}

// TestAnUnlistedClickerOpensNothingAndNeverReachesTheControlPlane. Opening this document is a smaller act
// than deciding, but it still shows one human another human's pending command — so it passes the same
// gate, in the same ORDER. A version that listed approvals first and checked the clicker afterwards would
// let every unauthorized click reach the server, race or no race.
func TestAnUnlistedClickerOpensNothingAndNeverReachesTheControlPlane(t *testing.T) {
	deps := showArgumentsDeps(t, nestedApproval())
	err := OnShowArguments(context.Background(), deps,
		showArgumentsClick("U_stranger", approvalActionValue("apr_1", "rh_1"), "trg_1"))
	if !errors.Is(err, ErrApproverNotAllowed) {
		t.Fatalf("OnShowArguments = %v, want ErrApproverNotAllowed", err)
	}
	if opened := len(deps.Slack.(*fakeApprovalSlack).opened); opened != 0 {
		t.Fatalf("an unlisted click opened %d view(s)", opened)
	}
	if lists := deps.Palai.(*fakeApprovalsPalai).lists; lists != 0 {
		t.Fatalf("an unlisted click made %d call(s) to GET /v1/approvals; the allow-list check must run "+
			"BEFORE the lookup", lists)
	}
}

// TestAClickWithNoTriggerSpendsNothing — a views.open without a trigger is a request Slack rejects, and
// the three seconds spent discovering that are the three seconds the modal had to open in.
func TestAClickWithNoTriggerSpendsNothing(t *testing.T) {
	deps := showArgumentsDeps(t, nestedApproval())
	if err := OnShowArguments(context.Background(), deps,
		showArgumentsClick("U_allowed", approvalActionValue("apr_1", "rh_1"), "")); err == nil {
		t.Fatal("a click with no trigger_id was accepted")
	}
	if lists := deps.Palai.(*fakeApprovalsPalai).lists; lists != 0 {
		t.Fatalf("a click that can open nothing still made %d control-plane call(s)", lists)
	}
}

// TestAValueWhoseHalvesDisagreeOpensNothing. Nothing in Slack can produce this from a button this bridge
// minted — both halves come off one GET /v1/approvals row — so it is a forged or mangled value.
func TestAValueWhoseHalvesDisagreeOpensNothing(t *testing.T) {
	deps := showArgumentsDeps(t, nestedApproval())
	err := OnShowArguments(context.Background(), deps,
		showArgumentsClick("U_allowed", approvalActionValue("apr_SOMEONE_ELSES", "rh_1"), "trg_1"))
	if err == nil {
		t.Fatal("a click whose approval id and request hash name different approvals opened a view")
	}
	if opened := len(deps.Slack.(*fakeApprovalSlack).opened); opened != 0 {
		t.Fatalf("opened %d view(s) for a value whose halves disagree", opened)
	}
}

// TestADecidedApprovalOpensNothing. GET /v1/approvals answers the OPEN rows, so a decided approval falls
// out of the lookup — which is right rather than unfortunate: the arguments a human is shown are the ones
// they are being asked to authorize, and once the answer is in there is nothing left to authorize.
func TestADecidedApprovalOpensNothing(t *testing.T) {
	deps := showArgumentsDeps(t) // no open approvals at all
	err := OnShowArguments(context.Background(), deps,
		showArgumentsClick("U_allowed", approvalActionValue("apr_1", "rh_1"), "trg_1"))
	if !errors.Is(err, ErrApprovalNotFound) {
		t.Fatalf("OnShowArguments = %v, want ErrApprovalNotFound", err)
	}
	if opened := len(deps.Slack.(*fakeApprovalSlack).opened); opened != 0 {
		t.Fatalf("opened %d view(s) for an approval nobody is waiting on", opened)
	}
}

// TestOpeningArgumentsWritesNothingDurable. The trigger dies in three seconds and a row written here is a
// lock taken here. This asserts the absence by giving the bridge a claim store that FAILS every write: a
// path that touched it would surface the error, and a path that does not is unaffected.
func TestOpeningArgumentsWritesNothingDurable(t *testing.T) {
	deps := showArgumentsDeps(t, nestedApproval())
	deps.Posts = refusingPosts{}
	if err := OnShowArguments(context.Background(), deps,
		showArgumentsClick("U_allowed", approvalActionValue("apr_1", "rh_1"), "trg_1")); err != nil {
		t.Fatalf("OnShowArguments touched the durable claim store: %v", err)
	}
	if opened := len(deps.Slack.(*fakeApprovalSlack).opened); opened != 1 {
		t.Fatalf("views.open was called %d time(s)", opened)
	}
}

// refusingPosts fails every durable write, so a caller that made one cannot go green.
type refusingPosts struct{}

func (refusingPosts) ClaimApprovalPost(context.Context, string, string, string, string) (bool, error) {
	return false, errors.New("this path must not claim anything")
}

func (refusingPosts) ReleaseApprovalPost(context.Context, string, string) error {
	return errors.New("this path must not release anything")
}
