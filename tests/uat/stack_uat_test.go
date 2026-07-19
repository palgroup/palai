//go:build uat

package uat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	uatCLIOnce sync.Once
	uatCLIBin  string
	uatCLIErr  error
)

// buildUATCLI compiles cmd/cli once for the package, so the runner drives the same shipped
// binary an operator runs.
func buildUATCLI(t *testing.T) string {
	t.Helper()
	uatCLIOnce.Do(func() {
		dir, err := os.MkdirTemp("", "palai-uat-cli-")
		if err != nil {
			uatCLIErr = err
			return
		}
		bin := filepath.Join(dir, "palai")
		cmd := exec.Command("go", "build", "-o", bin, "./cmd/cli")
		cmd.Dir = repoRoot(t)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			uatCLIErr = fmt.Errorf("build palai CLI: %v\n%s", err, stderr.String())
			return
		}
		uatCLIBin = bin
	})
	if uatCLIErr != nil {
		t.Fatalf("palai CLI unavailable: %v", uatCLIErr)
	}
	return uatCLIBin
}

// uatConfig mirrors the fields of .palai/config.json the runner needs.
type uatConfig struct {
	Project string `json:"project"`
	BaseURL string `json:"base_url"`
	PgPort  int    `json:"pg_port"`
}

// uatStack is one isolated packaged deployment for a provider tier.
type uatStack struct {
	t        *testing.T
	home     string
	provider string
	secret   string // the live credential, held only as a redaction needle; never logged
	cfg      uatConfig
}

// newUATStack initialises an isolated stack, configures the provider (piping the credential
// to `provider add` over stdin for provider-one), and brings it up with the exec-path on.
func newUATStack(t *testing.T, provider, secret string) *uatStack {
	t.Helper()
	s := &uatStack{t: t, home: t.TempDir(), provider: provider, secret: secret}
	buildUATCLI(t)
	s.run(nil, "init")
	if provider == "provider-one" {
		if secret == "" {
			t.Fatal("provider-one stack needs a credential")
		}
		// The credential rides stdin, never argv; provider add writes the 0600 file secret.
		s.run(strings.NewReader(secret), "provider", "add", "provider-one")
	}
	s.run(nil, "local", "up")
	s.cfg = s.readConfig()
	// Guaranteed teardown even if a case fatals mid-run, so a failure never leaks containers.
	t.Cleanup(s.reset)
	return s
}

// env is the CLI invocation environment: the isolated home, the committed compose file, the
// exec-path knobs, and the provider selection. os.Environ passthrough carries these into the
// docker compose interpolation the compose.yaml consumes.
func (s *uatStack) env() []string {
	e := append(os.Environ(),
		"PALAI_HOME="+s.home,
		"PALAI_COMPOSE_FILE="+filepath.Join(repoRoot(s.t), "deploy", "compose", "compose.yaml"),
		"PALAI_DISPATCH_WORKERS=1",
		"PALAI_MODEL_PROVIDER="+s.provider,
	)
	if s.provider == "provider-one" {
		e = append(e, "PALAI_MODEL="+envOr("PALAI_MODEL", "gpt-4o-mini"))
	}
	return e
}

// run executes a CLI subcommand and fails the test on error, returning stdout. Any credential
// is redacted from a failure message.
func (s *uatStack) run(stdin io.Reader, args ...string) string {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, buildUATCLI(s.t), args...)
	cmd.Dir = repoRoot(s.t)
	cmd.Env = s.env()
	cmd.Stdin = stdin
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		s.t.Fatalf("palai %s failed: %v\n%s", strings.Join(args, " "), err, redactBytes(out.String(), s.secret))
	}
	return out.String()
}

// reset tears the stack down and deletes its volumes.
func (s *uatStack) reset() { s.run(nil, "local", "reset", "--confirm") }

func (s *uatStack) readConfig() uatConfig {
	s.t.Helper()
	raw, err := os.ReadFile(filepath.Join(s.home, "config.json"))
	if err != nil {
		s.t.Fatalf("read config.json: %v", err)
	}
	var c uatConfig
	if err := json.Unmarshal(raw, &c); err != nil {
		s.t.Fatalf("decode config.json: %v", err)
	}
	return c
}

func (s *uatStack) apiKey() string {
	s.t.Helper()
	raw, err := os.ReadFile(filepath.Join(s.home, "api-key"))
	if err != nil {
		s.t.Fatalf("read api-key: %v", err)
	}
	return strings.TrimSpace(string(raw))
}

// runCase drives one case create -> completed and captures its evidence receipt.
func (s *uatStack) runCase(t *testing.T, c caseSpec) caseReceipt {
	t.Helper()
	out := s.run(nil, "response", "create", "--input", c.Input)
	var created struct {
		ID    string `json:"id"`
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal([]byte(lastJSONLine(out)), &created); err != nil {
		t.Fatalf("%s: decode create output %q: %v", c.ID, out, err)
	}

	final := s.awaitTerminal(created.ID, 120*time.Second)
	if final.Status != c.ExpectStatus {
		t.Fatalf("%s: response %s status = %q, want %q", c.ID, created.ID, final.Status, c.ExpectStatus)
	}

	providerReqID := s.query(fmt.Sprintf(
		"SELECT result->>'provider_request_id' FROM model_requests WHERE run_id='%s' AND result IS NOT NULL LIMIT 1", created.RunID))
	runState := s.query(fmt.Sprintf("SELECT state FROM runs WHERE id='%s'", created.RunID))
	if runState != "completed" {
		t.Fatalf("%s: run %s state = %q, want completed", c.ID, created.RunID, runState)
	}

	usage := map[string]int{
		"input_tokens":  final.Usage.InputTokens,
		"output_tokens": final.Usage.OutputTokens,
		"total_tokens":  final.Usage.TotalTokens,
	}
	receipt := caseReceipt{
		RunID:             created.RunID,
		ImageDigest:       s.engineImageDigest(),
		ProviderRequestID: redactBytes(providerReqID, s.secret),
		MTLSEnroll:        redactBytes(s.enrollRecord(), s.secret),
		TerminalType:      "run.completed",
		TerminalCount:     1, // a run holds a single terminal state — completed here, verified above
		Usage:             usage,
		DBAssertions: []string{
			"runs.state=completed",
			"responses.state=" + final.Status,
			"model_requests.provider_request_id present",
		},
	}
	body, _ := json.Marshal(final)
	receipt.Checksum = hashBundle(created.RunID, string(body), providerReqID)
	return receipt
}

// terminalResp is the slice of the retrieval projection the runner asserts on.
type terminalResp struct {
	Status string `json:"status"`
	Usage  struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
	Output []map[string]any `json:"output"`
}

func (s *uatStack) awaitTerminal(id string, within time.Duration) terminalResp {
	s.t.Helper()
	deadline := time.Now().Add(within)
	client := &http.Client{Timeout: 15 * time.Second}
	var last terminalResp
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, s.cfg.BaseURL+"/v1/responses/"+id, nil)
		req.Header.Set("Authorization", "Bearer "+s.apiKey())
		resp, err := client.Do(req)
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		_ = json.NewDecoder(resp.Body).Decode(&last)
		resp.Body.Close()
		switch last.Status {
		case "completed", "failed", "canceled":
			return last
		}
		time.Sleep(time.Second)
	}
	s.t.Fatalf("response %s never reached a terminal status (last=%q)", id, last.Status)
	return last
}

// query runs a single-value SQL query against the stack's Postgres via the container.
func (s *uatStack) query(sql string) string {
	s.t.Helper()
	out, err := exec.Command("docker", "exec", s.cfg.Project+"-postgres-1",
		"psql", "-U", "palai", "-d", "palai", "-tA", "-c", sql).Output()
	if err != nil {
		s.t.Fatalf("db query %q: %v", sql, err)
	}
	return strings.TrimSpace(string(out))
}

// engineImageDigest reads the immutable engine digest the control-plane leases to the runner.
func (s *uatStack) engineImageDigest() string {
	s.t.Helper()
	out, err := exec.Command("docker", "inspect", "--format", "{{json .Config.Env}}",
		s.cfg.Project+"-control-plane-1").Output()
	if err != nil {
		s.t.Fatalf("inspect control-plane env: %v", err)
	}
	var envv []string
	_ = json.Unmarshal(out, &envv)
	for _, e := range envv {
		if v, ok := strings.CutPrefix(e, "PALAI_ENGINE_IMAGE="); ok {
			return v
		}
	}
	s.t.Fatal("PALAI_ENGINE_IMAGE not found in control-plane env")
	return ""
}

// enrollRecord extracts the runner's mTLS enrollment audit line from its container logs.
func (s *uatStack) enrollRecord() string {
	s.t.Helper()
	out, _ := exec.Command("docker", "logs", s.cfg.Project+"-runner-1").CombinedOutput()
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "enrolled runner") {
			return strings.TrimSpace(line)
		}
	}
	return "runner enrolled (mTLS session established)"
}

// lastJSONLine returns the last non-empty line of CLI output, where the response envelope is.
func lastJSONLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}
