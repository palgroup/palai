package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	slack "github.com/palgroup/palai/adapters/integrations/slack"
	palai "github.com/palgroup/palai/sdks/go"
)

// fakeApprovalsPalai is ApprovalsPalai's test double. It is named DIFFERENTLY from inbound_test.go's
// own fakePalai deliberately: that type already exists in this package for relay.Palai (a different
// interface — Sessions/Responses, not Approvals), and the brief's own sketch test used the name
// `fakePalai` for what would be a SECOND, colliding declaration. See the approvals.go file doc for the
// other two places this task's brief did not match what the tree actually has.
type fakeApprovalsPalai struct {
	// pages is what successive ListApprovals calls return, one page per call; the last entry repeats
	// once exhausted so a test that only cares about the first page does not have to size this exactly.
	pages []palai.Page[palai.Approval]
	lists int

	approvals int // count of Approve+Deny calls together — the metric TestAnUnlistedClickerCannotDecide checks stayed zero
	decideErr error
	lastID    string
	lastKind  string // "approve" | "deny"
	lastParam palai.DecisionParams
}

func (f *fakeApprovalsPalai) ListApprovals(ctx context.Context, p palai.ListApprovalsParams) (*palai.Page[palai.Approval], error) {
	f.lists++
	idx := f.lists - 1
	if idx >= len(f.pages) {
		idx = len(f.pages) - 1
	}
	if idx < 0 {
		return &palai.Page[palai.Approval]{}, nil
	}
	page := f.pages[idx]
	return &page, nil
}

func (f *fakeApprovalsPalai) ApproveApproval(ctx context.Context, id string, p palai.DecisionParams) (*palai.ApprovalDecisionResult, error) {
	f.approvals++
	f.lastID, f.lastKind, f.lastParam = id, "approve", p
	if f.decideErr != nil {
		return nil, f.decideErr
	}
	return &palai.ApprovalDecisionResult{ID: id, Object: "approval", Decision: "approve"}, nil
}

func (f *fakeApprovalsPalai) DenyApproval(ctx context.Context, id string, p palai.DecisionParams) (*palai.ApprovalDecisionResult, error) {
	f.approvals++
	f.lastID, f.lastKind, f.lastParam = id, "deny", p
	if f.decideErr != nil {
		return nil, f.decideErr
	}
	return &palai.ApprovalDecisionResult{ID: id, Object: "approval", Decision: "deny"}, nil
}

// fakeApprovalSlack is ApprovalSlack's test double, named apart from relay_test.go's own fakeSlack
// (that one implements the narrower streaming relay.Slack — StartStream/AppendStream/StopStream — a
// different interface this file does not use).
type fakeApprovalSlack struct {
	posted  [][]byte
	updated [][]byte
	// opened are the views.open bodies, kept apart from posted and updated for the reason those two are
	// kept apart from each other: a test asserting that a human was SHOWN a document must not be
	// satisfiable by a message that was posted into the channel instead.
	opened [][]byte

	postErr   error
	updateErr error
	openErr   error
}

func (f *fakeApprovalSlack) PostMessage(ctx context.Context, body []byte) (string, error) {
	f.posted = append(f.posted, body)
	if f.postErr != nil {
		return "", f.postErr
	}
	return "1700000000.000100", nil
}

func (f *fakeApprovalSlack) UpdateMessage(ctx context.Context, body []byte) error {
	f.updated = append(f.updated, body)
	return f.updateErr
}

func (f *fakeApprovalSlack) OpenView(ctx context.Context, body []byte) error {
	f.opened = append(f.opened, body)
	return f.openErr
}

// newTestApprovalDeps builds an ApprovalDeps wired to fresh fakes, with one approver configured — a test that
// wants a different roster overwrites deps.AllowedApprovers, exactly as the brief's own sketch does.
func newTestApprovalDeps(t *testing.T) ApprovalDeps {
	t.Helper()
	return ApprovalDeps{
		Palai:            &fakeApprovalsPalai{},
		Slack:            &fakeApprovalSlack{},
		AllowedApprovers: []string{"U_allowed"},
		// The claim store is a real (in-memory) one rather than a permissive stub, because half the
		// properties this file asserts are about how many messages get posted — and a stub that always
		// granted the claim would make a double-post indistinguishable from a single one.
		Posts: newFakeStore(),
		BotID: "bot_1",
	}
}

// blockActionValues extracts every action's "value" field from a chat.postMessage/chat.update body's
// "actions" block, so a test can assert on the composite approvalID|requestHash string without
// hand-parsing the whole Block Kit tree.
func blockActionValues(t *testing.T, body []byte) []string {
	t.Helper()
	var doc struct {
		Blocks []struct {
			Type     string `json:"type"`
			Elements []struct {
				ActionID string `json:"action_id"`
				Value    string `json:"value"`
			} `json:"elements"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("decode message body: %v", err)
	}
	var values []string
	for _, b := range doc.Blocks {
		if b.Type != "actions" {
			continue
		}
		for _, e := range b.Elements {
			values = append(values, e.Value)
		}
	}
	return values
}

// --- OnButton: the allow-list gate --------------------------------------------------------------

func TestAnUnlistedClickerCannotDecide(t *testing.T) {
	deps := newTestApprovalDeps(t)
	deps.AllowedApprovers = []string{"U_allowed"}

	err := OnButton(context.Background(), deps, slack.ApprovalIntent{
		UserID: "U_stranger", Decision: "approve",
		RequestHash: approvalActionValue("apr_1", "hash_1"),
		ChannelID:   "C1", MessageTS: "1.1",
	})
	if err == nil {
		t.Fatal("an unlisted user decided an approval")
	}
	if deps.Palai.(*fakeApprovalsPalai).approvals != 0 {
		t.Fatal("the decision reached the API before the allow-list was checked")
	}
}

func TestAnUnlistedClickerErrorNamesTheGate(t *testing.T) {
	deps := newTestApprovalDeps(t)
	err := OnButton(context.Background(), deps, slack.ApprovalIntent{
		UserID: "U_stranger", Decision: "deny",
		RequestHash: approvalActionValue("apr_1", "hash_1"),
	})
	if !errors.Is(err, ErrApproverNotAllowed) {
		t.Fatalf("want ErrApproverNotAllowed, got %v", err)
	}
}

// --- OnButton: the happy paths ------------------------------------------------------------------

func TestAnAllowedClickerApproves(t *testing.T) {
	deps := newTestApprovalDeps(t)
	fp := deps.Palai.(*fakeApprovalsPalai)
	fs := deps.Slack.(*fakeApprovalSlack)

	err := OnButton(context.Background(), deps, slack.ApprovalIntent{
		UserID: "U_allowed", Decision: "approve",
		RequestHash: approvalActionValue("apr_1", "hash_1"),
		ChannelID:   "C1", MessageTS: "1.1",
	})
	if err != nil {
		t.Fatalf("an allowed approver could not decide: %v", err)
	}
	if fp.approvals != 1 || fp.lastKind != "approve" || fp.lastID != "apr_1" {
		t.Fatalf("want exactly one Approve(apr_1), got kind=%s id=%s count=%d", fp.lastKind, fp.lastID, fp.approvals)
	}
	if fp.lastParam.RequestHash != "hash_1" {
		t.Fatalf("want the request hash split off the composite value, got %q", fp.lastParam.RequestHash)
	}
	if fp.lastParam.Reason != "" {
		t.Fatal("this bridge has no reason-capture UI and must never send a Reason")
	}
	if len(fs.updated) != 1 {
		t.Fatalf("want exactly one message repair, got %d", len(fs.updated))
	}
}

func TestAnAllowedClickerDenies(t *testing.T) {
	deps := newTestApprovalDeps(t)
	fp := deps.Palai.(*fakeApprovalsPalai)

	err := OnButton(context.Background(), deps, slack.ApprovalIntent{
		UserID: "U_allowed", Decision: "deny",
		RequestHash: approvalActionValue("apr_2", "hash_2"),
		ChannelID:   "C1", MessageTS: "1.2",
	})
	if err != nil {
		t.Fatalf("an allowed approver could not deny: %v", err)
	}
	if fp.lastKind != "deny" || fp.lastID != "apr_2" {
		t.Fatalf("want Deny(apr_2), got kind=%s id=%s", fp.lastKind, fp.lastID)
	}
}

func TestOnButtonRejectsAnUnknownDecision(t *testing.T) {
	deps := newTestApprovalDeps(t)
	fp := deps.Palai.(*fakeApprovalsPalai)
	err := OnButton(context.Background(), deps, slack.ApprovalIntent{
		UserID: "U_allowed", Decision: "sideways",
		RequestHash: approvalActionValue("apr_1", "hash_1"),
	})
	if err == nil {
		t.Fatal("an unrecognized decision was accepted")
	}
	if fp.approvals != 0 {
		t.Fatal("an unrecognized decision must not reach the API either")
	}
}

func TestOnButtonRejectsAMalformedButtonValue(t *testing.T) {
	for _, value := range []string{"", "no_pipe_here", "|missing_approval_id", "apr_1|"} {
		deps := newTestApprovalDeps(t)
		fp := deps.Palai.(*fakeApprovalsPalai)
		err := OnButton(context.Background(), deps, slack.ApprovalIntent{
			UserID: "U_allowed", Decision: "approve", RequestHash: value,
		})
		if err == nil {
			t.Fatalf("value %q was accepted as a bound approval", value)
		}
		if fp.approvals != 0 {
			t.Fatalf("value %q reached the API", value)
		}
	}
}

// --- OnButton: the two already-decided codes, both handled ONE way -----------------------------

func TestOnButtonRefreshesRatherThanErrorsWhenAlreadyDecided(t *testing.T) {
	for _, status := range []int{http.StatusConflict, http.StatusNotFound} {
		deps := newTestApprovalDeps(t)
		fp := deps.Palai.(*fakeApprovalsPalai)
		fs := deps.Slack.(*fakeApprovalSlack)
		fp.decideErr = &palai.APIError{Status: status}

		err := OnButton(context.Background(), deps, slack.ApprovalIntent{
			UserID: "U_allowed", Decision: "approve",
			RequestHash: approvalActionValue("apr_1", "hash_1"),
			ChannelID:   "C1", MessageTS: "1.1",
		})
		if err != nil {
			t.Fatalf("status %d: an already-decided approval must repair the message, not surface as an error: %v", status, err)
		}
		if len(fs.updated) != 1 {
			t.Fatalf("status %d: want one repair, got %d", status, len(fs.updated))
		}
		body := string(fs.updated[0])
		if strings.Contains(body, "no such approval") {
			t.Fatalf("status %d: the repair must never say \"no such approval\" — got %s", status, body)
		}
	}
}

func TestOnButtonSurfacesOtherErrors(t *testing.T) {
	deps := newTestApprovalDeps(t)
	fs := deps.Slack.(*fakeApprovalSlack)
	deps.Palai.(*fakeApprovalsPalai).decideErr = &palai.APIError{Status: http.StatusInternalServerError}

	err := OnButton(context.Background(), deps, slack.ApprovalIntent{
		UserID: "U_allowed", Decision: "approve",
		RequestHash: approvalActionValue("apr_1", "hash_1"),
	})
	if err == nil {
		t.Fatal("a 500 from the API must surface as an error")
	}
	if len(fs.updated) != 0 {
		t.Fatal("a genuine failure must not be repainted as a quiet repair")
	}
}

// --- OnApprovalRequested -------------------------------------------------------------------------

func TestOnApprovalRequestedPostsAMessageForAToolApproval(t *testing.T) {
	deps := newTestApprovalDeps(t)
	fs := deps.Slack.(*fakeApprovalSlack)
	deps.Palai.(*fakeApprovalsPalai).pages = []palai.Page[palai.Approval]{{
		Data: []palai.Approval{{
			ID: "apr_1", Kind: "tool", RequestHash: "hash_1",
			Identity: "github.push", OperatorLabel: "pushes to the release branch",
			Arguments: `{"branch":"release"}`,
		}},
	}}

	ev := palai.Event{
		Type: ApprovalRequestedEventType, SessionID: "sess_1",
		Data: map[string]any{"tool_call_id": "tc_1", "approval_id": "apr_1", "tool_name": "github.push", "request_hash": "hash_1", "run_id": "run_1"},
	}
	if err := OnApprovalRequested(context.Background(), deps, "C1", "1.0", ev); err != nil {
		t.Fatalf("OnApprovalRequested: %v", err)
	}
	if len(fs.posted) != 1 {
		t.Fatalf("want one posted message, got %d", len(fs.posted))
	}
	values := blockActionValues(t, fs.posted[0])
	if len(values) == 0 {
		t.Fatal("no action buttons were rendered")
	}
	for _, v := range values {
		if v != approvalActionValue("apr_1", "hash_1") {
			t.Fatalf("button value %q does not carry the bound approval id + request hash", v)
		}
	}
}

// TestOnApprovalRequestedResolvesAPublicationApprovalWithNoApprovalIDInTheEvent is the asymmetry the
// approvals.go file doc measures directly against packages/coordinator/publication.go: the
// publication genesis event carries publication_id/operation/branch/request_hash/display and NO
// approval_id at all. This proves OnApprovalRequested still finds the row — via request_hash, not
// approval_id — rather than assuming every approval.requested.v1 event looks like the tool one.
func TestOnApprovalRequestedResolvesAPublicationApprovalWithNoApprovalIDInTheEvent(t *testing.T) {
	deps := newTestApprovalDeps(t)
	deps.Palai.(*fakeApprovalsPalai).pages = []palai.Page[palai.Approval]{{
		Data: []palai.Approval{{
			ID: "apr_pub_1", Kind: "publication", RequestHash: "hash_pub_1",
			Identity: "push", OperatorLabel: "release/<!channel>-cutover",
			Arguments: `{"branch":"release/<!channel>-cutover"}`,
		}},
	}}

	ev := palai.Event{
		Type: ApprovalRequestedEventType, SessionID: "sess_2",
		Data: map[string]any{"publication_id": "pub_1", "operation": "push", "branch": "release/x", "request_hash": "hash_pub_1", "display": "release cutover"},
	}
	if err := OnApprovalRequested(context.Background(), deps, "C1", "1.0", ev); err != nil {
		t.Fatalf("OnApprovalRequested could not resolve a publication approval with no approval_id in the event: %v", err)
	}
}

// decodedMessageText JSON-decodes body and re-encodes it WITHOUT HTML escaping, so a test can search
// for a literal `<!channel>`-shaped substring in the actual STRING VALUES the wire carries. Searching
// the raw bytes of body directly is the wrong check: Go's default json.Marshal (what
// slack.ToolApprovalMessage's own final `json.Marshal(body)` uses, same as this shipped codebase's
// ApprovalMessage/UpdateMessage) HTML-escapes `<`, `>` and `&` in EVERY string field, so an already
// -defused value like `&lt;!channel>` comes out as `&lt;!channel>` — a different SOURCE
// TEXT encoding of the identical decoded string, not a second, weaker escape. A raw substring check on
// that source text would report the token "gone" or "present" for reasons that have nothing to do with
// whether NeutralizeBroadcasts ran.
func decodedMessageText(t *testing.T, body []byte) string {
	t.Helper()
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode message body: %v", err)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(decoded); err != nil {
		t.Fatalf("re-encode message body: %v", err)
	}
	return buf.String()
}

// TestBuildApprovalMessageNeutralizesAPublicationsUnneutralizedFields is the load-bearing security
// test: GET /v1/approvals does NOT run a publication row's Identity/OperatorLabel/Arguments through
// slack.DeriveApprovalDisplay server-side (store/approvals.go:90 assigns the raw columns directly),
// so a run-controlled branch name or PR display containing a broadcast token would reach Slack live
// unless THIS function defuses it. It must.
func TestBuildApprovalMessageNeutralizesAPublicationsUnneutralizedFields(t *testing.T) {
	approval := palai.Approval{
		ID: "apr_pub_1", Kind: "publication", RequestHash: "hash_pub_1",
		Identity:      "<!channel> push",
		OperatorLabel: "merges <!here> into main",
		Arguments:     `{"branch":"<@U_everyone>-fix"}`,
	}
	body := buildApprovalMessage("C1", "1.0", approval)
	text := decodedMessageText(t, body)

	for _, raw := range []string{"<!channel>", "<!here>", "<@U_everyone>"} {
		if strings.Contains(text, raw) {
			t.Fatalf("a live broadcast token %q survived into the outbound message's decoded text: %s", raw, text)
		}
	}
	// The defused form must still be legible — this is a rendering discipline, not a silent drop.
	if !strings.Contains(text, "&lt;!channel> push") {
		t.Fatalf("the identity was not merely hidden, it must still be readable in its defused form: %s", text)
	}
}

func TestOnApprovalRequestedFailsWithoutARequestHash(t *testing.T) {
	deps := newTestApprovalDeps(t)
	ev := palai.Event{Type: ApprovalRequestedEventType, Data: map[string]any{"tool_call_id": "tc_1"}}
	if err := OnApprovalRequested(context.Background(), deps, "C1", "1.0", ev); err == nil {
		t.Fatal("an event with no request_hash must be refused")
	}
}

func TestOnApprovalRequestedFailsWhenNoOpenApprovalMatches(t *testing.T) {
	deps := newTestApprovalDeps(t)
	deps.Palai.(*fakeApprovalsPalai).pages = []palai.Page[palai.Approval]{{Data: nil}}
	ev := palai.Event{Type: ApprovalRequestedEventType, Data: map[string]any{"request_hash": "hash_missing"}}
	err := OnApprovalRequested(context.Background(), deps, "C1", "1.0", ev)
	if !errors.Is(err, ErrApprovalNotFound) {
		t.Fatalf("want ErrApprovalNotFound, got %v", err)
	}
}

func TestFindPendingApprovalPaginates(t *testing.T) {
	deps := newTestApprovalDeps(t)
	fp := deps.Palai.(*fakeApprovalsPalai)
	cursor := "cursor_1"
	fp.pages = []palai.Page[palai.Approval]{
		{Data: []palai.Approval{{ID: "apr_1", RequestHash: "hash_1"}}, HasMore: true, NextCursor: &cursor},
		{Data: []palai.Approval{{ID: "apr_2", RequestHash: "hash_2"}}, HasMore: false},
	}
	approval, err := findPendingApproval(context.Background(), deps, "hash_2")
	if err != nil {
		t.Fatalf("findPendingApproval: %v", err)
	}
	if approval.ID != "apr_2" {
		t.Fatalf("want apr_2 off the second page, got %s", approval.ID)
	}
	if fp.lists != 2 {
		t.Fatalf("want two ListApprovals calls, got %d", fp.lists)
	}
}
