package execution

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/packages/coordinator"
)

type fakeMergeStore struct{ recorded *coordinator.MergeRecordInput }

func (f *fakeMergeStore) RecordMerge(_ context.Context, _ coordinator.Tenant, in coordinator.MergeRecordInput) error {
	f.recorded = &in
	return nil
}

// TestMergeChildBranchRecordsConflictOutcome proves the merge seam (spec §30.5, REP-011): a
// conflicting child merge is recorded merged=false with the source child run + conflict paths, and
// the parent worktree is left consistent (the underlying merge aborts). A clean merge is recorded true.
func TestMergeChildBranchRecordsConflictOutcome(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not found: %v", err)
	}
	ctx := context.Background()
	repo, base := newWorkRepo(t)
	run := repoGit(t, repo)

	// Child branch and parent both edit the same file differently -> conflict.
	wt, err := repositories.AddIsolatedWorktree(ctx, repo, filepath.Join(t.TempDir(), "child"), "ses1", "run1", base)
	if err != nil {
		t.Fatalf("AddIsolatedWorktree() error = %v", err)
	}
	_ = os.WriteFile(filepath.Join(wt.Path, "f.txt"), []byte("child\n"), 0o644)
	child := repoGit(t, wt.Path)
	child("add", "f.txt")
	child("commit", "-q", "-m", "child")
	_ = os.WriteFile(filepath.Join(repo, "f.txt"), []byte("parent\n"), 0o644)
	run("add", "f.txt")
	run("commit", "-q", "-m", "parent")

	store := &fakeMergeStore{}
	tenant := coordinator.Tenant{Organization: "org_x", Project: "prj_x"}
	res, err := MergeChildBranch(ctx, store, tenant, MergeChildBranchInput{
		MergeID: "mrg_1", ParentRunID: "run_parent", SourceChildRunID: "run_child",
		RepoDir: repo, ChildBranch: "agent/ses1/run1",
	})
	if err != nil {
		t.Fatalf("MergeChildBranch() error = %v", err)
	}
	if res.Merged || store.recorded == nil || store.recorded.Merged {
		t.Fatalf("conflict result = %+v recorded = %+v, want merged=false recorded", res, store.recorded)
	}
	if store.recorded.SourceChildRunID != "run_child" || len(store.recorded.ConflictPaths) == 0 {
		t.Fatalf("recorded merge = %+v, want source child run + conflict paths", *store.recorded)
	}
}

func newWorkRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	run := repoGit(t, dir)
	run("init", "-q", "-b", "main")
	_ = os.WriteFile(filepath.Join(dir, "f.txt"), []byte("base\n"), 0o644)
	run("add", "f.txt")
	run("commit", "-q", "-m", "base")
	return dir, run("rev-parse", "HEAD")
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
