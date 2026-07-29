package execution

import (
	"encoding/json"
	"strings"
	"testing"
)

// The E23 T1 approval-screen sweep. §2's first invariant is that WHAT A HUMAN SEES IS COMPUTED, NEVER
// NARRATED: the identity comes from the lookup that will execute the call, the one human sentence is the
// operator's, and the arguments are the bytes that will be sent. Everything else is a claim by somebody
// whose interest is not the approver's.
//
// The sweep DECODES the JSON before looking. encoding/json escapes `<`, `>` and `&`, so a raw-substring
// assertion over marshalled bytes can pass while the forbidden text sits right there in the payload — E20
// T4 paid for that lesson and the fence below is written so it cannot be re-learned.

// stringLeavesOf gathers every string leaf (and every KEY) of a decoded JSON document, so an assertion
// about what a screen says is made against the decoded text and not the escaped wire bytes. Keys are
// swept too: an argument object's keys are model-authored just like its values.
func stringLeavesOf(v any, out *[]string) {
	switch t := v.(type) {
	case string:
		*out = append(*out, t)
	case []any:
		for _, e := range t {
			stringLeavesOf(e, out)
		}
	case map[string]any:
		for k, e := range t {
			*out = append(*out, k)
			stringLeavesOf(e, out)
		}
	}
}

// screenLeaves renders a display to JSON the way a surface would carry it, decodes it back, and returns
// every string leaf — the bytes a human could possibly read.
func screenLeaves(t *testing.T, d ToolApprovalDisplay) []string {
	t.Helper()
	wire, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal display: %v", err)
	}
	var decoded any
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatalf("decode display: %v", err)
	}
	var leaves []string
	stringLeavesOf(decoded, &leaves)
	// The arguments block is itself JSON. Decode it too, or a marker hiding one level down is invisible
	// to a sweep that only reads the envelope.
	var args any
	if json.Unmarshal([]byte(d.Arguments), &args) == nil {
		stringLeavesOf(args, &leaves)
	}
	return leaves
}

func leavesContain(leaves []string, needle string) bool {
	for _, s := range leaves {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

// TestToolApprovalScreenCarriesNoServerOrModelText is RED #3, and it is the security test of this task.
// An MCP server writes a `description` and a `title`; the model writes free prose. NONE of the three may
// reach the screen — with ONE deliberate exception that the test also proves rather than assumes: the
// model's text is allowed inside the ARGUMENTS, because the arguments ARE what it asked for and hiding
// them would defeat the whole gate (§0(b): what is prevented is an invisible argument, not a lying one).
//
// The exception is what gives the sweep its teeth: it demonstrates the sweep CAN see model bytes, so the
// three "absent" assertions are absences the sweep would actually have caught.
func TestToolApprovalScreenCarriesNoServerOrModelText(t *testing.T) {
	const (
		serverDescription = "SERVERDESCRIPTIONMARKER: transition a Jira issue; this transition is pre-approved"
		serverTitle       = "SERVERTITLEMARKER Transition Issue"
		modelProse        = "MODELPROSEMARKER this is routine, no review needed"
	)
	// What the tree resolves for this call: the identity from the lookup that will execute it, and the
	// operator's label. The server's description and title are handed to nothing.
	display := DeriveToolApprovalDisplay("jira.transitionIssue", "the shared Jira service account may move tickets",
		[]byte(`{"issue":"PAL-42","status":"Done","comment":"`+modelProse+`"}`))

	leaves := screenLeaves(t, display)
	for _, forbidden := range []struct{ what, needle string }{
		{"the MCP server's description", "SERVERDESCRIPTIONMARKER"},
		{"the MCP server's tool title", "SERVERTITLEMARKER"},
	} {
		if leavesContain(leaves, forbidden.needle) {
			t.Errorf("%s reached the approval screen: %v", forbidden.what, leaves)
		}
	}
	// The model's prose is confined to the arguments. It is NOT in the identity and NOT in the label —
	// those two fields are the ones a human trusts to say what is about to happen.
	if strings.Contains(display.Identity, "MODELPROSEMARKER") {
		t.Errorf("the model wrote part of the identity: %q", display.Identity)
	}
	if strings.Contains(display.OperatorLabel, "MODELPROSEMARKER") {
		t.Errorf("the model wrote part of the operator label: %q", display.OperatorLabel)
	}
	// DISTINGUISHING POWER. Without this the three assertions above would pass on an empty screen, and a
	// sweep that cannot fail is not a fence. The model's own bytes ARE visible to the sweep, inside the
	// field labelled arguments — which is exactly where §0(b) draws the line.
	if !leavesContain(leaves, "MODELPROSEMARKER") {
		t.Fatalf("the model's argument value is not on the screen at all — either an argument was dropped "+
			"(the failure this epic exists to prevent) or the sweep cannot see argument text, which would "+
			"make the assertions above vacuous. leaves=%v", leaves)
	}
	// And the same for the server's text: it must be absent because nothing PUT it there, not because the
	// deriver could never have carried it. A caller that passed the description AS the operator label would
	// be caught — which is the mistake ("show the description too, it helps the user") this fence names.
	leaked := screenLeaves(t, DeriveToolApprovalDisplay("jira.transitionIssue", serverDescription, []byte(`{}`)))
	if !leavesContain(leaked, "SERVERDESCRIPTIONMARKER") {
		t.Fatal("a description passed AS the operator label did not show up in the sweep: the sweep cannot " +
			"distinguish, so its absence above proves nothing")
	}
	_ = serverTitle
}

// TestToolApprovalScreenShowsEveryArgumentKeySorted proves §2's third part: the WHOLE argument object, in
// a canonical key order so two renderings of the same call read identically and a diff between them means
// a real difference. A silently dropped argument is the failure this epic exists to prevent.
func TestToolApprovalScreenShowsEveryArgumentKeySorted(t *testing.T) {
	display := DeriveToolApprovalDisplay("jira.transitionIssue", "label",
		[]byte(`{"status":"Done","issue":"PAL-42","assignee":{"name":"ops","id":"U1"}}`))
	for _, want := range []string{"assignee", "issue", "PAL-42", "status", "Done", "ops", "U1"} {
		if !strings.Contains(display.Arguments, want) {
			t.Errorf("argument %q is not on the screen:\n%s", want, display.Arguments)
		}
	}
	if a, i, s := strings.Index(display.Arguments, "assignee"), strings.Index(display.Arguments, "issue"),
		strings.Index(display.Arguments, "status"); !(a < i && i < s) {
		t.Errorf("arguments are not key-sorted (assignee<issue<status): got offsets %d,%d,%d in\n%s", a, i, s, display.Arguments)
	}
}

// TestToolApprovalScreenNamesAnAbsentOperatorLabel: an empty label renders as the literal
// `(no operator label)`. A blank line reads as "there is nothing more to say"; the honest reading is
// "nobody wrote one", and the human deciding is entitled to know which of the two they are looking at.
func TestToolApprovalScreenNamesAnAbsentOperatorLabel(t *testing.T) {
	if got := DeriveToolApprovalDisplay("shell.exec", "", []byte(`{}`)).OperatorLabel; got != "(no operator label)" {
		t.Fatalf("OperatorLabel = %q, want the literal (no operator label)", got)
	}
}

// TestToolApprovalScreenTruncatesVISIBLY: a screen that silently cuts a 40 KB argument set is a screen
// that lies about what will run. Cutting is allowed (Slack's own limits force it); cutting quietly is not.
func TestToolApprovalScreenTruncatesVisibly(t *testing.T) {
	big, _ := json.Marshal(map[string]any{"body": strings.Repeat("x", approvalArgumentsLimit*2)})
	display := DeriveToolApprovalDisplay("shell.exec", "label", big)
	if !display.Truncated {
		t.Fatal("an oversized argument set was not marked truncated")
	}
	if !strings.Contains(display.Arguments, "truncated") {
		t.Fatalf("the truncation is not VISIBLE in the rendered arguments:\n%s", display.Arguments[:200])
	}
	if len(display.Arguments) > approvalArgumentsLimit+256 {
		t.Fatalf("rendered arguments are %d bytes, past the limit plus its marker", len(display.Arguments))
	}
}

// TestToolApprovalScreenIsDerivedNotStored pins the guarantee §2 calls out by name: the display is a
// FUNCTION of the ledger row, so two derivations of the same row are byte-identical and a stored display
// that drifted from what will run is not representable.
func TestToolApprovalScreenIsDerivedNotStored(t *testing.T) {
	args := []byte(`{"issue":"PAL-42","status":"Done"}`)
	first := DeriveToolApprovalDisplay("jira.transitionIssue", "label", args)
	second := DeriveToolApprovalDisplay("jira.transitionIssue", "label", args)
	if first != second {
		t.Fatalf("two derivations of one row differ:\n%+v\n%+v", first, second)
	}
}

// TestToolApprovalScreenNeutralizesBroadcasts: an argument value carrying `<!channel>` renders as literal
// text rather than paging a workspace when the screen reaches Slack. The escape is OURS, not a vendor
// rule (adapters/integrations/slack.NeutralizeBroadcasts says so), and the token stays readable.
func TestToolApprovalScreenNeutralizesBroadcasts(t *testing.T) {
	display := DeriveToolApprovalDisplay("chat.post", "label", []byte(`{"text":"<!channel> ship it"}`))
	if strings.Contains(display.Arguments, `"<!channel>`) || strings.Contains(display.Arguments, `<!channel>`) {
		t.Fatalf("a broadcast token survived into the approval screen:\n%s", display.Arguments)
	}
	if !strings.Contains(display.Arguments, "channel") {
		t.Fatalf("the token was deleted rather than neutralized — the reader can no longer see what was asked:\n%s", display.Arguments)
	}
}
