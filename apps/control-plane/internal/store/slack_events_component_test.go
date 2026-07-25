//go:build component

package store_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/internal/extensions"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
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
	pool      *pgxpool.Pool
	url       string
	secret    []byte
	org       string
	project   string
	principal string
	revision  string
	team      string
	botUser   string
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
		pool: pool, secret: []byte("component-signing-secret-not-a-credential"),
		org: newID("org"), project: newID("prj"), principal: newID("prin"),
		revision: newID("arev"), team: strings.ToUpper(newID("T")), botUser: newID("Ubot"),
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
	const signingRef = "slack/component/signing"
	if _, err := ext.CreateSlackConnection(ctx, f.org, f.project, []byte(fmt.Sprintf(
		`{"team_id":%q,"bot_user_id":%q,"signing_secret_ref":%q,
		  "allowed_users":["Umapped"],
		  "default_policy":{"agent_revision_id":%q,"principal_id":%q}}`,
		f.team, f.botUser, signingRef, f.revision, f.principal))); err != nil {
		t.Fatalf("register the Slack workspace: %v", err)
	}

	// The org-scoped secret bridge, the production resolver's shape: a ref only resolves under the org it was
	// provisioned in, so a connection can never redeem another tenant's secret.
	secrets := func(org, ref string) ([]byte, error) {
		if org != f.org || ref != signingRef {
			return nil, fmt.Errorf("no secret bridge for %q/%q", org, ref)
		}
		return f.secret, nil
	}
	bridge := extensions.NewSlackAdmitter(ext, repo, secrets, api.AdmissionLimits{})
	ts := httptest.NewServer(api.NewRouter(nil, repo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		api.SSEConfig{}, nil, nil, api.WithSlack(bridge)))
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
	timestamp := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, f.secret)
	mac.Write([]byte("v0:" + timestamp + ":"))
	mac.Write(body)
	return f.deliverSigned(t, body, timestamp, "v0="+hex.EncodeToString(mac.Sum(nil)), retryNum, retryReason)
}

func (f *slackFixture) deliverSigned(t *testing.T, body []byte, timestamp, signature, retryNum, retryReason string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.url+"/v1/slack/events", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("build request: %v", err)
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
	resp, err := http.DefaultClient.Do(req)
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
	inner := map[string]any{"type": innerType, "user": user, "channel": channel, "ts": ts, "text": "hello"}
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
	// A second event in the SAME thread. It chains onto the thread's session rather than claiming a new one.
	resp := f.deliver(t, f.event("Ev4", "message", "Umapped", "C3", "1700000003.000100", root), time.Now(), "", "")
	resp.Body.Close()
	if n := f.sessionCount(t); n != 1 {
		t.Fatalf("a second event in the same thread produced %d thread↔session rows, want 1 canonical session (SLK-003)", n)
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
	for _, body := range [][]byte{edit, del} {
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
	var kinds []string
	rows, err := f.pool.Query(storage.WithSystemScope(context.Background()),
		`SELECT input->>'kind' FROM responses WHERE organization_id=$1 AND project_id=$2 ORDER BY input->>'event_id'`,
		f.org, f.project)
	if err != nil {
		t.Fatalf("read admitted kinds: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			t.Fatalf("scan kind: %v", err)
		}
		kinds = append(kinds, kind)
	}
	if len(kinds) != 2 || kinds[0] != "correction" || kinds[1] != "tombstone" {
		t.Fatalf("admitted kinds = %v, want [correction tombstone] — an edit supersedes and a delete retracts; neither is a fresh turn (SLK-005)", kinds)
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
