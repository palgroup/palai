//go:build component

package repository

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/palgroup/palai/adapters/repositories"
)

// TestChildIsolatedWorktreeSeparateBranch proves SUB-006 (spec §30.5): a code-editing child gets an
// isolated worktree on its own agent/<ses>/<run> branch; its edits land on that branch and the PARENT
// worktree is not mutated.
func TestChildIsolatedWorktreeSeparateBranch(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	repo := newLocalRemote(t) // a repo with README.md = "hello world" at repo.head
	run := gitRunner(t, repo.url)

	wt, err := repositories.AddIsolatedWorktree(ctx, repo.url, filepath.Join(t.TempDir(), "child"), "ses1", "run1", repo.head)
	if err != nil {
		t.Fatalf("AddIsolatedWorktree() error = %v", err)
	}
	if wt.Branch != "agent/ses1/run1" || !wt.Writable {
		t.Fatalf("worktree = %+v, want writable branch agent/ses1/run1", wt)
	}

	// The child edits + commits IN ITS WORKTREE, on its branch.
	if err := os.WriteFile(filepath.Join(wt.Path, "README.md"), []byte("child edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	child := gitRunner(t, wt.Path)
	child("add", "README.md")
	child("commit", "-q", "-m", "child change")

	// The parent worktree is untouched; the child branch carries the edit.
	if got, err := os.ReadFile(filepath.Join(repo.url, "README.md")); err != nil || strings.TrimSpace(string(got)) != "hello world" {
		t.Fatalf("parent README = %q (err=%v), want unchanged 'hello world'", got, err)
	}
	if branch := run("branch", "--list", "agent/ses1/run1"); !strings.Contains(branch, "agent/ses1/run1") {
		t.Fatalf("child branch not present in the shared repo: %q", branch)
	}
}

// TestExplicitMergeDetectsConflictParentConsistent proves REP-011 (spec §30.5): a conflicting child
// merge returns a typed conflict result and ABORTS, leaving the parent worktree exactly as it was —
// the conflict is reported, never silently overwritten. A non-conflicting merge applies cleanly.
func TestExplicitMergeDetectsConflictParentConsistent(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	repo := newLocalRemote(t)
	run := gitRunner(t, repo.url)

	// Child branch from base edits README.md; the parent then edits the SAME file differently.
	wt, err := repositories.AddIsolatedWorktree(ctx, repo.url, filepath.Join(t.TempDir(), "child"), "ses1", "run1", repo.head)
	if err != nil {
		t.Fatalf("AddIsolatedWorktree() error = %v", err)
	}
	child := gitRunner(t, wt.Path)
	_ = os.WriteFile(filepath.Join(wt.Path, "README.md"), []byte("child version\n"), 0o644)
	child("add", "README.md")
	child("commit", "-q", "-m", "child change")

	_ = os.WriteFile(filepath.Join(repo.url, "README.md"), []byte("parent version\n"), 0o644)
	run("add", "README.md")
	run("commit", "-q", "-m", "parent change")
	parentHeadBefore := run("rev-parse", "HEAD")

	res, err := repositories.MergeBranch(ctx, repo.url, "agent/ses1/run1")
	if err != nil {
		t.Fatalf("MergeBranch() error = %v", err)
	}
	if res.Merged || len(res.ConflictPaths) == 0 {
		t.Fatalf("merge result = %+v, want a reported conflict", res)
	}
	// The parent worktree is consistent: file back to the parent version, HEAD unchanged, no merge in
	// progress (the abort restored it).
	if got, _ := os.ReadFile(filepath.Join(repo.url, "README.md")); strings.TrimSpace(string(got)) != "parent version" {
		t.Fatalf("parent README after aborted merge = %q, want 'parent version'", got)
	}
	if head := run("rev-parse", "HEAD"); head != parentHeadBefore {
		t.Fatalf("parent HEAD moved on a conflicting merge: %q != %q", head, parentHeadBefore)
	}
	if _, err := os.Stat(filepath.Join(repo.url, ".git", "MERGE_HEAD")); err == nil {
		t.Fatal("parent left mid-merge (MERGE_HEAD present) after a conflict — abort did not restore it")
	}

	// A non-conflicting child change (a different file) merges cleanly.
	wt2, _ := repositories.AddIsolatedWorktree(ctx, repo.url, filepath.Join(t.TempDir(), "child2"), "ses1", "run2", repo.head)
	child2 := gitRunner(t, wt2.Path)
	_ = os.WriteFile(filepath.Join(wt2.Path, "NEW.md"), []byte("new file\n"), 0o644)
	child2("add", "NEW.md")
	child2("commit", "-q", "-m", "add new file")
	clean, err := repositories.MergeBranch(ctx, repo.url, "agent/ses1/run2")
	if err != nil || !clean.Merged {
		t.Fatalf("clean MergeBranch() = %+v err=%v, want merged", clean, err)
	}
	if _, err := os.Stat(filepath.Join(repo.url, "NEW.md")); err != nil {
		t.Fatalf("clean merge did not apply the child's new file: %v", err)
	}
}

// TestChildWorkspaceModeReadOnlyDeniesWrite proves the read_only workspace mode (spec §30.5): a
// read-only child worktree can be read but a write is denied at the filesystem — the adapter-level
// read-only enforcement.
func TestChildWorkspaceModeReadOnlyDeniesWrite(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	repo := newLocalRemote(t)

	roPath := filepath.Join(t.TempDir(), "ro")
	t.Cleanup(func() {
		_ = filepath.Walk(roPath, func(p string, _ os.FileInfo, _ error) error { _ = os.Chmod(p, 0o755); return nil })
	})

	wt, err := repositories.AddReadOnlyWorktree(ctx, repo.url, roPath, repo.head)
	if err != nil {
		t.Fatalf("AddReadOnlyWorktree() error = %v", err)
	}
	if wt.Writable {
		t.Fatal("read-only worktree reports Writable=true")
	}
	// Reads work.
	if got, err := os.ReadFile(filepath.Join(roPath, "README.md")); err != nil || strings.TrimSpace(string(got)) != "hello world" {
		t.Fatalf("read-only worktree read = %q err=%v, want the committed content", got, err)
	}
	// A write to an existing file is denied.
	if err := os.WriteFile(filepath.Join(roPath, "README.md"), []byte("mutate\n"), 0o644); err == nil {
		t.Fatal("write to a read-only worktree succeeded; want denied")
	}
	// Creating a new file is denied too.
	if err := os.WriteFile(filepath.Join(roPath, "NEW.md"), []byte("x\n"), 0o644); err == nil {
		t.Fatal("create in a read-only worktree succeeded; want denied")
	}
}
