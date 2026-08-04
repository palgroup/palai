package relay

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	palai "github.com/palgroup/palai/sdks/go"
)

// THE ROW THE OPERATOR'S FIRST REAL APPROVAL ARRIVED AS, copied from GET /v1/approvals on 2026-08-04. It
// is reproduced whole rather than trimmed because the point of these tests is which of these columns
// reaches a human and how — a fixture that dropped the ones that used to be invisible would be asserting
// the fix against itself.
func livePublicationApproval() palai.Approval {
	return palai.Approval{
		ID: "apr_df0be33e0fc0c2af", Object: "approval", Kind: "publication",
		RunID: "run_e4036140a36cba72b245b7f04201cc66", SessionID: "ses_3c2c5226a428bb77f9b67e5abb5aec29",
		ResponseID: "resp_dd0b460f63532213372c23f9d18a569d", RequestHash: "req_3d7a1cf12bf3a1a3498364bf7adbe91f",
		Identity: "push_branch",
		// The one run-on sentence this whole change exists to replace.
		OperatorLabel: "push agent/ws_986c86ee5a07915f75cd99ebf33d422a/run_e4036140a36cba72b245b7f04201cc66 " +
			"@ 8126e83cc544d6777b0c93d4543e26cb1c7cebdb -> https://github.com/palgroup/centauri-ios.git",
		Arguments:     "{}",
		PublicationID: "pub_87ee2c5f93df978c", Operation: "push_branch",
		Remote:  "https://github.com/palgroup/centauri-ios.git",
		Branch:  "agent/ws_986c86ee5a07915f75cd99ebf33d422a/run_e4036140a36cba72b245b7f04201cc66",
		Base:    "main",
		HeadSHA: "8126e83cc544d6777b0c93d4543e26cb1c7cebdb",
		// The server answers a DESCRIPTION here, never a value.
		CredentialRef: "github-ios-preview", Credential: "this repository binding's own credential",
	}
}

// screenText flattens every string on a rendered approval body, the way blocks_test.go's own sweeps do: a
// raw substring check over marshalled bytes cannot fail, because encoding/json escapes what it is looking
// for.
func screenText(t *testing.T, raw []byte) string {
	t.Helper()
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode the approval body: %v", err)
	}
	var out []string
	var walk func(any)
	walk = func(node any) {
		switch v := node.(type) {
		case string:
			out = append(out, v)
		case []any:
			for _, e := range v {
				walk(e)
			}
		case map[string]any:
			for _, e := range v {
				walk(e)
			}
		}
	}
	walk(decoded)
	return strings.Join(out, "\n")
}

// tableRowsOf returns each table row of a rendered body as "name=value", which is how the destination is
// asserted: the pairing is the claim, not the presence of two strings somewhere on the screen.
func tableRowsOf(t *testing.T, raw []byte) []string {
	t.Helper()
	var body struct {
		Blocks []struct {
			Type string `json:"type"`
			Rows [][]struct {
				Text string `json:"text"`
			} `json:"rows"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode the approval body: %v", err)
	}
	var out []string
	for _, b := range body.Blocks {
		if b.Type != "table" {
			continue
		}
		for _, row := range b.Rows {
			cells := make([]string, 0, len(row))
			for _, c := range row {
				cells = append(cells, c.Text)
			}
			out = append(out, strings.Join(cells, "="))
		}
	}
	return out
}

// EVERY FACT GETS ITS OWN LABELLED ROW. This is the whole of defect 2: the columns were always there, they
// were just being read out of one unlabelled sentence.
func TestAPublicationApprovalLabelsEachFactItAuthorizes(t *testing.T) {
	rows := tableRowsOf(t, buildApprovalMessage("C1", "1.1", livePublicationApproval()))
	want := []string{
		"what=value",
		"repository=https://github.com/palgroup/centauri-ios.git",
		"branch=agent/ws_986c86ee5a07915f75cd99ebf33d422a/run_e4036140a36cba72b245b7f04201cc66",
		"base=main",
		"commit=8126e83cc544d6777b0c93d4543e26cb1c7cebdb",
		"credential=this repository binding's own credential",
	}
	if len(rows) != len(want) {
		t.Fatalf("the screen has %d row(s), want %d:\n got: %q\nwant: %q", len(rows), len(want), rows, want)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Fatalf("row %d is %q, want %q (the order is how a human reads the decision: where, then what, then whose authority)", i, rows[i], want[i])
		}
	}
}

// THE RUN-ON SENTENCE IS GONE FROM THE SCREEN, and this is asserted separately from the rows above because
// a fix that ADDED a nice table while leaving the old line in place would satisfy that test and leave the
// message exactly as cluttered as the operator found it.
func TestAPublicationApprovalDropsTheRunOnSentence(t *testing.T) {
	approval := livePublicationApproval()
	text := screenText(t, buildApprovalMessage("C1", "1.1", approval))
	if strings.Contains(text, approval.OperatorLabel) {
		t.Fatalf("the row's pre-baked sentence is still on the screen:\n%s", text)
	}
	if !strings.Contains(text, "Push a branch to a repository") {
		t.Fatalf("the screen does not say in words what is being authorized:\n%s", text)
	}
}

// NOTHING IS ABBREVIATED. The readability fix must not become a truthfulness bug: what the button
// authorizes is this exact branch at this exact commit.
func TestAPublicationApprovalShowsTheBranchAndCommitInFull(t *testing.T) {
	approval := livePublicationApproval()
	text := screenText(t, buildApprovalMessage("C1", "1.1", approval))
	for _, exact := range []string{approval.Branch, approval.HeadSHA, approval.Remote} {
		if !strings.Contains(text, exact) {
			t.Fatalf("%q does not appear in full on the screen:\n%s", exact, text)
		}
	}
}

// AN UNKNOWN OPERATION IS NAMED, NOT DESCRIBED. Inventing a sentence for an identifier nobody has seen is
// how a screen ends up confidently describing the wrong action — the same fallback rule relay.go's
// toolTitle already follows, on the screen where being wrong costs the most.
func TestAnUnknownPublicationOperationFallsBackToItsOwnName(t *testing.T) {
	approval := livePublicationApproval()
	approval.Operation = "force_push_everything"
	text := screenText(t, buildApprovalMessage("C1", "1.1", approval))
	if !strings.Contains(text, "force_push_everything") {
		t.Fatalf("an unmapped operation lost its own name:\n%s", text)
	}
}

// AN EMPTY COLUMN IS OMITTED, NOT RENDERED BLANK. `base` means nothing for a bare push, and a labelled
// empty cell reads as "this is nothing" rather than "this does not apply".
func TestAPublicationApprovalOmitsTheColumnsThatDoNotApply(t *testing.T) {
	approval := livePublicationApproval()
	approval.Base, approval.Credential = "", ""
	for _, row := range tableRowsOf(t, buildApprovalMessage("C1", "1.1", approval)) {
		if strings.HasPrefix(row, "base=") || strings.HasPrefix(row, "credential=") {
			t.Fatalf("an inapplicable column was rendered anyway: %q", row)
		}
	}
}

// A TOOL APPROVAL IS UNTOUCHED BY ANY OF THIS. The publication branch keys on Kind, so the screen a gated
// TOOL call gets is the one it always got — arguments in their own table, all three buttons.
func TestAToolApprovalStillRendersItsArguments(t *testing.T) {
	approval := palai.Approval{
		ID: "apr_tool", Kind: "tool", RequestHash: "rh_1",
		Identity: "jira.transitionIssue", OperatorLabel: "the service account may move tickets",
		Arguments: `{"issue":"PAL-42"}`,
	}
	body := buildApprovalMessage("C1", "1.1", approval)
	rows := tableRowsOf(t, body)
	if len(rows) != 2 || rows[0] != "argument=value" || rows[1] != `issue="PAL-42"` {
		t.Fatalf("a tool approval's argument table is %q, want the argument header and its one row", rows)
	}
	if !strings.Contains(screenText(t, body), "the service account may move tickets") {
		t.Fatal("a tool approval lost its operator label")
	}
}

// THE CREDENTIAL IS NAMED, NEVER SHOWN. GET /v1/approvals answers a description for exactly this reason,
// and the screen must not become the first place a value appears.
func TestAPublicationApprovalNeverPutsACredentialValueOnTheScreen(t *testing.T) {
	approval := livePublicationApproval()
	text := screenText(t, buildApprovalMessage("C1", "1.1", approval))
	if strings.Contains(text, approval.CredentialRef) {
		t.Fatalf("the credential HANDLE reached the screen; only its description belongs there:\n%s", text)
	}
	if !strings.Contains(text, "this repository binding's own credential") {
		t.Fatalf("the screen does not say which authority the push would use:\n%s", text)
	}
}

// -------------------------------------------------------------------------------------------------
// defect 3: the container headline
// -------------------------------------------------------------------------------------------------

// A PARKED RUN SAYS IT IS WAITING FOR A PERSON, in the one line a reader sees without expanding anything.
//
// MEASURED, on the same live session: `model_step.created.v1` (seq 52) set the headline to "Thinking…",
// `model_step.completed.v1` (53) writes none by design, and `approval.requested.v1` (54) wrote body text
// and called the hook and nothing else. So the title said the model was thinking while it was in fact
// waiting for the very person reading that title — and, because a parked run produces no further event
// until somebody clicks, nothing would ever have overwritten it.
func TestAParkedRunsHeadlineSaysItIsWaitingForApproval(t *testing.T) {
	events := seq([]palai.Event{
		{Type: "model_step.created.v1"},
		{Type: ApprovalRequestedEventType, Data: map[string]any{"request_hash": "rh_1"}},
	})
	fake := &fakeSlack{}
	if err := Run(context.Background(), Deps{Events: staticStream(events), Slack: fake,
		OnApproval: noApprovals, Delivery: &recordedDelivery{}}, "sess_1", "C1", "1.1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fake.plans) == 0 {
		t.Fatal("the container headline was never written at all")
	}
	last := fake.plans[len(fake.plans)-1]
	if last != awaitingApprovalHeadline {
		t.Fatalf("a parked run's headline reads %q, want %q — the headline is what shows while the container is collapsed, and the body line saying the same thing is inside it (all headlines: %v)",
			last, awaitingApprovalHeadline, fake.plans)
	}
	// The thinking state really was written first, so this test is watching the headline being CHANGED out
	// of the state the operator was stuck in, rather than a run that never said "Thinking…" at all. The
	// sequence starts at openingHeadline — Run writes that before reading a single event — so the check is
	// on the ORDER of the two, not on the first entry.
	thinkingAt, waitingAt := -1, -1
	for i, headline := range fake.plans {
		if headline == thinkingHeadline && thinkingAt < 0 {
			thinkingAt = i
		}
		if headline == awaitingApprovalHeadline && waitingAt < 0 {
			waitingAt = i
		}
	}
	if thinkingAt < 0 || waitingAt < thinkingAt {
		t.Fatalf("the headline sequence was %v, want %q to be REPLACED by %q", fake.plans, thinkingHeadline, awaitingApprovalHeadline)
	}
}

// AND THE RUN RESUMING TAKES THE HEADLINE BACK. A decided approval is followed by more model steps, so a
// headline that stuck on "Waiting for your approval" would be the same defect with a different word.
func TestTheHeadlineLeavesTheWaitingStateWhenTheRunResumes(t *testing.T) {
	events := seq([]palai.Event{
		{Type: ApprovalRequestedEventType, Data: map[string]any{"request_hash": "rh_1"}},
		{Type: "model_step.created.v1"},
		{Type: "run.completed.v1"},
	})
	fake := &fakeSlack{}
	if err := Run(context.Background(), Deps{Events: staticStream(events), Slack: fake,
		OnApproval: noApprovals, Delivery: &recordedDelivery{}}, "sess_1", "C1", "1.1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	last := fake.plans[len(fake.plans)-1]
	if last == awaitingApprovalHeadline {
		t.Fatalf("the headline is still %q after the run finished; the sequence was %v", last, fake.plans)
	}
	if !strings.HasPrefix(last, "Done") {
		t.Fatalf("the closing headline reads %q, want the run's own outcome; sequence was %v", last, fake.plans)
	}
}
