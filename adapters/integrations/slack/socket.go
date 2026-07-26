package slack

import (
	"encoding/json"
	"errors"
)

// The Socket Mode WIRE FORMAT (E19 T3, spec §36, plan §3.5 rows D4/D6/D7). This file holds the frame codec
// and nothing else — no dialling, no connection state; the connect loop lives control-plane-side
// (apps/control-plane/internal/extensions/slack_socket.go) exactly as the HTTP receiver does.
//
// CONTRACT: https://docs.slack.dev/apis/events-api/using-socket-mode/ (checked 2026-07-26). Every literal
// below is that page's, quoted where it matters:
//
//	envelope      {"payload":…, "envelope_id":…, "type":…, "accepts_response_payload": <bool>}
//	acknowledgement {"envelope_id": "<$unique_identifier_string>", "payload": "<$payload_shape>"}  (payload optional)
//	hello         {"type":"hello","connection_info":{"app_id":"A1234"},"num_connections":1,
//	               "debug_info":{"host":…,"started":…,"build_number":54,"approximate_connection_time":3600}}
//	disconnect    {"type":"disconnect","reason":"link_disabled","debug_info":{"host":"wss-111.slack.com"}}
//
// D7, AND IT IS AN ANCHOR RATHER THAN A DIVERGENCE — the reason this transport skips the v0 verify the HTTP
// routes enforce is the page's own sentence, quoted so a reviewer's "where is the verify?" is answered by
// documentation instead of by argument:
//
//	"While acknowledging each event is required, there's no need to verify or validate inbound events,
//	 because you're receiving the events over a pre-authenticated WebSocket."
//
// The transport's authentication is therefore the APP-LEVEL TOKEN presented at connect time
// (apps.connections.open, Authorization: Bearer xapp-…), not a per-message MAC. VerifySignature is for the
// HTTP transports and must never be reached from here; the control-plane seam this loop drives structurally
// excludes it.
//
// D4 — THE ACK BUDGET IS NOT DOCUMENTED. The page says "Your app still needs to acknowledge receiving each
// event so that Slack knows whether to retry", and says NOTHING about how long that may take. The three-second
// figure this repo uses elsewhere is published for the Events API HTTP callback and for interactivity, not for
// a Socket Mode envelope (searched 2026-07-26; absent). So no number is baked in here: the loop writes the
// acknowledgement BEFORE it does any work, which satisfies any budget that may exist without inventing one.

// The documented envelope `type` values. hello and disconnect are connection lifecycle and carry no
// envelope_id (they are never acknowledged); the rest wrap a payload identical in shape to what the HTTP
// transports deliver, which is what makes correlation identity transport-invariant.
const (
	SocketHello         = "hello"
	SocketDisconnect    = "disconnect"
	SocketEventsAPI     = "events_api"
	SocketInteractive   = "interactive"
	SocketSlashCommands = "slash_commands"
)

// The documented `disconnect` reasons.
const (
	// DisconnectWarning is the ten-second advance notice: "You may receive a warning about 10 seconds before
	// the disconnect." It is an instruction to OVERLAP — open the successor while this socket is still
	// carrying traffic — not to close. See the connect loop for why a close-then-reconnect drops events.
	DisconnectWarning = "warning"
	// DisconnectRefreshRequested is the scheduled connection-refresh cycle ("you'll need to handle connection
	// refreshes once every few hours"). Handled exactly like a warning: overlap, then drain.
	DisconnectRefreshRequested = "refresh_requested"
	// DisconnectLinkDisabled is Socket Mode being toggled off in app settings. PERMANENT — reconnecting would
	// be a hot loop against a door that is closed on purpose.
	DisconnectLinkDisabled = "link_disabled"
)

// SocketMaxConnections is the documented ceiling: "Socket Mode allows your app to maintain up to 10 open
// WebSocket connections at the same time." It is what makes the overlap legal rather than abusive — a
// successor opened before its predecessor drains costs one of ten, briefly.
const SocketMaxConnections = 10

// ErrSocketNoResponsePayload is a caller trying to attach a response payload to an envelope that declared
// accepts_response_payload:false. Refused rather than silently dropped: a drop reads as success at the call
// site, and "the response we thought we sent was never sent" is the kind of bug that surfaces in production.
var ErrSocketNoResponsePayload = errors.New("slack: this envelope does not accept a response payload")

// socketFrame is the wire shape: a typed frame wrapping the SAME payload the Events API / interactivity HTTP
// transports deliver, plus the envelope_id the receiver echoes to acknowledge.
type socketFrame struct {
	Type       string          `json:"type"`
	EnvelopeID string          `json:"envelope_id"`
	Payload    json.RawMessage `json:"payload"`
	// AcceptsResponsePayload is plan §3.5 row D6 — documented, and previously not decoded at all. Attaching a
	// response payload to an envelope that declares false is a protocol error, so the flag has to survive
	// decoding for the rule to be enforceable.
	AcceptsResponsePayload bool `json:"accepts_response_payload"`
	// Reason is set on a disconnect frame and is the whole of the reconnect decision (see the constants).
	Reason string `json:"reason"`
	// NumConnections rides the hello frame. Reported by the loop so an operator can see how much of the
	// ten-connection budget an overlap is using; it drives no logic.
	NumConnections int `json:"num_connections"`
}

// SocketFrame is a decoded Socket Mode frame.
type SocketFrame struct {
	Type                   string
	EnvelopeID             string
	Payload                json.RawMessage
	AcceptsResponsePayload bool
	Reason                 string
	NumConnections         int
}

// UnwrapSocketFrame decodes one Socket Mode WebSocket frame. It runs NO signature check and takes no secret —
// see D7 above; the WebSocket peer identity established at connect IS the authentication. The unwrapped
// Payload is byte-identical to the Events API / interactivity HTTP body, so MapEvent / MapInteractiveApproval
// consume it unchanged and the correlation identity (event_id, request hash) does not move when the transport
// does.
func UnwrapSocketFrame(frame []byte) (SocketFrame, error) {
	var f socketFrame
	if err := json.Unmarshal(frame, &f); err != nil {
		return SocketFrame{}, ErrMalformed
	}
	if f.Type == "" {
		return SocketFrame{}, ErrMalformed
	}
	return SocketFrame{
		Type: f.Type, EnvelopeID: f.EnvelopeID, Payload: f.Payload,
		AcceptsResponsePayload: f.AcceptsResponsePayload, Reason: f.Reason, NumConnections: f.NumConnections,
	}, nil
}

// SocketAck builds the acknowledgement frame for an envelope. payload is optional and may ride the ack ONLY
// when the envelope declared accepts_response_payload — otherwise ErrSocketNoResponsePayload (D6).
//
// An empty envelopeID is refused: an acknowledgement Slack cannot correlate is not an acknowledgement, and
// writing one would look like the event was acked while Slack goes on treating it as unanswered.
func SocketAck(envelopeID string, acceptsResponsePayload bool, payload []byte) ([]byte, error) {
	if envelopeID == "" {
		return nil, errors.New("slack: an acknowledgement needs an envelope_id")
	}
	ack := map[string]any{"envelope_id": envelopeID}
	if len(payload) > 0 {
		if !acceptsResponsePayload {
			return nil, ErrSocketNoResponsePayload
		}
		ack["payload"] = json.RawMessage(payload)
	}
	return json.Marshal(ack)
}
