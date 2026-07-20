package repositories

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
