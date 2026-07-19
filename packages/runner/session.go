package runner

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/palgroup/palai/packages/contracts"
)

// RunnerProtocolV1 is the control-plane leasing protocol the session speaks.
const RunnerProtocolV1 = "runner.v1"

// dialHandshakeDeadline bounds the outbound dial + runner.v1 handshake when a Session sets
// no explicit DialHandshakeTimeout. It is shorter than the 30s worker-lease window the
// control plane grants (main.go) so a gateway that accepts the connection but never
// completes the handshake fails fast — the serve loop logs it and re-parks — instead of
// wedging on a dead control plane. It bounds only the dial/handshake, never the lease-offer
// park (a parked runner waits for work indefinitely) nor a held lease.
const dialHandshakeDeadline = 20 * time.Second

var imageDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// ErrFrameHashConflict reports a frame id reused with a different payload — a
// protocol violation under the stable request-id discipline.
var ErrFrameHashConflict = errors.New("frame id reused with a different payload hash")

// Lease is the internal projection of a lease.offer: the fenced run/attempt identity,
// the pinned engine image, and the execution bounds the supervisor enforces.
type Lease struct {
	LeaseID     string
	RunID       contracts.RunID
	AttemptID   contracts.AttemptID
	Fence       uint64
	ImageDigest string
	Limits      Limits
}

// Session is the runner's outbound leasing connection. It dials the control plane with
// the short-lived enrolled identity over mutually authenticated TLS; it owns no
// inbound listener. Every field is required.
type Session struct {
	Identity      Identity
	URL           string
	ControllerCAs *x509.CertPool
	ControllerDNS string
	Now           func() time.Time
	// DialHandshakeTimeout bounds the outbound dial + runner.v1 handshake. Zero uses
	// dialHandshakeDeadline. It never bounds the lease-offer park or a held lease.
	DialHandshakeTimeout time.Duration
}

// ReceiveLease opens the outbound session, completes the runner.v1 handshake, and
// returns the offered lease, closing the connection immediately. It never opens an
// inbound connection and returns an error (yielding no lease) if the handshake does not
// complete in ctx. OpenLease is the variant that keeps the connection for the frame relay.
func (s Session) ReceiveLease(ctx context.Context) (Lease, error) {
	connection, transport, lease, err := s.openConnection(ctx)
	if err != nil {
		return Lease{}, err
	}
	_ = connection.Close(websocket.StatusNormalClosure, "lease received")
	transport.CloseIdleConnections()
	return lease, nil
}

// OpenLease completes the same handshake as ReceiveLease but keeps the connection open,
// returning a LeaseSession over which the runner streams engine frames to the controller,
// receives controller frames, and finally reports the terminal outcome. It is the
// persistent form ReceiveLease's one-shot close forecloses.
func (s Session) OpenLease(ctx context.Context) (*LeaseSession, error) {
	connection, transport, lease, err := s.openConnection(ctx)
	if err != nil {
		return nil, err
	}
	// The handshake cap is small; a relayed controller.frame can be as large as the
	// lease's per-frame bound, so raise the read limit before any frame is read.
	connection.SetReadLimit(lease.Limits.MaxFrameBytes + 64*1024)
	return &LeaseSession{conn: connection, transport: transport, lease: lease, now: s.Now}, nil
}

// openConnection dials the control plane over mutually authenticated TLS, completes the
// runner.v1 handshake, and returns the live connection, its transport, and the offered
// lease. The caller owns closing the connection and transport on success; on any error
// the helper closes them itself and returns no lease.
func (s Session) openConnection(ctx context.Context) (*websocket.Conn, *http.Transport, Lease, error) {
	if s.Identity.Certificate.Leaf == nil || s.ControllerCAs == nil || s.ControllerDNS == "" || s.Now == nil {
		return nil, nil, Lease{}, errors.New("session identity, controller trust, DNS and clock are required")
	}
	if !strings.HasPrefix(s.URL, "wss://") {
		return nil, nil, Lease{}, errors.New("session URL must be outbound wss")
	}
	transport := &http.Transport{TLSClientConfig: s.tlsConfig(), Proxy: nil}

	// Bound the dial + hello write with an attempt-scoped deadline: a control plane that
	// accepts the connection but never completes the handshake must fail fast, not wedge
	// the serve loop. The lease-offer read below is the park — a runner waits for the next
	// lease indefinitely — so it stays on the parent context, never this deadline.
	timeout := s.DialHandshakeTimeout
	if timeout <= 0 {
		timeout = dialHandshakeDeadline
	}
	dialCtx, cancelDial := context.WithTimeout(ctx, timeout)
	defer cancelDial()

	connection, _, err := websocket.Dial(dialCtx, s.URL, &websocket.DialOptions{
		HTTPClient:   &http.Client{Transport: transport},
		Subprotocols: []string{RunnerProtocolV1},
	})
	if err != nil {
		transport.CloseIdleConnections()
		return nil, nil, Lease{}, fmt.Errorf("dial control plane: %w", err)
	}
	connection.SetReadLimit(64 * 1024)

	hello, err := json.Marshal(contracts.RunnerMessage{
		Protocol: RunnerProtocolV1,
		Type:     "runner.hello",
		Time:     s.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		closeConnection(connection, transport)
		return nil, nil, Lease{}, fmt.Errorf("encode runner hello: %w", err)
	}
	if err := connection.Write(dialCtx, websocket.MessageText, hello); err != nil {
		closeConnection(connection, transport)
		return nil, nil, Lease{}, fmt.Errorf("write runner hello: %w", err)
	}

	messageType, payload, err := connection.Read(ctx)
	if err != nil {
		closeConnection(connection, transport)
		return nil, nil, Lease{}, fmt.Errorf("read lease offer: %w", err)
	}
	if messageType != websocket.MessageText {
		closeConnection(connection, transport)
		return nil, nil, Lease{}, errors.New("lease offer must be a text message")
	}
	var message contracts.RunnerMessage
	if err := json.Unmarshal(payload, &message); err != nil {
		closeConnection(connection, transport)
		return nil, nil, Lease{}, fmt.Errorf("decode lease offer: %w", err)
	}
	lease, err := ParseLeaseOffer(message)
	if err != nil {
		closeConnection(connection, transport)
		return nil, nil, Lease{}, err
	}
	return connection, transport, lease, nil
}

func closeConnection(connection *websocket.Conn, transport *http.Transport) {
	connection.CloseNow()
	transport.CloseIdleConnections()
}

// LeaseSession is a lease held over a live connection. After OpenLease the runner relays
// each supervised engine frame to the controller with SendEngineFrame, feeds controller
// frames back into the supervisor via ReceiveControllerFrame, and reports the terminal
// outcome and redacted stderr digest with Complete. It stays outbound-only: the runner
// still opens no inbound port, it keeps the one connection it dialed.
type LeaseSession struct {
	conn      *websocket.Conn
	transport *http.Transport
	lease     Lease
	now       func() time.Time
}

// Lease returns the lease this session holds.
func (l *LeaseSession) Lease() Lease { return l.lease }

// SendEngineFrame relays one runner->controller engine frame inside an engine.frame
// message, carrying the single frame in its data.
func (l *LeaseSession) SendEngineFrame(ctx context.Context, frame contracts.EngineFrame) error {
	return l.write(ctx, "engine.frame", map[string]any{"frame": frame})
}

// ReceiveControllerFrame reads one controller->runner engine frame from a
// controller.frame message and returns it for injection into the supervisor.
func (l *LeaseSession) ReceiveControllerFrame(ctx context.Context) (contracts.EngineFrame, error) {
	messageType, payload, err := l.conn.Read(ctx)
	if err != nil {
		return contracts.EngineFrame{}, fmt.Errorf("read controller frame: %w", err)
	}
	if messageType != websocket.MessageText {
		return contracts.EngineFrame{}, errors.New("controller frame must be a text message")
	}
	var message contracts.RunnerMessage
	if err := json.Unmarshal(payload, &message); err != nil {
		return contracts.EngineFrame{}, fmt.Errorf("decode controller message: %w", err)
	}
	if message.Type != "controller.frame" {
		return contracts.EngineFrame{}, fmt.Errorf("unexpected message type %q, want controller.frame", message.Type)
	}
	return decodeRelayFrame(message.Data)
}

// Complete reports the terminal outcome class and the redacted stderr digest in a
// lease.complete message, then closes the connection.
func (l *LeaseSession) Complete(ctx context.Context, outcome string, stderrDigest string) error {
	// Bound the terminal write so a control plane that stopped reading cannot wedge the
	// serve loop at completion; the connection is torn down regardless.
	writeCtx, cancel := context.WithTimeout(ctx, dialHandshakeDeadline)
	defer cancel()
	err := l.write(writeCtx, "lease.complete", map[string]any{"outcome": outcome, "stderr_digest": stderrDigest})
	_ = l.conn.Close(websocket.StatusNormalClosure, "lease complete")
	l.transport.CloseIdleConnections()
	return err
}

// Close tears the lease connection down without reporting an outcome — an aborted lease.
func (l *LeaseSession) Close() error {
	closeConnection(l.conn, l.transport)
	return nil
}

func (l *LeaseSession) write(ctx context.Context, messageType string, data map[string]any) error {
	message := contracts.RunnerMessage{
		Protocol:  RunnerProtocolV1,
		Type:      messageType,
		Time:      l.now().UTC().Format(time.RFC3339),
		LeaseID:   l.lease.LeaseID,
		RunID:     l.lease.RunID,
		AttemptID: l.lease.AttemptID,
		Fence:     int(l.lease.Fence),
		Data:      data,
	}
	payload, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("encode %s: %w", messageType, err)
	}
	return l.conn.Write(ctx, websocket.MessageText, payload)
}

// decodeRelayFrame extracts the single engine.v1 frame a relay message carries in its
// data.frame field.
func decodeRelayFrame(data map[string]any) (contracts.EngineFrame, error) {
	raw, ok := data["frame"]
	if !ok {
		return contracts.EngineFrame{}, errors.New("relay message carries no frame")
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return contracts.EngineFrame{}, fmt.Errorf("encode relay frame: %w", err)
	}
	var frame contracts.EngineFrame
	if err := json.Unmarshal(encoded, &frame); err != nil {
		return contracts.EngineFrame{}, fmt.Errorf("decode relay frame: %w", err)
	}
	return frame, nil
}

func (s Session) tlsConfig() *tls.Config {
	now := s.Now
	config := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{s.Identity.Certificate},
		RootCAs:      s.ControllerCAs.Clone(),
		ServerName:   s.ControllerDNS,
		Time:         func() time.Time { return now() },
	}
	config.VerifyConnection = func(state tls.ConnectionState) error {
		if len(state.VerifiedChains) == 0 || len(state.VerifiedChains[0]) == 0 {
			return errors.New("controller certificate chain was not verified")
		}
		leaf := state.VerifiedChains[0][0]
		if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != s.ControllerDNS {
			return errors.New("controller certificate DNS identity is not exact")
		}
		return nil
	}
	return config
}

// ParseLeaseOffer validates a runner.v1 lease.offer and projects it to a Lease. It
// rejects a mismatched protocol major, a missing fence, or a mutable image reference,
// so nothing downstream ever runs on an unverified offer.
func ParseLeaseOffer(message contracts.RunnerMessage) (Lease, error) {
	if message.Protocol != RunnerProtocolV1 {
		return Lease{}, fmt.Errorf("unsupported lease protocol %q", message.Protocol)
	}
	if message.Type != "lease.offer" {
		return Lease{}, fmt.Errorf("unexpected message type %q", message.Type)
	}
	if message.LeaseID == "" || !message.RunID.Valid() || !message.AttemptID.Valid() {
		return Lease{}, errors.New("lease offer is missing its identity triple")
	}
	if message.Fence < 1 {
		return Lease{}, errors.New("lease offer must carry a positive fence")
	}
	digest, _ := message.Data["image_digest"].(string)
	if !imageDigestPattern.MatchString(digest) {
		return Lease{}, ErrMutableLeaseImage
	}
	limits, err := decodeLimits(message.Data["limits"])
	if err != nil {
		return Lease{}, err
	}
	return Lease{
		LeaseID:     message.LeaseID,
		RunID:       message.RunID,
		AttemptID:   message.AttemptID,
		Fence:       uint64(message.Fence),
		ImageDigest: digest,
		Limits:      limits,
	}, nil
}

// ErrMutableLeaseImage reports a lease.offer without an immutable sha256 image digest.
var ErrMutableLeaseImage = errors.New("lease offer image must be an immutable sha256 digest")

func decodeLimits(raw any) (Limits, error) {
	encoded, err := json.Marshal(raw)
	if err != nil {
		return Limits{}, fmt.Errorf("encode lease limits: %w", err)
	}
	var limits Limits
	if err := json.Unmarshal(encoded, &limits); err != nil {
		return Limits{}, fmt.Errorf("decode lease limits: %w", err)
	}
	if err := limits.validate(); err != nil {
		return Limits{}, err
	}
	return limits, nil
}

// FrameLedger deduplicates engine frames by id under the stable request-id discipline:
// a repeated id with the same payload hash is an idempotent retransmit, while a
// repeated id with a different payload is a protocol violation.
type FrameLedger struct {
	seen map[contracts.FrameID][sha256.Size]byte
}

// NewFrameLedger returns an empty ledger.
func NewFrameLedger() *FrameLedger {
	return &FrameLedger{seen: map[contracts.FrameID][sha256.Size]byte{}}
}

// Admit records frame and reports whether it duplicates an already-seen frame. It
// returns ErrFrameHashConflict when the id was seen with a different payload.
func (l *FrameLedger) Admit(frame contracts.EngineFrame) (bool, error) {
	hash := frameContentHash(frame)
	if prior, ok := l.seen[frame.ID]; ok {
		if prior == hash {
			return true, nil
		}
		return false, fmt.Errorf("%w: %s", ErrFrameHashConflict, frame.ID)
	}
	l.seen[frame.ID] = hash
	return false, nil
}

// frameContentHash hashes the semantic payload of a frame — its type, run/attempt
// identity, reply target, and data — excluding volatile transport fields (sequence
// and time) so an identical retransmit hashes equal.
func frameContentHash(frame contracts.EngineFrame) [sha256.Size]byte {
	canonical := struct {
		Type      string              `json:"type"`
		RunID     contracts.RunID     `json:"run_id"`
		AttemptID contracts.AttemptID `json:"attempt_id"`
		ReplyTo   *string             `json:"reply_to"`
		Data      map[string]any      `json:"data"`
	}{
		Type:      frame.Type,
		RunID:     frame.RunID,
		AttemptID: frame.AttemptID,
		ReplyTo:   frame.ReplyTo,
		Data:      frame.Data,
	}
	encoded, _ := json.Marshal(canonical)
	return sha256.Sum256(encoded)
}
