package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// A RATE LIMIT IS THE ONE INBOUND FACT THAT IS ABOUT THE ENVELOPES THAT NEVER ARRIVED.
//
// Every other refusal in OnEventsAPI is about one envelope this process is holding. This one says Slack
// is dropping deliveries for the whole workspace before they reach the socket — so the turns being lost
// have no event_id, no ack, and no later trace. A log line is the entire response available, which makes
// what the line SAYS the whole of the feature.
//
// Before this, the payload fell into ErrNotAnEvent and dispatch.go printed "a malformed events_api
// envelope arrived", which sends a reader hunting a parsing bug while messages are being dropped.

// capturingBot builds the production dispatcher over a log sink, so the assertion is about the line an
// operator reads rather than about a return value nobody sees (OnEventsAPI returns nothing on purpose).
func capturingBot(t *testing.T) (*dispatcher, func() string) {
	t.Helper()
	d, _, _, _, _ := testBot(t, nil, nil)
	var mu sync.Mutex
	var sb strings.Builder
	d.logf = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		sb.WriteString(fmt.Sprintf(format, args...))
		sb.WriteString("\n")
	}
	return d, func() string {
		mu.Lock()
		defer mu.Unlock()
		return sb.String()
	}
}

// TestAnAppRateLimitedNotificationNamesTheWorkspaceAndTheMinute drives the production surface —
// dispatcher.OnEventsAPI, the method the Socket Mode loop calls — with the payload Slack's own Events API
// page prints, and asserts the line an operator would find.
//
// CONTRACT: https://docs.slack.dev/apis/events-api/ (checked 2026-08-05) prints this exact payload shape.
// Whether Socket Mode delivers it is NOT documented and has not been observed here; see
// slack.ErrAppRateLimited, which says so rather than implying otherwise. What this test pins is that IF it
// arrives in an events_api envelope, it is named instead of being called malformed.
func TestAnAppRateLimitedNotificationNamesTheWorkspaceAndTheMinute(t *testing.T) {
	d, logged := capturingBot(t)

	d.OnEventsAPI(context.Background(), json.RawMessage(
		`{"token":"Jhj5dZrVaK7ZwHHjRyZWjbDl","type":"app_rate_limited","team_id":"T0AMPM5JX8U",`+
			`"minute_rate_limited":1518467820,"api_app_id":"A123ABC456"}`))

	line := logged()
	if strings.Contains(line, "malformed") {
		t.Fatalf("a rate limit was logged as a malformed envelope, which sends a reader looking for a "+
			"parsing bug while Slack is dropping this workspace's messages:\n%s", line)
	}
	// THE TWO FIELDS THE PAYLOAD CARRIES, not just the word "rate". A line that says only "rate limited"
	// cannot tell an operator WHICH workspace is being shed or WHEN it started, and Slack repeats the
	// notification every minute — so without the minute a reader cannot tell one outage from two.
	for _, want := range []string{"T0AMPM5JX8U", "1518467820"} {
		if !strings.Contains(line, want) {
			t.Fatalf("the rate-limit line does not carry %q, so the record of the gap is unusable:\n%s", want, line)
		}
	}
}

// TestAnAppRateLimitedNotificationBirthsNoTurn is the other half, and it is not obvious from the first:
// the payload carries a team_id and nothing else the mapping needs, so a branch that fell through would
// reach the run-birth checks with an empty channel and an empty event_id. Nothing may be created from a
// notification that no human sent.
func TestAnAppRateLimitedNotificationBirthsNoTurn(t *testing.T) {
	d, fp, streamSlack, _, _ := testBot(t, nil, nil)
	d.logf = func(string, ...any) {}

	d.OnEventsAPI(context.Background(), json.RawMessage(
		`{"type":"app_rate_limited","team_id":"T1","minute_rate_limited":1518467820,"api_app_id":"A1"}`))

	fp.mu.Lock()
	responses, sessions := len(fp.responses), fp.sessions
	fp.mu.Unlock()
	if responses != 0 || sessions != 0 {
		t.Fatalf("a rate-limit notification created %d session(s) and %d response(s); it is Slack telling "+
			"this app to slow down, and answering it with a run is the opposite", sessions, responses)
	}
	streamSlack.mu.Lock()
	appended, plans, stopped := len(streamSlack.appended), len(streamSlack.plans), streamSlack.stopped
	streamSlack.mu.Unlock()
	if appended != 0 || plans != 0 || stopped != 0 {
		t.Fatalf("a rate-limit notification wrote to Slack (%d appends, %d plan updates, %d stops) — into a "+
			"workspace Slack is already shedding traffic for", appended, plans, stopped)
	}
}
