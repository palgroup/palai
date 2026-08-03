package socket

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	slack "github.com/palgroup/palai/adapters/integrations/slack"
)

// The bot's Socket Mode loop, proved against a fake WSS peer built from the PUBLISHED protocol
// (https://docs.slack.dev/apis/events-api/using-socket-mode/). It is UNTAGGED — these are protocol
// guarantees and need no database, and hiding them behind a component skip is this tree's recurring
// failure.
//
// THE FAKE IS WRITTEN LONGHAND, and that is the point: every frame this peer sends is a literal from the
// page, and every acknowledgement it reads is decoded with encoding/json into a bare map —
// slack.SocketAck is never called on this side. A fixture built from our own writer can only confirm our
// own writer.

// ---------------------------------------------------------------------------------------------------
// the fake peer
// ---------------------------------------------------------------------------------------------------

// fakePeer serves both halves of the handshake: the apps.connections.open Web API call and the WebSocket
// the returned URL points at. Each accepted socket is handed to the test over `sockets`, so the test
// drives the frame ORDER itself — which is what the overlap proof needs.
type fakePeer struct {
	srv     *httptest.Server
	sockets chan *fakeSocket

	// refuse, when set, makes apps.connections.open answer {"ok":false,"error":<refuse>} instead.
	refuse atomic.Value // string

	mu    sync.Mutex
	opens int
	auth  []string
	acks  []string
	// withPayload records the envelope ids whose acknowledgement carried a `payload` key.
	withPayload map[string]bool
}

type fakeSocket struct {
	conn *websocket.Conn
	done chan struct{}
}

func newFakePeer(t *testing.T) *fakePeer {
	t.Helper()
	p := &fakePeer{sockets: make(chan *fakeSocket, slack.SocketMaxConnections), withPayload: map[string]bool{}}
	mux := http.NewServeMux()

	// CONTRACT: POST /apps.connections.open with `Authorization: Bearer xapp-***` answers
	// {"ok":true,"url":"wss://wss.slack.com/link/?ticket=1234-5678"}. The URL is generated per call and is
	// NOT durable, which is why a reconnect opens a new one rather than reusing the last.
	mux.HandleFunc("POST /apps.connections.open", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.opens++
		n := p.opens
		p.auth = append(p.auth, r.Header.Get("Authorization"))
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if code, _ := p.refuse.Load().(string); code != "" {
			fmt.Fprintf(w, `{"ok":false,"error":%q}`, code)
			return
		}
		fmt.Fprintf(w, `{"ok":true,"url":%q}`,
			"ws"+strings.TrimPrefix(p.srv.URL, "http")+fmt.Sprintf("/link/?ticket=ticket-%d", n))
	})

	mux.HandleFunc("/link/", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		s := &fakeSocket{conn: conn, done: make(chan struct{})}
		defer close(s.done)

		// The first message on every connection, verbatim from the page.
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"hello","connection_info":{"app_id":"A1234"},"num_connections":1,"debug_info":{"host":"applink-1","started":"2020-10-11 12:12:12.120","build_number":54,"approximate_connection_time":3600}}`))
		p.sockets <- s

		for {
			_, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			// LONGHAND: decoded as raw JSON, never through slack.SocketAck.
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

func (p *fakePeer) accept(t *testing.T, why string) *fakeSocket {
	t.Helper()
	select {
	case s := <-p.sockets:
		return s
	case <-time.After(5 * time.Second):
		t.Fatalf("no WebSocket was opened within 5s (%s)", why)
		return nil
	}
}

func (p *fakePeer) noSocket(t *testing.T, d time.Duration, why string) {
	t.Helper()
	select {
	case <-p.sockets:
		t.Fatalf("the bot opened another WebSocket (%s)", why)
	case <-time.After(d):
	}
}

func (p *fakePeer) acked() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.acks...)
}

func (p *fakePeer) openCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.opens
}

func (p *fakePeer) authHeaders() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.auth...)
}

func (p *fakePeer) waitForAcks(t *testing.T, ids ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got := map[string]bool{}
		for _, id := range p.acked() {
			got[id] = true
		}
		missing := []string(nil)
		for _, want := range ids {
			if !got[want] {
				missing = append(missing, want)
			}
		}
		if len(missing) == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("acknowledged %v, want all of %v", p.acked(), ids)
}

func (s *fakeSocket) sendEnvelope(t *testing.T, typ, envelopeID string, accepts bool, payload string) {
	t.Helper()
	frame := fmt.Sprintf(`{"payload":%s,"envelope_id":%q,"type":%q,"accepts_response_payload":%t}`,
		payload, envelopeID, typ, accepts)
	if err := s.conn.Write(context.Background(), websocket.MessageText, []byte(frame)); err != nil {
		t.Fatalf("send envelope %s: %v", envelopeID, err)
	}
}

func (s *fakeSocket) sendDisconnect(t *testing.T, reason string) {
	t.Helper()
	frame := fmt.Sprintf(`{"type":"disconnect","reason":%q,"debug_info":{"host":"wss-111.slack.com"}}`, reason)
	if err := s.conn.Write(context.Background(), websocket.MessageText, []byte(frame)); err != nil {
		t.Fatalf("send disconnect %s: %v", reason, err)
	}
}

func (s *fakeSocket) close() { _ = s.conn.Close(websocket.StatusNormalClosure, "scheduled restart") }

// ---------------------------------------------------------------------------------------------------
// the recording handler
// ---------------------------------------------------------------------------------------------------

type recordingHandler struct {
	mu           sync.Mutex
	events       []string
	interactives []string
	// block, when non-nil, holds OnEventsAPI until it is closed — the ack-before-work proof.
	block chan struct{}
}

func (h *recordingHandler) OnEventsAPI(_ context.Context, payload json.RawMessage) {
	if h.block != nil {
		<-h.block
	}
	h.mu.Lock()
	h.events = append(h.events, string(payload))
	h.mu.Unlock()
}

func (h *recordingHandler) OnInteractive(_ context.Context, payload json.RawMessage) {
	h.mu.Lock()
	h.interactives = append(h.interactives, string(payload))
	h.mu.Unlock()
}

func (h *recordingHandler) seen() (events, interactives []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.events...), append([]string(nil), h.interactives...)
}

// runLoop starts Run against the peer and returns a func that stops it and yields its error.
func runLoop(t *testing.T, p *fakePeer, h Handler) (context.CancelFunc, func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{
			AppToken: []byte("xapp-1-test-token"),
			APIBase:  p.srv.URL,
			Doer:     p.srv.Client(),
			Logf:     func(string, ...any) {},
		}, h)
	}()
	wait := func() error {
		select {
		case err := <-done:
			return err
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not return within 5s")
			return nil
		}
	}
	t.Cleanup(func() { cancel() })
	return cancel, wait
}

// ---------------------------------------------------------------------------------------------------
// the proofs
// ---------------------------------------------------------------------------------------------------

// The envelope reaches the handler that decides what it means, split by type — this is the whole reason
// the loop exists, and until this task nothing in the bot connected the two.
func TestSocketDeliversEachEnvelopeTypeToItsHandler(t *testing.T) {
	p := newFakePeer(t)
	h := &recordingHandler{}
	cancel, wait := runLoop(t, p, h)

	sock := p.accept(t, "the first connection")
	sock.sendEnvelope(t, slack.SocketEventsAPI, "env-1", false, `{"type":"event_callback","event_id":"Ev1"}`)
	sock.sendEnvelope(t, slack.SocketInteractive, "env-2", false, `{"type":"block_actions"}`)
	// slash_commands is documented, subscribed to for nothing, and must STILL be acknowledged: an
	// unacknowledged envelope is a retried envelope.
	sock.sendEnvelope(t, slack.SocketSlashCommands, "env-3", false, `{"command":"/palai"}`)
	p.waitForAcks(t, "env-1", "env-2", "env-3")

	events, interactives := h.seen()
	if len(events) != 1 || !strings.Contains(events[0], `"Ev1"`) {
		t.Fatalf("events_api payloads = %v, want the one carrying Ev1", events)
	}
	if len(interactives) != 1 || !strings.Contains(interactives[0], "block_actions") {
		t.Fatalf("interactive payloads = %v, want the one block_actions payload", interactives)
	}

	cancel()
	if err := wait(); err != nil {
		t.Fatalf("Run on a clean shutdown: %v", err)
	}
}

// The payload reaches the handler BYTE-IDENTICAL to what Slack sent. That is what makes the mapping
// transport-invariant: the same MapEvent runs over the same bytes an HTTP callback would have carried.
func TestSocketHandsThePayloadThroughUnchanged(t *testing.T) {
	p := newFakePeer(t)
	h := &recordingHandler{}
	cancel, wait := runLoop(t, p, h)

	const payload = `{"type":"event_callback","team_id":"T1","event_id":"Ev9","event":{"type":"app_mention","user":"U1","text":"hi \"there\"","ts":"1.1"}}`
	sock := p.accept(t, "the first connection")
	sock.sendEnvelope(t, slack.SocketEventsAPI, "env-1", false, payload)
	p.waitForAcks(t, "env-1")

	events, _ := h.seen()
	if len(events) != 1 || events[0] != payload {
		t.Fatalf("handler saw %q, want the payload verbatim %q", events, payload)
	}

	cancel()
	_ = wait()
}

// THE ACKNOWLEDGEMENT GOES FIRST. No ack budget is published for a Socket Mode envelope, so the only way
// to satisfy whatever it is turns out to be is to answer before starting work. The handler here blocks
// forever; the acknowledgement must arrive anyway.
func TestSocketAcknowledgesBeforeItWorks(t *testing.T) {
	p := newFakePeer(t)
	h := &recordingHandler{block: make(chan struct{})}
	cancel, wait := runLoop(t, p, h)

	sock := p.accept(t, "the first connection")
	sock.sendEnvelope(t, slack.SocketEventsAPI, "env-1", false, `{"type":"event_callback","event_id":"Ev1"}`)
	p.waitForAcks(t, "env-1") // fails if the ack waits on the handler

	if events, _ := h.seen(); len(events) != 0 {
		t.Fatalf("the handler recorded %v while still blocked", events)
	}
	close(h.block)
	cancel()
	_ = wait()
}

// D6: an acknowledgement carries a `payload` key only when the envelope declared
// accepts_response_payload. This loop attaches none either way — an admitted run answers in the thread
// later, not inside the acknowledgement.
func TestSocketAcknowledgementCarriesNoResponsePayload(t *testing.T) {
	p := newFakePeer(t)
	cancel, wait := runLoop(t, p, &recordingHandler{})

	sock := p.accept(t, "the first connection")
	sock.sendEnvelope(t, slack.SocketEventsAPI, "env-yes", true, `{"type":"event_callback","event_id":"Ev1"}`)
	sock.sendEnvelope(t, slack.SocketEventsAPI, "env-no", false, `{"type":"event_callback","event_id":"Ev2"}`)
	p.waitForAcks(t, "env-yes", "env-no")

	p.mu.Lock()
	defer p.mu.Unlock()
	for id, had := range p.withPayload {
		if had {
			t.Fatalf("the acknowledgement for %s carried a payload; this loop has nothing to say back", id)
		}
	}
	cancel()
	_ = wait()
}

// THE OVERLAP, and the events it exists to save. Slack's warning arrives "about 10 seconds before the
// disconnect" and it keeps delivering on the warned socket for that whole window. A close-then-reconnect
// loop drops every one of those events; this asserts they arrive.
func TestSocketOverlapsOnWarningAndLosesNoEvents(t *testing.T) {
	p := newFakePeer(t)
	h := &recordingHandler{}
	cancel, wait := runLoop(t, p, h)

	first := p.accept(t, "the first connection")
	first.sendDisconnect(t, slack.DisconnectWarning)

	second := p.accept(t, "the successor opened by the warning")

	// The warned socket is STILL live and Slack is still delivering on it.
	first.sendEnvelope(t, slack.SocketEventsAPI, "warned-1", false, `{"type":"event_callback","event_id":"Ev1"}`)
	first.sendEnvelope(t, slack.SocketEventsAPI, "warned-2", false, `{"type":"event_callback","event_id":"Ev2"}`)
	second.sendEnvelope(t, slack.SocketEventsAPI, "fresh-1", false, `{"type":"event_callback","event_id":"Ev3"}`)
	p.waitForAcks(t, "warned-1", "warned-2", "fresh-1")

	if events, _ := h.seen(); len(events) != 3 {
		t.Fatalf("the handler saw %d events across the overlap, want 3: %v", len(events), events)
	}

	// Only when the warned socket actually closes does the count settle — and no THIRD connection is
	// opened for it, because the successor already is that reconnect.
	first.close()
	p.noSocket(t, 300*time.Millisecond, "the successor was already carrying traffic")

	cancel()
	_ = wait()
}

// A socket that just ends, with nothing live behind it, is reconnected. This is the ordinary case: a
// dropped connection must not end the bot.
func TestSocketReconnectsWhenAConnectionEnds(t *testing.T) {
	p := newFakePeer(t)
	cancel, wait := runLoop(t, p, &recordingHandler{})

	first := p.accept(t, "the first connection")
	first.close()

	second := p.accept(t, "the reconnect after the socket ended")
	second.sendEnvelope(t, slack.SocketEventsAPI, "after-reconnect", false, `{"type":"event_callback","event_id":"Ev1"}`)
	p.waitForAcks(t, "after-reconnect")

	if n := p.openCount(); n < 2 {
		t.Fatalf("apps.connections.open was called %d times; a reconnect must open a FRESH url (the ticket is per-call)", n)
	}
	cancel()
	if err := wait(); err != nil {
		t.Fatalf("Run on a clean shutdown: %v", err)
	}
}

// A TRANSIENT dial failure backs off and retries rather than ending the process — the behaviour this
// package adds to the control plane's loop, which returns to a supervisor instead.
func TestSocketRetriesATransientDialFailure(t *testing.T) {
	p := newFakePeer(t)
	p.refuse.Store("ratelimited")
	cancel, wait := runLoop(t, p, &recordingHandler{})

	// It must keep trying while refused...
	deadline := time.Now().Add(3 * time.Second)
	for p.openCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := p.openCount(); n < 2 {
		t.Fatalf("apps.connections.open was called %d times after a transient refusal; want a retry", n)
	}

	// ...and connect the moment Slack stops refusing.
	p.refuse.Store("")
	sock := p.accept(t, "the connection after the refusal cleared")
	sock.sendEnvelope(t, slack.SocketEventsAPI, "recovered", false, `{"type":"event_callback","event_id":"Ev1"}`)
	p.waitForAcks(t, "recovered")

	cancel()
	if err := wait(); err != nil {
		t.Fatalf("Run on a clean shutdown: %v", err)
	}
}

// A PERMANENT refusal returns instead of hot-looping: no retry makes a revoked token valid, and a bot
// that spins on one is a bot whose log says nothing an operator can act on.
func TestSocketStopsOnAPermanentRefusal(t *testing.T) {
	p := newFakePeer(t)
	p.refuse.Store("invalid_auth")
	_, wait := runLoop(t, p, &recordingHandler{})

	err := wait()
	if err == nil || !strings.Contains(err.Error(), "invalid_auth") {
		t.Fatalf("Run returned %v, want a refusal naming Slack's own error code", err)
	}
	if n := p.openCount(); n != 1 {
		t.Fatalf("apps.connections.open was called %d times for a permanent refusal, want exactly 1", n)
	}
}

// `link_disabled` is Socket Mode switched off in the app's settings: permanent, loud, and NOT retried.
func TestSocketStopsPermanentlyOnLinkDisabled(t *testing.T) {
	p := newFakePeer(t)
	_, wait := runLoop(t, p, &recordingHandler{})

	sock := p.accept(t, "the first connection")
	sock.sendDisconnect(t, slack.DisconnectLinkDisabled)

	if err := wait(); err != ErrDisabled {
		t.Fatalf("Run returned %v, want ErrDisabled", err)
	}
	if n := p.openCount(); n != 1 {
		t.Fatalf("apps.connections.open was called %d times after link_disabled, want exactly 1 — reconnecting is a hot loop against a door closed on purpose", n)
	}
}

// The app-level token is what authenticates the dial, and it must be the one the caller configured —
// this process reads no Slack token from its own environment, so a token that came from anywhere else
// would be a bug the wire can see.
func TestSocketPresentsTheConfiguredAppToken(t *testing.T) {
	p := newFakePeer(t)
	cancel, wait := runLoop(t, p, &recordingHandler{})
	p.accept(t, "the first connection")

	headers := p.authHeaders()
	if len(headers) != 1 || headers[0] != "Bearer xapp-1-test-token" {
		t.Fatalf("apps.connections.open saw Authorization %v, want the configured app-level token", headers)
	}
	cancel()
	_ = wait()
}

// A malformed frame costs one frame, not the socket: it carries no envelope id, so there is nothing to
// acknowledge, and the next real envelope must still be handled.
func TestSocketSurvivesAMalformedFrame(t *testing.T) {
	p := newFakePeer(t)
	h := &recordingHandler{}
	cancel, wait := runLoop(t, p, h)

	sock := p.accept(t, "the first connection")
	if err := sock.conn.Write(context.Background(), websocket.MessageText, []byte(`{not json`)); err != nil {
		t.Fatalf("write malformed frame: %v", err)
	}
	sock.sendEnvelope(t, slack.SocketEventsAPI, "after-garbage", false, `{"type":"event_callback","event_id":"Ev1"}`)
	p.waitForAcks(t, "after-garbage")

	cancel()
	if err := wait(); err != nil {
		t.Fatalf("Run on a clean shutdown: %v", err)
	}
}

// Shutdown is clean: the loop returns nil, and it returns rather than hanging on a live socket.
func TestSocketReturnsCleanlyOnShutdown(t *testing.T) {
	p := newFakePeer(t)
	cancel, wait := runLoop(t, p, &recordingHandler{})
	sock := p.accept(t, "the first connection")

	cancel()
	if err := wait(); err != nil {
		t.Fatalf("Run on a clean shutdown returned %v, want nil", err)
	}
	select {
	case <-sock.done:
	case <-time.After(2 * time.Second):
		t.Fatal("the peer's read loop never ended; the socket was not closed on shutdown")
	}
}

// Run refuses rather than dialling with nothing to present, and refuses a nil handler rather than
// acknowledging events into the void — an acknowledged envelope is never redelivered, so dropping one
// silently is worse than not connecting at all.
func TestSocketRefusesAnUnusableConfiguration(t *testing.T) {
	if err := Run(context.Background(), Config{}, &recordingHandler{}); err == nil {
		t.Fatal("Run accepted an empty app token; it is Socket Mode's only authentication")
	}
	if err := Run(context.Background(), Config{AppToken: []byte("xapp-1")}, nil); err == nil {
		t.Fatal("Run accepted a nil handler")
	}
}
