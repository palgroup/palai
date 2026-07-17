//go:build e2e

// Package local is the end-to-end proof for the packaged local stack: the `palai`
// CLI drives a real four-service Docker Compose distribution (postgres, object-store,
// control-plane, runner) and the doctor lifecycle. It runs only under
// `go test -tags e2e ./tests/e2e/local`, needs a working Docker daemon (skipped
// otherwise), and is kept out of `make verify` by the e2e build tag so the unit tier
// stays Docker-free — the same gating the sse/responses e2e tiers use.
//
// Every stack is isolated: a fresh PALAI_HOME temp dir, a random Compose project
// name, and random host ports, so `-count=3` reruns and parallel suites never collide.
package local

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// palaiCLI builds the CLI once for the whole package and caches the binary path.
var (
	palaiOnce sync.Once
	palaiBin  string
	palaiErr  error
)

// buildCLI compiles cmd/cli into a temp binary the tests exec. It is the single build
// for the package; a failure here (e.g. the CLI does not exist yet) fails every test,
// which is the intended RED before the CLI is written.
func buildCLI(t *testing.T) string {
	t.Helper()
	palaiOnce.Do(func() {
		dir, err := os.MkdirTemp("", "palai-cli-")
		if err != nil {
			palaiErr = err
			return
		}
		bin := filepath.Join(dir, "palai")
		cmd := exec.Command("go", "build", "-o", bin, "./cmd/cli")
		cmd.Dir = repoRoot(t)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			palaiErr = fmt.Errorf("build palai CLI: %v\n%s", err, stderr.String())
			return
		}
		palaiBin = bin
	})
	if palaiErr != nil {
		t.Fatalf("palai CLI unavailable: %v", palaiErr)
	}
	return palaiBin
}

// repoRoot walks up from this test file to the module root (the directory holding
// go.mod), so the CLI build and the compose-file path resolve regardless of cwd.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve caller for repo root")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above test file")
		}
		dir = parent
	}
}

// requireDocker skips the test when no Docker daemon is reachable, so the suite is a
// no-op in a Docker-free environment instead of a failure.
func requireDocker(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
		t.Skipf("docker daemon not available: %v", err)
	}
}

// config mirrors the CLI's .palai/config.json: the random ports and identity `palai
// init` mints so the harness can reach the published surfaces.
type config struct {
	Project       string `json:"project"`
	DataDir       string `json:"data_dir"`
	APIPort       int    `json:"api_port"`
	RunnerPort    int    `json:"runner_port"`
	PgPort        int    `json:"pg_port"`
	S3Port        int    `json:"s3_port"`
	BaseURL       string `json:"base_url"`
	ControllerDNS string `json:"controller_dns"`
}

// stack is one isolated local deployment under a temp PALAI_HOME.
type stack struct {
	t    *testing.T
	home string // PALAI_HOME: the .palai data dir
}

// newStack allocates an isolated PALAI_HOME and registers teardown that force-removes
// the stack's containers and volumes (the suite retains volumes across down/up on
// purpose, so the cleanup — not `down` — is what stops leaks between runs).
func newStack(t *testing.T) *stack {
	t.Helper()
	requireDocker(t)
	buildCLI(t)
	s := &stack{t: t, home: t.TempDir()}
	t.Cleanup(func() {
		// reset --confirm tears down containers and volumes; ignore errors — the stack
		// may never have come up.
		_ = s.try("local", "reset", "--confirm")
	})
	return s
}

// env is the CLI invocation environment: PALAI_HOME points at this stack's data dir and
// PALAI_COMPOSE_FILE at the repo's committed compose file.
func (s *stack) env() []string {
	return append(os.Environ(),
		"PALAI_HOME="+s.home,
		"PALAI_COMPOSE_FILE="+filepath.Join(repoRoot(s.t), "deploy", "compose", "compose.yaml"),
	)
}

// run executes a CLI subcommand and fails the test on a non-zero exit, returning stdout.
func (s *stack) run(args ...string) string {
	s.t.Helper()
	out, err := s.exec(nil, args...)
	if err != nil {
		s.t.Fatalf("palai %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// runInput is run with data piped to the CLI's stdin (provider add reads the secret
// from stdin, never argv).
func (s *stack) runInput(stdin string, args ...string) string {
	s.t.Helper()
	out, err := s.exec(strings.NewReader(stdin), args...)
	if err != nil {
		s.t.Fatalf("palai %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// try runs a CLI subcommand and returns its combined output and error without failing
// the test — used by teardown and by the reset-exit-code assertions.
func (s *stack) try(args ...string) error {
	_, err := s.exec(nil, args...)
	return err
}

// exec runs the CLI with the stack environment, returning combined output. A long
// timeout guards against a hung compose build without hanging the whole suite.
func (s *stack) exec(stdin io.Reader, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, buildCLI(s.t), args...)
	cmd.Dir = repoRoot(s.t)
	cmd.Env = s.env()
	cmd.Stdin = stdin
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

// config reads the .palai/config.json the CLI wrote.
func (s *stack) config() config {
	s.t.Helper()
	raw, err := os.ReadFile(filepath.Join(s.home, "config.json"))
	if err != nil {
		s.t.Fatalf("read config.json: %v", err)
	}
	var c config
	if err := json.Unmarshal(raw, &c); err != nil {
		s.t.Fatalf("decode config.json: %v", err)
	}
	return c
}

// bootstrapKey reads the dev API key `palai init` minted (the file compose mounts as the
// bootstrap_api_key secret).
func (s *stack) bootstrapKey() string {
	s.t.Helper()
	raw, err := os.ReadFile(filepath.Join(s.home, "api-key"))
	if err != nil {
		s.t.Fatalf("read api-key: %v", err)
	}
	return strings.TrimSpace(string(raw))
}

// caPool trusts the local CA `palai init` generated, for the mTLS runner-port probes.
func (s *stack) caPool() *x509.CertPool {
	s.t.Helper()
	pem, err := os.ReadFile(filepath.Join(s.home, "ca", "ca.crt"))
	if err != nil {
		s.t.Fatalf("read ca.crt: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		s.t.Fatal("ca.crt held no certificates")
	}
	return pool
}

// doctorReport is the doctor --json contract: an overall verdict plus a check map, each
// entry carrying a status and a human detail.
type doctorReport struct {
	OK     bool                   `json:"ok"`
	Checks map[string]doctorCheck `json:"checks"`
}

type doctorCheck struct {
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// doctor runs `local doctor --json` and decodes the report. It does not assert health —
// callers decide whether they require all-green.
func (s *stack) doctor() doctorReport {
	s.t.Helper()
	out := s.run("local", "doctor", "--json")
	var report doctorReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		s.t.Fatalf("decode doctor --json: %v\n%s", err, out)
	}
	return report
}

// apiClient is a plain HTTP client for the public API (LP-0 serves it over http; only
// the runner gateway is mTLS).
func (s *stack) apiClient() *http.Client {
	return &http.Client{Timeout: 15 * time.Second}
}

// getResponse fetches a response by id over the public API with the bootstrap key.
func (s *stack) getResponse(id string) *http.Response {
	s.t.Helper()
	c := s.config()
	req, err := http.NewRequest(http.MethodGet, c.BaseURL+"/v1/responses/"+id, nil)
	if err != nil {
		s.t.Fatalf("build GET response: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.bootstrapKey())
	resp, err := s.apiClient().Do(req)
	if err != nil {
		s.t.Fatalf("GET /v1/responses/%s: %v", id, err)
	}
	return resp
}

// runnerTLSConfig trusts the local CA and pins the controller DNS the server cert
// carries, so a probe of the published runner port verifies the gateway's identity.
func (s *stack) runnerTLSConfig() *tls.Config {
	c := s.config()
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		RootCAs:    s.caPool(),
		ServerName: c.ControllerDNS,
	}
}

// composeConfigOutput renders the fully-interpolated compose config the CLI would apply,
// so a secret-surface scan sees exactly what Docker receives.
func (s *stack) composeConfigOutput() string {
	s.t.Helper()
	c := s.config()
	cmd := exec.Command("docker", "compose",
		"-p", c.Project,
		"-f", filepath.Join(repoRoot(s.t), "deploy", "compose", "compose.yaml"),
		"config")
	cmd.Dir = repoRoot(s.t)
	cmd.Env = s.env()
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		s.t.Fatalf("docker compose config: %v\n%s", err, out.String())
	}
	return out.String()
}

// controlPlaneEnv reads the control-plane container's resolved environment via docker
// inspect, so the secret-surface scan proves no raw credential rides in .Config.Env.
func (s *stack) controlPlaneEnv() string {
	s.t.Helper()
	c := s.config()
	name := c.Project + "-control-plane-1"
	cmd := exec.Command("docker", "inspect", "--format", "{{json .Config.Env}}", name)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		s.t.Fatalf("docker inspect %s: %v\n%s", name, err, out.String())
	}
	return out.String()
}

// projectVolumes lists the Docker volumes belonging to this stack's compose project.
func (s *stack) projectVolumes() []string {
	s.t.Helper()
	c := s.config()
	cmd := exec.Command("docker", "volume", "ls", "-q", "--filter", "label=com.docker.compose.project="+c.Project)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		s.t.Fatalf("docker volume ls: %v", err)
	}
	var vols []string
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line != "" {
			vols = append(vols, line)
		}
	}
	return vols
}
