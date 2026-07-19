package runner

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"
)

// TestOpenLeaseBoundsDialWhenServerNeverHandshakes proves the outbound dial + runner.v1
// handshake is bounded by an internal, attempt-scoped deadline that is independent of the
// parent context: a control plane that accepts the TCP connection but never completes the
// TLS/WebSocket handshake must not wedge the runner's serve loop forever. OpenLease returns
// a bounded error well within the (here never-expiring) parent lifetime, so cmd/runner logs
// it and re-parks rather than hanging on a dead gateway.
func TestOpenLeaseBoundsDialWhenServerNeverHandshakes(t *testing.T) {
	// A raw TCP listener that accepts connections and holds them without ever speaking
	// TLS: the dial blocks in the handshake until the session's internal deadline fires.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = conn.Close() }) // hold it, never respond
		}
	}()

	session := Session{
		Identity:             selfSignedIdentity(t),
		URL:                  "wss://" + ln.Addr().String(),
		ControllerCAs:        x509.NewCertPool(),
		ControllerDNS:        "controller.test",
		Now:                  time.Now,
		DialHandshakeTimeout: 500 * time.Millisecond, // injected short bound
	}

	// The parent context never expires; only the session's internal dial deadline may end
	// the blocked handshake. Without the bound OpenLease hangs forever on background ctx.
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := session.OpenLease(context.Background())
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("OpenLease returned no error against a server that never handshakes")
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("OpenLease took %s to return; the dial was not bounded", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OpenLease did not return; the blocked dial is unbounded (the serve loop would wedge)")
	}
}

// selfSignedIdentity builds a throwaway client identity with a parsed leaf — enough to
// satisfy openConnection's identity precondition. The test server never validates it (it
// never responds), so a self-signed cert is sufficient.
func selfSignedIdentity(t *testing.T) Identity {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "runner.test"},
		DNSNames:     []string{"runner.test"},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return Identity{
		RunnerID:    "runner.test",
		Certificate: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf},
		NotAfter:    leaf.NotAfter,
	}
}
