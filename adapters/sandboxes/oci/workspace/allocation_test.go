package workspace

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writeFile creates parent dirs and writes content, failing the test on error.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestSnapshotSkipsThePerSessionDirectoryAsASubtree covers the consequence E22 T2 created for this
// package. The native posture gives every run its own HOME, TMPDIR and CoreSimulator device set
// under <allocation>/.palai-session — and Snapshot walks the ALLOCATION, so without this rule a
// snapshot would os.ReadFile a booted simulator's device bundle into the control plane's memory and
// checksum it as if it were the run's repository.
//
// It is skipped as a SUBTREE with ONE exclusion entry, not enumerated: listing each cache file as an
// exclusion would cost the walk it is meant to avoid.
func TestSnapshotSkipsThePerSessionDirectoryAsASubtree(t *testing.T) {
	root := t.TempDir()
	if err := Prepare(root); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	writeFile(t, filepath.Join(root, RepoDir, "app.go"), "package main\n")
	writeFile(t, filepath.Join(root, SessionDir, "home", "Library", "Caches", "big"), "machine-local runtime state")
	writeFile(t, filepath.Join(root, SessionDir, "simulators", "device", "data"), "a simulator device bundle")

	manifest, err := Snapshot(root)
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if _, ok := manifest.FileChecksums["repo/app.go"]; !ok {
		t.Fatalf("the workspace's own file went missing: %v", manifest.FileChecksums)
	}
	for path := range manifest.FileChecksums {
		if strings.HasPrefix(path, SessionDir+"/") {
			t.Fatalf("per-session runtime state %q was checksummed into the snapshot", path)
		}
	}
	if !slices.Contains(manifest.Exclusions, SessionDir) {
		t.Fatalf("the skipped subtree is not recorded in the exclusion manifest %v — a snapshot must say what it left out", manifest.Exclusions)
	}
	for _, e := range manifest.Exclusions {
		if strings.HasPrefix(e, SessionDir+"/") {
			t.Fatalf("the session subtree was ENUMERATED (%q) rather than skipped; the walk paid the cost the rule exists to avoid", e)
		}
	}
}

// TestSnapshotCreateChecksumsAndExclusions proves the SAN-005 create half (spec §29.10): a
// snapshot checksums the real worktree deterministically, its index checksum reflects the staged
// state, and secret/credential paths are excluded — recorded in the exclusion manifest and never
// contributing a checksum, so no secret enters the snapshot. RESTORE is E10.
func TestSnapshotCreateChecksumsAndExclusions(t *testing.T) {
	root := t.TempDir()
	if err := Prepare(root); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	for _, d := range []string{RepoDir, ScratchDir, ArtifactsDir} {
		if info, err := os.Stat(filepath.Join(root, d)); err != nil || !info.IsDir() {
			t.Fatalf("Prepare did not create %s: err=%v", d, err)
		}
	}

	writeFile(t, filepath.Join(root, RepoDir, "app.go"), "package main\n")
	writeFile(t, filepath.Join(root, RepoDir, ".git", "index"), "staged-index-bytes")
	// Secret and credential paths that must be excluded from any snapshot.
	writeFile(t, filepath.Join(root, secretsDir, "token"), "sk-live-MUSTNOTSNAPSHOT")
	writeFile(t, filepath.Join(root, RepoDir, ".git-credentials"), "https://x:y@github.com")

	manifest, err := Snapshot(root)
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}

	// The ordinary file is checksummed; the secret and credential files are not.
	if _, ok := manifest.FileChecksums["repo/app.go"]; !ok {
		t.Fatalf("repo/app.go missing from snapshot file checksums: %v", manifest.FileChecksums)
	}
	for _, secret := range []string{"secrets/token", "repo/.git-credentials"} {
		if _, ok := manifest.FileChecksums[secret]; ok {
			t.Fatalf("excluded path %q leaked into snapshot checksums", secret)
		}
		if !slices.Contains(manifest.Exclusions, secret) {
			t.Fatalf("excluded path %q missing from exclusion manifest %v", secret, manifest.Exclusions)
		}
	}

	// The manifest is content-addressed and complete: non-empty tree + index checksums.
	if manifest.TreeChecksum == "" || manifest.IndexChecksum == "" {
		t.Fatalf("empty checksum: tree=%q index=%q", manifest.TreeChecksum, manifest.IndexChecksum)
	}

	// Determinism: re-snapshotting the same tree yields the identical tree checksum.
	again, err := Snapshot(root)
	if err != nil {
		t.Fatalf("re-Snapshot() error = %v", err)
	}
	if again.TreeChecksum != manifest.TreeChecksum {
		t.Fatalf("tree checksum is not deterministic: %q then %q", manifest.TreeChecksum, again.TreeChecksum)
	}

	// Integrity: mutating a tracked file changes the tree checksum.
	writeFile(t, filepath.Join(root, RepoDir, "app.go"), "package main // edited\n")
	mutated, err := Snapshot(root)
	if err != nil {
		t.Fatalf("mutated Snapshot() error = %v", err)
	}
	if mutated.TreeChecksum == manifest.TreeChecksum {
		t.Fatalf("tree checksum unchanged after a file edit: %q", mutated.TreeChecksum)
	}
}

// TestSnapshotSurvivesAFileThatVanishesMidWalk pins the behaviour a LIVE tree forces on us. The case that
// found it was git's own background maintenance: it creates `.git/objects/maintenance.lock`, WalkDir sees
// the entry, maintenance finishes and removes it, and the checksum then opens nothing. Before the fix the
// whole snapshot failed, so whether a snapshot succeeded depended on what git happened to be doing.
//
// IT DRIVES THE CHECKSUMMER, not the filesystem, and that is deliberate. The first version of this test
// deleted the file before calling Snapshot — which means the walk never saw it, the branch never ran, and
// the test passed with the fix disabled. Mutating `fs.ErrNotExist` to `fs.ErrPermission` proved it: still
// green. A guard that cannot fail is worse than none, so the checksummer is injected and the vanish is
// made to happen exactly where the race puts it.
func TestSnapshotSurvivesAFileThatVanishesMidWalk(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "kept.txt"), []byte("content"), 0o644); err != nil {
		t.Fatalf("write kept: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "maintenance.lock"), []byte("lock"), 0o644); err != nil {
		t.Fatalf("write lock: %v", err)
	}

	// The checksummer answers as the filesystem would for a file removed between the walk and the read.
	vanishing := func(path string) (string, error) {
		if filepath.Base(path) == "maintenance.lock" {
			return "", &os.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
		}
		return checksumFile(path)
	}

	m, err := snapshotWith(root, vanishing)
	if err != nil {
		t.Fatalf("a file that vanished between the walk and the read failed the whole snapshot: %v", err)
	}
	if _, ok := m.FileChecksums["kept.txt"]; !ok {
		t.Fatalf("the file that stayed is missing: %+v", m.FileChecksums)
	}
	if _, ok := m.FileChecksums["maintenance.lock"]; ok {
		t.Fatal("the vanished file was given a checksum it could not have")
	}

	// RECORDED, not dropped: present at walk and absent at read is exactly what an operator reading a
	// manifest needs to see, and silence would be indistinguishable from a file nobody looked at.
	var recorded bool
	for _, e := range m.Exclusions {
		if e == "maintenance.lock" {
			recorded = true
		}
	}
	if !recorded {
		t.Fatalf("the vanished file is absent from Exclusions %v — dropping it silently hides it", m.Exclusions)
	}

	// A checksum failure that is NOT a vanish must still fail the snapshot: this branch tolerates one
	// specific accident, not every read error.
	denied := func(path string) (string, error) {
		if filepath.Base(path) == "maintenance.lock" {
			return "", &os.PathError{Op: "open", Path: path, Err: fs.ErrPermission}
		}
		return checksumFile(path)
	}
	if _, err := snapshotWith(root, denied); err == nil {
		t.Fatal("a permission error was tolerated: the tolerance must be for a vanished file only")
	}
}
