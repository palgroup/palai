package slack

import (
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
