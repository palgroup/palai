package stack

// The production doctor's transports need a live production stack; its VERDICTS do not, and they
// are where a wrong answer hides. These prove the ones this file introduces — the edge-certificate
// comparison, the mutual-TLS rejection reading, and the two parsers standing between `psql -tA`
// output and the shared threshold functions. Untagged: they ride `go test ./cmd/...`.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/storage"
)

// selfSigned mints a certificate with the given names and lifetime — enough for the comparisons
// edgeCertCheck makes (it compares certificates and reads NotAfter; it never builds a chain).
func selfSigned(t *testing.T, cn string, dnsNames []string, notAfter time.Time) (*x509.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		DNSNames:     dnsNames,
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// The trap this check exists for: `docker compose up -d edge` sees no config change and leaves the
// container alone, so a replaced certificate sits on disk while the edge keeps serving the old one
// — no error anywhere (measured 2026-07-29, cloud-smoke-report.md, Bulgu 4).
func TestEdgeCertCheckCatchesAnUnreloadedSwap(t *testing.T) {
	now := time.Now()
	onDisk, _ := selfSigned(t, "palai.example.com", []string{"palai.example.com"}, now.AddDate(1, 0, 0))
	stillServed, _ := selfSigned(t, "control-plane", []string{"control-plane"}, now.AddDate(1, 0, 0))

	c := edgeCertCheck(onDisk, stillServed, "/srv/palai/ca/edge.crt", now)
	if c.Status != statusFail {
		t.Fatalf("a served certificate that differs from the configured file must FAIL, got %q (%s)", c.Status, c.Detail)
	}
	if !strings.Contains(c.Detail, "restart edge") {
		t.Fatalf("the detail must name the fix (`restart edge` reloads it), got %q", c.Detail)
	}
	// A RENEWED certificate carries the same names as the one it replaces, so names alone cannot
	// tell the operator the two differ — the serial must be in the message or it reads like a bug
	// in the check. Same subject, same SANs, different certificate:
	renewed, _ := selfSigned(t, "palai.example.com", []string{"palai.example.com"}, now.AddDate(2, 0, 0))
	sameNames := edgeCertCheck(renewed, onDisk, "/srv/palai/ca/edge.crt", now)
	if sameNames.Status != statusFail {
		t.Fatalf("a renewed-but-unloaded certificate must FAIL, got %q (%s)", sameNames.Status, sameNames.Detail)
	}
	for _, serial := range []string{renewed.SerialNumber.String(), onDisk.SerialNumber.String()} {
		if !strings.Contains(sameNames.Detail, serial) {
			t.Fatalf("the detail must carry both serials to distinguish two certificates with identical names, got %q", sameNames.Detail)
		}
	}
	// The same certificate served and configured → green, and the detail names it.
	if c := edgeCertCheck(onDisk, onDisk, "/srv/palai/ca/edge.crt", now); c.Status != statusOK {
		t.Fatalf("the configured certificate being served must be ok, got %q (%s)", c.Status, c.Detail)
	} else if !strings.Contains(c.Detail, "palai.example.com") {
		t.Fatalf("the detail must name the certificate's identity, got %q", c.Detail)
	}
	// Expired, and matching, is still a fault: nothing else in doctor would say so.
	expired, _ := selfSigned(t, "palai.example.com", []string{"palai.example.com"}, now.Add(-24*time.Hour))
	if c := edgeCertCheck(expired, expired, "/srv/palai/ca/edge.crt", now); c.Status != statusFail {
		t.Fatalf("an expired edge certificate must FAIL, got %q (%s)", c.Status, c.Detail)
	}
	if c := edgeCertCheck(onDisk, nil, "/srv/palai/ca/edge.crt", now); c.Status != statusFail {
		t.Fatalf("no served certificate must FAIL, got %q (%s)", c.Status, c.Detail)
	}
}

// leafFromPEMFile is what makes the check read the operator's file rather than assume a path, and
// certServerName is what lets the probe verify a real-domain certificate with FULL verification
// (it asks for the name the certificate carries instead of relaxing the check).
func TestLeafFromPEMFileAndServerName(t *testing.T) {
	want, pemBytes := selfSigned(t, "palai.example.com", []string{"palai.example.com", "www.example.com"}, time.Now().AddDate(1, 0, 0))
	path := filepath.Join(t.TempDir(), "edge.crt")
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := leafFromPEMFile(path)
	if err != nil {
		t.Fatalf("read the edge certificate: %v", err)
	}
	if !got.Equal(want) {
		t.Fatal("leafFromPEMFile returned a different certificate than the file holds")
	}
	if name := certServerName(got); name != "palai.example.com" {
		t.Fatalf("certServerName = %q, want the first DNS SAN", name)
	}
	// No SANs at all → the Common Name, which is what an old certificate would carry.
	noSAN, _ := selfSigned(t, "legacy-host", nil, time.Now().AddDate(1, 0, 0))
	if name := certServerName(noSAN); name != "legacy-host" {
		t.Fatalf("certServerName on a SAN-less certificate = %q, want the CN", name)
	}
	if _, err := leafFromPEMFile(filepath.Join(t.TempDir(), "absent.crt")); err == nil {
		t.Fatal("a missing certificate file must error, not return a zero certificate")
	}
}

// runnerRejectCheck reads a mutual-TLS security property off a raw response. A green here must mean
// "the gateway answered 401"; anything else — including a handshake that never produced a status —
// must NOT be green, or doctor would report mTLS enforced on a stack where it is not.
func TestRunnerRejectCheckIsGreenOnlyOn401(t *testing.T) {
	const okProbe = "\n" + probeExitMarker + "0\n"
	got := runnerRejectCheck("HTTP/1.1 401 Unauthorized\r\nContent-Length: 0\r\n"+okProbe, nil)
	if got.Status != statusOK {
		t.Fatalf("a 401 must be ok, got %q (%s)", got.Status, got.Detail)
	}
	for _, out := range []string{
		"HTTP/1.1 101 Switching Protocols\r\n\r\n",
		"HTTP/1.1 200 OK\r\n\r\n",
		"HTTP/1.1 500 Internal Server Error\r\n\r\n",
	} {
		if c := runnerRejectCheck(out+okProbe, nil); c.Status != statusFail {
			t.Fatalf("%q must FAIL the mutual-TLS rejection check, got %q (%s)", strings.TrimSpace(out), c.Status, c.Detail)
		}
	}
	// No status line at all is a fault, not a pass: the probe proved nothing.
	if c := runnerRejectCheck("connect: connection refused"+okProbe, nil); c.Status != statusFail {
		t.Fatalf("a probe with no HTTP status must FAIL, got %q (%s)", c.Status, c.Detail)
	}
	// A handshake that never completed cannot answer this question — n/a, NOT ok, and the `runner`
	// check carries the real failure.
	// A handshake that failed: the probe reports a non-zero exit, so the rejection question is
	// unanswerable (n/a) while the `runner` check carries the real failure with openssl's reason.
	const badProbe = "verify error:num=19:self-signed certificate\n" + probeExitMarker + "1\n"
	if c := runnerRejectCheck(badProbe, nil); c.Status != statusNA {
		t.Fatalf("a failed handshake must be n/a here, got %q (%s)", c.Status, c.Detail)
	}
	if c := runnerGatewayCheck(badProbe, nil); c.Status != statusFail {
		t.Fatalf("a failed handshake must FAIL the runner check, got %q (%s)", c.Status, c.Detail)
	} else if !strings.Contains(c.Detail, "verify error") {
		t.Fatalf("the runner failure must carry openssl's REASON, not the docker command: %q", c.Detail)
	}
	// The trap this marker exists for: a probe that produced NO verdict (the container died, the
	// output was truncated) must not be read as a successful handshake.
	if c := runnerGatewayCheck("HTTP/1.1 401 Unauthorized", nil); c.Status != statusFail {
		t.Fatalf("a probe with no exit marker must FAIL, not pass on the presence of a 401: %q (%s)", c.Status, c.Detail)
	}
	if c := runnerRejectCheck("HTTP/1.1 401 Unauthorized", nil); c.Status == statusOK {
		t.Fatalf("a 401 with no probe verdict must not be green: %q (%s)", c.Status, c.Detail)
	}
	if c := runnerGatewayCheck("", os.ErrDeadlineExceeded); c.Status != statusFail {
		t.Fatalf("a docker-level failure must FAIL the runner check, got %q (%s)", c.Status, c.Detail)
	}
}

// An n/a must never be counted as green in the verdict OR silently swallowed: the JSON carries it
// and the overall OK reflects only real failures.
func TestNotApplicableIsNeitherGreenNorAFailure(t *testing.T) {
	na := unavailable("cannot be measured here")
	if na.Status == statusOK {
		t.Fatal("an unmeasurable check must not report the green status")
	}
	if na.Status != statusNA || na.Detail == "" {
		t.Fatalf("an unmeasurable check must be %q with a reason, got %+v", statusNA, na)
	}
}

// oneLine feeds the SAME named statements the local doctor and the /metrics collector run into
// `psql -tAq` over stdin. If it dropped a line or kept a `--` comment, the statement would either
// change meaning or comment out its own tail — so prove it on the real shipped SQL.
func TestOneLineFlattensTheShippedMetricsQueries(t *testing.T) {
	for _, name := range []string{"MetricQueueReady", "MetricWebhookDeliveryStates"} {
		got := oneLine(storage.Query(name))
		if strings.Contains(got, "\n") {
			t.Errorf("%s still spans lines: %q", name, got)
		}
		if strings.Contains(got, "--") {
			t.Errorf("%s still carries a `--` comment, which would comment out its own tail: %q", name, got)
		}
		if strings.HasSuffix(got, ";") {
			t.Errorf("%s keeps a trailing semicolon; pgQueryScalar appends its own: %q", name, got)
		}
		if !strings.Contains(strings.ToUpper(got), "SELECT") {
			t.Errorf("%s lost its SELECT: %q", name, got)
		}
	}
	// A comment-only line between two clauses must not glue them together.
	if got := oneLine("SELECT 1\n-- a note\nFROM t;"); got != "SELECT 1 FROM t" {
		t.Fatalf("oneLine = %q, want %q", got, "SELECT 1 FROM t")
	}
}

func TestFirstStatusLine(t *testing.T) {
	if got, found := firstStatusLine("depth=0 ok\nHTTP/1.1 401 Unauthorized\r\nX: y"); !found || got != "HTTP/1.1 401 Unauthorized" {
		t.Fatalf("firstStatusLine = %q,%v — it must skip openssl's own output and find the response line", got, found)
	}
	if _, found := firstStatusLine("no status here"); found {
		t.Fatal("firstStatusLine must report not-found rather than inventing a status")
	}
}

// newProdStack refuses rather than silently checking the wrong stack: without the project it would
// docker-exec into containers that do not exist and report a broken stack that is fine.
func TestNewProdStackRequiresTheStackIdentity(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "production.env")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, tc := range []struct{ name, body string }{
		{"no project", "PALAI_HOME=/srv/palai\nPALAI_EDGE_PORT=443\n"},
		{"no home", "PALAI_COMPOSE_PROJECT=palai-prod\nPALAI_EDGE_PORT=443\n"},
		{"no edge port", "PALAI_HOME=/srv/palai\nPALAI_COMPOSE_PROJECT=palai-prod\n"},
		{"edge port not a port", "PALAI_HOME=/srv/palai\nPALAI_COMPOSE_PROJECT=palai-prod\nPALAI_EDGE_PORT=https\n"},
	} {
		if _, err := newProdStack(write(tc.body)); err == nil {
			t.Errorf("%s: newProdStack must refuse, not check some other stack", tc.name)
		}
	}
	s, err := newProdStack(write("PALAI_HOME=/srv/palai\nPALAI_COMPOSE_PROJECT=palai-prod\nPALAI_EDGE_PORT=443\n"))
	if err != nil {
		t.Fatalf("a complete env file must be accepted: %v", err)
	}
	// The container names must be the ones backup/restore/support-bundle already use.
	if got := s.cfg.containerName("postgres"); got != "palai-prod-postgres-1" {
		t.Fatalf("containerName = %q, want palai-prod-postgres-1", got)
	}
}
