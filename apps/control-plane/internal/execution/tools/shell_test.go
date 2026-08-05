package tools

import (
	"context"
	"testing"

	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// fakeShell is a ShellRunner double: it records the command it received and returns a canned result,
// so the tool's wrapper logic (argv coercion, egress findings, result mapping) is testable without a
// real sandbox.
type fakeShell struct {
	last   toolbroker.ShellCommand
	result toolbroker.ShellResult
}

func (f *fakeShell) Run(_ context.Context, cmd toolbroker.ShellCommand) (toolbroker.ShellResult, error) {
	f.last = cmd
	return f.result, nil
}

// TestShellToolFlagsMetadataEgress proves the SAN-004 finding half: a command whose argv names the
// cloud metadata address — bare or inside a URL — is flagged as an egress finding, while an ordinary
// command produces none.
func TestShellToolFlagsMetadataEgress(t *testing.T) {
	fs := &fakeShell{result: toolbroker.ShellResult{ExitCode: 0, Stdout: "dial_ok=false\n"}}
	env := toolbroker.ExecEnv{WorkspaceRoot: "/workspace", Shell: fs}

	for _, argv := range [][]any{
		{"curl", "http://169.254.169.254/latest/meta-data/"},
		{"nc", "169.254.169.254", "80"},
	} {
		out, err := ShellTool().Exec(context.Background(), env, map[string]any{"argv": argv})
		if err != nil {
			t.Fatalf("shell exec %v: %v", argv, err)
		}
		findings, ok := out["egress_findings"].([]any)
		if !ok || len(findings) == 0 {
			t.Fatalf("argv %v produced no egress finding: %#v", argv, out["egress_findings"])
		}
		if reason := findings[0].(map[string]any)["reason"]; reason != "metadata" {
			t.Fatalf("argv %v egress reason = %v, want metadata", argv, reason)
		}
	}

	// An ordinary command names no denied destination.
	out, err := ShellTool().Exec(context.Background(), env, map[string]any{"argv": []any{"go", "test", "./..."}})
	if err != nil {
		t.Fatalf("ordinary shell exec: %v", err)
	}
	if _, present := out["egress_findings"]; present {
		t.Fatalf("ordinary command produced an egress finding: %#v", out["egress_findings"])
	}
}

// TestShellToolRequiresArgvArrayAndRunner proves the tool rejects a bare-string argv (a shell line is
// never parsed from an unstructured string) and fails cleanly with no sandbox runner wired.
func TestShellToolRequiresArgvArrayAndRunner(t *testing.T) {
	fs := &fakeShell{}
	env := toolbroker.ExecEnv{WorkspaceRoot: "/workspace", Shell: fs}
	if _, err := ShellTool().Exec(context.Background(), env, map[string]any{"argv": "rm -rf /"}); err == nil {
		t.Fatal("a bare-string argv was accepted; it must be a JSON array of strings")
	}

	noRunner := toolbroker.ExecEnv{WorkspaceRoot: "/workspace"} // Shell nil
	if _, err := ShellTool().Exec(context.Background(), noRunner, map[string]any{"argv": []any{"ls"}}); err == nil {
		t.Fatal("shell tool ran with no sandbox runner wired; it must fail cleanly")
	}

	noWorkspace := toolbroker.ExecEnv{Shell: fs} // no workspace root
	if _, err := ShellTool().Exec(context.Background(), noWorkspace, map[string]any{"argv": []any{"ls"}}); err == nil {
		t.Fatal("shell tool ran with no workspace bound; it must fail cleanly")
	}
}

// TestTheSessionUidReachesTheCommandTheExecutorIsGiven is A.5 Task 3's control-plane-side end of the
// carry. execEnv resolves the uid off the session's account and this tool is what puts it on the struct
// the machine receives — so a uid that stopped travelling here would leave every command on a Mac
// running as the control plane, which is the state docs/measurements/faz-a5-residue.md §2 measured.
func TestTheSessionUidReachesTheCommandTheExecutorIsGiven(t *testing.T) {
	fs := &fakeShell{result: toolbroker.ShellResult{ExitCode: 0}}
	runAs := &toolbroker.RunAs{UID: 707, GID: 20}
	env := toolbroker.ExecEnv{WorkspaceRoot: "/workspace", Shell: fs, RunAs: runAs}

	if _, err := ShellTool().Exec(context.Background(), env, map[string]any{"argv": []any{"swift", "build"}}); err != nil {
		t.Fatalf("shell exec: %v", err)
	}
	if fs.last.RunAs == nil {
		t.Fatal("the executor was given a command with no RunAs: it would spawn the tenant's build as " +
			"whoever the machine's executor is")
	}
	if *fs.last.RunAs != *runAs {
		t.Fatalf("RunAs = %+v, want %+v", *fs.last.RunAs, *runAs)
	}

	// AND A DEPLOYMENT THAT MINTS NO ACCOUNTS IS UNCHANGED. This is every stack before A.5 and it is a
	// declared posture, not a hole: nothing is claimed, so nothing is refused.
	fs.last = toolbroker.ShellCommand{}
	plain := toolbroker.ExecEnv{WorkspaceRoot: "/workspace", Shell: fs}
	if _, err := ShellTool().Exec(context.Background(), plain, map[string]any{"argv": []any{"swift", "build"}}); err != nil {
		t.Fatalf("shell exec with no session accounts: %v", err)
	}
	if fs.last.RunAs != nil {
		t.Fatalf("a deployment with no session-account layer sent RunAs %+v", *fs.last.RunAs)
	}
}

// TestAMachineThatMintsUidsButCannotNameThisSessionsRunsNothing is the third state, and the one a single
// nil pointer could not have expressed. SlotAccounts keeps its session→slot map in process — a limit its
// own type comment declares — so a control plane that restarted mid-session holds no uid for a session
// whose tree is already owned by one.
//
// THE ANSWER MUST BE A REFUSAL AND NOT A RUN. Running it as the control plane's own uid is the failure
// this whole task exists to delete, and it would be invisible: the command succeeds, the files land, and
// the only evidence is their owner.
func TestAMachineThatMintsUidsButCannotNameThisSessionsRunsNothing(t *testing.T) {
	fs := &fakeShell{result: toolbroker.ShellResult{ExitCode: 0}}
	env := toolbroker.ExecEnv{WorkspaceRoot: "/workspace", Shell: fs, RunAsUnresolved: true}

	out, err := ShellTool().Exec(context.Background(), env, map[string]any{"argv": []any{"swift", "build"}})
	if err == nil {
		t.Fatalf("the command ran: %#v", out)
	}
	answer, ok := toolbroker.AsAnswer(err)
	if !ok || answer.Code != toolbroker.AnswerUnavailable {
		t.Fatalf("refusal = %v, want an AnswerUnavailable the model is told about rather than an error "+
			"that wedges the run", err)
	}
	if fs.last.Argv != nil {
		t.Fatalf("the executor was reached anyway with argv %v", fs.last.Argv)
	}
}
