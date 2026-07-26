package extensions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/palgroup/palai/adapters/integrations/slack"
	"github.com/palgroup/palai/apps/control-plane/api"
)

// The Slack SOCKET MODE connect loop (E19 T3, spec §36, plan §3.5 rows D4/D5/D6/D7). It is the third
// transport into the SAME admission bridge the HTTP Events route (T1) and the interactivity route (T2) drive
// — the whole point of the task is that swapping the transport changes NOTHING about identity:
//
//	events_api   envelope → slack.MapEvent              → SlackAdmitter.Admit   (the T1 path, byte for byte)
//	interactive  envelope → slack.MapInteractiveApproval → SlackAdmitter.Decide (the T2 path, byte for byte)
//
// so a Socket Mode delivery and an HTTP callback of the same Slack event produce ONE run (SLK-001), and the
// component sibling proves that by driving both legs with the same event_id against real Postgres.
//
// WHY THIS TRANSPORT MATTERS OPERATIONALLY (plan §0.1): Socket Mode needs NO PUBLIC URL. No tunnel, no DNS,
// no inbound firewall hole. It is therefore the cheapest live proof the owner can run — an app-level token in
// .env.local and nothing else — which is why the credential-gated live leg in tests/live/slack is written to
// run unchanged the moment SLACK_APP_TOKEN appears.
//
// D7 — THERE IS NO SIGNATURE VERIFICATION HERE, AND THAT IS THE DOCUMENTED CONTRACT, not an omission.
// https://docs.slack.dev/apis/events-api/using-socket-mode/ (checked 2026-07-26):
//
//	"While acknowledging each event is required, there's no need to verify or validate inbound events,
//	 because you're receiving the events over a pre-authenticated WebSocket."
//
// The transport's authentication is the APP-LEVEL TOKEN presented once, at connect time, to
// apps.connections.open. The adapter has said so since E17 T1 (adapters/integrations/slack/signature.go).
// The seam below makes it structural rather than a promise: slackSocketSink deliberately does NOT include
// VerifySignature, so this loop could not call it if it wanted to, and no future edit can quietly start
// verifying (or quietly stop) without changing the seam in front of a reviewer.
//
// D4 — NO ACK BUDGET IS BAKED IN, because none is published. The page says an app "still needs to acknowledge
// receiving each event so that Slack knows whether to retry" and gives NO number. The three-second figure this
// repo honours elsewhere is documented for the Events API HTTP callback and for interactivity, NOT for a
// Socket Mode envelope (searched 2026-07-26; absent). Inventing an SLA out of a neighbouring page is how a
// codebase acquires a constant nobody can source, so instead the acknowledgement is written BEFORE any work
// begins — which satisfies whatever the real budget turns out to be.

const (
	// slackSocketWorkBudget bounds ONE envelope's admission. It is OURS, not Slack's (see D4 above): the
	// acknowledgement has already gone out by the time it starts, so this is not an ack deadline — it is the
	// guard that stops a wedged dependency from freezing the socket's reader for good.
	slackSocketWorkBudget = 30 * time.Second

	// slackSocketDialTimeout bounds apps.connections.open + the WebSocket handshake. A dial that hangs holds
	// the whole loop, and the supervisor can only restart something that returns.
	slackSocketDialTimeout = 30 * time.Second
)

// errSlackSocketDisabled is the `link_disabled` disconnect: Socket Mode switched off in app settings. It is
// the ONE reason that must not be retried — reconnecting would be a hot loop against a door closed on purpose.
var errSlackSocketDisabled = errors.New("extensions: slack socket mode has been disabled in app settings")

// slackSocketSink is what the loop is allowed to do with a decoded envelope: resolve the workspace, admit an
// event, decide an interaction. *SlackAdmitter is the production implementation.
//
// THE ABSENCE IS THE DESIGN. There is no VerifySignature here (D7) and no way to reach one, so "why does the
// Socket Mode path not verify?" is answered by the type rather than by a comment somebody has to trust. The
// second implementation is the test fake, which is what lets the protocol guarantees — acknowledge-before-work,
// overlap-on-warning, zero event loss, permanent stop — run with NO database, instead of hiding behind a
// component skip.
type slackSocketSink interface {
	ResolveConnection(ctx context.Context, teamID, enterpriseID string) (api.SlackConnectionRef, bool, error)
	Admit(ctx context.Context, conn api.SlackConnectionRef, ev slack.Event) (api.SlackAdmitOutcome, error)
	Decide(ctx context.Context, conn api.SlackConnectionRef, intent slack.ApprovalIntent) (api.SlackDecisionOutcome, error)
}

// SlackSocket is the Socket Mode connect loop for ONE registered workspace.
//
// HONEST CEILING, where a reader meets it: ONE workspace, one env var, one loop. Multi-workspace fan-out is
// N connections = N sockets and it is not built here — connection-pool management (a listing query, per-row
// supervision, add/remove on registration) is scale work, and this phase proves the protocol with a single
// connection. A workspace registered AFTER boot is likewise not picked up until the control plane restarts.
// Neither is a defect of the protocol handling; both are stated rather than half-built.
type SlackSocket struct {
	sink    slackSocketSink
	secrets SecretResolver
	doer    slack.Doer
	apiBase string
	teamID  string
	logf    func(string, ...any)
}

// SocketMode returns the connect loop for the workspace named by teamID, or nil when teamID is empty (the
// deployment has not asked for Socket Mode). It reuses the bridge's own secret resolver, HTTP client and API
// base — the same three the outbound chat.update half already uses — so mounting costs no new plumbing.
//
// The bridge itself is the sink, which is what makes the transport swap invisible downstream.
func (a *SlackAdmitter) SocketMode(teamID string) *SlackSocket {
	if teamID == "" {
		return nil
	}
	base, doer := a.apiBase, a.doer
	if base == "" {
		base = "https://slack.com/api"
	}
	if doer == nil {
		doer = http.DefaultClient
	}
	return &SlackSocket{sink: a, secrets: a.secrets, doer: doer, apiBase: base, teamID: teamID}
}

func (s *SlackSocket) log(format string, args ...any) {
	if s.logf != nil {
		s.logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// Run holds Socket Mode open until ctx is cancelled (SIGTERM) or Slack disables the link. It is written for
// coordinator.Supervise: a returned ERROR means "restart me after the backoff", a returned NIL means
// "finished, do not restart" — which is exactly the distinction between a socket that broke and a link that
// was switched off.
//
// THE STATE MACHINE, and every branch of it exists because of a documented behaviour:
//
//   - a live socket that receives `warning` / `refresh_requested` asks for a SUCCESSOR and KEEPS READING.
//     It does not close. Slack's own words: the warning arrives "about 10 seconds before the disconnect",
//     and "if you'd like to handle a scheduled connection restart gracefully, you can generate an additional
//     connection before the restart occurs". A close-then-reconnect loop drops everything Slack delivers in
//     that window — that is not a theoretical risk, it is the RED this task was built on
//     (TestSlackSocketOverlapsOnWarningAndLosesNoEvents fails against the naive form, naming the lost events).
//   - the OVERLAP is legal because "Socket Mode allows your app to maintain up to 10 open WebSocket
//     connections at the same time"; slack.SocketMaxConnections caps how many this loop will hold.
//   - a socket that simply ends with nothing live behind it is reconnected.
//   - `link_disabled` stops everything, permanently, loudly.
func (s *SlackSocket) Run(ctx context.Context) error {
	conn, found, err := s.sink.ResolveConnection(ctx, s.teamID, "")
	switch {
	case err != nil:
		// Infrastructure: the supervisor retries after its backoff.
		return fmt.Errorf("resolve the slack socket connection: %w", err)
	case !found || conn.Disabled:
		// A configuration state a restart cannot fix, so it stops instead of hot-looping. The team id is NOT
		// logged: it is operator-supplied configuration, and the connection id we would print does not exist.
		s.log("slack socket: no enabled Slack connection is registered for the configured workspace; Socket Mode stays off until one is registered and the control plane restarts")
		return nil
	case conn.AppTokenRef == "":
		// Socket Mode's ONLY authentication is the app-level token at connect (D7). Without a handle for it
		// there is nothing to present, and no retry produces one.
		s.log("slack socket: connection %s carries no app_token_ref, and the app-level token is Socket Mode's only authentication; Socket Mode stays off", conn.ID)
		return nil
	}

	// Sockets share a child context so link_disabled (and any early return) can stop the stragglers; ctx's own
	// cancellation — SIGTERM — reaches them through it.
	sockCtx, stopSockets := context.WithCancel(ctx)
	defer stopSockets()

	// Buffered at the documented cap so a socket never blocks announcing itself.
	successor := make(chan struct{}, slack.SocketMaxConnections)
	ended := make(chan error, slack.SocketMaxConnections)
	live := 0

	open := func() error {
		c, err := s.dial(sockCtx, conn)
		if err != nil {
			return err
		}
		live++
		go func() { ended <- s.serve(sockCtx, conn, c, successor) }()
		return nil
	}
	// drain waits for every live socket to finish. Each one is already being told to stop (by ctx or by
	// stopSockets), and each finishes the envelope it is holding first — the acknowledgement for that envelope
	// is already on the wire, so abandoning it would mean an event Slack believes was delivered and we never
	// admitted.
	drain := func() {
		for ; live > 0; live-- {
			<-ended
		}
	}

	if err := open(); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			stopSockets()
			drain()
			s.log("slack socket: drained on shutdown (connection %s)", conn.ID)
			return nil

		case <-successor:
			// A live socket was warned. Open its replacement WITHOUT closing it.
			if live >= slack.SocketMaxConnections {
				// Unreachable with one warned socket at a time, but the cap is Slack's, not ours to exceed.
				s.log("slack socket: %d connections are already open (Slack's documented cap); not overlapping another", live)
				continue
			}
			if err := open(); err != nil {
				// The warned socket is STILL LIVE and still delivering; losing the successor costs the overlap,
				// not the events. When that socket does end, the `live == 0` branch below reconnects.
				s.log("slack socket: could not open the overlapping successor for connection %s (%v); the warned socket keeps carrying traffic until it closes", conn.ID, err)
			}

		case err := <-ended:
			live--
			switch {
			case errors.Is(err, errSlackSocketDisabled):
				s.log("slack socket: ALERT — Slack sent disconnect reason %q for connection %s. Socket Mode is switched off in the app's settings; no events will arrive until an operator re-enables it and the control plane restarts. Not reconnecting.",
					slack.DisconnectLinkDisabled, conn.ID)
				stopSockets()
				drain()
				return nil
			case err != nil && ctx.Err() == nil:
				s.log("slack socket: a connection ended (%v); %d still open", err, live)
			}
			if ctx.Err() != nil {
				drain()
				return nil
			}
			if live == 0 {
				// Nothing is carrying traffic. A successor request may still be queued — a socket that was
				// warned and then died before the request was served — and serving it AFTER this reconnect
				// would leave a second, redundant connection open for the life of the process. This reconnect
				// is that successor, so stale requests are discarded here.
				for stale := true; stale; {
					select {
					case <-successor:
					default:
						stale = false
					}
				}
				// Reconnect; a failure here returns to the supervisor, which backs off rather than spinning.
				if err := open(); err != nil {
					return err
				}
			}
		}
	}
}

// dial opens a connection: apps.connections.open with the app-level token, then the WebSocket at the URL it
// answers with.
//
// CONTRACT https://docs.slack.dev/apis/events-api/using-socket-mode/ (checked 2026-07-26): POST
// https://slack.com/api/apps.connections.open with `Authorization: Bearer xapp-***` answers
// {"ok":true,"url":"wss://wss.slack.com/link/?ticket=1234-5678"}. That URL is generated PER CALL and is not
// durable, so every reconnect opens a fresh one rather than reusing the last.
//
// NOTHING FROM THIS FUNCTION MAY BE LOGGED. The token is a credential, and so — for the life of the dial — is
// the `?ticket=` in the URL: anyone holding it can take the socket. So the failure paths below carry a typed
// reason and never the URL, and the dial error is deliberately flattened rather than wrapped, because
// websocket.Dial's error text embeds the URL it was given. TestSlackSocketNeverLogsTheTokenOrTheTicket is the
// guard, because "we were careful" is not one.
func (s *SlackSocket) dial(ctx context.Context, conn api.SlackConnectionRef) (*websocket.Conn, error) {
	if s.secrets == nil {
		return nil, errors.New("slack socket: no secret resolver is wired; the app-level token cannot be redeemed")
	}
	ctx, cancel := context.WithTimeout(ctx, slackSocketDialTimeout)
	defer cancel()

	token, err := s.secrets(conn.Org, conn.AppTokenRef)
	if err != nil || len(token) == 0 {
		// The ref NAME is not echoed and neither is the resolver's error — a resolver error can carry the
		// backend's own detail.
		return nil, errors.New("slack socket: the app_token_ref did not resolve to an app-level token")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.apiBase+"/apps.connections.open", nil)
	if err != nil {
		return nil, fmt.Errorf("build apps.connections.open request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+string(token))
	// The call takes no parameters; Slack's Web API expects a form content type regardless.
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.doer.Do(req)
	if err != nil {
		return nil, errors.New("slack socket: apps.connections.open did not answer")
	}
	defer resp.Body.Close()
	var opened struct {
		OK    bool   `json:"ok"`
		URL   string `json:"url"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&opened); err != nil {
		return nil, fmt.Errorf("apps.connections.open: undecodable answer (status %d)", resp.StatusCode)
	}
	if !opened.OK || opened.URL == "" {
		// Slack's own error CODE is safe and is the only thing that makes this diagnosable (invalid_auth,
		// not_allowed_token_type, ratelimited...). The URL field is empty on this branch by definition.
		return nil, fmt.Errorf("apps.connections.open refused: %q", opened.Error)
	}

	c, _, err := websocket.Dial(ctx, opened.URL, &websocket.DialOptions{HTTPClient: httpClientOf(s.doer)})
	if err != nil {
		// FLATTENED ON PURPOSE: websocket.Dial's error text contains the URL, ticket and all. Wrapping it here
		// would put a live credential into every reconnect log line.
		return nil, errors.New("slack socket: the WebSocket dial failed")
	}
	return c, nil
}

// httpClientOf reuses the bridge's HTTP client for the WebSocket handshake when it is a real one (so a test's
// httptest client, and a deployment's proxy/timeouts, apply to the dial too).
func httpClientOf(d slack.Doer) *http.Client {
	if c, ok := d.(*http.Client); ok {
		return c
	}
	return http.DefaultClient
}

// serve reads one socket to its end. It returns errSlackSocketDisabled for `link_disabled`, nil for a clean
// shutdown, and the read error otherwise. A `warning` / `refresh_requested` disconnect does NOT return: it
// signals for a successor and keeps reading, which IS the overlap.
func (s *SlackSocket) serve(ctx context.Context, conn api.SlackConnectionRef, c *websocket.Conn, successor chan<- struct{}) error {
	defer c.CloseNow()
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				// Shutdown: close politely so Slack sees a clean departure rather than a dropped peer. The
				// envelope this loop was working (if any) has already finished — handling is synchronous.
				_ = c.Close(websocket.StatusNormalClosure, "draining")
				return nil
			}
			return err
		}
		f, err := slack.UnwrapSocketFrame(data)
		if err != nil {
			// Not fatal to the socket: an undecodable frame is one frame. It carries no envelope id we could
			// acknowledge, so there is nothing to answer either.
			s.log("slack socket: an undecodable frame arrived on connection %s", conn.ID)
			continue
		}

		switch f.Type {
		case slack.SocketHello:
			s.log("slack socket: connected (connection %s; Slack reports %d of %d connections open)",
				conn.ID, f.NumConnections, slack.SocketMaxConnections)

		case slack.SocketDisconnect:
			if f.Reason == slack.DisconnectLinkDisabled {
				return errSlackSocketDisabled
			}
			// `warning` (~10 seconds' notice) and `refresh_requested` (the few-hourly refresh) mean the same
			// thing to us: ask for a successor and CARRY ON. Slack keeps delivering on this socket until it
			// actually closes, and every one of those events would be lost by a close-then-reconnect.
			s.log("slack socket: Slack sent disconnect reason %q on connection %s; opening an overlapping successor and draining this socket", f.Reason, conn.ID)
			select {
			case successor <- struct{}{}:
			default:
				// The buffer is the documented connection cap; a full one means a successor is already coming.
			}

		default:
			// Everything else is an ENVELOPE and must be acknowledged, "so that Slack knows whether to retry"
			// — including the documented types we act on for nothing (slash_commands), because an
			// unacknowledged envelope is a retried envelope.
			if f.EnvelopeID == "" {
				s.log("slack socket: an envelope of type %q arrived with no envelope_id on connection %s; nothing to acknowledge", f.Type, conn.ID)
				continue
			}
			// D4: THE ACKNOWLEDGEMENT GOES FIRST, before any work. No published budget can be missed by code
			// that answers before it starts. nil payload — see the D6 note on dispatch.
			ack, err := slack.SocketAck(f.EnvelopeID, f.AcceptsResponsePayload, nil)
			if err != nil {
				s.log("slack socket: could not build an acknowledgement on connection %s: %v", conn.ID, err)
				continue
			}
			if err := c.Write(ctx, websocket.MessageText, ack); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
			s.dispatch(ctx, conn, f)
		}
	}
}

// dispatch feeds one acknowledged envelope into the SAME bridge the HTTP routes drive.
//
// D6, and the reason no response payload is ever attached: an ack MAY carry one only when the envelope
// declared accepts_response_payload, and this loop has nothing to say back — an admitted run answers in the
// Slack thread later, through the outbound path, not inside the acknowledgement. slack.SocketAck enforces the
// rule (a payload for a refusing envelope is a typed error, not a silent drop) so a future caller that does
// have something to say cannot get it wrong.
//
// The context is deliberately DETACHED from the socket's: the acknowledgement is already on the wire, so a
// disconnect arriving mid-admission must not abandon work Slack now considers delivered. It is bounded by
// slackSocketWorkBudget instead.
//
// ponytail: handling is synchronous per socket, so a slow admission delays that socket's next envelope. That
// is also what keeps SLK-003's per-thread ordering intact and avoids the thread-lock contention a concurrent
// fan-out would create; a worker pool is the upgrade path if one socket's throughput ever binds.
func (s *SlackSocket) dispatch(ctx context.Context, conn api.SlackConnectionRef, f slack.SocketFrame) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), slackSocketWorkBudget)
	defer cancel()

	switch f.Type {
	case slack.SocketEventsAPI:
		// The payload is byte-identical to the Events API HTTP body, so this is T1's MapEvent over T1's bytes.
		// retry=false: Socket Mode publishes no redelivery hint on the envelope (the HTTP transport's
		// X-Slack-Retry-Num has no documented counterpart here), and the flag is advisory anyway — it is
		// excluded from the request hash precisely so a redelivery collapses onto the original.
		ev, err := slack.MapEvent(f.Payload, conn.BotUserID, false)
		switch {
		case errors.Is(err, slack.ErrIgnored):
			// SLK-008, the self-loop guard: our own bot's message. Acknowledged (it was delivered), admitted
			// nowhere.
			return
		case err != nil:
			s.log("slack socket: a malformed events_api envelope arrived on connection %s", conn.ID)
			return
		}
		out, err := s.sink.Admit(ctx, conn, ev)
		switch {
		case err != nil:
			// The envelope is already acknowledged, so Slack will NOT redeliver it: this event is lost, and
			// that has to be visible rather than silent. Acknowledging first is still the right trade — the
			// alternative is an unacknowledged envelope on every slow admission — but the loss is real and
			// named here so an operator reading the log knows what happened.
			s.log("slack socket: ADMISSION FAILED after the envelope was acknowledged — connection=%s event=%s. Slack will not redeliver an acknowledged envelope, so this event produced no run: %v",
				conn.ID, ev.SourceEventID, err)
		case out.Rejected != "":
			s.log("slack socket: admission refused: connection=%s event=%s retryable=%t reason=%s",
				conn.ID, ev.SourceEventID, out.Retryable, out.Rejected)
		default:
			s.log("slack socket: admitted connection=%s event=%s response=%s session=%s replayed=%t",
				conn.ID, ev.SourceEventID, out.ResponseID, out.SessionID, out.Replayed)
		}

	case slack.SocketInteractive:
		// T2's path over T2's bytes. The HTTP route has to url-decode a `payload` form parameter first; here
		// the envelope's payload IS that JSON, so the same mapping runs on the same shape.
		intent, err := slack.MapInteractiveApproval(f.Payload)
		switch {
		case errors.Is(err, slack.ErrNotApproval):
			s.log("slack socket: ignored a non-approval interaction on connection %s", conn.ID)
			return
		case err != nil:
			s.log("slack socket: a malformed interactive envelope arrived on connection %s", conn.ID)
			return
		}
		out, err := s.sink.Decide(ctx, conn, intent)
		switch {
		case err != nil:
			s.log("slack socket: decision failed: connection=%s action=%s: %v", conn.ID, intent.ActionID, err)
		case out.Rejected != "":
			s.log("slack socket: decision refused: connection=%s user=%s reason=%s", conn.ID, intent.UserID, out.Rejected)
		default:
			s.log("slack socket: decided connection=%s decision=%s session=%s publication=%s command=%s repaired=%t",
				conn.ID, out.Decision, out.SessionID, out.PublicationID, out.CommandID, out.Repaired)
		}

	default:
		// A documented type we subscribe to nothing for (slash_commands). Acknowledged above so Slack stops,
		// and deliberately acted on for nothing — no code is written for an event we have not subscribed to.
		s.log("slack socket: acknowledged a %q envelope on connection %s and took no action", f.Type, conn.ID)
	}
}
