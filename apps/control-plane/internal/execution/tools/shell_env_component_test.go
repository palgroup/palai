//go:build component

// E25 T3's headline proof, and it is written to demonstrate an ABSENCE before it demonstrates a
// presence: an agent's shell command asks the machine for one variable by name.
//
// THE ABSENCE, MEASURED (plan §3.6 D2). The environment an agent's shell sees comes from two CLOSED
// lists — adapters/sandboxes/host/exec.go's envAllowList and adapters/sandboxes/oci/workspace/exec.go's
// shellEnv() — and neither has any way to be extended by a caller. So a value in the CONTROL PLANE's
// own environment does not reach the command: `printenv MY_KEY` prints nothing and exits 1. That half
// stays asserted here forever, because it is the property the allow-list exists for.
//
// THE PRESENCE. ExecEnv.EnvValues is the pipe T3 builds: a per-attempt map layered ON TOP of the closed
// list at exec time. The same `printenv MY_KEY` then prints the value — and the host-inheritance half
// above is STILL false, which is the whole point: the pipe is a caller-supplied layer, not a hole in
// the allow-list.
//
// It is a component test rather than a unit test for the reason its sibling shell_host_component_test.go
// is: the thing under test is a real process on this machine seeing (or not seeing) a real environment.
// It needs no Docker and no Postgres.
package tools

import (
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/host"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

func TestAnEnvironmentValueReachesTheAgentsShellAndTheHostsDoesNot(t *testing.T) {
	const sentinel = "e25-t3-sentinel-value-not-a-real-credential"

	// The control plane's own environment carries the name the agent will ask for. Under the closed
	// list this is unreachable; it must STAY unreachable after the pipe exists.
	t.Setenv("MY_KEY", sentinel)

	broker := toolbroker.New(ShellTool())
	root := t.TempDir()

	// 1. THE ABSENCE. No EnvValues: the command must see nothing under this name.
	out, err := broker.Execute(t.Context(), "call_printenv_absent", "palai.workspace.shell",
		map[string]any{"argv": []any{"printenv", "MY_KEY"}}, 1,
		toolbroker.ExecEnv{WorkspaceRoot: root, Shell: host.NewExecutor(30 * time.Second)})
	if err != nil {
		t.Fatalf("palai.workspace.shell: %v", err)
	}
	stdout, _ := out.Result["stdout"].(string)
	if strings.TrimSpace(stdout) != "" {
		t.Fatalf("the agent's shell read MY_KEY out of the CONTROL PLANE's environment (%q) — the closed allow-list has a hole", stdout)
	}
	// printenv reports an unset name the way a shell does: exit 1, no output. Asserted so a printenv
	// that silently failed for another reason (missing binary → 127) cannot pass for an absence.
	if code, _ := out.Result["exit_code"].(int); code != 1 {
		t.Fatalf("printenv MY_KEY exited %v, want 1 (name unset); stderr=%q", out.Result["exit_code"], out.Result["stderr"])
	}

	// 2. THE PRESENCE. The attempt's own environment values, layered on top at exec time.
	out, err = broker.Execute(t.Context(), "call_printenv_present", "palai.workspace.shell",
		map[string]any{"argv": []any{"printenv", "MY_KEY"}}, 1,
		toolbroker.ExecEnv{
			WorkspaceRoot: root,
			Shell:         host.NewExecutor(30 * time.Second),
			EnvValues:     map[string]string{"MY_KEY": sentinel},
		})
	if err != nil {
		t.Fatalf("palai.workspace.shell (with EnvValues): %v", err)
	}
	if code, _ := out.Result["exit_code"].(int); code != 0 {
		t.Fatalf("printenv MY_KEY exited %v with the value wired: %v", out.Result["exit_code"], out.Result["stderr"])
	}
	stdout, _ = out.Result["stdout"].(string)
	// REDACTION IS WHY THIS IS NOT AN EQUALITY. The executor masks the attempt's own environment values
	// in captured output (RedactValues), so a command that prints one gets `***` back — which is exactly
	// what the journal must see. What proves the value ARRIVED is the exit code (printenv exits 1 on an
	// unset name) plus the mask standing where the value was.
	if strings.TrimSpace(stdout) != "***" {
		t.Fatalf("printenv printed %q; want the redaction mask — either the value never arrived or it came back unmasked", strings.TrimSpace(stdout))
	}

	// 3. THE PIPE IS A LAYER, NOT A HOLE. A second call with no EnvValues must be absent again — the
	// executor holds no state from the call above, and the host variable is still unreachable.
	out, err = broker.Execute(t.Context(), "call_printenv_absent_again", "palai.workspace.shell",
		map[string]any{"argv": []any{"printenv", "MY_KEY"}}, 1,
		toolbroker.ExecEnv{WorkspaceRoot: root, Shell: host.NewExecutor(30 * time.Second)})
	if err != nil {
		t.Fatalf("palai.workspace.shell (absent again): %v", err)
	}
	if stdout, _ := out.Result["stdout"].(string); strings.TrimSpace(stdout) != "" {
		t.Fatalf("a later call with no environment still saw %q — the value outlived the attempt that carried it", stdout)
	}
}

// TestAnEnvironmentKeyCannotShadowTheSandboxsOwnPATH is the exec-time half of the two-place key
// validation, driven through the PRODUCTION tool rather than the executor directly: a key named PATH
// must be refused, and the sandbox's own PATH must come back bit-unchanged.
//
// It is the exec-time check that is load-bearing, not the route's. A route can change; this cannot be
// reached around. A PATH the caller controls is a shell that runs the caller's `git`.
func TestAnEnvironmentKeyCannotShadowTheSandboxsOwnPATH(t *testing.T) {
	broker := toolbroker.New(ShellTool())
	root := t.TempDir()
	exec := host.NewExecutor(30 * time.Second)

	// The sandbox's own PATH, recorded before anything tries to shadow it.
	out, err := broker.Execute(t.Context(), "call_path_before", "palai.workspace.shell",
		map[string]any{"argv": []any{"printenv", "PATH"}}, 1,
		toolbroker.ExecEnv{WorkspaceRoot: root, Shell: exec})
	if err != nil {
		t.Fatalf("palai.workspace.shell: %v", err)
	}
	before, _ := out.Result["stdout"].(string)
	if strings.TrimSpace(before) == "" {
		t.Fatal("the sandbox's own PATH is empty, so the comparison below would prove nothing")
	}

	// The refusal is an executor ERROR, not a non-zero exit: nothing ran. A shell that ran under a
	// caller-supplied PATH and merely reported an error afterwards would already have executed the
	// caller's binaries.
	_, err = broker.Execute(t.Context(), "call_path_shadow", "palai.workspace.shell",
		map[string]any{"argv": []any{"printenv", "PATH"}}, 1,
		toolbroker.ExecEnv{WorkspaceRoot: root, Shell: exec, EnvValues: map[string]string{"PATH": "/tmp/attacker/bin"}})
	if err == nil {
		t.Fatal("an environment key named PATH was accepted at exec time — it shadows the sandbox's own PATH")
	}
	if !strings.Contains(err.Error(), "PATH") {
		t.Fatalf("the refusal does not name the offending key: %v", err)
	}

	// And the sandbox's PATH is what it was. A refusal that left the environment mutated would be a
	// refusal in name only.
	out, err = broker.Execute(t.Context(), "call_path_after", "palai.workspace.shell",
		map[string]any{"argv": []any{"printenv", "PATH"}}, 1,
		toolbroker.ExecEnv{WorkspaceRoot: root, Shell: exec})
	if err != nil {
		t.Fatalf("palai.workspace.shell: %v", err)
	}
	after, _ := out.Result["stdout"].(string)
	if after != before {
		t.Fatalf("the sandbox's PATH moved after a refused shadow attempt:\nbefore=%q\nafter =%q", before, after)
	}
}
