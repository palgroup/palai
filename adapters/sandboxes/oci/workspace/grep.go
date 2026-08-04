package workspace

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// GrepQuery is one content search. The field set is the one a coding agent actually uses — what to
// look for, where, how much surrounding context, and how much to return — and deliberately no more.
type GrepQuery struct {
	// Pattern is RE2 regex syntax — the same dialect ripgrep and Go's regexp both implement, so a
	// pattern written for one works in the other. `interface{}` must be written `interface\{\}`.
	Pattern string
	// Path scopes the search to a workspace-relative subtree; "" searches the whole workspace.
	Path string
	// Glob filters by filename, e.g. `*.go` or `**/*.ts`, using this package's glob syntax.
	Glob string
	// OutputMode is "content", "files_with_matches", or "count". Empty means files_with_matches.
	OutputMode string
	Before     int  // lines of leading context, content mode only
	After      int  // lines of trailing context, content mode only
	Multiline  bool // let `.` cross line boundaries
	Limit      int  // maximum entries LISTED; see GrepResult.Total
}

// GrepMatch is one matching line, with any context lines the query asked for.
type GrepMatch struct {
	Path   string
	Line   int
	Text   string
	Before []string
	After  []string
}

// GrepResult carries whichever shape the mode asked for.
type GrepResult struct {
	Mode      string
	Matches   []GrepMatch    // content
	Files     []string       // files_with_matches
	Counts    map[string]int // count
	Total     int            // count: EVERY match, including those the limit did not list
	Truncated bool
}

// maxGrepFileBytes skips files too large to be source. A search is for code a model will read, and a
// multi-megabyte blob is neither readable nor likely to be what was meant.
const maxGrepFileBytes = 1 << 20

// Grep searches file CONTENTS inside the workspace.
//
// IT IS IMPLEMENTED IN GO RATHER THAN BY SHELLING OUT TO ripgrep, and that was a measurement, not a
// preference: ripgrep is not installed on this machine. What looked like a present `rg` during
// planning was a shell FUNCTION supplied by the harness, which a subprocess never sees — the exact
// shape of contaminated measurement this tree's CLAUDE.md records, where the tool that answered was
// not the tool that would run.
//
// THE DIALECT IS UNCHANGED BY THAT DECISION, which is what makes it safe. Go's regexp and ripgrep
// both implement RE2, so a pattern written for one behaves the same in the other. Had the engines
// differed, silently swapping them would have been worse than depending on the binary.
//
// THE COUNT MODE'S TOTAL COVERS EVERY MATCH, including matches in files the limit did not list. A
// total that quietly sums only the listed rows reads as authoritative while under-reporting.
func (w *WorkspaceFS) Grep(ctx context.Context, q GrepQuery) (GrepResult, error) {
	mode := q.OutputMode
	if mode == "" {
		mode = "files_with_matches"
	}
	switch mode {
	case "content", "files_with_matches", "count":
	default:
		return GrepResult{}, fmt.Errorf("unknown output mode %q", q.OutputMode)
	}

	expr := q.Pattern
	if q.Multiline {
		expr = "(?s)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		// The engine's own diagnostic is the only text that tells a caller their REGEX was rejected
		// rather than their files empty — reporting "no matches" here is what sends them looking for a
		// missing file instead of a stray parenthesis.
		return GrepResult{}, fmt.Errorf("pattern %q: %w", q.Pattern, err)
	}

	searchRoot := w.root
	if q.Path != "" {
		abs, resolveErr := w.resolve(q.Path)
		if resolveErr != nil {
			return GrepResult{}, resolveErr
		}
		searchRoot = abs
	}

	var nameFilter *regexp.Regexp
	if q.Glob != "" {
		nameFilter, err = compileGlob(q.Glob)
		if err != nil {
			return GrepResult{}, fmt.Errorf("glob %q: %w", q.Glob, err)
		}
	}

	ignored := w.loadIgnorePrefixes()
	out := GrepResult{Mode: mode, Counts: map[string]int{}}
	var order []string

	walkErr := filepath.WalkDir(searchRoot, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // an unreadable subtree is skipped, not fatal
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rel, relErr := filepath.Rel(w.root, abs)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if isIgnoredPath(rel, ignored) {
				return filepath.SkipDir
			}
			return nil
		}
		if isIgnoredPath(rel, ignored) {
			return nil
		}
		if nameFilter != nil && !nameFilter.MatchString(rel) {
			return nil
		}
		if info, infoErr := d.Info(); infoErr != nil || info.Size() > maxGrepFileBytes {
			return nil
		}
		if _, resolveErr := w.resolve(rel); resolveErr != nil {
			return nil // a symlink leaving the workspace is not part of it
		}
		hits, scanErr := scanFile(abs, rel, re, mode, q.Before, q.After)
		if scanErr != nil {
			return nil // a file this process cannot read is skipped, not fatal
		}
		if len(hits.lines) == 0 {
			return nil
		}
		out.Total += hits.count
		if _, seen := out.Counts[rel]; !seen {
			order = append(order, rel)
		}
		out.Counts[rel] += hits.count
		if mode == "content" {
			out.Matches = append(out.Matches, hits.lines...)
		}
		return nil
	})
	if walkErr != nil {
		return GrepResult{}, fmt.Errorf("search: %w", walkErr)
	}

	sort.Strings(order)
	switch mode {
	case "content":
		if q.Limit > 0 && len(out.Matches) > q.Limit {
			out.Matches = out.Matches[:q.Limit]
			out.Truncated = true
		}
		out.Counts = nil
	case "files_with_matches":
		out.Files = order
		if q.Limit > 0 && len(out.Files) > q.Limit {
			out.Files = out.Files[:q.Limit]
			out.Truncated = true
		}
		out.Counts = nil
	case "count":
		if q.Limit > 0 && len(order) > q.Limit {
			kept := map[string]int{}
			for _, path := range order[:q.Limit] {
				kept[path] = out.Counts[path]
			}
			out.Counts = kept
			out.Truncated = true
			// out.Total is deliberately NOT recomputed from `kept`.
		}
	}
	return out, nil
}

type fileHits struct {
	lines []GrepMatch
	count int
}

// scanFile reads one file line by line. Line-at-a-time keeps a large file off the heap, and it is
// also what makes a line NUMBER available — the thing that lets a model read the match without
// re-opening the whole file.
func scanFile(abs, rel string, re *regexp.Regexp, mode string, before, after int) (fileHits, error) {
	f, err := os.Open(abs)
	if err != nil {
		return fileHits{}, err
	}
	defer f.Close()

	var hits fileHits
	var window []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), maxGrepFileBytes)
	pending := map[int]int{} // index into hits.lines -> remaining trailing-context lines to collect

	for lineNo := 1; scanner.Scan(); lineNo++ {
		text := scanner.Text()
		for idx, remaining := range pending {
			if remaining > 0 {
				hits.lines[idx].After = append(hits.lines[idx].After, text)
				pending[idx] = remaining - 1
			}
			if pending[idx] == 0 {
				delete(pending, idx)
			}
		}
		if re.MatchString(text) {
			hits.count++
			if mode == "content" {
				m := GrepMatch{Path: rel, Line: lineNo, Text: text}
				if before > 0 && len(window) > 0 {
					start := max(0, len(window)-before)
					m.Before = append([]string(nil), window[start:]...)
				}
				hits.lines = append(hits.lines, m)
				if after > 0 {
					pending[len(hits.lines)-1] = after
				}
			} else if len(hits.lines) == 0 {
				// Non-content modes only need to know the file matched; one placeholder is enough to
				// make len(hits.lines) non-zero without holding every line.
				hits.lines = append(hits.lines, GrepMatch{Path: rel, Line: lineNo})
			}
		}
		if before > 0 {
			window = append(window, text)
			if len(window) > before {
				window = window[1:]
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fileHits{}, err
	}
	return hits, nil
}

// loadIgnorePrefixes reads the workspace's root .gitignore for the simple directory and suffix
// patterns that matter to a code search, plus `.git` itself.
//
// IT IS NOT A GITIGNORE IMPLEMENTATION, and saying so is the honest form of that. Negation (`!`),
// nested .gitignore files, and the full pattern grammar are not handled. What it covers is the case
// that actually corrupts a search — a vendored or build directory listed at the root — and a caller
// who needs one of those files can still name its path directly.
func (w *WorkspaceFS) loadIgnorePrefixes() []string {
	ignored := []string{".git"}
	data, err := os.ReadFile(filepath.Join(w.root, ".gitignore"))
	if err != nil {
		return ignored
	}
	for _, line := range strings.Split(string(data), "\n") {
		entry := strings.TrimSpace(line)
		if entry == "" || strings.HasPrefix(entry, "#") || strings.HasPrefix(entry, "!") {
			continue
		}
		entry = strings.Trim(entry, "/")
		if entry == "" || strings.ContainsAny(entry, "*?[") {
			continue // globbed ignore rules are out of scope; see the doc comment
		}
		ignored = append(ignored, entry)
	}
	return ignored
}

// isIgnoredPath reports whether a workspace-relative path sits under one of the ignored names.
func isIgnoredPath(rel string, ignored []string) bool {
	for _, name := range ignored {
		if rel == name || strings.HasPrefix(rel, name+"/") {
			return true
		}
	}
	return false
}
