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
}

// ReceiveLease opens the outbound session, completes the runner.v1 handshake, and
// returns the offered lease. It never opens an inbound connection and returns an error
// (yielding no lease) if the handshake does not complete in ctx.
func (s Session) ReceiveLease(ctx context.Context) (Lease, error) {
	if s.Identity.Certificate.Leaf == nil || s.ControllerCAs == nil || s.ControllerDNS == "" || s.Now == nil {
		return Lease{}, errors.New("session identity, controller trust, DNS and clock are required")
	}
	if !strings.HasPrefix(s.URL, "wss://") {
		return Lease{}, errors.New("session URL must be outbound wss")
	}
	transport := &http.Transport{TLSClientConfig: s.tlsConfig(), Proxy: nil}
	defer transport.CloseIdleConnections()

	connection, _, err := websocket.Dial(ctx, s.URL, &websocket.DialOptions{
		HTTPClient:   &http.Client{Transport: transport},
		Subprotocols: []string{RunnerProtocolV1},
	})
	if err != nil {
		return Lease{}, fmt.Errorf("dial control plane: %w", err)
	}
	defer connection.CloseNow()
	connection.SetReadLimit(64 * 1024)

	hello, err := json.Marshal(contracts.RunnerMessage{
		Protocol: RunnerProtocolV1,
		Type:     "runner.hello",
		Time:     s.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return Lease{}, fmt.Errorf("encode runner hello: %w", err)
	}
	if err := connection.Write(ctx, websocket.MessageText, hello); err != nil {
		return Lease{}, fmt.Errorf("write runner hello: %w", err)
	}

	messageType, payload, err := connection.Read(ctx)
	if err != nil {
		return Lease{}, fmt.Errorf("read lease offer: %w", err)
	}
	if messageType != websocket.MessageText {
		return Lease{}, errors.New("lease offer must be a text message")
	}
	var message contracts.RunnerMessage
	if err := json.Unmarshal(payload, &message); err != nil {
		return Lease{}, fmt.Errorf("decode lease offer: %w", err)
	}
	lease, err := ParseLeaseOffer(message)
	if err != nil {
		return Lease{}, err
	}
	_ = connection.Close(websocket.StatusNormalClosure, "lease received")
	return lease, nil
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
