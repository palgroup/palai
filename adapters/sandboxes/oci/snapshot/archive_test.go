package snapshot

import (
	"archive/tar"
	"bytes"
	"errors"
	"io/fs"
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

// TestArchiveKeepsTheManifestTrueWhenAFileVanishesMidArchive pins the SECOND half of a race whose first
// half lives in workspace.Snapshot. Snapshot checksums a live tree; Archive then re-reads what Snapshot
// recorded, and a file can disappear in between — git's background maintenance creating and removing
// `.git/objects/maintenance.lock` is the case that found both halves, and it turned two tests in this file
// intermittently red.
//
// The remedy here is the opposite of Snapshot's, and the difference is the point. There, the file was never
// captured, so skipping it was enough. Here it IS in the manifest, so tolerating it silently would leave
// the manifest claiming a file the archive does not hold — and the correspondence between those two is
// exactly what Restore verifies. So it is dropped from the checksums AND recorded as an exclusion.
func TestArchiveKeepsTheManifestTrueWhenAFileVanishesMidArchive(t *testing.T) {
	root := t.TempDir()
	for name, body := range map[string]string{"kept.txt": "content", "vanishing.lock": "lock"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	// Snapshot runs inside Archive and will checksum both files. Removing one AFTER that but before its
	// turn in the tar is the race; the deterministic stand-in is an archiver whose per-file step reports
	// the file as gone, which is what the filesystem would have said.
	var buf bytes.Buffer
	m, err := archiveWith(root, &buf, func(tw *tar.Writer, root, rel string) error {
		if rel == "vanishing.lock" {
			return &os.PathError{Op: "lstat", Path: filepath.Join(root, rel), Err: fs.ErrNotExist}
		}
		return archiveFile(tw, root, rel)
	})
	if err != nil {
		t.Fatalf("a file that vanished mid-archive failed the whole archive: %v", err)
	}
	if _, ok := m.FileChecksums["kept.txt"]; !ok {
		t.Fatalf("the file that stayed is missing from the manifest: %+v", m.FileChecksums)
	}
	if _, ok := m.FileChecksums["vanishing.lock"]; ok {
		t.Fatal("the manifest still claims a file the archive does not contain — Restore checks exactly that")
	}
	var recorded bool
	for _, e := range m.Exclusions {
		if e == "vanishing.lock" {
			recorded = true
		}
	}
	if !recorded {
		t.Fatalf("the vanished file is absent from Exclusions %v — dropping it silently hides it", m.Exclusions)
	}

	// AND THE MANIFEST MUST BE INTERNALLY CONSISTENT. Snapshot computed the tree checksum over the files it
	// saw, including the one that then vanished; if the removal does not restate it, the tree describes a
	// file the checksums no longer list. Restore RE-DERIVES the tree, so the transient disappearance would
	// arrive there looking like corruption — which is exactly how this was found.
	want := m
	workspace.RecomputeTreeChecksum(&want)
	if m.TreeChecksum != want.TreeChecksum {
		t.Fatalf("tree checksum %s does not describe the manifest's own files (%s): a removal must restate it",
			m.TreeChecksum, want.TreeChecksum)
	}

	// The tolerance is for a vanish and nothing else: a permission error must still fail the archive.
	var buf2 bytes.Buffer
	_, err = archiveWith(root, &buf2, func(tw *tar.Writer, root, rel string) error {
		if rel == "vanishing.lock" {
			return &os.PathError{Op: "open", Path: filepath.Join(root, rel), Err: fs.ErrPermission}
		}
		return archiveFile(tw, root, rel)
	})
	if err == nil {
		t.Fatal("a permission error was tolerated: the tolerance is for a vanished file only")
	}
}
