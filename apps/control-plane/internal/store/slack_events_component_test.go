//go:build component

package store_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/internal/extensions"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/storage"
)

// The E19 T1 Slack Events API route, end to end against REAL PostgreSQL and the REAL admission Admitter —
// the same store.Store POST /v1/responses uses. Nothing is stubbed on our side of the wire: real router,
// real 000035 connection/thread store, real idempotency reservation, real run rows counted in the database.
//
// The other side of the wire is a FAKE Slack peer, and every line of it is justified against the PUBLISHED
// contract with the URL and the date it was checked. That discipline is not decoration: E17 T10's console
// fake invented an approval event the real API never emits, and every green proof that ran against it was
// evidence about the fixture. So the peer below signs the way the verification page says Slack signs, and
// retries the way the Events API page says Slack retries.
//
// HONEST CEILING, unavoidable and stated: this proves the code is correct against the DOCUMENTED contract.
// It is NOT evidence about a real Slack workspace — there is no socket to slack.com in this file, the signing
// secret is a test string, and `slack` therefore stays PREVIEW. The external receipt is §6 leg 1.

// slackFixture is one wired stack: a migrated spine, a registered workspace, and a router serving the route.
type slackFixture struct {
	// repo is the REAL api.Admitter this fixture serves — held so a sibling harness (the E19 T9 wiring
	// journey) can mount the rest of the production router over the same store rather than opening a second.
	repo      *store.Store
	pool      *pgxpool.Pool
	url       string
	secret    []byte
	botToken  []byte
	appToken  []byte // the Socket Mode app-level (xapp-) token, E19 T3
	org       string
	project   string
	principal string
	revision  string
	team      string
	botUser   string
	// apiBase is the local stand-in for https://slack.com/api — chat.* AND apps.connections.open.
	apiBase string
	// socket is the Socket Mode half of that stand-in, nil until a test asks for it (see socketPeer).
	socket *fakeSocketModePeer
	// secrets is the org-scoped secret bridge's backing map, keyed org+"/"+ref. A test that seeds a SECOND
	// tenant adds that tenant's own ref here, so a cross-tenant proof runs against a resolver that serves both
	// — otherwise "the other tenant could not verify" would be an artefact of the fixture, not of the code.
	secrets map[string][]byte

	// The E19 T2 decision half: the real coordinator the approval chain drives, the production bridge (so a
	// test can call Decide without the HTTP layer), and a local stand-in for slack.com/api.
	spine  *coordinator.Store
	bridge *extensions.SlackAdmitter
	slack  *fakeSlackWebAPI
}

// fakeSlackWebAPI is a local HTTP server standing in for slack.com/api. It records every call (path, auth
// header, body) and replays a scripted status sequence, so the outbound path is proven over REAL HTTP with a
// REAL token resolution rather than against a stubbed Doer.
//
// CONTRACT: https://docs.slack.dev/apis/web-api/rate-limits/ (checked 2026-07-26) — a throttled call answers
// 429 with Retry-After in seconds. Slack's Web API envelope is {"ok":bool,"ts":…,"error":…}.
type fakeSlackWebAPI struct {
	mu         sync.Mutex
	statuses   []int
	retryAfter string
	calls      []slackCall
}

type slackCall struct {
	path string
	auth string
	body string
}

func (s *fakeSlackWebAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	status := http.StatusOK
	if n := len(s.calls); n < len(s.statuses) {
		status = s.statuses[n]
	} else if len(s.statuses) > 0 {
		status = s.statuses[len(s.statuses)-1]
	}
	s.calls = append(s.calls, slackCall{path: r.URL.Path, auth: r.Header.Get("Authorization"), body: string(body)})
	ts := fmt.Sprintf("9%02d.000100", len(s.calls))
	after := s.retryAfter
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if status == http.StatusTooManyRequests {
		if after == "" {
			after = "1"
		}
		w.Header().Set("Retry-After", after)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"ok":false,"error":"ratelimited"}`))
		return
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"ok":true,"ts":"` + ts + `"}`))
}

func (f *slackFixture) slackStatuses(statuses ...int) {
	f.slack.mu.Lock()
	defer f.slack.mu.Unlock()
	f.slack.statuses = statuses
}

func (f *slackFixture) slackRetryAfter(after string) {
	f.slack.mu.Lock()
	defer f.slack.mu.Unlock()
	f.slack.retryAfter = after
}

func (f *slackFixture) slackCalls() []slackCall {
	f.slack.mu.Lock()
	defer f.slack.mu.Unlock()
	return append([]slackCall(nil), f.slack.calls...)
}

// connRef is the resolved connection as the route hands it to the bridge — read through the PRODUCTION
// resolve, so a test driving Decide directly is still using the tenant the real path would have established.
func (f *slackFixture) connRef(t *testing.T) api.SlackConnectionRef {
	t.Helper()
	conn, found, err := f.bridge.ResolveConnection(context.Background(), f.team, "")
	if err != nil || !found {
		t.Fatalf("resolve the fixture connection: (%v,%v)", found, err)
	}
	return conn
}

// newSlackFixture seeds the tenant, publishes an agent revision, registers the Slack workspace with the run
// target in default_policy, and serves the PRODUCTION router with the PRODUCTION bridge over the real store.
func newSlackFixture(t *testing.T) *slackFixture {
	t.Helper()
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-component TEST=postgres")
	}
	ctx := context.Background()
	repo, err := store.Open(ctx, url) // the real api.Admitter
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(repo.Close)
	if err := repo.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	pool := repo.Spine().Pool()

	f := &slackFixture{
		repo: repo, pool: pool, secret: []byte("component-signing-secret-not-a-credential"),
		botToken: []byte("xoxb-component-fake-not-a-credential"),
		appToken: []byte("xapp-1-component-fake-not-a-credential"),
		org:      newID("org"), project: newID("prj"), principal: newID("prin"),
		revision: newID("arev"), team: strings.ToUpper(newID("T")), botUser: newID("Ubot"),
		slack: &fakeSlackWebAPI{},
	}
	profileID := newID("aprof")
	exec(t, pool, `INSERT INTO organizations (id) VALUES ($1)`, f.org)
	exec(t, pool, `INSERT INTO projects (id, organization_id) VALUES ($1,$2)`, f.project, f.org)
	exec(t, pool, `INSERT INTO principals (id, organization_id, project_id, kind) VALUES ($1,$2,$3,'service')`,
		f.principal, f.org, f.project)
	exec(t, pool, `INSERT INTO agent_profiles (id, organization_id, project_id, name) VALUES ($1,$2,$3,$4)`,
		profileID, f.org, f.project, newID("name"))
	exec(t, pool, `INSERT INTO agent_revisions (id, organization_id, project_id, profile_id, revision_number, model, tools, published_at)
	               VALUES ($1,$2,$3,$4,1,'model-pinned','["file"]', clock_timestamp())`,
		f.revision, f.org, f.project, profileID)

	ext := extensions.New(pool)
	const signingRef, botRef, appRef = "slack/component/signing", "slack/component/bot", "slack/component/app"
	// app_token_ref is E19 T3's: Socket Mode's only authentication is the app-level token at connect. It is
	// registered here rather than in a second fixture so the SAME workspace binding serves all three transports
	// — which is the point of the transport-invariance proof in slack_socket_component_test.go.
	if _, err := ext.CreateSlackConnection(ctx, f.org, f.project, []byte(fmt.Sprintf(
		`{"team_id":%q,"bot_user_id":%q,"signing_secret_ref":%q,"bot_token_ref":%q,"app_token_ref":%q,
		  "allowed_users":["Umapped"],
		  "default_policy":{"agent_revision_id":%q,"principal_id":%q}}`,
		f.team, f.botUser, signingRef, botRef, appRef, f.revision, f.principal))); err != nil {
		t.Fatalf("register the Slack workspace: %v", err)
	}

	// The org-scoped secret bridge, the production resolver's shape: a ref only resolves under the org it was
	// provisioned in, so a connection can never redeem another tenant's secret.
	f.secrets = map[string][]byte{
		f.org + "/" + signingRef: f.secret,
		f.org + "/" + botRef:     f.botToken,
		f.org + "/" + appRef:     f.appToken,
	}
	secrets := func(org, ref string) ([]byte, error) {
		secret, ok := f.secrets[org+"/"+ref]
		if !ok {
			return nil, fmt.Errorf("no secret bridge for %q/%q", org, ref)
		}
		return secret, nil
	}
	// The local stand-in for slack.com/api. The bridge posts to it with a REAL http.Client, so the outbound
	// half is proven over real HTTP with a real Authorization header rather than against a stubbed Doer.
	//
	// ONE server serves the whole Slack API surface this stack talks to, because that is how the real one is
	// laid out: chat.postMessage / chat.update AND apps.connections.open live under the same base
	// (https://slack.com/api). The Socket Mode routes are wired in slack_socket_component_test.go; a fixture
	// that never runs one simply never receives a call on them.
	slackAPI := httptest.NewServer(f.slackAPIMux())
	t.Cleanup(slackAPI.Close)
	f.apiBase = slackAPI.URL

	f.spine = repo.Spine()
	bridge := extensions.NewSlackAdmitter(ext, repo, secrets, api.AdmissionLimits{}).
		WithDecisions(f.spine, http.DefaultClient, slackAPI.URL)
	f.bridge = bridge
	ts := httptest.NewServer(api.NewRouter(nil, repo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		api.SSEConfig{}, nil, nil, api.WithSlack(bridge), api.WithSlackInteractions(bridge)))
	t.Cleanup(ts.Close)
	f.url = ts.URL
	return f
}

// deliver signs a body the way Slack does and posts it. The MAC is built longhand from the published base
// string rather than by calling our own signer, so a drift in the verifier cannot move the fixture with it.
//
// CONTRACT: https://docs.slack.dev/authentication/verifying-requests-from-slack/ (checked 2026-07-25) —
// base string exactly 'v0:' + timestamp + ':' + request_body, HMAC-SHA256 hex, header value 'v0='-prefixed.
func (f *slackFixture) deliver(t *testing.T, body []byte, at time.Time, retryNum, retryReason string) *http.Response {
	t.Helper()
	return f.deliverAs(t, f.secret, body, at, retryNum, retryReason)
}

// deliverAs signs with a CALLER-CHOSEN secret, so a cross-tenant proof can present a body a different
// workspace binding's secret MACs.
func (f *slackFixture) deliverAs(t *testing.T, secret, body []byte, at time.Time, retryNum, retryReason string) *http.Response {
	t.Helper()
	timestamp, signature := signSlack(secret, body, at)
	return f.deliverSigned(t, body, timestamp, signature, retryNum, retryReason)
}

// signSlack builds the v0 MAC longhand from the published base string (see deliver's CONTRACT note).
func signSlack(secret, body []byte, at time.Time) (timestamp, signature string) {
	timestamp = strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("v0:" + timestamp + ":"))
	mac.Write(body)
	return timestamp, "v0=" + hex.EncodeToString(mac.Sum(nil))
}

// send is the transport, error-returning: a concurrent test drives it from a goroutine, where calling
// t.Fatalf would be illegal.
func (f *slackFixture) send(body []byte, timestamp, signature, retryNum, retryReason string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, f.url+"/v1/slack/events", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	// CONTRACT: same page — the two headers Slack signs with.
	req.Header.Set("X-Slack-Request-Timestamp", timestamp)
	req.Header.Set("X-Slack-Signature", signature)
	// CONTRACT: https://docs.slack.dev/apis/events-api/ (checked 2026-07-25) — a redelivery carries the
	// attempt number (1..3) and one of the six documented reasons.
	if retryNum != "" {
		req.Header.Set("X-Slack-Retry-Num", retryNum)
	}
	if retryReason != "" {
		req.Header.Set("X-Slack-Retry-Reason", retryReason)
	}
	return http.DefaultClient.Do(req)
}

func (f *slackFixture) deliverSigned(t *testing.T, body []byte, timestamp, signature, retryNum, retryReason string) *http.Response {
	t.Helper()
	resp, err := f.send(body, timestamp, signature, retryNum, retryReason)
	if err != nil {
		t.Fatalf("POST /v1/slack/events: %v", err)
	}
	return resp
}

// event builds an Events API envelope.
//
// CONTRACT: https://docs.slack.dev/apis/events-api/ (checked 2026-07-25) — {"type":"event_callback",
// "team_id":…, "event_id":…, "event":{…}}; the inner object carries the message's user/channel/ts, and
// thread_ts when it is a threaded reply.
func (f *slackFixture) event(eventID, innerType, user, channel, ts, threadTS string) []byte {
	return f.eventText(nil, eventID, innerType, user, channel, ts, threadTS, "hello")
}

// eventText is event() with the message the human typed spelled out, for the tests that assert what reaches
// the model. t is only for the signature's symmetry with the other helpers and may be nil.
func (f *slackFixture) eventText(_ *testing.T, eventID, innerType, user, channel, ts, threadTS, text string) []byte {
	inner := map[string]any{"type": innerType, "user": user, "channel": channel, "ts": ts, "text": text}
	if threadTS != "" {
		inner["thread_ts"] = threadTS
	}
	raw, _ := json.Marshal(map[string]any{
		"type": "event_callback", "team_id": f.team, "api_app_id": "A0001",
		"event_id": eventID, "event_time": 1700000000, "event": inner,
	})
	return raw
}

func (f *slackFixture) runCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(*) FROM runs WHERE organization_id=$1 AND project_id=$2`, f.org, f.project).Scan(&n); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	return n
}

func (f *slackFixture) sessionCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(*) FROM slack_thread_sessions WHERE organization_id=$1 AND project_id=$2`, f.org, f.project).Scan(&n); err != nil {
		t.Fatalf("count thread sessions: %v", err)
	}
	return n
}

// TestSlackEventBirthsARealRunThroughTheRealAdmitter is the wiring proof: a signed app_mention arriving on
// the shipped route births a GENUINE run in the database, under the connection's tenant, pinned to the
// connection's revision — through the same Admitter POST /v1/responses uses.
func TestSlackEventBirthsARealRunThroughTheRealAdmitter(t *testing.T) {
	f := newSlackFixture(t)

	resp := f.deliver(t, f.event("Ev1", "app_mention", "Umapped", "C1", "1700000000.000100", ""), time.Now(), "", "")
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("signed app_mention = %d, want a 2xx ack", resp.StatusCode)
	}
	if n := f.runCount(t); n != 1 {
		t.Fatalf("the route birthed %d runs, want exactly 1", n)
	}

	// The run is pinned to the revision default_policy named, and it belongs to the connection's principal —
	// both server-side facts, neither of them anything the payload said.
	var revision, principal string
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT COALESCE(r.agent_revision_id,''), COALESCE(i.principal_id,'')
		   FROM runs r
		   LEFT JOIN idempotency_records i
		     ON i.organization_id = r.organization_id AND i.project_id = r.project_id AND i.route = '/v1/slack/events'
		  WHERE r.organization_id=$1 AND r.project_id=$2`, f.org, f.project).Scan(&revision, &principal); err != nil {
		t.Fatalf("read the born run: %v", err)
	}
	if revision != f.revision {
		t.Fatalf("run pinned revision %q, want the connection's default_policy revision %q", revision, f.revision)
	}
	if principal != f.principal {
		t.Fatalf("run reserved under principal %q, want the connection's %q — a Slack user is never a principal", principal, f.principal)
	}
	// The reservation was taken in the SLACK namespace, so a native Idempotency-Key of the same value could
	// never collide with a Slack event_id.
	var route, key string
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT route, idempotency_key FROM idempotency_records WHERE organization_id=$1 AND project_id=$2`,
		f.org, f.project).Scan(&route, &key); err != nil {
		t.Fatalf("read the idempotency reservation: %v", err)
	}
	if route != "/v1/slack/events" || key != f.team+":Ev1" {
		t.Fatalf("reservation = (%q,%q), want (/v1/slack/events, %s:Ev1) — the route owns its own idempotency namespace, keyed team+event", route, key, f.team)
	}
}

// TestSlackRetryStormRunsTheEffectOnce is SLK-001/002 over the shipped route against a real database: Slack's
// three redeliveries of an unacknowledged event collapse onto ONE run.
//
// CONTRACT: https://docs.slack.dev/apis/events-api/ (checked 2026-07-25) — a delivery without a 2xx inside 3
// seconds is retried three times, carrying x-slack-retry-num 1..3 and a reason.
//
// D3 ASSUMPTION MADE VISIBLE: this fixture repeats the SAME event_id on every retry because that is what our
// dedupe requires — and the official page does NOT state that Slack does so. The assumption is labelled in
// adapters/integrations/slack (HeaderRetryNum) and ASSERTED against a real workspace by tests/live/slack.
// If it is ever falsified, this test still passes and the live one fails: that is the point of having both.
func TestSlackRetryStormRunsTheEffectOnce(t *testing.T) {
	f := newSlackFixture(t)
	body := f.event("Ev2", "app_mention", "Umapped", "C2", "1700000001.000100", "")

	f.deliver(t, body, time.Now(), "", "").Body.Close()
	for i, reason := range []string{"http_timeout", "http_timeout", "http_error"} {
		resp := f.deliver(t, body, time.Now(), strconv.Itoa(i+1), reason)
		if resp.StatusCode/100 != 2 {
			t.Fatalf("retry %d = %d, want a 2xx ack (the redelivery must be acknowledged, not refused)", i+1, resp.StatusCode)
		}
		resp.Body.Close()
	}
	if n := f.runCount(t); n != 1 {
		t.Fatalf("4 deliveries of one event birthed %d runs, want exactly 1 — the reservation on (team_id, event_id) is the whole of SLK-002", n)
	}
	// One canonical reservation, and it did NOT record a conflict: a redelivery that hashed differently would
	// have been refused as "same key, different request" instead of replaying.
	var records int
	var status string
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(*), COALESCE(max(status),'') FROM idempotency_records WHERE organization_id=$1 AND project_id=$2`,
		f.org, f.project).Scan(&records, &status); err != nil {
		t.Fatalf("read reservations: %v", err)
	}
	if records != 1 {
		t.Fatalf("%d idempotency records for one source event, want 1", records)
	}
	if status == "conflict" {
		t.Fatal("the redelivery was recorded as an idempotency CONFLICT; the admitted request must be a pure function of the EVENT, not of the delivery attempt")
	}
}

// TestSlackThreadCorrelatesToOneSession is SLK-003 through the shipped route: two events in one thread land
// in ONE canonical session, and a different thread gets its own.
func TestSlackThreadCorrelatesToOneSession(t *testing.T) {
	f := newSlackFixture(t)
	const root = "1700000002.000100"

	f.deliver(t, f.event("Ev3", "app_mention", "Umapped", "C3", root, ""), time.Now(), "", "").Body.Close()
	if n := f.sessionCount(t); n != 1 {
		t.Fatalf("after the first thread event there are %d thread↔session rows, want 1", n)
	}
	// The first run reaches a terminal before the follow-up arrives. That is not decoration: a session holds
	// ONE active root run (000006), so a second event while the first is still live is a different case
	// entirely — see TestSlackMessageDuringALiveRunNeverBirthsASecondRun, which owns it.
	f.terminateRuns(t)
	// A second event in the SAME thread. It chains onto the thread's session rather than claiming a new one.
	resp := f.deliver(t, f.event("Ev4", "message", "Umapped", "C3", "1700000003.000100", root), time.Now(), "", "")
	if resp.StatusCode/100 != 2 {
		t.Fatalf("the follow-up in the thread = %d, want a 2xx ack", resp.StatusCode)
	}
	resp.Body.Close()
	if n := f.sessionCount(t); n != 1 {
		t.Fatalf("a second event in the same thread produced %d thread↔session rows, want 1 canonical session (SLK-003)", n)
	}
	// The row count alone does NOT prove correlation, and this is where an earlier bug hid: the thread claim
	// is ON CONFLICT DO NOTHING, so a second event that FAILED to find the existing row still leaves exactly
	// one row — while its RUN lands in a brand-new session the thread does not point at. Assert the runs.
	if n := f.runCount(t); n != 2 {
		t.Fatalf("two events in one thread birthed %d runs, want 2 (each source event is its own effect)", n)
	}
	var sessions int
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(DISTINCT session_id) FROM runs WHERE organization_id=$1 AND project_id=$2`,
		f.org, f.project).Scan(&sessions); err != nil {
		t.Fatalf("count distinct run sessions: %v", err)
	}
	if sessions != 1 {
		t.Fatalf("two events in one thread produced runs across %d sessions, want 1 — the second event must CHAIN onto the thread's canonical session, not open a parallel conversation (SLK-003)", sessions)
	}
	var correlated, ran string
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT (SELECT session_id FROM slack_thread_sessions WHERE organization_id=$1 AND thread_ts=$2),
		        (SELECT DISTINCT session_id FROM runs WHERE organization_id=$1)`, f.org, root).Scan(&correlated, &ran); err != nil {
		t.Fatalf("compare the correlated session to the run's: %v", err)
	}
	if correlated != ran {
		t.Fatalf("the thread points at session %q but its runs are in %q — the correlation row and the conversation must be the same session", correlated, ran)
	}
	// A different thread gets its own correlation.
	f.deliver(t, f.event("Ev5", "app_mention", "Umapped", "C3", "1700000004.000100", ""), time.Now(), "", "").Body.Close()
	if n := f.sessionCount(t); n != 2 {
		t.Fatalf("a different thread produced %d rows in total, want 2 — one session per thread, not per workspace", n)
	}
}

// TestSlackEditsAndDeletesReachAdmissionAsTheirOwnKind is SLK-005 over the wire: an edit is a correction and
// a delete is a tombstone, each admitted under its OWN event id, and neither is mistaken for a new message.
//
// CONTRACT: https://docs.slack.dev/apis/events-api/ (checked 2026-07-25) — message_changed / message_deleted
// are SUBTYPES of the `message` event, and they nest the affected message under `message` /
// `previous_message` rather than at the top level.
func TestSlackEditsAndDeletesReachAdmissionAsTheirOwnKind(t *testing.T) {
	f := newSlackFixture(t)
	const root = "1700000005.000100"

	edit, _ := json.Marshal(map[string]any{
		"type": "event_callback", "team_id": f.team, "event_id": "Ev6",
		"event": map[string]any{"type": "message", "subtype": "message_changed", "channel": "C4",
			"message": map[string]any{"user": "Umapped", "ts": root, "thread_ts": root, "text": "edited"}},
	})
	del, _ := json.Marshal(map[string]any{
		"type": "event_callback", "team_id": f.team, "event_id": "Ev7",
		"event": map[string]any{"type": "message", "subtype": "message_deleted", "channel": "C4",
			"previous_message": map[string]any{"user": "Umapped", "ts": root, "thread_ts": root, "text": "gone"}},
	})
	for i, body := range [][]byte{edit, del} {
		if i > 0 {
			f.terminateRuns(t) // one active root run per session; the previous one has to finish first
		}
		resp := f.deliver(t, body, time.Now(), "", "")
		if resp.StatusCode/100 != 2 {
			t.Fatalf("edit/delete = %d, want a 2xx ack", resp.StatusCode)
		}
		resp.Body.Close()
	}
	if n := f.runCount(t); n != 2 {
		t.Fatalf("the edit and the delete birthed %d runs, want 2 (each is its own source event)", n)
	}
	// Both correlate to the SAME thread, so they continue one conversation rather than opening two.
	if n := f.sessionCount(t); n != 1 {
		t.Fatalf("the edit and the delete produced %d thread↔session rows, want 1 — both belong to the same thread", n)
	}
	// The kind is READ OFF THE INPUT the model receives, which is the only place it can still be observed
	// now that the input is the human's message rather than the event envelope — and it is the stronger
	// assertion of the two: SLK-005's classification is worth nothing if it does not change what the model
	// is told. An edit is MARKED as an edit (it does not arrive as a brand-new turn), and a delete does NOT
	// echo the retracted words back.
	var inputs []string
	rows, err := f.pool.Query(storage.WithSystemScope(context.Background()),
		`SELECT input::text FROM responses WHERE organization_id=$1 AND project_id=$2 ORDER BY created_at`,
		f.org, f.project)
	if err != nil {
		t.Fatalf("read admitted inputs: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var input string
		if err := rows.Scan(&input); err != nil {
			t.Fatalf("scan input: %v", err)
		}
		inputs = append(inputs, input)
	}
	if len(inputs) != 2 {
		t.Fatalf("admitted inputs = %v, want two", inputs)
	}
	if inputs[0] != `"(edited) edited"` {
		t.Fatalf("the edit reached the model as %s, want the corrected text marked as an edit (SLK-005: it supersedes, it is not a fresh turn)", inputs[0])
	}
	if !strings.Contains(inputs[1], "deleted their message") || strings.Contains(inputs[1], "gone") {
		t.Fatalf("the delete reached the model as %s, want a retraction that does not replay the removed words", inputs[1])
	}
}

// TestSlackBotSelfEventBirthsNothing is SLK-008 against the real database: the app's own message is acked and
// leaves no run behind, so the app cannot answer itself.
func TestSlackBotSelfEventBirthsNothing(t *testing.T) {
	f := newSlackFixture(t)

	resp := f.deliver(t, f.event("Ev8", "message", f.botUser, "C5", "1700000006.000100", ""), time.Now(), "", "")
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("bot-self event = %d, want a 2xx ack", resp.StatusCode)
	}
	if n := f.runCount(t); n != 0 {
		t.Fatalf("the app's own message birthed %d runs, want 0 — this is the infinite-loop guard", n)
	}
	if n := f.sessionCount(t); n != 0 {
		t.Fatalf("the app's own message correlated %d threads, want 0", n)
	}
}

// TestSlackUnauthenticatedAndPoisonDeliveriesWriteNothing is D1 plus the auth boundary, on real storage: a
// forged MAC, a stale timestamp, an unknown workspace and a poison envelope each answer with the suppress
// header AND leave the database untouched. The last part matters most — a rejected delivery that had already
// written a row would be an unauthenticated write.
func TestSlackUnauthenticatedAndPoisonDeliveriesWriteNothing(t *testing.T) {
	f := newSlackFixture(t)
	body := f.event("Ev9", "app_mention", "Umapped", "C6", "1700000007.000100", "")

	// A forged MAC.
	forged := f.deliverSigned(t, body, strconv.FormatInt(time.Now().Unix(), 10), "v0="+strings.Repeat("ab", 32), "", "")
	assertNoRetry(t, forged, http.StatusUnauthorized, "forged signature")
	// CONTRACT: https://docs.slack.dev/authentication/verifying-requests-from-slack/ (checked 2026-07-25) —
	// "if the timestamp is more than five minutes from local time" the request is a possible replay.
	stale := f.deliver(t, body, time.Now().Add(-10*time.Minute), "", "")
	assertNoRetry(t, stale, http.StatusUnauthorized, "stale timestamp")
	// A workspace this deployment has never been installed in.
	unknown, _ := json.Marshal(map[string]any{"type": "event_callback", "team_id": "TNOTINSTALLED",
		"event_id": "Ev10", "event": map[string]any{"type": "app_mention", "user": "U1", "channel": "C6", "ts": "1.0"}})
	assertNoRetry(t, f.deliver(t, unknown, time.Now(), "", ""), http.StatusNotFound, "unknown workspace")
	// Signed, but structurally unusable: no event_id to dedupe on.
	poison, _ := json.Marshal(map[string]any{"type": "event_callback", "team_id": f.team})
	assertNoRetry(t, f.deliver(t, poison, time.Now(), "", ""), http.StatusBadRequest, "poison envelope")

	if n := f.runCount(t); n != 0 {
		t.Fatalf("%d runs exist after four REJECTED deliveries, want 0 — a refused delivery must write nothing", n)
	}
	var reservations int
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(*) FROM idempotency_records WHERE organization_id=$1 AND project_id=$2`, f.org, f.project).Scan(&reservations); err != nil {
		t.Fatalf("count reservations: %v", err)
	}
	if reservations != 0 {
		t.Fatalf("%d idempotency reservations after four rejected deliveries, want 0", reservations)
	}
}

// TestSlackURLVerificationHandshakeOnTheRealRoute completes the peer's contract surface: the one exchange
// that configures a Request URL at all.
func TestSlackURLVerificationHandshakeOnTheRealRoute(t *testing.T) {
	f := newSlackFixture(t)
	const challenge = "3eZbrw1aBm2rZgRNFdxV2595E9CY3gmdALWMmHkvFXO7tYXAYM8P"

	resp := f.deliver(t, []byte(`{"token":"deprecated","challenge":"`+challenge+`","type":"url_verification"}`), time.Now(), "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("url_verification = %d, want 200", resp.StatusCode)
	}
	got := make([]byte, len(challenge))
	if _, err := resp.Body.Read(got); err != nil && err.Error() != "EOF" {
		t.Fatalf("read challenge: %v", err)
	}
	if string(got) != challenge {
		t.Fatalf("challenge echo = %q, want %q", got, challenge)
	}
	if n := f.runCount(t); n != 0 {
		t.Fatalf("the handshake birthed %d runs, want 0", n)
	}
}

// TestSlackTenantIsNeverPayloadSelectable is the §38.6 invariant on real storage, and the sharpest of the
// negatives: a SECOND tenant exists with its own workspace, and the payload names it every way it can. The
// run must still land in the tenant whose signing secret actually verified the body.
func TestSlackTenantIsNeverPayloadSelectable(t *testing.T) {
	f := newSlackFixture(t)
	victim, victimProject := newID("org"), newID("prj")
	exec(t, f.pool, `INSERT INTO organizations (id) VALUES ($1)`, victim)
	exec(t, f.pool, `INSERT INTO projects (id, organization_id) VALUES ($1,$2)`, victimProject, victim)

	forged, _ := json.Marshal(map[string]any{
		"type": "event_callback", "team_id": f.team, "event_id": "Ev11",
		"organization_id": victim, "project_id": victimProject,
		"org": victim, "project": victimProject, "principal_id": "prin_attacker",
		"agent_revision_id": "arev_attacker",
		"event": map[string]any{"type": "app_mention", "user": "Umapped", "channel": "C7", "ts": "1700000008.000100",
			"organization_id": victim, "project_id": victimProject},
	})
	resp := f.deliver(t, forged, time.Now(), "", "")
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("status = %d, want 2xx — the forged fields are IGNORED, not an error", resp.StatusCode)
	}

	var stolen int
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(*) FROM runs WHERE organization_id=$1`, victim).Scan(&stolen); err != nil {
		t.Fatalf("count victim runs: %v", err)
	}
	if stolen != 0 {
		t.Fatalf("%d runs landed in the tenant the PAYLOAD named; the tenant may only come from the resolved connection", stolen)
	}
	if n := f.runCount(t); n != 1 {
		t.Fatalf("%d runs in the connection's own tenant, want 1", n)
	}
	// The pin is the connection's, not the payload's.
	var revision string
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT COALESCE(agent_revision_id,'') FROM runs WHERE organization_id=$1`, f.org).Scan(&revision); err != nil {
		t.Fatalf("read pin: %v", err)
	}
	if revision != f.revision {
		t.Fatalf("run pinned %q, want the connection's %q — the payload's agent_revision_id must not select a target", revision, f.revision)
	}
}

// TestSlackWorkspaceCannotBeSquattedByAnotherTenant is the REGISTRATION-side half of the tenant boundary, and
// it is the one the payload-side proof above cannot reach: TestSlackTenantIsNeverPayloadSelectable seeds its
// second tenant with NO Slack row, so resolve-then-verify never runs against a competing connection.
//
// The hole three facts compose into: 000035's uniqueness is (organization_id, project_id, team_id,
// enterprise_id) — PER TENANT, not global; the resolve is keyed by team_id alone and runs system-scoped; so an
// ordinary project admin in ANY other org can register the victim's team_id with a secret it controls. The
// resolve then picks a row by chance, and whichever it picks is the one whose secret must verify: the victim's
// own signed events answer 401 WITH the suppress header, which cancels all three Slack retries and turns the
// hijack into permanent silent data loss of another tenant's event stream.
//
// It also bites with no attacker at all — two projects in one org legitimately connecting one workspace
// produce the same nondeterminism. Slack posts to ONE Request URL per app, so two tenants sharing a workspace
// is never legitimate: the registration refuses it.
func TestSlackWorkspaceCannotBeSquattedByAnotherTenant(t *testing.T) {
	f := newSlackFixture(t)
	ctx := context.Background()

	// A second tenant with an ordinary project admin's powers and nothing more.
	squatOrg, squatProject := newID("org"), newID("prj")
	exec(t, f.pool, `INSERT INTO organizations (id) VALUES ($1)`, squatOrg)
	exec(t, f.pool, `INSERT INTO projects (id, organization_id) VALUES ($1,$2)`, squatProject, squatOrg)
	squatPrincipal := newID("prin")
	exec(t, f.pool, `INSERT INTO principals (id, organization_id, project_id, kind) VALUES ($1,$2,$3,'service')`,
		squatPrincipal, squatOrg, squatProject)
	const squatRef = "slack/squatter/signing"
	squatSecret := []byte("the-squatter-signs-with-its-own-secret-not-the-victims")
	f.secrets[squatOrg+"/"+squatRef] = squatSecret

	// It registers the VICTIM's workspace under its own tenant, with a signing secret it controls.
	_, err := extensions.New(f.pool).CreateSlackConnection(ctx, squatOrg, squatProject, []byte(fmt.Sprintf(
		`{"team_id":%q,"signing_secret_ref":%q,"default_policy":{"agent_revision_id":"arev_squat","principal_id":%q}}`,
		f.team, squatRef, squatPrincipal)))
	if !errors.Is(err, extensions.ErrSlackWorkspaceBoundElsewhere) {
		t.Fatalf("registering another tenant's workspace: err = %v, want ErrSlackWorkspaceBoundElsewhere — a team_id already bound in a DIFFERENT org/project must be refused, or the squatter owns the victim's event stream", err)
	}

	// The victim's stream is untouched: its own signed event still admits, under its own tenant.
	resp := f.deliver(t, f.event("EvSquat1", "app_mention", "Umapped", "C20", "1700000020.000100", ""), time.Now(), "", "")
	if resp.StatusCode/100 != 2 {
		t.Fatalf("the victim's OWN signed event = %d, want a 2xx ack — a squatted registration must not be able to 401 another tenant's traffic", resp.StatusCode)
	}
	resp.Body.Close()
	if n := f.runCount(t); n != 1 {
		t.Fatalf("the victim's tenant holds %d runs, want 1", n)
	}

	// And the squatter's own signature buys it nothing: there is no connection of its own to verify against.
	f.terminateRuns(t)
	squatted := f.deliverAs(t, squatSecret, f.event("EvSquat2", "app_mention", "Usquat", "C20", "1700000021.000100", ""), time.Now(), "", "")
	defer squatted.Body.Close()
	if squatted.StatusCode/100 == 2 {
		t.Fatalf("a body signed by the SQUATTER's secret was accepted (%d) — the workspace resolves to the victim's connection, whose secret the squatter does not hold", squatted.StatusCode)
	}
	var stolen int
	if err := f.pool.QueryRow(storage.WithSystemScope(ctx),
		`SELECT count(*) FROM runs WHERE organization_id=$1`, squatOrg).Scan(&stolen); err != nil {
		t.Fatalf("count squatter runs: %v", err)
	}
	if stolen != 0 {
		t.Fatalf("%d runs landed in the squatter's tenant, want 0", stolen)
	}
}

// TestSlackAmbiguousWorkspaceBindingIsRefusedRepairably is the BELT behind the registration guard above. The
// guard is a check-then-insert, so two concurrent registrations in different tenants can still both pass it,
// and a deployment upgraded from before the guard may already hold the rows. Whatever the cause, the resolve
// must never pick one row by chance: pgx's QueryRow calls rows.Next() ONCE and closes, so a second row is
// silently ignored and the winner can flip between requests.
//
// The answer is 503 WITHOUT the suppress header, and the asymmetry is deliberate: an operator can repair a
// 503 (Slack keeps retrying while they delete the wrong row), but a suppressed retry cannot be un-suppressed.
func TestSlackAmbiguousWorkspaceBindingIsRefusedRepairably(t *testing.T) {
	f := newSlackFixture(t)

	otherOrg, otherProject := newID("org"), newID("prj")
	exec(t, f.pool, `INSERT INTO organizations (id) VALUES ($1)`, otherOrg)
	exec(t, f.pool, `INSERT INTO projects (id, organization_id) VALUES ($1,$2)`, otherProject, otherOrg)
	// Written with raw SQL ON PURPOSE: these are the rows the database already accepts (the unique index is
	// per-tenant), i.e. exactly what a pre-guard deployment can be holding right now.
	exec(t, f.pool, `INSERT INTO slack_connections (id, organization_id, project_id, team_id, signing_secret_ref)
	                 VALUES ($1,$2,$3,$4,$5)`,
		newID("slkc"), otherOrg, otherProject, f.team, "slack/other/signing")

	resp := f.deliver(t, f.event("EvAmb1", "app_mention", "Umapped", "C21", "1700000022.000100", ""), time.Now(), "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("an AMBIGUOUS workspace binding = %d, want 503 — the receiver must refuse rather than pick one of two tenants by chance", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Slack-No-Retry"); got != "" {
		t.Fatalf("the ambiguity answer carried X-Slack-No-Retry=%q; an operator can repair a 503, but a suppressed retry is a dropped event nobody can recover", got)
	}
	if n := f.runCount(t); n != 0 {
		t.Fatalf("%d runs were born while the binding was ambiguous, want 0 — neither tenant may be guessed", n)
	}
}

// TestSlackConcurrentFirstEventsNeverSplitAThread is SLK-003's race. Two FIRST events in one thread each read
// "no correlation yet" and each mint their own session; the unique index collapses only the thread ROW, so
// both admissions succeed (different sessions ⇒ the one-active-root index never fires) and the losing run
// ends up in a session the thread does not point at — a conversation silently split in two.
func TestSlackConcurrentFirstEventsNeverSplitAThread(t *testing.T) {
	f := newSlackFixture(t)
	const root = "1700000023.000100"

	var wg sync.WaitGroup
	start := make(chan struct{})
	codes, errs := make([]int, 2), make([]error, 2)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := f.event(fmt.Sprintf("EvRace%d", i), "message", "Umapped", "C22",
				fmt.Sprintf("17000000%d.000100", 24+i), root)
			timestamp, signature := signSlack(f.secret, body, time.Now())
			<-start
			resp, err := f.send(body, timestamp, signature, "", "")
			if err != nil {
				errs[i] = err
				return
			}
			codes[i] = resp.StatusCode
			resp.Body.Close()
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent delivery %d: %v", i, err)
		}
	}
	t.Logf("concurrent first events answered %v", codes)

	if n := f.sessionCount(t); n != 1 {
		t.Fatalf("%d thread↔session rows for one thread, want 1", n)
	}
	var sessions int
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(DISTINCT session_id) FROM runs WHERE organization_id=$1 AND project_id=$2`,
		f.org, f.project).Scan(&sessions); err != nil {
		t.Fatalf("count distinct run sessions: %v", err)
	}
	if sessions > 1 {
		t.Fatalf("two concurrent FIRST events in one thread produced runs across %d sessions, want 1 — the thread row collapses but the SESSIONS do not, so the losing run lives in a session the thread does not point at (SLK-003)", sessions)
	}
	// And the surviving run really is in the session the thread points at.
	var correlated, ran string
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT (SELECT session_id FROM slack_thread_sessions WHERE organization_id=$1 AND thread_ts=$2),
		        (SELECT DISTINCT session_id FROM runs WHERE organization_id=$1)`, f.org, root).Scan(&correlated, &ran); err != nil {
		t.Fatalf("compare the correlated session to the run's: %v", err)
	}
	if correlated != ran {
		t.Fatalf("the thread points at session %q but its run is in %q", correlated, ran)
	}
}

// TestSlackClosedSessionDoesNotBrickTheThread covers the failure a correlation row can outlive: the session it
// points at is closed (a T4 close_session command, an operator, a reap). Every later event in that thread
// chains onto a dead session, and a chained admission onto a non-active session is a typed SessionConflict.
// Classified as terminal it would answer 422 + the suppress header FOREVER, with nothing in the tree that
// repairs the correlation row — one closed session would silently retire a Slack thread for good.
//
// A dead correlation is repairable, so the receiver repairs it: the stale row is dropped and the refusal is
// retryable, so Slack's next attempt opens a fresh session in the same thread.
func TestSlackClosedSessionDoesNotBrickTheThread(t *testing.T) {
	f := newSlackFixture(t)
	const root = "1700000026.000100"

	f.deliver(t, f.event("EvDead1", "app_mention", "Umapped", "C23", root, ""), time.Now(), "", "").Body.Close()
	if n := f.sessionCount(t); n != 1 {
		t.Fatalf("the first event correlated %d threads, want 1", n)
	}
	f.terminateRuns(t)
	exec(t, f.pool, `UPDATE sessions SET state='closed' WHERE organization_id=$1 AND project_id=$2`, f.org, f.project)

	body := f.event("EvDead2", "message", "Umapped", "C23", "1700000027.000100", root)
	resp := f.deliver(t, body, time.Now(), "", "")
	if got := resp.Header.Get("X-Slack-No-Retry"); got == "1" {
		t.Fatalf("an event on a thread whose session is CLOSED answered %d with X-Slack-No-Retry=1 — the thread is then bricked forever: nothing repairs the correlation row, so every future message in it is refused",
			resp.StatusCode)
	}
	resp.Body.Close()
	if n := f.sessionCount(t); n != 0 {
		t.Fatalf("%d thread↔session rows survive, want 0 — a correlation pointing at a dead session must be dropped so the thread can open a new one", n)
	}

	// Slack's redelivery of the SAME event now opens a fresh session and admits: one delayed message rather
	// than a retired thread.
	retry := f.deliver(t, body, time.Now(), "1", "http_error")
	defer retry.Body.Close()
	if retry.StatusCode/100 != 2 {
		t.Fatalf("the redelivery after the repair = %d, want a 2xx ack", retry.StatusCode)
	}
	if n := f.runCount(t); n != 2 {
		t.Fatalf("%d runs, want 2 — the repaired thread must admit the message the dead session refused", n)
	}
	var state string
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT s.state FROM slack_thread_sessions t JOIN sessions s ON s.id = t.session_id
		  WHERE t.organization_id=$1 AND t.thread_ts=$2`, f.org, root).Scan(&state); err != nil {
		t.Fatalf("read the repaired correlation: %v", err)
	}
	if state != "active" {
		t.Fatalf("the thread was re-correlated to a %q session, want an active one", state)
	}
}

// TestSlackMessageDuringALiveRunNeverBirthsASecondRun documents, with a test rather than a comment, the ONE
// place where the shipped route is narrower than the §63.3 journey — because a reader deserves to find this
// as a green assertion of what actually happens, not as a surprise in production.
//
// What HOLDS (SLK-001's hard half): a Slack message arriving while the thread's run is live never opens a
// second run. The session's one-active-root index (000006) refuses it, the admission rolls back whole, and
// the route reports a RETRYABLE 429 with no suppress header — so Slack's +1min/+5min attempts get a second
// and third chance, which land if the run has finished by then.
//
// What is NOT wired by T1, stated plainly: the journey's richer behaviour, where such a message becomes a
// QUEUED send_message on the LIVE run instead of waiting for a new one. That path goes through the
// coordinator's command surface, not through admission, and the E17 T11 journey reaches it via the trigger
// pipeline's named_session correlation. Wiring it here would mean giving this bridge a command seam it does
// not have. Consequence, so nobody has to infer it: a follow-up message sent while the agent is still
// working is delivered late (on a retry) or, if all three attempts fall inside the run, dropped.
func TestSlackMessageDuringALiveRunNeverBirthsASecondRun(t *testing.T) {
	f := newSlackFixture(t)
	const root = "1700000009.000100"

	f.deliver(t, f.event("Ev12", "app_mention", "Umapped", "C8", root, ""), time.Now(), "", "").Body.Close()
	if n := f.runCount(t); n != 1 {
		t.Fatalf("the first event birthed %d runs, want 1", n)
	}
	// The run is still queued/live. A follow-up arrives in the same thread.
	resp := f.deliver(t, f.event("Ev13", "message", "Umapped", "C8", "1700000010.000100", root), time.Now(), "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("a message during a live run = %d, want 429 — the one-active-root rule refuses it as LOAD, not as poison", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Slack-No-Retry"); got != "" {
		t.Fatalf("the 429 carried X-Slack-No-Retry=%q; suppressing the retry here would DROP the message outright rather than give the finishing run a chance", got)
	}
	if got := resp.Header.Get("Retry-After"); got == "" {
		t.Fatal("a 429 must pair with Retry-After (§20.12)")
	}
	if n := f.runCount(t); n != 1 {
		t.Fatalf("%d runs after the follow-up, want still 1 — a message during a live run must never open a second run (SLK-001)", n)
	}
	// And nothing partial was left behind: the refused admission rolled back whole.
	var reservations int
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(*) FROM idempotency_records WHERE organization_id=$1 AND project_id=$2`, f.org, f.project).Scan(&reservations); err != nil {
		t.Fatalf("count reservations: %v", err)
	}
	if reservations != 1 {
		t.Fatalf("%d reservations, want 1 — the refused admission must leave no record, so the retry is free to succeed", reservations)
	}
}

// terminateRuns drives every run in the fixture's tenant to a terminal state, so the next event in a thread
// can admit. A session holds ONE active root run (000006, spec §22.3) and this suite starts no dispatcher, so
// a run stays queued forever unless a test finishes it. This is a HARNESS shortcut, not a claim about
// execution: it writes the terminal state directly rather than driving a run to completion.
func (f *slackFixture) terminateRuns(t *testing.T) {
	t.Helper()
	exec(t, f.pool, `UPDATE runs SET state='completed' WHERE organization_id=$1 AND project_id=$2`, f.org, f.project)
}

func assertNoRetry(t *testing.T, resp *http.Response, want int, what string) {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("%s = %d, want %d", what, resp.StatusCode, want)
	}
	if got := resp.Header.Get("X-Slack-No-Retry"); got != "1" {
		t.Fatalf("%s answered %d with X-Slack-No-Retry=%q, want \"1\" — Slack would otherwise pull this poison event three more times (plan §3.5 D1)",
			what, resp.StatusCode, got)
	}
}

// TestSlackEventStoresTheHumanMessageAsTheRunInput is the projection fix over the SHIPPED route and a real
// database: what lands in responses.input is what the orchestrator hands the engine as run.start's `input`,
// which the engine appends as the user message and model_dispatch passes to the provider verbatim when it is
// a string. So this column IS the prompt, and asserting it here asserts what the model reads.
//
// The regression it pins: the input used to be the Slack envelope, and a real workspace's first mention was
// answered with "It looks like you have shared a JSON object that represents a message event from Slack…".
func TestSlackEventStoresTheHumanMessageAsTheRunInput(t *testing.T) {
	f := newSlackFixture(t)

	f.deliver(t, f.eventText(t, "EvIn1", "app_mention", "Umapped", "C80", "1700000080.000100", "",
		"<@"+f.botUser+"> merhaba"), time.Now(), "", "").Body.Close()

	var input string
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT resp.input::text FROM responses resp WHERE resp.organization_id=$1 AND resp.project_id=$2`,
		f.org, f.project).Scan(&input); err != nil {
		t.Fatalf("read the stored run input: %v", err)
	}
	if input != `"merhaba"` {
		t.Fatalf("responses.input = %s, want the bare JSON string \"merhaba\" — an object reaches the model as raw JSON", input)
	}
	// Nothing about the transport, the tenant or the clicker may ride along: the run's identity is the
	// connection's principal (asserted elsewhere) and a prompt is not where scope belongs.
	for _, leaked := range []string{f.team, f.principal, f.revision, "C80", "Umapped", "EvIn1", "app_mention"} {
		if strings.Contains(input, leaked) {
			t.Fatalf("responses.input carries %q: %s", leaked, input)
		}
	}
}
