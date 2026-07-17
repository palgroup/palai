//go:build e2e

package responses

// Live channel parity: the same live_loop scenario driven through the production
// topology — orchestrator -> runner gateway -> real runner session -> StreamSupervisor ->
// reference engine OCI image — must produce the byte-for-byte same canonical outcome as
// the deterministic subprocess channel. It also proves the provider secret never reaches
// the engine over that path. Requires Docker and the reference engine image
// (PALAI_REFERENCE_ENGINE_IMAGE); the responses e2e script builds it and exports the id.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/oci"
	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
	"github.com/palgroup/palai/packages/runner"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// frameJSON renders a relayed engine frame as its JSON payload for the secret-hygiene scan.
func frameJSON(frame contracts.EngineFrame) string {
	encoded, _ := json.Marshal(frame)
	return string(encoded)
}

const (
	parityControllerDNS = "controller.parity.palai.test"
	parityRunnerID      = "runner-parity-01"
)

func referenceEngineImage(t *testing.T) string {
	t.Helper()
	image := os.Getenv("PALAI_REFERENCE_ENGINE_IMAGE")
	if image == "" {
		t.Skip("PALAI_REFERENCE_ENGINE_IMAGE is required; run make test-e2e TEST=responses")
	}
	return image
}

// canonicalOutcome is the deterministic result of one response, stripped of volatile ids
// and timestamps: the ordered journal event types, the terminal state, the message
// output, and the accumulated usage.
type canonicalOutcome struct {
	eventTypes []string
	state      string
	output     string
	usage      contracts.Usage
}

func (h *harness) captureOutcome(sessionID, responseID string) canonicalOutcome {
	h.t.Helper()
	var types []string
	for _, e := range h.events(sessionID) {
		types = append(types, e.typ)
	}
	state, projection := h.response(responseID)
	output := ""
	if len(projection.Output) == 1 {
		output, _ = projection.Output[0]["content"].(string)
	}
	return canonicalOutcome{eventTypes: types, state: state, output: output, usage: projection.Usage}
}

// TestGatewayLoopMatchesSubprocessChannelOutcome proves the production gateway path
// yields the identical canonical outcome as the deterministic subprocess channel: same
// ordered event types, same terminal state, same output, same usage. The orchestrator is
// unchanged — only the injected EngineDialer differs (subprocess vs gateway).
func TestGatewayLoopMatchesSubprocessChannelOutcome(t *testing.T) {
	image := referenceEngineImage(t)
	h := newHarness(t)

	// Reference path: the deterministic subprocess channel.
	subRespID, subSessID, subRunID := h.admit()
	if err := h.newOrchestrator(subprocessDialer{engineDir: h.engineDir}).
		ExecuteAttempt(context.Background(), h.descriptor(subRunID, 1)); err != nil {
		t.Fatalf("subprocess ExecuteAttempt error = %v", err)
	}
	subprocess := h.captureOutcome(subSessID, subRespID)

	// Gateway path: the real gateway -> runner session -> StreamSupervisor -> reference OCI.
	gwRespID, gwSessID, gwRunID := h.admit()
	gw := h.runGatewayLoop(t, image, gwRunID, modelbroker.SecretRef("model"), "unused")
	gateway := h.captureOutcome(gwSessID, gwRespID)

	if gw.runnerErr != nil {
		t.Fatalf("runner side error = %v", gw.runnerErr)
	}
	if gateway.state != "completed" || gateway.state != subprocess.state {
		t.Fatalf("gateway state = %q, subprocess state = %q, want both completed", gateway.state, subprocess.state)
	}
	if gateway.output != subprocess.output {
		t.Fatalf("gateway output = %q, subprocess output = %q", gateway.output, subprocess.output)
	}
	if gateway.usage.InputTokens != subprocess.usage.InputTokens ||
		gateway.usage.OutputTokens != subprocess.usage.OutputTokens ||
		gateway.usage.TotalTokens != subprocess.usage.TotalTokens ||
		gateway.usage.ToolCalls != subprocess.usage.ToolCalls {
		t.Fatalf("gateway usage = %+v, subprocess usage = %+v", gateway.usage, subprocess.usage)
	}
	if strings.Join(gateway.eventTypes, ",") != strings.Join(subprocess.eventTypes, ",") {
		t.Fatalf("gateway event types = %v, subprocess event types = %v", gateway.eventTypes, subprocess.eventTypes)
	}
}

// TestNoSecretReachesEngineThroughGateway proves the provider credential never crosses the
// engine boundary: a sentinel secret redeemed by the model broker appears in no relayed
// frame and no redacted stderr the runner surfaces.
func TestNoSecretReachesEngineThroughGateway(t *testing.T) {
	const sentinel = "sk-live-GATEWAYPARITYSECRETSENTINEL0123456789"
	image := referenceEngineImage(t)
	h := newHarness(t)

	respID, sessID, runID := h.admit()
	gw := h.runGatewayLoop(t, image, runID, modelbroker.SecretRef("model"), sentinel)
	if gw.runnerErr != nil {
		t.Fatalf("runner side error = %v", gw.runnerErr)
	}
	if state, _ := h.response(respID); state != "completed" {
		t.Fatalf("response state = %q, want completed", state)
	}
	_ = sessID

	for _, frame := range gw.frames {
		if strings.Contains(frame, sentinel) {
			t.Fatalf("the provider secret leaked into a relayed frame: %s", frame)
		}
	}
	if strings.Contains(string(gw.stderr), sentinel) {
		t.Fatalf("the provider secret leaked into the redacted stderr: %q", gw.stderr)
	}
}

// gatewayLoopResult carries the runner side's outcome and the wire traffic observed, so a
// test can assert on both parity and secret hygiene.
type gatewayLoopResult struct {
	runnerErr error
	frames    []string // every relayed frame payload, both directions
	stderr    []byte   // the runner's redacted engine stderr
}

// runGatewayLoop drives one attempt end to end over the production topology: it stands up
// the gateway behind a mutually-authenticated TLS server, enrolls and supervises the
// reference engine in a real OCI sandbox on one goroutine, and runs the orchestrator over
// the gateway EngineDialer on another. It blocks until both sides finish.
func (h *harness) runGatewayLoop(t *testing.T, image, runID string, secretRef modelbroker.SecretRef, secret string) gatewayLoopResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	fixture := newParityGateway(t)
	var recorder frameRecorder

	runnerDone := make(chan gatewayLoopResult, 1)
	go func() {
		runnerDone <- fixture.supervise(ctx, t, image, &recorder)
	}()

	// The orchestrator drives the run through the gateway EngineDialer; the injected
	// secret is what the model broker redeems for the fake provider.
	models := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"fake": h.provider},
		Secrets:  modelbroker.StaticResolver{secretRef: secret},
	})
	orch := execution.NewOrchestrator(h.repo, fixture.gateway, models, toolbroker.New(toolbroker.ConformanceMathAdd()))

	desc := h.descriptor(runID, 1)
	desc.ImageDigest = image
	orchErr := orch.ExecuteAttempt(ctx, desc)

	result := <-runnerDone
	if orchErr != nil && result.runnerErr == nil {
		result.runnerErr = orchErr
	}
	return result
}

// frameRecorder captures every relayed frame payload for the secret-hygiene assertion.
type frameRecorder struct {
	mu     sync.Mutex
	frames []string
}

func (r *frameRecorder) record(payload string) {
	r.mu.Lock()
	r.frames = append(r.frames, payload)
	r.mu.Unlock()
}

func (r *frameRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.frames...)
}

// parityGateway wires the real runner gateway behind a mutually-authenticated TLS server.
type parityGateway struct {
	gateway    *execution.RunnerGateway
	ca         *parityCA
	enrollURL  string
	sessionURL string
}

func newParityGateway(t *testing.T) *parityGateway {
	t.Helper()
	ca := newParityCA(t)
	gateway := execution.NewRunnerGateway(ca, newParityTokens("parity-token-1"))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &http.Server{
		Handler: gateway.Routes(),
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{ca.serverCertificate(t)},
			ClientAuth:   tls.VerifyClientCertIfGiven,
			ClientCAs:    ca.pool,
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
	return &parityGateway{
		gateway:    gateway,
		ca:         ca,
		enrollURL:  "https://" + addr + "/v1/runner/enroll",
		sessionURL: "wss://" + addr + "/v1/runner/connect",
	}
}

// supervise enrolls, holds a lease, and supervises the reference engine in a real OCI
// sandbox — the runner half of cmd/runner, in-process. It relays engine frames to the
// gateway and injects the controller frames the gateway relays back, recording every
// payload for the secret-hygiene assertion, and reports the terminal outcome.
func (p *parityGateway) supervise(ctx context.Context, t *testing.T, image string, recorder *frameRecorder) gatewayLoopResult {
	identity, err := runner.Enroll(ctx, runner.BootstrapConfig{
		RunnerID:        parityRunnerID,
		RunnerDNS:       parityRunnerID + ".runners.palai.test",
		EnrollmentToken: "parity-token-1",
		EnrollmentURL:   p.enrollURL,
		ControllerCAs:   p.ca.pool,
		ControllerDNS:   parityControllerDNS,
		Now:             time.Now,
	})
	if err != nil {
		return gatewayLoopResult{runnerErr: err}
	}
	session := runner.Session{
		Identity:      identity,
		URL:           p.sessionURL,
		ControllerCAs: p.ca.pool,
		ControllerDNS: parityControllerDNS,
		Now:           time.Now,
	}
	lease, err := session.OpenLease(ctx)
	if err != nil {
		return gatewayLoopResult{runnerErr: err}
	}
	defer lease.Close()

	driver, err := oci.NewDockerInteractiveDriver()
	if err != nil {
		return gatewayLoopResult{runnerErr: err}
	}
	if closer, ok := driver.(interface{ Close() error }); ok {
		defer func() { _ = closer.Close() }()
	}

	inbound := make(chan contracts.EngineFrame)
	go func() {
		defer close(inbound)
		for {
			frame, err := lease.ReceiveControllerFrame(ctx)
			if err != nil {
				return
			}
			recorder.record(frameJSON(frame))
			select {
			case inbound <- frame:
			case <-ctx.Done():
				return
			}
		}
	}()
	sink := func(ctx context.Context, frame contracts.EngineFrame) error {
		recorder.record(frameJSON(frame))
		return lease.SendEngineFrame(ctx, frame)
	}

	result, streamErr := runner.NewStreamSupervisor(driver).Stream(ctx, runner.EngineRequest{
		ImageDigest: lease.Lease().ImageDigest,
		RunID:       lease.Lease().RunID,
		AttemptID:   lease.Lease().AttemptID,
		Fence:       lease.Lease().Fence,
		Limits:      lease.Lease().Limits,
	}, inbound, sink)

	// The controller already terminated the run on the run.terminal engine frame; the
	// lease.complete is best-effort (the gateway may have torn the socket down first).
	_ = lease.Complete(ctx, outcomeClass(streamErr), "sha256:parity")
	return gatewayLoopResult{runnerErr: streamErr, frames: recorder.snapshot(), stderr: result.Stderr}
}

// outcomeClass mirrors cmd/runner's mapping of a supervised outcome to the lease.complete
// class.
func outcomeClass(err error) string {
	switch {
	case err == nil:
		return "succeeded"
	case errors.Is(err, runner.ErrEngineTimeout):
		return "lost"
	default:
		return "failed"
	}
}

// parityCA is the in-test control-plane CA for the parity gateway.
type parityCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newParityCA(t *testing.T) *parityCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Palai parity CA"},
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
	return &parityCA{cert: cert, key: key, pool: pool}
}

func (ca *parityCA) serverCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate server key: %v", err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: parityControllerDNS},
		DNSNames:     []string{parityControllerDNS},
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
func (ca *parityCA) SignRunnerCertificate(publicKeyDER []byte, runnerDNS string) ([]byte, error) {
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
		NotAfter:     now.Add(90 * time.Second),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	return x509.CreateCertificate(rand.Reader, template, ca.cert, pub, ca.key)
}

// parityTokens implements execution.EnrollmentTokens.
type parityTokens struct {
	mu       sync.Mutex
	consumed map[string]bool
}

func newParityTokens(tokens ...string) *parityTokens {
	set := &parityTokens{consumed: map[string]bool{}}
	for _, token := range tokens {
		set.consumed[token] = false
	}
	return set
}

func (p *parityTokens) Consume(token string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	spent, known := p.consumed[token]
	if !known || spent {
		return errors.New("invalid enrollment token")
	}
	p.consumed[token] = true
	return nil
}
