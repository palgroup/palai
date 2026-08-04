package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// globFixture builds a small tree and returns its WorkspaceFS. Files are given distinct modification
// times, oldest first in this list, so ordering assertions have something to bite on.
func globFixture(t *testing.T) *WorkspaceFS {
	t.Helper()
	root := t.TempDir()
	for i, rel := range []string{
		"top.go",
		"README.md",
		"src/a.go",
		"src/deep/b.go",
		"src/deep/c.ts",
	} {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(abs, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
		when := time.Now().Add(time.Duration(i-10) * time.Minute)
		if err := os.Chtimes(abs, when, when); err != nil {
			t.Fatalf("chtimes %s: %v", rel, err)
		}
	}
	fs, err := NewWorkspaceFS(root)
	if err != nil {
		t.Fatalf("NewWorkspaceFS: %v", err)
	}
	return fs
}

// TestGlobMatchesByDepth pins the one syntax rule a model relies on: `**` crosses directories and a
// bare `*` does not. Getting this backwards makes every nested search silently empty.
func TestGlobMatchesByDepth(t *testing.T) {
	fs := globFixture(t)

	nested, _, err := fs.Glob("**/*.go", 0)
	if err != nil {
		t.Fatalf("Glob(**/*.go): %v", err)
	}
	if len(nested) != 3 {
		t.Errorf("**/*.go matched %v, want top.go, src/a.go and src/deep/b.go", nested)
	}

	top, _, err := fs.Glob("*.go", 0)
	if err != nil {
		t.Fatalf("Glob(*.go): %v", err)
	}
	if len(top) != 1 || top[0] != "top.go" {
		t.Errorf("*.go matched %v, want only top.go", top)
	}

	scoped, _, err := fs.Glob("src/**/*.ts", 0)
	if err != nil {
		t.Fatalf("Glob(src/**/*.ts): %v", err)
	}
	if len(scoped) != 1 || scoped[0] != "src/deep/c.ts" {
		t.Errorf("src/**/*.ts matched %v, want src/deep/c.ts", scoped)
	}
}

// TestGlobOrdersByModificationTimeNewestFirst — the ordering is what makes a truncated answer useful
// rather than arbitrary: the files a model most likely wants are the ones just touched.
func TestGlobOrdersByModificationTimeNewestFirst(t *testing.T) {
	fs := globFixture(t)
	got, _, err := fs.Glob("**/*", 0)
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	if len(got) < 2 {
		t.Fatalf("matched %d files, want the whole fixture", len(got))
	}
	if got[0] != "src/deep/c.ts" {
		t.Errorf("newest match is %q, want src/deep/c.ts (the last file the fixture touched)", got[0])
	}
	if got[len(got)-1] != "top.go" {
		t.Errorf("oldest match is %q, want top.go", got[len(got)-1])
	}
}

// TestGlobTruncatesAndSaysSo — a caller that cannot tell a complete answer from a clipped one will
// conclude the missing files do not exist.
func TestGlobTruncatesAndSaysSo(t *testing.T) {
	fs := globFixture(t)
	got, truncated, err := fs.Glob("**/*", 2)
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("limit 2 returned %d paths", len(got))
	}
	if !truncated {
		t.Error("truncated is false although matches were dropped")
	}
	if got[0] != "src/deep/c.ts" {
		t.Errorf("a truncated answer kept %q first; the newest match must survive the cut", got[0])
	}

	all, truncatedAll, err := fs.Glob("**/*", 0)
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	if truncatedAll {
		t.Errorf("an unlimited glob reported truncation with %d matches", len(all))
	}
}

// TestGlobRefusesAnEscapingPattern — refusing is not the same as matching nothing. A pattern reaching
// outside the workspace is a caller error worth naming; returning an empty list would read as "there
// are no such files" and send the caller looking for a different reason.
func TestGlobRefusesAnEscapingPattern(t *testing.T) {
	fs := globFixture(t)
	for _, pattern := range []string{"../*", "/etc/*", "src/../../*"} {
		if _, _, err := fs.Glob(pattern, 0); !errors.Is(err, ErrPathEscape) {
			t.Errorf("Glob(%q) error = %v, want ErrPathEscape", pattern, err)
		}
	}
}

// TestGlobReportsABadPattern — an unparseable pattern must say so rather than come back empty, for
// the same reason as above.
func TestGlobReportsABadPattern(t *testing.T) {
	fs := globFixture(t)
	if _, _, err := fs.Glob("[", 0); err == nil {
		t.Error("an unparseable pattern returned no error")
	}
}
