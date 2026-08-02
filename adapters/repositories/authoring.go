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

// WorkingChange is one path the clone changed at, relative to the run's preparation base. Path is
// repo-relative and slash-separated (git's own form); Change is "added", "modified" or "deleted".
type WorkingChange struct {
	Path   string
	Change string
}

// WorkingChangeset is everything one staging pass over the clone can say about what a run did to it:
// the unified patch, the per-path change list, and how many .gitignore'd files were left out of both.
type WorkingChangeset struct {
	Patch     string
	Truncated bool
	Changes   []WorkingChange
	Ignored   int
}

// The change kinds a changeset entry carries. They are the vocabulary the workspace panel already
// renders (workspace-panel.tsx:40); "deleted" is the one the file-tool ledger can never produce.
const (
	changeAdded    = "added"
	changeModified = "modified"
	changeDeleted  = "deleted"
)

// DiffWorkingTree reports what the run did to the clone, whoever did it (spec §30.6, REP-005).
//
// It exists because the changeset has TWO writers and only one of them keeps a ledger: the file tool
// records a row per write, while the shell tool writes with a heredoc, a `git apply`, a code generator
// or a compiler and appears in no write ledger at all. The ledger is therefore not the authority on
// what changed — the workspace is.
//
// THE REFERENCE POINT IS THE PREPARATION BASE, NOT HEAD, and that is the whole design. `git status`
// answers "what is uncommitted", which is a different question and the wrong one: the commit tool
// moves HEAD, so after `palai.workspace.commit` a status-based scan reports NOTHING and the run that
// COMMITS — the only kind that can publish — compiles an empty changed set. MEASURED live on
// run_41e3c422acf6193c17b0be57ba2ee2c9: the approved publication's head contained the shell-written
// file and the changed set was empty. Diffing the base answers "what did this run do", which is what a
// changeset is, and it holds whether or not the run committed.
//
// Patch and Changes come from ONE staging pass into a THROWAWAY index, so they are two renderings of
// the same fact and cannot disagree, and the repo's own index and worktree are left untouched (a later
// commit or push sees no staged change). `git add -A` skips .gitignore'd paths, so build output is
// absent from both by construction — Ignored counts what was skipped, because "the run changed
// nothing" and "the run changed 1284 ignored build outputs" are different sentences.
//
// The patch is bounded to maxBytes; Truncated reports whether it was cut (maxBytes <= 0 disables the
// bound). The change LIST is deliberately unbounded: it is one line per path where the patch is one
// per changed line, and a truncated patch beside a complete list is the honest pair.
func DiffWorkingTree(ctx context.Context, repoDir, base string, maxBytes int) (WorkingChangeset, error) {
	if err := rejectFlagShaped("base commit", base); err != nil {
		return WorkingChangeset{}, err
	}
	scratch, err := os.MkdirTemp("", "palai-diff-")
	if err != nil {
		return WorkingChangeset{}, fmt.Errorf("diff scratch: %w", err)
	}
	defer os.RemoveAll(scratch)
	// A non-existent index path is treated as an empty index; staging into it re-reads the worktree
	// without disturbing the repo's real index (which a later commit/push depends on). The scratch dir
	// is OUTSIDE the repo, or `add -A` would stage the index file it is being written into.
	// ponytail: `add -A` into an empty scratch index re-hashes the worktree — bounded and fine for a
	// coding workspace; a base-seeded index (read-tree) is the upgrade path if a huge tree needs it.
	idxEnv := []string{"GIT_INDEX_FILE=" + filepath.Join(scratch, "index")}
	if _, err := gitInEnv(ctx, repoDir, idxEnv, "add", "-A"); err != nil {
		return WorkingChangeset{}, err
	}
	// diff base..index: base holds the old versions, the staged index the new ones, so this shows every
	// real change including new files (invisible to a plain `git diff base`) and deletions. The "--"
	// ends options so a flag-shaped base (already rejected) could never be reparsed as one.
	patch, err := gitInEnv(ctx, repoDir, idxEnv, "diff", "--cached", base, "--")
	if err != nil {
		return WorkingChangeset{}, err
	}
	names, err := gitInEnv(ctx, repoDir, idxEnv, "diff", "--cached", "--name-status", "-M", "-z", base, "--")
	if err != nil {
		return WorkingChangeset{}, err
	}
	changes, err := parseNameStatus(names)
	if err != nil {
		return WorkingChangeset{}, err
	}
	ignored, err := gitInEnv(ctx, repoDir, idxEnv, "ls-files", "--others", "--ignored", "--exclude-standard", "-z")
	if err != nil {
		return WorkingChangeset{}, err
	}
	out := WorkingChangeset{Patch: patch, Changes: changes, Ignored: countNulTerminated(ignored)}
	if maxBytes > 0 && len(out.Patch) > maxBytes {
		out.Patch, out.Truncated = out.Patch[:maxBytes], true
	}
	return out, nil
}

// parseNameStatus reads `git diff --name-status -M -z` records. -z makes every field NUL-terminated
// and disables git's path quoting, so a path containing a space, a quote or a newline arrives verbatim
// instead of C-escaped. A record is a status field followed by ONE path, except R/C which are followed
// by TWO (old then new) — and a parser that missed that would read the following record's status as a
// path.
func parseNameStatus(out string) ([]WorkingChange, error) {
	fields := strings.Split(out, "\x00")
	var changes []WorkingChange
	for i := 0; i < len(fields); i++ {
		status := fields[i]
		if status == "" {
			continue
		}
		// R/C carry a similarity score ("R100"); everything else is a bare letter.
		renamed := status[0] == 'R' || status[0] == 'C'
		want := 1
		if renamed {
			want = 2
		}
		if i+want >= len(fields) {
			return nil, fmt.Errorf("git diff --name-status: record %q has no path", status)
		}
		if renamed {
			from, to := fields[i+1], fields[i+2]
			i += 2
			changes = append(changes, WorkingChange{Path: to, Change: changeAdded})
			// A rename empties the old path; a COPY leaves it in place, and calling that a deletion
			// would be a claim about a file this run never touched.
			if status[0] == 'R' {
				changes = append(changes, WorkingChange{Path: from, Change: changeDeleted})
			}
			continue
		}
		path := fields[i+1]
		i++
		changes = append(changes, WorkingChange{Path: path, Change: changeFromStatus(status[0])})
	}
	return changes, nil
}

// changeFromStatus maps a diff status letter to a change kind. A type change (T, e.g. a file replaced
// by a symlink) is a modification of that path, which is what the patch shows too.
func changeFromStatus(status byte) string {
	switch status {
	case 'A':
		return changeAdded
	case 'D':
		return changeDeleted
	default:
		return changeModified
	}
}

// countNulTerminated counts the records in a NUL-terminated git list. It counts TERMINATORS rather
// than splitting, so a trailing empty segment is not miscounted as a path — and an empty output is
// zero rather than one.
func countNulTerminated(out string) int {
	return strings.Count(out, "\x00")
}
