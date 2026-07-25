package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/integrations/slack"
)

// The Slack Events API route (E19 T1), proven against a FAKE Slack peer built to the PUBLISHED contract.
//
// Every line of the peer below carries a CONTRACT: comment naming the page it came from and the date it was
// checked, because the failure mode this task exists to avoid is a fixture built to our own convenience: E17
// T10's console fake invented an approval event the real API never emits, and every proof that ran against it
// was evidence about the fixture, not about Slack. A fake that is wrong in our favour is worse than no fake.
//
// HONEST CEILING: nothing here is evidence about a real Slack workspace. The peer is in-process; the signing
// secret is a test string; there is no socket to slack.com. The real-workspace receipt is §6 leg 1.

const (
	// CONTRACT: https://docs.slack.dev/authentication/verifying-requests-from-slack/ (checked 2026-07-25).
	// The two headers Slack signs a request with.
	hdrTimestamp = "X-Slack-Request-Timestamp"
	hdrSignature = "X-Slack-Signature"
	// CONTRACT: https://docs.slack.dev/apis/events-api/ (checked 2026-07-25). A redelivery carries the attempt
	// number (1..3) and the reason.
	hdrRetryNum    = "X-Slack-Retry-Num"
	hdrRetryReason = "X-Slack-Retry-Reason"
	// CONTRACT: https://docs.slack.dev/apis/events-api/ (checked 2026-07-25). "you can also send a header of
	// x-slack-no-retry: 1" on a non-200 to stop Slack retrying.
	hdrNoRetry = "X-Slack-No-Retry"
)

// fakeSlackPeer signs and posts exactly the way the published contract says Slack does. The MAC construction
// is written out LONGHAND here rather than delegating to the adapter's own signer: a fixture that calls the
// code under test cannot detect that code drifting from the contract (this is the D9 regression pin).
type fakeSlackPeer struct {
	url    string
	secret []byte
}

// post signs `body` at `at` and delivers it. retryNum/retryReason are set only when non-empty, so a first
// delivery carries neither — which is what Slack does.
//
// CONTRACT: https://docs.slack.dev/authentication/verifying-requests-from-slack/ (checked 2026-07-25) —
// base string is exactly 'v0:' + timestamp + ':' + request_body, MAC is HMAC-SHA256 rendered hex, and the
// X-Slack-Signature VALUE is prefixed 'v0='. The body is posted as application/json.
func (p fakeSlackPeer) post(t *testing.T, body []byte, at time.Time, retryNum, retryReason string) *http.Response {
	t.Helper()
	timestamp := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, p.secret)
	mac.Write([]byte("v0:" + timestamp + ":"))
	mac.Write(body)
	return p.postSigned(t, body, timestamp, "v0="+hex.EncodeToString(mac.Sum(nil)), retryNum, retryReason)
}

// postSigned delivers a body under a CALLER-CHOSEN timestamp/signature pair, so a test can present a forged
// or absent MAC without going through the honest signer above.
func (p fakeSlackPeer) postSigned(t *testing.T, body []byte, timestamp, signature, retryNum, retryReason string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, p.url+"/v1/slack/events", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("build slack request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(hdrTimestamp, timestamp)
	req.Header.Set(hdrSignature, signature)
	if retryNum != "" {
		req.Header.Set(hdrRetryNum, retryNum)
	}
	if retryReason != "" {
		req.Header.Set(hdrRetryReason, retryReason)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/slack/events: %v", err)
	}
	return resp
}

// CONTRACT: https://docs.slack.dev/apis/events-api/ (checked 2026-07-25). The Events API outer envelope:
// type=event_callback, the workspace's team_id, a globally-unique event_id, and the inner event object.
func eventCallbackBody(team, eventID, innerType, user, channel, ts, threadTS string) []byte {
	inner := map[string]any{"type": innerType, "user": user, "channel": channel, "ts": ts}
	if threadTS != "" {
		inner["thread_ts"] = threadTS
	}
	raw, _ := json.Marshal(map[string]any{
		"type": "event_callback", "team_id": team, "api_app_id": "A0001", "event_id": eventID,
		"event_time": 1700000000, "event": inner,
	})
	return raw
}

// stubSlackBridge is the admission bridge seen from the route: it records every call so a test can assert
// WHAT the handler did and IN WHICH ORDER, and it can be made to fail each step independently.
type stubSlackBridge struct {
	conn       SlackConnectionRef
	found      bool
	secret     []byte
	resolveErr error

	calls    []string // "resolve", "verify", "admit" — the ORDER is the contract
	admitted []slack.Event
	scopes   []string // the org/project each admission ran under
	outcome  SlackAdmitOutcome
	admitErr error
}

func (s *stubSlackBridge) ResolveConnection(_ context.Context, teamID, enterpriseID string) (SlackConnectionRef, bool, error) {
	s.calls = append(s.calls, "resolve")
	if s.resolveErr != nil {
		return SlackConnectionRef{}, false, s.resolveErr
	}
	if !s.found || teamID != s.conn.TeamID {
		return SlackConnectionRef{}, false, nil
	}
	return s.conn, true, nil
}

func (s *stubSlackBridge) VerifySignature(_ context.Context, _ SlackConnectionRef, timestamp, signature string, body []byte) error {
	s.calls = append(s.calls, "verify")
	// The REAL adapter verify, against the REAL clock — a stale fixture is stale for the same reason a stale
	// live request would be.
	return slack.VerifySignature(s.secret, timestamp, signature, body, time.Now(), slack.DefaultTolerance)
}

func (s *stubSlackBridge) Admit(_ context.Context, conn SlackConnectionRef, ev slack.Event) (SlackAdmitOutcome, error) {
	s.calls = append(s.calls, "admit")
	s.admitted = append(s.admitted, ev)
	s.scopes = append(s.scopes, conn.Org+"/"+conn.Project)
	return s.outcome, s.admitErr
}

// slackPeerAgainst mounts the real router with the Slack surface wired to `bridge` and returns a peer aimed
// at it. The route rides the UNAUTHENTICATED top mux (the v0 signature is its auth), so no bearer is set.
func slackPeerAgainst(t *testing.T, bridge SlackEventsAPI, secret []byte) (fakeSlackPeer, func()) {
	t.Helper()
	router := NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		SSEConfig{}, nil, nil, WithSlack(bridge))
	ts := httptest.NewServer(router)
	return fakeSlackPeer{url: ts.URL, secret: secret}, ts.Close
}

func newSlackBridge(secret []byte) *stubSlackBridge {
	return &stubSlackBridge{
		conn:    SlackConnectionRef{ID: "slkc_1", Org: "org_real", Project: "prj_real", TeamID: "T100", BotUserID: "Ubot"},
		found:   true,
		secret:  secret,
		outcome: SlackAdmitOutcome{ResponseID: "resp_1", SessionID: "ses_1"},
	}
}

// TestSlackURLVerificationEchoesTheChallengeBeforeAnyLookup pins the handshake. It runs BEFORE the signature
// check for a structural reason, not a convenient one.
//
// CONTRACT: https://docs.slack.dev/apis/events-api/ (checked 2026-07-25) — when a Request URL is configured
// Slack POSTs {"token":…,"challenge":…,"type":"url_verification"} and the receiver echoes the challenge back.
// That body carries NO team_id, so there is no connection to resolve and therefore no signing secret to
// verify against: the order is forced by the payload shape.
func TestSlackURLVerificationEchoesTheChallengeBeforeAnyLookup(t *testing.T) {
	secret := []byte("test-signing-secret")
	bridge := newSlackBridge(secret)
	peer, done := slackPeerAgainst(t, bridge, secret)
	defer done()

	challenge := "3eZbrw1aBm2rZgRNFdxV2595E9CY3gmdALWMmHkvFXO7tYXAYM8P"
	body := []byte(`{"token":"deprecated","challenge":"` + challenge + `","type":"url_verification"}`)
	resp := peer.post(t, body, time.Now(), "", "")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("url_verification = %d, want 200", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != challenge {
		t.Fatalf("challenge echo = %q, want the PLAINTEXT challenge %q", got, challenge)
	}
	if len(bridge.calls) != 0 {
		t.Fatalf("the handshake touched the bridge (%v); it carries no team_id, so nothing may be resolved or verified", bridge.calls)
	}
}

// TestSlackChallengeEchoIsBoundedAndNotSniffable hardens the ONE surface that renders caller-chosen bytes on
// this deployment's own origin. The handshake is unauthenticated by construction (it carries no team_id, so
// there is no secret to verify against) and it echoes the challenge verbatim, so without a bound it reflects
// up to the whole body limit of attacker-chosen content back as a document the browser may sniff. It cannot
// bypass verification — but it is the same class as the console-relay finding, and the fix is three lines.
//
// CONTRACT: https://docs.slack.dev/apis/events-api/ (checked 2026-07-25) — the challenge is a short random
// token (the documented example is 50-odd alphanumerics), echoed back to confirm the Request URL.
func TestSlackChallengeEchoIsBoundedAndNotSniffable(t *testing.T) {
	secret := []byte("test-signing-secret")

	t.Run("a real challenge is served as inert text", func(t *testing.T) {
		bridge := newSlackBridge(secret)
		peer, done := slackPeerAgainst(t, bridge, secret)
		defer done()
		challenge := "3eZbrw1aBm2rZgRNFdxV2595E9CY3gmdALWMmHkvFXO7tYXAYM8P"
		resp := peer.post(t, []byte(`{"token":"deprecated","challenge":"`+challenge+`","type":"url_verification"}`), time.Now(), "", "")
		defer resp.Body.Close()
		if got, want := resp.Header.Get("Content-Type"), "text/plain; charset=utf-8"; got != want {
			t.Fatalf("Content-Type = %q, want %q — an unqualified text/plain leaves the encoding to the browser", got, want)
		}
		if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Fatalf("X-Content-Type-Options = %q, want \"nosniff\" — without it a browser may sniff the echoed bytes into a document on our own origin", got)
		}
		body, _ := io.ReadAll(resp.Body)
		if string(body) != challenge {
			t.Fatalf("challenge echo = %q, want %q", body, challenge)
		}
	})

	for _, tc := range []struct {
		name      string
		challenge string
	}{
		{"markup", `<script>alert(document.domain)</script>`},
		{"an oversized reflection", strings.Repeat("A", 1025)},
		{"an empty challenge", ""},
	} {
		t.Run(tc.name+" is refused", func(t *testing.T) {
			bridge := newSlackBridge(secret)
			peer, done := slackPeerAgainst(t, bridge, secret)
			defer done()
			body, _ := json.Marshal(map[string]any{"type": "url_verification", "challenge": tc.challenge})
			resp := peer.post(t, body, time.Now(), "", "")
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				echoed, _ := io.ReadAll(resp.Body)
				t.Fatalf("challenge %q answered %d (%q), want 400 — a real handshake carries a short alphanumeric token, so anything else is a reflection surface, not a handshake",
					tc.challenge, resp.StatusCode, echoed)
			}
			if len(bridge.calls) != 0 {
				t.Fatalf("the refused handshake touched the bridge (%v)", bridge.calls)
			}
		})
	}
}

// TestSlackEventAdmitsThroughTheBridgeInContractOrder is the happy path AND the order pin: resolve (to learn
// the secret) → verify → admit. Mapping strictly after authentication is the whole security posture of this
// route, so the recorded call order is asserted rather than assumed.
func TestSlackEventAdmitsThroughTheBridgeInContractOrder(t *testing.T) {
	secret := []byte("test-signing-secret")
	bridge := newSlackBridge(secret)
	peer, done := slackPeerAgainst(t, bridge, secret)
	defer done()

	resp := peer.post(t, eventCallbackBody("T100", "Ev100", "app_mention", "U1", "C1", "1700000000.000100", ""), time.Now(), "", "")
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("a genuine signed app_mention = %d, want a 2xx ack", resp.StatusCode)
	}
	if resp.Header.Get(hdrNoRetry) != "" {
		t.Fatalf("an ACCEPTED event carries %s=%q; the suppress header belongs only on a terminal reject",
			hdrNoRetry, resp.Header.Get(hdrNoRetry))
	}
	if want := []string{"resolve", "verify", "admit"}; !equalStrings(bridge.calls, want) {
		t.Fatalf("handler order = %v, want %v — the payload may only be MAPPED after its signature verifies", bridge.calls, want)
	}
	if len(bridge.admitted) != 1 {
		t.Fatalf("%d admissions, want exactly 1", len(bridge.admitted))
	}
	ev := bridge.admitted[0]
	if ev.SourceEventID != "Ev100" || ev.TeamID != "T100" || ev.ThreadTS != "1700000000.000100" {
		t.Fatalf("admitted event = %+v, want the canonical identity (Ev100/T100/thread root)", ev)
	}
}

// TestSlackTerminalRejectsCarryNoRetry closes D1 (plan §3.5). A poison event answered with a bare non-200 is
// pulled three more times: retry amplification, and for a signature reject an amplification surface an
// attacker controls.
//
// CONTRACT: https://docs.slack.dev/apis/events-api/ (checked 2026-07-25) — a delivery without a 2xx inside 3
// seconds is retried three times (immediately, +1min, +5min), and "x-slack-no-retry: 1" on the response
// suppresses that.
func TestSlackTerminalRejectsCarryNoRetry(t *testing.T) {
	secret := []byte("test-signing-secret")
	body := eventCallbackBody("T100", "Ev200", "app_mention", "U1", "C1", "1700000000.000100", "")

	cases := []struct {
		name string
		want int
		post func(t *testing.T, peer fakeSlackPeer, bridge *stubSlackBridge) *http.Response
	}{
		{
			name: "unknown workspace", want: http.StatusNotFound,
			post: func(t *testing.T, peer fakeSlackPeer, bridge *stubSlackBridge) *http.Response {
				bridge.found = false
				return peer.post(t, body, time.Now(), "", "")
			},
		},
		{
			name: "disabled connection", want: http.StatusNotFound,
			post: func(t *testing.T, peer fakeSlackPeer, bridge *stubSlackBridge) *http.Response {
				bridge.conn.Disabled = true
				return peer.post(t, body, time.Now(), "", "")
			},
		},
		{
			name: "no team_id to resolve a secret with", want: http.StatusBadRequest,
			post: func(t *testing.T, peer fakeSlackPeer, _ *stubSlackBridge) *http.Response {
				return peer.post(t, []byte(`{"type":"event_callback","event_id":"Ev200"}`), time.Now(), "", "")
			},
		},
		{
			name: "forged signature", want: http.StatusUnauthorized,
			post: func(t *testing.T, peer fakeSlackPeer, _ *stubSlackBridge) *http.Response {
				return peer.postSigned(t, body, strconv.FormatInt(time.Now().Unix(), 10),
					"v0="+strings.Repeat("00", 32), "", "")
			},
		},
		{
			// CONTRACT: same page — "if the timestamp is more than five minutes from local time, ignore it".
			name: "stale timestamp (replay window)", want: http.StatusUnauthorized,
			post: func(t *testing.T, peer fakeSlackPeer, _ *stubSlackBridge) *http.Response {
				return peer.post(t, body, time.Now().Add(-10*time.Minute), "", "")
			},
		},
		{
			name: "signed but malformed envelope", want: http.StatusBadRequest,
			post: func(t *testing.T, peer fakeSlackPeer, _ *stubSlackBridge) *http.Response {
				return peer.post(t, []byte(`{"type":"event_callback","team_id":"T100"}`), time.Now(), "", "")
			},
		},
		{
			name: "admission refused the event permanently", want: http.StatusUnprocessableEntity,
			post: func(t *testing.T, peer fakeSlackPeer, bridge *stubSlackBridge) *http.Response {
				bridge.outcome = SlackAdmitOutcome{Rejected: "the pinned revision is a draft"}
				return peer.post(t, body, time.Now(), "", "")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bridge := newSlackBridge(secret)
			peer, done := slackPeerAgainst(t, bridge, secret)
			defer done()
			resp := tc.post(t, peer, bridge)
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
			if got := resp.Header.Get(hdrNoRetry); got != "1" {
				t.Fatalf("a TERMINAL reject answered %d with %s=%q, want \"1\" — without it Slack pulls this poison event three more times (plan §3.5 D1)",
					resp.StatusCode, hdrNoRetry, got)
			}
		})
	}
}

// TestSlackTransientFailureInvitesTheRetry is D1's other half: a database that is down is not a poison event.
// Suppressing the retry there would DROP a legitimate event, so the header must be absent.
func TestSlackTransientFailureInvitesTheRetry(t *testing.T) {
	secret := []byte("test-signing-secret")
	body := eventCallbackBody("T100", "Ev300", "app_mention", "U1", "C1", "1700000000.000100", "")

	for _, tc := range []struct {
		name   string
		break_ func(*stubSlackBridge)
	}{
		{"connection lookup fails", func(b *stubSlackBridge) { b.resolveErr = errors.New("dial tcp: connection refused") }},
		{"admission fails", func(b *stubSlackBridge) { b.admitErr = errors.New("dial tcp: connection refused") }},
		{"admission is shedding load", func(b *stubSlackBridge) {
			b.outcome = SlackAdmitOutcome{Rejected: "the run queue is full", Retryable: true}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bridge := newSlackBridge(secret)
			tc.break_(bridge)
			peer, done := slackPeerAgainst(t, bridge, secret)
			defer done()
			resp := peer.post(t, body, time.Now(), "", "")
			defer resp.Body.Close()
			if resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want a retryable 5xx/429 — a transient failure is not a client error", resp.StatusCode)
			}
			if got := resp.Header.Get(hdrNoRetry); got != "" {
				t.Fatalf("a TRANSIENT failure carried %s=%q; suppressing the retry there DROPS a legitimate event", hdrNoRetry, got)
			}
		})
	}
}

// TestSlackRetryStormRecordsTheReason closes D2 (plan §3.5): the reason distinguishes "we were too slow" from
// "we answered with an error", and http_timeout is the only honest signal that the 3-second ack budget is not
// holding. Knowing only the retry NUMBER cannot tell them apart.
//
// CONTRACT: https://docs.slack.dev/apis/events-api/ (checked 2026-07-25) — a retry carries x-slack-retry-num
// (1..3) and x-slack-retry-reason, one of http_timeout / http_error / connection_failed / ssl_error /
// too_many_redirects / unknown_error.
func TestSlackRetryStormRecordsTheReason(t *testing.T) {
	secret := []byte("test-signing-secret")
	bridge := newSlackBridge(secret)
	handler := &slackHandler{slack: bridge, logf: func(string, ...any) {}}
	ts := httptest.NewServer(slackRouterFor(handler))
	defer ts.Close()
	peer := fakeSlackPeer{url: ts.URL, secret: secret}

	body := eventCallbackBody("T100", "Ev400", "app_mention", "U1", "C1", "1700000000.000100", "")
	// The first delivery carries no retry headers at all, then Slack's three attempts.
	peer.post(t, body, time.Now(), "", "").Body.Close()
	for i, reason := range []string{slack.RetryReasonHTTPTimeout, slack.RetryReasonHTTPTimeout, slack.RetryReasonHTTPError} {
		resp := peer.post(t, body, time.Now(), strconv.Itoa(i+1), reason)
		resp.Body.Close()
	}
	// An undocumented reason must fold rather than open a new counter (the header is NOT signed).
	peer.post(t, body, time.Now(), "3", "reason-we-invented").Body.Close()

	got := handler.retries.snapshot()
	if got[slack.RetryReasonHTTPTimeout] != 2 {
		t.Fatalf("http_timeout = %d, want 2 — it is counted SEPARATELY because it is the only honest signal that the 3s ack budget is not holding (plan §3.5 D2)", got[slack.RetryReasonHTTPTimeout])
	}
	if got[slack.RetryReasonHTTPError] != 1 {
		t.Fatalf("http_error = %d, want 1 — a slow ack and a failed ack are different defects", got[slack.RetryReasonHTTPError])
	}
	if got[slack.RetryReasonUnknownError] != 1 {
		t.Fatalf("unknown_error = %d, want 1 — an undocumented reason folds onto the documented catch-all", got[slack.RetryReasonUnknownError])
	}
	if total := got[slack.RetryReasonHTTPTimeout] + got[slack.RetryReasonHTTPError] + got[slack.RetryReasonUnknownError]; total != 4 {
		t.Fatalf("%d reasons recorded across 5 deliveries, want 4 — the FIRST delivery is not a retry", total)
	}
	// Every delivery still reached admission: collapsing the redelivery is the ADMITTER's idempotency
	// reservation on (team_id, event_id), not something the route decides for itself.
	if len(bridge.admitted) != 5 {
		t.Fatalf("%d admissions from 5 deliveries, want 5 — the route must not invent a second dedupe", len(bridge.admitted))
	}
	for _, ev := range bridge.admitted[1:] {
		if !ev.Retry {
			t.Fatal("a redelivery was not flagged as one; the retry hint is advisory but must survive the mapping")
		}
	}
}

// TestSlackBotSelfEventIsAckedAndAdmitsNothing is SLK-008 over the wire: the app's own message must not birth
// a run, or it answers itself forever. Slack still has to be told the delivery succeeded.
func TestSlackBotSelfEventIsAckedAndAdmitsNothing(t *testing.T) {
	secret := []byte("test-signing-secret")
	bridge := newSlackBridge(secret)
	peer, done := slackPeerAgainst(t, bridge, secret)
	defer done()

	// The app's OWN bot user posts in the channel it is watching.
	resp := peer.post(t, eventCallbackBody("T100", "Ev500", "message", "Ubot", "C1", "1700000000.000100", ""), time.Now(), "", "")
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("bot-self event = %d, want a 2xx ack (the delivery SUCCEEDED; it just produces nothing)", resp.StatusCode)
	}
	if resp.Header.Get(hdrNoRetry) != "" {
		t.Fatalf("an ACKED event carries %s; the header belongs on non-200s only", hdrNoRetry)
	}
	if len(bridge.admitted) != 0 {
		t.Fatalf("the loop guard let %d admission(s) through; the app would answer its own message", len(bridge.admitted))
	}
}

// TestSlackTenantComesOnlyFromTheResolvedConnection is the §2/§38.6 invariant on this transport: a payload
// field may never select a tenant. The body below carries every tenant-shaped field a forger would try; the
// admission must still run under the org/project of the connection the TEAM ID resolved.
func TestSlackTenantComesOnlyFromTheResolvedConnection(t *testing.T) {
	secret := []byte("test-signing-secret")
	bridge := newSlackBridge(secret)
	peer, done := slackPeerAgainst(t, bridge, secret)
	defer done()

	forged, _ := json.Marshal(map[string]any{
		"type": "event_callback", "team_id": "T100", "event_id": "Ev600",
		"organization_id": "org_attacker", "project_id": "prj_attacker",
		"org": "org_attacker", "project": "prj_attacker", "scope": "org_attacker/prj_attacker",
		"event": map[string]any{
			"type": "app_mention", "user": "U1", "channel": "C1", "ts": "1700000000.000100",
			"organization_id": "org_attacker", "project_id": "prj_attacker",
		},
	})
	resp := peer.post(t, forged, time.Now(), "", "")
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("status = %d, want 2xx (the forged fields are IGNORED, not a rejection)", resp.StatusCode)
	}
	if len(bridge.scopes) != 1 || bridge.scopes[0] != "org_real/prj_real" {
		t.Fatalf("admission scopes = %v, want [org_real/prj_real] — the tenant comes from the RESOLVED connection row and from nothing in the payload", bridge.scopes)
	}
}

// TestSlackOversizedBodyIsRefusedBeforeTheMAC guards the unauthenticated edge: the body has to be bounded
// BEFORE it is hashed, or an attacker who cannot sign anything can still make the receiver HMAC gigabytes.
func TestSlackOversizedBodyIsRefusedBeforeTheMAC(t *testing.T) {
	secret := []byte("test-signing-secret")
	bridge := newSlackBridge(secret)
	peer, done := slackPeerAgainst(t, bridge, secret)
	defer done()

	huge := append([]byte(`{"type":"event_callback","team_id":"T100","event_id":"Ev700","pad":"`),
		append([]byte(strings.Repeat("A", maxBodyBytes+16)), []byte(`"}`)...)...)
	resp := peer.post(t, huge, time.Now(), "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body = %d, want 413", resp.StatusCode)
	}
	if resp.Header.Get(hdrNoRetry) != "1" {
		t.Fatalf("an oversized body is terminal; it must carry %s=1 rather than be pulled three more times", hdrNoRetry)
	}
	if len(bridge.calls) != 0 {
		t.Fatalf("the oversized body reached the bridge (%v) — the size bound runs before any lookup or MAC", bridge.calls)
	}
}

// slackRouterFor mounts one handler INSTANCE so a test can read its retry ledger back. Every other test in
// this file drives the production NewRouter; this one needs the handler itself, and the route pattern is the
// same string router.go registers.
func slackRouterFor(h *slackHandler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /v1/slack/events", http.HandlerFunc(h.receive))
	return mux
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
