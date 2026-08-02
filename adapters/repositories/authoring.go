package repositories

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Commit stages the whole worktree and records a commit under a FIXED, configured author identity
// (spec §30.7): a deterministic author, NO credential, and it grants NO push permission — a commit is
// a local Git operation only, and the credential broker is never touched. It runs under the same
// untrusted-repo hardening as preparation (hooks disabled, ambient config stripped, so no committed
// hook or ~/.gitconfig identity leaks in). The returned sha is the resulting HEAD.
//
// Signing is a SEPARATE brokered capability (§30.7): --no-gpg-sign here keeps the signing key out of
// the workspace; brokered signing is wired by the publication task, not this local commit.
func Commit(ctx context.Context, repoDir, message string) (string, error) {
	if _, err := gitIn(ctx, repoDir, "add", "-A"); err != nil {
		return "", err
	}
	// The identity is fixed here, not caller-supplied, so it needs no flag-shape guard; the message
	// reaches git via -m (never a positional/ref). --allow-empty records a commit even for a no-op run
	// so the changeset always has a deterministic final commit to reference.
	if _, err := gitIn(ctx, repoDir,
		"-c", "user.name="+commitAuthorName,
		"-c", "user.email="+commitAuthorEmail,
		"commit", "--allow-empty", "--no-gpg-sign", "-m", message,
	); err != nil {
		return "", err
	}
	return gitIn(ctx, repoDir, "rev-parse", "HEAD")
}

// commitAuthorName/Email is the deterministic configured author identity a commit tool uses (spec
// §30.7). ponytail: a fixed built-in identity; per-binding author config is a later policy knob, not
// this seam. .invalid is a reserved non-routable TLD, so the email is never a real address.
const (
	commitAuthorName  = "Palai Agent"
	commitAuthorEmail = "agent@palai.invalid"
)

// Head returns repoDir's current HEAD commit and its tree hash (spec §30.6 final commit/tree). When
// the run made no commit, HEAD is still the preparation base, so final equals base — an honest record
// that nothing was committed, not an error.
func Head(ctx context.Context, repoDir string) (commit, tree string, err error) {
	if commit, err = gitIn(ctx, repoDir, "rev-parse", "HEAD"); err != nil {
		return "", "", err
	}
	if tree, err = gitIn(ctx, repoDir, "rev-parse", "HEAD^{tree}"); err != nil {
		return "", "", err
	}
	return commit, tree, nil
}

// WorkingChange is one path the clone's working tree differs from HEAD at, as git itself sees it.
// Path is repo-relative and slash-separated (git's own form); Change is "added", "modified" or
// "deleted".
type WorkingChange struct {
	Path   string
	Change string
}

// WorkingStatus observes what changed inside the clone, whoever changed it (spec §30.6). It exists
// because the changeset has TWO writers and only one of them keeps a ledger: the file tool records a
// row per write, while the shell tool writes with a heredoc, a `git apply`, a code generator or a
// compiler and appears in no write ledger at all. The ledger is therefore not the authority on what
// changed — the workspace is — and inside a clone git answers that exactly, cheaply and
// rename-aware.
//
// ignored counts the .gitignore'd files it found changed and did NOT return. Build output is noise in
// a changed set and is left out for the same reason `Commit`'s `git add -A` leaves it out, but the
// count comes back so a caller can say "1284 ignored outputs" instead of "nothing".
//
// It runs under the same untrusted-repo hardening as preparation, and GIT_OPTIONAL_LOCKS=0 keeps the
// read from writing the repo's index — the same promise WorkingDiff keeps with its scratch index.
//
// The `--porcelain=v2` choice is load-bearing rather than stylistic: in v1 a worktree-only change
// spends its FIRST column on the index status, so the record begins with a space, and this package's
// git helper returns strings.TrimSpace(stdout) — the leading space of the first record would be eaten
// and every field after it would be read one column short. Every v2 record starts with a non-space
// type char ('1', '2', 'u', '?', '!'), so the trim cannot reach into the data.
func WorkingStatus(ctx context.Context, repoDir string) (changes []WorkingChange, ignored int, err error) {
	out, err := gitInEnv(ctx, repoDir, []string{"GIT_OPTIONAL_LOCKS=0"},
		"status", "--porcelain=v2", "-z", "--untracked-files=all", "--ignored=traditional")
	if err != nil {
		return nil, 0, err
	}
	// -z makes every record NUL-terminated and disables git's path quoting, so a path containing a
	// space, a quote or a newline arrives verbatim instead of C-escaped.
	fields := strings.Split(out, "\x00")
	for i := 0; i < len(fields); i++ {
		record := fields[i]
		if record == "" {
			continue
		}
		switch record[0] {
		case '?': // "? <path>" — untracked, so new to the repository
			path, perr := statusPathAfter(record, "? ")
			if perr != nil {
				return nil, 0, perr
			}
			changes = append(changes, WorkingChange{Path: path, Change: changeAdded})
		case '!': // "! <path>" — ignored: counted, never listed
			ignored++
		case '1': // "1 <XY> <sub> <mH> <mI> <mW> <hH> <hI> <path>"
			path, xy, perr := statusFields(record, 9)
			if perr != nil {
				return nil, 0, perr
			}
			changes = append(changes, WorkingChange{Path: path, Change: changeFromXY(xy)})
		case 'u': // "u <XY> <sub> <m1> <m2> <m3> <mW> <h1> <h2> <h3> <path>" — a conflicted path
			// ELEVEN fields, not nine: an unmerged entry carries a mode and a hash for all THREE merge
			// stages. Splitting it like an ordinary record folds the last two hashes into the path.
			path, xy, perr := statusFields(record, 11)
			if perr != nil {
				return nil, 0, perr
			}
			changes = append(changes, WorkingChange{Path: path, Change: changeFromXY(xy)})
		case '2': // "2 <XY> <sub> <mH> <mI> <mW> <hH> <hI> <X><score> <path>" NUL "<origPath>"
			path, xy, perr := statusFields(record, 10) // one field more than '1': the rename score
			if perr != nil {
				return nil, 0, perr
			}
			if i+1 >= len(fields) {
				return nil, 0, fmt.Errorf("git status: rename record %q has no original path", record)
			}
			i++ // the original path is its own NUL-terminated field
			changes = append(changes, WorkingChange{Path: path, Change: changeAdded})
			// A rename empties the old path; a COPY leaves it in place, and calling that a deletion
			// would be a claim about a file this run never touched.
			if strings.HasPrefix(xy, "R") {
				changes = append(changes, WorkingChange{Path: fields[i], Change: changeDeleted})
			}
		}
	}
	return changes, ignored, nil
}

// The change kinds a changeset entry carries. They are the vocabulary the workspace panel already
// renders (workspace-panel.tsx:40); "deleted" is the one the file-tool ledger can never produce.
const (
	changeAdded    = "added"
	changeModified = "modified"
	changeDeleted  = "deleted"
)

// changeFromXY reduces a porcelain-v2 status pair (index, worktree; '.' means unmodified) to one
// change kind. A path staged AND re-edited carries two letters, so the test is membership rather than
// equality, and disappearance wins over the rest: a file staged as added and then removed from the
// tree is gone, whichever letter is read first.
func changeFromXY(xy string) string {
	switch {
	case strings.Contains(xy, "D"):
		return changeDeleted
	case strings.Contains(xy, "A"):
		return changeAdded
	default:
		return changeModified
	}
}

func statusPathAfter(record, prefix string) (string, error) {
	path, ok := strings.CutPrefix(record, prefix)
	if !ok || path == "" {
		return "", fmt.Errorf("git status: malformed record %q", record)
	}
	return path, nil
}

// statusFields splits a porcelain-v2 record into exactly `fields` parts and returns its trailing path
// and its XY status pair. The count is per record TYPE and is the whole reason this is a parameter:
// the path is whatever remains after the fixed fields, so a count that is one too small silently
// prepends a field to the path instead of failing. A record with fewer fields than its type declares
// is an error rather than a best effort — under-reporting the changed set is the defect this scan
// exists to remove.
func statusFields(record string, fields int) (path, xy string, err error) {
	parts := strings.SplitN(record, " ", fields)
	if len(parts) < fields || parts[fields-1] == "" {
		return "", "", fmt.Errorf("git status: malformed %q record %q", record[:1], record)
	}
	return parts[fields-1], parts[1], nil
}

// WorkingDiff returns the unified patch of repoDir's working tree — added, modified, and deleted
// files — against base, computed through a THROWAWAY index so the repo's own index and worktree are
// left untouched (a later commit or push sees no staged change). base is the model-independent
// preparation base commit (spec §30.3/§30.6). It runs under the same untrusted-repo hardening as
// preparation. The patch is bounded to maxBytes; truncated reports whether it was cut at the bound
// (spec §30.6 truncation marker). maxBytes <= 0 disables the bound.
func WorkingDiff(ctx context.Context, repoDir, base string, maxBytes int) (patch string, truncated bool, err error) {
	if err := rejectFlagShaped("base commit", base); err != nil {
		return "", false, err
	}
	scratch, err := os.MkdirTemp("", "palai-diff-")
	if err != nil {
		return "", false, fmt.Errorf("diff scratch: %w", err)
	}
	defer os.RemoveAll(scratch)
	// A non-existent index path is treated as an empty index; staging into it re-reads the worktree
	// without disturbing the repo's real index (which a later commit/push depends on).
	// ponytail: `add -A` into an empty scratch index re-hashes the worktree — bounded and fine for a
	// coding workspace; a base-seeded index (read-tree) is the upgrade path if a huge tree needs it.
	idxEnv := []string{"GIT_INDEX_FILE=" + filepath.Join(scratch, "index")}
	if _, err := gitInEnv(ctx, repoDir, idxEnv, "add", "-A"); err != nil {
		return "", false, err
	}
	// diff base..index: base holds the old versions, the staged index the new ones, so this shows every
	// real change including new files (invisible to a plain `git diff base`) and deletions. The "--"
	// ends options so a flag-shaped base (already rejected) could never be reparsed as one.
	out, err := gitInEnv(ctx, repoDir, idxEnv, "diff", "--cached", base, "--")
	if err != nil {
		return "", false, err
	}
	if maxBytes > 0 && len(out) > maxBytes {
		return out[:maxBytes], true, nil
	}
	return out, false, nil
}
