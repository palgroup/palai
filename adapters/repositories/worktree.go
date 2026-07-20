package repositories

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Worktree is an isolated working tree a child agent edits in (spec §30.5, SUB-006). Isolated mode
// gives a writable checkout on its own branch; read-only mode gives a detached, write-denied
// snapshot. Either way the PARENT worktree is never mutated concurrently — a child's edits reach the
// parent only through an explicit Merge.
type Worktree struct {
	Path     string
	Branch   string // the child branch (isolated); empty for a detached read-only snapshot
	Base     string // the commit the worktree was created at
	Writable bool
}

// MergeResult is the typed outcome of an explicit conflict-aware merge (spec §30.5, REP-011). On a
// conflict the merge is aborted so the parent worktree stays consistent, and the conflicting paths
// are reported rather than silently overwritten.
type MergeResult struct {
	Merged        bool
	MergeCommit   string
	ConflictPaths []string
}

// ChildBranch is the generated work branch a mutable child agent uses (spec §30.5):
// agent/<session-short-id>/<run-short-id>. Policy controls prefix/protected/fork elsewhere.
func ChildBranch(sessionShort, runShort string) string {
	return "agent/" + sessionShort + "/" + runShort
}

// AddIsolatedWorktree creates a WRITABLE isolated worktree on a fresh child branch at base (spec
// §30.5, SUB-006). git worktree shares the object store (copy-on-write): the checkout is cheap and
// the child's commits land on its own branch, never the parent's. worktreePath must not exist yet.
func AddIsolatedWorktree(ctx context.Context, repoDir, worktreePath, sessionShort, runShort, base string) (Worktree, error) {
	branch := ChildBranch(sessionShort, runShort)
	if err := rejectGitPositionals(map[string]string{"branch": branch, "worktree path": worktreePath, "base": base}); err != nil {
		return Worktree{}, err
	}
	if _, err := gitIn(ctx, repoDir, "worktree", "add", "-b", branch, "--", worktreePath, base); err != nil {
		return Worktree{}, err
	}
	return Worktree{Path: worktreePath, Branch: branch, Base: base, Writable: true}, nil
}

// AddReadOnlyWorktree creates a detached, WRITE-DENIED snapshot at base (spec §30.5 read-only child).
// The worktree files are made read-only so a direct write is refused at the filesystem — the
// adapter-level equivalent of the read-only mount a real sandbox uses. .git metadata stays writable
// so the checkout itself succeeds.
// ponytail: FS perms are the read-only enforcement here; a container gets a ro bind mount and the
// file tool (T4) also honors Writable=false — this is the layer available without either wired yet.
func AddReadOnlyWorktree(ctx context.Context, repoDir, worktreePath, base string) (Worktree, error) {
	if err := rejectGitPositionals(map[string]string{"worktree path": worktreePath, "base": base}); err != nil {
		return Worktree{}, err
	}
	if _, err := gitIn(ctx, repoDir, "worktree", "add", "--detach", "--", worktreePath, base); err != nil {
		return Worktree{}, err
	}
	if err := makeTreeReadOnly(worktreePath); err != nil {
		return Worktree{}, err
	}
	return Worktree{Path: worktreePath, Base: base, Writable: false}, nil
}

// MergeBranch merges branch into repoDir's current worktree with explicit conflict handling (spec
// §30.5, REP-011). On a conflict it records the conflicting paths and ABORTS the merge, so the
// parent worktree is left exactly as it was — a conflict is reported, never silently resolved.
func MergeBranch(ctx context.Context, repoDir, branch string) (MergeResult, error) {
	if err := rejectFlagShaped("merge branch", branch); err != nil {
		return MergeResult{}, err
	}
	if _, err := gitIn(ctx, repoDir, "merge", "--no-edit", "--no-ff", "--", branch); err != nil {
		// The merge failed. Inspect and recover on a FRESH context: the caller's ctx may already be
		// canceled (timeout/interrupt), and recovery that ran on a dead ctx could not run at all —
		// leaving the parent worktree mid-merge while we falsely reported a clean, aborted conflict.
		recoverCtx := context.Background()
		conflicts, _ := gitIn(recoverCtx, repoDir, "diff", "--name-only", "--diff-filter=U")
		if strings.TrimSpace(conflicts) != "" {
			// A real conflict: abort to restore the parent. If the abort ITSELF fails the parent is not
			// consistent, so surface an error instead of a clean conflict a caller would record as OK.
			if _, abortErr := gitIn(recoverCtx, repoDir, "merge", "--abort"); abortErr != nil {
				return MergeResult{}, fmt.Errorf("merge %s conflicted and abort failed, parent worktree may be inconsistent: %w", branch, abortErr)
			}
			return MergeResult{Merged: false, ConflictPaths: strings.Split(strings.TrimSpace(conflicts), "\n")}, nil
		}
		// Non-conflict failure (e.g. canceled before touching the index): best-effort cleanup of any
		// partial merge, then report the original error. The parent is unchanged.
		_, _ = gitIn(recoverCtx, repoDir, "merge", "--abort")
		return MergeResult{}, fmt.Errorf("merge %s: %w", branch, err)
	}
	head, err := gitIn(ctx, repoDir, "rev-parse", "HEAD")
	if err != nil {
		return MergeResult{}, err
	}
	return MergeResult{Merged: true, MergeCommit: head}, nil
}

// gitIn runs one Git command in repoDir under the same untrusted-repo hardening as preparation
// (hooks disabled, ambient config stripped) using ephemeral empty hooks/home dirs, so a committed
// hook or ambient config cannot run during a worktree or merge operation.
func gitIn(ctx context.Context, repoDir string, args ...string) (string, error) {
	return gitInEnv(ctx, repoDir, nil, args...)
}

// gitInEnv is gitIn with extra environment (e.g. a throwaway GIT_INDEX_FILE) appended to the hardened
// env, so a working-tree diff can stage into a scratch index without touching the repo's own index.
func gitInEnv(ctx context.Context, repoDir string, extraEnv []string, args ...string) (string, error) {
	return gitInConfigEnv(ctx, repoDir, nil, extraEnv, args...)
}

// gitInConfigEnv is gitInEnv with extra `-c` config (e.g. a brokered credential.helper for a push or
// ls-remote) prepended to the hardened config. The publication path passes the credential-helper
// config here so the token reaches Git ONLY via the store helper file, never in argv (spec §30.2).
func gitInConfigEnv(ctx context.Context, repoDir string, extraConfig, extraEnv []string, args ...string) (string, error) {
	scratch, err := os.MkdirTemp("", "palai-git-op-")
	if err != nil {
		return "", fmt.Errorf("git op scratch: %w", err)
	}
	defer os.RemoveAll(scratch)
	hooksDir := filepath.Join(scratch, "hooks")
	homeDir := filepath.Join(scratch, "home")
	if err := os.MkdirAll(hooksDir, 0o700); err != nil {
		return "", err
	}
	if err := os.MkdirAll(homeDir, 0o700); err != nil {
		return "", err
	}
	return runGit(ctx, repoDir, homeDir, hooksDir, extraConfig, extraEnv, args...)
}

// makeTreeReadOnly strips write bits from every file and directory under root except the .git
// metadata (git needs it writable to track the worktree). Directories become 0555 so no new file can
// be created; files 0444 so none can be overwritten.
func makeTreeReadOnly(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if rel == ".git" || strings.HasPrefix(rel, ".git"+string(os.PathSeparator)) {
			return nil
		}
		if d.IsDir() {
			return os.Chmod(path, 0o555)
		}
		return os.Chmod(path, 0o444)
	})
}
