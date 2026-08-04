package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func grepFixture(t *testing.T) *WorkspaceFS {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	files := map[string]string{
		"a.go":         "package main\nfunc Alpha() {}\n// Alpha again\n",
		"b.go":         "package main\nfunc Beta() {}\n",
		"src/c.go":     "package src\nfunc Alpha() {}\n",
		"notes.md":     "Alpha is documented here\n",
		".gitignore":   "ignored/\n",
		"ignored/d.go": "func Alpha() {}\n",
	}
	for rel, body := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	fs, err := NewWorkspaceFS(root)
	if err != nil {
		t.Fatalf("NewWorkspaceFS: %v", err)
	}
	return fs
}

// TestGrepContentModeCarriesFileAndLine — the mode a model uses to read what it found. A match with
// no line number sends it back to read the whole file, which is the cost this tool exists to avoid.
func TestGrepContentModeCarriesFileAndLine(t *testing.T) {
	fs := grepFixture(t)
	got, err := fs.Grep(context.Background(), GrepQuery{Pattern: `func Alpha`, OutputMode: "content"})
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if len(got.Matches) != 2 {
		t.Fatalf("matched %+v, want a.go and src/c.go", got.Matches)
	}
	for _, m := range got.Matches {
		if m.Path == "" || m.Line == 0 || m.Text == "" {
			t.Errorf("incomplete match %+v: a model cannot act on it", m)
		}
	}
}

// TestGrepDefaultsToFilesWithMatches pins the default mode. Files-with-matches is the cheap answer a
// model wants first: which files are worth opening.
func TestGrepDefaultsToFilesWithMatches(t *testing.T) {
	fs := grepFixture(t)
	got, err := fs.Grep(context.Background(), GrepQuery{Pattern: `Alpha`})
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if got.Mode != "files_with_matches" {
		t.Errorf("mode = %q, want files_with_matches by default", got.Mode)
	}
	if len(got.Files) == 0 {
		t.Fatal("no files reported")
	}
	for _, f := range got.Files {
		if f == "" {
			t.Error("an empty path in the file list")
		}
	}
}

// TestGrepCountTotalsEveryMatchEvenWhenTruncated is the bug Claude Code shipped until v2.1.208: a
// per-file list capped by a limit, with a total that summed only the listed entries. A total that
// silently under-reports is worse than no total, because it reads as authoritative.
func TestGrepCountTotalsEveryMatchEvenWhenTruncated(t *testing.T) {
	fs := grepFixture(t)
	full, err := fs.Grep(context.Background(), GrepQuery{Pattern: `Alpha`, OutputMode: "count"})
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if full.Total < 3 {
		t.Fatalf("total = %d, want every Alpha across the tracked files", full.Total)
	}

	capped, err := fs.Grep(context.Background(), GrepQuery{Pattern: `Alpha`, OutputMode: "count", Limit: 1})
	if err != nil {
		t.Fatalf("grep with a limit: %v", err)
	}
	if len(capped.Counts) != 1 {
		t.Errorf("limit 1 listed %d files", len(capped.Counts))
	}
	if capped.Total != full.Total {
		t.Errorf("truncated total = %d, want the untruncated %d — the cap trims the LIST, not the count",
			capped.Total, full.Total)
	}
	if !capped.Truncated {
		t.Error("a capped count did not report truncation")
	}
}

// TestGrepRespectsGitignoreUnlessAskedDirectly — the ignored file exists and contains the pattern, so
// a search that returns it is searching build output and vendored code the model did not ask about.
func TestGrepRespectsGitignore(t *testing.T) {
	fs := grepFixture(t)
	got, err := fs.Grep(context.Background(), GrepQuery{Pattern: `Alpha`})
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	for _, f := range got.Files {
		if f == "ignored/d.go" {
			t.Error("a gitignored file was searched")
		}
	}
}

// TestGrepScopesByGlob — the filter that keeps a search inside one language or one subtree.
func TestGrepScopesByGlob(t *testing.T) {
	fs := grepFixture(t)
	got, err := fs.Grep(context.Background(), GrepQuery{Pattern: `Alpha`, Glob: "*.md"})
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if len(got.Files) != 1 || got.Files[0] != "notes.md" {
		t.Errorf("glob *.md matched %v, want only notes.md", got.Files)
	}
}

// TestGrepReportsABadPatternWithTheEngineDiagnostic — a rejected pattern must say it was rejected.
// Claude Code reported one as "no files found" until v2.1.208, which sends a model looking for a
// missing file rather than for the stray parenthesis it actually typed.
func TestGrepReportsABadPatternWithTheEngineDiagnostic(t *testing.T) {
	fs := grepFixture(t)
	_, err := fs.Grep(context.Background(), GrepQuery{Pattern: `func Alpha(`})
	if err == nil {
		t.Fatal("an unparseable regex returned no error")
	}
	if errors.Is(err, ErrPathEscape) {
		t.Errorf("a regex error was classified as a path escape: %v", err)
	}
}

// TestGrepRefusesAnEscapingPath — same rule as glob: reaching outside is refused, not emptied.
func TestGrepRefusesAnEscapingPath(t *testing.T) {
	fs := grepFixture(t)
	if _, err := fs.Grep(context.Background(), GrepQuery{Pattern: `Alpha`, Path: "../"}); !errors.Is(err, ErrPathEscape) {
		t.Errorf("error = %v, want ErrPathEscape", err)
	}
}

// TestGrepFindsNothingWithoutErroring — an empty result is an ordinary answer and must be
// distinguishable from a failure, or a model cannot tell "not here" from "broken".
func TestGrepFindsNothingWithoutErroring(t *testing.T) {
	fs := grepFixture(t)
	got, err := fs.Grep(context.Background(), GrepQuery{Pattern: `NoSuchSymbolAnywhere`})
	if err != nil {
		t.Fatalf("a pattern with no matches returned an error: %v", err)
	}
	if len(got.Files) != 0 {
		t.Errorf("files = %v, want none", got.Files)
	}
}
