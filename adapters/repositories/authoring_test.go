package repositories

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestWorkingStatusSeesEveryShapeOfChangeInTheClone drives the observation the changeset compiler
// depends on for everything the shell tool writes. Each case below is a shape the file-tool ledger
// cannot report, and two of them are parser traps rather than features:
//
//   - "modified" is the TRIM TRAP. In `--porcelain=v1` a worktree-only modification spends its first
//     column on the index status, so the record begins with a SPACE — and this package's git helper
//     returns strings.TrimSpace(stdout), which would eat it and shift every field of the first record
//     by one column. The only change here is that modification, so it IS the first record, and a
//     regression to v1 mis-parses it rather than getting lucky about ordering.
//   - "a path with a space" is the QUOTING TRAP. Without -z, git C-quotes any path with a space,
//     quote or newline, and the changeset would carry `"sp ace.txt"` — quotes and all — as the path.
func TestWorkingStatusSeesEveryShapeOfChangeInTheClone(t *testing.T) {
	repoDir := newStatusRepo(t)

	writeStatusFile(t, filepath.Join(repoDir, "f.txt"), "rewritten\n") // worktree-only modification
	writeStatusFile(t, filepath.Join(repoDir, "new.swift"), "new\n")   // untracked
	writeStatusFile(t, filepath.Join(repoDir, "sp ace.txt"), "sp\n")   // untracked, space in the path
	if err := os.Remove(filepath.Join(repoDir, "gone.txt")); err != nil {
		t.Fatal(err)
	}

	changes, ignored, err := WorkingStatus(context.Background(), repoDir)
	if err != nil {
		t.Fatalf("WorkingStatus() error = %v", err)
	}
	if ignored != 0 {
		t.Fatalf("ignored = %d, want 0 — this repo has no ignore rules", ignored)
	}
	want := map[string]string{
		"f.txt":      "modified",
		"new.swift":  "added",
		"sp ace.txt": "added",
		"gone.txt":   "deleted",
	}
	assertChanges(t, changes, want)
}

// TestWorkingStatusReportsARenameAsBothEnds pins the rename decision. A renamed file is one git record
// naming two paths, and a changed set that reported only the new one would claim the old path still
// holds its content. Both ends are reported, in the vocabulary the record already has.
func TestWorkingStatusReportsARenameAsBothEnds(t *testing.T) {
	repoDir := newStatusRepo(t)
	git := statusGit(t, repoDir)
	git("mv", "f.txt", "renamed.txt")

	changes, _, err := WorkingStatus(context.Background(), repoDir)
	if err != nil {
		t.Fatalf("WorkingStatus() error = %v", err)
	}
	assertChanges(t, changes, map[string]string{"renamed.txt": "added", "f.txt": "deleted"})
}

// TestWorkingStatusCountsIgnoredFilesIndividually is the measurement the ignored count rests on. git
// has more than one way to report ignored paths and only one of them enumerates FILES: with
// `--ignored=matching` an ignored directory comes back as the single entry ".build/", so a run that
// wrote a thousand object files would be counted as one. `--ignored=traditional` with
// `--untracked-files=all` lists each file, which is the number worth recording.
func TestWorkingStatusCountsIgnoredFilesIndividually(t *testing.T) {
	repoDir := newStatusRepo(t)
	writeStatusFile(t, filepath.Join(repoDir, ".gitignore"), ".build/\n")
	if err := os.MkdirAll(filepath.Join(repoDir, ".build", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeStatusFile(t, filepath.Join(repoDir, ".build", "a.o"), "a\n")
	writeStatusFile(t, filepath.Join(repoDir, ".build", "sub", "b.o"), "b\n")
	writeStatusFile(t, filepath.Join(repoDir, ".build", "sub", "c.o"), "c\n")

	changes, ignored, err := WorkingStatus(context.Background(), repoDir)
	if err != nil {
		t.Fatalf("WorkingStatus() error = %v", err)
	}
	if ignored != 3 {
		t.Fatalf("ignored = %d, want 3 — a directory counted as one entry is the defect this pins", ignored)
	}
	// Non-vacuity: the ignore FILE itself is a real change and is still reported, so this is exclusion
	// of ignored content rather than the scan having stopped seeing anything.
	assertChanges(t, changes, map[string]string{".gitignore": "added"})
}

// TestWorkingStatusReadsAConflictedPathWhole is the third parser trap, and the one that was written
// wrong first: a `u` (unmerged) record carries THREE stage hashes and three modes where an ordinary
// `1` record carries two hashes, so it is ELEVEN space-separated fields, not nine. Splitting it like
// an ordinary record silently folds the last two hashes into the path — a changeset entry whose
// "path" is "<hash> <hash> f.txt". The shell tool can leave a clone in this state with one `git
// merge` or `git apply -3`, so it is reachable, and the corrupted form would still look like data.
func TestWorkingStatusReadsAConflictedPathWhole(t *testing.T) {
	repoDir := newStatusRepo(t)
	git := statusGit(t, repoDir)
	git("checkout", "-q", "-b", "other")
	writeStatusFile(t, filepath.Join(repoDir, "f.txt"), "other\n")
	git("-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-qam", "other")
	git("checkout", "-q", "main")
	writeStatusFile(t, filepath.Join(repoDir, "f.txt"), "main\n")
	git("-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-qam", "main")
	// The merge conflicts, which is the point; its non-zero exit is not a test failure.
	merge := exec.Command("git", "merge", "other")
	merge.Dir = repoDir
	_ = merge.Run()

	changes, _, err := WorkingStatus(context.Background(), repoDir)
	if err != nil {
		t.Fatalf("WorkingStatus() error = %v", err)
	}
	assertChanges(t, changes, map[string]string{"f.txt": "modified"})
}

func assertChanges(t *testing.T, got []WorkingChange, want map[string]string) {
	t.Helper()
	byPath := map[string]string{}
	for _, c := range got {
		byPath[c.Path] = c.Change
	}
	for path, change := range want {
		if byPath[path] != change {
			t.Fatalf("%q = %q, want %q (full status: %+v)", path, byPath[path], change, got)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("status = %+v, want exactly %d entries", got, len(want))
	}
}

// newStatusRepo returns a git repo with one commit holding f.txt and gone.txt, so a test can modify
// one tracked file and delete another.
func newStatusRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not found: %v", err)
	}
	repoDir := t.TempDir()
	if r, err := filepath.EvalSymlinks(repoDir); err == nil {
		repoDir = r
	}
	git := statusGit(t, repoDir)
	git("init", "-q", "-b", "main")
	writeStatusFile(t, filepath.Join(repoDir, "f.txt"), "base\n")
	writeStatusFile(t, filepath.Join(repoDir, "gone.txt"), "gone\n")
	git("add", "f.txt", "gone.txt")
	git("-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-q", "-m", "base")
	return repoDir
}

func statusGit(t *testing.T, repoDir string) func(args ...string) {
	t.Helper()
	return func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func writeStatusFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
