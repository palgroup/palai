package repositories

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestAFileTrackedANDIgnoredIsNotReportedDELETED — THE DIFF TOLD THE HUMAN THE AGENT DESTROYED SIXTY-FOUR
// FILES IT NEVER TOUCHED.
//
// Measured on a real repository on 2026-08-08: a run that created ONE file produced a changeset of
// "65 deleted, 1 added", while `git status` in that same workspace, against that same base commit,
// reported nothing but the one new file.
//
// The two sides of the comparison were not seeing the same tree. `add -A` RESPECTS .gitignore; a base
// commit does not. So every path TRACKED in base and IGNORED today — `.claude/` in that repository, and
// the pattern is everywhere: a committed .vscode, a lockfile added before the rule, generated assets
// somebody checked in — was never staged into the scratch index, and `diff --cached base` can only read
// that as a deletion.
//
// The fixture is the exact shape: a file committed FIRST and ignored AFTER, which is how every real one
// of these comes to exist.
func TestAFileTrackedANDIgnoredIsNotReportedDELETED(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(rel, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	git("init", "-q")
	write(".claude/rules.md", "committed before the ignore rule\n")
	write("App.swift", "let a = 1\n")
	git("add", "-A")
	git("commit", "-qm", "base")
	base := git("rev-parse", "HEAD")

	// The ignore rule arrives AFTER the file is tracked — the shape every real instance has.
	write(".gitignore", ".claude/\n")
	git("add", ".gitignore")
	git("commit", "-qm", "ignore .claude")
	base = git("rev-parse", "HEAD")

	// The agent's actual change: one new file. Nothing else is touched.
	write("PALAI_DEMO.md", "hello from the demo\n")

	changeset, err := DiffWorkingTree(context.Background(), dir, base, 1<<20)
	if err != nil {
		t.Fatalf("WorkingChangesetAt: %v", err)
	}

	var deleted []string
	for _, f := range changeset.Changes {
		if f.Change == "deleted" {
			deleted = append(deleted, f.Path)
		}
	}
	if len(deleted) != 0 {
		t.Errorf("the changeset reports %v as DELETED. They are tracked in the base commit and ignored by "+
			".gitignore, and nothing removed them — the diff a human approves would say the agent destroyed "+
			"files it never touched", deleted)
	}
	added := 0
	for _, f := range changeset.Changes {
		if f.Change == "added" && f.Path == "PALAI_DEMO.md" {
			added++
		}
	}
	if added != 1 {
		t.Errorf("the one real change is missing from the changeset: %+v", changeset.Changes)
	}
}
