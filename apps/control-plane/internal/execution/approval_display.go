package execution

import (
	"github.com/palgroup/palai/adapters/integrations/slack"
)

// The APPROVAL SCREEN (E23 T1, plan §2), which now lives one package over and is REACHED from here rather
// than duplicated here. E23 T4 moved the body into adapters/integrations/slack/approval_display.go for a
// structural reason and not a stylistic one: the modal that renders the same screen is opened by the Slack
// interactivity path in `extensions`, and `execution` imports `extensions`, so `extensions` can never
// import back. A display that lived here was reachable from exactly one of the two surfaces that need it,
// and the alternative — a second canonicaliser in the adapter — is the drift the original file's own
// comment refuses ("two recomputations of one row are byte-identical").
//
// So this stays the name the executor calls, and there is still exactly ONE implementation. Everything the
// original said about WHAT IS NOT ON THE SCREEN — no MCP `description`, no `title`, no model prose outside
// the arguments — is stated at that implementation, next to the code that enforces it.

// ToolApprovalDisplay is an alias rather than a wrapper struct: an alias keeps every field, every JSON tag
// and every existing `display.Arguments` untouched, so nothing downstream had to change to follow the move.
type ToolApprovalDisplay = slack.ApprovalDisplay

// approvalArgumentsLimit keeps this package's name for the render budget, so the tests that measure a cut
// still read in this package's vocabulary.
const approvalArgumentsLimit = slack.MaxApprovalArguments

// DeriveToolApprovalDisplay computes the screen from a parked call's ledger row. identity and
// operatorLabel MUST come from the same resolution the executor uses (RequiresApprovalResolved returns
// both from one lookup, which is why it returns both), and arguments are the row's committed bytes — the
// ones Execute will run, never the frame's.
func DeriveToolApprovalDisplay(identity, operatorLabel string, arguments []byte) ToolApprovalDisplay {
	return slack.DeriveApprovalDisplay(identity, operatorLabel, arguments)
}
