package execution

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
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
	// connected counts runner sessions currently held open on this gateway (handshaked and either
	// parked for a lease or serving one). An alive runner keeps its Concurrency park-loops dialed in,
	// so this is >0 while a runner is up and drops to 0 when it stops — the real signal behind the
	// palai_runner_sessions gauge and the runner-down alert (E14 Task 6).
	connected atomic.Int64
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

// Connected reports the number of runner sessions currently held open on the gateway — the value
// behind the palai_runner_sessions gauge. Safe to call from the metrics scrape goroutine.
func (g *RunnerGateway) Connected() int64 { return g.connected.Load() }

// pendingRunner is a runner that completed the handshake and is parked waiting for a
// lease. The connect handler holds the HTTP goroutine open on release so the hijacked
// WebSocket stays alive for the whole lease; the EngineChannel closes release when the
// attempt ends, letting the handler return and tear the connection down.
//
// A single readLoop goroutine owns the connection's read side for its whole life. gc is set by Dial
// (before it writes the lease offer) so that readLoop, which is the sole reader, relays the runner's
// engine frames once a lease is assigned and otherwise just detects a park-time disconnect. That
// disconnect detection is what keeps palai_runner_sessions honest: a runner that dies while parked-
// and-idle (nothing else reads the connection then) is noticed at once, not only at the next Dial.
type pendingRunner struct {
	conn         *websocket.Conn
	release      chan struct{}
	disconnected chan struct{}
	discOnce     sync.Once
	gc           atomic.Pointer[gatewayChannel]
}

// markDisconnected closes disconnected exactly once (readLoop may reach it from several paths).
func (pr *pendingRunner) markDisconnected() { pr.discOnce.Do(func() { close(pr.disconnected) }) }

// Routes returns the gateway HTTP surface: the certless enrollment endpoint and the
// mutually-authenticated session endpoint. It carries no public API auth middleware —
// the endpoints assert their own token and mTLS identity.
func (g *RunnerGateway) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/runner/enroll", g.handleEnroll)
	mux.HandleFunc("/v1/runner/renew", g.handleRenew)
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

// handleRenew re-issues a runner's client certificate over its EXISTING mutually-authenticated
// identity — no enrollment token. It asserts the verified client chain (the server TLS accepts
// a certless handshake for enrollment, so a tokenless, certless caller is rejected here), then
// re-signs the public key the presented certificate carries with a fresh validity window. An
// expired certificate cannot complete the mTLS handshake, so renewal is only possible while
// the current identity is still valid — the proactive ~80%-TTL renewal keeps it so. The
// one-use bootstrap token is never presented again, so a long-lived runner rolls its
// certificate forward without re-enrolling.
func (g *RunnerGateway) handleRenew(w http.ResponseWriter, r *http.Request) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		http.Error(w, "runner client certificate required", http.StatusUnauthorized)
		return
	}
	leaf := r.TLS.PeerCertificates[0]
	publicDER, err := x509.MarshalPKIXPublicKey(leaf.PublicKey)
	if err != nil {
		http.Error(w, "marshal runner public key", http.StatusBadRequest)
		return
	}
	certDER, err := g.issuer.SignRunnerCertificate(publicDER, renewDNS(leaf))
	if err != nil {
		http.Error(w, "sign runner certificate", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(enrollResponse{Certificate: base64.StdEncoding.EncodeToString(certDER)})
}

// renewDNS is the runner DNS identity the presented certificate carries — its SAN, or the
// common name when the SAN is absent — so a renewal preserves the enrolled identity.
func renewDNS(leaf *x509.Certificate) string {
	if len(leaf.DNSNames) > 0 {
		return leaf.DNSNames[0]
	}
	return leaf.Subject.CommonName
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

	// The handshake succeeded: this connection is a live runner session for as long as the handler
	// runs (parked, then leased). Count it here and release the count on any return path below.
	g.connected.Add(1)
	defer g.connected.Add(-1)

	pr := &pendingRunner{conn: conn, release: make(chan struct{}), disconnected: make(chan struct{})}
	// One goroutine owns the read side for the connection's whole life: while parked it turns a
	// dropped connection into a disconnected signal (nothing else reads then), and once a lease is
	// assigned it relays the runner's engine frames. Without it a runner that died while parked-and-
	// idle would keep the connected count — and so palai_runner_sessions — falsely at its old value.
	go g.readLoop(pr)

	select {
	case g.available <- pr:
	case <-pr.disconnected:
		return // the runner dropped before any lease
	case <-r.Context().Done():
		return
	}
	// Hold the hijacked connection open for the lease. release closes when the attempt ends;
	// disconnected closes if the runner drops mid-lease; the request context covers server shutdown.
	select {
	case <-pr.release:
	case <-pr.disconnected:
	case <-r.Context().Done():
	}
}

// Dial offers a connected runner the attempt's lease and returns the bridged EngineChannel. It blocks
// until a runner is available or ctx is done, then publishes the channel to the connection's readLoop
// and writes the lease.offer. It is the production EngineDialer the orchestrator drives unchanged.
func (g *RunnerGateway) Dial(ctx context.Context, attempt AttemptDescriptor) (EngineChannel, error) {
	select {
	case pr := <-g.available:
		// A relayed engine frame can be as large as the lease's per-frame bound; raise the read limit
		// off the handshake cap before the runner's post-offer frames reach readLoop's blocked Read.
		pr.conn.SetReadLimit(attempt.Limits.MaxFrameBytes + 64*1024)
		offer, err := leaseOffer(attempt, g.now())
		if err != nil {
			close(pr.release)
			return nil, err
		}
		gc := newGatewayChannel(pr, attempt)
		// Publish the channel BEFORE writing the offer, so the runner's first engine frame — which it
		// sends only after receiving the offer — always finds a relay target in readLoop.
		pr.gc.Store(gc)
		if err := pr.conn.Write(ctx, websocket.MessageText, offer); err != nil {
			gc.closeFrames()
			close(pr.release)
			return nil, fmt.Errorf("offer lease: %w", err)
		}
		return gc, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// readLoop is the connection's sole reader for its whole life. Before a lease (pr.gc unset) it exists
// only to notice a disconnect: a read error there closes disconnected, so the parked connect handler
// returns and the connected count — and palai_runner_sessions — drops at once rather than lingering
// until the next Dial. After Dial publishes the channel it relays the runner's session frames: an
// engine.frame surfaces on Receive; a lease.complete ends the stream (clean io.EOF on a succeeded
// outcome, an error otherwise so the attempt retries); a read error ends it as EOF.
func (g *RunnerGateway) readLoop(pr *pendingRunner) {
	for {
		messageType, payload, err := pr.conn.Read(context.Background())
		if err != nil {
			if gc := pr.gc.Load(); gc != nil {
				gc.closeFrames() // Receive sees io.EOF
			}
			pr.markDisconnected()
			return
		}
		gc := pr.gc.Load()
		if gc == nil {
			continue // parked: a runner sends nothing before a lease; ignore any stray frame
		}
		if messageType != websocket.MessageText {
			gc.emit(relayRead{err: errors.New("runner session frame must be a text message")})
			gc.closeFrames()
			return
		}
		var message contracts.RunnerMessage
		if err := json.Unmarshal(payload, &message); err != nil {
			gc.emit(relayRead{err: fmt.Errorf("decode runner session frame: %w", err)})
			gc.closeFrames()
			return
		}
		switch message.Type {
		case "engine.frame":
			frame, err := decodeRelayFrame(message.Data)
			if !gc.emit(relayRead{frame: frame, err: err}) || err != nil {
				gc.closeFrames()
				return
			}
		case "lease.complete":
			if outcome, _ := message.Data["outcome"].(string); outcome != "succeeded" {
				gc.emit(relayRead{err: fmt.Errorf("runner reported lease outcome %q", outcome)})
			}
			gc.closeFrames() // succeeded → close frames → Receive sees io.EOF
			return
		default:
			// heartbeat or other non-frame messages carry nothing to relay.
		}
	}
}

// gatewayChannel bridges the runner's lease session to the orchestrator's EngineChannel: Send relays a
// controller frame to the runner, the gateway's readLoop surfaces the runner's engine frames on
// Receive, and the runner's lease.complete closes the stream — clean (io.EOF) on a succeeded outcome,
// an error otherwise so the attempt is retried.
type gatewayChannel struct {
	pr          *pendingRunner
	attempt     AttemptDescriptor
	leaseID     string
	frames      chan relayRead
	releaseOnce sync.Once
	framesOnce  sync.Once
}

type relayRead struct {
	frame contracts.EngineFrame
	err   error
}

func newGatewayChannel(pr *pendingRunner, attempt AttemptDescriptor) *gatewayChannel {
	return &gatewayChannel{pr: pr, attempt: attempt, leaseID: leaseID(attempt), frames: make(chan relayRead)}
}

// closeFrames closes the frames channel exactly once (readLoop reaches it from several paths), so
// Receive sees io.EOF and a repeated close never panics.
func (c *gatewayChannel) closeFrames() { c.framesOnce.Do(func() { close(c.frames) }) }

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
	c.releaseOnce.Do(func() { close(c.pr.release) })
	return nil
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
	// Carry the workspace allocation the runner bind-mounts to /workspace (spec §29.9, FLAG A). Only
	// when the attempt holds one, so a workspace-less lease is byte-for-byte the pre-E09 offer.
	if attempt.WorkspaceHostPath != "" {
		message.Data["workspace_host_path"] = attempt.WorkspaceHostPath
		message.Data["workspace_read_only"] = attempt.WorkspaceReadOnly
		message.Data["workspace_unsafe"] = attempt.WorkspaceUnsafe
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
