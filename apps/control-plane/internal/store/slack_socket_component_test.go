//go:build component

package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/palgroup/palai/adapters/integrations/slack"
	"github.com/palgroup/palai/apps/control-plane/internal/extensions"
	"github.com/palgroup/palai/storage"
)

// E19 T3's TRANSPORT-INVARIANCE proof, against REAL PostgreSQL and the REAL admission Admitter — the same
// fixture, the same workspace binding, and the same shipped router the T1 Events-API tests drive.
//
// THE CLAIM UNDER TEST is SLK-001's hard half, and it is the one thing a protocol test against a fake sink
// cannot establish: A TRANSPORT CHANGE DOES NOT CHANGE THE CORRELATION IDENTITY. The same Slack event_id
// arriving over the HTTP callback and over a Socket Mode envelope must produce exactly ONE run — not two,
// and not an idempotency CONFLICT (which is what a transport that hashed its request differently would
// produce, and which would look like a refusal rather than a replay).
//
// Everything protocol-shaped — acknowledge-before-work, the overlap on `warning`, zero event loss,
// link_disabled — is proved WITHOUT a database in
// apps/control-plane/internal/extensions/slack_socket_test.go, deliberately: a guarantee hidden behind a
// Postgres skip is a guarantee nobody runs.
//
// HONEST CEILING, same as its siblings: the peer is FAKE. There is no socket to slack.com in this file, and
// `slack` stays PREVIEW — the external receipt is §6 leg 1, and the Socket Mode half of it is the
// credential-gated live leg in tests/live/slack.

// fakeSocketModePeer is the Socket Mode half of the fixture's stand-in for slack.com/api: the
// apps.connections.open Web API method and the WebSocket its URL points at.
//
// CONTRACT https://docs.slack.dev/apis/events-api/using-socket-mode/ (checked 2026-07-26). Every frame it
// writes is a literal from that page and every acknowledgement it reads is decoded as bare JSON — it never
// calls slack.SocketAck, so a drift in our encoder cannot drag the fixture along with it.
type fakeSocketModePeer struct {
	baseURL func() string

	mu      sync.Mutex
	opens   int
	auth    []string
	acks    []string
	sockets chan *websocket.Conn
}

func newFakeSocketModePeer(baseURL func() string) *fakeSocketModePeer {
	return &fakeSocketModePeer{baseURL: baseURL, sockets: make(chan *websocket.Conn, slack.SocketMaxConnections)}
}

func (p *fakeSocketModePeer) open(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.opens++
	n := p.opens
	p.auth = append(p.auth, r.Header.Get("Authorization"))
	p.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"url":%q}`,
		"ws"+strings.TrimPrefix(p.baseURL(), "http")+fmt.Sprintf("/link/?ticket=ticket-%d", n))
}

func (p *fakeSocketModePeer) link(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"hello","connection_info":{"app_id":"A1234"},"num_connections":1,"debug_info":{"host":"applink-1","started":"2020-10-11 12:12:12.120","build_number":54,"approximate_connection_time":3600}}`))
	p.sockets <- conn
	for {
		_, data, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		var ack map[string]any
		if err := json.Unmarshal(data, &ack); err != nil {
			continue
		}
		if id, _ := ack["envelope_id"].(string); id != "" {
			p.mu.Lock()
			p.acks = append(p.acks, id)
			p.mu.Unlock()
		}
	}
}

func (p *fakeSocketModePeer) acked(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, got := range p.acks {
		if got == id {
			return true
		}
	}
	return false
}

// slackAPIMux serves the whole local Slack API surface: chat.* on the recording fake, plus the Socket Mode
// pair. Real Slack has them all under one base, so the fixture does too.
func (f *slackFixture) slackAPIMux() http.Handler {
	f.socket = newFakeSocketModePeer(func() string { return f.apiBase })
	mux := http.NewServeMux()
	mux.Handle("/", f.slack)
	mux.HandleFunc("POST /apps.connections.open", f.socket.open)
	mux.HandleFunc("/link/", f.socket.link)
	return mux
}

// startSocketMode runs the PRODUCTION connect loop against the fixture's peer and returns the accepted
// socket. Nothing is stubbed on our side: extensions.SlackAdmitter is the sink, so an envelope travels the
// same ResolveConnection → MapEvent → Admit path the HTTP route travels.
func (f *slackFixture) startSocketMode(t *testing.T) *websocket.Conn {
	t.Helper()
	loop := f.bridge.SocketMode(f.team)
	if loop == nil {
		t.Fatal("SocketMode returned nil for a configured workspace")
	}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { finished <- loop.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("the Socket Mode loop did not return within 5s of its context being cancelled")
		}
	})
	select {
	case conn := <-f.socket.sockets:
		return conn
	case <-time.After(10 * time.Second):
		t.Fatal("the Socket Mode loop never opened a WebSocket")
		return nil
	}
}

// deliverOverSocket writes one events_api envelope wrapping the SAME body the HTTP transport carries, and
// waits for the loop to acknowledge it.
func (f *slackFixture) deliverOverSocket(t *testing.T, conn *websocket.Conn, envelopeID string, body []byte) {
	t.Helper()
	frame := fmt.Sprintf(`{"payload":%s,"envelope_id":%q,"type":"events_api","accepts_response_payload":false}`,
		body, envelopeID)
	if err := conn.Write(context.Background(), websocket.MessageText, []byte(frame)); err != nil {
		t.Fatalf("write envelope %s: %v", envelopeID, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if f.socket.acked(envelopeID) {
			// The acknowledgement is written BEFORE the admission (D4), so it is not proof the work finished.
			// The DB assertions below poll for the effect rather than assuming it has landed.
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("envelope %s was never acknowledged", envelopeID)
}

// waitForRuns polls until the tenant holds `want` runs, then holds a beat and re-reads — so a test that
// expects NO new run cannot pass merely by looking too early.
func (f *slackFixture) waitForRuns(t *testing.T, want int, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if f.runCount(t) == want {
			time.Sleep(250 * time.Millisecond)
			if got := f.runCount(t); got != want {
				t.Fatalf("%s: run count settled at %d then moved to %d, want %d", what, want, got, want)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s: %d runs, want %d", what, f.runCount(t), want)
}

// TestSlackSocketModeAndHTTPShareOneCanonicalIdentity is the E19 T3 acceptance condition for SLK-001: the
// SAME event delivered over BOTH transports produces ONE run, in BOTH directions.
//
// It runs both orders on purpose. Socket-first proves the Socket Mode leg is a genuine admission (it births
// the run, pinned to the connection's revision, reserved under the connection's principal — none of which the
// payload chose). HTTP-first proves the socket leg REPLAYS onto an existing reservation rather than opening a
// second one. A transport that composed its request differently — a retry hint folded into the input, a
// different route constant, a different idempotency key — would fail one of the two.
func TestSlackSocketModeAndHTTPShareOneCanonicalIdentity(t *testing.T) {
	f := newSlackFixture(t)
	conn := f.startSocketMode(t)

	// ---- direction 1: Socket Mode first, then the identical HTTP callback ----
	socketFirst := f.event("EvSock1", "app_mention", "Umapped", "C30", "1700000030.000100", "")
	f.deliverOverSocket(t, conn, "env-sock-1", socketFirst)
	f.waitForRuns(t, 1, "a Socket Mode envelope must birth a real run")

	// The run is the connection's, not the payload's — the same server-side bindings the HTTP leg asserts.
	var revision, principal, route, key string
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT COALESCE(r.agent_revision_id,''), COALESCE(i.principal_id,''), i.route, i.idempotency_key
		   FROM runs r
		   JOIN idempotency_records i
		     ON i.organization_id = r.organization_id AND i.project_id = r.project_id
		  WHERE r.organization_id=$1 AND r.project_id=$2`, f.org, f.project).
		Scan(&revision, &principal, &route, &key); err != nil {
		t.Fatalf("read the socket-born run: %v", err)
	}
	if revision != f.revision || principal != f.principal {
		t.Fatalf("socket-born run = revision %q principal %q, want the connection's %q/%q — Socket Mode must not widen what a transport may choose",
			revision, principal, f.revision, f.principal)
	}
	// THE IDENTITY ITSELF: the same route constant and the same team+event key the HTTP leg reserves under.
	if route != "/v1/slack/events" || key != f.team+":EvSock1" {
		t.Fatalf("socket reservation = (%q,%q), want (/v1/slack/events, %s:EvSock1) — a transport swap must not move the idempotency namespace or the key",
			route, key, f.team)
	}

	// Now the SAME event over HTTP. It must REPLAY onto that reservation: no second run, no conflict.
	resp := f.deliver(t, socketFirst, time.Now(), "", "")
	if resp.StatusCode/100 != 2 {
		t.Fatalf("the HTTP callback for an event already admitted over Socket Mode = %d, want a 2xx ack (a replay, not a refusal)", resp.StatusCode)
	}
	resp.Body.Close()
	f.waitForRuns(t, 1, "the same event over a second transport must not birth a second run")
	assertOneCleanReservation(t, f, 1, "socket then HTTP")

	// ---- direction 2: HTTP first, then the identical Socket Mode envelope ----
	f.terminateRuns(t) // one active root run per session; the first conversation has to finish
	httpFirst := f.event("EvHTTP1", "app_mention", "Umapped", "C31", "1700000031.000100", "")
	resp = f.deliver(t, httpFirst, time.Now(), "", "")
	if resp.StatusCode/100 != 2 {
		t.Fatalf("the HTTP callback = %d, want a 2xx ack", resp.StatusCode)
	}
	resp.Body.Close()
	f.waitForRuns(t, 2, "the second event's HTTP delivery")

	f.deliverOverSocket(t, conn, "env-http-1", httpFirst)
	f.waitForRuns(t, 2, "the same event over Socket Mode must REPLAY onto the HTTP reservation, not open a second run")
	assertOneCleanReservation(t, f, 2, "HTTP then socket")

	// The app-level token was presented the one way Socket Mode authenticates (D7): at connect, as a Bearer.
	f.socket.mu.Lock()
	defer f.socket.mu.Unlock()
	if len(f.socket.auth) == 0 {
		t.Fatal("apps.connections.open was never called")
	}
	if f.socket.auth[0] != "Bearer "+string(f.appToken) {
		t.Fatalf("apps.connections.open Authorization = %q, want the app-level token resolved from app_token_ref", f.socket.auth[0])
	}
	// And the fixture's own tenant/secret bridge is what supplied it — never an inline value on the row.
	var inline int
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(*) FROM information_schema.columns
		  WHERE table_name = 'slack_connections' AND column_name = 'app_token'`).Scan(&inline); err != nil {
		t.Fatalf("scan for an app_token value column: %v", err)
	}
	if inline != 0 {
		t.Fatal("slack_connections has a raw app_token column; the app-level token must be a secret_ref handle only")
	}
}

// assertOneCleanReservation checks that `want` idempotency records exist and none of them recorded a
// CONFLICT. A conflict is the specific failure a transport-dependent request hash produces: the same key
// presented with a different request, refused instead of replayed.
func assertOneCleanReservation(t *testing.T, f *slackFixture, want int, what string) {
	t.Helper()
	var records, conflicts int
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(*), count(*) FILTER (WHERE status = 'conflict')
		   FROM idempotency_records WHERE organization_id=$1 AND project_id=$2`,
		f.org, f.project).Scan(&records, &conflicts); err != nil {
		t.Fatalf("read reservations: %v", err)
	}
	if records != want {
		t.Fatalf("%s: %d idempotency records, want %d — one per SOURCE EVENT, regardless of how many transports delivered it", what, records, want)
	}
	if conflicts != 0 {
		t.Fatalf("%s: %d reservation(s) recorded a CONFLICT — the two transports composed DIFFERENT requests for one event, so the second was refused rather than replayed. The admitted request must be a pure function of the event.", what, conflicts)
	}
}

// TestSlackSocketModeSelfEventBirthsNothing is SLK-008 on the Socket Mode transport: the loop guard lives in
// the adapter's MapEvent, so it applies to every transport — but "it should" is not evidence, and a bot event
// arriving over a WebSocket that DID open a run would be an infinite loop nobody notices until it is running.
func TestSlackSocketModeSelfEventBirthsNothing(t *testing.T) {
	f := newSlackFixture(t)
	conn := f.startSocketMode(t)

	f.deliverOverSocket(t, conn, "env-self", f.event("EvSelf", "message", f.botUser, "C32", "1700000032.000100", ""))
	// Give the (acknowledged-first) admission time to have happened, then assert it did not.
	time.Sleep(500 * time.Millisecond)
	if n := f.runCount(t); n != 0 {
		t.Fatalf("the app's own message over Socket Mode birthed %d runs, want 0 — the self-loop guard is transport-independent", n)
	}
	if !f.socket.acked("env-self") {
		t.Fatal("the ignored event was never acknowledged; an unacknowledged envelope is one Slack goes on treating as undelivered")
	}
}

// TestSlackSocketModeIsNotMountedWithoutAWorkspace: SocketMode(\"\") is how a deployment that has not
// configured Socket Mode expresses it. It must produce no loop at all rather than a loop that fails.
func TestSlackSocketModeIsNotMountedWithoutAWorkspace(t *testing.T) {
	f := newSlackFixture(t)
	if loop := f.bridge.SocketMode(""); loop != nil {
		t.Fatal("SocketMode(\"\") returned a loop; an unconfigured deployment must mount nothing")
	}
	if _, ok := any(f.bridge).(interface {
		SocketMode(string) *extensions.SlackSocket
	}); !ok {
		t.Fatal("the bridge no longer exposes SocketMode")
	}
}

// ---- E20 T2: the agent panel is a THIRD ENTRANCE into the SAME admission bridge ------------------------

// TestSlackPanelDMEntersTheSameAdmissionBridge extends E19 T3's transport-invariance to the surface E20 adds.
// The panel is not a third TRANSPORT — it is a third ENTRANCE: a conversation that arrives as message.im
// rather than as app_mention, over either of the two transports that already exist. The claim is that it
// changes nothing about identity.
//
// It asserts what a new entrance could plausibly have broken, in order:
//
//	the panel DM births a real run through the UNCHANGED Admit — same route constant, same team+event_id key;
//	under the CONNECTION's principal and pinned revision, neither of which the DM chose;
//	and the SAME event_id arriving over the OTHER transport REPLAYS onto that one reservation — no second run,
//	no idempotency conflict (which is what a surface that composed its request differently would produce).
//
// Together with the app_mention pair above, that is the three-entrance guarantee: {HTTP mention, Socket Mode
// mention, panel DM} all land in ONE Admit, and one source event yields one run whichever way it arrives.
func TestSlackPanelDMEntersTheSameAdmissionBridge(t *testing.T) {
	f := newSlackFixture(t)
	conn := f.startSocketMode(t)

	// The panel's DM over HTTP first.
	dm := f.dmEvent("EvPanelDM", "Uoutsider", "D024BE91L", "1700000060.000100", "", "what is left on the release?")
	resp := f.deliver(t, dm, time.Now(), "", "")
	if resp.StatusCode/100 != 2 {
		t.Fatalf("the panel DM over HTTP = %d, want a 2xx ack", resp.StatusCode)
	}
	resp.Body.Close()
	f.waitForRuns(t, 1, "a panel DM must birth a real run through the unchanged admission bridge")

	var revision, principal, route, key string
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT COALESCE(r.agent_revision_id,''), COALESCE(i.principal_id,''), i.route, i.idempotency_key
		   FROM runs r
		   JOIN idempotency_records i
		     ON i.organization_id = r.organization_id AND i.project_id = r.project_id
		  WHERE r.organization_id=$1 AND r.project_id=$2`, f.org, f.project).
		Scan(&revision, &principal, &route, &key); err != nil {
		t.Fatalf("read the panel-born run: %v", err)
	}
	if revision != f.revision || principal != f.principal {
		t.Fatalf("panel-born run = revision %q principal %q, want the connection's %q/%q — a new surface must not widen what a payload may choose",
			revision, principal, f.revision, f.principal)
	}
	if route != "/v1/slack/events" || key != f.team+":EvPanelDM" {
		t.Fatalf("panel reservation = (%q,%q), want (/v1/slack/events, %s:EvPanelDM) — the panel must reserve in the SAME idempotency namespace, under the SAME key",
			route, key, f.team)
	}

	// The identical event over Socket Mode: a REPLAY, not a second run and not a conflict.
	f.deliverOverSocket(t, conn, "env-panel-dm", dm)
	f.waitForRuns(t, 1, "the same panel DM over the other transport must replay onto the one reservation")
	assertOneCleanReservation(t, f, 1, "panel DM over both transports")
}

// TestSlackSocketModePanelSurfaceEventsBirthNothing is the Socket Mode half of the no-run guarantee. The
// mapper is what refuses, so both transports inherit it — but "it should" is not evidence, and this is the
// transport the owner actually runs (Socket Mode needs no public URL). The envelope must still be
// ACKNOWLEDGED: an unacknowledged envelope is one Slack goes on treating as undelivered.
func TestSlackSocketModePanelSurfaceEventsBirthNothing(t *testing.T) {
	f := newSlackFixture(t)
	conn := f.startSocketMode(t)

	f.deliverOverSocket(t, conn, "env-home", f.panelEvent("EvHome", map[string]any{
		"type": "app_home_opened", "user": "U9", "channel": "D42", "event_ts": "1515449522.000016", "tab": "messages"}))
	f.deliverOverSocket(t, conn, "env-ctx", f.panelEvent("EvCtx", map[string]any{
		"type": "app_context_changed", "context": map[string]any{"entities": []any{
			map[string]any{"type": "slack#/types/channel_id", "value": "C01234ABDCE", "team_id": "T0ABCDE6543"}}}}))

	// The acknowledgement precedes the work (D4), so waiting on it proves nothing about admission: give the
	// admission that never happened time to have happened, then assert it did not.
	time.Sleep(500 * time.Millisecond)
	if n := f.runCount(t); n != 0 {
		t.Fatalf("panel surface events over Socket Mode birthed %d runs, want 0", n)
	}
	for _, id := range []string{"env-home", "env-ctx"} {
		if !f.socket.acked(id) {
			t.Fatalf("panel envelope %s was never acknowledged; Slack goes on treating it as undelivered", id)
		}
	}
}
