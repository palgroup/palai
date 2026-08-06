package stack

// The RUNNER as a process on this machine, and it exists because A.3 moved execution onto the machine
// that holds the attempt's lease.
//
// Until then the shell tool ran in the control plane, so `--native` put the control plane on the Mac and
// the Mac's toolchain was one process away — that is what the 2026-07-28 `xcodebuild` transcript
// measured. Now a run's command travels to the runner, and a runner in Docker is a Linux box with no
// Xcode in it: the capability the native posture exists for would have been unreachable on the only
// shipped Mac deployment there is.
//
// So the runner joins the control plane on this side of the boundary. Everything here mirrors native.go
// deliberately — the same pid record, the same pid-reuse refusal, the same process group, the same log
// an operator is pointed at — because two lifecycles that behave differently are two things to remember.
//
// ORPHANING IS NOT WHAT BREAKS XCODE, MEASURED 2026-08-04. The tree recorded that a native control plane
// could not resolve a user (`whoami` printing 501, and `confstr(_CS_DARWIN_USER_CACHE_DIR)` failing,
// which is what Xcode's DVT layer aborts on) and attributed it to the process being orphaned onto
// launchd. A faithful mimic of the spawn below — Setpgid, Release, parent exited, child reparented to
// ppid 1 — resolves the user, reads DARWIN_USER_CACHE_DIR and runs `xcrun` normally. What a child
// inherits is the bootstrap namespace of whoever started it, and being reparented does not take it away.
// The operating rule that follows is about the CALLER, not this code: `palai up --native` must be run
// from a live user session, because a bring-up driven from a context that has no namespace hands that
// absence to every command the machine will ever run.

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// nativeRunnerBinary names a build of the agent to start beside the plane, or EMPTY when this machine
// has none to start.
//
// ‼️ IT NO LONGER COMPILES ONE, AND THE DELETION IS THE POINT (device plan §3.7). It used to run
// `go build -o … ./cmd/runner` from the checkout, which put a source tree and a Go toolchain on the path
// a machine joins a fleet by — and, worse, meant the packaged path was never the one exercised. Measured
// 2026-08-06: the Milestone A0 session was served by this conjured runner while the enrolled device sat
// parked beside it, so nobody noticed the device had no shell executor at all. A fallback that quietly
// supplies what the product is supposed to install is a fallback that hides whether the product works.
//
// EMPTY IS NOT AN ERROR. A control plane with no agent beside it is a correct deployment — capacity comes
// from devices that installed and enrolled themselves, which is the whole design. PALAI_RUNNER_BIN stays
// for the checkout and for CI, where an agent is built deliberately rather than conjured.
func nativeRunnerBinary() (string, error) {
	bin := strings.TrimSpace(os.Getenv("PALAI_RUNNER_BIN"))
	if bin == "" {
		return "", nil
	}
	if _, err := os.Stat(bin); err != nil {
		return "", fmt.Errorf("PALAI_RUNNER_BIN=%s: %w", bin, err)
	}
	return bin, nil
}

// nativeRunnerEnv is the environment the native runner runs with: what the compose service's
// `environment:` block gave the CONTAINER, bridged the way runner-entrypoint.sh bridges it, with every
// container-side address replaced by one this process can reach.
//
// THE BRIDGE IS THE POINT, not a detail under it. The compose/Helm/systemd contract's public variable
// names (PALAI_RUNNER_CA_CERT among them) are not what cmd/runner's binary reads — mustEnv("PALAI_CONTROLLER_CA")
// is (main.go:174) — and every other launcher translates one to the other in a shell wrapper before
// exec'ing the binary (deploy/compose/runner-entrypoint.sh, scripts/package/runner/palai-runner.sh). This
// process has no wrapper: it calls exec.Command on the binary directly, so THIS function is that bridge,
// and it has to hand the binary the names the binary actually reads, not the contract's names.
//
// THE CONTROLLER IS DIALLED ON LOOPBACK AND VERIFIED BY THE NAME ON ITS CERTIFICATE, and those are two
// different fields on purpose. certs.go mints ONE SAN — `control-plane` — and packages/runner verifies
// the leaf carries exactly one DNS name equal to ControllerDNS (session.go tlsConfig). The container
// satisfied that by resolving `control-plane` to the host gateway; a process on this machine has no such
// alias and adding one would mean editing /etc/hosts. It does not need to: PALAI_CONTROLLER_DNS is the
// TLS ServerName and the exact-SAN check, and it is INDEPENDENT of the URL's host, so the connection
// goes to 127.0.0.1 while the identity checked is still `control-plane`. No new certificate, no new SAN,
// no name resolution at all — which is the same "the fix is DNS, not certificates" the overlay's header
// states, applied to a machine that has no DNS to fix.
//
// It MERGES over the inherited environment for the reason nativeEnv does: os.Getenv in the child returns
// the FIRST occurrence of a key, so appending an override to os.Environ() produces a variable that is
// silently ignored.
func nativeRunnerEnv(cfg Config, p paths, get func(string) string, engine, root string) []string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if _, dup := env[k]; !dup {
			env[k] = v
		}
	}
	controllerURL := fmt.Sprintf("https://127.0.0.1:%d", cfg.RunnerPort)
	for k, v := range map[string]string{
		"PALAI_CONTROLLER_URL": controllerURL,
		"PALAI_CONTROLLER_DNS": nativeControllerDNS,
		// The stack CA at its REAL path, under the name the binary reads (mustEnv("PALAI_CONTROLLER_CA")) —
		// the container's /palai/* mounts name files that do not exist off a container, and PALAI_RUNNER_CA_CERT
		// is a name only the compose/systemd bridge scripts translate, which this process is instead of.
		"PALAI_CONTROLLER_CA":         p.caCert,
		"PALAI_ENROLLMENT_TOKEN_FILE": p.runnerToken,
		// ‼️ PALAI_SESSION_URL IS NO LONGER SET HERE, and the comment that used to justify it was true when
		// it was written and false by the time it was read: cmd/runner's derivation DID leave the session
		// on https, so all three bridges — this map, the compose entrypoint and the host launcher —
		// special-cased the swap, and the binary's own path was one no shipped deployment ever exercised.
		// It exercises it now (loadConfig -> outboundSessionURL), so the swap lives in one place and this
		// process relies on it exactly as a packaged agent does.
		"PALAI_ENGINE_IMAGE":    engine,
		"PALAI_COMPOSE_PROJECT": cfg.Project,
		// The allocation root, which on this posture is one path on one filesystem: the control plane
		// mints under it and the runner is on the same machine, so there is no bind to keep in step.
		"PALAI_WORKSPACE_ROOT": root,
	} {
		env[k] = v
	}
	// THE POSTURE, AND IT IS THE WHOLE REASON THIS PROCESS IS NATIVE. On the container runner this said
	// "the container's own filesystem"; here it says this Mac. A wrong value is not corrected — the
	// runner refuses it at boot (posture.Resolve), which is the refusal that keeps a machine from
	// silently running in the other posture.
	if posture := strings.TrimSpace(get("PALAI_SHELL_NATIVE")); posture != "" {
		env["PALAI_SHELL_NATIVE"] = posture
	}
	// Read through `get` rather than inherited, for the reason the tool-error budget taught this tree:
	// a value that lives only in .env.local never reaches a child unless it is named here.
	// PALAI_RUNNER_CAPACITY joins this list for the reason the list exists: cmd/runner reads it at enrolment
	// to declare its session ceiling, and a name missing from here never reaches the binary however
	// carefully .env.local sets it. Wiring the reader without this line would have closed one link of a
	// two-link chain and changed nothing observable.
	for _, name := range []string{"PALAI_RUNNER_CONCURRENCY", "PALAI_RUNNER_CAPACITY", "PALAI_RUNNER_POOL", "PALAI_RUNNER_POSTURE", "PALAI_SANDBOX_WALL_TIME", "PALAI_VERSION"} {
		if v := strings.TrimSpace(get(name)); v != "" {
			env[name] = v
		}
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// nativeControllerDNS is the single SAN certs.go mints and packages/runner pins. It is the TLS identity
// the native runner checks, never an address it resolves.
const nativeControllerDNS = "control-plane"

// startNativeRunner launches the runner in its OWN process group and waits until the control plane says
// a machine is connected.
//
// The group matters more here than it does for the control plane: since A.3 this process is the one that
// spawns `xcodebuild`, so a teardown that signalled one pid would leave a compiler running.
func startNativeRunner(cfg Config, p paths, bin string, env []string) (int, error) {
	if stopped, err := stopNativeRunner(p); err != nil {
		return 0, err
	} else if stopped {
		fmt.Fprintln(os.Stderr, "        stopped the runner a previous bring-up left running")
	}
	log, err := os.OpenFile(p.nativeRunnerLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", p.nativeRunnerLog, err)
	}
	defer log.Close()
	cmd := exec.Command(bin)
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start %s: %w", bin, err)
	}
	pid := cmd.Process.Pid
	if err := writeNativeRunnerRecord(p, pid, bin); err != nil {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		return 0, err
	}
	_ = cmd.Process.Release()
	if err := waitForRunnerSession(cfg, p); err != nil {
		return 0, fmt.Errorf("%w\n        the runner's own last words (%s):\n%s", err, p.nativeRunnerLog, nativeRunnerLogTail(p, 15))
	}
	return pid, nil
}

// waitForRunnerSession polls until the control plane reports a connected runner.
//
// IT ASKS THE CONTROL PLANE RATHER THAN THE PROCESS TABLE, because "the process is still alive" is not
// the claim that matters: cmd/runner log.Fatalf's on a failed enroll, so a pid that exists proves only
// that it has not failed YET, and a runner that enrolled but could not open a session would satisfy it
// forever. palai_runner_sessions is the gauge the runner-down alert already reads, and it counts
// sessions the gateway HOLDS.
func waitForRunnerSession(cfg Config, p paths) error {
	key, err := readTrimmed(p.apiKey)
	if err != nil {
		return fmt.Errorf("read api key: %w", err)
	}
	wait, err := readyTimeout()
	if err != nil {
		return err
	}
	deadline := time.Now().Add(wait)
	client := &http.Client{Timeout: 3 * time.Second}
	for {
		sessions, err := runnerSessions(client, cfg, key)
		if err == nil && sessions >= 1 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the native runner did not open a session with the control plane within %s "+
				"(palai_runner_sessions stayed at 0)", wait)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// runnerSessions reads the palai_runner_sessions gauge off the metrics surface.
func runnerSessions(client *http.Client, cfg Config, key string) (float64, error) {
	req, err := http.NewRequest(http.MethodGet, cfg.BaseURL+"/metrics", nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("GET /metrics: %s", resp.Status)
	}
	body := make([]byte, 1<<20)
	n, _ := resp.Body.Read(body)
	return scrapeGauge(string(body[:n]), "palai_runner_sessions")
}

// scrapeGauge pulls one scalar out of Prometheus text exposition. It skips HELP/TYPE lines, which carry
// the metric's name too and would otherwise be parsed as its value.
func scrapeGauge(exposition, name string) (float64, error) {
	for _, line := range strings.Split(exposition, "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		field, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || field != name {
			continue
		}
		return strconv.ParseFloat(strings.TrimSpace(value), 64)
	}
	return 0, fmt.Errorf("%s is not in the exposition", name)
}

// stopNativeRunner stops a runner a native bring-up started, and reports whether there was one. It is
// the same pid-reuse discipline stopNative uses: a record older than a reboot names a pid the kernel has
// handed to something else, and killing that would be this command doing real damage.
func stopNativeRunner(p paths) (bool, error) {
	raw, err := os.ReadFile(p.nativeRunnerPID)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", p.nativeRunnerPID, err)
	}
	line, bin, _ := strings.Cut(strings.TrimSpace(string(raw)), "\n")
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		return false, fmt.Errorf("%s does not hold a pid (%q): remove it by hand once you know what is running", p.nativeRunnerPID, line)
	}
	switch running, comm := processCommand(pid); {
	case running && filepath.Base(comm) != filepath.Base(strings.TrimSpace(bin)):
		return false, fmt.Errorf("%s records pid %d as the runner (%s) but that pid is running %q — refusing to kill it. "+
			"Fix: the process is gone and its pid was reused; delete %s", p.nativeRunnerPID, pid, bin, comm, p.nativeRunnerPID)
	case !running:
		return false, os.Remove(p.nativeRunnerPID)
	}
	// The GROUP: an `xcodebuild` this runner spawned is a child of it, and since A.3 this is the process
	// those children hang off.
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		return false, fmt.Errorf("stop the native runner (pid %d): %w", pid, err)
	}
	// ‼️ THE WAIT WATCHES THE GROUP, AND IT USED TO WATCH THE LEADER. `processCommand(pid)` answers about
	// ONE process, so a leader that exited on SIGTERM ended this loop while a child it had spawned was
	// still running — and the next line deletes the pid record, which is the ONLY handle anything has on
	// that group. The function returned `true`, the operator was told the runner had stopped, and an
	// `xcodebuild` kept a workspace and a Mac busy with nothing left that could name it.
	//
	// Measured 2026-08-06: TestNativeRunnerStopKillsTheProcessGroupAndClearsTheRecord passes in 0.4s in
	// isolation and failed inside `make verify` with the group ALIVE fifteen seconds after this returned.
	// That is not the machine being slow — the leader had gone in milliseconds either way; it is this
	// loop having finished for a reason unrelated to what it promises.
	//
	// AND THE ESCALATION NOW WAITS FOR ITS OWN KILL. `SIGKILL` then `break` reported success at the
	// instant the signal was queued, which is the same defect one layer down.
	deadline := time.Now().Add(nativeRunnerStopGrace)
	for !groupGone(pid) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			for i := 0; i < 50 && !groupGone(pid); i++ {
				time.Sleep(100 * time.Millisecond)
			}
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	return true, os.Remove(p.nativeRunnerPID)
}

// nativeRunnerStopGrace is how long a SIGTERM'd group is given to leave on its own before SIGKILL. Ten
// seconds is a build's chance to finish writing what it had open.
//
// IT IS A VAR SO A TEST CAN SHORTEN IT, AND IT IS NOT CONFIGURATION. No environment variable reads it and
// no catalogue entry names it: an operator has no decision to make here, and the only caller that needs a
// different number is the one proving that the escalation happens at all — which at ten seconds would be
// a ten-second unit test, and a suite that slow gets run less, which costs more than the knob saves.
var nativeRunnerStopGrace = 10 * time.Second

// groupGone answers whether process group `pid` has any live member left.
//
// ESRCH is "no member". EPERM is what Darwin answers for a group whose only remaining members are
// ZOMBIES — measured on this tree, and a zombie holds no workspace, no port and no file — so it counts as
// gone. ANY OTHER ERROR COUNTS AS STILL THERE, deliberately: the caller deletes the pid record on the
// strength of this answer, and a wrong "gone" leaks a process nothing can ever name again.
func groupGone(pid int) bool {
	switch err := syscall.Kill(-pid, 0); err {
	case syscall.ESRCH, syscall.EPERM:
		return true
	default:
		return false
	}
}

func writeNativeRunnerRecord(p paths, pid int, bin string) error {
	return os.WriteFile(p.nativeRunnerPID, []byte(fmt.Sprintf("%d\n%s\n", pid, bin)), 0o600)
}

// nativeRunnerLogTail returns the last n lines of the native runner's log — where its boot refusals (a
// bad shell posture, a spent enrollment token, a control plane it cannot reach) are written, since it is
// not a compose service and `compose logs` cannot see it.
func nativeRunnerLogTail(p paths, n int) string {
	raw, err := os.ReadFile(p.nativeRunnerLog)
	if err != nil {
		return "        (no log at " + p.nativeRunnerLog + ")"
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return "        " + strings.Join(lines, "\n        ")
}
