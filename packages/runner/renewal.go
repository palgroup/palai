package runner

import (
	"context"
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

// RenewConfig is the input to a certificate renewal: the renew endpoint and the controller
// trust anchor and exact DNS identity the runner pins on every outbound connection. The
// current identity (its client certificate and private key) authenticates the renewal — the
// one-use bootstrap token is never presented again.
type RenewConfig struct {
	RenewURL      string
	ControllerCAs *x509.CertPool
	ControllerDNS string
	Now           func() time.Time
}

// Renew rolls the runner's client certificate forward over its existing mutually
// authenticated identity. It presents the current certificate to the renew endpoint (an
// expired certificate cannot complete the mTLS handshake, so renewal must happen before
// expiry — the serve loop drives it at ~80% of the TTL), keeps the same private key, and
// returns the identity with the freshly issued certificate. No enrollment token is involved.
func Renew(ctx context.Context, current Identity, config RenewConfig) (Identity, error) {
	if current.Certificate.Leaf == nil || current.Certificate.PrivateKey == nil ||
		config.ControllerCAs == nil || config.ControllerDNS == "" || config.Now == nil {
		return Identity{}, errors.New("renewal requires the current identity, controller trust, DNS and clock")
	}
	if !strings.HasPrefix(config.RenewURL, "https://") {
		return Identity{}, errors.New("renew URL must be outbound https")
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, config.RenewURL, nil)
	if err != nil {
		return Identity{}, fmt.Errorf("build renew request: %w", err)
	}

	transport := &http.Transport{TLSClientConfig: renewTLS(current, config), Proxy: nil}
	defer transport.CloseIdleConnections()
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		return Identity{}, fmt.Errorf("renew with control plane: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Identity{}, fmt.Errorf("renewal rejected: %s", response.Status)
	}

	var decoded struct {
		Certificate string `json:"certificate"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64*1024)).Decode(&decoded); err != nil {
		return Identity{}, fmt.Errorf("decode renewal response: %w", err)
	}
	certDER, err := base64.StdEncoding.DecodeString(decoded.Certificate)
	if err != nil {
		return Identity{}, fmt.Errorf("decode renewed certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(certDER)
	if err != nil {
		return Identity{}, fmt.Errorf("parse renewed certificate: %w", err)
	}
	if !leaf.NotAfter.After(config.Now()) {
		return Identity{}, errors.New("control plane renewed an already-expired certificate")
	}

	return Identity{
		RunnerID:    current.RunnerID,
		Certificate: tls.Certificate{Certificate: [][]byte{certDER}, PrivateKey: current.Certificate.PrivateKey, Leaf: leaf},
		NotAfter:    leaf.NotAfter,
	}, nil
}

// renewTLS is the outbound, mutually-authenticated TLS config for renewal: the runner
// presents its CURRENT certificate (unlike enrollment, which is certless) and pins the
// controller CA and exact DNS identity, exactly as the lease session does.
func renewTLS(current Identity, config RenewConfig) *tls.Config {
	now := config.Now
	tlsConfig := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{current.Certificate},
		RootCAs:      config.ControllerCAs.Clone(),
		ServerName:   config.ControllerDNS,
		Time:         func() time.Time { return now() },
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
