package workspace

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Glob returns the workspace-relative paths of regular files matching a glob pattern, newest
// modification first, capped at limit (0 means uncapped). The second return reports whether matches
// were dropped by that cap.
//
// THE ORDER IS WHAT MAKES A TRUNCATED ANSWER USEFUL. A caller that asks for a hundred paths and gets
// the hundred oldest has been handed the least likely candidates; newest-first means the cut falls
// on the files least likely to be the ones being worked on. Claude Code's Glob orders the same way,
// for the same reason.
//
// CONFINEMENT IS ENFORCED TWICE, and neither check is redundant. The pattern itself is refused if it
// reaches outward, because a caller who writes `../*` has made an error worth naming — returning an
// empty list would read as "no such files" and send them looking for a different cause. Then every
// candidate path is put through resolve(), which is what catches a symlink inside the workspace
// pointing out of it: the walk starts at the root, so only a link can leave it.
func (w *WorkspaceFS) Glob(pattern string, limit int) ([]string, bool, error) {
	if filepath.IsAbs(pattern) {
		return nil, false, fmt.Errorf("%w: %q is absolute", ErrPathEscape, pattern)
	}
	if slashed := filepath.ToSlash(pattern); slashed == ".." ||
		strings.HasPrefix(slashed, "../") || strings.Contains(slashed, "/../") {
		return nil, false, fmt.Errorf("%w: %q", ErrPathEscape, pattern)
	}
	matcher, err := compileGlob(pattern)
	if err != nil {
		return nil, false, fmt.Errorf("glob %q: %w", pattern, err)
	}

	type match struct {
		rel string
		mod time.Time
	}
	var matches []match
	walkErr := filepath.WalkDir(w.root, func(abs string, d fs.DirEntry, err error) error {
		// AN UNREADABLE SUBTREE IS SKIPPED, NOT FATAL: a workspace with one permission-denied directory
		// still answers for the rest of itself, which is what a caller can act on.
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // deliberate: skip what cannot be read, keep walking
		}
		rel, relErr := filepath.Rel(w.root, abs)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if !matcher.MatchString(rel) {
			return nil
		}
		if _, resolveErr := w.resolve(rel); resolveErr != nil {
			return nil // a symlink that leaves the workspace is not part of it
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}
		matches = append(matches, match{rel: rel, mod: info.ModTime()})
		return nil
	})
	if walkErr != nil {
		return nil, false, fmt.Errorf("glob %q: %w", pattern, walkErr)
	}

	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].mod.Equal(matches[j].mod) {
			return matches[i].rel < matches[j].rel // a stable tiebreak keeps the answer reproducible
		}
		return matches[i].mod.After(matches[j].mod)
	})

	truncated := limit > 0 && len(matches) > limit
	if truncated {
		matches = matches[:limit]
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m.rel)
	}
	return out, truncated, nil
}

// compileGlob turns a glob pattern into an anchored regexp over slash-separated relative paths.
//
// IT IS NOT path.Match, because that one has no `**`: it treats every `*` as "anything but a
// separator", so `**/*.go` would match nothing below the top level and a model searching a real tree
// would conclude its files are absent. The translation is the smallest thing that fixes that:
//
//	**/  → any number of leading directories, including none
//	**   → anything, separators included
//	*    → anything within one path segment
//	?    → one character within one path segment
//
// Everything else is quoted, so a dot in `*.go` matches a dot and not any character.
func compileGlob(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString(`^`)
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				i++
				if i+1 < len(pattern) && pattern[i+1] == '/' {
					i++
					b.WriteString(`(?:.*/)?`)
				} else {
					b.WriteString(`.*`)
				}
				continue
			}
			b.WriteString(`[^/]*`)
		case '?':
			b.WriteString(`[^/]`)
		case '[', ']':
			// Character classes are NOT supported, and saying so beats guessing: a caller who writes
			// one gets an error naming the pattern rather than a silently different match set.
			return nil, fmt.Errorf("character classes are not supported in a glob pattern")
		default:
			b.WriteString(regexp.QuoteMeta(pattern[i : i+1]))
		}
	}
	b.WriteString(`$`)
	return regexp.Compile(b.String())
}
