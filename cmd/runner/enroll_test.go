package main

// The `palai enroll` proofs. They drive runEnroll — the real function main dispatches to — against a
// real TLS gateway stub, because every claim here is about what the COMMAND does to the machine: which
// files it writes, in which order it checks them, and what it leaves behind that a later reader could
// find a credential in.
//
// WHAT THEY DO NOT COVER, said first: the service install. runEnroll calls device.Install, which shells
// out to the real launchctl/systemctl, so these run with --no-service and the install is measured in
// packages/device against a substituted runner. And "the machine comes back after a reboot" is measured
// nowhere in this task — plan Milestone A0 owns it and it needs a machine that can be powered off.

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
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/palgroup/palai/packages/device"
)

// enrollStub is a runner gateway that does exactly what the real one does to a request as far as this
// command can observe: verifies the CSR, mints an id, and returns a certificate for it. It records every
// request body so the leak scan can read what actually went on the wire rather than what the client
// meant to send.
type enrollStub struct {
	mu       sync.Mutex
	bodies   []string
	url      string
	caPEM    []byte
	nextID   string
	caKey    *ecdsa.PrivateKey
	caCert   *x509.Certificate
	requests int
}

func newEnrollStub(t *testing.T) *enrollStub {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "palai-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, template, template, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}

	// The server certificate carries EXACTLY ONE SAN and it is `localhost`, because packages/runner
	// requires the leaf's single DNS name to equal the configured controller DNS. A certificate with two
	// SANs would be refused, which is a property this fixture must not accidentally hide.
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, caCert, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}

	stub := &enrollStub{
		caPEM:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		nextID: "rnr_stub_first",
		caKey:  caKey, caCert: caCert,
	}

	// LISTEN ON `localhost`, NOT ON 127.0.0.1, and the difference is measured rather than stylistic: the
	// client dials the name in --url, and on this machine `localhost` resolves to ::1 first. A listener
	// bound only to 127.0.0.1 leaves that dial with nothing to reach, and the failure is a 30-second
	// context deadline rather than a connection refusal — a fixture defect that reads exactly like a
	// product hang.
	listener, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{
		Handler:  http.HandlerFunc(stub.handle),
		ErrorLog: newDiscardLogger(),
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{{Certificate: [][]byte{serverDER}, PrivateKey: serverKey}},
		},
	}
	go func() { _ = server.ServeTLS(listener, "", "") }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	})
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	stub.url = "https://localhost:" + port
	return stub
}

func (s *enrollStub) handle(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 64*1024))
	s.mu.Lock()
	s.bodies = append(s.bodies, string(raw))
	s.requests++
	id := s.nextID
	s.mu.Unlock()

	var request struct {
		CSR             string `json:"csr"`
		RecoverRunnerID string `json:"recover_runner_id"`
	}
	if err := json.Unmarshal(raw, &request); err != nil || request.CSR == "" {
		http.Error(w, "the command did not send a signed certificate request", http.StatusBadRequest)
		return
	}
	der, err := base64.StdEncoding.DecodeString(request.CSR)
	if err != nil {
		http.Error(w, "bad csr encoding", http.StatusBadRequest)
		return
	}
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil || csr.CheckSignature() != nil {
		http.Error(w, "csr does not verify", http.StatusBadRequest)
		return
	}
	// The recovery contract, restated by the stub: a machine that names an id gets that id back. It is
	// what lets this test assert the id survives a second `enroll` without needing a registry.
	if request.RecoverRunnerID != "" {
		id = request.RecoverRunnerID
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: id},
		DNSNames:     []string{id + ".runners.palai.internal"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, s.caCert, csr.PublicKey, s.caKey)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"certificate": base64.StdEncoding.EncodeToString(leafDER),
		"runner_id":   id,
	})
}

func (s *enrollStub) sentBodies() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.bodies...)
}

// enrollFixture is one throwaway machine: a home directory, a key file and a CA file.
type enrollFixture struct {
	home, keyFile, caFile, configPath string
	servicesInstalled                 int
	stub                              *enrollStub
}

// poolKey is the value the leak scan hunts for. It is shaped like a real one (`rpk_` + opaque tail) so a
// scan that matched only the prefix would not be doing the work.
const poolKey = "rpk_live_7f3a9c2e4b1d8056"

func newEnrollFixture(t *testing.T) *enrollFixture {
	t.Helper()
	home := t.TempDir()
	// HOME is redirected so device.HostPaths resolves inside the temporary directory. It is the one
	// environment variable this test sets, and it is the OS's rather than Palai's.
	t.Setenv("HOME", home)
	stub := newEnrollStub(t)

	keyFile := filepath.Join(home, "pool.key")
	if err := os.WriteFile(keyFile, []byte(poolKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	caFile := filepath.Join(home, "ca.pem")
	if err := os.WriteFile(caFile, stub.caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return &enrollFixture{home: home, keyFile: keyFile, caFile: caFile, stub: stub,
		configPath: filepath.Join(home, "agent.json")}
}

func (f *enrollFixture) args(extra ...string) []string {
	return append([]string{
		"--url", f.stub.url,
		"--key-file", f.keyFile,
		"--ca-file", f.caFile,
	}, extra...)
}

// seams point every device file at this fixture's directory and replace the service installation, so
// these tests drive the SHIPPED enrol function without shelling out to launchctl or systemctl on the
// machine running them. They are fields rather than the `--config` and `--no-service` flags that used to
// carry them: a product surface that exists for a test is one an operator can reach, and `--no-service`
// could leave a machine enrolled with nothing to bring it back after a power cycle.
func (f *enrollFixture) seams() enrolSeams {
	paths := device.DefaultPaths(runtime.GOOS, f.home, func(string) string { return "" })
	paths.ConfigFile = f.configPath
	return enrolSeams{
		Paths: &paths,
		InstallService: func(p device.Paths, spec device.ServiceSpec) (device.InstalledService, error) {
			f.servicesInstalled++
			return device.InstalledService{Path: p.ServiceFile, Loaded: true, Started: true}, nil
		},
	}
}

// TestEnrollWritesTheMachineAndKeepsItsIdentityAcrossRuns is the command's whole contract in one
// measurement: one command, config + identity on disk at 0600, and a SECOND run of the same command
// recovering the same id rather than becoming a second machine.
//
// ‼️ THE SECOND RUN IS THE LOAD-BEARING HALF. Re-running an installer is what an operator does when
// something looked wrong, and before durable device keys that action produced a second machine row with
// a second approval — silently. The assertion is that the id, the device key and the fingerprint on disk
// are all the same afterwards.
func TestEnrollWritesTheMachineAndKeepsItsIdentityAcrossRuns(t *testing.T) {
	f := newEnrollFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var out strings.Builder
	if err := enrol(ctx, f.args(), &out, f.seams()); err != nil {
		t.Fatalf("enroll: %v\n%s", err, out.String())
	}

	paths := devicePathsFor(t, f.home)
	paths.ConfigFile = f.configPath
	for _, path := range []string{paths.ConfigFile, paths.DeviceKeyFile, paths.IdentityFile} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("enroll did not write %s: %v", path, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s has mode %04o, want 0600", path, info.Mode().Perm())
		}
	}

	firstIdentity := readIdentity(t, paths.IdentityFile)
	if firstIdentity.RunnerID != "rnr_stub_first" {
		t.Fatalf("recorded runner id %q, want the one the server minted", firstIdentity.RunnerID)
	}
	firstKey := readFile(t, paths.DeviceKeyFile)

	// SECOND RUN — the operator re-running the installer.
	out.Reset()
	if err := enrol(ctx, f.args(), &out, f.seams()); err != nil {
		t.Fatalf("second enroll: %v\n%s", err, out.String())
	}
	secondIdentity := readIdentity(t, paths.IdentityFile)
	if secondIdentity.RunnerID != firstIdentity.RunnerID {
		t.Fatalf("re-running the installer produced id %q, want %q — a second machine row for one box",
			secondIdentity.RunnerID, firstIdentity.RunnerID)
	}
	if string(readFile(t, paths.DeviceKeyFile)) != string(firstKey) {
		t.Fatal("the second run replaced the device key: the machine's identity is its key, so replacing it " +
			"makes every restart a new machine")
	}
	if secondIdentity.Fingerprint != firstIdentity.Fingerprint {
		t.Fatalf("fingerprint changed across runs: %q -> %q", firstIdentity.Fingerprint, secondIdentity.Fingerprint)
	}

	// The second request carried the held id, which is what asks the control plane to recover rather than
	// mint. Reading it off the WIRE rather than off a local variable is the point: an agent that computed
	// the claim and failed to send it would satisfy every local assertion above.
	bodies := f.stub.sentBodies()
	if len(bodies) != 2 {
		t.Fatalf("the gateway saw %d enrolment requests", len(bodies))
	}
	if !strings.Contains(bodies[1], firstIdentity.RunnerID) {
		t.Fatalf("the second request did not present the machine's own id:\n%s", bodies[1])
	}
	if strings.Contains(bodies[0], "recover_runner_id") {
		t.Fatalf("the FIRST request claimed an identity it did not hold:\n%s", bodies[0])
	}
	// ‼️ THE SERVICE IS INSTALLED, AND SAYING SO IS WHY `--no-service` IS GONE. That flag let this test
	// skip the step, which meant nothing measured whether enrolling a machine also gave it something to
	// bring it back — the state DoD 20 forbids. The seam substitutes the platform call; it does not skip
	// it, so removing the install from enrol reddens this line.
	// TWO runs, so two installs: re-running the installer must leave the machine with a service, not
	// take one away. Zero is the failure this guards.
	if f.servicesInstalled != 2 {
		t.Fatalf("enrol installed %d services across two runs, want 2: a machine enrolled with nothing to start it does not come back from a power cycle", f.servicesInstalled)
	}
}

// TestAnUnsafeKeyIsRefusedBeforeTheGatewayIsCalled is plan §T2's fourth RED with the word that makes it
// a property rather than a message: BEFORE.
//
// ‼️ THE ASSERTION IS ON THE REQUEST COUNT AT THE SERVER, not on the error text. A command that presented
// the key and then complained about its mode would produce an identical error and would have already
// spent the credential from a file every account on the machine could read. Counting the requests the
// gateway received is the only way to tell those two apart.
func TestAnUnsafeKeyIsRefusedBeforeTheGatewayIsCalled(t *testing.T) {
	f := newEnrollFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := os.Chmod(f.keyFile, 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	err := enrol(ctx, f.args(), &out, f.seams())
	if err == nil {
		t.Fatal("a world-readable pool key was accepted")
	}
	f.stub.mu.Lock()
	requests := f.stub.requests
	f.stub.mu.Unlock()
	if requests != 0 {
		t.Fatalf("the gateway received %d request(s) before the permission refusal: the key was already spent "+
			"from a file every account on this machine could read", requests)
	}
	paths := devicePathsFor(t, f.home)
	if _, statErr := os.Stat(paths.DeviceKeyFile); statErr == nil {
		t.Fatal("the refused enrolment still minted and persisted a device key")
	}
}

// TestTheKeyIsNeverOnArgvInTheEnvironmentOrInAnythingWrittenIsTheLeakScan is plan §T2's fifth RED.
//
// ‼️ A SCAN THAT FINDS NOTHING PROVES NOTHING UNLESS IT CAN FIND SOMETHING. The corpus below is checked
// for a HARMLESS token first — the controller URL, which is genuinely present in the config and in every
// request body — and the test fails if that positive control is not found. Without it, a corpus assembled
// from the wrong files, or a scan over bytes that were never decoded, reports the same clean result as a
// product with no leak.
//
// ‼️ AND THE BODIES ARE DECODED BEFORE THEY ARE SCANNED. The wire carries base64 in two fields (the CSR
// and the certificate), and a raw-byte scan over base64 can never fail: `rpk_live_...` base64-encoded
// contains none of those characters in that order. Every base64-looking field is decoded and the decoded
// bytes join the corpus.
func TestTheKeyIsNeverOnArgvInTheEnvironmentOrInAnythingWrittenIsTheLeakScan(t *testing.T) {
	f := newEnrollFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var out strings.Builder
	args := f.args()
	if err := enrol(ctx, args, &out, f.seams()); err != nil {
		t.Fatalf("enroll: %v\n%s", err, out.String())
	}
	paths := devicePathsFor(t, f.home)
	paths.ConfigFile = f.configPath

	corpus := map[string]string{
		"the command's own output": out.String(),
		"argv":                     strings.Join(append([]string{"palai", "enroll"}, args...), " "),
		"process environment":      strings.Join(os.Environ(), "\n"),
		"agent config":             string(readFile(t, paths.ConfigFile)),
		"device identity":          string(readFile(t, paths.IdentityFile)),
		"device key file":          string(readFile(t, paths.DeviceKeyFile)),
	}
	for i, body := range f.stub.sentBodies() {
		corpus[fmt.Sprintf("enrolment request %d (raw)", i)] = body
		corpus[fmt.Sprintf("enrolment request %d (decoded)", i)] = decodeEmbeddedBase64(body)
	}

	// THE POSITIVE CONTROL. It is a harmless token that MUST be present, and finding it is what makes the
	// zero below a measurement instead of an assertion about an empty corpus.
	control := f.stub.url
	found := 0
	for _, body := range corpus {
		if strings.Contains(body, control) {
			found++
		}
	}
	if found == 0 {
		t.Fatalf("the scan's positive control %q was not found anywhere in a %d-part corpus: the scan is looking "+
			"at the wrong bytes, so its clean result about the pool key means nothing", control, len(corpus))
	}
	t.Logf("leak scan is non-vacuous: the harmless control %q occurs in %d of %d corpus parts", control, found, len(corpus))

	// THE ACTUAL SCAN.
	for name, body := range corpus {
		if strings.Contains(body, poolKey) {
			t.Errorf("the pool key VALUE occurs in %s", name)
		}
		// The bare `rpk_` prefix catches a truncated or reformatted copy that the exact-value check above
		// would miss — a log line that printed the first eight characters, for instance.
		if strings.Contains(body, "rpk_") {
			t.Errorf("something shaped like a pool key occurs in %s", name)
		}
	}

	// THE DECODED HALF IS ITSELF CHECKED FOR NON-VACUITY. A decoder that silently returned nothing would
	// make every decoded-corpus assertion above pass over an empty string. The certificate the server
	// issued carries the runner id in its subject, so the decoded bytes must contain it.
	decoded := corpus["enrolment request 0 (decoded)"]
	if len(decoded) == 0 {
		t.Fatal("decoding the enrolment request produced no bytes: every assertion over the decoded corpus was vacuous")
	}
	t.Logf("decoded corpus for request 0 is %d bytes", len(decoded))
}

// decodeEmbeddedBase64 returns the concatenated DECODED bytes of every base64-looking JSON string value
// in a request body.
//
// ‼️ IT EXISTS BECAUSE A RAW SCAN OVER ENCODED BYTES CAN NEVER FAIL. `rpk_live_...` base64-encoded shares
// no substring with its plaintext, so a scan that skipped this step would report the same clean result
// for a body that carried the key inside the CSR as for one that did not.
func decodeEmbeddedBase64(body string) string {
	var fields map[string]any
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		return ""
	}
	var decoded strings.Builder
	for _, value := range fields {
		text, ok := value.(string)
		if !ok || len(text) < 16 {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(text)
		if err != nil {
			continue
		}
		decoded.Write(raw)
		decoded.WriteByte('\n')
	}
	return decoded.String()
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return raw
}

func readIdentity(t *testing.T, path string) struct {
	RunnerID    string `json:"runner_id"`
	Fingerprint string `json:"public_key_sha256"`
} {
	t.Helper()
	var identity struct {
		RunnerID    string `json:"runner_id"`
		Fingerprint string `json:"public_key_sha256"`
	}
	if err := json.Unmarshal(readFile(t, path), &identity); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return identity
}

// devicePathsFor is the standard layout for a throwaway home directory, resolved through the same
// function the command uses rather than restated — a test that hardcoded the paths would keep passing
// after the product moved them.
func devicePathsFor(t *testing.T, home string) device.Paths {
	t.Helper()
	return device.DefaultPaths(runtime.GOOS, home, os.Getenv)
}

// newDiscardLogger silences the TLS server's own error log; a client that hangs up on a refused
// handshake is an expected event in these proofs, not a failure worth printing.
func newDiscardLogger() *log.Logger { return log.New(io.Discard, "", 0) }
