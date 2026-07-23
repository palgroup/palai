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
	"github.com/palgroup/palai/packages/version"
)

// ErrRunnerCordoned is returned by Dial when the gateway is cordoned: no NEW lease is offered so an
// attempt requeues rather than dispatching onto a runner that is draining for an upgrade (§48.4). An
// in-flight lease is untouched — cordon stops new work, drain waits for the current work to finish.
var ErrRunnerCordoned = errors.New("runner gateway cordoned: no new leases")

// ErrRunnerRevoked is returned by Dial (and closes an incoming connect) when the gateway is revoked: a
// decommissioned/compromised runner's new leases AND stale session frames are refused (SAN-011). Revoke
// is the hard stop cordon is not — a cordoned runner still completes its lease, a revoked one does not.
var ErrRunnerRevoked = errors.New("runner gateway revoked: leases and session frames refused")

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
	// active counts leases currently in flight (offered and not yet closed). Drain waits on THIS, not on
	// connected: a parked-and-idle runner (connected>0, active==0) must not block a drain, only an
	// in-flight lease does. Incremented on a successful Dial offer, decremented when its channel closes.
	active atomic.Int64
	// cpVersion is this control-plane's version stamp, checked against the runner's advertised version in
	// the connect handshake for the §48.2 support window (OPS-008). Defaulted to version.Resolve; a test
	// or a deploy override sets it with SetControlPlaneVersion.
	cpVersion string
	// cordoned stops NEW leases (Dial refuses) while an in-flight lease finishes — the upgrade-drain
	// signal. revoked is the hard stop: new connects rejected AND session frames dropped (SAN-011).
	// ponytail: two atomic.Bool at whole-gateway granularity, not a per-runner registry — SH-0 is a
	// single-runner topology (there is no hosts/runners table in this tier), so "the runner" IS the
	// gateway. A multi-runner fleet would key these by runner id; that is the SaaS/post-SH-0 upgrade path.
	cordoned atomic.Bool
	revoked  atomic.Bool
}

// NewRunnerGateway binds the CA issuer and the one-use token store into a gateway.
func NewRunnerGateway(issuer CertIssuer, tokens EnrollmentTokens) *RunnerGateway {
	return &RunnerGateway{
		issuer:    issuer,
		tokens:    tokens,
		now:       time.Now,
		available: make(chan *pendingRunner),
		cpVersion: version.Resolve(),
	}
}

// SetControlPlaneVersion overrides the control-plane version stamp the connect handshake checks the
// runner's advertised version against (§48.2 window). Defaulted to version.Resolve; a test injects a
// concrete version to exercise the OPS-008 skew rejection deterministically.
func (g *RunnerGateway) SetControlPlaneVersion(v string) { g.cpVersion = v }

// Connected reports the number of runner sessions currently held open on the gateway — the value
// behind the palai_runner_sessions gauge. Safe to call from the metrics scrape goroutine.
func (g *RunnerGateway) Connected() int64 { return g.connected.Load() }

// Cordon stops the gateway offering NEW leases: Dial returns ErrRunnerCordoned so a waiting attempt
// requeues instead of dispatching onto a runner that is about to be replaced (§48.4 drain). An in-flight
// lease is untouched. Resume clears it. Idempotent.
func (g *RunnerGateway) Cordon() { g.cordoned.Store(true) }

// Resume clears a cordon so the gateway offers leases again — the rollback/abort counterpart to Cordon.
func (g *RunnerGateway) Resume() { g.cordoned.Store(false) }

// Revoke is the hard stop (SAN-011): new connects are rejected and session frames from any live runner
// connection are dropped (stale events refused), on top of the cordon's new-lease refusal. A revoked
// gateway never un-revokes in-process — a revoked runner identity is decommissioned, not paused.
func (g *RunnerGateway) Revoke() {
	g.cordoned.Store(true)
	g.revoked.Store(true)
}

// Cordoned reports whether new leases are currently refused (cordoned or revoked). Revoked reports the
// hard-stop state. Both back the drain/revoke drills and a doctor surface.
func (g *RunnerGateway) Cordoned() bool { return g.cordoned.Load() }
func (g *RunnerGateway) Revoked() bool  { return g.revoked.Load() }

// Drain cordons the gateway and blocks until every runner session has quiesced (Connected == 0) or ctx is
// done. It stops new leases and waits for the in-flight lease to finish; if it cannot finish within ctx,
// the caller (a control-plane shutting down for a swap) exits anyway and the interrupted run is reclaimed
// and completed by the EXISTING E10 recovery layer (coordinator reconcile + WorkspaceRecovery, §26.3) —
// drain REUSES that layer, it does not re-implement run migration here. Returns nil on quiesce, ctx.Err()
// on timeout.
func (g *RunnerGateway) Drain(ctx context.Context) error {
	g.Cordon()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if g.active.Load() == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

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
	// A revoked gateway refuses the session before the upgrade, so a decommissioned runner never even
	// parks (SAN-011). Cordon does NOT reject the connect — a cordoned runner still parks and finishes an
	// in-flight lease; it is only Dial that stops offering it NEW work.
	if g.revoked.Load() {
		http.Error(w, ErrRunnerRevoked.Error(), http.StatusForbidden)
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
	_, helloPayload, err := conn.Read(r.Context()) // consume runner.hello
	if err != nil {
		return
	}
	// §48.2 support window (OPS-008): the runner advertises its build stamp in the hello's data.version.
	// A skew outside the current+previous-two-minors window is rejected here — at CONNECT, not enroll — so
	// an ALREADY-ENROLLED runner that is now too old after a control-plane upgrade is caught every session
	// (an enroll-time check would miss it: the runner never re-enrolls). The close reason carries the
	// required intermediate-hop message the runner logs. Two unstamped dev builds compare equal (skip).
	if ok, message := version.Supported(g.cpVersion, helloRunnerVersion(helloPayload)); !ok {
		_ = conn.Close(websocket.StatusPolicyViolation, truncateCloseReason(message))
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
	// A cordoned/revoked gateway offers no NEW lease: return before touching the available channel so the
	// attempt requeues (drain) rather than dispatching onto a runner being replaced or decommissioned.
	if g.revoked.Load() {
		return nil, ErrRunnerRevoked
	}
	if g.cordoned.Load() {
		return nil, ErrRunnerCordoned
	}
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
		gc.active = &g.active // Close decrements the in-flight-lease counter Drain waits on.
		// Publish the channel BEFORE writing the offer, so the runner's first engine frame — which it
		// sends only after receiving the offer — always finds a relay target in readLoop.
		pr.gc.Store(gc)
		if err := pr.conn.Write(ctx, websocket.MessageText, offer); err != nil {
			// Do NOT close frames here: Write can flush the offer and still return a ctx-cancel error, so
			// the runner may already be sending a frame that readLoop is mid-emit on. close(release) alone
			// unblocks that emit (it returns false); the handler then returns → CloseNow → readLoop's Read
			// errors → readLoop closes frames itself. readLoop is the SOLE frames-closer, so there is no
			// send-on-closed-channel panic (which would crash the whole control plane).
			close(pr.release)
			return nil, fmt.Errorf("offer lease: %w", err)
		}
		g.active.Add(1) // the lease is in flight; Close (always called on terminal) decrements it.
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
		// A gateway revoked mid-session refuses this runner's stale frames (SAN-011): tear the relay down
		// as if the runner disconnected, so a decommissioned runner's in-flight events reach no attempt.
		if g.revoked.Load() {
			if gc := pr.gc.Load(); gc != nil {
				gc.emit(relayRead{err: ErrRunnerRevoked})
				gc.closeFrames()
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
	// active is the gateway's in-flight-lease counter (nil in the white-box channel tests). Close
	// decrements it exactly once so a drain sees the lease finish.
	active *atomic.Int64
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

// Close releases the connect handler, which tears the WebSocket down, and decrements the gateway's
// in-flight-lease counter exactly once so a concurrent Drain sees this lease finish.
func (c *gatewayChannel) Close() error {
	c.releaseOnce.Do(func() {
		close(c.pr.release)
		if c.active != nil {
			c.active.Add(-1)
		}
	})
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

// helloRunnerVersion extracts the runner's advertised build stamp from a runner.hello payload's
// data.version field. A hello that carries none (a pre-E15-T2 runner, or a malformed frame) yields the
// empty string, which version.Supported treats as an unstamped build and does not enforce — so an older
// runner that predates the advertised-version handshake is not spuriously rejected by the window check.
func helloRunnerVersion(payload []byte) string {
	var message contracts.RunnerMessage
	if err := json.Unmarshal(payload, &message); err != nil {
		return ""
	}
	v, _ := message.Data["version"].(string)
	return v
}

// truncateCloseReason bounds a WebSocket close reason to the 123-byte control-frame limit (RFC 6455), so
// a long OPS-008 hop message still closes cleanly with as much of the reason as fits.
func truncateCloseReason(reason string) string {
	const maxCloseReason = 123
	if len(reason) <= maxCloseReason {
		return reason
	}
	return reason[:maxCloseReason]
}

// bearer extracts the token from an "Authorization: Bearer <token>" header.
func bearer(header string) (string, bool) {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || header[:len(prefix)] != prefix {
		return "", false
	}
	return header[len(prefix):], true
}
