package stack

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Init generates the .palai layout: the bootstrap API key, the Postgres password, the
// local CA and gateway server certificate, an empty provider secret slot, and
// config.json with freshly reserved loopback ports. It is a no-op when the stack is
// already initialised, so it never clobbers the credentials a running stack depends on.
func Init() error {
	p, err := resolvePaths()
	if err != nil {
		return err
	}
	if _, err := os.Stat(p.config); err == nil {
		fmt.Fprintf(os.Stderr, "already initialised at %s\n", p.home)
		return nil
	}
	for _, dir := range []string{p.home, p.caDir, p.secretsDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}

	apiKey := "palai-" + randomHex(24)
	if err := os.WriteFile(p.apiKey, []byte(apiKey), 0o600); err != nil {
		return fmt.Errorf("write api key: %w", err)
	}
	if err := os.WriteFile(p.pgPassword, []byte(randomHex(24)), 0o600); err != nil {
		return fmt.Errorf("write pg password: %w", err)
	}
	if err := ensureSecretSlots(p); err != nil {
		return err
	}
	if err := writeLocalCA(p); err != nil {
		return err
	}

	ports, err := freePorts(4)
	if err != nil {
		return err
	}
	cfg := Config{
		Project:       "palai-" + randomHex(4),
		DataDir:       p.home,
		APIPort:       ports[0],
		RunnerPort:    ports[1],
		PgPort:        ports[2],
		S3Port:        ports[3],
		BaseURL:       fmt.Sprintf("http://127.0.0.1:%d", ports[0]),
		ControllerDNS: controllerDNS,
	}
	if err := writeConfig(p.config, cfg); err != nil {
		return err
	}
	// On a packaged binary this writes the embedded deploy files into ${PALAI_HOME}/compose, so the
	// compose files and the env examples an operator needs are there right after `init` rather than
	// appearing on the first command that happens to drive compose. In a checkout it resolves to the
	// committed tree and writes nothing.
	if _, err := deployDir(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "initialised %s (project %s, api :%d)\n", p.home, cfg.Project, cfg.APIPort)
	return nil
}

// ensureSecretSlots creates the file-secret sources compose bind-mounts, and never touches one that
// already holds a value. A missing source fails `compose up` outright, which is why the provider-one
// slot has always been created unconfigured.
//
// It runs on every bring-up, not only on `init`: Init short-circuits on an existing config.json, so
// a .palai written by an earlier build has only the slots that existed then and would otherwise be
// unable to start at all. That also makes it the ONE funnel every compose bring-up routes through —
// `palai local up` calls it directly, `palai up` reaches it through Up() — which is why the master
// key is minted HERE (E21 T2, §3.6 D5) and no longer on the Slack path.
//
// The master-key slot is MINTED rather than left empty. Empty was safe only while compose named the
// key conditionally; compose now names it on every stack, and the control-plane treats a set-but-
// unparseable key file as log.Fatalf by design. An empty slot would no longer mean "secret store
// off" — it would mean "this stack never comes up".
func ensureSecretSlots(p paths) error {
	if err := os.MkdirAll(p.secretsDir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", p.secretsDir, err)
	}
	if err := ensureMasterKey(p); err != nil {
		return err
	}
	// github-app-key joins provider-one as a slot that exists EMPTY on every stack: compose names it as a
	// file secret unconditionally, and a missing mount source fails `compose up` outright. Empty is the
	// correct unconfigured state here (unlike the master key) because main.gitHubAppPublisherFromEnv never
	// reads the path until PALAI_GITHUB_APP_ID and the installation id are BOTH set — so an empty slot means
	// "no deployment-global App", not "no publisher" and not "this stack cannot boot": a binding carrying its
	// own connection_ref publishes without one. applyGitHubAppEnv fills it when the operator configured an App.
	for _, path := range []string{p.secretPath("provider-one"), p.secretPath(gitHubAppKeySlot)} {
		switch _, err := os.Stat(path); {
		case err == nil:
			continue
		case !os.IsNotExist(err):
			return fmt.Errorf("stat %s: %w", path, err)
		}
		if err := os.WriteFile(path, []byte{}, 0o600); err != nil {
			return fmt.Errorf("create secret slot %s: %w", path, err)
		}
	}
	return nil
}

// Up is `palai local up`: the compose bring-up, and then the repository binding a session codes
// against.
//
// THE BINDING USED TO BE `palai up`'s ALONE, AND THAT WAS A GAP RATHER THAN A DIVISION OF LABOUR.
// Measured 2026-08-04:
//
//	grep -rn "resolveRepository" --include='*.go' cmd/ | grep -v _test   -> up.go:229 only
//	grep -c  "resolveRepository" cmd/cli/internal/stack/lifecycle.go     -> 0
//
// and `palai up` REFUSES the deterministic adapter by name (resolveProvider), pointing the operator
// at `palai local up` — so the one bring-up that serves a credential-less stack was also the one
// that could not give it a repository. A local stack that cannot name a repository cannot clone,
// edit or commit, and nothing said why.
//
// It calls the SAME resolveRepository `palai up` does rather than deriving a binding a second time:
// two composition roots deriving one thing is how they end up disagreeing, which is the defect A.3
// T3 consolidated the shell posture to fix. What differs is only the reporting — `palai up` folds
// the outcome into its own report, so it drives composeUp below and resolves the binding itself.
func Up() error {
	if err := composeUp(); err != nil {
		return err
	}
	cfg, p, err := loadConfig()
	if err != nil {
		return err
	}
	// os.Getenv, not a dotenv reader: `palai local up` has never read .env.local (composeEnv is
	// os.Environ plus six keys), so the two values arrive the way every other one on this path does.
	return reportRepositoryBinding(cfg, p, os.Getenv)
}

// composeUp builds the images, mints a fresh runner enrollment token, brings the four services up with
// compose --wait, and blocks until the API answers. The token is re-minted every boot so a repeated one
// never inherits the previous boot's credential (LP-012).
//
// THE TOKEN IS NOT ONE-USE, AND THIS COMMENT USED TO SAY IT WAS (§3.6 D4, corrected in E24 T3): the
// control plane admits it once per issued-certificate lifetime and the runner re-presents it to recover
// an identity that has already expired, which is the only path back for a machine that missed its
// renewal window. See execution.FileEnrollmentTokens.
func composeUp() error {
	cfg, p, err := loadConfig()
	if err != nil {
		return err
	}
	if err := ensureSecretSlots(p); err != nil {
		return err
	}

	env, buildArgs, err := composeRunEnv(cfg, p)
	if err != nil {
		return err
	}

	// A fresh enrollment token for this boot — re-minted rather than reused, so a credential does not
	// outlive the stack it was minted for. It is re-presentable within its own boot (the runner's
	// expired-identity recovery path depends on that), not one-use.
	if err := os.WriteFile(p.runnerToken, []byte(randomHex(24)), 0o600); err != nil {
		return fmt.Errorf("mint runner token: %w", err)
	}
	upArgs := append([]string{"up", "-d", "--wait"}, buildArgs...)
	if err := runVisible(env, "docker", append([]string{"compose", "-p", cfg.Project, "-f", p.compose}, upArgs...)...); err != nil {
		return fmt.Errorf("compose up: %w", err)
	}
	if err := waitForAPI(cfg, p); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "stack up: api %s, runner :%d\n", cfg.BaseURL, cfg.RunnerPort)
	return nil
}

// composeRunEnv resolves the compose interpolation environment for a bring-up and the build
// argument that goes with it. It is the ONE place the reference engine image is built and resolved
// to the digest the runner's lease requires, shared by the container bring-up above and the native
// one (native.go) — which needs the same digest for a control plane that is not a compose service.
func composeRunEnv(cfg Config, p paths) (env []string, buildArgs []string, err error) {
	env = cfg.composeEnv(p.home, engineImage)
	if root, fromSource := buildContext(p.compose); fromSource {
		// Build the reference engine image `local up` hands the runner. It is not a compose
		// service (the runner launches it per-lease through the Docker socket).
		if err := runVisible(env, "docker", "build", "-t", engineImage, filepath.Join(root, "engines", "reference")); err != nil {
			return nil, nil, fmt.Errorf("build reference engine image: %w", err)
		}
		// The runner's lease requires an immutable sha256 image, but the locally-built engine is
		// a mutable tag (release-digest pinning is E18). Resolve the built image's id so the
		// exec-path hands the runner a digest its lease accepts rather than the tag.
		engineDigest, err := imageID(engineImage)
		if err != nil {
			return nil, nil, err
		}
		return cfg.composeEnv(p.home, engineDigest), []string{"--build"}, nil
	}
	// Packaged: there are no Dockerfiles and no engine source on disk, so nothing here can be
	// built. Every image must already exist — named by the operator's PALAI_*_IMAGE overrides or
	// by this build's published defaults — and compose is told explicitly not to try building
	// one, because its `build.context: ../..` points at a repo root that is not there.
	if env, err = packagedImageEnv(cfg, p.home); err != nil {
		return nil, nil, err
	}
	return env, []string{"--no-build"}, nil
}

// Down stops the stack, RETAINING the named volumes so a subsequent Up serves the same
// data back (spec §44; LP-012).
func Down() error {
	cfg, p, err := loadConfig()
	if err != nil {
		return err
	}
	// The NATIVE control plane first, and unconditionally: it is not a compose service, so
	// `compose down` cannot see it, and a control plane still holding the runner port after a
	// teardown is worse than one that never started. A stack that was never native has no record
	// and this is a no-op (E22 T5).
	if err := downNative(p); err != nil {
		return err
	}
	if err := runVisible(cfg.composeEnv(p.home, engineImage), "docker", "compose", "-p", cfg.Project, "-f", p.compose,
		"down", "--remove-orphans"); err != nil {
		return err
	}
	return sweepEngineContainers(cfg.Project)
}

// downNative stops a native control plane and a native runner and says so, or does nothing at all.
//
// THE RUNNER GOES FIRST, which is the reverse of the bring-up and is the order that leaves nothing
// running: since A.3 the runner is the process an `xcodebuild` hangs off, and it dials the control plane
// rather than the other way round. Stopping the control plane first would leave that compiler running
// under a machine whose controller has gone, for as long as it takes to reach the next line.
//
// Each is reported separately because a stack can have one without the other — a bring-up that failed
// after the control plane started leaves exactly that — and "stopped the native stack" would be a
// sentence that is true of two different states.
func downNative(p paths) error {
	stoppedRunner, err := stopNativeRunner(p)
	if err != nil {
		return err
	}
	if stoppedRunner {
		fmt.Fprintln(os.Stderr, "stopped the native runner")
	}
	stopped, err := stopNative(p)
	if err != nil {
		return err
	}
	if stopped {
		fmt.Fprintln(os.Stderr, "stopped the native control plane")
	}
	return nil
}

// Reset tears the stack down and DELETES its volumes — the destructive path. It refuses
// without --confirm (a non-zero exit that removes nothing), so data is never dropped by a
// bare `reset` (spec §44.4). The .palai identity is retained so the same project can be
// brought back up.
func Reset(confirm bool) error {
	cfg, p, err := loadConfig()
	if err != nil {
		return err
	}
	if !confirm {
		return fmt.Errorf("refusing to delete volumes without --confirm")
	}
	if err := downNative(p); err != nil {
		return err
	}
	if err := runVisible(cfg.composeEnv(p.home, engineImage), "docker", "compose", "-p", cfg.Project, "-f", p.compose,
		"down", "--volumes", "--remove-orphans"); err != nil {
		return err
	}
	return sweepEngineContainers(cfg.Project)
}

// buildContext returns the repo root a from-source bring-up builds from, and whether it is actually
// there. compose.yaml declares `build.context: ../..` relative to the compose FILE, so the same
// two-levels-up walk answers for the reference engine too — which also makes the answer independent
// of cwd, where `docker build engines/reference` used to require the caller to stand at the repo root.
//
// Absent (a packaged binary driving the embedded files materialised under ${PALAI_HOME}) means the
// bring-up MUST NOT pass --build: there is nothing to build from, and compose would otherwise try
// and fail on a context path that does not exist.
func buildContext(composePath string) (string, bool) {
	abs, err := filepath.Abs(composePath)
	if err != nil {
		return "", false
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(abs), "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "engines", "reference", "Dockerfile")); err != nil {
		return "", false
	}
	return root, true
}

// packagedEngineDigest resolves the reference-engine reference into the form the runner's lease will
// accept. The lease validator requires a BARE sha256 config digest (packages/runner/session.go's
// imageDigestPattern) — a repository ref with a tag is rejected — so a reference that is not already
// one is resolved through the local daemon, pulling it once if it is not present. Getting this wrong
// is not a bring-up failure: the stack comes up healthy and then every run fails at lease time.
func packagedEngineDigest() (string, error) {
	ref, err := stackImage("reference-engine", "PALAI_ENGINE_IMAGE")
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(ref, "sha256:") {
		return ref, nil
	}
	if digest, err := imageID(ref); err == nil {
		return digest, nil
	}
	if err := runVisible(os.Environ(), "docker", "pull", ref); err != nil {
		return "", fmt.Errorf("pull the engine image %s: %w", ref, err)
	}
	return imageID(ref)
}

// engineSandboxLabel marks a runner-launched engine container, mirroring packages/runner's
// io.palai.sandbox=engine. The sweep pairs it with the compose project (io.palai.project) so
// only this stack's engines are removed.
const engineSandboxLabel = "io.palai.sandbox=engine"

// sweepEngineContainers force-removes the engine sandbox containers this stack's runner
// launched through the Docker socket. They are not compose services, so `compose down` never
// tracks them — a mid-run-killed engine would otherwise leak. Filtering by the compose
// project keeps a concurrent stack's engines untouched. An empty result is not an error.
func sweepEngineContainers(project string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "ps", "-aq",
		"--filter", "label="+engineSandboxLabel,
		"--filter", "label=io.palai.project="+project).Output()
	if err != nil {
		return fmt.Errorf("list engine containers: %w", err)
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return nil
	}
	return runVisible(os.Environ(), "docker", append([]string{"rm", "-f"}, ids...)...)
}

// waitForAPI polls GET /v1/capabilities with the bootstrap key until it answers 200 or the
// deadline elapses. compose --wait already gated on the control-plane healthcheck, so this
// is a short belt-and-suspenders wait for the authenticated surface.
// readyTimeout is how long a bring-up waits for the control-plane API, default 90s and overridable
// with PALAI_STACK_READY_TIMEOUT (any time.ParseDuration string). A malformed value is a refusal, not
// a silent fallback: an operator who set it meant to change this number.
func readyTimeout() (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv("PALAI_STACK_READY_TIMEOUT"))
	if v == "" {
		return 90 * time.Second, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("PALAI_STACK_READY_TIMEOUT=%q is not a duration (try 90s, 3m): %w", v, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("PALAI_STACK_READY_TIMEOUT=%q must be positive", v)
	}
	return d, nil
}

func waitForAPI(cfg Config, p paths) error {
	key, err := readTrimmed(p.apiKey)
	if err != nil {
		return fmt.Errorf("read api key: %w", err)
	}
	// THE MACHINE THIS RUNS ON IS THE PRODUCT, so the deadline has to be calibratable. A Mac hosting
	// concurrent sessions is by definition a LOADED Mac, and a fixed 30s was measured too tight on a
	// 12-core box under load: the native control plane needed 34s to serve /v1/capabilities (migrations
	// against a containerised Postgres, plus Slack socket init), so `palai up --native` failed and never
	// started the runner — leaving a stack that looked broken but was merely slow.
	// ponytail: one env knob, no config file — a fixed number cannot be right on every box.
	wait, err := readyTimeout()
	if err != nil {
		return err
	}
	deadline := time.Now().Add(wait)
	client := &http.Client{Timeout: 3 * time.Second}
	for {
		req, _ := http.NewRequest(http.MethodGet, cfg.BaseURL+"/v1/capabilities", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("control-plane API did not become ready at %s within %s "+
				"(a loaded machine can need longer: set PALAI_STACK_READY_TIMEOUT=3m and re-run)", cfg.BaseURL, wait)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// imageID resolves a built image's immutable id (sha256:...) — the digest the runner's lease
// offer requires, since the locally-built engine tag is mutable (release-digest pinning is
// E18). It captures stdout directly rather than routing it to stderr.
func imageID(ref string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "image", "inspect", ref, "--format", "{{.Id}}").Output()
	if err != nil {
		return "", fmt.Errorf("resolve %s image id: %w", ref, err)
	}
	id := strings.TrimSpace(string(out))
	if !strings.HasPrefix(id, "sha256:") {
		return "", fmt.Errorf("image %s id %q is not a sha256 digest", ref, id)
	}
	return id, nil
}

// runVisible runs a command with progress routed to stderr, keeping stdout clean for the
// structured output (`doctor --json`, `response create`) the harness parses.
func runVisible(env []string, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// writeConfig writes config.json at 0600.
func writeConfig(path string, cfg Config) error {
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

// randomHex returns n random bytes hex-encoded — used for the API key, pg password, and the runner
// enrollment token.
func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(buf)
}
