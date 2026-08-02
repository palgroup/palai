//go:build component

package execution

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/palgroup/palai/storage"
)

// THE TOOL FRAME CARRIES ITS NAME, AND THE LEDGER CARRIES THE REST (E30 T2).
//
// Before this, `tool_call.executing.v1` carried {run_id, tool_call_id, replay_class} and
// `tool_call.completed.v1` carried {run_id, tool_call_id}. A client could see THAT a tool ran and never
// WHAT — so a chat rendering an iOS build had nothing to label the call with, and the demo's adapter
// said so honestly by drawing `nameUnavailable: true`.
//
// The split these tests pin: the NAME goes on the frame (short, and from the closed set of tools the
// deployment registered), the ARGUMENTS and RESULT stay on the ledger and are read back. The reason is
// measured and lives at the emitter — an event payload is POSTed to every registered webhook endpoint
// and stored immutably per delivery, and a trivial `xcodebuild` build is 51,422 bytes.

// journalPayloads returns the payloads of a session's events of one type, oldest first.
func journalPayloads(t *testing.T, h *approvalHarness, eventType string) []map[string]any {
	t.Helper()
	rows, err := h.spine.Pool().Query(storage.WithSystemScope(context.Background()),
		`SELECT payload FROM events WHERE session_id = $1 AND type = $2 ORDER BY seq`, h.sessionID, eventType)
	if err != nil {
		t.Fatalf("read %s events: %v", eventType, err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan event payload: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("decode event payload: %v", err)
		}
		out = append(out, m)
	}
	return out
}

// TestToolFrameCarriesTheToolName drives a real dispatch and reads the JOURNAL, because the journal is
// what a client actually receives — asserting on a Go value here would prove the dispatcher knew the
// name, which was never in doubt, rather than that it wrote it down.
func TestToolFrameCarriesTheToolName(t *testing.T) {
	h := newApprovalHarness(t, gatedTool)
	armSession(t, h, "key:operator-anna", true, false)

	if _, err := h.dispatch(t, 1, map[string]any{"command": "xcodebuild -list"}); err != nil {
		t.Fatalf("dispatchTool error = %v", err)
	}

	executing := journalPayloads(t, h, "tool_call.executing.v1")
	if len(executing) != 1 {
		t.Fatalf("got %d tool_call.executing.v1 frame(s), want 1", len(executing))
	}
	if got := executing[0]["tool_name"]; got != "jira.transitionIssue" {
		t.Fatalf("tool_call.executing.v1 tool_name = %v, want the tool's name — a frame that omits it "+
			"leaves every renderer labelling the call \"tool\"", got)
	}

	completed := journalPayloads(t, h, "tool_call.completed.v1")
	if len(completed) != 1 {
		t.Fatalf("got %d tool_call.completed.v1 frame(s), want 1", len(completed))
	}
	if got := completed[0]["tool_name"]; got != "jira.transitionIssue" {
		t.Fatalf("tool_call.completed.v1 tool_name = %v, want the tool's name", got)
	}
}

// TestToolFrameCarriesNeitherArgumentsNorResult is the OTHER half, and it is the one that stops this
// from drifting into "put everything on the frame" the next time somebody wants a field.
//
// The measurement behind it: automation/webhook_pump.go:328 puts an event's whole payload in the body it
// POSTs to every registered endpoint and stores that envelope immutably for byte-for-byte redelivery,
// and nothing in the coordinator bounds a payload. A tool's arguments and result are model-authored and
// unbounded — a trivial `xcodebuild` build measured 51,422 bytes — so they must not be here.
func TestToolFrameCarriesNeitherArgumentsNorResult(t *testing.T) {
	h := newApprovalHarness(t, gatedTool)
	armSession(t, h, "key:operator-anna", true, false)

	secret := "SHOULD-NEVER-REACH-THE-JOURNAL"
	if _, err := h.dispatch(t, 1, map[string]any{"command": secret}); err != nil {
		t.Fatalf("dispatchTool error = %v", err)
	}

	for _, eventType := range []string{"tool_call.executing.v1", "tool_call.completed.v1"} {
		for _, payload := range journalPayloads(t, h, eventType) {
			if _, ok := payload["arguments"]; ok {
				t.Fatalf("%s carries `arguments`: model-authored bytes now reach every registered webhook "+
					"endpoint and are stored immutably per delivery", eventType)
			}
			if _, ok := payload["result"]; ok {
				t.Fatalf("%s carries `result`: an unbounded tool output (a trivial xcodebuild build is "+
					"51,422 bytes) now fans out to every endpoint and is stored per delivery", eventType)
			}
			// And the property rather than the field name, so a differently-spelled field cannot smuggle
			// the same bytes past this guard.
			raw, _ := json.Marshal(payload)
			if containsSubstring(string(raw), secret) {
				t.Fatalf("%s payload contains the call's own argument text: %s", eventType, raw)
			}
		}
	}
}

// TestToolCallLedgerReadReturnsWhatTheFrameOmits closes the loop: the bytes the frame does not carry
// must be reachable, or the split above is just a deletion. This is the read the chat renders an
// `xcodebuild` failure from.
func TestToolCallLedgerReadReturnsWhatTheFrameOmits(t *testing.T) {
	h := newApprovalHarness(t, gatedTool)
	armSession(t, h, "key:operator-anna", true, false)
	respID := seedResponseForRun(t, h)

	args := map[string]any{"command": "xcodebuild -scheme Demo -destination 'platform=iOS Simulator,name=iPhone 17 Pro' build"}
	if _, err := h.dispatch(t, 1, args); err != nil {
		t.Fatalf("dispatchTool error = %v", err)
	}

	views, err := h.spine.ToolCallsForResponse(context.Background(), h.tenant, respID)
	if err != nil {
		t.Fatalf("ToolCallsForResponse() error = %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("the ledger read returned %d tool call(s), want 1", len(views))
	}
	v := views[0]
	if v.Name != "jira.transitionIssue" {
		t.Fatalf("tool call name = %q, want the registered name", v.Name)
	}
	if v.State != "completed" {
		t.Fatalf("tool call state = %q, want completed", v.State)
	}
	// The ARGUMENTS come back — this is the whole point, and it is what lets a chat print the command
	// line that produced a build failure.
	var gotArgs map[string]any
	if err := json.Unmarshal(v.Arguments, &gotArgs); err != nil {
		t.Fatalf("decode ledger arguments: %v", err)
	}
	if gotArgs["command"] != args["command"] {
		t.Fatalf("ledger arguments = %v, want the dispatched command verbatim", gotArgs)
	}
	if len(v.Result) == 0 {
		t.Fatal("the ledger read returned no result for a COMPLETED call — a chat has nothing to render")
	}
}

func containsSubstring(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
