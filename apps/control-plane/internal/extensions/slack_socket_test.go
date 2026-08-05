package extensions

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/palgroup/palai/adapters/integrations/slack"
	"github.com/palgroup/palai/apps/control-plane/api"
)

// The Socket Mode connect loop's proof (E19 T3, plan §3.5 rows D4/D5/D6/D7), against a fake WSS peer built
// from the PUBLISHED protocol — https://docs.slack.dev/apis/events-api/using-socket-mode/, checked
// 2026-07-26. It is deliberately UNTAGGED: the guarantees under test are protocol guarantees, and hiding
// them behind a Postgres skip is this family's recurring failure (six findings so far). The transport-
// invariance proof that DOES need a database is the component sibling in slack_socket_component_test.go.
//
// THE FAKE IS WRITTEN LONGHAND, and that is the point (E17 T10: a fixture built from our own writer can only
// confirm our own writer; E19 T1's fake signed longhand for the same reason). Every frame this peer sends is
// a literal from the page, and every acknowledgement it reads is decoded with encoding/json into a bare map —
// slack.SocketAck is never called here. If our encoder drifts from the documented shape, this peer notices.

// ---------------------------------------------------------------------------------------------------
// the fake peer
// ---------------------------------------------------------------------------------------------------

// fakeSocketPeer serves both halves of the Socket Mode handshake: the apps.connections.open Web API call and
// the WebSocket the returned URL points at. Each accepted socket is handed to the test over `sockets`, so the
// test drives the frame ORDER itself — which is what the overlap proof needs.
type fakeSocketPeer struct {
	srv     *httptest.Server
	sockets chan *fakeSocket

	mu    sync.Mutex
	opens int      // apps.connections.open calls
	auth  []string // the Authorization header each one presented
	acks  []string // every envelope_id acknowledged, in arrival order, across all sockets
	// withPayload records the envelope ids whose acknowledgement carried a `payload` key (D6).
	withPayload map[string]bool
}

// fakeSocket is one accepted WebSocket from the peer's side.
type fakeSocket struct {
	conn   *websocket.Conn
	ticket string
	done   chan struct{} // closed when the peer's read loop ends (the socket is gone)
}

func newFakeSocketPeer(t *testing.T) *fakeSocketPeer {
	t.Helper()
	p := &fakeSocketPeer{
		sockets:     make(chan *fakeSocket, slack.SocketMaxConnections),
		withPayload: map[string]bool{},
	}
	mux := http.NewServeMux()

	// CONTRACT: POST https://slack.com/api/apps.connections.open with `Authorization: Bearer xapp-***`
	// answers {"ok":true,"url":"wss://wss.slack.com/link/?ticket=1234-5678"}. The URL is generated per call
	// and is NOT durable, which is exactly why a reconnect has to open a new one rather than reuse the last.
	mux.HandleFunc("POST /apps.connections.open", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.opens++
		n := p.opens
		p.auth = append(p.auth, r.Header.Get("Authorization"))
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"url":%q}`, "ws"+strings.TrimPrefix(p.srv.URL, "http")+fmt.Sprintf("/link/?ticket=ticket-%d", n))
	})

	mux.HandleFunc("/link/", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		s := &fakeSocket{conn: conn, ticket: r.URL.Query().Get("ticket"), done: make(chan struct{})}
		defer close(s.done)

		// The first message on every connection, verbatim from the page.
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"hello","connection_info":{"app_id":"A1234"},"num_connections":1,"debug_info":{"host":"applink-1","started":"2020-10-11 12:12:12.120","build_number":54,"approximate_connection_time":3600}}`))
		p.sockets <- s

		for {
			_, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			// LONGHAND: the acknowledgement is decoded as raw JSON, never through slack.SocketAck.
			var ack map[string]any
			if err := json.Unmarshal(data, &ack); err != nil {
				continue
			}
			id, _ := ack["envelope_id"].(string)
			if id == "" {
				continue
			}
			_, hasPayload := ack["payload"]
			p.mu.Lock()
			p.acks = append(p.acks, id)
			p.withPayload[id] = hasPayload
			p.mu.Unlock()
		}
	})

	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

// accept waits for the next socket the client opens.
func (p *fakeSocketPeer) accept(t *testing.T, why string) *fakeSocket {
	t.Helper()
	select {
	case s := <-p.sockets:
		return s
	case <-time.After(5 * time.Second):
		t.Fatalf("no WebSocket was opened within 5s (%s)", why)
		return nil
	}
}

// noSocket asserts the client does NOT open another connection within d.
func (p *fakeSocketPeer) noSocket(t *testing.T, d time.Duration, why string) {
	t.Helper()
	select {
	case <-p.sockets:
		t.Fatalf("the client opened another WebSocket (%s)", why)
	case <-time.After(d):
	}
}

func (p *fakeSocketPeer) acked() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.acks...)
}

func (p *fakeSocketPeer) openCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.opens
}

// waitForAcks blocks until every id has been acknowledged, or fails naming what is missing.
func (p *fakeSocketPeer) waitForAcks(t *testing.T, ids ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got := map[string]int{}
		for _, id := range p.acked() {
			got[id]++
		}
		missing := []string(nil)
		for _, want := range ids {
			if got[want] == 0 {
				missing = append(missing, want)
			}
		}
		if len(missing) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("acknowledged %v, want all of %v", p.acked(), ids)
}

// sendEnvelope writes one envelope in the documented shape.
func (s *fakeSocket) sendEnvelope(t *testing.T, typ, envelopeID string, accepts bool, payload string) {
	t.Helper()
	frame := fmt.Sprintf(`{"payload":%s,"envelope_id":%q,"type":%q,"accepts_response_payload":%t}`,
		payload, envelopeID, typ, accepts)
	if err := s.conn.Write(context.Background(), websocket.MessageText, []byte(frame)); err != nil {
		t.Fatalf("send envelope %s: %v", envelopeID, err)
	}
}

// sendDisconnect writes the documented disconnect frame.
func (s *fakeSocket) sendDisconnect(t *testing.T, reason string) {
	t.Helper()
	frame := fmt.Sprintf(`{"type":"disconnect","reason":%q,"debug_info":{"host":"wss-111.slack.com"}}`, reason)
	if err := s.conn.Write(context.Background(), websocket.MessageText, []byte(frame)); err != nil {
		t.Fatalf("send disconnect %s: %v", reason, err)
	}
}

func (s *fakeSocket) close() { _ = s.conn.Close(websocket.StatusNormalClosure, "scheduled restart") }

// ---------------------------------------------------------------------------------------------------
// the fake admission sink
// ---------------------------------------------------------------------------------------------------

// fakeSocketSink stands in for the T1/T2 bridge so the PROTOCOL guarantees run with no database. The
// transport-invariance guarantee — the one that says a Socket Mode envelope and an HTTP callback produce the
// SAME canonical identity — cannot be proven against a fake and is not attempted here: it lives in the
// component sibling, driving the real admitter.
type fakeSocketSink struct {
	conn api.SlackConnectionRef

	mu       sync.Mutex
	admitted []string
	decided  []string
	// block, when non-nil, holds every Admit until it is closed — the D4 ordering proof.
	block chan struct{}
	// entered is signalled once per Admit, as it starts.
	entered chan string
}

func (f *fakeSocketSink) ResolveConnection(context.Context, string, string) (api.SlackConnectionRef, bool, error) {
	return f.conn, true, nil
}

func (f *fakeSocketSink) Admit(ctx context.Context, _ api.SlackConnectionRef, ev slack.Event) (api.SlackAdmitOutcome, error) {
	if f.entered != nil {
		select {
		case f.entered <- ev.SourceEventID:
		default:
		}
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return api.SlackAdmitOutcome{}, ctx.Err()
		}
	}
	f.mu.Lock()
	f.admitted = append(f.admitted, ev.SourceEventID)
	f.mu.Unlock()
	return api.SlackAdmitOutcome{ResponseID: "resp_" + ev.SourceEventID}, nil
}

func (f *fakeSocketSink) Decide(_ context.Context, _ api.SlackConnectionRef, intent slack.ApprovalIntent) (api.SlackDecisionOutcome, error) {
	f.mu.Lock()
	f.decided = append(f.decided, intent.RequestHash)
	f.mu.Unlock()
	return api.SlackDecisionOutcome{Decision: intent.Decision}, nil
}

func (f *fakeSocketSink) admittedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.admitted...)
}

// ---------------------------------------------------------------------------------------------------
// the loop under test
// ---------------------------------------------------------------------------------------------------

const fakeAppToken = "xapp-1-A1234-0000-thisisafaketokenandmustnevershowupinalog"

// newTestSocket builds the loop against the fake peer, plus the captured log the leak guard reads.
func newTestSocket(t *testing.T, p *fakeSocketPeer, sink *fakeSocketSink) (*SlackSocket, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var lines []string
	s := &SlackSocket{
		sink:    sink,
		secrets: func(ref string) ([]byte, error) { return []byte(fakeAppToken), nil },
		doer:    p.srv.Client(),
		apiBase: p.srv.URL,
		teamID:  sink.conn.TeamID,
		logf: func(format string, args ...any) {
			mu.Lock()
			lines = append(lines, fmt.Sprintf(format, args...))
			mu.Unlock()
			t.Logf(format, args...)
		},
	}
	return s, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), lines...)
	}
}

func testConnRef() api.SlackConnectionRef {
	return api.SlackConnectionRef{
		ID: "slkc_test", Project: "proj_test",
		TeamID: "T0SOCKET", BotUserID: "Ubot", AppTokenRef: "slack/app", RunPolicy: []byte(`{}`),
	}
}

// eventPayload is an Events API event_callback body — byte-identical in shape to what the HTTP transport
// receives, which is the whole reason a transport swap cannot move the identity.
func eventPayload(team, eventID string) string {
	return fmt.Sprintf(`{"type":"event_callback","team_id":%q,"event_id":%q,"event":{"type":"app_mention","user":"U1","channel":"C1","ts":"1.1","text":"hi"}}`,
		team, eventID)
}

// runSocket starts the loop and returns a stop func plus the channel carrying Run's result. The cleanup does
// not merely cancel: it WAITS for Run to return, so a loop that ignored its context cannot go on logging into
// a finished test (which is how a leaked connect loop would show up in production too).
func runSocket(t *testing.T, s *SlackSocket) (stop func(), result chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	result = make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		result <- s.Run(ctx)
		close(finished)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("the connect loop did not return within 5s of its context being cancelled")
		}
	})
	return cancel, result
}

// ---------------------------------------------------------------------------------------------------
// D4 — the acknowledgement goes out BEFORE the work
// ---------------------------------------------------------------------------------------------------

// TestSlackSocketAcknowledgesBeforeDoingTheWork is plan §3.5 row D4. No ack budget is published for Socket
// Mode (the three-second figure is documented for the Events API HTTP callback and for interactivity, not
// here), so the loop must not bake a number in — instead it acknowledges FIRST and works after, which
// satisfies any budget that might exist.
//
// The assertion is an ORDERING one, not a timing one: the admission is held open, and the acknowledgement
// must already have arrived while it is still held. A loop that acks after the work cannot pass this no
// matter how fast it is.
func TestSlackSocketAcknowledgesBeforeDoingTheWork(t *testing.T) {
	peer := newFakeSocketPeer(t)
	sink := &fakeSocketSink{conn: testConnRef(), block: make(chan struct{}), entered: make(chan string, 4)}
	s, _ := newTestSocket(t, peer, sink)
	runSocket(t, s)

	sock := peer.accept(t, "the initial connection")
	sock.sendEnvelope(t, slack.SocketEventsAPI, "env-slow", false, eventPayload("T0SOCKET", "Ev-slow"))

	select {
	case <-sink.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the admission never started")
	}
	// The work is now held open. The acknowledgement must ALREADY be on the wire.
	peer.waitForAcks(t, "env-slow")
	if got := sink.admittedIDs(); len(got) != 0 {
		t.Fatalf("the admission completed (%v) before the ordering could be observed; the test lost its grip, not the loop", got)
	}
	close(sink.block)
}

// TestSlackSocketAckShapeMatchesTheDocumentedEnvelope pins what goes back on the wire (D6): exactly one
// acknowledgement per envelope, carrying that envelope's id, and no `payload` key — because this loop sends
// no response payloads.
//
// CEILING, stated where a reader meets it: nothing here ever attaches a response payload, so
// accepts_response_payload:true is answered the same way as false. The RULE (a payload may ride the ack only
// when the envelope accepts one) is enforced in slack.SocketAck and proved in its own unit test; what this
// asserts is that the loop cannot accidentally emit one.
func TestSlackSocketAckShapeMatchesTheDocumentedEnvelope(t *testing.T) {
	peer := newFakeSocketPeer(t)
	sink := &fakeSocketSink{conn: testConnRef()}
	s, _ := newTestSocket(t, peer, sink)
	runSocket(t, s)

	sock := peer.accept(t, "the initial connection")
	sock.sendEnvelope(t, slack.SocketEventsAPI, "env-accepts", true, eventPayload("T0SOCKET", "Ev-a"))
	sock.sendEnvelope(t, slack.SocketEventsAPI, "env-refuses", false, eventPayload("T0SOCKET", "Ev-b"))
	// A slash_commands envelope is a documented type we act on for nothing — it must still be acknowledged,
	// or Slack goes on treating it as unanswered.
	sock.sendEnvelope(t, slack.SocketSlashCommands, "env-slash", false, `{"command":"/palai"}`)
	peer.waitForAcks(t, "env-accepts", "env-refuses", "env-slash")

	peer.mu.Lock()
	defer peer.mu.Unlock()
	for id, had := range peer.withPayload {
		if had {
			t.Errorf("the acknowledgement for %s carried a payload key; this loop sends no response payloads", id)
		}
	}
	seen := map[string]int{}
	for _, id := range peer.acks {
		seen[id]++
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("envelope %s was acknowledged %d times, want exactly 1", id, n)
		}
	}
}

// ---------------------------------------------------------------------------------------------------
// D5 — the overlap, and ZERO event loss
// ---------------------------------------------------------------------------------------------------

// TestSlackSocketOverlapsOnWarningAndLosesNoEvents is plan §3.5 row D5 and the single most valuable test in
// this task.
//
// CONTRACT: "You may receive a warning about 10 seconds before the disconnect" and "If you'd like to handle
// a scheduled connection restart gracefully, you can generate an additional connection before the restart
// occurs" — with "Socket Mode allows your app to maintain up to 10 open WebSocket connections at the same
// time" making that legal.
//
// A naive close-then-reconnect DROPS every event Slack delivers in that ten-second window. The sequence
// below is built so that it cannot pass by accident:
//
//  1. socket #1 delivers Ev1, which is acknowledged;
//  2. socket #1 carries the `warning`;
//  3. the test WAITS for socket #2 — proof the client reacted to the warning at all;
//  4. socket #1 (still open, as real Slack leaves it) delivers Ev2 and Ev3 — the window's traffic;
//  5. socket #2 delivers Ev4;
//  6. socket #1 is closed by the peer, as Slack would.
//
// A client that closed at step 2 has no socket to receive Ev2/Ev3 on, so the writes at step 4 land nowhere
// and the assertion fails naming them.
func TestSlackSocketOverlapsOnWarningAndLosesNoEvents(t *testing.T) {
	peer := newFakeSocketPeer(t)
	sink := &fakeSocketSink{conn: testConnRef()}
	s, _ := newTestSocket(t, peer, sink)
	runSocket(t, s)

	first := peer.accept(t, "the initial connection")
	first.sendEnvelope(t, slack.SocketEventsAPI, "env-1", false, eventPayload("T0SOCKET", "Ev1"))
	peer.waitForAcks(t, "env-1")

	// The ten-second notice. The socket STAYS OPEN — that is the whole point of the warning.
	first.sendDisconnect(t, slack.DisconnectWarning)

	second := peer.accept(t, "the successor the `warning` asks for — a client that closed first cannot receive the window's events")
	if second.ticket == first.ticket {
		t.Fatalf("the successor reused ticket %q; the apps.connections.open URL is generated per call and is not durable", first.ticket)
	}

	// The window's traffic, on the OLD socket, exactly as Slack keeps delivering it.
	first.sendEnvelope(t, slack.SocketEventsAPI, "env-2", false, eventPayload("T0SOCKET", "Ev2"))
	first.sendEnvelope(t, slack.SocketEventsAPI, "env-3", false, eventPayload("T0SOCKET", "Ev3"))
	// ...and the successor is already carrying traffic of its own.
	second.sendEnvelope(t, slack.SocketEventsAPI, "env-4", false, eventPayload("T0SOCKET", "Ev4"))

	peer.waitForAcks(t, "env-1", "env-2", "env-3", "env-4")

	// Now Slack closes the old one. The loop must NOT treat that as a failure to reconnect from — it already
	// has a successor.
	first.close()
	<-first.done
	peer.noSocket(t, 500*time.Millisecond, "the successor is already live; a third connection means the drain was read as a disconnect")

	// Every event reached the admission exactly once.
	want := []string{"Ev1", "Ev2", "Ev3", "Ev4"}
	got := map[string]int{}
	for _, id := range sink.admittedIDs() {
		got[id]++
	}
	for _, id := range want {
		if got[id] != 1 {
			t.Fatalf("event %s was admitted %d times, want exactly 1 (admitted: %v) — ZERO EVENT LOSS across a warning-driven reconnect is the guarantee", id, got[id], sink.admittedIDs())
		}
	}
	if n := peer.openCount(); n != 2 {
		t.Fatalf("apps.connections.open was called %d times, want 2 (the initial connection and the overlapping successor)", n)
	}
	if n := peer.openCount(); n > slack.SocketMaxConnections {
		t.Fatalf("opened %d connections, above Slack's documented cap of %d", n, slack.SocketMaxConnections)
	}
}

// TestSlackSocketReconnectsWhenTheSocketDiesWithoutWarning covers the unannounced drop: no warning frame, the
// socket simply ends. There is nothing to overlap with, so the loop reconnects — and no event is in flight to
// lose, which is what makes this case the easy one.
func TestSlackSocketReconnectsWhenTheSocketDiesWithoutWarning(t *testing.T) {
	peer := newFakeSocketPeer(t)
	sink := &fakeSocketSink{conn: testConnRef()}
	s, _ := newTestSocket(t, peer, sink)
	runSocket(t, s)

	first := peer.accept(t, "the initial connection")
	first.close()
	<-first.done

	second := peer.accept(t, "the reconnect after an unannounced drop")
	second.sendEnvelope(t, slack.SocketEventsAPI, "env-after", false, eventPayload("T0SOCKET", "Ev-after"))
	peer.waitForAcks(t, "env-after")
}

// ---------------------------------------------------------------------------------------------------
// D5 — link_disabled is PERMANENT
// ---------------------------------------------------------------------------------------------------

// TestSlackSocketStopsPermanentlyOnLinkDisabled: `link_disabled` is Socket Mode switched off in app
// settings. Reconnecting would be a hot loop against a door closed on purpose, so the loop stops and says so
// loudly enough for an operator to find it. Run returns nil so the supervisor does not restart it either —
// coordinator.Supervise treats a nil return as "finished, do not restart".
func TestSlackSocketStopsPermanentlyOnLinkDisabled(t *testing.T) {
	peer := newFakeSocketPeer(t)
	sink := &fakeSocketSink{conn: testConnRef()}
	s, logs := newTestSocket(t, peer, sink)
	_, result := runSocket(t, s)

	sock := peer.accept(t, "the initial connection")
	sock.sendDisconnect(t, slack.DisconnectLinkDisabled)

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Run returned %v on link_disabled, want nil — a non-nil error makes the supervisor restart the loop into a door that is shut on purpose", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s of link_disabled")
	}
	peer.noSocket(t, 500*time.Millisecond, "link_disabled is permanent")

	alerted := false
	for _, line := range logs() {
		if strings.Contains(line, slack.DisconnectLinkDisabled) {
			alerted = true
		}
	}
	if !alerted {
		t.Fatalf("nothing in the log names link_disabled; an operator has to be able to find out why Slack went quiet. logs=%v", logs())
	}
}

// ---------------------------------------------------------------------------------------------------
// SIGTERM — graceful drain
// ---------------------------------------------------------------------------------------------------

// TestSlackSocketDrainsGracefully: on SIGTERM the control plane cancels the loop's context. An envelope
// already being worked must finish — the acknowledgement is out, so abandoning it would mean an event Slack
// considers delivered and we never admitted.
func TestSlackSocketDrainsGracefully(t *testing.T) {
	peer := newFakeSocketPeer(t)
	sink := &fakeSocketSink{conn: testConnRef(), block: make(chan struct{}), entered: make(chan string, 4)}
	s, _ := newTestSocket(t, peer, sink)
	stop, result := runSocket(t, s)

	sock := peer.accept(t, "the initial connection")
	sock.sendEnvelope(t, slack.SocketEventsAPI, "env-inflight", false, eventPayload("T0SOCKET", "Ev-inflight"))
	select {
	case <-sink.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the admission never started")
	}

	stop() // SIGTERM lands while the admission is in flight
	select {
	case err := <-result:
		t.Fatalf("Run returned %v while an admission was still in flight; the drain abandoned committed work", err)
	case <-time.After(200 * time.Millisecond):
	}

	close(sink.block) // the in-flight admission completes
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Run returned %v after a clean drain, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s of the drain")
	}
	if got := sink.admittedIDs(); len(got) != 1 || got[0] != "Ev-inflight" {
		t.Fatalf("admitted %v, want the in-flight event to have completed during the drain", got)
	}
}

// ---------------------------------------------------------------------------------------------------
// credentials
// ---------------------------------------------------------------------------------------------------

// TestSlackSocketNeverLogsTheTokenOrTheTicket. The app-level token rides an Authorization header and the WSS
// URL carries a `?ticket=` that is credential-equivalent for the lifetime of the dial — either one in a log
// line is a leak, and a dial URL is the easiest place in this task to put one by accident.
func TestSlackSocketNeverLogsTheTokenOrTheTicket(t *testing.T) {
	peer := newFakeSocketPeer(t)
	sink := &fakeSocketSink{conn: testConnRef()}
	s, logs := newTestSocket(t, peer, sink)
	runSocket(t, s)

	sock := peer.accept(t, "the initial connection")
	sock.sendEnvelope(t, slack.SocketEventsAPI, "env-1", false, eventPayload("T0SOCKET", "Ev1"))
	peer.waitForAcks(t, "env-1")
	sock.sendDisconnect(t, slack.DisconnectWarning)
	peer.accept(t, "the successor")

	for _, line := range logs() {
		for _, forbidden := range []string{fakeAppToken, "xapp-", "ticket=", "ticket-1", "ticket-2"} {
			if strings.Contains(line, forbidden) {
				t.Errorf("a log line leaked %q: %s", forbidden, line)
			}
		}
	}

	// The token DID reach the wire, in the one place it belongs.
	peer.mu.Lock()
	defer peer.mu.Unlock()
	if len(peer.auth) == 0 {
		t.Fatal("apps.connections.open was never called")
	}
	for _, h := range peer.auth {
		if h != "Bearer "+fakeAppToken {
			t.Fatalf("apps.connections.open Authorization = %q, want the app-level token as a Bearer credential", h)
		}
	}
}

// TestSlackSocketRefusesAConnectionWithNoAppToken: Socket Mode's ONLY authentication is the app-level token
// presented at connect (D7). A connection registered without an app_token_ref therefore cannot open a socket,
// and must not try — it stops, permanently and loudly, rather than looping on a dial it can never authenticate.
func TestSlackSocketRefusesAConnectionWithNoAppToken(t *testing.T) {
	peer := newFakeSocketPeer(t)
	conn := testConnRef()
	conn.AppTokenRef = ""
	sink := &fakeSocketSink{conn: conn}
	s, logs := newTestSocket(t, peer, sink)
	_, result := runSocket(t, s)

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Run returned %v, want nil — a connection with no app token cannot be fixed by a restart", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return for a connection with no app_token_ref")
	}
	if peer.openCount() != 0 {
		t.Fatalf("apps.connections.open was called %d times with no token to present", peer.openCount())
	}
	found := false
	for _, line := range logs() {
		if strings.Contains(line, "app_token_ref") {
			found = true
		}
	}
	if !found {
		t.Fatalf("nothing in the log names app_token_ref; the operator cannot tell why Socket Mode is off. logs=%v", logs())
	}
}
