//go:build e2e

package local

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// doctorChecks is the exact set of checks `local doctor --json` must report, each with a
// status and a human detail (spec §44 doctor surface; LP-002).
var doctorChecks = []string{
	"api", "migration", "object_store", "runner", "image_digests",
	"provider", "clock", "retention_ttl", "runner_tls_reject",
}

// TestDoctorJSONShape proves the doctor --json contract: all nine checks are present and
// each carries {status, detail}. retention_ttl is read from GET /v1/capabilities (the
// Task 11d discovery surface) and object_store reports the ADR-0004 digest reachable via
// an S3-endpoint ping.
func TestDoctorJSONShape(t *testing.T) {
	s := newStack(t)
	s.run("init")
	// A provider secret makes the provider check green out of the box; the sentinel-scan
	// test covers the credential-hygiene path separately.
	s.runInput("local-dev-key\n", "provider", "add", "provider-one")
	s.run("local", "up")

	report := s.doctor()
	for _, name := range doctorChecks {
		check, ok := report.Checks[name]
		if !ok {
			t.Fatalf("doctor report missing check %q: %+v", name, report.Checks)
		}
		if check.Status == "" {
			t.Errorf("check %q has empty status", name)
		}
		if check.Detail == "" {
			t.Errorf("check %q has empty detail", name)
		}
	}
	if len(report.Checks) != len(doctorChecks) {
		t.Errorf("doctor reported %d checks, want %d: %+v", len(report.Checks), len(doctorChecks), report.Checks)
	}
	if !report.OK {
		t.Fatalf("doctor not green: %+v", report.Checks)
	}
	if got := report.Checks["object_store"].Status; got != "ok" {
		t.Errorf("object_store status = %q, want ok (S3 ping)", got)
	}
	if got := report.Checks["runner_tls_reject"].Status; got != "ok" {
		t.Errorf("runner_tls_reject status = %q, want ok", got)
	}
}

// TestRunnerPortRejectsNonMTLS proves the runner gateway listener enforces mutual TLS: a
// client trusting the CA but presenting no runner certificate is rejected at
// /v1/runner/connect (401), and a plain-HTTP request to the TLS port is refused by the
// transport before any handler. (Binding-1 proof — the live round-trip itself is Task 15.)
func TestRunnerPortRejectsNonMTLS(t *testing.T) {
	s := newStack(t)
	s.run("init")
	s.run("local", "up")
	c := s.config()

	// Certless mTLS client: the handshake is allowed (enrollment is certless) but the
	// session handler asserts the client chain and returns 401.
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: s.runnerTLSConfig()}}
	resp, err := client.Get(fmt.Sprintf("https://127.0.0.1:%d/v1/runner/connect", c.RunnerPort))
	if err != nil {
		t.Fatalf("certless connect probe errored before a status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("certless connect = %d, want 401", resp.StatusCode)
	}

	// A plain-HTTP request to the TLS port must never reach the application. Go's TLS
	// server refuses plaintext at the transport with 400 "Client sent an HTTP request to
	// an HTTPS server." (before any handler runs); a transport error is equally valid.
	// Either way the runner endpoint is never served over plaintext — a 2xx or the app's
	// own 401 over plain HTTP would be the failure.
	plain, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/runner/connect", c.RunnerPort))
	if err == nil {
		body, _ := io.ReadAll(plain.Body)
		plain.Body.Close()
		if plain.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "HTTP request to an HTTPS server") {
			t.Fatalf("plain HTTP to the mTLS runner port was served (%d: %q); want a TLS-layer refusal",
				plain.StatusCode, strings.TrimSpace(string(body)))
		}
	}
}

// --- shared helpers ---

// lastJSONLine returns the last non-empty line of out that begins with '{', so a command
// that prints progress before its JSON envelope still parses cleanly.
func lastJSONLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "{") {
			return line
		}
	}
	return strings.TrimSpace(out)
}

// readCompose reads the committed compose file, the surface a secret must never touch.
func readCompose(t *testing.T, s *stack) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "deploy", "compose", "compose.yaml"))
	if err != nil {
		t.Fatalf("read compose.yaml: %v", err)
	}
	return string(raw)
}

// readSecret reads a .palai file secret by ref.
func readSecret(t *testing.T, s *stack, ref string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(s.home, "secrets", ref))
	if err != nil {
		t.Fatalf("read secret %s: %v", ref, err)
	}
	return string(raw)
}

// assertMode0600 fails unless the .palai-relative path is a strict 0600 file.
func assertMode0600(t *testing.T, s *stack, rel string) {
	t.Helper()
	info, err := os.Stat(filepath.Join(s.home, rel))
	if err != nil {
		t.Fatalf("stat %s: %v", rel, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("%s mode = %o, want 600", rel, perm)
	}
}
