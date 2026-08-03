// Package socket is the slack-bot's Socket Mode connect loop (2026-08-03 plan, Task 12.5): the one
// piece of genuinely new code the wiring task needed, because every other piece already existed and
// only lacked a caller.
//
// IT IS THE CONTROL PLANE'S LOOP RE-BOUND, NOT MOVED, and the difference is worth naming because a
// reader will find the two side by side. apps/control-plane/internal/extensions/slack_socket.go is a
// METHOD ON *SlackAdmitter whose every branch takes an api.SlackConnectionRef — a control-plane row —
// and whose failures return to coordinator.Supervise. None of that exists in this process. What was
// taken is the state machine those types were wrapped around, and only that:
//
//   - acknowledge BEFORE any work (no published ack budget can be missed by code that answers first);
//   - a `warning` / `refresh_requested` disconnect opens an OVERLAPPING successor and KEEPS READING the
//     warned socket, because Slack goes on delivering on it for ~10 seconds and a close-then-reconnect
//     loses every event in that window;
//   - `link_disabled` stops permanently — reconnecting would be a hot loop against a door closed on
//     purpose;
//   - a socket that simply ends with nothing live behind it is reconnected.
//
// THE ONE BEHAVIOUR THAT IS NEW HERE RATHER THAN INHERITED IS THE RETRY, and it is new because the
// supervisor is: the control plane's loop returns an error and lets coordinator.Supervise decide when to
// call it again. This process has no supervisor above it — Run IS the top of the bot — so a dial that
// fails has to back off and try again here or the bot exits on the first blip. Which failures are worth
// retrying is therefore a decision this file has to make and the control plane's does not; see
// permanentOpenError.
//
// WHAT THIS PACKAGE DELIBERATELY DOES NOT DO: it never decodes a payload. UnwrapSocketFrame and SocketAck
// are the adapter's (adapters/integrations/slack/socket.go) and are called here; MapEvent and
// MapInteractiveApproval are NOT, because the moment this package mapped an event it would need a bot
// user id, a store and an SDK client, and the transport would stop being testable without them. A Handler
// receives the envelope's payload bytes exactly as Slack sent them — byte-identical to the Events API /
// interactivity HTTP body, which is what makes the mapping transport-invariant.
//
// D7, THE ABSENT SIGNATURE CHECK, IS THE DOCUMENTED CONTRACT AND NOT AN OMISSION.
// https://docs.slack.dev/apis/events-api/using-socket-mode/ (quoted from the adapter, which checked it
// 2026-07-26): "While acknowledging each event is required, there's no need to verify or validate inbound
// events, because you're receiving the events over a pre-authenticated WebSocket." The authentication is
// the app-level token presented once at connect. This package takes no signing secret, so it could not
// verify a signature if a later edit wanted it to.
package socket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/coder/websocket"

	slack "github.com/palgroup/palai/adapters/integrations/slack"
)

const (
	// workBudget bounds ONE envelope's handling. It is OURS, not Slack's: the acknowledgement is already on
	// the wire by the time it starts, so this is not an ack deadline — it is the guard that stops a wedged
	// dependency from freezing the socket's reader for good.
	workBudget = 30 * time.Second

	// dialTimeout bounds apps.connections.open plus the WebSocket handshake. A dial that hangs holds the
	// whole loop, and a retry can only be scheduled by something that returns.
	dialTimeout = 30 * time.Second

	// initialBackoff / maxBackoff pace the reconnect after a dial fails. They are short at the start
	// because the common case is a blip and the bot is silent until it is back, and capped because the
	// uncommon case is Slack being down for an hour and a bot that keeps hammering it is the reason
	// ratelimited exists.
	initialBackoff = 250 * time.Millisecond
	maxBackoff     = 30 * time.Second
)

// ErrDisabled is the `link_disabled` disconnect: Socket Mode switched off in the app's settings. Run
// returns it rather than nil so the process can say WHY it stopped listening — the one stop that an
// operator has to undo in Slack rather than in this deployment.
var ErrDisabled = errors.New("socket: Slack Socket Mode has been disabled in the app's settings")

// Handler is what the loop is allowed to do with an acknowledged envelope. Both methods receive the
// payload bytes verbatim; neither returns an error, because by the time they run the envelope has already
// been acknowledged and Slack will not redeliver it — there is no failure this loop could still act on,
// only one the handler must report itself.
type Handler interface {
	OnEventsAPI(ctx context.Context, payload json.RawMessage)
	OnInteractive(ctx context.Context, payload json.RawMessage)
}

// Config is one bot's connection: the app-level token it presents, and where to present it.
//
// AppToken is BYTES rather than a string for the same reason every other credential in this tree is: it
// comes from a secret redemption, not from a literal, and a []byte does not end up in a %v by accident.
type Config struct {
	AppToken []byte
	// APIBase overrides Slack's own base for a staging or proxied deployment; empty means
	// https://slack.com/api.
	APIBase string
	// Doer is the HTTP client apps.connections.open — and the WebSocket handshake, when it is a real
	// *http.Client — go through. Empty means http.DefaultClient.
	Doer slack.Doer
	// Logf is where this loop narrates. Empty means log.Printf.
	Logf func(string, ...any)
}

func (c Config) apiBase() string {
	if c.APIBase == "" {
		return "https://slack.com/api"
	}
	return c.APIBase
}

func (c Config) doer() slack.Doer {
	if c.Doer == nil {
		return http.DefaultClient
	}
	return c.Doer
}

func (c Config) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// permanentOpenError reports whether Slack's own error code from apps.connections.open names a condition a
// retry cannot change. These are the auth-family codes the Web API answers for every method, plus the
// token-TYPE refusal Socket Mode adds — apps.connections.open accepts an app-level token and nothing else,
// so a bot token presented here is refused identically on every attempt.
//
// EVERYTHING ELSE IS TRANSIENT AND BACKS OFF, and the asymmetry is deliberate: an unknown code that is
// really permanent costs a bot that retries a wall every 30 seconds, which an operator sees in the log; an
// unknown code treated as permanent costs a bot that exited on a blip Slack recovered from a second later,
// which an operator sees only when someone asks why it never answered.
func permanentOpenError(code string) bool {
	switch code {
	case "invalid_auth", "not_authed", "account_inactive", "token_revoked", "token_expired", "no_permission", "not_allowed_token_type":
		return true
	}
	return false
}

// openRefusal is a refusal from apps.connections.open carrying Slack's own error code, so the retry
// decision is made on the code rather than on a formatted string.
type openRefusal struct {
	code string
}

func (e openRefusal) Error() string { return fmt.Sprintf("apps.connections.open refused: %q", e.code) }

// Run holds Socket Mode open until ctx is cancelled, until Slack disables the link, or until a dial is
// refused for a reason no retry can change.
//
// It returns nil for a clean shutdown (ctx cancelled), ErrDisabled for `link_disabled`, and the refusal
// otherwise. It does NOT return for a socket that broke, a dial that failed transiently, or a Slack
// hiccup: those back off and reconnect, which is the whole difference between a bot that stays up and one
// that needs a process supervisor to look like it does.
func Run(ctx context.Context, cfg Config, h Handler) error {
	if len(cfg.AppToken) == 0 {
		return errors.New("socket: no app-level token; it is Socket Mode's only authentication")
	}
	if h == nil {
		return errors.New("socket: no handler; a loop that acknowledges events and drops them is worse than one that does not connect")
	}

	// Sockets share a child context so link_disabled (and any early return) stops the stragglers; ctx's own
	// cancellation — SIGINT/SIGTERM — reaches them through it.
	sockCtx, stopSockets := context.WithCancel(ctx)
	defer stopSockets()

	// Buffered at the documented connection cap so a socket never blocks announcing itself.
	successor := make(chan struct{}, slack.SocketMaxConnections)
	ended := make(chan error, slack.SocketMaxConnections)
	live := 0
	backoff := initialBackoff

	// drain waits for every live socket to finish. Each is already being told to stop, and each finishes
	// the envelope it is holding first — that envelope's acknowledgement is already on the wire, so
	// abandoning it would mean an event Slack believes was delivered and this bot never handled.
	drain := func() {
		for ; live > 0; live-- {
			<-ended
		}
	}

	// open dials once and starts serving it.
	open := func() error {
		c, err := dial(sockCtx, cfg)
		if err != nil {
			return err
		}
		live++
		go func() { ended <- serve(sockCtx, cfg, c, successor, h) }()
		return nil
	}

	// reconnect keeps dialling until one succeeds, ctx ends, or Slack refuses permanently. This is the loop
	// that makes "stays up" true; without it the first failed reconnect ends the process.
	reconnect := func() error {
		for {
			err := open()
			if err == nil {
				backoff = initialBackoff
				return nil
			}
			var refusal openRefusal
			if errors.As(err, &refusal) && permanentOpenError(refusal.code) {
				return err
			}
			cfg.logf("slack-bot: could not open a Socket Mode connection (%v); retrying in %s", err, backoff)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			if backoff *= 2; backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}

	if err := reconnect(); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}

	for {
		select {
		case <-ctx.Done():
			stopSockets()
			drain()
			cfg.logf("slack-bot: Socket Mode drained on shutdown")
			return nil

		case <-successor:
			// A live socket was warned. Open its replacement WITHOUT closing it.
			if live >= slack.SocketMaxConnections {
				// Unreachable with one warned socket at a time, but the cap is Slack's, not ours to exceed.
				cfg.logf("slack-bot: %d Socket Mode connections are already open (Slack's documented cap); not overlapping another", live)
				continue
			}
			if err := open(); err != nil {
				// The warned socket is STILL LIVE and still delivering; losing the successor costs the
				// overlap, not the events. When that socket does end, the live == 0 branch reconnects.
				cfg.logf("slack-bot: could not open the overlapping successor (%v); the warned socket keeps carrying traffic until it closes", err)
			}

		case err := <-ended:
			live--
			switch {
			case errors.Is(err, ErrDisabled):
				cfg.logf("slack-bot: ALERT — Slack sent disconnect reason %q. Socket Mode is switched off in the app's settings; no events will arrive until an operator re-enables it and this process is restarted. Not reconnecting.",
					slack.DisconnectLinkDisabled)
				stopSockets()
				drain()
				return ErrDisabled
			case err != nil && ctx.Err() == nil:
				cfg.logf("slack-bot: a Socket Mode connection ended (%v); %d still open", err, live)
			}
			if ctx.Err() != nil {
				drain()
				return nil
			}
			if live == 0 {
				// Nothing is carrying traffic. A successor request may still be queued — a socket that was
				// warned and then died before the request was served — and serving it AFTER this reconnect
				// would leave a second, redundant connection open for the life of the process. This reconnect
				// IS that successor, so stale requests are discarded here.
				for stale := true; stale; {
					select {
					case <-successor:
					default:
						stale = false
					}
				}
				if err := reconnect(); err != nil {
					if ctx.Err() != nil {
						return nil
					}
					return err
				}
			}
		}
	}
}

// dial opens one connection: apps.connections.open with the app-level token, then the WebSocket at the URL
// it answers with.
//
// CONTRACT https://docs.slack.dev/apis/events-api/using-socket-mode/: POST
// https://slack.com/api/apps.connections.open with `Authorization: Bearer xapp-***` answers
// {"ok":true,"url":"wss://wss.slack.com/link/?ticket=1234-5678"}. That URL is generated PER CALL and is not
// durable, so every reconnect opens a fresh one rather than reusing the last.
//
// NOTHING FROM THIS FUNCTION MAY BE LOGGED OR WRAPPED. The token is a credential and so — for the life of
// the dial — is the `?ticket=` in the URL: whoever holds it can take the socket. Every failure below
// carries a typed reason and never the URL, and the WebSocket error is flattened rather than wrapped
// because websocket.Dial's error text embeds the URL it was given. Run logs these errors on every retry,
// which is exactly why they may not carry one.
func dial(ctx context.Context, cfg Config) (*websocket.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.apiBase()+"/apps.connections.open", nil)
	if err != nil {
		return nil, fmt.Errorf("build apps.connections.open request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+string(cfg.AppToken))
	// The call takes no parameters; Slack's Web API expects a form content type regardless.
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := cfg.doer().Do(req)
	if err != nil {
		return nil, errors.New("apps.connections.open did not answer")
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
		// not_allowed_token_type, ratelimited...), and it is also what decides whether a retry is worth
		// making. The URL field is empty on this branch by definition.
		return nil, openRefusal{code: opened.Error}
	}

	c, _, err := websocket.Dial(ctx, opened.URL, &websocket.DialOptions{HTTPClient: httpClientOf(cfg.doer())})
	if err != nil {
		// FLATTENED ON PURPOSE: websocket.Dial's error text contains the URL, ticket and all. Wrapping it
		// would put a live credential into every retry log line.
		return nil, errors.New("the Socket Mode WebSocket dial failed")
	}
	return c, nil
}

// httpClientOf reuses the configured HTTP client for the WebSocket handshake when it is a real one, so a
// test's httptest client — and a deployment's proxy and timeouts — apply to the dial too.
func httpClientOf(d slack.Doer) *http.Client {
	if c, ok := d.(*http.Client); ok {
		return c
	}
	return http.DefaultClient
}

// serve reads one socket to its end. It returns ErrDisabled for `link_disabled`, nil for a clean shutdown,
// and the read error otherwise. A `warning` / `refresh_requested` disconnect does NOT return: it signals
// for a successor and keeps reading, which IS the overlap.
func serve(ctx context.Context, cfg Config, c *websocket.Conn, successor chan<- struct{}, h Handler) error {
	defer c.CloseNow()
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				// Shutdown: close politely so Slack sees a clean departure rather than a dropped peer. The
				// envelope this loop was handling (if any) has already finished — handling is synchronous.
				_ = c.Close(websocket.StatusNormalClosure, "draining")
				return nil
			}
			return err
		}
		f, err := slack.UnwrapSocketFrame(data)
		if err != nil {
			// Not fatal to the socket: an undecodable frame is one frame. It carries no envelope id that
			// could be acknowledged, so there is nothing to answer either.
			cfg.logf("slack-bot: an undecodable Socket Mode frame arrived")
			continue
		}

		switch f.Type {
		case slack.SocketHello:
			cfg.logf("slack-bot: Socket Mode connected (Slack reports %d of %d connections open)",
				f.NumConnections, slack.SocketMaxConnections)

		case slack.SocketDisconnect:
			if f.Reason == slack.DisconnectLinkDisabled {
				return ErrDisabled
			}
			// `warning` (~10 seconds' notice) and `refresh_requested` (the few-hourly refresh) mean the same
			// thing here: ask for a successor and CARRY ON. Slack keeps delivering on this socket until it
			// actually closes, and every one of those events would be lost by a close-then-reconnect.
			cfg.logf("slack-bot: Slack sent disconnect reason %q; opening an overlapping successor and draining this socket", f.Reason)
			select {
			case successor <- struct{}{}:
			default:
				// The buffer is the documented connection cap; a full one means a successor is already coming.
			}

		default:
			// Everything else is an ENVELOPE and must be acknowledged, "so that Slack knows whether to
			// retry" — including the documented types this bot acts on for nothing (slash_commands), because
			// an unacknowledged envelope is a retried envelope.
			if f.EnvelopeID == "" {
				cfg.logf("slack-bot: a Socket Mode envelope of type %q arrived with no envelope_id; nothing to acknowledge", f.Type)
				continue
			}
			// THE ACKNOWLEDGEMENT GOES FIRST, before any work: no published budget can be missed by code
			// that answers before it starts. nil payload — this loop has nothing to say back inside an
			// acknowledgement; an admitted run answers in the Slack thread later, through the outbound path.
			ack, err := slack.SocketAck(f.EnvelopeID, f.AcceptsResponsePayload, nil)
			if err != nil {
				cfg.logf("slack-bot: could not build a Socket Mode acknowledgement: %v", err)
				continue
			}
			if err := c.Write(ctx, websocket.MessageText, ack); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
			dispatch(ctx, cfg, f, h)
		}
	}
}

// dispatch hands one acknowledged envelope to the handler.
//
// The context is deliberately DETACHED from the socket's: the acknowledgement is already on the wire, so a
// disconnect arriving mid-handling must not abandon work Slack now considers delivered. It is bounded by
// workBudget instead.
//
// Handling is synchronous per socket, so a slow handler delays that socket's next envelope. That is also
// what keeps one thread's messages in order; a worker pool is the upgrade path if one socket's throughput
// ever binds.
func dispatch(ctx context.Context, cfg Config, f slack.SocketFrame, h Handler) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workBudget)
	defer cancel()

	switch f.Type {
	case slack.SocketEventsAPI:
		h.OnEventsAPI(ctx, f.Payload)
	case slack.SocketInteractive:
		h.OnInteractive(ctx, f.Payload)
	default:
		// A documented type this bot subscribes to nothing for (slash_commands). Acknowledged above so
		// Slack stops, and deliberately acted on for nothing — no code is written for an event nobody
		// subscribed to.
		cfg.logf("slack-bot: acknowledged a %q Socket Mode envelope and took no action", f.Type)
	}
}
