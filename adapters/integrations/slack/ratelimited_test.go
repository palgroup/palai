package slack

import (
	"errors"
	"testing"
)

// THE MAPPING'S OWN HALF OF THE RATE-LIMIT ANSWER, pinned separately from the bot's log line because the
// two can rot apart: this package serves every transport, and a transport that never learns about the
// sentinel would fall back on `err != nil` — which for this payload used to mean "malformed".

// TestMapEventAnswersAnAppRateLimitedPayloadWithItsOwnSentinel is the classification. The payload is the
// one https://docs.slack.dev/apis/events-api/ prints (checked 2026-08-05), verbatim.
func TestMapEventAnswersAnAppRateLimitedPayloadWithItsOwnSentinel(t *testing.T) {
	body := []byte(`{"token":"Jhj5dZrVaK7ZwHHjRyZWjbDl","type":"app_rate_limited","team_id":"T123ABC456",` +
		`"minute_rate_limited":1518467820,"api_app_id":"A123ABC456"}`)

	ev, err := MapEvent(body, "U_BOT", false)
	if !errors.Is(err, ErrAppRateLimited) {
		t.Fatalf("MapEvent answered %v, want ErrAppRateLimited", err)
	}
	// NOT ErrNotAnEvent AND NOT ErrMalformed, asserted rather than left to the sentinel above: those are the
	// two answers this payload used to get, and a caller that switches on them would carry on treating a
	// delivery outage as a parsing problem even with the new sentinel present.
	if errors.Is(err, ErrNotAnEvent) || errors.Is(err, ErrMalformed) {
		t.Fatalf("a rate limit still answers %v — the sentinel was added without taking the payload off the "+
			"malformed/not-an-event paths, so every existing caller reads it exactly as it did before", err)
	}
	// THE POPULATED EVENT IS THE FEATURE, not a courtesy: a sentinel alone cannot say which workspace is
	// being shed, and this bot may serve more than one.
	if ev.TeamID != "T123ABC456" || ev.SourceTenant != "T123ABC456" {
		t.Fatalf("team = %q/%q, want the workspace the payload names in both the Slack and the canonical field",
			ev.TeamID, ev.SourceTenant)
	}
	if ev.RateLimitedMinute != 1518467820 {
		t.Fatalf("RateLimitedMinute = %d, want the minute the payload names — Slack repeats this notification "+
			"every minute it keeps shedding, so without it two outages read as one", ev.RateLimitedMinute)
	}
	if ev.Type != "app_rate_limited" {
		t.Fatalf("Type = %q, want the payload's own outer type so a log line can name it without re-decoding", ev.Type)
	}
}

// TestARealEventCarriesNoRateLimitedMinute is the other direction, and it is the one that would catch the
// field being populated from something else: a new struct field decoded off every envelope is a field that
// can pick up a value on envelopes it has no business describing.
func TestARealEventCarriesNoRateLimitedMinute(t *testing.T) {
	body := []byte(`{"type":"event_callback","team_id":"T1","event_id":"Ev1","minute_rate_limited":99,
		"event":{"type":"app_mention","user":"U_HUMAN","text":"<@U_BOT> hi","ts":"100.001","channel":"C1"}}`)

	ev, err := MapEvent(body, "U_BOT", false)
	if err != nil {
		t.Fatalf("MapEvent refused an ordinary app_mention: %v", err)
	}
	if ev.RateLimitedMinute != 0 {
		t.Fatalf("an ordinary event carries RateLimitedMinute = %d; the field is populated only alongside "+
			"ErrAppRateLimited, and a real event that carries one would make the bot's loudest line fire on a "+
			"working workspace", ev.RateLimitedMinute)
	}
}
