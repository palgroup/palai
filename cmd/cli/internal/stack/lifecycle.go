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
	// The provider secret slot must exist even when unconfigured: compose bind-mounts it
	// as a file-secret and the mount fails on a missing source. `provider add` fills it.
	if err := os.WriteFile(p.secretPath("provider-one"), []byte{}, 0o600); err != nil {
		return fmt.Errorf("create provider secret slot: %w", err)
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
	fmt.Fprintf(os.Stderr, "initialised %s (project %s, api :%d)\n", p.home, cfg.Project, cfg.APIPort)
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

	// Build the reference engine image `local up` hands the runner. It is not a compose
	// service (the runner launches it per-lease through the Docker socket).
	if err := runVisible(cfg.composeEnv(p.home, engineImage), "docker", "build", "-t", engineImage, "engines/reference"); err != nil {
		return fmt.Errorf("build reference engine image: %w", err)
	}
	// The runner's lease requires an immutable sha256 image, but the locally-built engine is
	// a mutable tag (release-digest pinning is E18). Resolve the built image's id so the
	// exec-path hands the runner a digest its lease accepts rather than the tag.
	engineDigest, err := imageID(engineImage)
	if err != nil {
		return err
	}
	env := cfg.composeEnv(p.home, engineDigest)

	// A fresh one-use enrollment token for this boot.
	if err := os.WriteFile(p.runnerToken, []byte(randomHex(24)), 0o600); err != nil {
		return fmt.Errorf("mint runner token: %w", err)
	}
	if err := runVisible(env, "docker", "compose", "-p", cfg.Project, "-f", composeFile(),
		"up", "-d", "--build", "--wait"); err != nil {
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
	return runVisible(cfg.composeEnv(p.home, engineImage), "docker", "compose", "-p", cfg.Project, "-f", composeFile(),
		"down", "--remove-orphans")
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
	return runVisible(cfg.composeEnv(p.home, engineImage), "docker", "compose", "-p", cfg.Project, "-f", composeFile(),
		"down", "--volumes", "--remove-orphans")
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
