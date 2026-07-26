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

// NAIVE FIRST — this file is committed in its broken form ONLY as the RED step for the zero-event-loss
// guarantee (plan §3.5 D5). It closes on `warning` and reconnects, which drops every event Slack delivers in
// the ten-second window. The next commit replaces it with the overlap.

const slackSocketWorkBudget = 30 * time.Second

var errSlackSocketDisabled = errors.New("extensions: slack socket mode is disabled for this app")

type slackSocketSink interface {
	ResolveConnection(ctx context.Context, teamID, enterpriseID string) (api.SlackConnectionRef, bool, error)
	Admit(ctx context.Context, conn api.SlackConnectionRef, ev slack.Event) (api.SlackAdmitOutcome, error)
	Decide(ctx context.Context, conn api.SlackConnectionRef, intent slack.ApprovalIntent) (api.SlackDecisionOutcome, error)
}

// SlackSocket is the Socket Mode connect loop for ONE registered workspace.
type SlackSocket struct {
	sink    slackSocketSink
	secrets SecretResolver
	doer    slack.Doer
	apiBase string
	teamID  string
	logf    func(string, ...any)
}

func (s *SlackSocket) log(format string, args ...any) {
	if s.logf != nil {
		s.logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// Run holds the connection until ctx is cancelled.
func (s *SlackSocket) Run(ctx context.Context) error {
	conn, found, err := s.sink.ResolveConnection(ctx, s.teamID, "")
	switch {
	case err != nil:
		return fmt.Errorf("resolve the slack socket connection: %w", err)
	case !found || conn.Disabled:
		s.log("slack socket: no enabled connection is registered for the configured workspace; Socket Mode stays off")
		return nil
	case conn.AppTokenRef == "":
		s.log("slack socket: the connection carries no app_token_ref, and the app-level token is Socket Mode's only authentication; Socket Mode stays off")
		return nil
	}

	for {
		if ctx.Err() != nil {
			return nil
		}
		c, err := s.dial(ctx, conn)
		if err != nil {
			return err
		}
		err = s.serve(ctx, conn, c)
		if errors.Is(err, errSlackSocketDisabled) {
			s.log("slack socket: Slack sent %s — Socket Mode has been switched off in app settings; the loop stops until an operator turns it back on and restarts the control plane", slack.DisconnectLinkDisabled)
			return nil
		}
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			s.log("slack socket: the connection ended (%v); reconnecting", err)
		}
	}
}

// dial opens apps.connections.open and dials the URL it returns.
func (s *SlackSocket) dial(ctx context.Context, conn api.SlackConnectionRef) (*websocket.Conn, error) {
	token, err := s.secrets(conn.Org, conn.AppTokenRef)
	if err != nil || len(token) == 0 {
		return nil, fmt.Errorf("resolve the slack app_token_ref: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.apiBase+"/apps.connections.open", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+string(token))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.doer.Do(req)
	if err != nil {
		return nil, fmt.Errorf("apps.connections.open: %w", err)
	}
	defer resp.Body.Close()
	var opened struct {
		OK    bool   `json:"ok"`
		URL   string `json:"url"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&opened); err != nil {
		return nil, fmt.Errorf("apps.connections.open: decode: %w", err)
	}
	if !opened.OK || opened.URL == "" {
		return nil, fmt.Errorf("apps.connections.open refused: %s", opened.Error)
	}
	c, _, err := websocket.Dial(ctx, opened.URL, &websocket.DialOptions{HTTPClient: httpClientOf(s.doer)})
	if err != nil {
		return nil, errors.New("slack socket: the websocket dial failed")
	}
	return c, nil
}

func httpClientOf(d slack.Doer) *http.Client {
	if c, ok := d.(*http.Client); ok {
		return c
	}
	return http.DefaultClient
}

// serve reads frames until the socket ends.
func (s *SlackSocket) serve(ctx context.Context, conn api.SlackConnectionRef, c *websocket.Conn) error {
	defer c.CloseNow()
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				_ = c.Close(websocket.StatusNormalClosure, "draining")
				return nil
			}
			return err
		}
		f, err := slack.UnwrapSocketFrame(data)
		if err != nil {
			s.log("slack socket: an undecodable frame arrived on connection %s", conn.ID)
			continue
		}
		switch f.Type {
		case slack.SocketHello:
			s.log("slack socket: connected (connection %s, %d open)", conn.ID, f.NumConnections)
		case slack.SocketDisconnect:
			if f.Reason == slack.DisconnectLinkDisabled {
				return errSlackSocketDisabled
			}
			// NAIVE: close now and reconnect. This is what drops the window's events.
			return fmt.Errorf("disconnect: %s", f.Reason)
		default:
			if f.EnvelopeID == "" {
				continue
			}
			ack, err := slack.SocketAck(f.EnvelopeID, f.AcceptsResponsePayload, nil)
			if err != nil {
				s.log("slack socket: could not build an acknowledgement on connection %s: %v", conn.ID, err)
				continue
			}
			if err := c.Write(ctx, websocket.MessageText, ack); err != nil {
				return err
			}
			s.dispatch(ctx, conn, f)
		}
	}
}

// dispatch feeds the envelope into the SAME admission bridge the HTTP routes drive.
func (s *SlackSocket) dispatch(ctx context.Context, conn api.SlackConnectionRef, f slack.SocketFrame) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), slackSocketWorkBudget)
	defer cancel()
	switch f.Type {
	case slack.SocketEventsAPI:
		ev, err := slack.MapEvent(f.Payload, conn.BotUserID, false)
		if err != nil {
			return
		}
		out, err := s.sink.Admit(ctx, conn, ev)
		if err != nil {
			s.log("slack socket: admission failed: connection=%s event=%s", conn.ID, ev.SourceEventID)
			return
		}
		if out.Rejected != "" {
			s.log("slack socket: admission refused: connection=%s event=%s reason=%s", conn.ID, ev.SourceEventID, out.Rejected)
			return
		}
		s.log("slack socket: admitted connection=%s event=%s response=%s replayed=%t", conn.ID, ev.SourceEventID, out.ResponseID, out.Replayed)
	case slack.SocketInteractive:
		intent, err := slack.MapInteractiveApproval(f.Payload)
		if err != nil {
			return
		}
		if _, err := s.sink.Decide(ctx, conn, intent); err != nil {
			s.log("slack socket: decision failed: connection=%s action=%s", conn.ID, intent.ActionID)
		}
	}
}
