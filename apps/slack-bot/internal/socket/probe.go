package socket

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/coder/websocket"

	slack "github.com/palgroup/palai/adapters/integrations/slack"
)

// helloTimeout bounds how long Probe waits for Slack's own `hello` after the WebSocket handshake has
// already succeeded. It is generous because the only thing being measured is whether the frame arrives at
// all: a connection that handshakes and then says nothing is the failure this bound exists to name, and it
// is not one a shorter wait diagnoses better.
const helloTimeout = 30 * time.Second

// Probe opens ONE Socket Mode connection, waits for the `hello` frame, and closes it — the connection leg
// of a bot's self-test (2026-08-03 plan, Task 13), and nothing more.
//
// WHY IT IS NOT Run WITH A TIMEOUT. Run is the thing that stays up: it reconnects on every transient
// failure by design, so "it did not connect" and "it is still trying" are deliberately indistinguishable
// from outside it — which is exactly right for a bot and exactly wrong for a test whose whole answer is
// which leg failed. Probe makes ONE attempt and reports what happened to it. It shares Run's dial, so what
// it proves about apps.connections.open, the app-level token and the handshake is what the shipped loop
// does, not a second implementation of it.
//
// IT ACKNOWLEDGES NOTHING, and that is the reason it must not be left open. An envelope arriving on this
// connection would be delivered here and never acknowledged, so Slack would retry it — onto the relay's own
// connection, which is the one that answers. Closing promptly keeps that window to the length of one
// handshake, and Probe drops any envelope it does see rather than pretending to handle it.
//
// The returned count is Slack's own `num_connections` from the hello frame: how many connections this app
// has open, INCLUDING this one. A running relay makes it 2, which is not an error — Socket Mode allows ten
// (slack.SocketMaxConnections) — but it is the one number that tells an operator whether their bot process
// is actually connected, so it is reported rather than discarded.
func Probe(ctx context.Context, cfg Config) (numConnections int, err error) {
	if len(cfg.AppToken) == 0 {
		return 0, errors.New("socket: no app-level token; it is Socket Mode's only authentication")
	}
	c, err := dial(ctx, cfg)
	if err != nil {
		return 0, err
	}
	// CloseNow, not Close: the polite close below is the one that matters and it has already run by here.
	// This is the leak-stopper for every path that leaves before it.
	defer func() { _ = c.CloseNow() }()

	readCtx, cancel := context.WithTimeout(ctx, helloTimeout)
	defer cancel()
	for {
		_, data, readErr := c.Read(readCtx)
		if readErr != nil {
			// FLATTENED, like dial's own WebSocket error: a read error on this connection can carry the URL
			// the handshake was made against, ticket and all.
			if readCtx.Err() != nil {
				return 0, fmt.Errorf("the Socket Mode connection opened but sent no hello within %s", helloTimeout)
			}
			return 0, errors.New("the Socket Mode connection broke before Slack said hello")
		}
		f, unwrapErr := slack.UnwrapSocketFrame(data)
		if unwrapErr != nil {
			// One undecodable frame is not the connection failing — serve() takes the same view.
			continue
		}
		if f.Type != slack.SocketHello {
			// An envelope, or a disconnect. Neither is this leg's question: the connection demonstrably
			// carries frames, but `hello` is the one Slack sends to say the connection is established, so
			// keep reading until the deadline rather than accepting anything at all as proof.
			continue
		}
		// A polite close, so Slack sees a departure rather than a dropped peer — the same courtesy serve()
		// extends on shutdown, and the reason this leg costs the workspace nothing.
		_ = c.Close(websocket.StatusNormalClosure, "self-test")
		return f.NumConnections, nil
	}
}
