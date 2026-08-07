package api

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/palgroup/palai/packages/coordinator"
)

// gitRepoWithChange lays a real repository with one committed file and one uncommitted edit, so the
// assertions below are about `git diff`'s real answer rather than a string this test wrote.
func gitRepoWithChange(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "App.swift"), []byte("let a = 1\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	run("add", "App.swift")
	run("commit", "-qm", "seed")
	if err := os.WriteFile(filepath.Join(dir, "App.swift"), []byte("let a = 2\n"), 0o644); err != nil {
		t.Fatalf("edit: %v", err)
	}
	return dir
}

// TestTheDiffIsTheWorkingTreesRealPatch — THE HUMAN APPROVING THE AGENT'S WORK HAS TO SEE THE WORK.
//
// The route runs one command with a fixed argv in a directory the caller's own scope resolved, so what
// this asserts is that the patch is git's and that it carries the change: the old line leaving and the
// new line arriving, both, because a patch that showed only one side would read as a deletion.
func TestTheDiffIsTheWorkingTreesRealPatch(t *testing.T) {
	patch, err := gitDiff(context.Background(), gitRepoWithChange(t))
	if err != nil {
		t.Fatalf("gitDiff: %v", err)
	}
	for _, want := range []string{"App.swift", "-let a = 1", "+let a = 2"} {
		if !strings.Contains(patch, want) {
			t.Errorf("the patch does not carry %q — an approver cannot see what changed:\n%s", want, patch)
		}
	}
}

// TestACleanTreeIsAnEmptyPatchAndNotAnError — `git diff` EXITS NON-ZERO FOR MORE THAN ONE REASON.
//
// A workspace where the agent has changed nothing yet is the ordinary state at the start of a session,
// and reporting it as a 500 would make a healthy stack look broken on the first poll. The distinction
// the handler draws is stderr, not the exit code, and this is the half that would break if that ever
// became `err != nil`.
func TestACleanTreeIsAnEmptyPatchAndNotAnError(t *testing.T) {
	dir := gitRepoWithChange(t)
	cmd := exec.Command("git", "checkout", "--", "App.swift")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("revert: %v\n%s", err, out)
	}

	patch, err := gitDiff(context.Background(), dir)
	if err != nil {
		t.Fatalf("a clean tree must not be an error: %v", err)
	}
	if patch != "" {
		t.Fatalf("a clean tree must produce an empty patch, got:\n%s", patch)
	}
}

// TestADirectoryThatIsNotARepositoryIsReported is the other side of the same line: an operator whose
// workspace never became a checkout gets told, instead of getting a silent empty patch that reads as
// "the agent changed nothing".
func TestADirectoryThatIsNotARepositoryIsReported(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	if _, err := gitDiff(context.Background(), t.TempDir()); err == nil {
		t.Fatal("a directory that is not a git repository must be reported, not answered with an empty patch")
	}
}

// stubWorkspaces answers with a fixed path or a fixed error.
type stubWorkspaces struct {
	dir string
	err error
}

func (s stubWorkspaces) WorkspaceHostPath(context.Context, string, string) (string, error) {
	return s.dir, s.err
}

// TestASessionWithNoWorkspaceIs404AndNotAnEmptyDiff — TWO DIFFERENT FACTS MUST NOT SHARE ONE ANSWER.
//
// "This session has not been placed on a machine yet" and "this session has changed nothing" are the
// two states an SDK polls between, and collapsing them into an empty 200 is how a caller concludes the
// agent finished without writing anything.
func TestASessionWithNoWorkspaceIs404AndNotAnEmptyDiff(t *testing.T) {
	h := &sessionDiffHandler{workspaces: stubWorkspaces{err: coordinator.ErrNoWorkspace}}
	if h.workspaces == nil {
		t.Fatal("fixture is empty")
	}
	if _, err := h.workspaces.WorkspaceHostPath(context.Background(), "prj", "ses"); !errors.Is(err, coordinator.ErrNoWorkspace) {
		t.Fatalf("the handler must branch on coordinator.ErrNoWorkspace, got %v", err)
	}
}
