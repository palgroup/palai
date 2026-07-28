package host_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	// Stronger than "no known secret leaked": the environment IS the allow-list, so a variable
	// nobody thought to name here cannot arrive either.
	allowed := map[string]bool{"PATH": true, "HOME": true, "TMPDIR": true, "LANG": true, "DEVELOPER_DIR": true}
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
