package palai

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// Sessions is the /v1/sessions resource group (spec §9.1, §9.2): open a standalone session and
// deliver a durable steer command into it. Mirrors Responses's shape (a struct hanging off *Client,
// each method taking a trailing opts ...CallOption) rather than the TS SDK's separate
// Sessions/SessionCommands split — Steer lives directly on Sessions since this task ships only the
// one delivery mode; a later task can still add a nested Commands group without breaking this one.
type Sessions struct{ client *Client }

// CreateSessionParams carries the one thing a caller may supply when opening a session: its
// display label (E29). Mirrors the TS SDK's SessionCreateParams; the server's SessionWrite decode
// is strict (DisallowUnknownFields), so a field beyond `name` is a 400, not a silently ignored one.
type CreateSessionParams struct {
	Name string `json:"name,omitempty"`
}

// SteerParams carries the message a steer delivers (spec §22.4): the command applies at the next
// safe boundary WITHOUT interrupting the session's current step. CommandID is the command's own
// idempotency handle — the SDK mints a stable one per call and a caller may supply its own to
// dedupe across processes, mirroring the Idempotency-Key pattern Responses.Create uses for creates.
type SteerParams struct {
	Message   string
	CommandID string
}

// commandCreateRequest is the wire body for POST /v1/sessions/{id}/commands (spec §22.4, §9.2).
// It is unexported: Sessions.Steer is the only delivery mode this task ships (kind=send_message,
// delivery=steer) — queue/interrupt and the other command kinds are a later task's surface.
type commandCreateRequest struct {
	CommandID string `json:"command_id"`
	Kind      string `json:"kind"`
	Delivery  string `json:"delivery"`
	Message   string `json:"message"`
}

// Session is a standalone session's projection (spec §9.1). Known fields are typed for ergonomics;
// any field a newer server adds is preserved in Extra and re-emitted on marshal (spec API-009),
// matching every other decoded model in this SDK (see types.go).
type Session struct {
	ID     string                     `json:"id"`
	Object string                     `json:"object"`
	Status string                     `json:"status"`
	Extra  map[string]json.RawMessage `json:"-"`
}

func (s *Session) UnmarshalJSON(b []byte) error {
	type alias Session
	var a alias
	if err := forwardUnmarshal(b, &a, &a.Extra); err != nil {
		return err
	}
	*s = Session(a)
	return nil
}

func (s Session) MarshalJSON() ([]byte, error) {
	type alias Session
	return forwardMarshal(alias(s), s.Extra)
}

// Command is a session's durable command (spec §22.4, §9.2). Acceptance means durably queued, not
// applied: Status starts "queued", and a later terminal state arrives over the session's event
// stream, not by re-fetching this struct.
type Command struct {
	ID              string                     `json:"id"`
	Object          string                     `json:"object"`
	SessionID       string                     `json:"session_id"`
	Kind            string                     `json:"kind"`
	Delivery        string                     `json:"delivery,omitempty"`
	Status          string                     `json:"status"`
	CreatedAt       string                     `json:"created_at,omitempty"`
	AppliedSequence *int                       `json:"applied_sequence,omitempty"`
	Result          map[string]any             `json:"result,omitempty"`
	Extra           map[string]json.RawMessage `json:"-"`
}

func (c *Command) UnmarshalJSON(b []byte) error {
	type alias Command
	var a alias
	if err := forwardUnmarshal(b, &a, &a.Extra); err != nil {
		return err
	}
	*c = Command(a)
	return nil
}

func (c Command) MarshalJSON() ([]byte, error) {
	type alias Command
	return forwardMarshal(alias(c), c.Extra)
}

// Create opens a standalone session (201). Creation is cheap and unkeyed — a retried create mints
// a NEW session, never the same one back — so, unlike Responses.Create, this carries no
// Idempotency-Key: a connection torn after the server committed is left alone rather than retried
// into a duplicate session.
func (s *Sessions) Create(ctx context.Context, p CreateSessionParams, opts ...CallOption) (*Session, error) {
	o := requestOptions{body: p}
	for _, opt := range opts {
		opt(&o)
	}
	var out Session
	if err := s.client.doJSON(ctx, http.MethodPost, "/v1/sessions", o, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Steer posts a durable send_message/steer command against a session (spec §22.4). It is 202
// (durably queued, not yet applied). CommandID rides in the body as the server's own dedupe key
// (the command table's unique on it) — a duplicate command_id returns the original resource
// unchanged, so a retried steer settles exactly one command; that is why this call, unlike Create,
// defaults to idempotent for the transport's network-failure retry.
func (s *Sessions) Steer(ctx context.Context, sessionID string, p SteerParams, opts ...CallOption) (*Command, error) {
	commandID := p.CommandID
	if commandID == "" {
		commandID = newCommandID()
	}
	body := commandCreateRequest{
		CommandID: commandID,
		Kind:      "send_message",
		Delivery:  "steer",
		Message:   p.Message,
	}
	o := requestOptions{body: body, idempotent: true}
	for _, opt := range opts {
		opt(&o)
	}
	path := "/v1/sessions/" + escapePathSegment(sessionID) + "/commands"
	var out Command
	if err := s.client.doJSON(ctx, http.MethodPost, path, o, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// newCommandID mints a fresh command idempotency handle (spec §22.4), the same entropy source as
// newIdempotencyKey with the command-scoped prefix the TS SDK's `cmd_${randomUUID()}` also uses.
func newCommandID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "cmd_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return "cmd_" + hex.EncodeToString(b[:])
}
