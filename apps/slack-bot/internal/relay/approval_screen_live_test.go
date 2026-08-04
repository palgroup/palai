//go:build live

// THE APPROVAL SCREEN, PROVED FROM THE WIRE AND FROM THE THREAD.
//
// The unit tests one file over assert what this package BUILDS. These two legs assert what Slack ACCEPTED
// and what a human can read back, which is a different claim and the one the operator made: the message
// they were shown had an empty table, an unreadable destination line and a card titled "Thinking…" while
// it waited for them.
//
// THE HEADLINE LEG READS THE WIRE RATHER THAN THE THREAD, and that is forced rather than chosen: a Slack
// message mid-stream comes back from conversations.replies with NO blocks at all — measured — so the one
// state this leg is about, the container title while a run is parked, is unreadable from the thread by
// construction. It is read from the calls the relay actually made and from Slack's own `ok` on each.
package relay

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/palgroup/palai/adapters/integrations/slack"
	palai "github.com/palgroup/palai/sdks/go"
)

// recordingDoer forwards every Slack call to the real API and keeps a copy of the request body and the
// answer. It records the REQUEST rather than reconstructing it, so what is asserted is the bytes that went
// out, and it keeps Slack's `ok` so a call the API refused cannot be read as one it took.
type recordingDoer struct {
	mu    sync.Mutex
	calls []sentCall
}

type sentCall struct {
	method string
	body   string
	ok     bool
}

func (d *recordingDoer) Do(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
		req.Body = io.NopCloser(strings.NewReader(string(body)))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	answer, _ := io.ReadAll(resp.Body)
	resp.Body = io.NopCloser(strings.NewReader(string(answer)))
	var decoded struct {
		OK bool `json:"ok"`
	}
	_ = json.Unmarshal(answer, &decoded)
	d.mu.Lock()
	d.calls = append(d.calls, sentCall{method: req.URL.Path, body: string(body), ok: decoded.OK})
	d.mu.Unlock()
	return resp, nil
}

func (d *recordingDoer) sent() []sentCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]sentCall(nil), d.calls...)
}

// TestAParkedRunTellsTheThreadItIsWaiting drives the REAL relay over a REAL session journal — one that
// genuinely contains an approval.requested.v1 — into a REAL Slack stream, and asserts the container
// headline Slack accepted.
//
// PALAI_PROOF_SESSION_ID must name a session whose journal carries a parked approval. Replaying a finished
// run's journal is deliberate: the events are the ones the control plane really wrote, and the relay cannot
// tell a replay from a live tail (that is the whole basis of the recovery path), so what it sends here is
// what it sends when the run is actually parked.
func TestAParkedRunTellsTheThreadItIsWaiting(t *testing.T) {
	apiURL := liveEnv(t, "PALAI_API_URL")
	apiKey := liveEnv(t, "PALAI_API_KEY")
	token := []byte(liveEnv(t, "SLACK_BOT_TOKEN"))
	channel := liveEnv(t, "SLACK_TEST_CHANNEL")
	teamID := liveEnv(t, "SLACK_TEAM_ID")
	userID := liveEnv(t, "SLACK_RECIPIENT_USER_ID")
	sessionID := liveEnv(t, "PALAI_PROOF_SESSION_ID")

	ctx := context.Background()
	client, err := palai.New(palai.WithBaseURL(apiURL), palai.WithAPIKey(apiKey))
	if err != nil {
		t.Fatalf("palai client: %v", err)
	}
	stream, err := client.Sessions.Events(ctx, sessionID, palai.EventsParams{AfterSequence: 0})
	if err != nil {
		t.Fatalf("open the journal: %v", err)
	}

	doer := &recordingDoer{}
	root, err := slack.PostMessage(ctx, doer, slack.PostRequest{
		MethodURL: "https://slack.com/api/chat.postMessage", Token: token,
		Body: mustJSON(map[string]any{"channel": channel, "text": "approval screen proof — the card's title while a run is parked"}),
	}, slack.PostOptions{})
	if err != nil {
		t.Fatalf("post the thread root: %v", err)
	}
	t.Logf("PROOF thread=%s/%s", channel, root.MessageTS)

	var approvals int
	err = Run(ctx, Deps{
		Events: stream,
		Slack:  NewChannelSlackStreamer(doer, "https://slack.com/api", token)(userID, teamID),
		// The approval message is the OTHER leg's subject; this one is about the container title, so the
		// hook only counts that the journal really did park.
		OnApproval: func(context.Context, string, string, palai.Event) { approvals++ },
		Delivery:   DeliveryFuncs{},
	}, sessionID, channel, root.MessageTS)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if approvals == 0 {
		t.Fatalf("session %s's journal carries no approval.requested.v1 — this leg would prove nothing", sessionID)
	}

	// THE HEADLINE, ON THE WIRE. A plan_update chunk is how the container title is set; there is no other
	// way to write it, so its absence here is its absence on screen.
	var wroteWaiting, acceptedWaiting bool
	for _, call := range doer.sent() {
		if !strings.Contains(call.body, `"plan_update"`) || !strings.Contains(call.body, awaitingApprovalHeadline) {
			continue
		}
		wroteWaiting = true
		if call.ok {
			acceptedWaiting = true
		}
	}
	if !wroteWaiting {
		t.Fatalf("the relay never sent %q as the container headline for a parked run; %d Slack call(s) were made",
			awaitingApprovalHeadline, len(doer.sent()))
	}
	// ACCEPTED, not merely sent: a chunk Slack refused is a headline nobody saw, and every earlier
	// measurement in this tree that read only the outgoing side missed exactly that.
	if !acceptedWaiting {
		t.Fatal("Slack refused the parked-run headline; it was sent and never rendered")
	}
	t.Logf("HEADLINE ACCEPTED %q over %d Slack call(s)", awaitingApprovalHeadline, len(doer.sent()))
}

// TestTheApprovalScreenAsAHumanReadsIt renders the message for a REAL open approval — one this bot's own
// thread owns — and reads it back out of the thread.
//
// It asserts the three defects are gone AS RENDERED BY SLACK, not as built in memory: no table without
// rows, no button over an empty document, and every fact the button authorizes on its own labelled line.
func TestTheApprovalScreenAsAHumanReadsIt(t *testing.T) {
	apiURL := liveEnv(t, "PALAI_API_URL")
	apiKey := liveEnv(t, "PALAI_API_KEY")
	token := []byte(liveEnv(t, "SLACK_BOT_TOKEN"))
	channel := liveEnv(t, "SLACK_TEST_CHANNEL")
	approvalID := liveEnv(t, "PALAI_PROOF_APPROVAL_ID")

	ctx := context.Background()
	client, err := palai.New(palai.WithBaseURL(apiURL), palai.WithAPIKey(apiKey))
	if err != nil {
		t.Fatalf("palai client: %v", err)
	}
	page, err := client.Approvals.List(ctx, palai.ListApprovalsParams{Limit: 100})
	if err != nil {
		t.Fatalf("list approvals: %v", err)
	}
	var approval palai.Approval
	for _, a := range page.Data {
		if a.ID == approvalID {
			approval = a
		}
	}
	if approval.ID == "" {
		t.Fatalf("approval %s is not open; this leg needs a genuinely parked run", approvalID)
	}

	root, err := slack.PostMessage(ctx, http.DefaultClient, slack.PostRequest{
		MethodURL: "https://slack.com/api/chat.postMessage", Token: token,
		Body: mustJSON(map[string]any{"channel": channel, "text": "approval screen proof — the message a human decides from"}),
	}, slack.PostOptions{})
	if err != nil {
		t.Fatalf("post the thread root: %v", err)
	}
	posted, err := slack.PostMessage(ctx, http.DefaultClient, slack.PostRequest{
		MethodURL: "https://slack.com/api/chat.postMessage", Token: token,
		Body: buildApprovalMessage(channel, root.MessageTS, approval),
	}, slack.PostOptions{})
	if err != nil {
		t.Fatalf("post the approval message: %v", err)
	}
	t.Logf("PROOF thread=%s/%s message=%s", channel, root.MessageTS, posted.MessageTS)

	// READ BACK AS SLACK STORED IT, blocks and all. slack.ThreadReplies carries only the text (it exists for
	// the history leg, which reads prose), and the whole claim here is about BLOCKS — so this leg makes the
	// raw call rather than asserting over the one field that could never show the defect.
	blocks := replyBlocks(t, token, channel, root.MessageTS, posted.MessageTS)

	var tables, buttons int
	var headers, rows, actionIDs []string
	for _, b := range blocks {
		switch b["type"] {
		case "table":
			tables++
			for i, row := range b["rows"].([]any) {
				var cells []string
				for _, c := range row.([]any) {
					text, _ := c.(map[string]any)["text"].(string)
					cells = append(cells, text)
				}
				if i == 0 {
					headers = append(headers, strings.Join(cells, "|"))
				} else {
					rows = append(rows, strings.Join(cells, "="))
				}
			}
		case "actions":
			for _, e := range b["elements"].([]any) {
				id, _ := e.(map[string]any)["action_id"].(string)
				actionIDs = append(actionIDs, id)
				buttons++
			}
		}
	}
	t.Logf("AS RENDERED tables=%d headers=%v rows=%v buttons=%v", tables, headers, rows, actionIDs)

	// DEFECT 1 — no table that lists nothing, and no button over an empty document.
	for _, header := range headers {
		if header == "argument|value" {
			t.Fatalf("the argument table is back on a call that carries no arguments; rows=%v", rows)
		}
	}
	for _, id := range actionIDs {
		if id == slack.ActionShowArguments {
			t.Fatalf("the show-arguments button is back over an empty document; buttons=%v", actionIDs)
		}
	}
	if buttons != 2 {
		t.Fatalf("the screen offers %d button(s) (%v), want approve and deny", buttons, actionIDs)
	}
	// DEFECT 2 — every fact on its own labelled row, and the run-on sentence gone.
	if len(headers) != 1 || headers[0] != "what|value" {
		t.Fatalf("the destination table headers are %v, want exactly [what|value]", headers)
	}
	joined := strings.Join(rows, "\n")
	for _, want := range []string{approval.Remote, approval.Branch, approval.HeadSHA} {
		if want != "" && !strings.Contains(joined, want) {
			t.Fatalf("%q is not on the screen in full; rows were %v", want, rows)
		}
	}
	if strings.Contains(strings.Join(blockStrings(blocks), "\n"), approval.OperatorLabel) {
		t.Fatalf("the row's pre-baked run-on sentence is still rendered")
	}
}

// replyBlocks fetches one message out of a thread and returns its blocks as Slack stored them.
func replyBlocks(t *testing.T, token []byte, channel, threadTS, messageTS string) []map[string]any {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet,
		"https://slack.com/api/conversations.replies?channel="+channel+"&ts="+threadTS+"&limit=20", nil)
	if err != nil {
		t.Fatalf("build the read-back: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+string(token))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("conversations.replies: %v", err)
	}
	defer resp.Body.Close()
	var body struct {
		OK       bool `json:"ok"`
		Messages []struct {
			TS     string           `json:"ts"`
			Blocks []map[string]any `json:"blocks"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode conversations.replies: %v", err)
	}
	if !body.OK {
		t.Fatal("conversations.replies answered not-ok")
	}
	for _, m := range body.Messages {
		if m.TS == messageTS {
			return m.Blocks
		}
	}
	t.Fatalf("message %s did not come back from the thread", messageTS)
	return nil
}

// blockStrings flattens every string in a decoded block array — the same discipline blocks_test.go's own
// sweeps use, because a raw substring check over marshalled bytes cannot fail.
func blockStrings(blocks []map[string]any) []string {
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
	for _, b := range blocks {
		walk(b)
	}
	return out
}
