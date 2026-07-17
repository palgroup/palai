package execution

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/runner"
)

// CertIssuer signs an enrolling runner's public key into a short-lived client
// certificate with the local control-plane CA. The CA the gateway binds is injected
// (an in-test CA in the conformance proof); binding it to the .palai layout is Task 12.
type CertIssuer interface {
	SignRunnerCertificate(publicKeyDER []byte, runnerDNS string) (certificateDER []byte, err error)
}

// EnrollmentTokens redeems a one-use bootstrap token. Consume returns an error for an
// unknown or already-spent token, so a stolen or replayed token mints no second identity.
type EnrollmentTokens interface {
	Consume(token string) error
}

// RunnerGateway is the control-plane counterpart of the runner's outbound-only model:
// it serves the enrollment endpoint and the mutually-authenticated session endpoint the
// runner dials out to, and it is the production EngineDialer — Dial offers a connected
// runner the waiting attempt's lease and bridges its session frames as an EngineChannel.
// The orchestrator is written once against that seam and never learns it drives a runner
// over a WebSocket rather than a subprocess.
type RunnerGateway struct {
	issuer    CertIssuer
	tokens    EnrollmentTokens
	now       func() time.Time
	available chan *pendingRunner
}

// NewRunnerGateway binds the CA issuer and the one-use token store into a gateway.
func NewRunnerGateway(issuer CertIssuer, tokens EnrollmentTokens) *RunnerGateway {
	return &RunnerGateway{
		issuer:    issuer,
		tokens:    tokens,
		now:       time.Now,
		available: make(chan *pendingRunner),
	}
}

// pendingRunner is a runner that completed the handshake and is parked waiting for a
// lease. The connect handler holds the HTTP goroutine open on release so the hijacked
// WebSocket stays alive for the whole lease; the EngineChannel closes release when the
// attempt ends, letting the handler return and tear the connection down.
type pendingRunner struct {
	conn    *websocket.Conn
	release chan struct{}
}

// Routes returns the gateway HTTP surface: the certless enrollment endpoint and the
// mutually-authenticated session endpoint. It carries no public API auth middleware —
// the endpoints assert their own token and mTLS identity.
func (g *RunnerGateway) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/runner/enroll", g.handleEnroll)
	mux.HandleFunc("/v1/runner/connect", g.handleConnect)
	return mux
}

type enrollRequest struct {
	RunnerID  string `json:"runner_id"`
	PublicKey string `json:"public_key"`
}

type enrollResponse struct {
	Certificate string `json:"certificate"`
}

// handleEnroll exchanges a one-use bearer token for a short-lived client certificate: it
// spends the token, signs the runner's public key with the local CA, and returns the
// certificate. The token is authenticated before the key is signed, so an invalid token
// mints nothing.
func (g *RunnerGateway) handleEnroll(w http.ResponseWriter, r *http.Request) {
	token, ok := bearer(r.Header.Get("Authorization"))
	if !ok || g.tokens.Consume(token) != nil {
		http.Error(w, "invalid enrollment token", http.StatusUnauthorized)
		return
	}
	var request enrollRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&request); err != nil || request.RunnerID == "" {
		http.Error(w, "invalid enrollment request", http.StatusBadRequest)
		return
	}
	publicDER, err := base64.StdEncoding.DecodeString(request.PublicKey)
	if err != nil {
		http.Error(w, "invalid public key", http.StatusBadRequest)
		return
	}
	certDER, err := g.issuer.SignRunnerCertificate(publicDER, runnerDNS(request.RunnerID))
	if err != nil {
		http.Error(w, "sign runner certificate", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(enrollResponse{Certificate: base64.StdEncoding.EncodeToString(certDER)})
}

// handleConnect accepts the runner's mutually-authenticated WebSocket, completes the
// runner.v1 handshake, and parks the connection as available. It asserts the verified
// client chain itself — the server TLS accepts a certless handshake for enrollment, so a
// session presenting no runner certificate is rejected here — then holds the HTTP
// goroutine open until the lease's channel releases it.
func (g *RunnerGateway) handleConnect(w http.ResponseWriter, r *http.Request) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		http.Error(w, "runner client certificate required", http.StatusUnauthorized)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{runner.RunnerProtocolV1}})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(64 * 1024)
	if conn.Subprotocol() != runner.RunnerProtocolV1 {
		_ = conn.Close(websocket.StatusPolicyViolation, "subprotocol")
		return
	}
	if _, _, err := conn.Read(r.Context()); err != nil { // consume runner.hello
		return
	}

	pr := &pendingRunner{conn: conn, release: make(chan struct{})}
	select {
	case g.available <- pr:
	case <-r.Context().Done():
		return
	}
	// Hold the hijacked connection open for the lease. The channel closes release when
	// the attempt ends; a runner that disconnects first cancels the request context.
	select {
	case <-pr.release:
	case <-r.Context().Done():
	}
}

// Dial offers a connected runner the attempt's lease and returns the bridged
// EngineChannel. It blocks until a runner is available or ctx is done, then writes the
// lease.offer and starts relaying the runner's session frames. It is the production
// EngineDialer the orchestrator drives unchanged.
func (g *RunnerGateway) Dial(ctx context.Context, attempt AttemptDescriptor) (EngineChannel, error) {
	select {
	case pr := <-g.available:
		// A relayed engine frame can be as large as the lease's per-frame bound; raise
		// the read limit off the handshake cap before any frame is read.
		pr.conn.SetReadLimit(attempt.Limits.MaxFrameBytes + 64*1024)
		offer, err := leaseOffer(attempt, g.now())
		if err != nil {
			close(pr.release)
			return nil, err
		}
		if err := pr.conn.Write(ctx, websocket.MessageText, offer); err != nil {
			close(pr.release)
			return nil, fmt.Errorf("offer lease: %w", err)
		}
		return newGatewayChannel(pr, attempt), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// gatewayChannel bridges the runner's lease session to the orchestrator's EngineChannel:
// Send relays a controller frame to the runner, a background pump surfaces the runner's
// engine frames on Receive, and the runner's lease.complete closes the stream — clean
// (io.EOF) on a succeeded outcome, an error otherwise so the attempt is retried.
type gatewayChannel struct {
	pr        *pendingRunner
	attempt   AttemptDescriptor
	leaseID   string
	frames    chan relayRead
	closeOnce sync.Once
}

type relayRead struct {
	frame contracts.EngineFrame
	err   error
}

func newGatewayChannel(pr *pendingRunner, attempt AttemptDescriptor) *gatewayChannel {
	c := &gatewayChannel{pr: pr, attempt: attempt, leaseID: leaseID(attempt), frames: make(chan relayRead)}
	go c.pump()
	return c
}

// Send relays one controller->engine frame to the runner inside a controller.frame.
func (c *gatewayChannel) Send(ctx context.Context, frame contracts.EngineFrame) error {
	message := contracts.RunnerMessage{
		Protocol:  runner.RunnerProtocolV1,
		Type:      "controller.frame",
		Time:      time.Now().UTC().Format(time.RFC3339),
		LeaseID:   c.leaseID,
		RunID:     c.attempt.RunID,
		AttemptID: c.attempt.AttemptID,
		Fence:     int(c.attempt.Fence),
		Data:      map[string]any{"frame": frame},
	}
	payload, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("encode controller frame: %w", err)
	}
	return c.pr.conn.Write(ctx, websocket.MessageText, payload)
}

// Receive yields the next engine frame the runner streamed, or io.EOF once the lease
// completes cleanly.
func (c *gatewayChannel) Receive(ctx context.Context) (contracts.EngineFrame, error) {
	select {
	case read, ok := <-c.frames:
		if !ok {
			return contracts.EngineFrame{}, io.EOF
		}
		return read.frame, read.err
	case <-ctx.Done():
		return contracts.EngineFrame{}, ctx.Err()
	}
}

// Close releases the connect handler, which tears the WebSocket down.
func (c *gatewayChannel) Close() error {
	c.closeOnce.Do(func() { close(c.pr.release) })
	return nil
}

// pump reads the runner's session messages and routes them: an engine.frame surfaces on
// Receive; a lease.complete ends the stream, classified from its outcome; a read error
// (the connection closed) ends it as EOF. It is the sole reader of the connection.
func (c *gatewayChannel) pump() {
	defer close(c.frames)
	for {
		messageType, payload, err := c.pr.conn.Read(context.Background())
		if err != nil {
			return // connection closed → Receive sees io.EOF
		}
		if messageType != websocket.MessageText {
			c.emit(relayRead{err: errors.New("runner session frame must be a text message")})
			return
		}
		var message contracts.RunnerMessage
		if err := json.Unmarshal(payload, &message); err != nil {
			c.emit(relayRead{err: fmt.Errorf("decode runner session frame: %w", err)})
			return
		}
		switch message.Type {
		case "engine.frame":
			frame, err := decodeRelayFrame(message.Data)
			if !c.emit(relayRead{frame: frame, err: err}) || err != nil {
				return
			}
		case "lease.complete":
			if outcome, _ := message.Data["outcome"].(string); outcome != "succeeded" {
				c.emit(relayRead{err: fmt.Errorf("runner reported lease outcome %q", outcome)})
			}
			return // succeeded → close frames → Receive sees io.EOF
		default:
			// heartbeat or other non-frame messages carry nothing to relay.
		}
	}
}

// emit delivers one read to Receive, or stops if the channel was closed.
func (c *gatewayChannel) emit(read relayRead) bool {
	select {
	case c.frames <- read:
		return true
	case <-c.pr.release:
		return false
	}
}

// leaseOffer builds the runner.v1 lease.offer for an attempt: the fenced identity, the
// immutable engine image digest, and the execution bounds the runner enforces.
func leaseOffer(attempt AttemptDescriptor, now time.Time) ([]byte, error) {
	message := contracts.RunnerMessage{
		Protocol:  runner.RunnerProtocolV1,
		Type:      "lease.offer",
		Time:      now.UTC().Format(time.RFC3339),
		LeaseID:   leaseID(attempt),
		RunID:     attempt.RunID,
		AttemptID: attempt.AttemptID,
		Fence:     int(attempt.Fence),
		Data: map[string]any{
			"image_digest": attempt.ImageDigest,
			"limits":       attempt.Limits,
		},
	}
	return json.Marshal(message)
}

// decodeRelayFrame extracts the single engine.v1 frame a relay message carries in its
// data.frame field — the inbound mirror of Send's controller.frame wrapping.
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

// leaseID derives a stable lease id for an attempt so every offer for the same attempt
// carries the same lease identity.
func leaseID(attempt AttemptDescriptor) string {
	return "lease_" + string(attempt.AttemptID)
}

// runnerDNS derives the client-certificate DNS identity for an enrolling runner. The
// session verifies the controller's identity, not its own, so this names the runner in
// its certificate without being hostname-checked.
func runnerDNS(runnerID string) string {
	return runnerID + ".runners.palai.internal"
}

// bearer extracts the token from an "Authorization: Bearer <token>" header.
func bearer(header string) (string, bool) {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || header[:len(prefix)] != prefix {
		return "", false
	}
	return header[len(prefix):], true
}
