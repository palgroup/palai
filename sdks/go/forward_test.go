package palai

import (
	"encoding/json"
	"testing"
)

// The forward-compatibility edge the shared corpus targets for a struct language: a struct decode
// must PRESERVE an unknown field a newer server adds, where a naive plain-struct decode silently
// strips it. These tests lock that guarantee at the unit level (the corpus locks it cross-language).

func TestResponsePreservesUnknownFields(t *testing.T) {
	raw := `{"id":"resp_1","object":"response","status":"completed","created_at":"2026-07-18T00:00:00Z",` +
		`"model":"fake-1","output":[{"type":"output_text","text":"done"}],` +
		`"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8},` +
		`"x_experimental_scoring":{"confidence":0.9}}`

	var r Response
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Known fields are typed.
	if r.ID != "resp_1" || r.Status != "completed" || r.Model != "fake-1" {
		t.Fatalf("typed fields wrong: %+v", r)
	}
	if r.Usage == nil || r.Usage.InputTokens != 5 || r.Usage.TotalTokens != 8 {
		t.Fatalf("usage wrong: %+v", r.Usage)
	}
	// The unknown field survives a round-trip.
	out, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	scoring, ok := got["x_experimental_scoring"].(map[string]any)
	if !ok || scoring["confidence"] != 0.9 {
		t.Fatalf("unknown field NOT preserved on round-trip: %s", out)
	}
	// And no key was invented or lost.
	if len(got) != 8 {
		t.Fatalf("expected 8 keys after round-trip, got %d: %s", len(got), out)
	}
}

// TestNaiveStructWouldStrip proves the edge is real, not hypothetical: a plain struct with only the
// known fields drops the unknown one — which is exactly why the lossless Response type exists and
// why the corpus is load-bearing for a struct language.
func TestNaiveStructWouldStrip(t *testing.T) {
	raw := `{"id":"resp_1","object":"response","status":"completed","x_experimental_scoring":{"confidence":0.9}}`
	type naive struct {
		ID     string `json:"id"`
		Object string `json:"object"`
		Status string `json:"status"`
	}
	var n naive
	if err := json.Unmarshal([]byte(raw), &n); err != nil {
		t.Fatalf("decode: %v", err)
	}
	out, _ := json.Marshal(n)
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	if _, present := got["x_experimental_scoring"]; present {
		t.Fatal("a plain struct unexpectedly kept the unknown field — the negative control is broken")
	}
	// The lossless type keeps it where the naive one drops it.
	var r Response
	_ = json.Unmarshal([]byte(raw), &r)
	lossless, _ := json.Marshal(r)
	var keep map[string]any
	_ = json.Unmarshal(lossless, &keep)
	if _, present := keep["x_experimental_scoring"]; !present {
		t.Fatal("Response dropped the unknown field the naive struct also dropped")
	}
}

func TestEventPreservesUnknownEventTypeAndField(t *testing.T) {
	raw := `{"specversion":"1.0","id":"e9","source":"palai","type":"some.brand.new.v9",` +
		`"time":"2026-07-18T00:00:09Z","sequence":9,"data":{"ok":true},"x_future":"preserved"}`
	var e Event
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if e.Type != "some.brand.new.v9" { // an unknown event type is delivered, not rejected
		t.Fatalf("unknown event type not preserved: %q", e.Type)
	}
	out, _ := json.Marshal(e)
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	if got["x_future"] != "preserved" {
		t.Fatalf("unknown event field not preserved: %s", out)
	}
}

// TestSessionPreservesUnknownFields locks Session's Unmarshal/MarshalJSON pair (sessions.go) to the
// same edge Response and Event are already held to: an unknown field survives a decode→encode
// round-trip rather than a copy-paste slip in the type-alias trick silently stripping it.
func TestSessionPreservesUnknownFields(t *testing.T) {
	raw := `{"id":"sess_1","object":"session","status":"open","x_future_field":{"kept":true}}`

	var s Session
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if s.ID != "sess_1" || s.Object != "session" || s.Status != "open" {
		t.Fatalf("typed fields wrong: %+v", s)
	}
	out, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	future, ok := got["x_future_field"].(map[string]any)
	if !ok || future["kept"] != true {
		t.Fatalf("unknown field NOT preserved on round-trip: %s", out)
	}
	if len(got) != 4 {
		t.Fatalf("expected 4 keys after round-trip, got %d: %s", len(got), out)
	}
}

// TestCommandPreservesUnknownFields is Session's sibling for Command, the other type sessions.go
// added without a round-trip test the first time.
func TestCommandPreservesUnknownFields(t *testing.T) {
	raw := `{"id":"cmd_1","object":"command","session_id":"sess_1","kind":"send_message",` +
		`"delivery":"steer","status":"queued","x_future_field":"kept"}`

	var c Command
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if c.ID != "cmd_1" || c.SessionID != "sess_1" || c.Kind != "send_message" || c.Status != "queued" {
		t.Fatalf("typed fields wrong: %+v", c)
	}
	out, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	if got["x_future_field"] != "kept" {
		t.Fatalf("unknown field NOT preserved on round-trip: %s", out)
	}
}

// TestApprovalPreservesUnknownFields is Session's sibling for Approval (approvals.go).
func TestApprovalPreservesUnknownFields(t *testing.T) {
	raw := `{"id":"apr_1","object":"approval","kind":"tool","tool_call_id":"tc_1","run_id":"run_1",` +
		`"session_id":"sess_1","request_hash":"h1","created_at":"2026-08-03T00:00:00Z",` +
		`"identity":"repo.write","operator_label":"Write to repo","arguments":"{}","truncated":false,` +
		`"x_future_field":{"kept":true}}`

	var a Approval
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if a.ID != "apr_1" || a.Kind != "tool" || a.ToolCallID != "tc_1" || a.SessionID != "sess_1" {
		t.Fatalf("typed fields wrong: %+v", a)
	}
	out, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	future, ok := got["x_future_field"].(map[string]any)
	if !ok || future["kept"] != true {
		t.Fatalf("unknown field NOT preserved on round-trip: %s", out)
	}
}

// TestApprovalDecisionResultPreservesUnknownFields is Command's sibling for ApprovalDecisionResult
// (approvals.go), the type Approvals.Approve/Deny decode the server's confirmation into.
func TestApprovalDecisionResultPreservesUnknownFields(t *testing.T) {
	raw := `{"id":"apr_1","object":"approval.decision","decision":"approved","x_future_field":"kept"}`

	var r ApprovalDecisionResult
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if r.ID != "apr_1" || r.Object != "approval.decision" || r.Decision != "approved" {
		t.Fatalf("typed fields wrong: %+v", r)
	}
	out, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	if got["x_future_field"] != "kept" {
		t.Fatalf("unknown field NOT preserved on round-trip: %s", out)
	}
}

// TestEnvelopeSplit locks the T1 Page/ListView distinction: the two envelopes decode into distinct
// types and are NOT conflated (the second struct-decoder edge the corpus targets).
func TestEnvelopeSplit(t *testing.T) {
	pageRaw := `{"data":[{"id":"resp_1"},{"id":"resp_2"}],"has_more":true,"next_cursor":"cur_next","previous_cursor":"cur_prev"}`
	var page Page[json.RawMessage]
	if err := json.Unmarshal([]byte(pageRaw), &page); err != nil {
		t.Fatalf("decode page: %v", err)
	}
	if !page.HasMore || page.NextCursor == nil || *page.NextCursor != "cur_next" {
		t.Fatalf("page fields wrong: %+v", page)
	}
	if len(page.Data) != 2 {
		t.Fatalf("page data len = %d, want 2", len(page.Data))
	}

	listRaw := `{"object":"list","data":[{"id":"sr_1"},{"id":"sr_2"}]}`
	var list ListView[json.RawMessage]
	if err := json.Unmarshal([]byte(listRaw), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if list.Object != "list" || len(list.Data) != 2 {
		t.Fatalf("listview wrong: %+v", list)
	}
	// A Page decoded as ListView loses the cursor (they are NOT interchangeable): the classifier in
	// the read path keys on has_more, never on shape coincidence.
	var mis ListView[json.RawMessage]
	_ = json.Unmarshal([]byte(pageRaw), &mis)
	if mis.Object == "list" {
		t.Fatal("a Page must not classify as a ListView")
	}
}

func TestPageFinalHasNoCursor(t *testing.T) {
	var page Page[json.RawMessage]
	if err := json.Unmarshal([]byte(`{"data":[{"id":"resp_9"}],"has_more":false}`), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if page.HasMore || page.NextCursor != nil || page.PreviousCursor != nil {
		t.Fatalf("final page should carry no cursor: %+v", page)
	}
}
