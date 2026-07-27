package slack

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestMapEventNormalizesToCanonicalIdentity(t *testing.T) {
	body := []byte(`{
		"type":"event_callback","team_id":"T1","enterprise_id":"E1","event_id":"Ev01",
		"event":{"type":"app_mention","user":"U9","channel":"C1","ts":"111.1","thread_ts":"100.0","text":"hi"}
	}`)
	ev, err := MapEvent(body, "Ubot", false)
	if err != nil {
		t.Fatalf("MapEvent error = %v", err)
	}
	if ev.Source != Source || ev.SourceTenant != "T1" || ev.SourceEventID != "Ev01" {
		t.Fatalf("canonical identity = %q/%q/%q, want slack/T1/Ev01", ev.Source, ev.SourceTenant, ev.SourceEventID)
	}
	if ev.ChannelID != "C1" || ev.ThreadTS != "100.0" || ev.UserID != "U9" {
		t.Fatalf("correlation = %q/%q/%q, want C1/100.0/U9", ev.ChannelID, ev.ThreadTS, ev.UserID)
	}
	if ev.Kind != KindMessage {
		t.Fatalf("kind = %q, want message", ev.Kind)
	}
	// Data stays opaque — the inner event object, not re-parsed here.
	if len(ev.Data) == 0 {
		t.Fatal("Data (opaque inner event) was dropped")
	}
}

// A redelivery carries the SAME event_id, so it maps to the identical dedupe key — the canonical
// source-dedupe (AUT-009) then collapses it to one effect. The transport (Events API vs Socket Mode) does
// not change identity either.
func TestMapEventRedeliveryIsIdentityStable(t *testing.T) {
	body := []byte(`{"type":"event_callback","team_id":"T1","event_id":"Ev42","event":{"type":"message","user":"U1","channel":"C1","ts":"5.5"}}`)
	first, err := MapEvent(body, "Ubot", false)
	if err != nil {
		t.Fatalf("first MapEvent error = %v", err)
	}
	retry, err := MapEvent(body, "Ubot", true) // Slack's redelivery: X-Slack-Retry-Num set
	if err != nil {
		t.Fatalf("retry MapEvent error = %v", err)
	}
	if first.SourceEventID != retry.SourceEventID || first.Source != retry.Source || first.SourceTenant != retry.SourceTenant {
		t.Fatalf("redelivery identity drifted: %q vs %q", first.SourceEventID, retry.SourceEventID)
	}
	if !retry.Retry {
		t.Fatal("retry flag not carried")
	}
}

func TestMapEventDropsBotAndSelfEvents(t *testing.T) {
	// A message carrying a bot_id is another bot — dropped (loop guard).
	botEvt := []byte(`{"type":"event_callback","team_id":"T1","event_id":"Ev1","event":{"type":"message","bot_id":"B1","channel":"C1","ts":"1.1"}}`)
	if _, err := MapEvent(botEvt, "Ubot", false); !errors.Is(err, ErrIgnored) {
		t.Fatalf("bot event: err = %v, want ErrIgnored", err)
	}
	// The app's OWN bot user posting — dropped, or the app answers itself in a loop.
	selfEvt := []byte(`{"type":"event_callback","team_id":"T1","event_id":"Ev2","event":{"type":"message","user":"Ubot","channel":"C1","ts":"2.2"}}`)
	if _, err := MapEvent(selfEvt, "Ubot", false); !errors.Is(err, ErrIgnored) {
		t.Fatalf("self event: err = %v, want ErrIgnored", err)
	}
}

// Slack's real message_changed nests the author identity under `message` (message_deleted under
// `previous_message`): bot_id/user/ts/thread_ts are NOT top-level for those subtypes. The bot's OWN
// SLK-006 repair does a chat.update, which Slack re-emits as a message_changed carrying the BOT identity
// nested — so the loop guard MUST read the nested object, or the bot's own edit flows through as a
// KindCorrection and re-triggers a run (the exact loop SLK-008 exists to kill).
func TestMapEventDropsNestedBotEdit(t *testing.T) {
	edit := []byte(`{"type":"event_callback","team_id":"T1","event_id":"Ev9","event":{
		"type":"message","subtype":"message_changed","channel":"C1","ts":"9.9",
		"message":{"type":"message","bot_id":"B1","user":"Ubot","ts":"1.1","thread_ts":"1.1","text":"edited by bot"}
	}}`)
	if _, err := MapEvent(edit, "Ubot", false); !errors.Is(err, ErrIgnored) {
		t.Fatalf("nested bot edit: err = %v, want ErrIgnored (loop guard must see the nested bot_id)", err)
	}
	// A message_deleted nests the author under previous_message — a bot's own deletion is likewise dropped.
	del := []byte(`{"type":"event_callback","team_id":"T1","event_id":"Ev10","event":{
		"type":"message","subtype":"message_deleted","channel":"C1","ts":"10.1",
		"previous_message":{"type":"message","bot_id":"B1","ts":"1.1","thread_ts":"1.1"}
	}}`)
	if _, err := MapEvent(del, "Ubot", false); !errors.Is(err, ErrIgnored) {
		t.Fatalf("nested bot delete: err = %v, want ErrIgnored", err)
	}
}

// A HUMAN edit nests the real user + thread root under `message`; the correction must carry that user
// (SLK-004 authz reads it) and the thread ROOT (not the edit-event ts) so it correlates to the existing
// thread-session rather than claiming a new one.
func TestMapEventNestedHumanEditCorrelation(t *testing.T) {
	edit := []byte(`{"type":"event_callback","team_id":"T1","event_id":"Ev11","event":{
		"type":"message","subtype":"message_changed","channel":"C1","ts":"11.9",
		"message":{"type":"message","user":"U9","ts":"5.5","thread_ts":"100.0","text":"fixed typo"}
	}}`)
	ev, err := MapEvent(edit, "Ubot", false)
	if err != nil {
		t.Fatalf("nested human edit: err = %v", err)
	}
	if ev.Kind != KindCorrection {
		t.Fatalf("kind = %q, want correction", ev.Kind)
	}
	if ev.UserID != "U9" || ev.ThreadTS != "100.0" {
		t.Fatalf("correlation = user %q thread %q, want U9/100.0 (from the nested message, not top-level)", ev.UserID, ev.ThreadTS)
	}
}

func TestMapEventClassifiesEditsAndDeletes(t *testing.T) {
	edit := []byte(`{"type":"event_callback","team_id":"T1","event_id":"Ev3","event":{"type":"message","subtype":"message_changed","channel":"C1","ts":"3.3"}}`)
	if ev, _ := MapEvent(edit, "Ubot", false); ev.Kind != KindCorrection {
		t.Fatalf("edit kind = %q, want correction", ev.Kind)
	}
	del := []byte(`{"type":"event_callback","team_id":"T1","event_id":"Ev4","event":{"type":"message","subtype":"message_deleted","channel":"C1","ts":"4.4"}}`)
	if ev, _ := MapEvent(del, "Ubot", false); ev.Kind != KindTombstone {
		t.Fatalf("delete kind = %q, want tombstone", ev.Kind)
	}
}

func TestMapEventRejectsMalformedAndNonEvents(t *testing.T) {
	if _, err := MapEvent([]byte(`not json`), "", false); !errors.Is(err, ErrMalformed) {
		t.Fatalf("non-json: err = %v, want ErrMalformed", err)
	}
	// event_callback with no team/event id is unroutable.
	if _, err := MapEvent([]byte(`{"type":"event_callback"}`), "", false); !errors.Is(err, ErrMalformed) {
		t.Fatalf("no identity: err = %v, want ErrMalformed", err)
	}
	// A url_verification body is not a normal event — it is handled by ParseChallenge, not MapEvent.
	if _, err := MapEvent([]byte(`{"type":"url_verification","challenge":"abc"}`), "", false); !errors.Is(err, ErrNotAnEvent) {
		t.Fatalf("url_verification: err = %v, want ErrNotAnEvent", err)
	}
}

func TestParseChallengeReturnsTheHandshake(t *testing.T) {
	c, ok := ParseChallenge([]byte(`{"token":"x","challenge":"3eZbrw","type":"url_verification"}`))
	if !ok || c != "3eZbrw" {
		t.Fatalf("challenge = %q ok = %v, want 3eZbrw true", c, ok)
	}
	if _, ok := ParseChallenge([]byte(`{"type":"event_callback"}`)); ok {
		t.Fatal("event_callback wrongly parsed as a challenge")
	}
}

// Socket Mode frames wrap the SAME event payload the HTTP transport delivers, so an events_api frame's
// payload feeds MapEvent unchanged and yields the identical identity — the transport switch is invisible.
func TestUnwrapSocketFrameFeedsTheSameMapping(t *testing.T) {
	frame := []byte(`{"type":"events_api","envelope_id":"env-1","payload":{"type":"event_callback","team_id":"T1","event_id":"Ev7","event":{"type":"message","user":"U1","channel":"C1","ts":"7.7"}}}`)
	f, err := UnwrapSocketFrame(frame)
	if err != nil {
		t.Fatalf("UnwrapSocketFrame error = %v", err)
	}
	if f.Type != "events_api" || f.EnvelopeID != "env-1" {
		t.Fatalf("frame = %q/%q, want events_api/env-1", f.Type, f.EnvelopeID)
	}
	ev, err := MapEvent(f.Payload, "Ubot", false)
	if err != nil {
		t.Fatalf("MapEvent on socket payload error = %v", err)
	}
	if ev.SourceEventID != "Ev7" || ev.ChannelID != "C1" {
		t.Fatalf("socket-mode identity = %q/%q, want Ev7/C1", ev.SourceEventID, ev.ChannelID)
	}
}

// The MESSAGE the human wrote is what a run is about, so the adapter has to surface it as text — and the
// bot's own mention is addressing, not content. Everything else the human typed, including OTHER people's
// mentions, survives verbatim: "ask <@U999>" is a sentence, not a routing artefact.
func TestMapEventCarriesTheHumanTextWithoutTheBotMention(t *testing.T) {
	for _, tc := range []struct {
		name, text, want string
	}{
		{"leading mention", "<@Ubot> merhaba", "merhaba"},
		{"labelled mention", "<@Ubot|palai> merhaba", "merhaba"},
		{"trailing mention", "merhaba <@Ubot>", "merhaba"},
		{"mid-sentence mention", "hey <@Ubot> ship it", "hey ship it"},
		{"another user survives", "<@Ubot> ask <@U999> about it", "ask <@U999> about it"},
		{"no mention at all", "just a message", "just a message"},
		{"only the mention", "<@Ubot>", ""},
		{"unterminated mention is content", "a <@Ubot literal", "a <@Ubot literal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"type":"event_callback","team_id":"T1","event_id":"Ev01","event":` +
				`{"type":"app_mention","user":"U9","channel":"C1","ts":"111.1","text":` + quote(tc.text) + `}}`)
			ev, err := MapEvent(body, "Ubot", false)
			if err != nil {
				t.Fatalf("MapEvent error = %v", err)
			}
			if ev.Text != tc.want {
				t.Fatalf("Text = %q, want %q", ev.Text, tc.want)
			}
		})
	}
}

// An EDIT nests its text under `message` exactly as it nests its author, so the correction carries the
// corrected words — not the empty top-level text field.
func TestMapEventNestedEditCarriesTheEditedText(t *testing.T) {
	edit := []byte(`{"type":"event_callback","team_id":"T1","event_id":"Ev11","event":{
		"type":"message","subtype":"message_changed","channel":"C1","ts":"11.9",
		"message":{"type":"message","user":"U9","ts":"5.5","thread_ts":"100.0","text":"<@Ubot> fixed typo"}
	}}`)
	ev, err := MapEvent(edit, "Ubot", false)
	if err != nil {
		t.Fatalf("nested edit: err = %v", err)
	}
	if ev.Text != "fixed typo" {
		t.Fatalf("Text = %q, want %q (from the nested message, not top-level)", ev.Text, "fixed typo")
	}
}

// quote is json.Marshal for a string, so a table case can carry quotes and backslashes safely.
func quote(s string) string {
	raw, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// ---- E20 T2: the agent panel's three events ----------------------------------------------------------

// TestMapEventPanelSurfaceEventsBirthNoRun is the RED-first guarantee the panel could not ship without: the
// two SURFACE events are not conversation, and before this they classified as KindOther and mapped CLEANLY —
// so every panel open would have birthed a run with an empty prompt. They must now be a typed "handled, no
// run" outcome, ErrIgnored's sibling, on BOTH tabs and on both transports (the caller branches on the error,
// so neither transport can forget).
//
// CONTRACT: https://docs.slack.dev/reference/events/app_home_opened/ (checked 2026-07-27) — the payload is
// {type, user, channel, event_ts, tab, view}; tab is "home" or "messages"; "No scopes required!".
// https://docs.slack.dev/reference/events/app_context_changed/ (checked 2026-07-27) — the payload is
// {type, context:{entities:[…]}}; it carries NO channel and NO user at all.
func TestMapEventPanelSurfaceEventsBirthNoRun(t *testing.T) {
	for _, tc := range []struct {
		name, inner string
		wantType    string
		wantTab     string
		wantChannel string
	}{
		{
			name:     "app_home_opened messages tab",
			inner:    `{"type":"app_home_opened","user":"U9","channel":"D42","event_ts":"1515449522.000016","tab":"messages"}`,
			wantType: "app_home_opened", wantTab: "messages", wantChannel: "D42",
		},
		{
			name:     "app_home_opened home tab",
			inner:    `{"type":"app_home_opened","user":"U9","channel":"D42","event_ts":"1515449522.000016","tab":"home","view":{"id":"V1","type":"home"}}`,
			wantType: "app_home_opened", wantTab: "home", wantChannel: "D42",
		},
		{
			name:     "app_context_changed",
			inner:    `{"type":"app_context_changed","context":{"entities":[{"type":"slack#/types/channel_id","value":"C01234ABDCE","team_id":"T0ABCDE6543"}]}}`,
			wantType: "app_context_changed", wantTab: "", wantChannel: "",
		},
		{
			name:     "app_context_changed with an empty context object",
			inner:    `{"type":"app_context_changed","context":{}}`,
			wantType: "app_context_changed", wantTab: "", wantChannel: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"type":"event_callback","team_id":"T1","event_id":"Ev-panel","event":` + tc.inner + `}`)
			ev, err := MapEvent(body, "Ubot", false)
			if !errors.Is(err, ErrNoRun) {
				t.Fatalf("MapEvent err = %v, want ErrNoRun — a panel surface event must never reach admission", err)
			}
			// The event is still returned, populated: the caller acknowledges it and logs WHICH surface moved.
			// An error that also carries its value is unusual, so it is asserted rather than assumed.
			if ev.Type != tc.wantType || ev.Tab != tc.wantTab || ev.ChannelID != tc.wantChannel {
				t.Fatalf("no-run event = (%q,%q,%q), want (%q,%q,%q)",
					ev.Type, ev.Tab, ev.ChannelID, tc.wantType, tc.wantTab, tc.wantChannel)
			}
			if ev.SourceEventID != "Ev-panel" {
				t.Fatalf("no-run event lost its identity: SourceEventID = %q", ev.SourceEventID)
			}
		})
	}
}

// TestMapEventCarriesTheDMChannelType: the DM exemption in SlackAuthorizationPolicy.ChannelAllowed turns on
// this one field, so it has to come from the PAYLOAD Slack actually sends rather than from a "D" prefix
// nobody documented. A channel message must NOT report itself as a DM, or the exemption would swallow the
// allow-list whole.
//
// CONTRACT: https://docs.slack.dev/reference/events/message.im/ (checked 2026-07-27) — the inner event is
// {"type":"message","channel":"D024BE91L","user":…,"text":…,"ts":…,"event_ts":…,"channel_type":"im"} and the
// event requires the `im:history` scope.
func TestMapEventCarriesTheDMChannelType(t *testing.T) {
	dm := []byte(`{"type":"event_callback","team_id":"T123ABC456","api_app_id":"A0PNCHHK2","event_id":"Ev0PV52K21","event":{
		"type":"message","channel":"D024BE91L","user":"U2147483697","text":"Hello hello can you hear me?",
		"ts":"1355517523.000005","event_ts":"1355517523.000005","channel_type":"im"}}`)
	ev, err := MapEvent(dm, "Ubot", false)
	if err != nil {
		t.Fatalf("message.im: err = %v, want a normal mapped event — a DM message IS a message", err)
	}
	if !ev.IsDM() || ev.ChannelType != "im" {
		t.Fatalf("message.im: ChannelType = %q IsDM = %t, want im/true", ev.ChannelType, ev.IsDM())
	}
	if ev.Kind != KindMessage {
		t.Fatalf("message.im Kind = %q, want %q — a DM message earns no new Kind", ev.Kind, KindMessage)
	}
	if ev.ThreadTS != "1355517523.000005" || ev.Text != "Hello hello can you hear me?" {
		t.Fatalf("message.im correlation/text = %q/%q", ev.ThreadTS, ev.Text)
	}

	channel := []byte(`{"type":"event_callback","team_id":"T1","event_id":"Ev-chan","event":{
		"type":"message","channel":"C1","channel_type":"channel","user":"U9","ts":"5.5","text":"hi"}}`)
	ev, err = MapEvent(channel, "Ubot", false)
	if err != nil {
		t.Fatalf("channel message: err = %v", err)
	}
	if ev.IsDM() {
		t.Fatal("a channel message reported IsDM() — the allowed_channels exemption would then cover everything")
	}
}

// TestMapEventDropsTheBotsOwnDM is SLK-008 in a DM. The loop guard lives in MapEvent so it is
// transport- AND surface-independent, but "it should" is not evidence: an app answering itself inside the
// panel is a loop with no channel members to notice it.
func TestMapEventDropsTheBotsOwnDM(t *testing.T) {
	selfDM := []byte(`{"type":"event_callback","team_id":"T1","event_id":"Ev-selfdm","event":{
		"type":"message","channel":"D024BE91L","channel_type":"im","user":"Ubot","ts":"7.7","text":"my own answer"}}`)
	if _, err := MapEvent(selfDM, "Ubot", false); !errors.Is(err, ErrIgnored) {
		t.Fatalf("the app's own DM: err = %v, want ErrIgnored", err)
	}
	botDM := []byte(`{"type":"event_callback","team_id":"T1","event_id":"Ev-botdm","event":{
		"type":"message","channel":"D024BE91L","channel_type":"im","bot_id":"B1","ts":"8.8","text":"another bot"}}`)
	if _, err := MapEvent(botDM, "Ubot", false); !errors.Is(err, ErrIgnored) {
		t.Fatalf("another bot's DM: err = %v, want ErrIgnored", err)
	}
}
