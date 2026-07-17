// Package engine_test proves the runner engine boundary — one-use enrollment, an
// outbound short-lived-identity session, handshake version policy, and frame
// deduplication — Docker-free against an in-test control-plane stub. The stub is
// the control-plane counterpart the production runner-gateway task will replace; it
// exists only to drive real TLS handshakes and real runner.v1 wire messages
// (contracts.RunnerMessage) so `make verify` can assert these semantics without a
// container runtime.
package engine_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
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

	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/runner"
)

const (
	controllerDNS = "controller.runner.conformance.palai.test"
	runnerID      = "runner-01"
	runnerDNS     = "runner-01.runners.conformance.palai.test"
)

// testCA is the control-plane certificate authority: it authenticates itself to the
// runner, signs the short-lived client certificate enrollment issues, and verifies
// runner client certificates on the session. It is a test construct; the runner
// never signs anything.
type testCA struct {
	certificate *x509.Certificate
	key         *ecdsa.PrivateKey
	pool        *x509.CertPool
}

func newTestCA(t *testing.T, now time.Time) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          serial(t),
		Subject:               pkix.Name{CommonName: "Palai runner conformance CA"},
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
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(certificate)
	return &testCA{certificate: certificate, key: key, pool: pool}
}

// issueServer mints the controller's own TLS server certificate over the given SANs.
func (ca *testCA) issueServer(t *testing.T, now time.Time, dnsNames ...string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate server key: %v", err)
	}
	der := ca.sign(t, &key.PublicKey, x509.ExtKeyUsageServerAuth, now.Add(-time.Minute), now.Add(time.Hour), dnsNames...)
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse server certificate: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

// signClient signs an enrolling runner's public key into a short-lived client
// certificate — the wire behaviour of the enrollment endpoint.
func (ca *testCA) signClient(pub *ecdsa.PublicKey, dnsName string, now time.Time) ([]byte, error) {
	return ca.signRaw(pub, x509.ExtKeyUsageClientAuth, now.Add(-time.Second), now.Add(45*time.Second), dnsName)
}

func (ca *testCA) sign(t *testing.T, pub *ecdsa.PublicKey, usage x509.ExtKeyUsage, notBefore, notAfter time.Time, dnsNames ...string) []byte {
	t.Helper()
	der, err := ca.signRaw(pub, usage, notBefore, notAfter, dnsNames...)
	if err != nil {
		t.Fatalf("sign certificate: %v", err)
	}
	return der
}

func (ca *testCA) signRaw(pub *ecdsa.PublicKey, usage x509.ExtKeyUsage, notBefore, notAfter time.Time, dnsNames ...string) ([]byte, error) {
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: dnsNames[0]},
		DNSNames:     dnsNames,
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
	}
	return x509.CreateCertificate(rand.Reader, template, ca.certificate, pub, ca.key)
}

func serial(t *testing.T) *big.Int {
	t.Helper()
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	value, err := rand.Int(rand.Reader, limit)
	if err != nil {
		t.Fatalf("serial: %v", err)
	}
	return value
}

// stubOptions configures the control-plane stub's handshake behaviour so a single
// harness can drive the accept, timeout, and version-mismatch cases.
type stubOptions struct {
	now             time.Time
	offerProtocol   string   // protocol value the lease.offer carries (default runner.v1)
	subprotocol     string   // websocket subprotocol the connect endpoint offers (default runner.v1)
	silentHandshake bool     // accept the socket but never send a lease.offer
	relay           bool     // after the offer, run one engine.frame/controller.frame/lease.complete exchange
	serverSANs      []string // server certificate SANs (default [controllerDNS])
}

// relayComplete is the terminal outcome a relayed lease.complete carried, surfaced to
// the test asserting the runner reported it.
type relayComplete struct {
	outcome string
	digest  string
}

// stubControlPlane is an outbound-only counterpart: it serves the enrollment
// endpoint (bearer, one-use token) and the mutually authenticated session endpoint.
type stubControlPlane struct {
	ca       *testCA
	server   *http.Server
	listener net.Listener
	options  stubOptions

	mu             sync.Mutex
	consumedTokens map[string]bool
	relayComplete  chan relayComplete
}

func newStubControlPlane(t *testing.T, tokens []string, options stubOptions) *stubControlPlane {
	t.Helper()
	if options.offerProtocol == "" {
		options.offerProtocol = "runner.v1"
	}
	if options.subprotocol == "" {
		options.subprotocol = "runner.v1"
	}
	if len(options.serverSANs) == 0 {
		options.serverSANs = []string{controllerDNS}
	}
	ca := newTestCA(t, options.now)
	stub := &stubControlPlane{ca: ca, options: options, consumedTokens: map[string]bool{}, relayComplete: make(chan relayComplete, 1)}
	for _, token := range tokens {
		stub.consumedTokens[token] = false
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	stub.listener = listener

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/runner/enroll", stub.handleEnroll)
	mux.HandleFunc("/v1/runner/connect", stub.handleConnect)

	// VerifyClientCertIfGiven, not RequireAndVerify: the enrollment endpoint must accept
	// the certless bootstrap request. The connect handler enforces the client identity
	// itself (see handleConnect), so a session that presents no certificate is rejected.
	serverCert := ca.issueServer(t, options.now, options.serverSANs...)
	stub.server = &http.Server{
		Handler: mux,
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{serverCert},
			ClientAuth:   tls.VerifyClientCertIfGiven,
			ClientCAs:    ca.pool,
			Time:         func() time.Time { return options.now },
		},
		ErrorLog: log.New(io.Discard, "", 0),
	}
	go func() { _ = stub.server.ServeTLS(listener, "", "") }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = stub.server.Shutdown(ctx)
	})
	return stub
}

func (s *stubControlPlane) enrollURL() string {
	return "https://" + s.listener.Addr().String() + "/v1/runner/enroll"
}
func (s *stubControlPlane) sessionURL() string {
	return "wss://" + s.listener.Addr().String() + "/v1/runner/connect"
}

func (s *stubControlPlane) bootstrap(token string) runner.BootstrapConfig {
	return runner.BootstrapConfig{
		RunnerID:        runnerID,
		RunnerDNS:       runnerDNS,
		EnrollmentToken: token,
		EnrollmentURL:   s.enrollURL(),
		ControllerCAs:   s.ca.pool,
		ControllerDNS:   controllerDNS,
		Now:             func() time.Time { return s.options.now },
	}
}

func (s *stubControlPlane) session(identity runner.Identity) runner.Session {
	return runner.Session{
		Identity:      identity,
		URL:           s.sessionURL(),
		ControllerCAs: s.ca.pool,
		ControllerDNS: controllerDNS,
		Now:           func() time.Time { return s.options.now },
	}
}

// issueIdentity mints a valid short-lived runner identity directly from the stub's CA,
// bypassing the enrollment exchange. It lets a test isolate the session's server-identity
// check when enrollment against the same (deliberately bad) server would itself fail.
func (s *stubControlPlane) issueIdentity(t *testing.T) runner.Identity {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate identity key: %v", err)
	}
	der, err := s.ca.signClient(&key.PublicKey, runnerDNS, s.options.now)
	if err != nil {
		t.Fatalf("sign identity: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse identity: %v", err)
	}
	return runner.Identity{
		RunnerID:    runnerID,
		Certificate: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf},
		NotAfter:    leaf.NotAfter,
	}
}

type enrollRequest struct {
	RunnerID  string `json:"runner_id"`
	PublicKey string `json:"public_key"`
}

type enrollResponse struct {
	Certificate string `json:"certificate"`
}

func (s *stubControlPlane) handleEnroll(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.mu.Lock()
	consumed, known := s.consumedTokens[token]
	if known && !consumed {
		s.consumedTokens[token] = true // one-use: the token is spent on first success
	}
	s.mu.Unlock()
	if token == "" || !known || consumed {
		http.Error(w, "invalid enrollment token", http.StatusUnauthorized)
		return
	}
	var request enrollRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.RunnerID != runnerID {
		http.Error(w, "invalid enrollment request", http.StatusBadRequest)
		return
	}
	der, err := base64.StdEncoding.DecodeString(request.PublicKey)
	if err != nil {
		http.Error(w, "invalid public key", http.StatusBadRequest)
		return
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		http.Error(w, "invalid public key", http.StatusBadRequest)
		return
	}
	pub, ok := parsed.(*ecdsa.PublicKey)
	if !ok {
		http.Error(w, "unsupported public key", http.StatusBadRequest)
		return
	}
	certDER, err := s.ca.signClient(pub, runnerDNS, s.options.now)
	if err != nil {
		http.Error(w, "sign", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(enrollResponse{Certificate: base64.StdEncoding.EncodeToString(certDER)})
}

func (s *stubControlPlane) handleConnect(w http.ResponseWriter, r *http.Request) {
	// The session must present its short-lived client identity: the server TLS accepts a
	// certless handshake (for enrollment), so the leasing endpoint asserts the verified
	// client chain here. Without this, a session that never sent a certificate would still
	// be served.
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		http.Error(w, "runner client certificate required", http.StatusUnauthorized)
		return
	}
	connection, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{s.options.subprotocol}})
	if err != nil {
		return
	}
	defer connection.CloseNow()
	connection.SetReadLimit(8 * 1024)
	if connection.Subprotocol() != s.options.subprotocol {
		_ = connection.Close(websocket.StatusPolicyViolation, "subprotocol")
		return
	}
	if _, _, err := connection.Read(r.Context()); err != nil { // consume runner.hello
		return
	}
	if s.options.silentHandshake {
		<-r.Context().Done() // never offer a lease: the runner must time out
		return
	}
	offer := contracts.RunnerMessage{
		Protocol:  s.options.offerProtocol,
		Type:      "lease.offer",
		Time:      s.options.now.Format(time.RFC3339),
		LeaseID:   "lease_conformance1",
		RunID:     contracts.RunID("run_conformance1"),
		AttemptID: contracts.AttemptID("att_conformance1"),
		Fence:     7,
		Data: map[string]any{
			"image_digest": "sha256:" + strings.Repeat("a", 64),
			"limits": map[string]any{
				"wall_time_ms":      5000,
				"max_stdout_bytes":  65536,
				"max_stderr_bytes":  16384,
				"max_frame_bytes":   8192,
				"max_memory_bytes":  67108864,
				"max_process_count": 16,
			},
		},
	}
	payload, err := json.Marshal(offer)
	if err != nil {
		return
	}
	_ = connection.Write(r.Context(), websocket.MessageText, payload)

	if s.options.relay {
		s.runRelay(r.Context(), connection)
	}
}

// runRelay is the control-plane counterpart of a LeaseSession over an open lease: it
// reads one relayed engine.frame, echoes a controller.frame, and reports the lease.complete
// outcome the runner sent. It proves the runner's frame relay wire without a container.
func (s *stubControlPlane) runRelay(ctx context.Context, connection *websocket.Conn) {
	_, enginePayload, err := connection.Read(ctx)
	if err != nil {
		return
	}
	var engineMsg contracts.RunnerMessage
	if err := json.Unmarshal(enginePayload, &engineMsg); err != nil || engineMsg.Type != "engine.frame" {
		return
	}

	controllerMsg := contracts.RunnerMessage{
		Protocol:  "runner.v1",
		Type:      "controller.frame",
		Time:      s.options.now.Format(time.RFC3339),
		LeaseID:   engineMsg.LeaseID,
		RunID:     engineMsg.RunID,
		AttemptID: engineMsg.AttemptID,
		Data: map[string]any{"frame": map[string]any{
			"protocol": "engine.v1",
			"id":       "frm_controller1",
			"type":     "model.result",
			"sequence": 1,
			"time":     s.options.now.Format(time.RFC3339),
			"data":     map[string]any{"model_request_id": "mreq_1"},
		}},
	}
	controllerPayload, err := json.Marshal(controllerMsg)
	if err != nil {
		return
	}
	if err := connection.Write(ctx, websocket.MessageText, controllerPayload); err != nil {
		return
	}

	_, completePayload, err := connection.Read(ctx)
	if err != nil {
		return
	}
	var completeMsg contracts.RunnerMessage
	if err := json.Unmarshal(completePayload, &completeMsg); err != nil || completeMsg.Type != "lease.complete" {
		return
	}
	outcome, _ := completeMsg.Data["outcome"].(string)
	digest, _ := completeMsg.Data["stderr_digest"].(string)
	s.relayComplete <- relayComplete{outcome: outcome, digest: digest}
}
