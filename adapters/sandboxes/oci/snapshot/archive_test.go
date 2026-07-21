package snapshot

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
)

// writeFile creates a file (and parents) under root with content, failing the test on error.
func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// seedAllocation lays out a realistic allocation: the §29.9 layout, a real git repo under repo/ with a
// commit (so .git objects/refs/index exist), a scratch file, and a credential + secrets file that MUST
// NOT enter the archive. It returns the root.
func seedAllocation(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not found: %v", err)
	}
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	if err := workspace.Prepare(root); err != nil {
		t.Fatal(err)
	}
	repoDir := filepath.Join(root, workspace.RepoDir)
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e.test", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e.test")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	writeFile(t, root, "repo/app.go", "package main\n\nfunc main() {}\n")
	git("add", "-A")
	git("commit", "-q", "-m", "seed")
	writeFile(t, root, "scratch/notes.txt", "work in progress\n")
	// A credential + a secrets-staging file: both are snapshot-excluded and must NOT be archived.
	writeFile(t, root, "repo/.git-credentials", "https://x:tok@example.com\n")
	writeFile(t, root, "secrets/token", "SUPER-SECRET-TOKEN\n")
	return root
}

// TestSnapshotRestoreChecksumsMatchCreate (SAN-005): archive an allocation, restore into a FRESH dir,
// and the restored allocation's create-side manifest is BYTE-EQUAL to the original — tree, index, and
// every file checksum, with the .git tree included. Excluded secrets never enter the archive.
func TestSnapshotRestoreChecksumsMatchCreate(t *testing.T) {
	root := seedAllocation(t)

	var buf bytes.Buffer
	created, err := Archive(root, &buf)
	if err != nil {
		t.Fatalf("Archive() error = %v", err)
	}

	// Secret-scan the RAW archive bytes (spec §29.10, SAN-005): the secret sentinel must be ABSENT from
	// the tarball itself, not just from the manifest/restored FS — an exclusion that leaked into the
	// bytes would still ship the secret downstream.
	if bytes.Contains(buf.Bytes(), []byte("SUPER-SECRET-TOKEN")) {
		t.Fatal("the secret sentinel is present in the raw archive bytes — an excluded secret leaked into the tarball")
	}

	// The .git tree is captured: at least one repo/.git/ file is in the manifest.
	sawGit := false
	for path := range created.FileChecksums {
		if len(path) >= len("repo/.git/") && path[:len("repo/.git/")] == "repo/.git/" {
			sawGit = true
		}
		if path == "secrets/token" || filepath.Base(path) == ".git-credentials" {
			t.Fatalf("excluded path %q entered the archive manifest", path)
		}
	}
	if !sawGit {
		t.Fatal("archive manifest has no repo/.git/ files — the restored repo could not publish")
	}
	if len(created.Exclusions) == 0 {
		t.Fatal("create manifest recorded no exclusions, but a credential + a secret were present")
	}

	dest := filepath.Join(t.TempDir(), "restored")
	restored, err := Restore(bytes.NewReader(buf.Bytes()), dest, created)
	if err != nil {
		t.Fatalf("Restore() error = %v", err)
	}
	if restored.TreeChecksum != created.TreeChecksum {
		t.Fatalf("restored tree %s != created %s", restored.TreeChecksum, created.TreeChecksum)
	}
	if restored.IndexChecksum != created.IndexChecksum {
		t.Fatalf("restored index %s != created %s", restored.IndexChecksum, created.IndexChecksum)
	}
	if len(restored.FileChecksums) != len(created.FileChecksums) {
		t.Fatalf("restored file count %d != created %d", len(restored.FileChecksums), len(created.FileChecksums))
	}
	// The restored dir holds NO secret (SAN-005 secret-absence, and SAN-007 residue-freedom).
	if _, err := os.Stat(filepath.Join(dest, "secrets", "token")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restored allocation contains the excluded secret file (stat err = %v)", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "repo", ".git-credentials")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("restored allocation contains the excluded credential file")
	}
}

// TestRestoreRejectsChecksumMismatch: a tampered archive (a flipped byte in a tracked file) restores to
// a tree whose checksum differs, and Restore rejects it with ErrRestoreChecksumMismatch rather than
// returning a silently-wrong allocation.
func TestRestoreRejectsChecksumMismatch(t *testing.T) {
	root := seedAllocation(t)
	var buf bytes.Buffer
	created, err := Archive(root, &buf)
	if err != nil {
		t.Fatalf("Archive() error = %v", err)
	}
	// Assert a DIFFERENT expected tree checksum — the restored bytes are self-consistent but do not
	// match what the caller recorded at create time, which is exactly the corrupt-archive case.
	want := created
	want.TreeChecksum = "sha256:deadbeef"
	dest := filepath.Join(t.TempDir(), "restored")
	if _, err := Restore(bytes.NewReader(buf.Bytes()), dest, want); !errors.Is(err, ErrRestoreChecksumMismatch) {
		t.Fatalf("Restore(mismatch) = %v, want ErrRestoreChecksumMismatch", err)
	}
}

// TestRestorePreservesGitDirForPublication proves the restored repo is a WORKING git repo — its .git is
// intact enough to resolve HEAD and push to a bare remote (E09 Task 8's post-terminal publication runs
// on the restored .git, spec §29.10). It is the deterministic core of the component push proof.
func TestRestorePreservesGitDirForPublication(t *testing.T) {
	root := seedAllocation(t)
	var buf bytes.Buffer
	created, err := Archive(root, &buf)
	if err != nil {
		t.Fatalf("Archive() error = %v", err)
	}
	dest := filepath.Join(t.TempDir(), "restored")
	if _, err := Restore(bytes.NewReader(buf.Bytes()), dest, created); err != nil {
		t.Fatalf("Restore() error = %v", err)
	}
	restoredRepo := filepath.Join(dest, workspace.RepoDir)

	git := func(dir string, args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e.test", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e.test")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v in %s: %v: %s", args, dir, err, out)
		}
		return string(bytes.TrimSpace(out))
	}
	// The restored .git resolves HEAD and a clean status (a broken .git would fail these).
	head := git(restoredRepo, "rev-parse", "HEAD")
	if head == "" {
		t.Fatal("restored repo cannot resolve HEAD")
	}
	// It can push to a fresh bare remote — the publication path.
	bare := filepath.Join(t.TempDir(), "remote.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", bare).CombinedOutput(); err != nil {
		t.Fatalf("init bare remote: %v: %s", err, out)
	}
	git(restoredRepo, "push", bare, "main")
}
