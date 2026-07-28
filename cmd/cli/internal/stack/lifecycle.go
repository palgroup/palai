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
	for _, path := range []string{p.secretPath("provider-one")} {
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

// Up builds the images, mints a fresh one-use runner enrollment token, brings the four
// services up with compose --wait, and blocks until the API answers. The token is
// re-minted every Up so a repeated boot never reuses a spent identity (LP-012).
func Up() error {
	cfg, p, err := loadConfig()
	if err != nil {
		return err
	}
	if err := ensureSecretSlots(p); err != nil {
		return err
	}

	env := cfg.composeEnv(p.home, engineImage)
	upArgs := []string{"up", "-d", "--wait"}
	if root, fromSource := buildContext(p.compose); fromSource {
		// Build the reference engine image `local up` hands the runner. It is not a compose
		// service (the runner launches it per-lease through the Docker socket).
		if err := runVisible(env, "docker", "build", "-t", engineImage, filepath.Join(root, "engines", "reference")); err != nil {
			return fmt.Errorf("build reference engine image: %w", err)
		}
		// The runner's lease requires an immutable sha256 image, but the locally-built engine is
		// a mutable tag (release-digest pinning is E18). Resolve the built image's id so the
		// exec-path hands the runner a digest its lease accepts rather than the tag.
		engineDigest, err := imageID(engineImage)
		if err != nil {
			return err
		}
		env = cfg.composeEnv(p.home, engineDigest)
		upArgs = append(upArgs, "--build")
	} else {
		// Packaged: there are no Dockerfiles and no engine source on disk, so nothing here can be
		// built. Every image must already exist — named by the operator's PALAI_*_IMAGE overrides or
		// by this build's published defaults — and compose is told explicitly not to try building
		// one, because its `build.context: ../..` points at a repo root that is not there.
		if env, err = packagedImageEnv(cfg, p.home); err != nil {
			return err
		}
		upArgs = append(upArgs, "--no-build")
	}

	// A fresh one-use enrollment token for this boot.
	if err := os.WriteFile(p.runnerToken, []byte(randomHex(24)), 0o600); err != nil {
		return fmt.Errorf("mint runner token: %w", err)
	}
	if err := runVisible(env, "docker", append([]string{"compose", "-p", cfg.Project, "-f", p.compose}, upArgs...)...); err != nil {
		return fmt.Errorf("compose up: %w", err)
	}
	if err := waitForAPI(cfg, p); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "stack up: api %s, runner :%d\n", cfg.BaseURL, cfg.RunnerPort)
	return nil
}

// Down stops the stack, RETAINING the named volumes so a subsequent Up serves the same
// data back (spec §44; LP-012).
func Down() error {
	cfg, p, err := loadConfig()
	if err != nil {
		return err
	}
	if err := runVisible(cfg.composeEnv(p.home, engineImage), "docker", "compose", "-p", cfg.Project, "-f", p.compose,
		"down", "--remove-orphans"); err != nil {
		return err
	}
	return sweepEngineContainers(cfg.Project)
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
func waitForAPI(cfg Config, p paths) error {
	key, err := readTrimmed(p.apiKey)
	if err != nil {
		return fmt.Errorf("read api key: %w", err)
	}
	deadline := time.Now().Add(30 * time.Second)
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
			return fmt.Errorf("control-plane API did not become ready at %s", cfg.BaseURL)
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

// randomHex returns n random bytes hex-encoded — used for the API key, pg password, and
// one-use runner token.
func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(buf)
}
