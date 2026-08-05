package slack

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The interactivity payload shape (plan §3.5 row D8) and the Special-Tier pacing (row D10).
//
// Every fixture below is built to the PUBLISHED contract and carries the page it came from.

// CONTRACT: https://docs.slack.dev/reference/interaction-payloads/block_actions-payload/ (checked
// 2026-07-26) — the documented top-level fields of a block_actions payload. Only the ones this adapter reads
// are populated here; `container`, `trigger_id`, `response_url` and `api_app_id` are present in the real
// payload and deliberately ignored (see MapInteractiveApproval on why response_url is not used).
func blockActionsPayload(team, user, channel, messageTS, threadTS, actionID, value string) []byte {
	message := map[string]any{"type": "message", "ts": messageTS, "text": "approve this?"}
	if threadTS != "" {
		message["thread_ts"] = threadTS
	}
	raw, _ := json.Marshal(map[string]any{
		"type":       "block_actions",
		"team":       map[string]any{"id": team, "domain": "example"},
		"user":       map[string]any{"id": user, "username": "clicker", "team_id": team},
		"api_app_id": "A0001",
		"container": map[string]any{
			"type": "message", "message_ts": messageTS, "channel_id": channel, "is_ephemeral": false,
		},
		"channel":      map[string]any{"id": channel, "name": "approvals"},
		"message":      message,
		"response_url": "https://hooks.slack.invalid/actions/T0/1/2",
		"trigger_id":   "12321423423.333649436676.d8c1bb837935619ccad0f624c448ffb3",
		"actions": []any{map[string]any{
			"action_id": actionID, "block_id": "=qXel", "value": value, "type": "button",
			"action_ts": "1548426417.840180",
		}},
	})
	return raw
}

// TestMapInteractiveApprovalCarriesTheClickedThread is what the decision path correlates on: a click can only
// decide the session ITS OWN thread owns, so the mapping has to carry the channel + thread root the button
// was clicked in. Without them the request hash alone would have to select a session — a hash from any
// channel deciding any conversation.
func TestMapInteractiveApprovalCarriesTheClickedThread(t *testing.T) {
	const root = "1700000000.000100"
	// A threaded approval message: the root is thread_ts.
	intent, err := MapInteractiveApproval(blockActionsPayload("T1", "U1", "C1", "1700000009.000200", root, ActionApprove, "req_hash"))
	if err != nil {
		t.Fatalf("map threaded click: %v", err)
	}
	if intent.ChannelID != "C1" || intent.ThreadTS != root {
		t.Fatalf("intent = %+v, want channel C1 and thread root %s", intent, root)
	}
	// A top-level approval message IS its own thread root, the MapEvent fallback.
	intent, err = MapInteractiveApproval(blockActionsPayload("T1", "U1", "C1", root, "", ActionDeny, "req_hash"))
	if err != nil {
		t.Fatalf("map top-level click: %v", err)
	}
	if intent.ChannelID != "C1" || intent.ThreadTS != root || intent.Decision != "deny" {
		t.Fatalf("intent = %+v, want channel C1, thread root %s, deny", intent, root)
	}
}

// TestChannelPacerHoldsSpecialTierPerChannel closes D10's pacing half: chat.postMessage is not in a numbered
// tier, it is the Special Tier — roughly one message per second PER CHANNEL. A coalesced-update loop that
// writes twice in the same instant is exactly the traffic that limit exists to refuse.
//
// CONTRACT: https://docs.slack.dev/apis/web-api/rate-limits/ (checked 2026-07-26) — "chat.postMessage
// generally allows posting one message per second per channel, while also maintaining a workspace-wide
// limit"; "Short bursts >1 allowed" with no guarantee the burst is stored.
func TestChannelPacerHoldsSpecialTierPerChannel(t *testing.T) {
	now := time.Unix(1700000000, 0)
	var waited []time.Duration
	pacer := &ChannelPacer{
		Now: func() time.Time { return now },
		Sleep: func(_ context.Context, d time.Duration) error {
			waited = append(waited, d)
			now = now.Add(d)
			return nil
		},
	}
	ctx := context.Background()

	if err := pacer.Wait(ctx, "C1"); err != nil {
		t.Fatalf("first post in a channel waited: %v", err)
	}
	if len(waited) != 0 {
		t.Fatalf("the FIRST post in a channel waited %v, want no wait — the limit is a rate, not a delay", waited)
	}
	// A second update in the same instant must be paced to the per-channel interval.
	if err := pacer.Wait(ctx, "C1"); err != nil {
		t.Fatalf("second post: %v", err)
	}
	if len(waited) != 1 || waited[0] != SpecialTierPerChannel {
		t.Fatalf("second post in the SAME channel waited %v, want one wait of %v — coalesced updates must be paced per channel (plan §3.5 D10)", waited, SpecialTierPerChannel)
	}
	// A different channel has its own budget: pacing is per channel, not global.
	if err := pacer.Wait(ctx, "C2"); err != nil {
		t.Fatalf("first post in a second channel: %v", err)
	}
	if len(waited) != 1 {
		t.Fatalf("a post in a DIFFERENT channel waited %v — the documented limit is per channel; pacing globally throttles unrelated conversations", waited)
	}
}

// TestApprovalMessageCarriesBothMintedButtons pins the minimal Block Kit surface: the two action ids the
// decision path matches on, each carrying the one-shot request hash, plus the authoritative detail. A button
// minted without the hash decides nothing (MapInteractiveApproval refuses it), so this is the message
// construction and the mapping agreeing with each other rather than a golden string.
func TestApprovalMessageCarriesBothMintedButtons(t *testing.T) {
	body := ApprovalMessage("C1", "1700000000.000100", "push agent/journey to main", "req_hash_abc")
	var msg struct {
		Channel  string `json:"channel"`
		ThreadTS string `json:"thread_ts"`
		Text     string `json:"text"`
		Blocks   []struct {
			Type     string `json:"type"`
			Elements []struct {
				ActionID string `json:"action_id"`
				Value    string `json:"value"`
			} `json:"elements"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		t.Fatalf("the approval message is not valid JSON: %v", err)
	}
	if msg.Channel != "C1" || msg.ThreadTS != "1700000000.000100" {
		t.Fatalf("message = %+v, want it posted into the correlated thread", msg)
	}
	if !strings.Contains(msg.Text, "push agent/journey to main") {
		t.Fatalf("text = %q, want the authoritative detail — a button with no statement of WHAT it approves is a blind approval", msg.Text)
	}
	var seen []string
	for _, b := range msg.Blocks {
		for _, e := range b.Elements {
			if e.Value != "req_hash_abc" {
				t.Fatalf("button %q carries value %q, want the one-shot request hash", e.ActionID, e.Value)
			}
			seen = append(seen, e.ActionID)
		}
	}
	if len(seen) != 2 || seen[0] != ActionApprove || seen[1] != ActionDeny {
		t.Fatalf("minted actions = %v, want [%s %s]", seen, ActionApprove, ActionDeny)
	}
	// The round trip: the message this builds maps back to an intent carrying the same hash.
	intent, err := MapInteractiveApproval(blockActionsPayload("T1", "U1", "C1", "1700000000.000100", "", ActionApprove, "req_hash_abc"))
	if err != nil || intent.RequestHash != "req_hash_abc" {
		t.Fatalf("round trip = (%+v,%v), want the minted hash back", intent, err)
	}
}

// A model can answer at any length. Slack truncates a `text` over 40,000 characters and "may also
// automatically split the content into multiple messages" — and a split message is several visible messages
// under one ts, which silently breaks the one-run-one-message claim the delivery row is keyed on. So the
// truncation is ours, it is visible, and it says where the whole answer is.
func TestThreadReplyTruncatesLongOutputWithAMarker(t *testing.T) {
	long := strings.Repeat("ü", 10_000) // multi-byte on purpose: the budget is characters, not bytes
	var body struct {
		Channel  string `json:"channel"`
		ThreadTS string `json:"thread_ts"`
		Text     string `json:"text"`
	}
	if err := json.Unmarshal(ThreadReply("C1", "100.0", long, "resp_abc"), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if n := len([]rune(body.Text)); n != MaxMessageText {
		t.Fatalf("truncated text = %d runes, want exactly %d", n, MaxMessageText)
	}
	if !strings.Contains(body.Text, "truncated") || !strings.Contains(body.Text, "resp_abc") {
		t.Fatalf("the truncated reply does not say it was cut or where the rest is: %q", body.Text[len(body.Text)-120:])
	}
	if body.Channel != "C1" || body.ThreadTS != "100.0" {
		t.Fatalf("reply addressed to %q/%q, want C1/100.0", body.Channel, body.ThreadTS)
	}
}

// A short answer is posted verbatim — no marker, no blocks, no rewriting of what the model said.
func TestThreadReplyPassesShortOutputThrough(t *testing.T) {
	var body struct {
		Text   string `json:"text"`
		Blocks any    `json:"blocks"`
	}
	if err := json.Unmarshal(ThreadReply("C1", "100.0", "merhaba", "resp_abc"), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Text != "merhaba" {
		t.Fatalf("text = %q, want it verbatim", body.Text)
	}
	if body.Blocks != nil {
		t.Fatalf("the reply carries Block Kit blocks (%v); a model's prose needs no rendering surface", body.Blocks)
	}
}

// A message posted OUTSIDE a thread carries no thread_ts at all rather than an empty one, which Slack would
// read as a malformed parent.
func TestThreadReplyOmitsAnEmptyThreadTS(t *testing.T) {
	if got := string(ThreadReply("C1", "", "hi", "")); strings.Contains(got, "thread_ts") {
		t.Fatalf("body = %s, want no thread_ts key", got)
	}
}
