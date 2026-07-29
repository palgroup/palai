package slack

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// This file is the APPROVAL SCREEN (E23 T1, plan §2). It answers one question — what does a human read
// before authorizing a tool call — and the answer is deliberately narrow: the identity resolved by the
// lookup that will EXECUTE the call, the one sentence an operator wrote at registration, and every
// argument that will be sent, in a canonical order.
//
// IT LIVES IN THIS PACKAGE, and E23 T4 moved it here for a reason worth writing down: the two surfaces
// that render an approval — the message a run's own path posts (execution) and the modal an interactivity
// click opens (extensions) — sit on OPPOSITE sides of an import edge. `execution` imports `extensions`,
// so `extensions` cannot import `execution`, and a display living there was reachable from only one of
// the two. The alternative was a second canonicaliser in the Slack adapter, which is exactly the drift
// the paragraph below refuses. `execution.DeriveToolApprovalDisplay` is now a one-line delegation to
// this function, so there is still exactly ONE of it.
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
// there deliberately, and the sweeps in approval_display_test.go and approval_widgets_test.go are where
// it stands rather than in a comment.

// MaxApprovalArguments bounds the rendered argument block. Slack's table block caps a message at 10,000
// characters (E21 M14) and its markdown block at 12,000 cumulative (M13); 8,000 leaves both a margin. It
// is a RENDER limit, never a truth limit: past it the block is cut and the cut is STATED, because a screen
// that silently drops half a command is the screen this epic exists to prevent.
const MaxApprovalArguments = 8000

// NoOperatorLabel is what an unwritten operator label renders as, literally. A blank line reads as "there
// is nothing more to say"; the honest reading is "nobody wrote one", and the human deciding is entitled to
// know which of the two they are looking at.
const NoOperatorLabel = "(no operator label)"

// ApprovalDisplay is the three-part screen, each part with its provenance fixed by construction:
// Identity comes from the tool resolution that will run the call, OperatorLabel from a human at
// registration, Arguments from the ledger row's own bytes. Truncated says whether the argument block was
// cut — the surfaces render it as a visible warning (E23 T4's `alert` block), never as silence.
type ApprovalDisplay struct {
	Identity      string `json:"identity"`
	OperatorLabel string `json:"operator_label"`
	Arguments     string `json:"arguments"`
	Truncated     bool   `json:"truncated"`
}

// DeriveApprovalDisplay computes the screen from a parked call's ledger row.
//
// identity and operatorLabel MUST come from the same resolution the executor uses
// (toolbroker.RequiresApprovalResolved returns both from one lookup, which is why it returns both);
// passing a name off the engine frame instead would let the model name the thing it is asking permission
// for. arguments are the row's committed bytes — the ones Execute will run — not the frame's.
func DeriveApprovalDisplay(identity, operatorLabel string, arguments []byte) ApprovalDisplay {
	label := operatorLabel
	if label == "" {
		label = NoOperatorLabel
	}
	rendered, truncated := renderApprovalArguments(arguments)
	return ApprovalDisplay{
		Identity:      NeutralizeBroadcasts(identity),
		OperatorLabel: NeutralizeBroadcasts(label),
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
	decoded, err := decodeApprovalArguments(arguments)
	if err != nil {
		return truncateVisibly(NeutralizeBroadcasts(string(arguments)))
	}
	canonical, err := encodeCanonicalJSON(neutralizeJSON(decoded), "  ")
	if err != nil {
		return truncateVisibly(NeutralizeBroadcasts(string(arguments)))
	}
	return truncateVisibly(canonical)
}

// encodeCanonicalJSON is json.Marshal WITH HTML ESCAPING OFF, and the difference is visible to a human, so
// it is not a preference.
//
// MEASURED (E23 T4): encoding/json escapes `<`, `>` and `&` to <, > and & by default. The
// broadcast defence has already turned `<!channel>` into `&lt;!channel>` by the time a value is encoded,
// so the default escape turns that into the literal characters `&lt;!channel>` — and this string
// is then marshalled AGAIN as a field of the outbound body, which escapes nothing further because the
// backslashes are now ordinary text. Slack therefore renders `&lt;` to the reader. Nothing is unsafe
// (the token is still defused) but the human is reading gibberish where the model wrote a word, on the one
// screen in this system whose whole job is to be read exactly. The outer marshal does the one escape the
// wire needs; this one must not do a second.
func encodeCanonicalJSON(v any, indent string) (string, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if indent != "" {
		encoder.SetIndent("", indent)
	}
	if err := encoder.Encode(v); err != nil {
		return "", err
	}
	// Encode appends a newline that Marshal does not; the canonical form is the document, not the document
	// plus a blank line, and two renderings have to be byte-identical.
	return strings.TrimSuffix(buf.String(), "\n"), nil
}

// decodeApprovalArguments is the ONE decode both the canonical dump and the argument table go through,
// and it uses json.Number rather than float64 deliberately: a screen that turns 10000000000 into 1e+10,
// or 1.0 into 1, is showing a human bytes that are not the bytes about to be sent. UseNumber keeps the
// literal exactly as the engine wrote it.
//
// Trailing bytes are an ERROR, matching what json.Unmarshal did before the decoder replaced it: a body
// that is one document plus something else is not a document, and it falls to the verbatim render where a
// human can see whatever it actually is.
func decodeApprovalArguments(arguments []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(arguments))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, err
	}
	if decoder.More() {
		return nil, errTrailingArgumentBytes
	}
	return decoded, nil
}

var errTrailingArgumentBytes = errors.New("slack: the argument bytes carry more than one JSON document")

// neutralizeJSON walks a decoded document applying the Slack broadcast escape to every string leaf and
// every key. It runs here rather than in each renderer because the screen is ONE artefact with one set of
// rules; a per-surface escape is a per-surface omission waiting to happen.
func neutralizeJSON(v any) any {
	switch t := v.(type) {
	case string:
		return NeutralizeBroadcasts(t)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = neutralizeJSON(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[NeutralizeBroadcasts(k)] = neutralizeJSON(e)
		}
		return out
	default:
		return v
	}
}

// truncateVisibly cuts at the render limit and SAYS SO, naming both numbers so a reader can tell how much
// they are not seeing. The full bytes remain in the ledger row; this is a rendering, and it admits it.
func truncateVisibly(s string) (string, bool) {
	if len(s) <= MaxApprovalArguments {
		return s, false
	}
	return s[:MaxApprovalArguments] + fmt.Sprintf("\n… truncated: %d of %d bytes shown; the full arguments are on the tool call this button is bound to",
		MaxApprovalArguments, len(s)), true
}
