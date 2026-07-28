package host_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/host"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// run is the common shape of every case here: one command, one throwaway workspace, a short wall
// time so a hung case fails fast rather than hanging the tier.
func run(t *testing.T, wall time.Duration, cmd toolbroker.ShellCommand) (toolbroker.ShellResult, error) {
	t.Helper()
	if cmd.WorkspaceRoot == "" {
		cmd.WorkspaceRoot = t.TempDir()
	}
	return host.NewExecutor(wall).Run(context.Background(), cmd)
}

// TestHostShellDropsTheOperatorsEnvironment is the sharpest claim in the native posture, and it is
// the one the container gave for free: in a container the shell inherited NOTHING, so a secret in
// the control plane's own environment was structurally out of reach. On the host the command is a
// child of the control-plane process, so without an explicit allow-list it would inherit
// SLACK_BOT_TOKEN, PALAI_GITHUB_APP_*, and the master key. The runner reduces the environment to a
// closed list and drops the rest; `env` sees only that list.
func TestHostShellDropsTheOperatorsEnvironment(t *testing.T) {
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-operator-secret-value")
	t.Setenv("PALAI_GITHUB_APP_PRIVATE_KEY_FILE", "/operator/secrets/github-app.pem")
	t.Setenv("PALAI_MASTER_KEY", "operator-master-key-value")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "operator-object-store-secret")

	res, err := run(t, 10*time.Second, toolbroker.ShellCommand{Argv: []string{"/usr/bin/env"}})
	if err != nil {
		t.Fatalf("run env: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("env exited %d: %s", res.ExitCode, res.Stderr)
	}
	for _, secret := range []string{
		"xoxb-operator-secret-value", "SLACK_BOT_TOKEN",
		"operator-master-key-value", "PALAI_MASTER_KEY",
		"github-app.pem", "PALAI_GITHUB_APP_PRIVATE_KEY_FILE",
		"operator-object-store-secret", "AWS_SECRET_ACCESS_KEY",
	} {
		if strings.Contains(res.Stdout, secret) {
			t.Fatalf("the agent's shell saw %q in its environment — the host runner inherited the operator's environment instead of an allow-list:\n%s", secret, res.Stdout)
		}
	}
	// The operator's HOME is part of the operator's environment, and it is the part that used to
	// arrive anyway: HOME was INHERITED, so the agent's shell got the home directory holding the
	// operator's ~/.ssh, ~/.aws and ~/.gitconfig. E22 T2 derives it from the allocation instead.
	if home := envValue(res.Stdout, "HOME"); home == os.Getenv("HOME") {
		t.Fatalf("the agent's shell inherited the operator's HOME (%q) instead of a per-allocation one", home)
	}

	// Stronger than "no known secret leaked": the environment IS the allow-list, so a variable
	// nobody thought to name here cannot arrive either.
	allowed := map[string]bool{
		"PATH": true, "LANG": true, "DEVELOPER_DIR": true, // inherited from the control plane
		"HOME": true, "TMPDIR": true, "PALAI_SIMCTL_SET": true, // derived from the allocation
	}
	for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		if line == "" {
			continue
		}
		name, _, ok := strings.Cut(line, "=")
		if !ok {
			continue // a continuation line of a multi-line value
		}
		if !allowed[name] {
			t.Fatalf("environment carries %q, which is not on the allow-list; full environment:\n%s", name, res.Stdout)
		}
	}
}

// envValue pulls one variable out of an `env` dump. A name the dump does not carry yields "", which
// every caller below treats as a failure rather than as a default.
func envValue(dump, name string) string {
	for _, line := range strings.Split(dump, "\n") {
		if n, v, ok := strings.Cut(line, "="); ok && n == name {
			return v
		}
	}
	return ""
}

// TestHostShellGivesConcurrentAllocationsDisjointSessionDirectories is E22 T2's mechanism. Every run
// already gets its own allocation directory and the shell already runs there as its cwd — but HOME,
// TMPDIR and the CoreSimulator device set that follows HOME were INHERITED from the control-plane
// process, so every concurrent run shared one of each. Two runs could wipe each other's DerivedData,
// each other's caches, and (through HOME) each other's simulators.
//
// THIS IS NOT A BOUNDARY. Both runs are the same uid and each can open the other's directory by
// path at any time. It is accident prevention: the adversary is a confused agent, not an attacker.
func TestHostShellGivesConcurrentAllocationsDisjointSessionDirectories(t *testing.T) {
	// Two runs AT THE SAME TIME, because that is the failure being prevented: a sequential pair would
	// pass even if the runner handed out one shared directory and reused it.
	type dump struct {
		env map[string]string
		err error
	}
	roots := []string{t.TempDir(), t.TempDir()}
	dumps := make([]dump, len(roots))
	var wg sync.WaitGroup
	for i, root := range roots {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := host.NewExecutor(10*time.Second).Run(t.Context(), toolbroker.ShellCommand{
				Argv: []string{"/usr/bin/env"}, WorkspaceRoot: root,
			})
			if err != nil {
				dumps[i] = dump{err: err}
				return
			}
			env := map[string]string{}
			for _, name := range []string{"HOME", "TMPDIR", "PALAI_SIMCTL_SET"} {
				env[name] = envValue(res.Stdout, name)
			}
			dumps[i] = dump{env: env}
		}()
	}
	wg.Wait()

	for i, d := range dumps {
		if d.err != nil {
			t.Fatalf("run %d: %v", i, d.err)
		}
		real, err := filepath.EvalSymlinks(roots[i])
		if err != nil {
			t.Fatalf("resolve %s: %v", roots[i], err)
		}
		for name, value := range d.env {
			if value == "" {
				t.Fatalf("run %d received no %s — the agent's toolchain would fall back to the control plane's own", i, name)
			}
			// BOTH sides resolved before comparing. macOS hands out /var/folders/… while the same
			// directory resolves to /private/var/folders/…, and a containment check that resolves one
			// side only is the failure shape this repo has shipped four times (known-gaps SUP-2).
			// EvalSymlinks doubles as the existence check: a path pointing nowhere does not resolve.
			resolved, statErr := filepath.EvalSymlinks(value)
			if statErr != nil {
				t.Fatalf("run %d has %s=%q, which does not resolve — a variable pointing nowhere is worse than an inherited one: %v", i, name, value, statErr)
			}
			if !strings.HasPrefix(resolved, real+string(filepath.Separator)) {
				t.Fatalf("run %d has %s=%q (%q), which is not inside its own allocation %q", i, name, value, resolved, real)
			}
		}
	}
	for name := range dumps[0].env {
		if dumps[0].env[name] == dumps[1].env[name] {
			t.Fatalf("two concurrent runs share %s=%q", name, dumps[0].env[name])
		}
	}

	// The disjointness that matters is what a run SEES, not what its variables say. A marker written
	// in one run's HOME must be absent from the other's.
	if _, err := run(t, 10*time.Second, toolbroker.ShellCommand{
		Argv: []string{"touch", filepath.Join(dumps[0].env["HOME"], "run-0-was-here")}, WorkspaceRoot: roots[0],
	}); err != nil {
		t.Fatalf("write a marker in run 0's HOME: %v", err)
	}
	res, err := run(t, 10*time.Second, toolbroker.ShellCommand{
		Argv: []string{"ls", "-A", "$HOME"}, WorkspaceRoot: roots[1], Shell: true,
	})
	if err != nil {
		t.Fatalf("list run 1's HOME: %v", err)
	}
	if strings.Contains(res.Stdout, "run-0-was-here") {
		t.Fatalf("run 1's HOME shows run 0's file — the two runs share a home directory:\n%s", res.Stdout)
	}
	// …and the check above is only worth anything if it CAN see a file, so show it seeing its own.
	if _, err := run(t, 10*time.Second, toolbroker.ShellCommand{
		Argv: []string{"touch", filepath.Join(dumps[1].env["HOME"], "run-1-was-here")}, WorkspaceRoot: roots[1],
	}); err != nil {
		t.Fatalf("write a marker in run 1's HOME: %v", err)
	}
	res, err = run(t, 10*time.Second, toolbroker.ShellCommand{
		Argv: []string{"ls", "-A", "$HOME"}, WorkspaceRoot: roots[1], Shell: true,
	})
	if err != nil {
		t.Fatalf("re-list run 1's HOME: %v", err)
	}
	if !strings.Contains(res.Stdout, "run-1-was-here") {
		t.Fatalf("run 1's HOME does not even show run 1's own file — the listing proves nothing:\n%s", res.Stdout)
	}
}

// TestSimctlSetIsAdvisoryNotEnforced carries E22 T2's ceiling IN ITS NAME, because a ceiling in a
// comment is a ceiling nobody re-reads.
//
// MEASURED ON THIS MACHINE 2026-07-28 (E22 X20, macOS 26.3 / Xcode 26.6), and the answer was the
// inconvenient one: `HOME` does NOT select the CoreSimulator device set. `simctl` is a thin client;
// the set belongs to `com.apple.CoreSimulator.CoreSimulatorService`, a launchd-managed per-user XPC
// service that is already running under the login session's own HOME. A device created from a shell
// whose HOME pointed at a scratch directory landed in the DEFAULT set, was listed from both HOMEs,
// and left nothing at all under the scratch directory.
//
// So the device set is partitioned by `simctl --set <dir>`, which is an ARGV FLAG — and argv belongs
// to the model. `PALAI_SIMCTL_SET` names a per-allocation directory for it and
// docs/operations/palai-on-a-mac.md tells the agent to pass it, but nothing here enforces it: the
// executor hands the machine the argv it was given, unrewritten. That is what "advisory" means, and
// research T22 is the other half — any same-uid process can point `--set` at another session's
// directory and drive its devices.
func TestSimctlSetIsAdvisoryNotEnforced(t *testing.T) {
	root := t.TempDir()

	// (a) The advice is deliverable: the variable the doc tells the agent to expand is really there.
	res, err := run(t, 10*time.Second, toolbroker.ShellCommand{
		Argv: []string{"printf", "%s", "$PALAI_SIMCTL_SET"}, WorkspaceRoot: root, Shell: true,
	})
	if err != nil {
		t.Fatalf("expand PALAI_SIMCTL_SET: %v", err)
	}
	if res.Stdout == "" {
		t.Fatal("PALAI_SIMCTL_SET expands to nothing — the instruction in palai-on-a-mac.md would produce `simctl --set ''`")
	}

	// (b) …and it is NOT enforced. The runner rewrites no argv, so an argv naming a different device
	// set reaches the machine verbatim, and an argv naming none at all gets the default set. Both are
	// the ceiling; neither is a defect.
	elsewhere := filepath.Join(t.TempDir(), "someone-elses-device-set")
	res, err = run(t, 10*time.Second, toolbroker.ShellCommand{
		Argv: []string{"echo", "simctl", "--set", elsewhere, "list"}, WorkspaceRoot: root,
	})
	if err != nil {
		t.Fatalf("echo an argv: %v", err)
	}
	if got := strings.TrimSpace(res.Stdout); got != "simctl --set "+elsewhere+" list" {
		t.Fatalf("the runner altered the argv it was handed: %q", got)
	}

	// (c) The measurement the whole fallback rests on, re-run rather than recalled: pointing HOME at
	// the allocation does NOT give the run its own device set. If this ever starts failing, HOME alone
	// partitions simulators, `--set` stops being necessary, and this test's NAME is the thing to fix.
	if _, lookErr := exec.LookPath("xcrun"); runtime.GOOS != "darwin" || lookErr != nil {
		t.Log("HOME-vs-device-set half skipped: needs a Mac with xcrun (the argv half above ran)")
		return
	}
	operator, err := exec.Command("xcrun", "simctl", "list", "devices").Output()
	if err != nil {
		t.Skipf("simctl list under the operator's own HOME failed: %v", err)
	}
	udid := firstUDID(string(operator))
	if udid == "" {
		t.Skip("this Mac has no simulator devices, so a set comparison would be vacuous")
	}
	res, err = run(t, 60*time.Second, toolbroker.ShellCommand{
		Argv: []string{"xcrun", "simctl", "list", "devices"}, WorkspaceRoot: root,
	})
	if err != nil {
		t.Fatalf("simctl list under the per-allocation HOME: %v", err)
	}
	if !strings.Contains(res.Stdout, udid) {
		t.Fatalf("a per-allocation HOME hid device %s from simctl — HOME now partitions the device set, "+
			"so the advisory `--set` fallback this test is named for is no longer the mechanism", udid)
	}
}

// firstUDID returns the first device UDID in `simctl list devices` output, or "" if there are none.
func firstUDID(listing string) string {
	re := "0123456789ABCDEF"
	for _, line := range strings.Split(listing, "\n") {
		open := strings.Index(line, "(")
		if open < 0 || len(line) < open+37 || line[open+37] != ')' {
			continue
		}
		candidate := line[open+1 : open+37]
		if len(candidate) == 36 && candidate[8] == '-' && candidate[13] == '-' &&
			strings.ContainsRune(re, rune(candidate[0])) {
			return candidate
		}
	}
	return ""
}

// TestHostShellRefusesAReadOnlyAttempt pins the honest half of losing the container. ReadOnly was a
// mount flag; a host has no read-only bind of the same directory, so the runner REFUSES the attempt
// rather than running it writable and calling it read-only. A silently-writable "read-only" run is
// the failure this test exists to make impossible.
func TestHostShellRefusesAReadOnlyAttempt(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "it-ran")
	res, err := run(t, 10*time.Second, toolbroker.ShellCommand{
		Argv:          []string{"/usr/bin/touch", marker},
		WorkspaceRoot: dir,
		ReadOnly:      true,
	})
	if err == nil {
		t.Fatalf("a ReadOnly attempt ran on the host: %+v", res)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("the refused command still ran — refusal must happen BEFORE exec, not after")
	}
	if !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("refusal does not say what it refused: %v", err)
	}
}

// TestHostShellRunsInTheWorkspaceRootAndRedactsSecrets covers the two properties the OCI executor
// gets from its mount and its redactor: the working directory IS the allocation root, and
// secret-shaped output is masked before it reaches a model or a log.
func TestHostShellRunsInTheWorkspaceRootAndRedactsSecrets(t *testing.T) {
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	res, err := run(t, 10*time.Second, toolbroker.ShellCommand{
		Argv:          []string{"pwd", "&&", "echo", "ghp_0123456789abcdefghijklmnopqrstuvwx"},
		WorkspaceRoot: dir,
		Shell:         true,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := strings.SplitN(strings.TrimSpace(res.Stdout), "\n", 2)[0]; got != real {
		t.Fatalf("working directory = %q, want the workspace root %q", got, real)
	}
	if strings.Contains(res.Stdout, "ghp_0123456789") {
		t.Fatalf("a GitHub-shaped token survived redaction: %s", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "***") {
		t.Fatalf("nothing was redacted: %s", res.Stdout)
	}
}

// TestHostShellBoundsOutput pins the same caps the OCI executor applies (1 MiB stdout / 64 KiB
// stderr) and the truncation flag that says so. Unbounded output is how a shell tool takes down the
// control plane it now runs inside.
func TestHostShellBoundsOutput(t *testing.T) {
	res, err := run(t, 30*time.Second, toolbroker.ShellCommand{
		Argv:  []string{"head", "-c", "3000000", "/dev/zero"},
		Shell: true,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Stdout) > 1<<20 {
		t.Fatalf("stdout = %d bytes, want at most %d", len(res.Stdout), 1<<20)
	}
	if !res.Truncated {
		t.Fatal("output was cut but Truncated is false — the caller cannot tell it is reading a prefix")
	}
}

// TestHostShellWallTimeKillsTheWholeProcessGroup is the property the container gave by destroying
// itself: killing the command must kill what the command spawned. `xcodebuild` is a process tree,
// so a wall-time expiry that reaps only the direct child leaves a compiler running on the host
// forever. The grandchild here outlives its parent on purpose.
func TestHostShellWallTimeKillsTheWholeProcessGroup(t *testing.T) {
	dir := t.TempDir()
	res, err := run(t, 500*time.Millisecond, toolbroker.ShellCommand{
		// The grandchild writes its pid, is disowned by the intermediate shell, and sleeps well past
		// the wall time; only a process-GROUP kill reaches it.
		Argv:          []string{"sleep", "60", "&", "echo", "$!", ">", filepath.Join(dir, "pid"), ";", "sleep", "60"},
		WorkspaceRoot: dir,
		Shell:         true,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.TimedOut {
		t.Fatalf("wall time expired but TimedOut is false: %+v", res)
	}
	if res.Signal != "KILL" {
		t.Fatalf("signal = %q, want KILL", res.Signal)
	}
	raw, readErr := os.ReadFile(filepath.Join(dir, "pid"))
	if readErr != nil {
		t.Fatalf("grandchild never recorded its pid (%v) — the case proves nothing", readErr)
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw)))
	if convErr != nil {
		t.Fatalf("pid file %q: %v", raw, convErr)
	}
	// Signal 0 probes for existence without delivering anything.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return // reaped with its group
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL) // do not leak it out of the test either
	t.Fatalf("grandchild pid %d survived the wall-time kill — only the direct child was reaped", pid)
}

// TestHostShellRequiresAnArgvAndAWorkspaceRoot mirrors the OCI executor's two preconditions, so a
// posture swap cannot turn a clean refusal into a command running somewhere unintended — a
// workspace-less run must never execute in the control plane's own working directory.
func TestHostShellRequiresAnArgvAndAWorkspaceRoot(t *testing.T) {
	if _, err := host.NewExecutor(time.Second).Run(context.Background(), toolbroker.ShellCommand{WorkspaceRoot: t.TempDir()}); err == nil {
		t.Fatal("an empty argv ran")
	}
	if _, err := host.NewExecutor(time.Second).Run(context.Background(), toolbroker.ShellCommand{Argv: []string{"true"}}); err == nil {
		t.Fatal("a command with no workspace root ran")
	}
}

// TestHostShellReportsAMissingCommandAsExit127 keeps the tool's contract posture-independent: in the
// container a missing binary is a normal shell outcome (exit 127, "command not found"), not an
// infrastructure error. The host runner reports it the same way, so a model reads one story.
func TestHostShellReportsAMissingCommandAsExit127(t *testing.T) {
	res, err := run(t, 10*time.Second, toolbroker.ShellCommand{Argv: []string{"palai-no-such-command-exists"}})
	if err != nil {
		t.Fatalf("a missing command surfaced as an executor error: %v", err)
	}
	if res.ExitCode != 127 {
		t.Fatalf("exit code = %d, want 127", res.ExitCode)
	}
	if !strings.Contains(res.Stderr, "not found") {
		t.Fatalf("stderr does not say what happened: %q", res.Stderr)
	}
}
