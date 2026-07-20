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
