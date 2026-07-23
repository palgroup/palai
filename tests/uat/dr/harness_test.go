//go:build uat

// The E15 T5 DR-drill harness: two isolated PRODUCTION-compose stacks driven by docker-exec-by-name
// (the E14 T4/T7 pattern), under a steady per-second marker-write traffic so RPO has something to
// measure. It REUSES the shipped `palai backup`/`restore`/`restore verify` (the E14 install-backup
// tooling) verbatim through the CLI — this harness never reimplements them; its job is the drill
// choreography + measurement + the machine-generated report.
//
// Credential hygiene: the drills run on the FAKE provider (no model credential needed — DR is about
// data recovery, not the model), so nothing secret rides argv/env/log/evidence. The DB is reached
// password-free over the local socket via `docker exec` (trust auth), never a host-published port —
// the production profile keeps pg/object-store internal (production.yml `ports: !reset []`).
//
// ponytail: the thin compose bring-up plumbing below (init, identity adopt, master-key/token mint,
// up/down) is a small copy of the E14 self-host journey's stack scaffolding — that helper is
// `//go:build uat` in package uat and cannot be imported here (a different package). The copy is the
// price of the plan's mandated `tests/uat/dr/` home; the LOAD-BEARING backup/restore tooling is
// reused, not copied (same call as tests/uat/self-host's 4-line hashParts copy precedent).
package dr

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	drCLIOnce sync.Once
	drCLIBin  string
	drCLIErr  error
	drRootOne sync.Once
	drRoot    string
)

// requireDocker skips the whole suite when Docker is not reachable, so the tag-only gate stays green
// on a machine without a daemon.
func requireDocker(t *testing.T) {
	t.Helper()
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker not available; the DR drills need a running Docker Desktop")
	}
}

// repoRoot resolves the worktree root from the test's cwd (tests/uat/dr) via git.
func repoRoot(t *testing.T) string {
	t.Helper()
	drRootOne.Do(func() {
		out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
		if err != nil {
			drRoot = ""
			return
		}
		drRoot = strings.TrimSpace(string(out))
	})
	if drRoot == "" {
		t.Fatal("could not resolve repo root (git rev-parse)")
	}
	return drRoot
}

// buildCLI compiles the shipped `palai` binary once — the same binary an operator runs backup/restore
// with.
func buildCLI(t *testing.T) string {
	t.Helper()
	drCLIOnce.Do(func() {
		dir, err := os.MkdirTemp("", "palai-dr-cli-")
		if err != nil {
			drCLIErr = err
			return
		}
		bin := filepath.Join(dir, "palai")
		cmd := exec.Command("go", "build", "-o", bin, "./cmd/cli")
		cmd.Dir = repoRoot(t)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			drCLIErr = fmt.Errorf("build palai CLI: %v\n%s", err, stderr.String())
			return
		}
		drCLIBin = bin
	})
	if drCLIErr != nil {
		t.Fatalf("palai CLI unavailable: %v", drCLIErr)
	}
	return drCLIBin
}

// ensureImage builds a tagged image, reusing an existing tag if the build is blocked (offline/proxy-
// broken builder) — the E14 journey ceiling: the drills exercise recovery, not the image build.
func ensureImage(t *testing.T, tag string, buildArgs ...string) {
	t.Helper()
	args := append([]string{"build", "-t", tag}, buildArgs...)
	build := exec.Command("docker", args...)
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		if exec.Command("docker", "image", "inspect", tag).Run() == nil {
			t.Logf("ensureImage: build of %s failed (%v) but the tag exists — reusing it (build ceiling)", tag, err)
			return
		}
		t.Fatalf("build %s (and no pre-built image to fall back to): %v", tag, err)
	}
}

func imageDigest(t *testing.T, tag string) string {
	t.Helper()
	out, err := exec.Command("docker", "image", "inspect", tag, "--format", "{{.Id}}").Output()
	if err != nil {
		t.Fatalf("resolve %s digest: %v", tag, err)
	}
	id := strings.TrimSpace(string(out))
	if !strings.HasPrefix(id, "sha256:") {
		t.Fatalf("%s id %q is not a digest", tag, id)
	}
	return id
}

// ensureStackImages builds the three stack images both stacks reuse.
func ensureStackImages(t *testing.T) string {
	t.Helper()
	root := repoRoot(t)
	ensureImage(t, "palai/control-plane:local", "-f", filepath.Join(root, "deploy", "compose", "control-plane.Dockerfile"), root)
	ensureImage(t, "palai/runner:local", "-f", filepath.Join(root, "deploy", "compose", "runner.Dockerfile"), root)
	ensureImage(t, "palai/reference-engine:local", filepath.Join(root, "engines", "reference"))
	return imageDigest(t, "palai/reference-engine:local")
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// drStack is one isolated production-compose deployment for the DR drills. It carries no secret
// needle — the drills use the fake provider.
type drStack struct {
	t         *testing.T
	home      string
	project   string
	apiPort   int
	edgePort  int // reserved so production.yml's ${PALAI_EDGE_PORT:?} interpolates; the edge is NOT started
	engineDig string
	masterKey string
}

// newDRStack initialises a stack fixture (fresh PALAI_HOME) with a guaranteed teardown so a failed
// drill never leaks containers/volumes.
func newDRStack(t *testing.T, project, engineDig, masterKey string) *drStack {
	t.Helper()
	s := &drStack{t: t, home: t.TempDir(), project: project, edgePort: freePort(t), engineDig: engineDig, masterKey: masterKey}
	t.Cleanup(s.downCleanup)
	return s
}

// cleanInstall runs `palai init`, adopts config.json's identity (project + api port), and mints the
// production-mandatory files the base `init` does not: the secret master key (fail-closed guard) and
// the runner enrollment token (a hand-run production compose, unlike `palai local up`, does not mint
// it). No edge cert — this harness does not start the TLS edge.
func (s *drStack) cleanInstall() {
	s.cli(nil, "init")
	s.loadIdentity()
	writeFile(s.t, filepath.Join(s.home, "secrets", "master-key"), s.masterKey)
	writeFile(s.t, filepath.Join(s.home, "runner-token"), randomHex32(s.t))
}

// loadIdentity reads config.json (written by init) and adopts its compose project + api port, so the
// shipped CLI (which reads config.json) and the harness's compose bring-up share one identity. init
// mints its own project; we OVERRIDE it with the caller's unique DR project so sibling tasks' leak-
// guards never collide (plan house rule), then rewrite config.json's project + base_url to match.
func (s *drStack) loadIdentity() {
	path := filepath.Join(s.home, "config.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		s.t.Fatalf("read config.json: %v", err)
	}
	var c map[string]any
	if err := json.Unmarshal(raw, &c); err != nil {
		s.t.Fatalf("decode config.json: %v", err)
	}
	c["project"] = s.project
	apiPort, _ := c["api_port"].(float64)
	s.apiPort = int(apiPort)
	// config.json's base_url must point at the api port the DR override republishes on loopback, so
	// `palai response create`/`secret create` reach the control-plane behind the edge-less production
	// stack.
	c["base_url"] = fmt.Sprintf("http://127.0.0.1:%d", s.apiPort)
	out, _ := json.MarshalIndent(c, "", "  ")
	writeFile(s.t, path, string(out))
}

// composeEnv is the process environment for a compose invocation: PALAI_HOME + the ports/knobs the
// compose files interpolate. The fake provider means no model credential rides here.
func (s *drStack) composeEnv() []string {
	return append(os.Environ(),
		"PALAI_HOME="+s.home,
		"PALAI_COMPOSE_PROJECT="+s.project,
		fmt.Sprintf("PALAI_API_PORT=%d", s.apiPort),
		fmt.Sprintf("PALAI_EDGE_PORT=%d", s.edgePort),
		"PALAI_ENGINE_IMAGE="+s.engineDig,
		"PALAI_DISPATCH_WORKERS=1",
		"PALAI_MODEL_PROVIDER=fake",
		// production.yml `!reset []`s the base pg/object-store/runner host ports, but compose still
		// interpolates the vars at parse time — set them to 0 so no "variable is not set" warning
		// prints. The reset means they are never actually published (production posture intact).
		"PALAI_PG_PORT=0", "PALAI_S3_PORT=0", "PALAI_RUNNER_PORT=0",
	)
}

// composeArgs assembles compose over compose.yaml + production.yml + the DR port-republish override.
func (s *drStack) composeArgs(extra ...string) []string {
	root := repoRoot(s.t)
	base := []string{
		"compose", "-p", s.project,
		"-f", filepath.Join(root, "deploy", "compose", "compose.yaml"),
		"-f", filepath.Join(root, "deploy", "compose", "production.yml"),
		"-f", filepath.Join(root, "tests", "uat", "dr", "testdata", "dr.override.yml"),
	}
	return append(base, extra...)
}

// upServices brings the named services up and blocks on their healthchecks. The edge is never listed,
// so it is never started (no edge cert needed).
func (s *drStack) upServices(services ...string) {
	args := append(s.composeArgs("up", "-d", "--wait"), services...)
	if err := s.docker(15*time.Minute, args...); err != nil {
		s.t.Fatalf("compose up %v: %v", services, err)
	}
}

// up brings the four persistent services up (postgres, object-store, control-plane, runner).
func (s *drStack) up() { s.upServices("postgres", "object-store", "control-plane", "runner") }

// downCleanup tears the stack down and deletes its volumes (best-effort guaranteed teardown).
func (s *drStack) downCleanup() {
	_ = s.docker(5*time.Minute, s.composeArgs("down", "--volumes", "--remove-orphans")...)
}

// cli runs the shipped `palai` binary with PALAI_HOME set, failing the test on error, returning stdout.
func (s *drStack) cli(stdin any, args ...string) string {
	s.t.Helper()
	out, err := s.cliErr(stdin, args...)
	if err != nil {
		s.t.Fatalf("palai %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// cliErr runs `palai` and returns (stdout+stderr, err) WITHOUT failing — for the drills that assert a
// command must FAIL closed (restore verify under a wrong key, a tampered archive).
func (s *drStack) cliErr(stdin any, args ...string) (string, error) {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, buildCLI(s.t), args...)
	cmd.Dir = repoRoot(s.t)
	cmd.Env = append(os.Environ(), "PALAI_HOME="+s.home)
	if r, ok := stdin.(*strings.Reader); ok {
		cmd.Stdin = r
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

// docker runs `docker <args...>` with the stack env, progress on stderr, under a deadline.
func (s *drStack) docker(timeout time.Duration, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = s.composeEnv()
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	return cmd.Run()
}

// pg is the postgres container name.
func (s *drStack) pg() string { return s.project + "-postgres-1" }

// dbScalar runs a single-value SQL query over docker-exec (no host port, trust auth on the socket).
// -tAq: tuples-only + unaligned + QUIET, so an INSERT ... RETURNING prints only the returned value,
// never the "INSERT 0 1" status tag (matching the shipped tooling's `psql -tAq`).
func (s *drStack) dbScalar(sql string) (string, error) {
	out, err := exec.Command("docker", "exec", s.pg(),
		"psql", "-U", "palai", "-d", "palai", "-tAq", "-c", sql).Output()
	if err != nil {
		return "", fmt.Errorf("db query %q: %w", sql, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (s *drStack) mustScalar(sql string) string {
	s.t.Helper()
	v, err := s.dbScalar(sql)
	if err != nil {
		s.t.Fatalf("%v", err)
	}
	return v
}

// --- small local helpers (a copy, not a shared export; see the file header ponytail note) ---

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func randomHex32(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return fmt.Sprintf("%x", buf)
}
