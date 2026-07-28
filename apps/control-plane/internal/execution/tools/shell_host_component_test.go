//go:build component

// E22 T1's component proof: the agent's capability is the HOST's capability. It runs the production
// `palai.workspace.shell` tool, through the production broker, over the production host executor,
// and asks the machine for `xcodebuild -version`. Palai types no `xcode-simulator` capability, no
// `ios.build` operation and no dispatch tool — the answer comes back because the machine has Xcode
// and the shell tool ran an argv.
//
// It is a component test rather than a unit test because the thing under test is the REAL toolchain
// on the REAL machine: no fake runner, no fixture output. It needs no Docker and no Postgres, which
// is the point — a Mac deployment runs the control plane natively (X18).
package tools

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/host"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// TestNativeShellPostureRunsTheHostsOwnToolchain is the whole epic in one call: argv
// ["xcodebuild","-version"] through palai.workspace.shell answers with the Xcode on this machine.
// Under the container posture the same call runs in a Linux image and answers `command not found`.
func TestNativeShellPostureRunsTheHostsOwnToolchain(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skipf("the host toolchain proof needs the host to BE a Mac (GOOS=%s)", runtime.GOOS)
	}
	if _, err := exec.LookPath("xcodebuild"); err != nil {
		t.Skipf("xcodebuild is not on this host's PATH: %v", err)
	}

	broker := toolbroker.New(ShellTool())
	env := toolbroker.ExecEnv{
		WorkspaceRoot: t.TempDir(),
		Shell:         host.NewExecutor(2 * time.Minute),
	}

	out, err := broker.Execute(t.Context(), "call_xcodebuild_version", "palai.workspace.shell",
		map[string]any{"argv": []any{"xcodebuild", "-version"}}, 1, env)
	if err != nil {
		t.Fatalf("palai.workspace.shell: %v", err)
	}
	if code, _ := out.Result["exit_code"].(int); code != 0 {
		t.Fatalf("xcodebuild -version exited %v: %v", out.Result["exit_code"], out.Result["stderr"])
	}
	stdout, _ := out.Result["stdout"].(string)
	if !strings.HasPrefix(stdout, "Xcode ") {
		t.Fatalf("palai.workspace.shell did not reach the host's Xcode; stdout=%q stderr=%q", stdout, out.Result["stderr"])
	}
	t.Logf("the host answered: %s", strings.TrimSpace(strings.SplitN(stdout, "\n", 2)[0]))

	// The second half of the same claim, and the reason no capability is typed: `simctl` is reached
	// the same way — one more argv, zero more Palai code.
	out, err = broker.Execute(t.Context(), "call_simctl_help", "palai.workspace.shell",
		map[string]any{"argv": []any{"xcrun", "simctl", "help"}}, 1, env)
	if err != nil {
		t.Fatalf("palai.workspace.shell (simctl): %v", err)
	}
	help, _ := out.Result["stdout"].(string)
	if !strings.Contains(help, "Subcommands:") || !strings.Contains(help, "\tboot ") {
		t.Fatalf("simctl help did not come back from the host: %q", help)
	}
	// MEASURED 2026-07-28 on Xcode 26.6 (17F113), and it is why palai-on-a-mac.md names `bootstatus`
	// explicitly: `simctl help` does NOT list it, so a model that enumerates subcommands from help
	// will never find the one command that makes a boot deterministic (E22 X1, corrected).
	if strings.Contains(help, "bootstatus") {
		t.Fatal("`simctl help` now lists bootstatus — palai-on-a-mac.md's note that it is unlisted is stale")
	}
	out, err = broker.Execute(t.Context(), "call_simctl_bootstatus_help", "palai.workspace.shell",
		map[string]any{"argv": []any{"xcrun", "simctl", "help", "bootstatus"}}, 1, env)
	if err != nil {
		t.Fatalf("palai.workspace.shell (bootstatus help): %v", err)
	}
	// simctl writes per-subcommand help to STDERR (measured 2026-07-28) — another host fact the doc
	// carries, because a model that reads only stdout concludes the command does not exist.
	stdout, _ = out.Result["stdout"].(string)
	stderr, _ := out.Result["stderr"].(string)
	if !strings.Contains(stdout+stderr, "Checks device boot status") {
		t.Fatalf("the unlisted bootstatus subcommand is not reachable either: stdout=%q stderr=%q", stdout, stderr)
	}
	if strings.Contains(stdout, "Checks device boot status") {
		t.Fatal("`simctl help <subcommand>` now writes to stdout — palai-on-a-mac.md's stderr note is stale")
	}
}

// TestShellToolStillFailsCleanlyWithNoPostureConfigured holds the unchanged half. shellRunnerFromEnv
// returns nil when no posture is declared, and a nil runner must stay a clean tool error — the same
// error a deployment with no sandbox image has always produced. A new posture branch is exactly the
// kind of change that turns "fails cleanly" into "runs somewhere unintended".
func TestShellToolStillFailsCleanlyWithNoPostureConfigured(t *testing.T) {
	broker := toolbroker.New(ShellTool())
	_, err := broker.Execute(t.Context(), "call_no_runner", "palai.workspace.shell",
		map[string]any{"argv": []any{"echo", "hi"}}, 1, toolbroker.ExecEnv{WorkspaceRoot: t.TempDir()})
	if err == nil {
		t.Fatal("a shell call ran with no runner wired")
	}
	if !strings.Contains(err.Error(), "no sandbox shell runner wired") {
		t.Fatalf("the clean failure changed shape: %v", err)
	}
}
