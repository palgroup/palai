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
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/host"
	"github.com/palgroup/palai/packages/contracts"
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

// TestNativeShellPostureSeparatesConcurrentSessionsOnOneMac is E22 T2's component-real proof, and it
// runs the PRODUCTION path: the production broker, the production `palai.workspace.shell` tool, the
// production host executor. Two runs at the same time, two allocations, and each one's HOME, TMPDIR
// and CoreSimulator device set are its own.
//
// WHAT THIS IS NOT: a security boundary. Both runs are the same uid; either can open the other's
// directory by absolute path and nothing stops it. The failure being prevented is a CONFUSED AGENT —
// two runs booting, wiping or renaming each other's simulators, or clobbering one DerivedData — not
// a hostile one. The ceiling on the device-set half is carried by the NAME of
// TestSimctlSetIsAdvisoryNotEnforced (adapters/sandboxes/host).
func TestNativeShellPostureSeparatesConcurrentSessionsOnOneMac(t *testing.T) {
	roots := []string{t.TempDir(), t.TempDir()}
	homes := make([]string, len(roots))

	// Concurrently, because a sequential pair would pass even if the runner handed out one shared
	// directory and reused it. Each goroutine gets its own broker: the broker caches a completed call
	// by id, and one shared instance would be a race rather than a proof.
	var wg sync.WaitGroup
	errs := make([]error, len(roots))
	for i, root := range roots {
		wg.Add(1)
		go func() {
			defer wg.Done()
			broker := toolbroker.New(ShellTool())
			env := toolbroker.ExecEnv{WorkspaceRoot: root, Shell: host.NewExecutor(2 * time.Minute)}
			out, err := broker.Execute(t.Context(), contracts.ToolCallID(fmt.Sprintf("call_session_%d", i)),
				"palai.workspace.shell",
				// One argv element, because the executor joins argv with spaces before handing it to
				// `sh -c` — a multi-element form would have its quoting eaten on the way.
				map[string]any{"argv": []any{`echo "$HOME"; echo "$TMPDIR"; echo "$PALAI_SIMCTL_SET"`}, "shell": true}, 1, env)
			if err != nil {
				errs[i] = err
				return
			}
			lines := strings.Split(strings.TrimSpace(out.Result["stdout"].(string)), "\n")
			if len(lines) != 3 {
				errs[i] = fmt.Errorf("expected three session directories, got %q", out.Result["stdout"])
				return
			}
			for _, dir := range lines {
				if !strings.HasPrefix(dir, root) {
					errs[i] = fmt.Errorf("%q is not inside this run's allocation %q", dir, root)
					return
				}
			}
			homes[i] = lines[0]
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	if homes[0] == homes[1] {
		t.Fatalf("two concurrent runs share one HOME (%q) — they share one CoreSimulator device set, one DerivedData and one cache", homes[0])
	}

	// The claim that matters is what a run SEES. Run 0 writes a marker in its own HOME; run 1 lists
	// its own HOME and must not find it — and must find its own, or the listing proves nothing.
	broker := toolbroker.New(ShellTool())
	shell := func(id, root, argv string) string {
		t.Helper()
		out, err := broker.Execute(t.Context(), contracts.ToolCallID(id), "palai.workspace.shell",
			map[string]any{"argv": []any{argv}, "shell": true}, 1,
			toolbroker.ExecEnv{WorkspaceRoot: root, Shell: host.NewExecutor(time.Minute)})
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		stdout, _ := out.Result["stdout"].(string)
		return stdout
	}
	shell("call_marker_0", roots[0], `touch "$HOME/run-0-was-here"`)
	shell("call_marker_1", roots[1], `touch "$HOME/run-1-was-here"`)
	listing := shell("call_list_1", roots[1], `ls -A "$HOME"`)
	if strings.Contains(listing, "run-0-was-here") {
		t.Fatalf("run 1's HOME shows run 0's file — the two sessions are not separated:\n%s", listing)
	}
	if !strings.Contains(listing, "run-1-was-here") {
		t.Fatalf("run 1's HOME does not show run 1's own file, so the check above proves nothing:\n%s", listing)
	}
}

// TestAWallTimeExpiryTellsTheModelWhatHappened is the other half of giving the shell tool a wall
// time: a cap the model cannot distinguish from any other failure teaches it nothing, so it would
// retry the same hanging command. The executors classify the expiry (TimedOut + Signal "KILL", proven
// for the host in adapters/sandboxes/host and for the container in the oci workspace package) — what
// nothing asserted until now is that the classification survives the TOOL, which is the only layer
// the model ever sees.
//
// The hazard is specific and one line up in shell.go: `if err != nil { return nil, err }` DISCARDS
// the result, so a timeout that came back as an error would reach the model as a bare error with no
// stdout, no duration and no timed_out flag. Both postures return (result, nil) on an expiry, which
// is what makes the flag reachable, and this pins it.
//
// The wall time is explicit and short here because the subject is the RENDERING of an expiry, not the
// default that produces one (that is main_test.go's TestTheNativePostureBoundsAShellCallWith...).
func TestAWallTimeExpiryTellsTheModelWhatHappened(t *testing.T) {
	broker := toolbroker.New(ShellTool())
	out, err := broker.Execute(t.Context(), "call_hangs", "palai.workspace.shell",
		map[string]any{"argv": []any{"sleep", "60"}}, 1, toolbroker.ExecEnv{
			WorkspaceRoot: t.TempDir(),
			Shell:         host.NewExecutor(500 * time.Millisecond),
		})
	if err != nil {
		t.Fatalf("a wall-time expiry reached the model as an ERROR, losing the result: %v", err)
	}
	if timedOut, _ := out.Result["timed_out"].(bool); !timedOut {
		t.Fatalf("the model cannot tell this apart from any other failure — timed_out is not set: %#v", out.Result)
	}
	if signal, _ := out.Result["signal"].(string); signal != "KILL" {
		t.Fatalf("signal = %v, want KILL: %#v", out.Result["signal"], out.Result)
	}
	// A killed command must not read as one that ran and exited cleanly.
	if code, _ := out.Result["exit_code"].(int); code == 0 {
		t.Fatalf("a reaped command reported exit_code 0: %#v", out.Result)
	}
	// The host posture bounds no memory, so it must not claim an OOM it never observed.
	if oom, _ := out.Result["oom_killed"].(bool); oom {
		t.Fatalf("the host posture claimed an OOM kill it cannot observe: %#v", out.Result)
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
