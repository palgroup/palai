package stack

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// native.go is `palai up --native`: Postgres, the object store and the runner in Docker, and the
// control plane as a PROCESS ON THIS MACHINE. That combination is what a Mac deployment IS (E22 T1)
// — the control-plane binary compiles for darwin/arm64, so reaching `xcodebuild`, `xcrun simctl` and
// `axe` needs no worker protocol and no typed capability, it needs the control plane to BE on the Mac.
//
// deploy/compose/native-control-plane.yml already stated the three facts that make it work, in
// prose. This file is where they stop being the operator's memory: each one is CHECKED before any
// Docker work, and each refusal names the variable that is wrong. Getting any of them wrong used to
// surface twenty seconds later as a TLS error, a DNS failure or an empty workspace inside a
// container nobody was reading.
//
// The shell POSTURE is deliberately not decided here. `--native` says where the control plane runs;
// PALAI_SHELL_NATIVE=unsandboxed-host says the shell boundary is gone. Folding the second into the
// first would make switching off a security boundary reachable by the reflex that switches on a
// feature, which docs/operations/palai-on-a-mac.md §1 refuses on purpose. An unset posture is a
// WARNING here, not a default.

// nativeOverlayFile is the compose overlay this posture applies, beside the compose file the CLI
// already resolved (checkout, PALAI_COMPOSE_FILE, or the materialised embedded copies).
const nativeOverlayFile = "native-control-plane.yml"

// nativeShellPosture is the one string a Palai process accepts for the host shell posture. Named here
// so the operator warning cannot drift from what posture.Resolve actually takes — and since A.3 that
// derivation is read by the RUNNER as well as the control plane, because the runner is what executes
// the command.
const nativeShellPosture = "unsandboxed-host"

// UpNative brings the native posture up and returns the operator-facing posture line for the report.
//
// THE ORDER IS THE POINT. The overlay resets the runner's `depends_on`, so compose has nothing left
// to make it wait — and cmd/runner/main.go log.Fatalf's on a failed enroll with no restart policy
// behind it. A runner started before the control plane listens is therefore a DEAD container, not a
// retrying one. So: the two backing services first, then the control plane (which is a CLIENT of
// them), then the runner.
func UpNative(get func(string) string) (string, error) {
	cfg, p, err := loadConfig()
	if err != nil {
		return "", err
	}
	overlay := filepath.Join(filepath.Dir(p.compose), nativeOverlayFile)
	if _, err := os.Stat(overlay); err != nil {
		return "", fmt.Errorf("the native overlay is not beside the compose file (%s): %w", overlay, err)
	}
	listen, err := nativeRunnerListen(get, cfg.RunnerPort)
	if err != nil {
		return "", err
	}
	root, err := nativeWorkspaceRoot(get)
	if err != nil {
		return "", err
	}
	// The overlay interpolates ${PALAI_WORKSPACE_ROOT} on BOTH sides of the runner's bind, and the
	// native control plane must name the same string. Exported once, here, so the two cannot differ.
	if err := os.Setenv("PALAI_WORKSPACE_ROOT", root); err != nil {
		return "", err
	}
	bin, err := nativeBinary(p)
	if err != nil {
		return "", err
	}
	if err := ensureSecretSlots(p); err != nil {
		return "", err
	}
	env, upArgs, err := composeRunEnv(cfg, p)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(p.runnerToken, []byte(randomHex(24)), 0o600); err != nil {
		return "", fmt.Errorf("mint runner token: %w", err)
	}

	files := []string{"compose", "-p", cfg.Project, "-f", p.compose, "-f", overlay}
	if err := runVisible(env, "docker", append(append(append([]string{}, files...), "up", "-d", "--wait"), nativeComposeServices()...)...); err != nil {
		return "", fmt.Errorf("compose up (%s): %w", strings.Join(nativeComposeServices(), ", "), err)
	}

	pid, err := startNative(cfg, p, bin, env, listen, root, get)
	if err != nil {
		return "", err
	}

	// THE RUNNER IS NATIVE TOO SINCE A.3, and this is where the epic's Mac case is won or lost. A run's
	// shell command executes on the machine holding the attempt's lease, so a runner in Docker is a
	// Linux box with no Xcode in it. It starts LAST for the reason the container one did — cmd/runner
	// log.Fatalf's on a failed enroll with nothing behind it to retry — and the control plane above is
	// already serving by the time this line runs.
	runnerBin, err := nativeRunnerBinary(p)
	if err != nil {
		return "", err
	}
	runnerPID, err := startNativeRunner(cfg, p, runnerBin, nativeRunnerEnv(cfg, p, get, envValue(env, "PALAI_ENGINE_IMAGE"), root))
	if err != nil {
		return "", err
	}
	_ = upArgs // the runner is no longer a compose service on this posture; see nativeComposeServices
	fmt.Fprintf(os.Stderr, "stack up: api %s (native control plane, pid %d), native runner pid %d\n", cfg.BaseURL, pid, runnerPID)
	return fmt.Sprintf("NATIVE control plane, pid %d, log %s — NATIVE runner, pid %d, log %s — postgres/object-store in Docker (%s)",
		pid, p.nativeLog, runnerPID, p.nativeRunnerLog, nativeOverlayFile), nil
}

// nativeComposeServices are the compose services a native bring-up starts, and THE RUNNER IS NOT AMONG
// THEM since A.3 T4. It is a named list rather than an inline argument so a test can ask what the
// bring-up actually starts: the property is not "the runner is native" on its own but that the two never
// run TOGETHER — two machines in one pool means a command lands on whichever the gateway hands out, which
// is precisely the "where did this run" ambiguity the epic exists to remove.
//
// The overlay profiles the service out as well, and the two are not redundant: this list is what this
// command starts, the profile is what `docker compose up` started by hand would skip.
func nativeComposeServices() []string { return []string{"postgres", "object-store"} }

// nativeRunnerListen returns the address the native control plane binds its RUNNER gateway on, and
// refuses the two ways an operator override makes the runner container unable to reach it.
//
// Unset is the normal case and is not an error: this command knows the port the overlay dials
// (PALAI_RUNNER_PORT, from config.json) and binds every interface on it.
func nativeRunnerListen(get func(string) string, port int) (string, error) {
	want := ":" + strconv.Itoa(port)
	addr := strings.TrimSpace(get("PALAI_RUNNER_LISTEN_ADDR"))
	if addr == "" {
		return want, nil
	}
	host, p, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("PALAI_RUNNER_LISTEN_ADDR=%q is not host:port (%v) — fix: unset it, or set it to %q", addr, err, want)
	}
	// FACT 1. The runner is a CONTAINER. It dials `control-plane`, which the overlay aliases to
	// host-gateway, so the connection arrives on this machine from the Docker bridge — never on the
	// loopback interface. A loopback bind is a connection refused the runner reports as a dial error
	// long after this command said the stack was up.
	if ip := net.ParseIP(host); host == "localhost" || (ip != nil && ip.IsLoopback()) {
		return "", fmt.Errorf("PALAI_RUNNER_LISTEN_ADDR=%q binds the LOOPBACK interface, and the runner is a container: it dials "+
			"control-plane -> host-gateway, so nothing it sends can arrive on 127.0.0.1 — fix: unset it, or set it to %q "+
			"(deploy/compose/native-control-plane.yml, point 2)", addr, want)
	}
	// FACT 2. The overlay's URL is https://control-plane:${PALAI_RUNNER_PORT}. A listener on any other
	// port is the same refused connection with a different cause, so it is named separately.
	if p != strconv.Itoa(port) {
		return "", fmt.Errorf("PALAI_RUNNER_LISTEN_ADDR=%q binds port %s but the runner container dials PALAI_RUNNER_PORT=%d "+
			"(the overlay's https://control-plane:${PALAI_RUNNER_PORT}) — fix: unset it, or set it to %q", addr, p, port, want)
	}
	return addr, nil
}

// nativeWorkspaceRoot resolves the ONE absolute path a run's workspace has on both sides.
//
// FACT 3. The control plane mints an allocation under this root and hands the runner the path; the
// runner hands it to the Docker daemon as a bind SOURCE, which the daemon resolves on the host.
// Because the control plane is native its own path IS the host path — but the overlay still binds
// "${PALAI_WORKSPACE_ROOT}:${PALAI_WORKSPACE_ROOT}", so an unset value is a compose error about a
// malformed mount and a relative one is a path the daemon resolves against something else entirely.
//
// Symlinks are resolved for a reason this machine supplies: on macOS /tmp is a symlink to
// /private/tmp, so a root named through one is a directory the daemon knows by another name.
func nativeWorkspaceRoot(get func(string) string) (string, error) {
	root := strings.TrimSpace(get("PALAI_WORKSPACE_ROOT"))
	switch {
	case root == "":
		return "", fmt.Errorf("PALAI_WORKSPACE_ROOT is unset, and the native posture cannot run without it: the overlay binds " +
			"\"${PALAI_WORKSPACE_ROOT}:${PALAI_WORKSPACE_ROOT}\" into the runner, and the control plane mints every run's " +
			"workspace under it — fix: set PALAI_WORKSPACE_ROOT=<absolute host path> in .env.local")
	case !filepath.IsAbs(root):
		return "", fmt.Errorf("PALAI_WORKSPACE_ROOT=%q is relative: the Docker daemon resolves a bind source on the HOST, with no "+
			"idea what directory this command was run from — fix: make it an absolute path", root)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("create PALAI_WORKSPACE_ROOT %s: %w", root, err)
	}
	// EvalSymlinks needs the directory to exist, which is why it runs after MkdirAll.
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve PALAI_WORKSPACE_ROOT %s: %w", root, err)
	}
	if shared, err := worldWritableAncestor(real); err != nil {
		return "", fmt.Errorf("inspect PALAI_WORKSPACE_ROOT %s: %w", real, err)
	} else if shared != "" {
		return "", fmt.Errorf("PALAI_WORKSPACE_ROOT=%s sits under %s, which is world-writable and sticky (drwxrwxrwt): in the native "+
			"posture every run's workspace, HOME, TMPDIR and CoreSimulator device set live under this root, and there ANY local "+
			"account — not merely this uid — can create and replace paths beside them, which is weaker than the only boundary this "+
			"posture has (docs/research/macos-isolation-without-accounts.md §6) — fix: put it somewhere only this user can write, "+
			"e.g. $HOME/palai/workspaces", real, shared)
	}
	return real, nil
}

// worldWritableAncestor returns the first directory at or above dir that is world-writable AND
// sticky — the `/private/tmp` and `/Users/Shared` shape — or "" if there is none. It walks the
// RESOLVED path, because on macOS /tmp is a symlink to /private/tmp and a check that reads the name
// it was handed is one an everyday alias walks straight through (known-gaps `SUP-2`).
//
// Sticky is required, not incidental: it is what distinguishes a shared drop-box from an ordinary
// directory an operator deliberately made group- or world-writable for their own reasons.
func worldWritableAncestor(dir string) (string, error) {
	for {
		info, err := os.Stat(dir)
		if err != nil {
			return "", err
		}
		if mode := info.Mode(); mode&os.ModeSticky != 0 && mode.Perm()&0o002 != 0 {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", nil
		}
		dir = parent
	}
}

// nativeEnv is the environment the native control plane runs with: everything the compose service
// got from its `environment:` block, plus everything control-plane-entrypoint.sh bridged from
// /run/secrets — with every container-side address replaced by the one this process can reach.
//
// It is built by MERGING over the inherited environment rather than appending to it. os.Getenv in
// the child returns the FIRST occurrence of a key (os/env.go copyenv keeps the first index), so
// appending an override to os.Environ() produces a variable that is silently ignored — which is the
// exact failure mode this whole file exists to remove.
func nativeEnv(cfg Config, p paths, get func(string) string, engine, listen, root string) ([]string, error) {
	password, err := readTrimmed(p.pgPassword)
	if err != nil {
		return nil, fmt.Errorf("read the Postgres password: %w", err)
	}
	env := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if _, dup := env[k]; !dup {
			env[k] = v
		}
	}
	for k, v := range map[string]string{
		// The two backing services, by their PUBLISHED loopback ports — the compose service names
		// postgres/object-store resolve only inside the compose network.
		"PALAI_DATABASE_URL": cfg.databaseURL(password),
		"PALAI_S3_ENDPOINT":  fmt.Sprintf("http://127.0.0.1:%d", cfg.S3Port),
		"PALAI_S3_BUCKET":    "palai-artifacts",
		// The public API stays on loopback: nothing in a container calls it, and this process has no
		// container boundary in front of it.
		"PALAI_LISTEN_ADDR":        fmt.Sprintf("127.0.0.1:%d", cfg.APIPort),
		"PALAI_RUNNER_LISTEN_ADDR": listen,
		// The stack CA and the runner identity, at their real paths rather than the /palai mounts.
		"PALAI_RUNNER_CA_CERT":         p.caCert,
		"PALAI_RUNNER_CA_KEY":          p.caKey,
		"PALAI_RUNNER_SERVER_CERT":     p.serverCert,
		"PALAI_RUNNER_SERVER_KEY":      p.serverKey,
		"PALAI_ENROLLMENT_TOKEN_FILE":  p.runnerToken,
		"PALAI_BOOTSTRAP_API_KEY_FILE": p.apiKey,
		// The file secrets, at their host paths. The entrypoint's /run/secrets/* destinations name
		// files that do not exist off a container.
		"PALAI_SECRET_MASTER_KEY_FILE":      p.masterKey,
		"PALAI_GITHUB_APP_PRIVATE_KEY_FILE": p.secretPath(gitHubAppKeySlot),
		"PALAI_ENGINE_IMAGE":                engine,
		"PALAI_WORKSPACE_ROOT":              root,
	} {
		env[k] = v
	}
	// THE PROVIDER CREDENTIAL IS NOT BRIDGED HERE ANY MORE, removed 2026-08-04 together with the
	// control plane's env fallback and the compose entrypoint's copy of the same bridge.
	//
	// It used to be read from the 0600 file into the child's environment — never into argv, never back
	// onto disk, which was careful about the wrong thing. On this posture the agent's shell runs as the
	// same uid as the control plane, and an environment value is readable by anything that uid can run:
	// `ps -E -p <pid>` served 62 variables with their values, and `os.Unsetenv` cannot take one back
	// because macOS answers ps from the kernel's copy of the initial environment.
	//
	// The credential now takes the path the console already builds: sealed through POST /v1/secret-refs
	// into the encrypted store, named by a model connection, redeemed at call time. A bring-up that
	// finds no connection says so (see missingModelConnectionNotice) instead of quietly working through
	// a weaker route nobody chose.
	// The shell posture rides through from .env.local unchanged — including a wrong value, which the
	// control plane refuses at boot by design (docs/operations/palai-on-a-mac.md §1).
	if posture := strings.TrimSpace(get("PALAI_SHELL_NATIVE")); posture != "" {
		env["PALAI_SHELL_NATIVE"] = posture
	}
	// AND SO DOES THE TOOL-ERROR BUDGET, and it is here because a LIVE RUN proved it otherwise does not
	// arrive. This function MERGES over os.Environ(), and .env.local is read through `get` rather than
	// exported into this process — so a variable that lives only in that file and is not named here never
	// reaches the child. MEASURED 2026-08-01, twice, on the same stack: with PALAI_TOOL_ERROR_BUDGET=2 in
	// .env.local a native run was handed FOUR refusals and completed; with the same value EXPORTED into
	// the process environment the third refusal ended the run at `run.failed.v1`. A ceiling an operator
	// can write in the file the documentation points them at, and that the process never sees, is a
	// ceiling that does not exist.
	if budget := strings.TrimSpace(get("PALAI_TOOL_ERROR_BUDGET")); budget != "" {
		env["PALAI_TOOL_ERROR_BUDGET"] = budget
	}
	// AND SO DOES THE SCRIPTED EXCHANGE, for the same reason and with the same consequence. It names a
	// JSON file the deterministic adapter replays, which is how a stack with no provider credential drives
	// a run that calls a tool — the native posture's whole point, since the machine the tool runs on is
	// this one. Written in .env.local and not named here, it would reach nothing and the run would answer
	// the built-in "ok" without calling anything, which is exactly the belief the seam exists to prevent.
	if script := strings.TrimSpace(get("PALAI_FAKE_SCRIPT_FILE")); script != "" {
		env["PALAI_FAKE_SCRIPT_FILE"] = script
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out, nil
}

// nativeBinary resolves the control-plane binary to run. In a checkout it is BUILT — that is the
// whole cost of the native posture and it is a `go build`, not a Docker build. Outside one there is
// no source, so PALAI_CONTROL_PLANE_BIN must name it.
func nativeBinary(p paths) (string, error) {
	if bin := strings.TrimSpace(os.Getenv("PALAI_CONTROL_PLANE_BIN")); bin != "" {
		if _, err := os.Stat(bin); err != nil {
			return "", fmt.Errorf("PALAI_CONTROL_PLANE_BIN=%s: %w", bin, err)
		}
		return bin, nil
	}
	root, fromSource := buildContext(p.compose)
	if !fromSource {
		return "", fmt.Errorf("the native posture runs the control-plane BINARY on this machine and there is no source tree here to " +
			"build it from — fix: set PALAI_CONTROL_PLANE_BIN to a darwin build of it, or run from a checkout")
	}
	out := filepath.Join(p.home, "bin", "palai-control-plane")
	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		return "", err
	}
	fmt.Fprintf(os.Stderr, "        building the control plane for this machine (%s)\n", out)
	cmd := exec.Command("go", "build", "-o", out, "./apps/control-plane/cmd/palai-control-plane")
	cmd.Dir = root
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("build the control plane: %w", err)
	}
	return out, nil
}

// startNative launches the control plane in its OWN process group and waits for its API. The group
// is what makes the teardown honest: a control plane that spawns `xcodebuild` leaves a compiler
// running unless the whole group is signalled.
func startNative(cfg Config, p paths, bin string, composeEnv []string, listen, root string, get func(string) string) (int, error) {
	if stopped, err := stopNative(p); err != nil {
		return 0, err
	} else if stopped {
		fmt.Fprintln(os.Stderr, "        stopped the control plane a previous bring-up left running")
	}
	env, err := nativeEnv(cfg, p, get, envValue(composeEnv, "PALAI_ENGINE_IMAGE"), listen, root)
	if err != nil {
		return 0, err
	}
	log, err := os.OpenFile(p.nativeLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", p.nativeLog, err)
	}
	defer log.Close()
	cmd := exec.Command(bin)
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = log, log
	// Setpgid detaches it from this CLI's process group, so ^C on `palai up` does not take the stack
	// down with it, and so the teardown can signal the group rather than one pid.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start %s: %w", bin, err)
	}
	pid := cmd.Process.Pid
	if err := writeNativeRecord(p, pid, bin); err != nil {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		return 0, err
	}
	_ = cmd.Process.Release()
	if err := waitForAPI(cfg, p); err != nil {
		return 0, fmt.Errorf("%w\n        the control plane's own last words (%s):\n%s", err, p.nativeLog, nativeLogTail(p, 15))
	}
	return pid, nil
}

// stopNative stops the control plane a native bring-up started, and reports whether there was one.
// It is called by every teardown path unconditionally: a stack that was never native has no record
// and this is a no-op.
// restartNative is the native deployment's drift repair: stop this machine's control-plane process and
// bring it back with the desired document applied.
//
// IT IS THE COUNTERPART OF recreateControlPlane AND NOT AN ALIAS FOR IT. That one recreates the COMPOSE
// SERVICE, which on a native bring-up is in a profile and deliberately not running — asking compose to
// start it takes the port the native process holds, and the bring-up that had already succeeded is
// reported as a failure. The two collide because they are the same port and different processes, so the
// repair has to know which one this deployment actually runs.
//
// UpNative is idempotent (it stops a previous process before starting one), so the repair is simply
// running it again: the desired values are read from the environment this call re-exports.
func restartNative(_ Config, _ paths, get func(string) string) error {
	_, err := UpNative(get)
	return err
}

func stopNative(p paths) (bool, error) {
	raw, err := os.ReadFile(p.nativePID)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", p.nativePID, err)
	}
	line, bin, _ := strings.Cut(strings.TrimSpace(string(raw)), "\n")
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		return false, fmt.Errorf("%s does not hold a pid (%q): remove it by hand once you know what is running", p.nativePID, line)
	}
	// PID REUSE. A record older than a reboot names a pid the kernel has since handed to something
	// else, and killing it would be this command doing real damage. So the pid must still be running
	// the binary the record named; anything else is a refusal, not a best effort.
	switch running, comm := processCommand(pid); {
	case running && filepath.Base(comm) != filepath.Base(strings.TrimSpace(bin)):
		return false, fmt.Errorf("%s records pid %d as the control plane (%s) but that pid is running %q — refusing to kill it. "+
			"Fix: the process is gone and its pid was reused; delete %s", p.nativePID, pid, bin, comm, p.nativePID)
	case !running:
		// Already gone (a crash, a reboot). Clearing the record is the whole job.
		return false, os.Remove(p.nativePID)
	}
	// The GROUP, not the pid: an `xcodebuild` this control plane spawned is a child of it, and a
	// reaped parent that leaves a compiler running is the leak the host executor's own kill closes.
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		return false, fmt.Errorf("stop the native control plane (pid %d): %w", pid, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if running, _ := processCommand(pid); !running {
			break
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	return true, os.Remove(p.nativePID)
}

// writeNativeRecord records what was started: the pid on the first line, the binary on the second.
// The binary is what makes the pid-reuse guard above possible at all.
func writeNativeRecord(p paths, pid int, bin string) error {
	return os.WriteFile(p.nativePID, []byte(fmt.Sprintf("%d\n%s\n", pid, bin)), 0o600)
}

// processCommand reports whether a pid is running and what it is running. `ps` is the portable
// answer on macOS and Linux alike; /proc does not exist on a Mac, which is the machine this posture
// is for.
//
// The STATE is read alongside the command for one reason: a signalled process whose parent has not
// reaped it is a zombie, and `ps -o comm=` still prints its name. Without this a teardown run from
// the process that started the control plane would wait out the full SIGTERM grace and then SIGKILL
// something that had already exited.
func processCommand(pid int) (bool, string) {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "state=,comm=").Output()
	if err != nil {
		return false, ""
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 || strings.HasPrefix(fields[0], "Z") {
		return false, ""
	}
	return true, strings.Join(fields[1:], " ")
}

// nativeLogTail returns the last n lines of the native control plane's log — the only place its
// boot refusals (a bad shell posture, an unparseable master key, a database it cannot reach) are
// written, since it is not a compose service and `compose logs` cannot see it.
func nativeLogTail(p paths, n int) string {
	raw, err := os.ReadFile(p.nativeLog)
	if err != nil {
		return "        (no log at " + p.nativeLog + ")"
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return "        " + strings.Join(lines, "\n        ")
}

// envValue reads one key out of a compose environment slice.
func envValue(env []string, key string) string {
	for _, kv := range env {
		if k, v, _ := strings.Cut(kv, "="); k == key {
			return v
		}
	}
	return ""
}

// nativeShellWarning is the line an operator would otherwise meet as a tool that simply is not there.
//
// TWO OF ITS THREE ARMS ARE UNCHANGED SINCE A.3, AND THE THIRD HAS NOW BEEN RETIRED — counted rather
// than summarised, because "the warning was fixed" would hide which fact moved.
//
//   - posture UNSET: correct before A.3 and correct now. No process has an executor, every
//     `xcodebuild`/`simctl`/`axe` call is refused cleanly, and the operator is told why.
//   - posture MISSPELLED: correct before A.3 and correct now, and it covers MORE ground than it used
//     to — both binaries refuse it at boot since the derivation became shared (adapters/sandboxes/
//     posture), where once only the control plane did.
//   - posture CORRECT: this arm existed for one task's width. T3 moved execution onto the machine
//     holding the attempt's lease while `palai up --native` still left that machine a Linux container,
//     so a correctly configured stack really did have an unreachable toolchain and the warning said so.
//     T4 made the runner native too, so the condition it reported is gone and the arm is retired — a
//     warning that outlived its defect is worse than none, because the next real one reads as noise.
//
// What is NOT claimed here is that the posture reaching this machine means Xcode will build. That
// depends on the bootstrap namespace the bring-up inherited (native_runner.go's header measures what
// does and does not take it away), and this function cannot see it.
func nativeShellWarning(get func(string) string) string {
	switch posture := strings.TrimSpace(get("PALAI_SHELL_NATIVE")); {
	case posture == "":
		return "no shell posture is declared, so palai.workspace.shell has no runner on either process and every " +
			"`xcodebuild`/`simctl`/`axe` call fails cleanly. Set PALAI_SHELL_NATIVE=" + nativeShellPosture +
			" in .env.local — and read what it deletes first (docs/operations/palai-on-a-mac.md §1: the boundary " +
			"becomes the uid, and nothing else)"
	case posture != nativeShellPosture:
		// Both binaries refuse this at boot, so the stack never came up; the warning exists for the case
		// where it somehow did.
		return fmt.Sprintf("PALAI_SHELL_NATIVE=%q is not the string a Palai process accepts (%q)", posture, nativeShellPosture)
	}
	return ""
}
