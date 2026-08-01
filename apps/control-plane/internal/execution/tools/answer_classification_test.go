package tools

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// This file pins WHICH of the built-in tools' errors are answers the model is handed and which are
// faults that still abort the attempt. It is the tool-side half of packages/tool-broker/answer.go; the
// orchestrator-side half (the row reaching `completed`, the run staying alive) is the component tier.
//
// The tests run the SHIPPED tool functions against a REAL directory. A table that asserted "these
// sentinels map to these codes" without executing the tool would pass just as well against a tool that
// never reaches the classifier at all.

// TestEveryFailedWorkspaceReadIsAnAnswer is the rule in one test: A READ THAT FAILED CHANGED NOTHING.
// Missing, refused, non-regular, out of bounds — all four are things the model can hear and act on, and
// every one of them used to end the run.
func TestEveryFailedWorkspaceReadIsAnAnswer(t *testing.T) {
	root := realTempDir(t)
	env := toolbroker.ExecEnv{WorkspaceRoot: root}
	if err := os.Symlink("/etc", filepath.Join(root, "escape")); err != nil {
		t.Fatalf("plant escaping symlink: %v", err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, "runtime.fifo"), 0o600); err != nil {
		t.Fatalf("plant fifo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("SECRET=1"), 0o600); err != nil {
		t.Fatalf("plant dotenv: %v", err)
	}

	cases := []struct {
		name string
		args map[string]any
		code string
	}{
		{"a missing file", map[string]any{"op": "read", "path": "README"}, toolbroker.AnswerNotFound},
		{"a missing directory listing", map[string]any{"op": "list", "path": "nope"}, toolbroker.AnswerNotFound},
		{"a missing stat", map[string]any{"op": "stat", "path": "nope"}, toolbroker.AnswerNotFound},
		{"a missing checksum", map[string]any{"op": "checksum", "path": "nope"}, toolbroker.AnswerNotFound},
		{"a relative traversal", map[string]any{"op": "read", "path": "../../../../etc/passwd"}, toolbroker.AnswerRefused},
		{"an absolute path", map[string]any{"op": "read", "path": "/etc/passwd"}, toolbroker.AnswerRefused},
		{"an escaping symlink", map[string]any{"op": "read", "path": "escape/passwd"}, toolbroker.AnswerRefused},
		{"a fifo", map[string]any{"op": "read", "path": "runtime.fifo"}, toolbroker.AnswerRefused},
		{"a likely-secret path", map[string]any{"op": "read", "path": ".env"}, toolbroker.AnswerRefused},
		{"an unknown op", map[string]any{"op": "teleport", "path": "x"}, toolbroker.AnswerInvalidArguments},
		{"a traversal WRITE, refused before the temp file exists", map[string]any{"op": "write", "path": "../outside.txt", "content": "x"}, toolbroker.AnswerRefused},
	}
	tool := FileTool()
	for _, tc := range cases {
		_, err := tool.Exec(context.Background(), env, tc.args)
		if err == nil {
			t.Fatalf("%s was ALLOWED; the refusal itself has regressed", tc.name)
		}
		answer, ok := toolbroker.AsAnswer(err)
		if !ok {
			t.Fatalf("%s is not an answer (%v); it would kill the attempt and wedge the run", tc.name, err)
		}
		if answer.Code != tc.code {
			t.Fatalf("%s answer code = %q, want %q", tc.name, answer.Code, tc.code)
		}
	}
	// Non-vacuity: the same tool, the same env, a path that IS there still returns a normal result. Every
	// assertion above would pass against a tool that refused everything.
	if err := os.WriteFile(filepath.Join(root, "real.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("seed real.txt: %v", err)
	}
	out, err := tool.Exec(context.Background(), env, map[string]any{"op": "read", "path": "real.txt"})
	if err != nil || out["content"] != "hello" {
		t.Fatalf("read of an existing file = (%v, %v), want the content", out, err)
	}
}

// TestAWorkspacelessWorkspaceToolAnswersInsteadOfHanging is the case
// docs/operations/palai-on-a-mac.md §4.1 MEASURED on 2026-07-28 and wrote down as a hang: a run offered
// a workspace tool with no workspace bound. Nothing runs, so it is an answer, and the model is told.
func TestAWorkspacelessWorkspaceToolAnswersInsteadOfHanging(t *testing.T) {
	for name, tool := range map[string]toolbroker.Tool{"file": FileTool(), "shell": ShellTool()} {
		env := toolbroker.ExecEnv{}
		if name == "shell" {
			env.Shell = stubShell{} // past the no-posture check, so this test measures the WORKSPACE one
		}
		args := map[string]any{"op": "read", "path": "x"}
		if name == "shell" {
			args = map[string]any{"argv": []any{"echo", "ok"}}
		}
		_, err := tool.Exec(context.Background(), env, args)
		answer, ok := toolbroker.AsAnswer(err)
		if !ok {
			t.Fatalf("%s tool with no workspace bound = %v, want an answer (this shape HUNG a bring-up on 2026-07-28)", name, err)
		}
		if answer.Code != toolbroker.AnswerUnavailable {
			t.Fatalf("%s tool no-workspace code = %q, want %q", name, answer.Code, toolbroker.AnswerUnavailable)
		}
	}
}

// TestTheShellToolAnswersItsPreFlightRefusalsAndFaultsOnTheRest draws the line at the moment a process
// could exist. Everything before env.Shell.Run is a statement about a call that did not happen; Run's
// own error is the one shell outcome that is genuinely uncertain.
func TestTheShellToolAnswersItsPreFlightRefusalsAndFaultsOnTheRest(t *testing.T) {
	tool := ShellTool()
	root := realTempDir(t)

	// No posture configured at all: nothing to run in, nothing ran.
	_, err := tool.Exec(context.Background(), toolbroker.ExecEnv{WorkspaceRoot: root}, map[string]any{"argv": []any{"echo"}})
	if answer, ok := toolbroker.AsAnswer(err); !ok || answer.Code != toolbroker.AnswerUnavailable {
		t.Fatalf("no shell posture = %v, want an %q answer", err, toolbroker.AnswerUnavailable)
	}
	// Malformed argv: rejected before the runner is touched.
	_, err = tool.Exec(context.Background(), toolbroker.ExecEnv{WorkspaceRoot: root, Shell: stubShell{}},
		map[string]any{"argv": "rm -rf /"})
	if answer, ok := toolbroker.AsAnswer(err); !ok || answer.Code != toolbroker.AnswerInvalidArguments {
		t.Fatalf("a bare-string argv = %v, want an %q answer", err, toolbroker.AnswerInvalidArguments)
	}
	// THE FAULT HALF. The runner could not say what the command did — a process may exist.
	_, err = tool.Exec(context.Background(), toolbroker.ExecEnv{WorkspaceRoot: root, Shell: stubShell{err: errors.New("sandbox create failed")}},
		map[string]any{"argv": []any{"echo", "ok"}})
	if err == nil {
		t.Fatal("a failing shell runner returned no error")
	}
	if _, ok := toolbroker.AsAnswer(err); ok {
		t.Fatalf("a shell RUN failure was classified as an answer: %v — a process may exist, so it is uncertain", err)
	}
	// Non-vacuity: the same stub with no error returns a normal result through the same path.
	out, err := tool.Exec(context.Background(), toolbroker.ExecEnv{WorkspaceRoot: root, Shell: stubShell{}},
		map[string]any{"argv": []any{"echo", "ok"}})
	if err != nil || out["exit_code"] != 0 {
		t.Fatalf("a working shell call = (%v, %v), want exit_code 0", out, err)
	}
}

// TestANonZeroExitCodeIsStillAResultAndNotAnError is the DISCRIMINATOR from the reproduction, kept as a
// test so it cannot quietly change: `shell false` completed cleanly while a missing file wedged the run,
// and the difference was that an exit code is a FIELD. Nothing in this change may turn it into an error.
func TestANonZeroExitCodeIsStillAResultAndNotAnError(t *testing.T) {
	out, err := ShellTool().Exec(context.Background(),
		toolbroker.ExecEnv{WorkspaceRoot: realTempDir(t), Shell: stubShell{exit: 1}},
		map[string]any{"argv": []any{"false"}})
	if err != nil {
		t.Fatalf("a non-zero exit produced an error = %v, want a result with exit_code 1", err)
	}
	if out["exit_code"] != 1 {
		t.Fatalf("exit_code = %v, want 1", out["exit_code"])
	}
}

// TestAnAllocationRootThatDoesNotExistIsRefusedBeforeAnyRead measures where the file tool's
// UNCLASSIFIED-fault branch actually is, which turned out not to be where its comment first said.
//
// NewWorkspaceFS calls EvalSymlinks on the root, so a root that is ABSENT fails in the constructor and
// takes the fault path — an allocation that was never provisioned is the deployment being broken, and
// the reconciler's uncertain machinery is the right place for it. But a root that EXISTS and is merely
// unusable (a regular file where a directory should be) constructs fine and fails inside Read — and that
// is an ANSWER, because the rule is about what the operation DID, not about whose fault it is: the read
// touched nothing. This test pins both, so the asymmetry is a decision on the record rather than a
// surprise the next reader has to re-derive.
func TestAnAllocationRootThatDoesNotExistIsRefusedBeforeAnyRead(t *testing.T) {
	base := t.TempDir()

	absent := filepath.Join(base, "never-provisioned")
	_, err := FileTool().Exec(context.Background(), toolbroker.ExecEnv{WorkspaceRoot: absent},
		map[string]any{"op": "read", "path": "anything"})
	if err == nil {
		t.Fatal("a read against an absent allocation root succeeded")
	}
	if _, ok := toolbroker.AsAnswer(err); ok {
		t.Fatalf("an absent allocation root was classified as an answer: %v", err)
	}

	notADir := filepath.Join(base, "root")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err = FileTool().Exec(context.Background(), toolbroker.ExecEnv{WorkspaceRoot: notADir},
		map[string]any{"op": "read", "path": "anything"})
	if _, ok := toolbroker.AsAnswer(err); !ok {
		t.Fatalf("a read that reached the filesystem and failed = %v, want an answer (it changed nothing)", err)
	}
}

// TestARefusalDoesNotCarryTheHostPathOffThisMachine. MEASURED on a live run on 2026-08-01: the first
// refusal this change delivered to a real model read
// `read "README": open /Users/salih/palai-toolerr/workspaces/alloc_de92…/README: no such file`. A refusal
// now travels into a model's context — for a hosted provider, off this machine — and into a durable row,
// and the operator's home directory is no part of anything the model can act on: every path it may name
// is workspace-relative by construction.
//
// The second half is the one that would silently not work: NewWorkspaceFS resolves the root through
// symlinks, so on macOS a /var workspace reports as /private/var. t.TempDir() gives exactly that shape,
// which is why this test can tell the two-substitution version from the one-substitution one.
func TestARefusalDoesNotCarryTheHostPathOffThisMachine(t *testing.T) {
	root := realTempDir(t)
	_, err := FileTool().Exec(context.Background(), toolbroker.ExecEnv{WorkspaceRoot: root},
		map[string]any{"op": "read", "path": "README"})
	answer, ok := toolbroker.AsAnswer(err)
	if !ok {
		t.Fatalf("missing file = %v, want an answer", err)
	}
	msg := answer.Error()
	if strings.Contains(msg, root) {
		t.Fatalf("the refusal carries the declared host root:\n  %s", msg)
	}
	if real, rerr := filepath.EvalSymlinks(root); rerr == nil && strings.Contains(msg, real) {
		t.Fatalf("the refusal carries the RESOLVED host root (the /var -> /private/var alias):\n  %s", msg)
	}
	// Non-vacuity: the message still says what happened and still names the path the MODEL asked for,
	// which is the whole reason to deliver it.
	if !strings.Contains(msg, "README") || !strings.Contains(msg, "no such file") {
		t.Fatalf("the refusal was gutted rather than folded: %q", msg)
	}
	if !strings.Contains(msg, "<workspace>") {
		t.Fatalf("the refusal names no workspace at all: %q", msg)
	}
}

// TestAnAnswerNamesTheCauseItWraps keeps the codes derived from SENTINELS rather than from message text.
func TestAnAnswerNamesTheCauseItWraps(t *testing.T) {
	if got := fileAnswerCode(workspace.ErrPathEscape); got != toolbroker.AnswerRefused {
		t.Fatalf("ErrPathEscape -> %q, want %q", got, toolbroker.AnswerRefused)
	}
	if got := fileAnswerCode(workspace.ErrNotRegular); got != toolbroker.AnswerRefused {
		t.Fatalf("ErrNotRegular -> %q, want %q", got, toolbroker.AnswerRefused)
	}
	if got := fileAnswerCode(&fs.PathError{Err: fs.ErrNotExist}); got != toolbroker.AnswerNotFound {
		t.Fatalf("fs.ErrNotExist -> %q, want %q", got, toolbroker.AnswerNotFound)
	}
	if got := fileAnswerCode(&fs.PathError{Err: fs.ErrPermission}); got != "permission_denied" {
		t.Fatalf("fs.ErrPermission -> %q, want permission_denied", got)
	}
	if got := fileAnswerCode(errors.New("something else entirely")); got != toolbroker.AnswerFailed {
		t.Fatalf("an unrecognised cause -> %q, want the %q fallback rather than a guess", got, toolbroker.AnswerFailed)
	}
}

// stubShell is a ShellRunner that answers without a sandbox, so the classification tests measure the
// shell tool's OWN branches rather than Docker's availability.
type stubShell struct {
	exit int
	err  error
}

func (s stubShell) Run(context.Context, toolbroker.ShellCommand) (toolbroker.ShellResult, error) {
	if s.err != nil {
		return toolbroker.ShellResult{}, s.err
	}
	return toolbroker.ShellResult{ExitCode: s.exit}, nil
}
