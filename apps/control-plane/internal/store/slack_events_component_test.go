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
	// The E20 image leg (slack_image_component_test.go): the local stand-in for Slack's FILE hosts and a
	// recording stand-in for the object store. Both are mounted on every fixture so a file-less delivery is
	// PROVEN to touch neither.
	fileHost  *fakeSlackFileHost
	artifacts *recordingInboundArtifacts
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
	// scripted queues per-METHOD replies, consumed one per call. The positional `statuses` script above
	// cannot express "the second append is refused" once a run also sets a status and opens a stream, and
	// E20 T1's cancel and rate-limit proofs need exactly that.
	scripted map[string][]scriptedReply
}

type slackCall struct {
	path string
	auth string
	body string
	// query is the URL's raw query string. Every WRITE this stack makes is a POST carrying JSON, so it is
	// empty for those; a READ (conversations.replies) is a documented GET, and its arguments are the only
	// place the channel and thread it addressed can be seen.
	query string
}

// scriptedReply is one queued answer for a method: an HTTP status plus the envelope body ("" takes the
// method's documented success envelope).
type scriptedReply struct {
	status int
	body   string
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
	s.calls = append(s.calls, slackCall{path: r.URL.Path, auth: r.Header.Get("Authorization"),
		body: string(body), query: r.URL.RawQuery})
	ts := fmt.Sprintf("9%02d.000100", len(s.calls))
	after := s.retryAfter
	var scripted *scriptedReply
	if queue := s.scripted[r.URL.Path]; len(queue) > 0 {
		reply := queue[0]
		s.scripted[r.URL.Path] = queue[1:]
		scripted = &reply
	}
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if scripted != nil {
		if scripted.status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "0")
		}
		w.WriteHeader(scripted.status)
		if scripted.body == "" {
			scripted.body = slackOKEnvelope(r.URL.Path, ts)
		}
		_, _ = w.Write([]byte(scripted.body))
		return
	}
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
	_, _ = w.Write([]byte(slackOKEnvelope(r.URL.Path, ts)))
}

// slackOKEnvelope is the DOCUMENTED success shape of each method this stack calls. It matters that these come
// from the published references rather than from our own client: a fake shaped by the code it tests confirms
// itself, which is exactly how E17 T10 shipped an invented event.
//
// CONTRACT (all checked 2026-07-27):
//   - https://docs.slack.dev/reference/methods/chat.startStream/  → {"ok":true,"channel":…,"ts":…}
//   - https://docs.slack.dev/reference/methods/chat.appendStream/ → {"ok":true,"channel":…,"ts":…}
//   - https://docs.slack.dev/reference/methods/chat.stopStream/   → {"ok":true,"channel":…,"ts":…,"message":{…}}
//   - https://docs.slack.dev/reference/methods/assistant.threads.setStatus/ → {"ok":true}
//   - https://docs.slack.dev/reference/methods/conversations.replies/ → {"ok":true,"messages":[…],"has_more":…}
func slackOKEnvelope(path, ts string) string {
	switch path {
	case "/assistant.threads.setStatus":
		return `{"ok":true}`
	case "/conversations.replies":
		// An EMPTY thread by default, which is the honest shape for a fixture that scripted no history: the
		// read succeeded and the thread had nothing in it. A test that wants messages scripts them.
		return `{"ok":true,"messages":[],"has_more":false}`
	case "/chat.startStream", "/chat.appendStream":
		return `{"ok":true,"channel":"C1","ts":"` + ts + `"}`
	case "/chat.stopStream":
		return `{"ok":true,"channel":"C1","ts":"` + ts + `","message":{"text":"","bot_id":"B1","ts":"` + ts + `","type":"message"}}`
	}
	return `{"ok":true,"ts":"` + ts + `"}`
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

// slackScript queues per-method replies, consumed one per call to that method. `body` "" takes the method's
// documented success envelope, so a caller scripting only a 429 does not have to restate the success shape.
func (f *slackFixture) slackScript(path string, replies ...scriptedReply) {
	f.slack.mu.Lock()
	defer f.slack.mu.Unlock()
	if f.slack.scripted == nil {
		f.slack.scripted = map[string][]scriptedReply{}
	}
	f.slack.scripted[path] = append(f.slack.scripted[path], replies...)
}

// slackRefuse queues one documented API refusal ({"ok":false,"error":code}) for a method — an HTTP 200 that
// Slack nonetheless says no to, which is a different failure from a transport error and must be handled as
// one.
func (f *slackFixture) slackRefuse(path, code string) {
	f.slackScript(path, scriptedReply{status: http.StatusOK, body: `{"ok":false,"error":"` + code + `"}`})
}

// callsTo returns every call the fake saw on one method, in order.
func (f *slackFixture) callsTo(path string) []slackCall {
	var out []slackCall
	for _, c := range f.slackCalls() {
		if c.path == path {
			out = append(out, c)
		}
	}
	return out
}

// awaitCalls waits until the fake has seen at least n calls to a method, so a test never races the follower's
// journal poll. It fails the test rather than hanging.
func (f *slackFixture) awaitCalls(t *testing.T, path string, n int) []slackCall {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if calls := f.callsTo(path); len(calls) >= n {
			return calls
		}
		if time.Now().After(deadline) {
			t.Fatalf("fake Slack saw %d call(s) to %s within 15s, want at least %d (all calls: %v)",
				len(f.callsTo(path)), path, n, f.slackCalls())
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
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
	// The IMAGE leg is mounted on EVERY fixture, not only the image tests, and that is the point: a payload
	// with no files must produce no fetch, no artifact and a byte-identical input, and the only way that is a
	// fact rather than a hope is for the leg to be present while the other tests run.
	//
	// The file host is a Doer rather than a local httptest server BECAUSE the host allow-list is real: it
	// admits only https on Slack's own file hosts, so a http://127.0.0.1:PORT fixture would be refused (which
	// the unit tests in adapters/integrations/slack assert directly). The URL here is therefore a genuine
	// https://files.slack.com address and only the transport is local — the guard runs for real.
	f.fileHost = &fakeSlackFileHost{content: componentPNG}
	f.artifacts = &recordingInboundArtifacts{}
	bridge := extensions.NewSlackAdmitter(ext, repo, secrets, api.AdmissionLimits{}).
		WithDecisions(f.spine, http.DefaultClient, slackAPI.URL).
		WithFileFetch(f.fileHost, f.artifacts)
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

// answerRuns finishes every run in the tenant AND gives its response a terminal projection carrying `text`,
// so the exchange is COMPLETE. terminateRuns alone is not enough for any assertion about history: a response
// with no output is skipped by execution.historyMessages ("neither half of the exchange is settled"), so a
// history proof built on it would pass whether or not the code under test does anything — the twelfth
// green-by-vacuity in this tree. This is a HARNESS shortcut like terminateRuns: it writes the terminal
// projection directly rather than driving an engine.
func (f *slackFixture) answerRuns(t *testing.T, text string) {
	t.Helper()
	f.terminateRuns(t)
	exec(t, f.pool, `UPDATE responses SET state='completed', output=$3::jsonb
	                 WHERE organization_id=$1 AND project_id=$2 AND output IS NULL`,
		f.org, f.project, `{"output":[{"type":"message","content":`+strconv.Quote(text)+`}]}`)
}

// threadSessionID is the canonical session the fixture's single correlated thread resolved to.
func (f *slackFixture) threadSessionID(t *testing.T) string {
	t.Helper()
	var session string
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT session_id FROM slack_thread_sessions WHERE organization_id=$1 AND project_id=$2`,
		f.org, f.project).Scan(&session); err != nil {
		t.Fatalf("read the thread's session: %v", err)
	}
	return session
}

// responseIDs lists the tenant's responses in creation order, with the input each one carries.
func (f *slackFixture) responseIDs(t *testing.T) (ids, inputs []string) {
	t.Helper()
	rows, err := f.pool.Query(storage.WithSystemScope(context.Background()),
		`SELECT id, input::text FROM responses WHERE organization_id=$1 AND project_id=$2 ORDER BY created_at, id`,
		f.org, f.project)
	if err != nil {
		t.Fatalf("read responses: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, input string
		if err := rows.Scan(&id, &input); err != nil {
			t.Fatalf("scan response: %v", err)
		}
		ids, inputs = append(ids, id), append(inputs, input)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read responses: %v", err)
	}
	return ids, inputs
}

// modelHistory is THE HISTORY SHOWN TO THE MODEL, read through the SHIPPED query: coordinator.SessionHistory
// is what execution.historyMessages assembles run.start's conversation from, so a turn this returns is a turn
// the provider is told about. Asserting here rather than on a column is the difference between "a flag was
// written" and "the words stopped reaching the model".
func (f *slackFixture) modelHistory(t *testing.T, session, before string) []coordinator.PriorResponse {
	t.Helper()
	prior, err := f.spine.SessionHistory(context.Background(),
		coordinator.Tenant{Organization: f.org, Project: f.project}, session, before)
	if err != nil {
		t.Fatalf("read session history: %v", err)
	}
	return prior
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

// TestSlackEditsAndDeletesReachAdmissionAsTheirOwnKind is SLK-005 over the wire, and the kind is what DECIDES
// the effect: an edit SUPERSEDES the turn it edits and a delete RETRACTS the turn it removes. Neither is a new
// thing said, so neither births a run — the words already in the conversation are what change.
//
// THE DEFECT IT PINS, found against the owner's real workspace minutes after the run-birth rule shipped: a
// deletion inside a thread the app held satisfied "a follow-up in a thread we already hold", so it birthed a
// run, and the model — handed "(the user deleted their message…)" as a prompt — replied "it seems you deleted
// your message, would you like help with something else?". The run-birth rule was about WHERE an event
// happened; this one is about WHAT arrived, and both are needed.
//
// CONTRACT: https://docs.slack.dev/apis/events-api/ (checked 2026-07-25) — message_changed / message_deleted
// are SUBTYPES of the `message` event, and they nest the affected message under `message` /
// `previous_message` rather than at the top level. The nested `ts` is the ORIGINAL message's, which is the
// handle both effects act on.
func TestSlackEditsAndDeletesReachAdmissionAsTheirOwnKind(t *testing.T) {
	f := newSlackFixture(t)
	const root = "1700000005.000100"
	const followUp = "1700000005.000200"

	// The thread has to be OURS before an edit or a delete inside it means anything: since the run-birth rule
	// (slackBirthsRun) an unaddressed channel message births nothing, and an edit to a message the app was
	// never talking about is exactly that. So the conversation opens the way a real one does — with a mention —
	// and SLK-005's claim is then made about a thread the app is actually in, which is the only place the claim
	// was ever true. TestSlackEditOrDeleteOutsideOurThreadsBirthsNothing owns the other side.
	f.deliver(t, f.eventText(t, "Ev6a", "app_mention", "Umapped", "C4", root, "",
		"<@"+f.botUser+"> ilk soru"), time.Now(), "", "").Body.Close()
	f.answerRuns(t, "ilk cevap")

	// A SECOND turn in the same thread, because history is "everything before this response": without a later
	// response there is nothing to look back FROM, and the retraction proof would have no vantage point. It is
	// also what a real thread does next.
	f.deliver(t, f.eventText(t, "Ev6b", "message", "Umapped", "C4", followUp, root,
		"ve sonra?"), time.Now(), "", "").Body.Close()
	f.answerRuns(t, "ikinci cevap")

	ids, inputs := f.responseIDs(t)
	if len(ids) != 2 || inputs[0] != `"ilk soru"` {
		t.Fatalf("the thread opened with %v, want two turns whose first is the bare question", inputs)
	}
	session, first, second := f.threadSessionID(t), ids[0], ids[1]

	// POSITIVE CONTROL, and the assertions below are worth nothing without it: the first turn IS in the
	// history the second run was shown. A "the turn is gone" proof over a history that was empty anyway is
	// the vacuous shape this tree has shipped twelve times.
	if prior := f.modelHistory(t, session, second); len(prior) != 1 || string(prior[0].Input) != `"ilk soru"` {
		t.Fatalf("the second run's history = %v, want the first turn — the retraction proof needs something to retract", prior)
	}

	// 1. THE EDIT. It supersedes the turn it names: no new run, and the STORED turn — the one every later run
	//    is shown — now carries the corrected words.
	edit := mustJSON(map[string]any{
		"type": "event_callback", "team_id": f.team, "event_id": "Ev6",
		"event": map[string]any{"type": "message", "subtype": "message_changed", "channel": "C4",
			"message": map[string]any{"user": "Umapped", "ts": root, "thread_ts": root, "text": "düzeltilmiş soru"}},
	})
	resp := f.deliver(t, edit, time.Now(), "", "")
	if resp.StatusCode/100 != 2 {
		t.Fatalf("edit = %d, want a 2xx ack", resp.StatusCode)
	}
	resp.Body.Close()
	if n := f.runCount(t); n != 2 {
		t.Fatalf("the edit brought the run total to %d, want 2 — a correction supersedes a turn, it does not open one", n)
	}
	if _, inputs := f.responseIDs(t); inputs[0] != `"(edited) düzeltilmiş soru"` {
		t.Fatalf("the superseded turn reads %s, want the corrected text marked as an edit (SLK-005: it supersedes, it is not a fresh turn)", inputs[0])
	}
	if prior := f.modelHistory(t, session, second); len(prior) != 1 || string(prior[0].Input) != `"(edited) düzeltilmiş soru"` {
		t.Fatalf("the history shown to the model = %v, want the CORRECTED turn — an edit nobody is shown is not a correction", prior)
	}

	// 2. THE DELETE. It retracts: no run, and the turn stops being part of the conversation the model is
	//    shown. A user who deletes a message expects it to stop influencing the agent, and since history
	//    carries USER turns (ed44544) a retracted turn left visible is a genuine leak.
	del := mustJSON(map[string]any{
		"type": "event_callback", "team_id": f.team, "event_id": "Ev7",
		"event": map[string]any{"type": "message", "subtype": "message_deleted", "channel": "C4",
			"previous_message": map[string]any{"user": "Umapped", "ts": root, "thread_ts": root, "text": "düzeltilmiş soru"}},
	})
	resp = f.deliver(t, del, time.Now(), "", "")
	if resp.StatusCode/100 != 2 {
		t.Fatalf("delete = %d, want a 2xx ack", resp.StatusCode)
	}
	resp.Body.Close()
	if n := f.runCount(t); n != 2 {
		t.Fatalf("the deletion brought the run total to %d, want 2 — THE LIVE DEFECT: the app answered a deletion", n)
	}
	if prior := f.modelHistory(t, session, second); len(prior) != 0 {
		t.Fatalf("the history shown to the model still carries %v after the turn was deleted — a retraction that keeps feeding the words to the model retracts nothing", prior)
	}
	// The response itself is NOT destroyed: what the user wrote and that they withdrew it are both facts an
	// operator can still read. Retraction is about what the model is shown, not about erasing the ledger.
	var retracted bool
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT retracted_at IS NOT NULL FROM responses WHERE id=$1`, first).Scan(&retracted); err != nil {
		t.Fatalf("read the retracted turn: %v", err)
	}
	if !retracted {
		t.Fatal("the deleted turn's response is not marked retracted")
	}
	// All of it stayed ONE conversation: neither the edit nor the delete opened a thread of its own.
	if n := f.sessionCount(t); n != 1 {
		t.Fatalf("the edit and the delete produced %d thread↔session rows, want 1 — both belong to the same thread", n)
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
			// app_mention, not a bare message: two concurrent FIRST events in a thread nobody has correlated
			// yet is precisely the case the run-birth rule turns away for a plain message (there is no thread
			// of ours to follow up in), and a mention is what a real first event in a thread IS.
			body := f.event(fmt.Sprintf("EvRace%d", i), "app_mention", "Umapped", "C22",
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
// A dead correlation is repairable, so the receiver repairs it — at LOOKUP time, in the delivery that finds
// it: the stale row is dropped and the very same message opens a fresh session in the same thread. Two things
// make that placement load bearing rather than tidy. The run-birth rule admits a bare follow-up only in a
// thread that is OURS, and a repair that waited for a redelivery would have thrown that fact away between the
// two deliveries — a thread whose session died would then answer nothing again until somebody re-mentioned the
// bot. And Socket Mode has no redelivery at all (the envelope is acked before dispatch), so a repair that
// needs one never completes there.
func TestSlackClosedSessionDoesNotBrickTheThread(t *testing.T) {
	f := newSlackFixture(t)
	const root = "1700000026.000100"

	f.deliver(t, f.event("EvDead1", "app_mention", "Umapped", "C23", root, ""), time.Now(), "", "").Body.Close()
	if n := f.sessionCount(t); n != 1 {
		t.Fatalf("the first event correlated %d threads, want 1", n)
	}
	f.terminateRuns(t)
	exec(t, f.pool, `UPDATE sessions SET state='closed' WHERE organization_id=$1 AND project_id=$2`, f.org, f.project)

	// A BARE follow-up, with no mention: the case the run-birth rule would turn away if the repair had let the
	// thread stop being ours.
	body := f.event("EvDead2", "message", "Umapped", "C23", "1700000027.000100", root)
	resp := f.deliver(t, body, time.Now(), "", "")
	if got := resp.Header.Get("X-Slack-No-Retry"); got == "1" {
		t.Fatalf("an event on a thread whose session is CLOSED answered %d with X-Slack-No-Retry=1 — the thread is then bricked forever: nothing repairs the correlation row, so every future message in it is refused",
			resp.StatusCode)
	}
	if resp.StatusCode/100 != 2 {
		t.Fatalf("the message that found the dead session = %d, want a 2xx ack — the repair runs in THIS delivery, so there is nothing to redeliver", resp.StatusCode)
	}
	resp.Body.Close()
	if n := f.runCount(t); n != 2 {
		t.Fatalf("%d runs, want 2 — the message that found the dead correlation must itself be admitted into the fresh session", n)
	}
	if n := f.sessionCount(t); n != 1 {
		t.Fatalf("%d thread↔session rows, want 1 — the dead row is dropped and the thread re-correlated in one delivery", n)
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

// TestSlackOnlyMentionsAndFollowUpsBirthRuns is the RUN-BIRTH RULE, and it is here because the first real live
// run against the owner's workspace answered EVERY message in the channel it had been invited to. `message`
// and `app_mention` shared one branch, so ordinary chatter between two colleagues opened a run each.
//
// The rule, and why each half exists:
//
//   - IN A CHANNEL a message births a run only if it is an app_mention, or if it lands in a thread this
//     connection already has a session for — a follow-up to the bot's own conversation, where re-mentioning it
//     every line would be absurd. Everything else is other people talking.
//   - IN A DM every message births a run. That IS the agent panel; there is nothing else a DM to the app
//     could mean, and Slack's own channel_type == "im" is the authority (slack.Event.IsDM).
//
// The `message.channels` SUBSCRIPTION is untouched, and that is deliberate: SLK-002 and SLK-005 need those
// events (edits, deletes, file shares in threads the bot is in). What changed is what BIRTHS A RUN, not what
// arrives.
func TestSlackOnlyMentionsAndFollowUpsBirthRuns(t *testing.T) {
	f := newSlackFixture(t)
	const root = "1700000061.000100"

	// 1. ORDINARY CHATTER: a channel message with no mention, in no thread of ours. Acknowledged — Slack
	//    delivered it, and refusing the delivery would only earn three redeliveries of the same message — and
	//    nothing else at all: no run, no session, no thread correlation.
	chatter := f.deliver(t, f.eventText(t, "EvBirth1", "message", "Umapped", "C60", "1700000060.000100", "",
		"anyone up for lunch?"), time.Now(), "", "")
	if chatter.StatusCode/100 != 2 {
		t.Fatalf("ordinary channel chatter = %d, want a 2xx ack (it is handled, it is simply not a question for us)", chatter.StatusCode)
	}
	chatter.Body.Close()
	if n := f.runCount(t); n != 0 {
		t.Fatalf("an ordinary channel message birthed %d run(s), want 0 — the bot must answer mentions and its own threads, not the channel", n)
	}
	if n := f.sessionCount(t); n != 0 {
		t.Fatalf("an ordinary channel message claimed %d thread↔session row(s), want 0", n)
	}

	// 2. THE SAME CHANNEL, WITH A MENTION. This is what the integration is for.
	f.deliver(t, f.eventText(t, "EvBirth2", "app_mention", "Umapped", "C60", root, "",
		"<@"+f.botUser+"> ship it"), time.Now(), "", "").Body.Close()
	if n := f.runCount(t); n != 1 {
		t.Fatalf("a mention birthed %d run(s), want 1", n)
	}

	// 3. A FOLLOW-UP INSIDE THE THREAD THE MENTION OPENED, with no mention of its own. The thread is ours, so
	//    this is the conversation continuing (SLK-003) — the owner's live session did exactly this four times.
	f.terminateRuns(t) // one active root run per session; the previous one has to finish first
	followUp := f.deliver(t, f.eventText(t, "EvBirth3", "message", "Umapped", "C60", "1700000062.000100", root,
		"and the release notes?"), time.Now(), "", "")
	if followUp.StatusCode/100 != 2 {
		t.Fatalf("a follow-up in our own thread = %d, want a 2xx ack", followUp.StatusCode)
	}
	followUp.Body.Close()
	if n := f.runCount(t); n != 2 {
		t.Fatalf("a follow-up in a thread the bot is correlated with birthed a total of %d run(s), want 2 — a thread we are in does not need re-mentioning", n)
	}

	// 4. A DM. Every message is a turn; that is the whole of the panel.
	f.terminateRuns(t)
	dm := f.deliver(t, f.dmEvent("EvBirth4", "Umapped", "D024BE91L", "1700000063.000100", "",
		"what is left on the release?"), time.Now(), "", "")
	if dm.StatusCode/100 != 2 {
		t.Fatalf("a DM = %d, want a 2xx ack", dm.StatusCode)
	}
	dm.Body.Close()
	if n := f.runCount(t); n != 3 {
		t.Fatalf("a DM birthed a total of %d run(s), want 3 — every panel message is a turn", n)
	}
}

// TestSlackDeletingAReplyRetractsThatReplyOnly is the wrong-turn guard, and it is the case a real user hits
// first: they delete a FOLLOW-UP, not the message that opened the thread. Inside a thread the deleted
// message's ts and the thread's ts are DIFFERENT numbers, and a handle keyed on the thread — the only
// per-thread key this tree had before 000042 — would retract the conversation's FIRST turn instead. So the
// claim is not just "a turn was retracted", it is "that one, and only that one".
func TestSlackDeletingAReplyRetractsThatReplyOnly(t *testing.T) {
	f := newSlackFixture(t)
	const root, reply, third = "1700000070.000100", "1700000070.000200", "1700000070.000300"

	f.deliver(t, f.eventText(t, "EvRep1", "app_mention", "Umapped", "C70", root, "",
		"<@"+f.botUser+"> birinci"), time.Now(), "", "").Body.Close()
	f.answerRuns(t, "birinci cevap")
	f.deliver(t, f.eventText(t, "EvRep2", "message", "Umapped", "C70", reply, root, "ikinci"), time.Now(), "", "").Body.Close()
	f.answerRuns(t, "ikinci cevap")
	f.deliver(t, f.eventText(t, "EvRep3", "message", "Umapped", "C70", third, root, "üçüncü"), time.Now(), "", "").Body.Close()
	f.answerRuns(t, "üçüncü cevap")

	ids, inputs := f.responseIDs(t)
	if len(ids) != 3 {
		t.Fatalf("the thread holds %v, want three turns", inputs)
	}
	session := f.threadSessionID(t)
	// POSITIVE CONTROL: the third run was shown both earlier turns, in order.
	if prior := f.modelHistory(t, session, ids[2]); len(prior) != 2 {
		t.Fatalf("the third run's history = %v, want both earlier turns", prior)
	}

	// Delete the MIDDLE message — a threaded reply, whose ts is not the thread's.
	f.deliver(t, mustJSON(map[string]any{
		"type": "event_callback", "team_id": f.team, "event_id": "EvRep4",
		"event": map[string]any{"type": "message", "subtype": "message_deleted", "channel": "C70",
			"previous_message": map[string]any{"user": "Umapped", "ts": reply, "thread_ts": root, "text": "ikinci"}},
	}), time.Now(), "", "").Body.Close()

	if n := f.runCount(t); n != 3 {
		t.Fatalf("deleting a reply brought the run total to %d, want 3", n)
	}
	prior := f.modelHistory(t, session, ids[2])
	if len(prior) != 1 {
		t.Fatalf("history after deleting the reply = %v, want exactly the first turn", prior)
	}
	if got := string(prior[0].Input); got != `"birinci"` {
		t.Fatalf("history kept %s, want the FIRST turn — a handle keyed on the thread would have retracted this one instead of the reply", got)
	}
}

// TestSlackEditOrDeleteOutsideOurThreadsBirthsNothing is the run-birth rule applied to the kinds SLK-005
// classifies. An edit is a correction and a delete is a tombstone — but only of a conversation we are in.
// Someone editing their own message in a channel the bot merely sits in is not addressing the bot, and the
// live incident was exactly the deleted half: a `message_deleted` for a message nobody had answered birthed a
// run whose every outbound call was then refused, because the thread it named no longer existed.
func TestSlackEditOrDeleteOutsideOurThreadsBirthsNothing(t *testing.T) {
	f := newSlackFixture(t)
	const orphan = "1700000065.000100"

	for _, body := range [][]byte{
		mustJSON(map[string]any{
			"type": "event_callback", "team_id": f.team, "event_id": "EvOrphan1",
			"event": map[string]any{"type": "message", "subtype": "message_changed", "channel": "C61",
				"message": map[string]any{"user": "Umapped", "ts": orphan, "thread_ts": orphan, "text": "edited"}},
		}),
		mustJSON(map[string]any{
			"type": "event_callback", "team_id": f.team, "event_id": "EvOrphan2",
			"event": map[string]any{"type": "message", "subtype": "message_deleted", "channel": "C61",
				"previous_message": map[string]any{"user": "Umapped", "ts": orphan, "thread_ts": orphan, "text": "gone"}},
		}),
	} {
		resp := f.deliver(t, body, time.Now(), "", "")
		if resp.StatusCode/100 != 2 {
			t.Fatalf("an edit/delete outside our threads = %d, want a 2xx ack", resp.StatusCode)
		}
		resp.Body.Close()
	}
	if n := f.runCount(t); n != 0 {
		t.Fatalf("an edit and a delete in a thread the bot has never been in birthed %d run(s), want 0", n)
	}
}

func mustJSON(v map[string]any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}
