//go:build live

package posture

// THE MAC LEG OF THE A.3 EXIT CRITERION, and it is tagged `live` because it asserts a fact about the
// MACHINE it runs on rather than about the tree. On any other operating system it would be a false red,
// so it skips; in CI it does not run at all.
//
// WHAT IT PROVES AND WHAT IT DOES NOT. It proves the executor the NATIVE RUNNER builds from
// PALAI_SHELL_NATIVE runs a command on this machine and that the machine is a Mac — the half of the exit
// criterion that only a Mac can answer. It does NOT drive a model, an engine or a tool dispatch: those
// legs are proven separately and without a credential (packages/runner's toolserver tests for the
// machine's own execution, apps/control-plane/internal/execution/remote_shell_test.go for the control
// plane reaching it over the real gateway wire, attempt_shell_test.go for the attempt choosing its own
// machine). The end-to-end RUN is not producible on a stack with no provider: the shipped
// credential-free adapter answers a fixed script with no tool calls
// (adapters/models/registry/registry.go FakeScript), so no run it drives can reach a shell tool at all.

import (
	"context"
	"runtime"
	"strings"
	"testing"

	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// TestTheNativeMachineAnswersDarwin is the second line of the uname proof. Before A.3 a command ran in
// the control plane, so `uname` reported whichever machine that process sat on; now it runs on the
// machine holding the attempt's lease, and on this posture that machine is this Mac.
func TestTheNativeMachineAnswersDarwin(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skipf("the Mac leg of the uname proof needs a Mac; this is %s", runtime.GOOS)
	}
	t.Setenv("PALAI_SANDBOX_IMAGE", "")
	t.Setenv("PALAI_SHELL_NATIVE", Native)

	shell, err := RunnerFromEnv()
	if err != nil {
		t.Fatalf("the native posture was refused: %v", err)
	}
	if shell == nil {
		t.Fatal("PALAI_SHELL_NATIVE was declared and no executor was built, so this machine would run no command")
	}

	result, err := shell.Run(context.Background(), toolbroker.ShellCommand{
		Argv: []string{"uname", "-s"}, WorkspaceRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("uname -s on this machine: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("uname -s exited %d (stderr %q)", result.ExitCode, result.Stderr)
	}
	if got := strings.TrimSpace(result.Stdout); got != "Darwin" {
		t.Fatalf("uname -s = %q, want Darwin — the executor this posture builds is not running on this Mac", got)
	}
}

// TestTheNativeMachineCanResolveItsUser is the precondition Xcode needs and the one this tree has
// already been wrong about twice.
//
// `whoami` printing the numeric uid rather than a name means getpwuid() is failing, and the SAME fault
// makes confstr(_CS_DARWIN_USER_CACHE_DIR) fail — which is exactly what Xcode's DVT layer aborts on
// (exit 134, Abort trap: 6). A posture being SET is not the mechanism WORKING, so this drives the
// executor and reads what it actually got.
//
// It is separate from the uname test on purpose: `uname` succeeds under a broken bootstrap namespace,
// so a single test asserting only Darwin would go green on a machine where no build can run.
func TestTheNativeMachineCanResolveItsUser(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skipf("this precondition is about macOS; this is %s", runtime.GOOS)
	}
	t.Setenv("PALAI_SANDBOX_IMAGE", "")
	t.Setenv("PALAI_SHELL_NATIVE", Native)

	shell, err := RunnerFromEnv()
	if err != nil || shell == nil {
		t.Fatalf("the native posture built no executor: %v", err)
	}
	result, err := shell.Run(context.Background(), toolbroker.ShellCommand{
		Argv: []string{"whoami"}, WorkspaceRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("whoami on this machine: %v", err)
	}
	name := strings.TrimSpace(result.Stdout)
	if name == "" || result.ExitCode != 0 {
		t.Fatalf("whoami exited %d with %q", result.ExitCode, name)
	}
	if isAllDigits(name) {
		t.Fatalf("whoami returned the numeric uid %q rather than a user name: getpwuid() is failing on this "+
			"machine, and the same fault aborts every Xcode invocation on confstr(DARWIN_USER_CACHE_DIR). "+
			"The bring-up inherited no bootstrap namespace — run `palai up --native` from a live user session", name)
	}
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}
