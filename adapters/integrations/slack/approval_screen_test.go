package slack

import (
	"encoding/json"
	"strings"
	"testing"
)

// THE SCREEN A HUMAN ACTUALLY MET, 2026-08-04. This deployment's first real gated call was a publication,
// and a publication's schema is `"properties": {}` — so the operator was shown a table headed
// `argument | value` with NO ROWS, a "Show arguments" button over that same nothing, and one unlabelled
// line reading `push agent/ws_986c…/run_e403… @ 8126e83cc… -> …centauri-ios.git`.
//
// Every assertion below is about that message. They are separate tests rather than one, because the three
// defects are independent and a single test would go green the moment any one of them was fixed.

// argumentlessCall is the fixture the whole file turns on: a call whose arguments are a valid, EMPTY JSON
// object. Not malformed, not absent — empty, which is what every publication sends.
func argumentlessRequest() ApprovalRequest {
	return ApprovalRequest{
		ApprovalID:  "apr_pub",
		RequestHash: "req_9",
		Identity:    "push_branch",
		Arguments:   []byte(`{}`),
	}
}

// blockTypesOf lists the block types of a decoded message body, in order — the coarsest reading of the
// screen there is, and the one that catches a block being drawn at all.
func blockTypesOf(t *testing.T, raw []byte) []string {
	t.Helper()
	var body struct {
		Blocks []map[string]any `json:"blocks"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode the approval body: %v", err)
	}
	out := make([]string, 0, len(body.Blocks))
	for _, b := range body.Blocks {
		kind, _ := b["type"].(string)
		out = append(out, kind)
	}
	return out
}

// actionIDsOf lists the action_id of every button on the screen.
func actionIDsOf(t *testing.T, raw []byte) []string {
	t.Helper()
	var body struct {
		Blocks []struct {
			Type     string `json:"type"`
			Elements []struct {
				ActionID string `json:"action_id"`
			} `json:"elements"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode the approval body: %v", err)
	}
	var out []string
	for _, b := range body.Blocks {
		if b.Type != "actions" {
			continue
		}
		for _, e := range b.Elements {
			out = append(out, e.ActionID)
		}
	}
	return out
}

// DEFECT 1a — a call with no arguments must not draw a table that lists nothing.
func TestACallWithNoArgumentsDrawsNoArgumentTable(t *testing.T) {
	body := ToolApprovalMessage("C1", "1.1", argumentlessRequest())
	for _, kind := range blockTypesOf(t, body) {
		if kind == "table" {
			t.Fatalf("an argument-less call still drew a table; the cells were %q", tableCells(t, body))
		}
	}
}

// THE SECOND GUARD, ASSERTED DIRECTLY, and this test exists because of what a perturbation did NOT do.
//
// Deleting approvalArgumentTable's own empty-set refusal left every test above GREEN — correctly, because
// the property never broke: ToolApprovalMessage guards at the call site too, and a probe against the
// perturbed build confirmed the empty table still did not appear. Two independent guards, which is the
// right shape for a screen this one; the tree's own rule says a green perturbation means the test is
// innocent whenever the PROPERTY held, and weakening the design to make one bite is the harm that rule
// warns about.
//
// What it did reveal is that the inner guard was UNWITNESSED — a future refactor could delete it silently
// and nothing would say so until the day a caller forgot its own check. So it is asserted here, at the
// level it lives, rather than made load-bearing at a level it does not.
func TestTheArgumentTableHelperRefusesToDrawAHeaderOverNothing(t *testing.T) {
	if got := approvalArgumentTable("argument", "value", nil); got != nil {
		t.Fatalf("an empty set produced %#v, want no block at all", got)
	}
	if got := approvalArgumentTable("what", "value", []ApprovalArgument{}); got != nil {
		t.Fatalf("an empty (non-nil) set produced %#v, want no block at all", got)
	}
	if got := approvalArgumentTable("what", "value", []ApprovalArgument{{Name: "repository", Value: "r"}}); got == nil {
		t.Fatal("a populated set produced no block")
	}
}

// DEFECT 1b — and it must SAY there are none, rather than leaving a gap.
//
// The two halves are separate assertions because hiding the block and reporting the absence are different
// decisions, and only one of them is right (see NoArguments): a screen that silently omitted the table
// would pass the test above and still leave its reader unable to tell "sends nothing" from "failed to
// render".
func TestACallWithNoArgumentsSaysSoOutLoud(t *testing.T) {
	body := ToolApprovalMessage("C1", "1.1", argumentlessRequest())
	if !strings.Contains(strings.Join(decodedStrings(t, body), "\n"), NoArguments) {
		t.Fatalf("the screen never says the call carries no arguments; it said:\n%s",
			strings.Join(decodedStrings(t, body), "\n"))
	}
}

// DEFECT 1c — and it must not offer a button whose only job is to show them.
func TestACallWithNoArgumentsOffersNoShowArgumentsButton(t *testing.T) {
	ids := actionIDsOf(t, ToolApprovalMessage("C1", "1.1", argumentlessRequest()))
	for _, id := range ids {
		if id == ActionShowArguments {
			t.Fatalf("the screen offers %q over an empty document; buttons were %v", ActionShowArguments, ids)
		}
	}
	// The two that DO decide something are still there — the point is a narrower screen, not a broken one.
	if len(ids) != 2 || ids[0] != ActionApprove || ids[1] != ActionDeny {
		t.Fatalf("the decision buttons were %v, want exactly [%s %s]", ids, ActionApprove, ActionDeny)
	}
}

// A CALL THAT HAS ARGUMENTS IS UNCHANGED, and this is the assertion that lets the three above be safe: six
// released manifests pin this renderer through fixtures that all pass non-empty arguments, so the rules
// only fire where nothing was ever rendered before.
func TestACallWithArgumentsStillDrawsItsTableAndAllThreeButtons(t *testing.T) {
	body := ToolApprovalMessage("C1", "1.1", gatedRequest())
	var tables int
	for _, kind := range blockTypesOf(t, body) {
		if kind == "table" {
			tables++
		}
	}
	if tables != 1 {
		t.Fatalf("a call with arguments drew %d table(s), want 1", tables)
	}
	ids := actionIDsOf(t, body)
	if len(ids) != 3 || ids[2] != ActionShowArguments {
		t.Fatalf("the buttons were %v, want approve/deny/show-arguments", ids)
	}
	if !strings.Contains(strings.Join(decodedStrings(t, body), "\n"), "PAL-42") {
		t.Fatal("the arguments no longer reach the screen")
	}
}

// DEFECT 2 — the destination is drawn as LABELLED ROWS, under its own header.
//
// The header matters as much as the rows: `argument | value` would file a run's chosen branch under the
// word "argument" on the one screen whose job is to be exact about where a value came from.
func TestADestinationIsDrawnUnderItsOwnHeader(t *testing.T) {
	req := argumentlessRequest()
	req.Destination = []ApprovalArgument{
		{Name: "repository", Value: "https://github.com/palgroup/centauri-ios.git"},
		{Name: "branch", Value: "agent/ws_986c86ee5a07915f75cd99ebf33d422a/run_e4036140a36cba72b245b7f04201cc66"},
		{Name: "commit", Value: "8126e83cc544d6777b0c93d4543e26cb1c7cebdb"},
	}
	cells := tableCells(t, ToolApprovalMessage("C1", "1.1", req))
	if len(cells) < 2 || cells[0] != "what" || cells[1] != "value" {
		t.Fatalf("the destination table is headed %v, want [what value]", cells[:min(2, len(cells))])
	}
	joined := strings.Join(cells, "\n")
	for _, want := range []string{"repository", "branch", "commit"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("the destination table has no %q row; cells were %q", want, cells)
		}
	}
	// NOTHING IS SHORTENED. An approval screen that abbreviated the branch or the sha would be describing
	// something other than what the button authorizes.
	for _, want := range []string{
		"agent/ws_986c86ee5a07915f75cd99ebf33d422a/run_e4036140a36cba72b245b7f04201cc66",
		"8126e83cc544d6777b0c93d4543e26cb1c7cebdb",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("%q does not appear in full on the screen; cells were %q", want, cells)
		}
	}
}

// A DESTINATION CELL IS DEFUSED LIKE EVERY OTHER CELL. This matters more here than for an argument: a
// publication's columns reach GET /v1/approvals RAW (store/approvals.go's publication branch passes
// p.Display and friends straight through), and a branch name is a string the RUN chose — `<!channel>` is a
// valid git ref.
func TestADestinationCellCannotBroadcast(t *testing.T) {
	req := argumentlessRequest()
	req.Destination = []ApprovalArgument{{Name: "branch", Value: "agent/<!channel>/run_1"}}
	for _, s := range decodedStrings(t, ToolApprovalMessage("C1", "1.1", req)) {
		if strings.Contains(s, "<!channel>") {
			t.Fatalf("a live broadcast token reached the screen: %q", s)
		}
	}
}

// WITH A DESTINATION AND NO ARGUMENTS, BOTH RULES HOLD AT ONCE: one table (the destination), the sentence
// saying there are no arguments, and two buttons. This is the exact shape of the message the operator
// complained about, and it is the one a fix could most easily get half right.
func TestThePublicationShapeDrawsTheDestinationAndSaysThereAreNoArguments(t *testing.T) {
	req := argumentlessRequest()
	req.Destination = []ApprovalArgument{{Name: "repository", Value: "https://github.com/palgroup/centauri-ios.git"}}
	body := ToolApprovalMessage("C1", "1.1", req)

	var tables int
	for _, kind := range blockTypesOf(t, body) {
		if kind == "table" {
			tables++
		}
	}
	if tables != 1 {
		t.Fatalf("drew %d table(s), want exactly 1 (the destination)", tables)
	}
	if cells := tableCells(t, body); len(cells) == 0 || cells[0] != "what" {
		t.Fatalf("the one table is not the destination table; cells were %q", cells)
	}
	if !strings.Contains(strings.Join(decodedStrings(t, body), "\n"), NoArguments) {
		t.Fatal("the screen does not say the call carries no arguments")
	}
	if ids := actionIDsOf(t, body); len(ids) != 2 {
		t.Fatalf("the buttons were %v, want just approve and deny", ids)
	}
}
