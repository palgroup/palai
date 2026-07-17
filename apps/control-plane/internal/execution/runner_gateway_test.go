package execution_test

// This is the Docker-free wire proof for the production runner gateway: it drives the
// REAL gateway (enrollment + mTLS WebSocket + lease relay) against a REAL runner
// enrollment and session (packages/runner), the same wire the stub in
// tests/conformance/engine asserts against a control-plane stand-in. The proof lives
// here rather than in tests/conformance/engine because the gateway is an internal
// package (apps/control-plane/internal/execution) that Go forbids importing from
// tests/; the existing stub-based conformance tests are unchanged and keep proving the
// runner-side semantics.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/runner"
)

const (
	gwControllerDNS = "controller.gateway.execution.palai.test"
	gwRunnerID      = "runner-gw-01"
)

func gwLimits() runner.Limits {
	return runner.Limits{
		WallTimeMS: 5000, MaxStdoutBytes: 1 << 20, MaxStderrBytes: 16 * 1024,
		MaxFrameBytes: 64 * 1024, MaxMemoryBytes: 64 * 1024 * 1024, MaxProcessCount: 16,
	}
}

// gatewayCA is the in-test control-plane certificate authority. It mints the gateway's
// TLS server certificate and implements execution.CertIssuer, signing an enrolling
// runner's public key into a short-lived client certificate — the CA Task 12 will bind
// to the .palai layout.
type gatewayCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newGatewayCA(t *testing.T) *gatewayCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Palai gateway execution CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &gatewayCA{cert: cert, key: key, pool: pool}
}

func (ca *gatewayCA) serverCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate server key: %v", err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: gwControllerDNS},
		DNSNames:     []string{gwControllerDNS},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("create server certificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse server certificate: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

// SignRunnerCertificate implements execution.CertIssuer.
func (ca *gatewayCA) SignRunnerCertificate(publicKeyDER []byte, runnerDNS string) ([]byte, error) {
	parsed, err := x509.ParsePKIXPublicKey(publicKeyDER)
	if err != nil {
		return nil, err
	}
	pub, ok := parsed.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("unsupported public key")
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: runnerDNS},
		DNSNames:     []string{runnerDNS},
		NotBefore:    now.Add(-time.Second),
		NotAfter:     now.Add(45 * time.Second),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	return x509.CreateCertificate(rand.Reader, template, ca.cert, pub, ca.key)
}

// oneUseTokens implements execution.EnrollmentTokens: each token is spent on first use.
type oneUseTokens struct {
	mu       sync.Mutex
	consumed map[string]bool
}

func newOneUseTokens(tokens ...string) *oneUseTokens {
	set := &oneUseTokens{consumed: map[string]bool{}}
	for _, token := range tokens {
		set.consumed[token] = false
	}
	return set
}

func (o *oneUseTokens) Consume(token string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	spent, known := o.consumed[token]
	if !known || spent {
		return errors.New("invalid enrollment token")
	}
	o.consumed[token] = true
	return nil
}

// gatewayFixture wires the real gateway behind a real mutually-authenticated TLS server.
type gatewayFixture struct {
	gateway    *execution.RunnerGateway
	ca         *gatewayCA
	enrollURL  string
	sessionURL string
}

func newGatewayFixture(t *testing.T, tokens *oneUseTokens) *gatewayFixture {
	t.Helper()
	ca := newGatewayCA(t)
	gateway := execution.NewRunnerGateway(ca, tokens)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &http.Server{
		Handler: gateway.Routes(),
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{ca.serverCertificate(t)},
			// VerifyClientCertIfGiven, not RequireAndVerify: the enrollment endpoint must
			// accept the certless bootstrap request. The connect handler enforces the
			// verified client chain itself.
			ClientAuth: tls.VerifyClientCertIfGiven,
			ClientCAs:  ca.pool,
		},
		ErrorLog: log.New(io.Discard, "", 0),
	}
	go func() { _ = server.ServeTLS(listener, "", "") }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	})
	addr := listener.Addr().String()
	return &gatewayFixture{
		gateway:    gateway,
		ca:         ca,
		enrollURL:  "https://" + addr + "/v1/runner/enroll",
		sessionURL: "wss://" + addr + "/v1/runner/connect",
	}
}

func (f *gatewayFixture) bootstrap(token string) runner.BootstrapConfig {
	return runner.BootstrapConfig{
		RunnerID:        gwRunnerID,
		RunnerDNS:       gwRunnerID + ".runners.palai.test",
		EnrollmentToken: token,
		EnrollmentURL:   f.enrollURL,
		ControllerCAs:   f.ca.pool,
		ControllerDNS:   gwControllerDNS,
		Now:             time.Now,
	}
}

func (f *gatewayFixture) session(identity runner.Identity) runner.Session {
	return runner.Session{
		Identity:      identity,
		URL:           f.sessionURL,
		ControllerCAs: f.ca.pool,
		ControllerDNS: gwControllerDNS,
		Now:           time.Now,
	}
}

func (f *gatewayFixture) attempt(runID, attemptID string, fence uint64) execution.AttemptDescriptor {
	return execution.AttemptDescriptor{
		RunID:       contracts.RunID(runID),
		AttemptID:   contracts.AttemptID(attemptID),
		Fence:       fence,
		ImageDigest: "sha256:" + strings.Repeat("a", 64),
		Limits:      gwLimits(),
	}
}

// TestGatewayEnrollmentConsumesTokenOnce proves the enrollment endpoint exchanges a
// one-use bootstrap token for a short-lived client identity exactly once: a replay of
// the same token is rejected.
func TestGatewayEnrollmentConsumesTokenOnce(t *testing.T) {
	f := newGatewayFixture(t, newOneUseTokens("gw-token-1"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	identity, err := runner.Enroll(ctx, f.bootstrap("gw-token-1"))
	if err != nil {
		t.Fatalf("first enrollment: %v", err)
	}
	if identity.RunnerID != gwRunnerID || identity.Certificate.Leaf == nil {
		t.Fatalf("enrollment returned no usable identity: %+v", identity)
	}
	if !identity.NotAfter.After(time.Now()) {
		t.Fatalf("issued identity is already expired: NotAfter=%s", identity.NotAfter)
	}
	if _, err := runner.Enroll(ctx, f.bootstrap("gw-token-1")); err == nil {
		t.Fatal("gateway accepted a reused one-use enrollment token")
	}
}

// TestGatewayConnectRequiresRunnerClientCertificate proves the mTLS session endpoint
// refuses a client that presents no runner certificate: the server TLS accepts a
// certless handshake (for enrollment), so the connect handler asserts the verified
// client chain itself.
func TestGatewayConnectRequiresRunnerClientCertificate(t *testing.T) {
	f := newGatewayFixture(t, newOneUseTokens())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS13,
		RootCAs:    f.ca.pool,
		ServerName: gwControllerDNS,
	}}}
	conn, _, err := websocket.Dial(ctx, f.sessionURL, &websocket.DialOptions{
		HTTPClient:   client,
		Subprotocols: []string{runner.RunnerProtocolV1},
	})
	if err == nil {
		conn.Close(websocket.StatusNormalClosure, "unexpected")
		t.Fatal("gateway served the session to a runner presenting no client certificate")
	}
}

// TestGatewayOffersLeaseWithImmutableDigestAndFence proves Dial offers a connected
// runner the waiting attempt's lease, carrying its immutable image digest and fencing
// token, and the runner projects them from the wire.
func TestGatewayOffersLeaseWithImmutableDigestAndFence(t *testing.T) {
	f := newGatewayFixture(t, newOneUseTokens("gw-token-1"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	identity, err := runner.Enroll(ctx, f.bootstrap("gw-token-1"))
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}

	leaseCh := make(chan runner.Lease, 1)
	errCh := make(chan error, 1)
	go func() {
		lease, err := f.session(identity).ReceiveLease(ctx)
		if err != nil {
			errCh <- err
			return
		}
		leaseCh <- lease
	}()

	attempt := f.attempt("run_gwoffer1", "att_gwoffer1", 7)
	ch, err := f.gateway.Dial(ctx, attempt)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer ch.Close()

	select {
	case lease := <-leaseCh:
		if lease.Fence != 7 {
			t.Fatalf("lease fence = %d, want 7", lease.Fence)
		}
		if lease.ImageDigest != attempt.ImageDigest {
			t.Fatalf("lease image digest = %q, want the attempt's immutable digest %q", lease.ImageDigest, attempt.ImageDigest)
		}
		if lease.RunID != attempt.RunID || lease.AttemptID != attempt.AttemptID {
			t.Fatalf("lease identity = %s/%s, want %s/%s", lease.RunID, lease.AttemptID, attempt.RunID, attempt.AttemptID)
		}
	case err := <-errCh:
		t.Fatalf("runner never received the lease: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Dial did not offer the connected runner a lease")
	}
}

// TestGatewayRelaysFramesBothWays proves the EngineChannel Dial returns bridges the
// runner's session frames: an engine frame the runner streams surfaces on Receive, a
// controller frame the orchestrator sends reaches the runner, and the runner's
// lease.complete closes the channel cleanly.
func TestGatewayRelaysFramesBothWays(t *testing.T) {
	f := newGatewayFixture(t, newOneUseTokens("gw-token-1"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	identity, err := runner.Enroll(ctx, f.bootstrap("gw-token-1"))
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}

	runnerErr := make(chan error, 1)
	go func() {
		runnerErr <- runnerSide(ctx, f.session(identity))
	}()

	ch, err := f.gateway.Dial(ctx, f.attempt("run_gwrelay1", "att_gwrelay1", 3))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	engineFrame, err := ch.Receive(ctx)
	if err != nil {
		t.Fatalf("Receive engine frame: %v", err)
	}
	if engineFrame.Type != "engine.ready" || engineFrame.ID != contracts.FrameID("frm_gwready1") {
		t.Fatalf("relayed engine frame = %+v, want the engine.ready frame", engineFrame)
	}

	controllerFrame := contracts.EngineFrame{
		Protocol: "engine.v1", ID: "frm_gwctrl1", Type: "run.start", Sequence: 1,
		Time:  time.Now().UTC().Format(time.RFC3339),
		RunID: "run_gwrelay1", Data: map[string]any{"input": "go"},
	}
	if err := ch.Send(ctx, controllerFrame); err != nil {
		t.Fatalf("Send controller frame: %v", err)
	}

	// After the runner reports lease.complete the channel closes cleanly (io.EOF).
	if _, err := ch.Receive(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("Receive after lease.complete error = %v, want io.EOF", err)
	}
	// Close promptly (as the orchestrator does after a terminal), releasing the runner's
	// graceful close so its Complete returns.
	_ = ch.Close()

	select {
	case err := <-runnerErr:
		if err != nil {
			t.Fatalf("runner side: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runner side did not finish")
	}
}

// runnerSide plays the runner over an open lease: it streams one engine.ready frame,
// expects the controller frame the gateway relays back, and reports the terminal
// outcome — the exact wire packages/runner drives in production.
func runnerSide(ctx context.Context, session runner.Session) error {
	lease, err := session.OpenLease(ctx)
	if err != nil {
		return err
	}
	defer lease.Close()

	ready := contracts.EngineFrame{
		Protocol: "engine.v1", ID: "frm_gwready1", Type: "engine.ready", Sequence: 1,
		Time:      time.Now().UTC().Format(time.RFC3339),
		RunID:     lease.Lease().RunID,
		AttemptID: lease.Lease().AttemptID,
		Data:      map[string]any{"selected_protocol": "engine.v1"},
	}
	if err := lease.SendEngineFrame(ctx, ready); err != nil {
		return err
	}
	controllerFrame, err := lease.ReceiveControllerFrame(ctx)
	if err != nil {
		return err
	}
	if controllerFrame.Type != "run.start" {
		return errors.New("runner did not receive the relayed run.start")
	}
	return lease.Complete(ctx, "succeeded", "sha256:redacted")
}
