package slack

import "testing"

// D2 (E19 plan §3.5): Slack's redelivery carries a REASON alongside the retry number, and the six values are
// enumerated by the published Events API page. The reason is the difference between "we answered too slowly"
// (http_timeout — the only honest signal that the 3-second ack budget is not holding) and "we answered with an
// error" (http_error) — collapsing them loses the one measurement worth having.
func TestRetryReasonKeepsTheDocumentedSetAndNothingElse(t *testing.T) {
	for _, want := range []string{
		RetryReasonHTTPTimeout, RetryReasonHTTPError, RetryReasonConnectionFailed,
		RetryReasonSSLError, RetryReasonTooManyRedirects, RetryReasonUnknownError,
	} {
		if got := RetryReason(want); got != want {
			t.Fatalf("RetryReason(%q) = %q, want it preserved — the value is documented", want, got)
		}
	}
	// An empty header is NOT a retry; it must stay empty rather than become a counted reason.
	if got := RetryReason(""); got != "" {
		t.Fatalf("RetryReason(\"\") = %q, want \"\" — a first delivery carries no reason", got)
	}
	// Anything outside the documented set folds onto unknown_error. The header is NOT covered by the v0
	// signature (only the timestamp and the body are), so an unbounded value must never reach a counter.
	for _, weird := range []string{"http_timeout ", "TEAPOT", "'; DROP TABLE", "http_timeoutx"} {
		if got := RetryReason(weird); got != RetryReasonUnknownError {
			t.Fatalf("RetryReason(%q) = %q, want %q — an undocumented reason must fold, never widen the set",
				weird, got, RetryReasonUnknownError)
		}
	}
}

// The receiver has to resolve WHICH workspace an inbound callback belongs to before it can resolve that
// workspace's signing secret — i.e. strictly BEFORE the signature verifies. ParseTeam exists to make that
// pre-authentication read explicit and narrow: it yields a LOOKUP KEY and nothing else.
func TestParseTeamYieldsTheLookupKeyOnly(t *testing.T) {
	body := []byte(`{"type":"event_callback","team_id":"T1","enterprise_id":"E1","api_app_id":"A1",
		"event_id":"Ev1","event":{"type":"app_mention","user":"U1","channel":"C1","ts":"1.0"}}`)
	team, enterprise, ok := ParseTeam(body)
	if !ok || team != "T1" || enterprise != "E1" {
		t.Fatalf("ParseTeam = (%q,%q,%v), want (T1,E1,true)", team, enterprise, ok)
	}

	// No team id ⇒ no lookup key ⇒ the caller cannot resolve a secret and must refuse. Non-JSON likewise.
	for _, unusable := range [][]byte{
		[]byte(`{"type":"event_callback","event_id":"Ev1"}`),
		[]byte(`not json`),
		[]byte(`{"type":"url_verification","challenge":"c"}`), // the handshake carries NO team_id at all
	} {
		if team, _, ok := ParseTeam(unusable); ok {
			t.Fatalf("ParseTeam(%s) = (%q,true), want ok=false — an unresolvable body has no lookup key", unusable, team)
		}
	}
}
