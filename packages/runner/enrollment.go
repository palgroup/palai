package runner

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Identity is the runner's short-lived enrolled identity: the run-scoped client
// certificate and the private key that never leaves the runner. It deliberately
// carries no enrollment token — the one-use bootstrap token is spent during Enroll
// and discarded, never retained as a credential.
type Identity struct {
	RunnerID    string
	Certificate tls.Certificate
	NotAfter    time.Time
}

// BootstrapConfig is the one-use enrollment input. EnrollmentToken is presented once
// and never stored; ControllerCAs and ControllerDNS are the trust anchor and exact
// server identity the runner requires for every outbound connection.
type BootstrapConfig struct {
	RunnerID        string
	RunnerDNS       string
	EnrollmentToken string
	EnrollmentURL   string
	ControllerCAs   *x509.CertPool
	ControllerDNS   string
	Now             func() time.Time
	// Posture is what this machine SAYS it is: "sandboxed-linux" (a container runner) or
	// "unsandboxed-host" (a rented Mac). The control plane compares it with the pool's and refuses the
	// enrolment on a disagreement (E24 T2) — it does NOT verify it, and cannot.
	//
	// Empty declares nothing, which is what every runner built before E24 sends and is why this field
	// is omitempty below: an undeclared machine's request body is byte-identical to the one this
	// package has always sent, so no deployment observes the field's existence.
	Posture string
	// PoolID is the pool this machine was CONFIGURED to join (E24 T3). It is a declaration, not an
	// authorisation: the pool a certificate is actually minted into comes from the presented KEY, and a
	// declaration that disagrees with the key REFUSES the enrolment instead of being silently widened
	// or silently overridden.
	//
	// Why declare it at all, when the key already decides: because the realistic operator mistake is
	// pasting the wrong pool's key onto a machine, and posture comparison (T2) cannot catch it when
	// both pools have the same posture. Saying where the machine believes it belongs is what turns that
	// into a refusal at the door instead of a machine quietly running another pool's work.
	//
	// Empty declares nothing and inherits the key's pool, which is what every runner built before E24
	// sends — hence omitempty, so an unconfigured machine's request body is unchanged to the byte.
	PoolID string
}

type enrollmentRequest struct {
	RunnerID  string `json:"runner_id"`
	PublicKey string `json:"public_key"`
	Posture   string `json:"posture,omitempty"`
	PoolID    string `json:"pool_id,omitempty"`
}

type enrollmentResponse struct {
	Certificate string `json:"certificate"`
	// RunnerID is the id the CONTROL PLANE minted (E24 T1). The runner_id this runner sent is a label
	// — the compose entrypoint hardcodes it, so it is not unique across machines — and the server's is
	// the one that names this machine everywhere else. A control plane too old to send it leaves this
	// empty and the runner keeps its own name, which is what makes an old control plane still work.
	RunnerID string `json:"runner_id"`
}

// Enroll exchanges the one-use bootstrap token for a short-lived client identity over
// an outbound, server-authenticated TLS connection. The runner generates its own
// keypair locally; only the public key is sent, and the returned certificate plus that
// local key form the identity. The token is used once and discarded.
func Enroll(ctx context.Context, config BootstrapConfig) (Identity, error) {
	if config.RunnerID == "" || config.RunnerDNS == "" || config.EnrollmentToken == "" ||
		config.ControllerCAs == nil || config.ControllerDNS == "" || config.Now == nil {
		return Identity{}, errors.New("enrollment requires runner identity, one-use token, controller trust, DNS and clock")
	}
	if !strings.HasPrefix(config.EnrollmentURL, "https://") {
		return Identity{}, errors.New("enrollment URL must be outbound https")
	}

	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Identity{}, fmt.Errorf("generate runner key: %w", err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		return Identity{}, fmt.Errorf("marshal runner public key: %w", err)
	}

	// ponytail: no CSR proof-of-possession — the one-use token over server-authenticated
	// TLS is the enrollment boundary for the local-live proof; add a PoP CSR when the
	// enrollment PKI gate lands (ADR-0003 defers PKI).
	body, err := json.Marshal(enrollmentRequest{
		RunnerID:  config.RunnerID,
		PublicKey: base64.StdEncoding.EncodeToString(publicDER),
		Posture:   config.Posture,
		PoolID:    config.PoolID,
	})
	if err != nil {
		return Identity{}, fmt.Errorf("encode enrollment request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, config.EnrollmentURL, bytes.NewReader(body))
	if err != nil {
		return Identity{}, fmt.Errorf("build enrollment request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+config.EnrollmentToken)
	request.Header.Set("Content-Type", "application/json")

	transport := &http.Transport{TLSClientConfig: enrollmentTLS(config), Proxy: nil}
	defer transport.CloseIdleConnections()
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		return Identity{}, fmt.Errorf("enroll with control plane: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Identity{}, fmt.Errorf("enrollment rejected: %s", response.Status)
	}

	var decoded enrollmentResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 64*1024)).Decode(&decoded); err != nil {
		return Identity{}, fmt.Errorf("decode enrollment response: %w", err)
	}
	certDER, err := base64.StdEncoding.DecodeString(decoded.Certificate)
	if err != nil {
		return Identity{}, fmt.Errorf("decode issued certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(certDER)
	if err != nil {
		return Identity{}, fmt.Errorf("parse issued certificate: %w", err)
	}
	if !leaf.NotAfter.After(config.Now()) {
		return Identity{}, errors.New("control plane issued an already-expired certificate")
	}

	// Adopt the server's id when it sent one. Falling back to the requested name is not a courtesy —
	// it is what lets this runner enroll against a control plane built before E24 T1.
	runnerID := decoded.RunnerID
	if runnerID == "" {
		runnerID = config.RunnerID
	}
	return Identity{
		RunnerID:    runnerID,
		Certificate: tls.Certificate{Certificate: [][]byte{certDER}, PrivateKey: privateKey, Leaf: leaf},
		NotAfter:    leaf.NotAfter,
	}, nil
}

// enrollmentTLS is the outbound, server-authenticated TLS config for enrollment: the
// runner presents no client certificate (it has none yet — the token authenticates it)
// but pins the controller CA and exact DNS identity.
func enrollmentTLS(config BootstrapConfig) *tls.Config {
	now := config.Now
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS13,
		RootCAs:    config.ControllerCAs.Clone(),
		ServerName: config.ControllerDNS,
		Time:       func() time.Time { return now() },
	}
	tlsConfig.VerifyConnection = func(state tls.ConnectionState) error {
		if len(state.VerifiedChains) == 0 || len(state.VerifiedChains[0]) == 0 {
			return errors.New("controller certificate chain was not verified")
		}
		leaf := state.VerifiedChains[0][0]
		if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != config.ControllerDNS {
			return errors.New("controller certificate DNS identity is not exact")
		}
		return nil
	}
	return tlsConfig
}
