package slack

import (
	"bytes"
	"encoding/json"
	"errors"
)

// Source is the source family Slack events dedupe within — the InboundEvent.Source value the canonical
// automation pipeline scopes the source-dedupe unique index by. Both transports (Events API HTTP callback
// and Socket Mode WebSocket) carry the SAME event_id inside, so a transport switch never changes identity.
const Source = "slack"

// Kind classifies a normalized Slack event so the mapping downstream can treat an edit as a correction and a
// delete as a tombstone rather than a fresh message (SLK-005), without decoding the opaque payload twice.
type Kind string

const (
	KindMessage    Kind = "message"    // a new user message / app_mention
	KindCorrection Kind = "correction" // message_changed — an edit supersedes, it is not a new turn
	KindTombstone  Kind = "tombstone"  // message_deleted — the prior content is retracted
	KindFileShare  Kind = "file_share" // a file was shared — a scoped fetch+scan happens control-plane-side
	KindOther      Kind = "other"      // any other subscribed event the mapping may still act on
)

var (
	// ErrIgnored is a well-formed event the adapter deliberately drops so no run is born: a bot's own
	// message or any bot event (SLK-008 — the loop guard). The caller ACKs 2xx and does nothing.
	ErrIgnored = errors.New("slack: event ignored (bot/self — loop guard)")
	// ErrNotAnEvent is a payload whose outer type is not event_callback (e.g. url_verification, which the
	// caller handles via ParseChallenge before verifying a normal event, or an unknown outer type).
	ErrNotAnEvent = errors.New("slack: payload is not an event_callback")
	// ErrMalformed is a structurally unusable envelope — non-JSON, or missing the team/event identity that
	// anchors dedupe and tenant correlation. The caller maps it to a 400 (authenticated client error), the
	// ParseInbound malformed shape.
	ErrMalformed = errors.New("slack: event envelope is malformed")
)

// Event is a Slack event normalized to the canonical inbound identity PLUS the Slack correlation fields the
// downstream mapping needs. Source/SourceTenant/SourceEventID/Data ARE the webhook.InboundEvent identity —
// SourceEventID is Slack's globally-unique event_id, the dedupe key a redelivery repeats, so Slack events
// flow through the exact source-dedupe the webhook seam already proves (AUT-001/AUT-009); no parallel dedupe
// is invented. The correlation fields (team/channel/thread/user) drive thread↔session (SLK-003) and the
// authorization/self-loop guards; Data stays opaque (the mapping validates the inner event later).
type Event struct {
	Source        string          // always Source ("slack")
	SourceTenant  string          // team_id — the workspace the event belongs to
	SourceEventID string          // Slack event_id — the redelivery-stable dedupe key
	Data          json.RawMessage // the inner event object, opaque to this adapter

	TeamID       string
	EnterpriseID string
	ChannelID    string
	ThreadTS     string // the thread root (thread_ts, or the message ts when it starts a thread) — the correlation key
	UserID       string
	Kind         Kind
	Retry        bool // a redelivery (X-Slack-Retry-Num set) — advisory; the dedupe is on SourceEventID
}

// eventCallback is the Events API outer envelope. Only the fields the mapping needs are decoded; the inner
// event stays a RawMessage so the opaque payload is not re-parsed here.
type eventCallback struct {
	Type         string          `json:"type"`
	TeamID       string          `json:"team_id"`
	EnterpriseID string          `json:"enterprise_id"`
	APIAppID     string          `json:"api_app_id"`
	EventID      string          `json:"event_id"`
	Event        json.RawMessage `json:"event"`
}

// innerEvent is the subset of the inner event object the mapping reads to classify + correlate. bot_id (or
// a user equal to the app's own bot user) marks a self/bot event the loop guard drops.
type innerEvent struct {
	Type     string `json:"type"`
	Subtype  string `json:"subtype"`
	User     string `json:"user"`
	BotID    string `json:"bot_id"`
	Channel  string `json:"channel"`
	TS       string `json:"ts"`
	ThreadTS string `json:"thread_ts"`
}

// ParseChallenge returns the url_verification challenge, if the body is that handshake. Slack POSTs it once
// when a Request URL is configured; the receiver echoes the challenge back in plaintext. The token field is
// the deprecated verification token and is ignored. A non-handshake body returns ("", false).
func ParseChallenge(body []byte) (string, bool) {
	var probe struct {
		Type      string `json:"type"`
		Challenge string `json:"challenge"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return "", false
	}
	if probe.Type != "url_verification" {
		return "", false
	}
	return probe.Challenge, true
}

// MapEvent normalizes a verified Events API event_callback body into the canonical Event. botUserID is the
// app's own bot user id (from the connection registry): an inner event whose user IS the bot, or any event
// carrying a bot_id, is ErrIgnored so the app never answers itself (SLK-008). retry is whether Slack marked
// this a redelivery (X-Slack-Retry-Num) — recorded as advisory; identity is the event_id, so a retry
// deduplicates against the original regardless.
//
// The body MUST already have passed VerifySignature: mapping runs strictly after authentication, never
// before, so a forged payload is rejected before it is decoded.
func MapEvent(body []byte, botUserID string, retry bool) (Event, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	var outer eventCallback
	if err := dec.Decode(&outer); err != nil {
		return Event{}, ErrMalformed
	}
	if outer.Type != "event_callback" {
		return Event{}, ErrNotAnEvent
	}
	// team_id + event_id anchor tenant correlation and dedupe; without either the event is unroutable.
	if outer.TeamID == "" || outer.EventID == "" {
		return Event{}, ErrMalformed
	}
	var inner innerEvent
	if len(outer.Event) > 0 {
		if err := json.Unmarshal(outer.Event, &inner); err != nil {
			return Event{}, ErrMalformed
		}
	}
	// Loop guard (SLK-008): a bot event, or the app's own bot user, is dropped BEFORE a run can be born —
	// otherwise the app's own posted reply would re-trigger it. Checked here (not by the caller) so every
	// transport shares one guard.
	if inner.BotID != "" || (botUserID != "" && inner.User == botUserID) {
		return Event{}, ErrIgnored
	}

	thread := inner.ThreadTS
	if thread == "" {
		thread = inner.TS // a top-level message starts its own thread; ts is the correlation root
	}
	return Event{
		Source:        Source,
		SourceTenant:  outer.TeamID,
		SourceEventID: outer.EventID,
		Data:          outer.Event,
		TeamID:        outer.TeamID,
		EnterpriseID:  outer.EnterpriseID,
		ChannelID:     inner.Channel,
		ThreadTS:      thread,
		UserID:        inner.User,
		Kind:          classify(inner.Type, inner.Subtype),
		Retry:         retry,
	}, nil
}

// classify maps a Slack (type, subtype) pair onto the coarse Kind the downstream mapping branches on. An
// edit and a delete are their own kinds so a correction supersedes rather than starting a fresh turn and a
// tombstone retracts (SLK-005), instead of both being treated as new messages.
func classify(typ, subtype string) Kind {
	switch subtype {
	case "message_changed":
		return KindCorrection
	case "message_deleted":
		return KindTombstone
	case "file_share":
		return KindFileShare
	}
	switch typ {
	case "message", "app_mention":
		return KindMessage
	default:
		return KindOther
	}
}

// socketFrame is the Socket Mode WebSocket envelope: a typed frame wrapping the SAME payload the Events API
// / interactivity HTTP transports deliver, plus an envelope_id the receiver echoes to acknowledge.
type socketFrame struct {
	Type       string          `json:"type"`
	EnvelopeID string          `json:"envelope_id"`
	Payload    json.RawMessage `json:"payload"`
}

// SocketFrame is a decoded Socket Mode frame. Type is "events_api" | "interactive" | "hello" | "disconnect"
// | ...; Payload (for events_api / interactive) is exactly the body the HTTP transports carry, so it feeds
// MapEvent / MapInteractiveApproval unchanged — the transport swap does not change correlation identity.
type SocketFrame struct {
	Type       string
	EnvelopeID string
	Payload    json.RawMessage
}

// UnwrapSocketFrame decodes a Socket Mode WebSocket frame. Socket Mode frames are authenticated by the
// app-level token at connect, so they carry NO v0 signature — the caller does not (and must not) run
// VerifySignature on them; the WS peer identity is the auth. The unwrapped Payload is identical in shape to
// the Events API / interactivity HTTP body, so identity (event_id / request_hash) is transport-invariant.
func UnwrapSocketFrame(frame []byte) (SocketFrame, error) {
	var f socketFrame
	if err := json.Unmarshal(frame, &f); err != nil {
		return SocketFrame{}, ErrMalformed
	}
	if f.Type == "" {
		return SocketFrame{}, ErrMalformed
	}
	return SocketFrame{Type: f.Type, EnvelopeID: f.EnvelopeID, Payload: f.Payload}, nil
}
