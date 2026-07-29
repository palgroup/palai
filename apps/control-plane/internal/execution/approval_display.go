package execution

import (
	"encoding/json"
	"fmt"

	"github.com/palgroup/palai/adapters/integrations/slack"
)

// This file is the APPROVAL SCREEN (E23 T1, plan §2). It answers one question — what does a human read
// before authorizing a tool call — and the answer is deliberately narrow: the identity resolved by the
// lookup that will EXECUTE the call, the one sentence an operator wrote at registration, and every
// argument that will be sent, in a canonical order.
//
// THE DISPLAY IS DERIVED, NEVER STORED, and that is a guarantee rather than a saving. A stored display is
// a second copy of the truth, and a second copy can drift: an argument patched after the row was written,
// a tool re-registered under a different binding, a render cached from a call that is no longer the one
// queued. A derived display cannot drift, because there is nothing to drift FROM — every surface that
// shows the screen recomputes it from the same ledger row, and two recomputations of one row are
// byte-identical.
//
// WHAT IS NOT HERE IS THE POINT. The MCP server's `description` is not here, and neither is its `title`,
// even though the vendor documents both as the human-readable display text. The same page says clients
// "MUST consider tool annotations to be untrusted unless they come from trusted servers"
// (https://modelcontextprotocol.io/specification/2025-11-25/server/tools, fetched 2026-07-29, §3.5 P3/P4)
// — so the two fields recommended FOR display are the two whose author has an interest in the answer. The
// model's prose is not here either, with one exception it is allowed to write: the contents of the
// arguments, because those ARE what it asked for. Hiding them would defeat the gate; §0(b) draws the line
// there deliberately, and the sweep in approval_display_test.go is where it stands rather than in a comment.

// approvalArgumentsLimit bounds the rendered argument block. Slack's table block caps a message at 10,000
// characters (E21 M14) and its markdown block at 12,000 cumulative (M13); 8,000 leaves both a margin. It
// is a RENDER limit, never a truth limit: past it the block is cut and the cut is STATED, because a screen
// that silently drops half a command is the screen this epic exists to prevent.
const approvalArgumentsLimit = 8000

// noOperatorLabel is what an unwritten operator label renders as, literally. A blank line reads as "there
// is nothing more to say"; the honest reading is "nobody wrote one", and the human deciding is entitled to
// know which of the two they are looking at.
const noOperatorLabel = "(no operator label)"

// ToolApprovalDisplay is the three-part screen, each part with its provenance fixed by construction:
// Identity comes from the tool resolution that will run the call, OperatorLabel from a human at
// registration, Arguments from the ledger row's own bytes. Truncated says whether the argument block was
// cut — the surfaces render it as a visible warning (E23 T4's `alert` block), never as silence.
type ToolApprovalDisplay struct {
	Identity      string `json:"identity"`
	OperatorLabel string `json:"operator_label"`
	Arguments     string `json:"arguments"`
	Truncated     bool   `json:"truncated"`
}

// DeriveToolApprovalDisplay computes the screen from a parked call's ledger row.
//
// identity and operatorLabel MUST come from the same resolution the executor uses
// (toolbroker.RequiresApprovalResolved returns both from one lookup, which is why it returns both);
// passing a name off the engine frame instead would let the model name the thing it is asking permission
// for. arguments are the row's committed bytes — the ones Execute will run — not the frame's.
func DeriveToolApprovalDisplay(identity, operatorLabel string, arguments []byte) ToolApprovalDisplay {
	label := operatorLabel
	if label == "" {
		label = noOperatorLabel
	}
	rendered, truncated := renderApprovalArguments(arguments)
	return ToolApprovalDisplay{
		Identity:      slack.NeutralizeBroadcasts(identity),
		OperatorLabel: slack.NeutralizeBroadcasts(label),
		Arguments:     rendered,
		Truncated:     truncated,
	}
}

// renderApprovalArguments canonicalizes the argument bytes: decode, neutralize every string (keys
// included — an object's keys are model-authored exactly like its values), re-encode indented. Go's
// encoding/json sorts map keys at every level, so the canonical order is the standard library's and not a
// hand-rolled one; two renderings of the same call are byte-identical and a diff between them is a real
// difference.
//
// Bytes that are not JSON are shown VERBATIM rather than swallowed. A tool whose arguments the engine did
// not send as an object is a strange call, and a human about to authorize a strange call should see it.
func renderApprovalArguments(arguments []byte) (string, bool) {
	var decoded any
	if err := json.Unmarshal(arguments, &decoded); err != nil {
		return truncateVisibly(slack.NeutralizeBroadcasts(string(arguments)))
	}
	canonical, err := json.MarshalIndent(neutralizeJSON(decoded), "", "  ")
	if err != nil {
		return truncateVisibly(slack.NeutralizeBroadcasts(string(arguments)))
	}
	return truncateVisibly(string(canonical))
}

// neutralizeJSON walks a decoded document applying the Slack broadcast escape to every string leaf and
// every key. It runs here rather than in each renderer because the screen is ONE artefact with one set of
// rules; a per-surface escape is a per-surface omission waiting to happen.
func neutralizeJSON(v any) any {
	switch t := v.(type) {
	case string:
		return slack.NeutralizeBroadcasts(t)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = neutralizeJSON(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[slack.NeutralizeBroadcasts(k)] = neutralizeJSON(e)
		}
		return out
	default:
		return v
	}
}

// truncateVisibly cuts at the render limit and SAYS SO, naming both numbers so a reader can tell how much
// they are not seeing. The full bytes remain in the ledger row; this is a rendering, and it admits it.
func truncateVisibly(s string) (string, bool) {
	if len(s) <= approvalArgumentsLimit {
		return s, false
	}
	return s[:approvalArgumentsLimit] + fmt.Sprintf("\n… truncated: %d of %d bytes shown; the full arguments are on the tool call this button is bound to",
		approvalArgumentsLimit, len(s)), true
}
