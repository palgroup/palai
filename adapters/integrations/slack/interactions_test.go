package slack

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The interactivity transport contract (E19 T2, plan §3.5 row D8) and the Special-Tier pacing (row D10).
//
// Every fixture below is built to the PUBLISHED contract and carries the page it came from. The one that
// matters most is the NEGATIVE: a JSON body. Slack does not send one, so a receiver that happily accepts it
// is a receiver whose signature check is verifying bytes Slack never signed.

// CONTRACT: https://docs.slack.dev/interactivity/handling-user-interaction/ (checked 2026-07-26) — "They'll
// be sent to your specified Request URL in an HTTP POST request in the form application/x-www-form-urlencoded"
// and "The body of the request will contain a payload parameter; your app should parse this payload parameter
// as JSON."
func formBody(payload []byte) []byte {
	return []byte("payload=" + url.QueryEscape(string(payload)))
}

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

// TestExtractInteractionPayloadRefusesAnythingButTheDocumentedFormBody is the D8 RED: the ONE body shape
// Slack sends is a form with a single `payload` parameter. A JSON body — the shape every other route in this
// tree receives, and therefore the shape a reader assumes — must be REFUSED here, because accepting it means
// the v0 MAC was checked over bytes that are not what Slack signed.
func TestExtractInteractionPayloadRefusesAnythingButTheDocumentedFormBody(t *testing.T) {
	payload := blockActionsPayload("T1", "U1", "C1", "1700000000.000100", "", ActionApprove, "req_hash")

	got, err := ExtractInteractionPayload(formBody(payload))
	if err != nil {
		t.Fatalf("the documented form body was refused: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("extracted payload = %s, want the url-decoded JSON %s", got, payload)
	}

	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"a raw JSON body (the shape Slack never sends)", payload},
		{"an empty body", nil},
		{"a form with no payload parameter", []byte("token=deprecated&team_id=T1")},
		{"a form with an EMPTY payload parameter", []byte("payload=")},
		// Two `payload` parameters: url.Values keeps both, and silently taking the first is a smuggling
		// surface — the MAC covers the whole body, so a signer could carry a decoy the receiver never reads.
		{"a form with TWO payload parameters", []byte(string(formBody(payload)) + "&" + string(formBody(payload)))},
	} {
		t.Run(tc.name+" is refused", func(t *testing.T) {
			if got, err := ExtractInteractionPayload(tc.body); !errors.Is(err, ErrNotFormEncoded) {
				t.Fatalf("ExtractInteractionPayload = (%s, %v), want ErrNotFormEncoded", got, err)
			}
		})
	}
}

// TestParseInteractionTeamIsALookupKeyOnly pins the pre-verification read: the interactivity body carries the
// workspace id INSIDE the url-encoded payload, so the receiver has to peek at it to know which connection's
// signing secret to verify against — exactly the events route's posture. It is a lookup key, never trust.
func TestParseInteractionTeamIsALookupKeyOnly(t *testing.T) {
	payload := blockActionsPayload("T1", "U1", "C1", "1700000000.000100", "", ActionApprove, "req_hash")
	if team, _, ok := ParseInteractionTeam(payload); !ok || team != "T1" {
		t.Fatalf("ParseInteractionTeam = (%q,%v), want (T1,true)", team, ok)
	}
	// The team falls back to user.team_id, which is what a payload from a user in the app's own workspace
	// carries when the top-level team object is absent.
	fallback, _ := json.Marshal(map[string]any{"type": "block_actions", "user": map[string]any{"id": "U1", "team_id": "T2"}})
	if team, _, ok := ParseInteractionTeam(fallback); !ok || team != "T2" {
		t.Fatalf("ParseInteractionTeam(user.team_id) = (%q,%v), want (T2,true)", team, ok)
	}
	for _, body := range [][]byte{nil, []byte("not json"), []byte(`{"type":"block_actions"}`)} {
		if _, _, ok := ParseInteractionTeam(body); ok {
			t.Fatalf("ParseInteractionTeam(%s) reported a team; without one there is no secret to verify against", body)
		}
	}
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
		Now:   func() time.Time { return now },
		Sleep: func(_ context.Context, d time.Duration) error { waited = append(waited, d); now = now.Add(d); return nil },
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

// TestParseAppRateLimitedIsRecognized closes D10's other half. app_rate_limited arrives as an OUTER type, not
// as an event_callback, so MapEvent refuses it — and the events route would answer 400 + x-slack-no-retry,
// discarding the one notification Slack sends to say we are being throttled.
//
// CONTRACT: https://docs.slack.dev/apis/web-api/rate-limits/ (checked 2026-07-26) — the Events API delivers
// at most "30,000 per workspace/team per app per 60 minutes"; past that the app receives an app_rate_limited
// event carrying token, type, team_id, minute_rate_limited and api_app_id.
func TestParseAppRateLimitedIsRecognized(t *testing.T) {
	body := []byte(`{"token":"deprecated","type":"app_rate_limited","team_id":"T1","minute_rate_limited":1518467820,"api_app_id":"A0001"}`)
	team, minute, ok := ParseAppRateLimited(body)
	if !ok || team != "T1" || minute != 1518467820 {
		t.Fatalf("ParseAppRateLimited = (%q,%d,%v), want (T1,1518467820,true)", team, minute, ok)
	}
	// It must not swallow an ordinary event: an event_callback is NOT a throttling notice.
	if _, _, ok := ParseAppRateLimited([]byte(`{"type":"event_callback","team_id":"T1","event_id":"Ev1"}`)); ok {
		t.Fatal("an event_callback was read as app_rate_limited; the throttling branch would then eat real events")
	}
	// And MapEvent still refuses it, so the route's ordering (throttle notice BEFORE mapping) is load-bearing.
	if _, err := MapEvent(body, "Ubot", false); !errors.Is(err, ErrNotAnEvent) {
		t.Fatalf("MapEvent(app_rate_limited) = %v, want ErrNotAnEvent", err)
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
