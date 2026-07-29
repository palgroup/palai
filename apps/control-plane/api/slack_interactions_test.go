package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/integrations/slack"
)

// The Slack interactivity route (E19 T2), against a fake peer built to the PUBLISHED contract.
//
// The single most important assertion in this file is negative: a JSON body must FAIL. Slack sends
// application/x-www-form-urlencoded with one `payload` parameter, and the v0 MAC covers those raw form bytes.
// A receiver that accepts a JSON body is a receiver whose signature check has stopped meaning anything, and
// that shape is exactly the one a reader coming from every other route in this tree would write by reflex.
//
// HONEST CEILING: no socket to slack.com exists here either. The signing secret is a test string.

// stubSlackDecider is the decision bridge seen from the route: it records the call ORDER and the tenant every
// decision ran under, and it can be made to fail or hang each step independently.
type stubSlackDecider struct {
	conn       SlackConnectionRef
	found      bool
	secret     []byte
	resolveErr error

	calls     []string // "resolve", "verify", "decide" — the ORDER is the contract
	verified  [][]byte // the EXACT bytes handed to the signature check
	intents   []slack.ApprovalIntent
	scopes    []string
	outcome   SlackDecisionOutcome
	decideErr error
	hang      bool

	// E23 T4's modal branch. openDeadlines records the DEADLINE the route handed the call, which is how the
	// three-second rule is measured rather than asserted: views.open is only reachable inside the ack budget
	// if the context it runs under is the budget's.
	opens         []slack.ShowArgumentsIntent
	openDeadlines []time.Time
	openRejected  string
	openErr       error
	openHang      bool
}

func (s *stubSlackDecider) ResolveConnection(_ context.Context, teamID, _ string) (SlackConnectionRef, bool, error) {
	s.calls = append(s.calls, "resolve")
	if s.resolveErr != nil {
		return SlackConnectionRef{}, false, s.resolveErr
	}
	if !s.found || teamID != s.conn.TeamID {
		return SlackConnectionRef{}, false, nil
	}
	return s.conn, true, nil
}

func (s *stubSlackDecider) VerifySignature(_ context.Context, _ SlackConnectionRef, timestamp, signature string, body []byte) error {
	s.calls = append(s.calls, "verify")
	s.verified = append(s.verified, append([]byte(nil), body...))
	// The REAL adapter verify against the REAL clock — a fixture that stubs this proves nothing about order.
	return slack.VerifySignature(s.secret, timestamp, signature, body, time.Now(), slack.DefaultTolerance)
}

func (s *stubSlackDecider) Decide(ctx context.Context, conn SlackConnectionRef, intent slack.ApprovalIntent) (SlackDecisionOutcome, error) {
	s.calls = append(s.calls, "decide")
	s.intents = append(s.intents, intent)
	s.scopes = append(s.scopes, conn.Org+"/"+conn.Project)
	if s.hang {
		<-ctx.Done() // a stuck dependency: the route still owes Slack an answer inside the budget
		return SlackDecisionOutcome{}, ctx.Err()
	}
	return s.outcome, s.decideErr
}

func (s *stubSlackDecider) OpenApprovalArguments(ctx context.Context, conn SlackConnectionRef, intent slack.ShowArgumentsIntent) (string, error) {
	s.calls = append(s.calls, "open")
	s.opens = append(s.opens, intent)
	s.scopes = append(s.scopes, conn.Org+"/"+conn.Project)
	deadline, _ := ctx.Deadline()
	s.openDeadlines = append(s.openDeadlines, deadline)
	if s.openHang {
		<-ctx.Done() // a stalled ledger read: the route still owes Slack an answer inside the budget
		return "", ctx.Err()
	}
	return s.openRejected, s.openErr
}

func newSlackDecider(secret []byte) *stubSlackDecider {
	return &stubSlackDecider{
		conn:   SlackConnectionRef{ID: "slkc_1", Org: "org_real", Project: "prj_real", TeamID: "T100", BotUserID: "Ubot"},
		found:  true,
		secret: secret,
	}
}

// interactionPeer posts interactivity callbacks the way the contract says Slack does.
type interactionPeer struct {
	url    string
	secret []byte
}

// CONTRACT: https://docs.slack.dev/interactivity/handling-user-interaction/ (checked 2026-07-26) — the POST
// is application/x-www-form-urlencoded carrying a single `payload` parameter holding the JSON.
// CONTRACT: https://docs.slack.dev/authentication/verifying-requests-from-slack/ (checked 2026-07-26) — the
// base string is 'v0:'+timestamp+':'+the RAW request body, i.e. the form bytes, not the JSON inside them.
// The MAC is built longhand here rather than by calling our own signer: a fixture that calls the code under
// test cannot notice that code drifting from the contract.
func (p interactionPeer) postForm(t *testing.T, payload []byte, at time.Time) *http.Response {
	t.Helper()
	raw := []byte("payload=" + url.QueryEscape(string(payload)))
	timestamp, signature := signInteraction(p.secret, raw, at)
	return p.postRaw(t, raw, timestamp, signature, "application/x-www-form-urlencoded")
}

func (p interactionPeer) postRaw(t *testing.T, body []byte, timestamp, signature, contentType string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, p.url+"/v1/slack/interactions", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("build interactivity request: %v", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Slack-Request-Timestamp", timestamp)
	req.Header.Set("X-Slack-Signature", signature)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/slack/interactions: %v", err)
	}
	return resp
}

func signInteraction(secret, body []byte, at time.Time) (timestamp, signature string) {
	timestamp = strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("v0:" + timestamp + ":"))
	mac.Write(body)
	return timestamp, "v0=" + hex.EncodeToString(mac.Sum(nil))
}

// CONTRACT: https://docs.slack.dev/reference/interaction-payloads/block_actions-payload/ (checked
// 2026-07-26) — the documented block_actions shape.
func interactionPayload(team, user, channel, messageTS, actionID, value string) []byte {
	raw, _ := json.Marshal(map[string]any{
		"type": "block_actions",
		"team": map[string]any{"id": team, "domain": "example"},
		"user": map[string]any{"id": user, "username": "clicker", "team_id": team},
		"container": map[string]any{
			"type": "message", "message_ts": messageTS, "channel_id": channel, "is_ephemeral": false,
		},
		"channel":      map[string]any{"id": channel, "name": "approvals"},
		"message":      map[string]any{"type": "message", "ts": messageTS, "text": "approve?"},
		"response_url": "https://hooks.slack.invalid/actions/T0/1/2",
		"actions": []any{map[string]any{
			"action_id": actionID, "block_id": "=qXel", "value": value, "type": "button",
			"action_ts": "1548426417.840180",
		}},
	})
	return raw
}

// interactionPeerAgainst mounts the PRODUCTION router with the interactivity surface wired to `decider`.
func interactionPeerAgainst(t *testing.T, decider SlackInteractionsAPI, secret []byte) (interactionPeer, func()) {
	t.Helper()
	router := NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		SSEConfig{}, nil, nil, WithSlackInteractions(decider))
	ts := httptest.NewServer(router)
	return interactionPeer{url: ts.URL, secret: secret}, ts.Close
}

// TestSlackInteractionVerifiesTheRawFormBodyBeforeDecoding is the D8 pin, and it asserts the two things that
// can each independently make this route unsigned:
//
//	(1) the ORDER — resolve (to learn the secret) → verify → decide. Nothing is mapped before authentication.
//	(2) WHAT was verified — the exact raw form bytes, not the JSON extracted from them. Verifying the
//	    extracted payload is verifying a string Slack never signed.
func TestSlackInteractionVerifiesTheRawFormBodyBeforeDecoding(t *testing.T) {
	secret := []byte("test-signing-secret")
	decider := newSlackDecider(secret)
	peer, done := interactionPeerAgainst(t, decider, secret)
	defer done()

	payload := interactionPayload("T100", "Umapped", "C1", "1700000000.000100", slack.ActionApprove, "req_hash")
	resp := peer.postForm(t, payload, time.Now())
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a genuine signed click = %d, want 200 within 3 seconds (https://docs.slack.dev/interactivity/handling-user-interaction/)", resp.StatusCode)
	}
	if want := []string{"resolve", "verify", "decide"}; !equalStrings(decider.calls, want) {
		t.Fatalf("handler order = %v, want %v — the payload may only be MAPPED after its signature verifies", decider.calls, want)
	}
	if len(decider.verified) != 1 {
		t.Fatalf("%d verifications, want 1", len(decider.verified))
	}
	verified := string(decider.verified[0])
	if !strings.HasPrefix(verified, "payload=") {
		t.Fatalf("the signature was checked over %q, want the RAW form body (payload=<urlencoded JSON>) — verifying the extracted JSON checks bytes Slack never signed", verified)
	}
	if verified == string(payload) {
		t.Fatal("the signature was checked over the EXTRACTED JSON; that MAC can never match a real Slack request, so such a receiver only 'works' by not checking at all")
	}
	if len(decider.intents) != 1 || decider.intents[0].RequestHash != "req_hash" || decider.intents[0].UserID != "Umapped" {
		t.Fatalf("decided intents = %+v, want one carrying the clicked hash and clicker", decider.intents)
	}
	if decider.intents[0].ChannelID != "C1" || decider.intents[0].ThreadTS != "1700000000.000100" {
		t.Fatalf("intent = %+v, want the clicked thread — a decision is bound to the conversation it was clicked in", decider.intents[0])
	}
}

// TestSlackInteractionRefusesBodiesSlackNeverSends is the RED this task was written around: a JSON body — the
// shape every sibling route in this tree receives — is not what Slack posts here, and accepting it would mean
// the signature was verified over something other than the signed form bytes.
func TestSlackInteractionRefusesBodiesSlackNeverSends(t *testing.T) {
	secret := []byte("test-signing-secret")
	payload := interactionPayload("T100", "Umapped", "C1", "1700000000.000100", slack.ActionApprove, "req_hash")

	for _, tc := range []struct {
		name string
		want int
		post func(t *testing.T, peer interactionPeer) *http.Response
	}{
		{
			// A correctly SIGNED JSON body. The MAC verifies (the peer signed these very bytes), so only the
			// body-shape check can refuse it — which is the point: shape is not a formatting preference here.
			name: "a signed JSON body", want: http.StatusBadRequest,
			post: func(t *testing.T, peer interactionPeer) *http.Response {
				timestamp, signature := signInteraction(secret, payload, time.Now())
				return peer.postRaw(t, payload, timestamp, signature, "application/json")
			},
		},
		{
			// The reverse-order defect made concrete: the form body is posted, but the signature was computed
			// over the EXTRACTED JSON. A receiver that verifies after decoding would accept this; one that
			// verifies the raw body cannot.
			name: "a form body signed over the extracted JSON", want: http.StatusUnauthorized,
			post: func(t *testing.T, peer interactionPeer) *http.Response {
				raw := []byte("payload=" + url.QueryEscape(string(payload)))
				timestamp, signature := signInteraction(secret, payload, time.Now()) // signs the JSON, not the form
				return peer.postRaw(t, raw, timestamp, signature, "application/x-www-form-urlencoded")
			},
		},
		{
			name: "a form with no payload parameter", want: http.StatusBadRequest,
			post: func(t *testing.T, peer interactionPeer) *http.Response {
				raw := []byte("token=deprecated&team_id=T100")
				timestamp, signature := signInteraction(secret, raw, time.Now())
				return peer.postRaw(t, raw, timestamp, signature, "application/x-www-form-urlencoded")
			},
		},
		{
			name: "a forged signature", want: http.StatusUnauthorized,
			post: func(t *testing.T, peer interactionPeer) *http.Response {
				raw := []byte("payload=" + url.QueryEscape(string(payload)))
				return peer.postRaw(t, raw, strconv.FormatInt(time.Now().Unix(), 10),
					"v0="+strings.Repeat("00", 32), "application/x-www-form-urlencoded")
			},
		},
		{
			// CONTRACT: https://docs.slack.dev/authentication/verifying-requests-from-slack/ (checked
			// 2026-07-26) — a timestamp more than five minutes from local time is a possible replay.
			name: "a stale timestamp", want: http.StatusUnauthorized,
			post: func(t *testing.T, peer interactionPeer) *http.Response {
				return peer.postForm(t, payload, time.Now().Add(-10*time.Minute))
			},
		},
		{
			name: "an unknown workspace", want: http.StatusNotFound,
			post: func(t *testing.T, peer interactionPeer) *http.Response {
				return peer.postForm(t, interactionPayload("TNOTINSTALLED", "Umapped", "C1", "1.0", slack.ActionApprove, "req_hash"), time.Now())
			},
		},
	} {
		t.Run(tc.name+" is refused", func(t *testing.T) {
			decider := newSlackDecider(secret)
			peer, done := interactionPeerAgainst(t, decider, secret)
			defer done()
			resp := tc.post(t, peer)
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
			if len(decider.intents) != 0 {
				t.Fatalf("%d decisions were taken on a refused body (%+v) — nothing may be decided before the raw body verifies", len(decider.intents), decider.intents)
			}
		})
	}
}

// TestSlackForeignInteractionDecidesNothing: a signed interaction that is not one of OUR minted approve/deny
// buttons authorizes nothing and reaches no decision path. It is still acknowledged — the delivery succeeded,
// it simply asks us for nothing, and a non-200 would show the clicking user an error for someone else's
// button.
func TestSlackForeignInteractionDecidesNothing(t *testing.T) {
	secret := []byte("test-signing-secret")

	for _, tc := range []struct {
		name    string
		payload []byte
	}{
		{"another app's block action", interactionPayload("T100", "Umapped", "C1", "1.0", "someone_elses_button", "whatever")},
		{"our approve button with an EMPTY value", interactionPayload("T100", "Umapped", "C1", "1.0", slack.ActionApprove, "")},
		{"a view_submission", []byte(`{"type":"view_submission","team":{"id":"T100"},"user":{"id":"Umapped"}}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decider := newSlackDecider(secret)
			peer, done := interactionPeerAgainst(t, decider, secret)
			defer done()
			resp := peer.postForm(t, tc.payload, time.Now())
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200 — the delivery succeeded; it just decides nothing", resp.StatusCode)
			}
			if len(decider.intents) != 0 {
				t.Fatalf("a foreign/empty-value button reached the decision path (%+v); only a minted button carrying the one-shot hash may decide", decider.intents)
			}
		})
	}
}

// TestSlackInteractionTenantComesOnlyFromTheResolvedConnection is §2/§38.6 on this transport: the payload
// carries every tenant-shaped field a forger would try, and the decision still runs under the org/project of
// the connection whose secret signed the body.
func TestSlackInteractionTenantComesOnlyFromTheResolvedConnection(t *testing.T) {
	secret := []byte("test-signing-secret")
	decider := newSlackDecider(secret)
	peer, done := interactionPeerAgainst(t, decider, secret)
	defer done()

	forged, _ := json.Marshal(map[string]any{
		"type":            "block_actions",
		"team":            map[string]any{"id": "T100"},
		"user":            map[string]any{"id": "Umapped", "team_id": "T100"},
		"organization_id": "org_attacker", "project_id": "prj_attacker",
		"org": "org_attacker", "project": "prj_attacker", "principal_id": "prin_attacker",
		"channel": map[string]any{"id": "C1"},
		"message": map[string]any{"ts": "1700000000.000100"},
		"actions": []any{map[string]any{"action_id": slack.ActionApprove, "value": "req_hash", "type": "button"}},
	})
	resp := peer.postForm(t, forged, time.Now())
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the forged fields are IGNORED, not a rejection)", resp.StatusCode)
	}
	if len(decider.scopes) != 1 || decider.scopes[0] != "org_real/prj_real" {
		t.Fatalf("decision scopes = %v, want [org_real/prj_real] — the tenant comes from the RESOLVED connection and from nothing in the payload", decider.scopes)
	}
}

// TestSlackInteractionAnswersInsideTheAckBudget: Slack requires a 200 within 3 seconds or the clicking user
// is shown an error. A stuck dependency must therefore be turned into an answer, not into a hung request.
func TestSlackInteractionAnswersInsideTheAckBudget(t *testing.T) {
	secret := []byte("test-signing-secret")
	decider := newSlackDecider(secret)
	decider.hang = true
	handler := &slackInteractionsHandler{slack: decider, budget: 50 * time.Millisecond, logf: func(string, ...any) {}}
	mux := http.NewServeMux()
	mux.Handle("POST /v1/slack/interactions", http.HandlerFunc(handler.receive))
	ts := httptest.NewServer(mux)
	defer ts.Close()
	peer := interactionPeer{url: ts.URL, secret: secret}

	start := time.Now()
	resp := peer.postForm(t, interactionPayload("T100", "Umapped", "C1", "1.0", slack.ActionApprove, "req_hash"), time.Now())
	defer resp.Body.Close()
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("a stuck decision answered after %v; the budget must bound the handler, not the dependency", elapsed)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a stuck decision = %d, want 503 — an honest failure, not a silent 200 the operator never sees", resp.StatusCode)
	}
}

// TestSlackInteractionsRouteIsNotMountedWithoutTheOption is the discovery-honesty half: a stack that wires no
// interactivity bridge must not answer on the path at all.
func TestSlackInteractionsRouteIsNotMountedWithoutTheOption(t *testing.T) {
	router := NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		SSEConfig{}, nil, nil)
	ts := httptest.NewServer(router)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/slack/interactions", "application/x-www-form-urlencoded", strings.NewReader("payload=%7B%7D"))
	if err != nil {
		t.Fatalf("probe the unmounted route: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("the unmounted interactivity route answered 200; the handler must not exist without WithSlackInteractions")
	}
}

// TestSlackAppRateLimitedIsAckedAndCounted closes D10's second half on the EVENTS route: app_rate_limited is
// an OUTER type, so MapEvent refuses it and the route would have answered 400 + x-slack-no-retry — throwing
// away the one notification Slack sends to say our app is being throttled.
//
// CONTRACT: https://docs.slack.dev/apis/web-api/rate-limits/ (checked 2026-07-26) — Events API delivery is
// capped at 30,000 per workspace/app per 60 minutes, and exceeding it delivers `app_rate_limited`.
func TestSlackAppRateLimitedIsAckedAndCounted(t *testing.T) {
	secret := []byte("test-signing-secret")
	bridge := newSlackBridge(secret)
	handler := &slackHandler{slack: bridge, logf: func(string, ...any) {}}
	ts := httptest.NewServer(slackRouterFor(handler))
	defer ts.Close()
	peer := fakeSlackPeer{url: ts.URL, secret: secret}

	body := []byte(`{"token":"deprecated","type":"app_rate_limited","team_id":"T100","minute_rate_limited":1518467820,"api_app_id":"A0001"}`)
	resp := peer.post(t, body, time.Now(), "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("app_rate_limited = %d, want 200 — it is a notice, not a poison event", resp.StatusCode)
	}
	if got := resp.Header.Get(hdrNoRetry); got != "" {
		t.Fatalf("the throttling notice carried %s=%q; it is acknowledged, not suppressed", hdrNoRetry, got)
	}
	if len(bridge.admitted) != 0 {
		t.Fatalf("the throttling notice birthed %d admissions, want 0", len(bridge.admitted))
	}
	if got := handler.retries.snapshot()[slackThrottleCounter]; got != 1 {
		t.Fatalf("%s counter = %d, want 1 — being throttled by Slack must be visible to an operator, not discarded as a 400", slackThrottleCounter, got)
	}
	// It is verified like any other body: an UNSIGNED throttling notice must not move the counter.
	forged := peer.postSigned(t, body, strconv.FormatInt(time.Now().Unix(), 10), "v0="+strings.Repeat("00", 32), "", "")
	defer forged.Body.Close()
	if forged.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an unsigned app_rate_limited = %d, want 401", forged.StatusCode)
	}
	if got := handler.retries.snapshot()[slackThrottleCounter]; got != 1 {
		t.Fatalf("an unsigned notice moved the counter to %d; an unauthenticated caller must not be able to write our operational signals", got)
	}
}
