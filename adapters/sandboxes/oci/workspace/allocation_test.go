package workspace

import (
	"os"
	"path/filepath"
	"slices"
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
