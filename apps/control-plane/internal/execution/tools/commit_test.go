package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// TestCommitDoesNotImplyPush proves the commit tool (spec §30.7): it records a local Git commit under
// a fixed author with NO credential, and its result grants NO push capability — committing does not
// imply the ability to push (push is a separate approved capability, T8). The commit really lands
// (HEAD advances, the new file is tracked), but the tool surface exposes only the commit sha.
func TestCommitDoesNotImplyPush(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not found: %v", err)
	}
	root := realTempDir(t)
	repoDir := filepath.Join(root, workspace.RepoDir)
	base := initRepo(t, repoDir)

	// A file the model wrote via the file tool, then commits.
	if err := os.WriteFile(filepath.Join(repoDir, "new.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := commitExec(context.Background(), toolbroker.ExecEnv{WorkspaceRoot: root}, map[string]any{"message": "add new.txt"})
	if err != nil {
		t.Fatalf("commitExec() error = %v", err)
	}

	// The result carries ONLY the commit sha — no push token, credential, remote, or publication handle.
	if len(out) != 1 {
		t.Fatalf("commit result = %v, want only {commit}", out)
	}
	sha, _ := out["commit"].(string)
	if len(sha) < 7 {
		t.Fatalf("commit result sha = %q, want a real commit id", sha)
	}
	for _, k := range []string{"push", "pushed", "credential", "token", "remote", "publication"} {
		if _, leaked := out[k]; leaked {
			t.Fatalf("commit result leaked a push-capability key %q: %v", k, out)
		}
	}

	// The commit really landed: HEAD advanced past base and the new file is in the tree.
	if head := repoGit(t, repoDir)("rev-parse", "HEAD"); head == base {
		t.Fatalf("HEAD did not advance: still %s", base)
	}
	if tracked := repoGit(t, repoDir)("ls-files", "new.txt"); !strings.Contains(tracked, "new.txt") {
		t.Fatalf("new.txt not tracked after commit: %q", tracked)
	}
}

// initRepo creates a git repo at dir with one base commit and returns the base sha.
func initRepo(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	run := repoGit(t, dir)
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-q", "-m", "base")
	return run("rev-parse", "HEAD")
}

func repoGit(t *testing.T, dir string) func(args ...string) string {
	t.Helper()
	return func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e.test", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e.test")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
}
