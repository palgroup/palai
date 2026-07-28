//go:build live

// E22 T1's live leg: the agent drives a REAL iOS simulator on THIS machine, and every verb is an
// argv through the production `palai.workspace.shell` tool. Palai types no simulator capability, no
// `ios.drive` operation and no gesture union — `xcrun` and `axe` are binaries on a PATH, and that is
// the entire mechanism.
//
// It runs under `make test-live-mac` and skips by the NAME of the variable it is missing:
// PALAI_SIMULATOR_UDID for the simulator legs, PALAI_IOS_PROJECT for the build leg.
//
// HONEST CEILINGS, all measured 2026-07-28 on macOS 26.3 / Xcode 26.6 / AXe 1.7.0:
//   - THERE IS NO SANDBOX. These commands run as the control plane's own uid.
//   - `axe` is a third-party tool over Apple's PRIVATE accessibility+HID APIs. An OS update can break
//     it, and when it breaks the shell call fails honestly rather than silently doing nothing.
//   - A recording is stopped by a SIGNAL, and the shell tool returns ONE result with no way to signal
//     a running command — so the stop is part of the model's own argv (`kill -INT`), which is exactly
//     what docs/operations/palai-on-a-mac.md tells the agent to write.
package live

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/host"
	"github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	"github.com/palgroup/palai/packages/contracts"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// shellCall makes each call id unique: the broker caches a completed call by id, so a reused id would
// replay the previous result instead of running the next command.
var shellCall int

// shellOnHost runs one argv through the production tool over the production host executor and
// returns (stdout, stderr, exitCode). Nothing here is a fixture: the broker, the tool and the runner
// are the ones a Mac deployment binds.
func shellOnHost(t *testing.T, root string, shellMode bool, argv ...string) (string, string, int) {
	t.Helper()
	callID := contracts.ToolCallID("call_" + strings.ReplaceAll(t.Name(), "/", "_") + "_" + strconv.Itoa(shellCall))
	shellCall++
	broker := toolbroker.New(tools.ShellTool())
	args := map[string]any{"argv": toAnySlice(argv), "shell": shellMode}
	out, err := broker.Execute(t.Context(), callID, "palai.workspace.shell", args, 1,
		toolbroker.ExecEnv{WorkspaceRoot: root, Shell: host.NewExecutor(10 * time.Minute)})
	if err != nil {
		t.Fatalf("palai.workspace.shell %v: %v", argv, err)
	}
	stdout, _ := out.Result["stdout"].(string)
	stderr, _ := out.Result["stderr"].(string)
	code, _ := out.Result["exit_code"].(int)
	return stdout, stderr, code
}

func toAnySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// TestLiveMacHostDrivesASimulatorThroughShellCalls is the epic's crown claim, live: boot, wait,
// read the accessibility tree, tap, screenshot, record — six verbs, six argv, zero typed operations.
func TestLiveMacHostDrivesASimulatorThroughShellCalls(t *testing.T) {
	udid := os.Getenv("PALAI_SIMULATOR_UDID")
	if udid == "" {
		t.Skip("PALAI_SIMULATOR_UDID is not set")
	}
	if _, err := exec.LookPath("axe"); err != nil {
		t.Skipf("axe is not on this host's PATH (brew install cameroncooke/axe/axe): %v", err)
	}
	root := t.TempDir()

	// 1. Boot deterministically. `bootstatus -b` boots if needed and WAITS — a fixed sleep is the
	// flake factory this replaces (E22 X3).
	if _, stderr, code := shellOnHost(t, root, false, "xcrun", "simctl", "bootstatus", udid, "-b"); code != 0 {
		t.Fatalf("bootstatus exited %d: %s", code, stderr)
	}

	// 2. MEASURED CORRECTION TO X5: bootstatus returning is NOT the same as the device being
	// drivable. The accessibility translation service comes up SECONDS LATER, and until it does both
	// `describe-ui` and `tap` fail with "No translation object returned for simulator". What makes
	// the device drivable is TIME, not a Simulator.app window — there is no window in this test.
	var tree string
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		stdout, _, code := shellOnHost(t, root, false, "axe", "describe-ui", "--udid", udid)
		if code == 0 && strings.HasPrefix(strings.TrimSpace(stdout), "[") {
			tree = stdout
			break
		}
		time.Sleep(2 * time.Second)
	}
	if tree == "" {
		t.Fatal("the accessibility tree never arrived within 90s of a completed bootstatus")
	}
	if !strings.Contains(tree, "AXFrame") {
		t.Fatalf("the tree is not an accessibility tree: %.200s", tree)
	}

	// 3. Drive it. The tap is an argv; the union of gestures the plan's v1 wanted to type lives in
	// `axe`'s own subcommands (E22 X4).
	if stdout, stderr, code := shellOnHost(t, root, false, "axe", "tap", "-x", "100", "-y", "100", "--udid", udid); code != 0 {
		t.Fatalf("axe tap exited %d: %s %s", code, stdout, stderr)
	}

	// 4. A screenshot is an artefact on the workspace, which is what T5 uploads to a thread.
	shot := filepath.Join(root, "screen.png")
	if _, stderr, code := shellOnHost(t, root, false, "xcrun", "simctl", "io", udid, "screenshot", shot); code != 0 {
		t.Fatalf("simctl io screenshot exited %d: %s", code, stderr)
	}
	if info, err := os.Stat(shot); err != nil || info.Size() == 0 {
		t.Fatalf("screenshot is missing or empty: %v", err)
	}

	// 5. Recording, stopped the ONLY way it can be stopped: SIGINT (E22 X2). The stop is inside the
	// model's own argv because the shell tool has no signal channel — this is the exact form
	// palai-on-a-mac.md tells the agent to write, so the doc is proven rather than asserted.
	movie := filepath.Join(root, "run.mp4")
	stdout, stderr, code := shellOnHost(t, root, true,
		"xcrun", "simctl", "io", udid, "recordVideo", "--codec=h264", "--force", movie,
		"&", "REC=$!;", "sleep", "4;", "kill", "-INT", "$REC;", "wait", "$REC")
	if info, err := os.Stat(movie); err != nil || info.Size() == 0 {
		t.Fatalf("no recording was written (exit %d): %v\nstdout=%s\nstderr=%s", code, err, stdout, stderr)
	}

	// 6. THE `.mp4` IS A LIE, and this is why T5 sniffs the container instead of trusting the name
	// the model chose (E22 X2). `--codec=h264` still writes a QuickTime container.
	kind, err := exec.Command("/usr/bin/file", "--brief", movie).Output()
	if err != nil {
		t.Fatalf("file(1): %v", err)
	}
	if !strings.Contains(string(kind), "QuickTime") {
		t.Fatalf("the recording is no longer a QuickTime container (%s) — X2's trap and T5's sniffing may be stale", strings.TrimSpace(string(kind)))
	}
	t.Logf("recorded %q as %s — the extension the model chose was wrong, as measured", filepath.Base(movie), strings.TrimSpace(string(kind)))
}

// TestLiveMacHostBuildsAnXcodeProjectThroughShellCalls is the build half: the expensive compile runs
// ONCE (`build-for-testing`) and the tests run off the produced .xctestrun (`test-without-building`),
// which is a quality property a model has to know rather than a correctness one the code enforces
// (E22 X8). Simulator builds need NO signing identity (X7).
func TestLiveMacHostBuildsAnXcodeProjectThroughShellCalls(t *testing.T) {
	project := os.Getenv("PALAI_IOS_PROJECT")
	if project == "" {
		t.Skip("PALAI_IOS_PROJECT is not set")
	}
	scheme := os.Getenv("PALAI_IOS_SCHEME")
	if scheme == "" {
		t.Skip("PALAI_IOS_SCHEME is not set")
	}
	udid := os.Getenv("PALAI_SIMULATOR_UDID")
	if udid == "" {
		t.Skip("PALAI_SIMULATOR_UDID is not set")
	}
	root := t.TempDir()
	derived := filepath.Join(root, "DerivedData")

	stdout, stderr, code := shellOnHost(t, root, false,
		"xcodebuild", "build-for-testing",
		"-project", project, "-scheme", scheme,
		"-destination", "platform=iOS Simulator,id="+udid,
		"-derivedDataPath", derived,
		"CODE_SIGNING_ALLOWED=NO")
	if code != 0 {
		t.Fatalf("build-for-testing exited %d\nstdout tail: %s\nstderr tail: %s", code, tail(stdout), tail(stderr))
	}
	matches, _ := filepath.Glob(filepath.Join(derived, "Build", "Products", "*.xctestrun"))
	if len(matches) == 0 {
		t.Fatal("build-for-testing produced no .xctestrun — test-without-building has nothing to run")
	}
	if _, stderr, code := shellOnHost(t, root, false,
		"xcodebuild", "test-without-building",
		"-xctestrun", matches[0],
		"-destination", "platform=iOS Simulator,id="+udid); code != 0 {
		t.Fatalf("test-without-building exited %d: %s", code, tail(stderr))
	}
}

func tail(s string) string {
	if len(s) > 2000 {
		return s[len(s)-2000:]
	}
	return s
}
