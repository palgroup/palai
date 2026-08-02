package repositories

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestDiffWorkingTreeSeesEveryShapeOfChangeInTheClone drives the observation the changeset compiler
// depends on for everything the shell tool writes. Each case is a shape the file-tool ledger cannot
// report — a deletion most of all, since a ledger of writes has no way to say a file stopped existing.
//
// "sp ace.txt" is the QUOTING TRAP and is the reason the seam reads `-z`: without it git C-quotes any
// path containing a space, a quote or a newline, and the changeset would carry `"sp ace.txt"` — quotes
// included — as the path of a file that is not named that.
func TestDiffWorkingTreeSeesEveryShapeOfChangeInTheClone(t *testing.T) {
	repoDir, base := newStatusRepo(t)

	writeStatusFile(t, filepath.Join(repoDir, "f.txt"), "rewritten\n") // worktree-only modification
	writeStatusFile(t, filepath.Join(repoDir, "new.swift"), "new\n")   // untracked
	writeStatusFile(t, filepath.Join(repoDir, "sp ace.txt"), "sp\n")   // untracked, space in the path
	if err := os.Remove(filepath.Join(repoDir, "gone.txt")); err != nil {
		t.Fatal(err)
	}

	changes, ignored, err := workingChanges(t, repoDir, base)
	if err != nil {
		t.Fatalf("DiffWorkingTree() error = %v", err)
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

// TestDiffWorkingTreeReportsARenameAsBothEnds pins the rename decision. A renamed file is one git record
// naming two paths, and a changed set that reported only the new one would claim the old path still
// holds its content. Both ends are reported, in the vocabulary the record already has.
func TestDiffWorkingTreeReportsARenameAsBothEnds(t *testing.T) {
	repoDir, base := newStatusRepo(t)
	git := statusGit(t, repoDir)
	git("mv", "f.txt", "renamed.txt")

	changes, _, err := workingChanges(t, repoDir, base)
	if err != nil {
		t.Fatalf("DiffWorkingTree() error = %v", err)
	}
	assertChanges(t, changes, map[string]string{"renamed.txt": "added", "f.txt": "deleted"})
}

// TestDiffWorkingTreeCountsIgnoredFilesIndividually is the measurement the ignored count rests on, and
// the sub-directory here is the point. git has more than one way to report ignored paths and they do
// not agree on the UNIT: `git status --ignored=matching` reports an ignored directory as the single
// entry ".build/", so a run that wrote a thousand object files would be counted as one — a number that
// reads like a measurement and is not. `ls-files --others --ignored --exclude-standard` enumerates
// files, and files are what the sentence beside this count claims to be about.
func TestDiffWorkingTreeCountsIgnoredFilesIndividually(t *testing.T) {
	repoDir, base := newStatusRepo(t)
	writeStatusFile(t, filepath.Join(repoDir, ".gitignore"), ".build/\n")
	if err := os.MkdirAll(filepath.Join(repoDir, ".build", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeStatusFile(t, filepath.Join(repoDir, ".build", "a.o"), "a\n")
	writeStatusFile(t, filepath.Join(repoDir, ".build", "sub", "b.o"), "b\n")
	writeStatusFile(t, filepath.Join(repoDir, ".build", "sub", "c.o"), "c\n")

	changes, ignored, err := workingChanges(t, repoDir, base)
	if err != nil {
		t.Fatalf("DiffWorkingTree() error = %v", err)
	}
	if ignored != 3 {
		t.Fatalf("ignored = %d, want 3 — a directory counted as one entry is the defect this pins", ignored)
	}
	// Non-vacuity: the ignore FILE itself is a real change and is still reported, so this is exclusion
	// of ignored content rather than the scan having stopped seeing anything.
	assertChanges(t, changes, map[string]string{".gitignore": "added"})
}

// TestDiffWorkingTreeReadsAConflictedPathWhole keeps a case that an earlier version of this seam got
// wrong. That version read `git status --porcelain=v2`, where a conflicted path is a `u` record
// carrying a mode and a hash for all THREE merge stages — eleven fields where an ordinary record has
// nine — and parsing it as an ordinary one folded two hashes INTO the path.
//
// Staging into a scratch index removes the record type rather than parsing it better: `add -A` takes
// the conflicted file's worktree content and the diff calls it a modification. The case stays because
// the state is reachable — one `git merge` or `git apply -3` from the shell tool — and because a
// changeset must still name the path when it happens.
func TestDiffWorkingTreeReadsAConflictedPathWhole(t *testing.T) {
	repoDir, base := newStatusRepo(t)
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

	changes, _, err := workingChanges(t, repoDir, base)
	if err != nil {
		t.Fatalf("DiffWorkingTree() error = %v", err)
	}
	assertChanges(t, changes, map[string]string{"f.txt": "modified"})
}

// TestDiffWorkingTreeMeasuresFromTheBaseNotHead is the property the whole seam turns on, and the one a
// live run had to teach: the reference point is the run's preparation BASE, so a run that COMMITTED
// its work still reports it.
//
// The earlier version asked `git status`, which answers "what is uncommitted". The commit tool moves
// HEAD, so after a commit that question answers "nothing" — and the only runs that can publish are
// exactly the runs that commit. Everything below is committed before it is measured; a seam that
// reports it is measuring what the run DID, and one that reports an empty list is measuring what is
// left over.
func TestDiffWorkingTreeMeasuresFromTheBaseNotHead(t *testing.T) {
	repoDir, base := newStatusRepo(t)
	git := statusGit(t, repoDir)

	writeStatusFile(t, filepath.Join(repoDir, "FromShell.swift"), "public struct FromShell {}\n")
	writeStatusFile(t, filepath.Join(repoDir, "f.txt"), "rewritten\n")
	if err := os.Remove(filepath.Join(repoDir, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-q", "-m", "the commit tool ran")

	changes, _, err := workingChanges(t, repoDir, base)
	if err != nil {
		t.Fatalf("DiffWorkingTree() error = %v", err)
	}
	assertChanges(t, changes, map[string]string{
		"FromShell.swift": "added",
		"f.txt":           "modified",
		"gone.txt":        "deleted",
	})
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

// workingChanges drives the shipped seam and returns the two parts these tests assert on. It exists so
// each test names the BASE it diffs against — the reference point is the whole design, and a helper
// that hid it would let a test pass against HEAD.
func workingChanges(t *testing.T, repoDir, base string) ([]WorkingChange, int, error) {
	t.Helper()
	out, err := DiffWorkingTree(context.Background(), repoDir, base, 0)
	return out.Changes, out.Ignored, err
}

// newStatusRepo returns a git repo with one commit holding f.txt and gone.txt, plus that commit's sha
// as the base, so a test can modify one tracked file and delete another and still diff from the start.
func newStatusRepo(t *testing.T) (repoDir, base string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not found: %v", err)
	}
	repoDir = t.TempDir()
	if r, err := filepath.EvalSymlinks(repoDir); err == nil {
		repoDir = r
	}
	git := statusGit(t, repoDir)
	git("init", "-q", "-b", "main")
	writeStatusFile(t, filepath.Join(repoDir, "f.txt"), "base\n")
	writeStatusFile(t, filepath.Join(repoDir, "gone.txt"), "gone\n")
	git("add", "f.txt", "gone.txt")
	git("-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-q", "-m", "base")
	out, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return repoDir, strings.TrimSpace(string(out))
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
