package runner

// WHAT A REFUSED MACHINE IS TOLD, which is not the same question as what the gateway wrote.
//
// ‼️ THE SERVER'S REASON WAS DISCARDED AND IT COST THREE ROUNDS OF GUESSING ON A REAL ENROLMENT. The
// gateway writes a distinct sentence for every refusal — "declared posture is not this pool's", "this
// machine does not support the isolation mode this pool requires", "the claimed runner identity does not
// belong to this device key" — and this client printed `response.Status` alone, so all of them reached
// the operator as "409 Conflict". The person reading it is standing at the machine with a key file in
// their hand, and the difference between those three sentences is the difference between minting another
// key, choosing another pool, and re-imaging the box. Measured live 2026-08-06: with the body surfaced,
// the real reason appeared on the FIRST attempt.
//
// A server writing a good message and a client showing it are two properties, and only the second one is
// what an operator experiences.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// refusingGateway answers every enrolment with one status and one body, over TLS, and returns the
// bootstrap a machine would present to it.
//
// ‼️ IT MINTS ITS OWN CERTIFICATE WITH EXACTLY ONE SAN, because httptest's built-in one names TWO
// (example.com AND *.example.com) and verifyControllerIdentity refuses any certificate that speaks for
// more than the configured host — "a certificate that speaks for anyone else is one somebody else can
// present". Two drafts of this fixture died on that pin, which is the pin working.
func refusingGateway(t *testing.T, status int, body string) BootstrapConfig {
	t.Helper()
	const identity = "controller.test"

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: identity},
		DNSNames:     []string{identity},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:         true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, body, status)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}}}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return BootstrapConfig{
		RunnerID:  "runner-under-test",
		RunnerDNS: "runner-under-test.runner.palai.internal",
		// RunnerDNS is required by Enroll's own precondition — the first draft omitted it and the test
		// failed BEFORE the network, on a refusal that had nothing to do with what it measures.
		EnrollmentToken: "rpk_live_fixture",
		EnrollmentURL:   srv.URL + "/v1/runner/enroll",
		ControllerCAs:   pool,
		// The address dialled is 127.0.0.1 and the identity verified is this name: exactly the split
		// `palai enroll --server-name` exists for.
		ControllerDNS: identity,
		Now:           time.Now,
	}
}

// TestARefusedEnrolmentCarriesTheGatewaysOWNREASON is the property, over a real TLS round trip.
func TestARefusedEnrolmentCarriesTheGatewaysOWNREASON(t *testing.T) {
	const reason = "the claimed runner identity does not belong to this device key"
	cfg := refusingGateway(t, http.StatusConflict, reason)
	_, err := Enroll(context.Background(), cfg)
	if err == nil {
		t.Fatal("a 409 enrolled the machine")
	}
	if !strings.Contains(err.Error(), reason) {
		t.Fatalf("the refusal reached the operator as %q — the gateway's own sentence is missing, and every "+
			"409 then reads the same whether the fix is another key, another pool, or a re-image", err)
	}
	// The status stays too: "409" is what an operator greps for and what a runbook names.
	if !strings.Contains(err.Error(), "409") {
		t.Fatalf("the status was dropped in favour of the body: %v", err)
	}
}

// TestARefusalWithNoBodyStillReportsItsStatus keeps the change from making a bodiless refusal worse than
// it was: a gateway that writes nothing must still produce a usable error rather than a dangling colon.
func TestARefusalWithNoBodyStillReportsItsStatus(t *testing.T) {
	cfg := refusingGateway(t, http.StatusUnauthorized, "")

	_, err := Enroll(context.Background(), cfg)
	if err == nil {
		t.Fatal("a 401 enrolled the machine")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("a bodiless refusal lost its status: %v", err)
	}
	if strings.HasSuffix(strings.TrimSpace(err.Error()), ":") {
		t.Fatalf("a bodiless refusal left a dangling separator: %q", err)
	}
}
