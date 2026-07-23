package stack

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/palgroup/palai/packages/version"
)

// ReleaseManifest is the scripts/release/build.sh output the upgrade reads: the target version stamp and
// the OCI image digests the swap pins. The engine digest is the alias new runs pin AFTER the roll.
type ReleaseManifest struct {
	Version string `json:"version"`
	Stamp   string `json:"stamp"`
	Commit  string `json:"commit"`
	Images  struct {
		ControlPlane imageRef `json:"control_plane"`
		Runner       imageRef `json:"runner"`
		Engine       imageRef `json:"engine"`
	} `json:"images"`
}

type imageRef struct {
	Ref    string `json:"ref"`
	Digest string `json:"digest"`
}

// UpgradeOptions drives `palai upgrade`. Manifest is the target (N+1) release manifest; From is the
// currently-running version for the compat check (defaults to the VERSION file). SkipBackup skips the
// pre-upgrade backup (a drill convenience — never the operator default).
type UpgradeOptions struct {
	Manifest   string
	From       string
	SkipBackup bool
	DrainRun   string        // optional response id to wait terminal before the engine-alias roll
	DrainWait  time.Duration // cap on the drain wait (default 90s)
}

// loadReleaseManifest reads and validates a release manifest: version + both control-plane and runner
// images (with digests) are required; the engine digest is required for the alias roll.
func loadReleaseManifest(path string) (ReleaseManifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ReleaseManifest{}, fmt.Errorf("read release manifest %s: %w", path, err)
	}
	var m ReleaseManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return ReleaseManifest{}, fmt.Errorf("decode release manifest %s: %w", path, err)
	}
	switch {
	case m.Version == "":
		return ReleaseManifest{}, fmt.Errorf("release manifest %s has no version", path)
	case m.Images.ControlPlane.Ref == "" || m.Images.ControlPlane.Digest == "":
		return ReleaseManifest{}, fmt.Errorf("release manifest %s has no control-plane image (rebuild with build.sh, not --no-images)", path)
	case m.Images.Runner.Ref == "" || m.Images.Runner.Digest == "":
		return ReleaseManifest{}, fmt.Errorf("release manifest %s has no runner image", path)
	case m.Images.Engine.Digest == "":
		return ReleaseManifest{}, fmt.Errorf("release manifest %s has no engine image digest (needed for the alias roll)", path)
	}
	return m, nil
}

// verifyUpgradeCompat is the §48.4 signature/compat check: the target must be able to run against state
// the current version wrote — i.e. current must sit inside target's §48.2 support window. A downgrade or
// a skew wider than the window is refused with the operator-facing message (the same rule the runner
// handshake enforces, reused here so a bad upgrade is caught BEFORE the swap, not after).
func verifyUpgradeCompat(target, current string) error {
	if ok, msg := version.Supported(target, current); !ok {
		return fmt.Errorf("incompatible upgrade: %s", msg)
	}
	return nil
}

// currentVersionOrFile returns the operator-supplied current version, or the repo VERSION file, or "dev".
// currentVersionOrFile resolves the current version for the compat check: the operator's --from, else the
// repo VERSION file. It ERRORS when neither is present rather than defaulting to "dev" — a "dev" current
// makes version.Supported skip the window (dev is unstamped), silently disabling the compat gate for the
// normal distributed-CLI case (run from a dir with no ./VERSION). Better to demand --from than fail open.
func currentVersionOrFile(from string) (string, error) {
	if from != "" {
		return from, nil
	}
	if raw, err := os.ReadFile("VERSION"); err == nil {
		if v := strings.TrimSpace(string(raw)); v != "" {
			return v, nil
		}
	}
	return "", fmt.Errorf("cannot determine the current version for the compat check: pass --from <version> (no ./VERSION file here)")
}

// Upgrade runs the §48.4 N->N+1 compose sequence: backup + restore-status -> compat verify -> expand +
// control-plane swap -> runner drain -> new-run engine-alias roll -> smoke. Expand is folded into the
// control-plane swap for the single-node compose profile (the swapped control-plane applies the
// idempotent, advisory-locked migration chain at boot, gated by the backup marker); the separate
// pre-swap migration Job is the Kubernetes path (T3). The active run stays on its PINNED engine because
// the engine alias is rolled ONLY AFTER the runner drains — a drained run has already completed on the
// engine digest that was current when it started.
func Upgrade(opts UpgradeOptions) error {
	cfg, p, err := loadConfig()
	if err != nil {
		return err
	}
	m, err := loadReleaseManifest(opts.Manifest)
	if err != nil {
		return err
	}
	current, err := currentVersionOrFile(opts.From)
	if err != nil {
		return err
	}
	if err := verifyUpgradeCompat(m.Version, current); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "upgrade: %s -> %s (control-plane %s)\n", current, m.Version, m.Images.ControlPlane.Ref)

	// The engine the active run is pinned to: the digest the CURRENTLY-running control-plane hands out.
	// The swap keeps this so an in-flight run's retry re-pins the SAME engine; the alias rolls to the new
	// digest only after the drain.
	oldEngine, err := currentEngineDigest(cfg)
	if err != nil {
		return fmt.Errorf("resolve current engine digest: %w", err)
	}
	// Read the running stack's runtime config ONCE, before any recreate resets it to a compose default.
	preserved := preservedRuntimeEnv(cfg)

	// 1. backup + restore-status: the pre-upgrade restore point (§48.4). It is taken BEFORE the swap so a
	// failed migration can be rolled back to it. Skipped only by an explicit drill flag. The boot-time
	// require-backup PREFLIGHT gate (PALAI_MIGRATE_REQUIRE_BACKUP) is deliberately NOT wired here: its
	// marker must be readable INSIDE the control-plane container, and the compose profile mounts no backup
	// volume — that gate is the operator/K8s-Job option (a mounted marker path), documented in upgrade.md.
	if !opts.SkipBackup {
		backupPath := fmt.Sprintf("%s/palai-upgrade-backup-%s.tar.gz", p.home, time.Now().UTC().Format("20060102T150405Z"))
		if err := InstallBackup(backupPath); err != nil {
			return fmt.Errorf("pre-upgrade backup: %w", err)
		}
		fmt.Fprintf(os.Stderr, "upgrade: backup captured at %s\n", backupPath)
	}

	// 2. control-plane swap (expand folded in). The old control-plane gets SIGTERM and drains its gateway;
	// the new one boots, applies the idempotent migration chain, and serves. Keep the OLD engine so an
	// interrupted run re-pins it.
	swapEnv := upgradeComposeEnv(cfg, p.home, oldEngine, m.Images.ControlPlane.Ref, currentRunnerImage(cfg), preserved)
	if err := runVisible(swapEnv, "docker", "compose", "-p", cfg.Project, "-f", composeFile(),
		"up", "-d", "--wait", "control-plane"); err != nil {
		return fmt.Errorf("control-plane swap: %w", err)
	}
	if err := waitForAPI(cfg, p); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "upgrade: control-plane swapped + migrations applied")

	// 3. runner drain: swap the runner to N+1. Recreating it drains the old runner; any run interrupted by
	// the swap is reclaimed and completed by the E10 recovery layer on the new control-plane.
	drainEnv := upgradeComposeEnv(cfg, p.home, oldEngine, m.Images.ControlPlane.Ref, m.Images.Runner.Ref, preserved)
	if err := runVisible(drainEnv, "docker", "compose", "-p", cfg.Project, "-f", composeFile(),
		"up", "-d", "--wait", "runner"); err != nil {
		return fmt.Errorf("runner drain/swap: %w", err)
	}

	// 4. Wait for the pre-upgrade active run to finish BEFORE rolling the engine, so it completes on its
	// pinned (old) engine and is never migrated onto the new one.
	if err := drainActiveRun(cfg, p, opts); err != nil {
		return err
	}

	// 5. new-run engine-alias roll: recreate the control-plane pointing PALAI_ENGINE_IMAGE at the new
	// engine digest, so runs started FROM NOW pin the new engine. Active runs already drained on the old.
	rollEnv := upgradeComposeEnv(cfg, p.home, m.Images.Engine.Digest, m.Images.ControlPlane.Ref, m.Images.Runner.Ref, preserved)
	if err := runVisible(rollEnv, "docker", "compose", "-p", cfg.Project, "-f", composeFile(),
		"up", "-d", "--wait", "control-plane"); err != nil {
		return fmt.Errorf("engine-alias roll: %w", err)
	}
	if err := waitForAPI(cfg, p); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "upgrade: engine alias rolled to %s (new runs only)\n", short(m.Images.Engine.Digest))

	// 6. smoke: a fake response admits and the surface answers. A real-provider smoke is the drill's job
	// (credential via .env.local), never wired into the CLI.
	if err := upgradeSmoke(cfg, p); err != nil {
		return fmt.Errorf("post-upgrade smoke: %w", err)
	}
	fmt.Fprintf(os.Stderr, "upgrade complete: now on %s\n", m.Version)
	return nil
}

// RollbackOptions drives `palai upgrade rollback`: To is the N (previous) release manifest to return to.
type RollbackOptions struct {
	To string
}

// UpgradeRollback is the §48.5 APPLICATION rollback: it swaps the control-plane (and runner) image back
// to N and rolls the engine alias back to N's engine for NEW runs, while the SCHEMA stays expanded (a
// contract's dropped shape is not re-created — a real downgrade past a contract restores from the
// pre-upgrade backup, spec §48.5). The N binary boots on the expanded schema because a contract only
// drops shapes no in-rollback-window binary reads, so the schema head equals N's chain head. It DRAINS
// first: the runner recreate SIGTERMs the active run, and without a drain E10 recovery would reopen that
// attempt on the N control-plane's N engine — silently MIGRATING a run pinned to the N+1 engine. So an
// active run is drained (warn-on-timeout) before the recreate; a run still active past the window then
// completes on N's engine, and the operator is warned (never silent).
func UpgradeRollback(opts RollbackOptions) error {
	cfg, p, err := loadConfig()
	if err != nil {
		return err
	}
	m, err := loadReleaseManifest(opts.To)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "rollback: control-plane -> %s (schema stays expanded)\n", m.Images.ControlPlane.Ref)

	// Read the runtime config to preserve BEFORE any recreate resets it, then drain the active run so it is
	// not silently migrated onto N's engine by the recreate's SIGTERM + E10 recovery.
	preserved := preservedRuntimeEnv(cfg)
	if err := drainActiveRun(cfg, p, UpgradeOptions{}); err != nil {
		return err
	}

	// Swap the control-plane + runner image back to N and roll the engine alias back to N's engine.
	env := upgradeComposeEnv(cfg, p.home, m.Images.Engine.Digest, m.Images.ControlPlane.Ref, m.Images.Runner.Ref, preserved)
	if err := runVisible(env, "docker", "compose", "-p", cfg.Project, "-f", composeFile(),
		"up", "-d", "--wait", "control-plane", "runner"); err != nil {
		return fmt.Errorf("rollback swap: %w", err)
	}
	if err := waitForAPI(cfg, p); err != nil {
		return err
	}
	if err := upgradeSmoke(cfg, p); err != nil {
		return fmt.Errorf("post-rollback smoke: %w", err)
	}
	fmt.Fprintf(os.Stderr, "rollback complete: control-plane on %s, schema expanded\n", m.Version)
	return nil
}

// upgradeComposeEnv is composeEnv plus the E15 T2 image overrides the swap/roll set, plus the preserved
// runtime env. cpImage and runnerImage pin the control-plane/runner service to a specific image TAG
// (compose resolves a tag, not a bare image id); engine is the PALAI_ENGINE_IMAGE alias new leases carry
// (an immutable sha256 digest — the runner rejects a mutable tag). preserved carries the running stack's
// runtime config forward so a recreate does not silently reset it to a compose default (see
// preservedRuntimeEnv). The require-backup preflight gate is intentionally not set here (see Upgrade's
// step 1 comment).
func upgradeComposeEnv(cfg Config, home, engine, cpImage, runnerImage string, preserved []string) []string {
	env := append(cfg.composeEnv(home, engine),
		"PALAI_CONTROL_PLANE_IMAGE="+cpImage,
		"PALAI_RUNNER_IMAGE="+runnerImage,
	)
	return append(env, preserved...)
}

// cpRuntimeVars / runnerRuntimeVars are the runtime-behaviour env vars the compose file DEFAULTS — so a
// swap/roll/rollback that does NOT carry them forward silently resets them: dispatch off (exec-path dead),
// provider back to fake, retention reaper disabled, or runner concurrency back to 1 (a delegation stack's
// inline-child runs then DEADLOCK, §25.18/E08 T5). They are split PER SERVICE because each var lives only
// in its own service's compose env: PALAI_RUNNER_CONCURRENCY is on the RUNNER, the rest on the
// control-plane — reading a runner-scoped var off the control-plane container always misses it.
var cpRuntimeVars = []string{
	"PALAI_DISPATCH_WORKERS", "PALAI_MODEL_PROVIDER", "PALAI_MODEL",
	"PALAI_RETENTION_STORE_FALSE_TTL", "PALAI_RUNNER_CERT_TTL",
}
var runnerRuntimeVars = []string{"PALAI_RUNNER_CONCURRENCY"}

// preservedRuntimeEnv reads the running stack's runtime config so an upgrade preserves it instead of
// resetting to the compose defaults. Each var is read from the CONTAINER THAT CARRIES IT (control-plane vs
// runner); one absent from its container is skipped (its default applies). Read ONCE at the start of an
// upgrade — the first recreate would otherwise overwrite it with the default.
func preservedRuntimeEnv(cfg Config) []string {
	var out []string
	read := func(container string, keys []string) {
		for _, key := range keys {
			if v, err := inspectContainerEnv(container, key); err == nil {
				out = append(out, key+"="+v)
			}
		}
	}
	read(cfg.containerName("control-plane"), cpRuntimeVars)
	read(cfg.containerName("runner"), runnerRuntimeVars)
	return out
}

// currentEngineDigest reads PALAI_ENGINE_IMAGE from the running control-plane container's environment —
// the digest new leases currently pin, which the swap preserves so an interrupted run re-pins the SAME
// engine.
func currentEngineDigest(cfg Config) (string, error) {
	return inspectContainerEnv(cfg.containerName("control-plane"), "PALAI_ENGINE_IMAGE")
}

// currentRunnerImage reads the running runner container's image reference so a control-plane-only swap
// does not accidentally recreate the runner with a different image. Falls back to the compose default.
func currentRunnerImage(cfg Config) string {
	// .Config.Image is the image REFERENCE the container was created with (a tag), which compose can
	// interpolate back into the service; .Image would be the resolved image ID.
	out, err := exec.Command("docker", "inspect", cfg.containerName("runner"), "--format", "{{.Config.Image}}").Output()
	if err != nil {
		return "palai/runner:local"
	}
	if ref := strings.TrimSpace(string(out)); ref != "" {
		return ref
	}
	return "palai/runner:local"
}

// inspectContainerEnv extracts one VAR=value from a container's Config.Env.
func inspectContainerEnv(container, key string) (string, error) {
	out, err := exec.Command("docker", "inspect", container, "--format", "{{range .Config.Env}}{{println .}}{{end}}").Output()
	if err != nil {
		return "", fmt.Errorf("inspect %s: %w", container, err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), key+"="); ok {
			return v, nil
		}
	}
	return "", fmt.Errorf("%s not set on %s", key, container)
}

// drainActiveRun waits for the pre-upgrade active run to reach a terminal status before the engine rolls,
// so it completes on its pinned engine. With an explicit --drain-run it polls that response; otherwise it
// polls the response list for any non-terminal run. A timeout is a warning, not a failure: an unfinished
// run then completes on the new engine (a documented degradation), and the operator sees it.
func drainActiveRun(cfg Config, p paths, opts UpgradeOptions) error {
	wait := opts.DrainWait
	if wait <= 0 {
		wait = 90 * time.Second
	}
	key, err := readTrimmed(p.apiKey)
	if err != nil {
		return fmt.Errorf("read api key: %w", err)
	}
	deadline := time.Now().Add(wait)
	client := &http.Client{Timeout: 5 * time.Second}
	for {
		var active bool
		if opts.DrainRun != "" {
			active = !runIsTerminal(client, cfg.BaseURL, key, opts.DrainRun)
		} else {
			active = anyRunActive(client, cfg.BaseURL, key)
		}
		if !active {
			fmt.Fprintln(os.Stderr, "upgrade: runner drained (no active run)")
			return nil
		}
		if time.Now().After(deadline) {
			fmt.Fprintln(os.Stderr, "upgrade: WARNING drain timed out with a run still active; it will complete on the NEW engine")
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// terminalResponseStatus is the set of response projection statuses that mean the run is done.
var terminalResponseStatus = map[string]bool{
	"completed": true, "failed": true, "cancelled": true, "canceled": true, "incomplete": true, "expired": true,
}

func runIsTerminal(client *http.Client, baseURL, key, id string) bool {
	status, ok := getJSONField(client, baseURL+"/v1/responses/"+id, key, "status")
	return ok && terminalResponseStatus[status]
}

// anyRunActive reports whether a PINNED (dispatched, in_progress) run is still executing — the runs a roll
// would silently migrate. It filters SERVER-SIDE by status=in_progress rather than scanning a newest-first
// page: an unfiltered ?limit=100 would read an active run beyond the newest 100 (a page full of completed
// runs) as "quiesced" and cut the roll under it. A queued run is deliberately NOT counted — it holds no
// lease and pins no engine, so it dispatches fresh on whatever engine is current when it runs.
func anyRunActive(client *http.Client, baseURL, key string) bool {
	const limit = 100
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/v1/responses?status=in_progress&limit=%d", baseURL, limit), nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		return false // an unreachable API is not an "active run" — the caller's waitForAPI already gated this
	}
	defer resp.Body.Close()
	var page struct {
		Data []json.RawMessage `json:"data"`
	}
	if json.NewDecoder(resp.Body).Decode(&page) != nil {
		return false
	}
	// Every returned row is in_progress by the filter; a full page means there may be even more, so either
	// way a non-empty result is an active run.
	return len(page.Data) > 0
}

// upgradeSmoke admits one fake response and waits for it to reach a terminal status — the post-swap /
// post-rollback health gate that the exec-path answers end to end. It uses the fake provider (no
// credential); the real-provider smoke is the drill's, over .env.local.
func upgradeSmoke(cfg Config, p paths) error {
	key, err := readTrimmed(p.apiKey)
	if err != nil {
		return fmt.Errorf("read api key: %w", err)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	id, err := admitResponse(client, cfg.BaseURL, key, "upgrade smoke")
	if err != nil {
		return err
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		if runIsTerminal(client, cfg.BaseURL, key, id) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("smoke response %s did not reach a terminal status in 60s", id)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// admitResponse POSTs a fake response and returns its id.
func admitResponse(client *http.Client, baseURL, key, input string) (string, error) {
	body, _ := json.Marshal(map[string]string{"input": input})
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/v1/responses", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Idempotency-Key", "upgrade-"+randomHex(12))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("admit smoke response: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return "", fmt.Errorf("admit smoke response: status %d", resp.StatusCode)
	}
	var env struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil || env.ID == "" {
		return "", fmt.Errorf("admit smoke response: no id in envelope")
	}
	return env.ID, nil
}

// getJSONField GETs a JSON object and returns one top-level string field.
func getJSONField(client *http.Client, url, key, field string) (string, bool) {
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	var obj map[string]any
	if json.NewDecoder(resp.Body).Decode(&obj) != nil {
		return "", false
	}
	v, ok := obj[field].(string)
	return v, ok
}

// short trims a sha256:... digest to a readable prefix for logging.
func short(digest string) string {
	d := strings.TrimPrefix(digest, "sha256:")
	if len(d) > 12 {
		return d[:12]
	}
	return d
}
