// Package workspace lays out a physical workspace allocation on the runner host and computes a
// create-side snapshot of it. A logical Workspace (spec §29.7) is a host-independent filesystem
// lineage; a physical allocation is one host directory realising it, bind-mounted to /workspace
// in the sandbox (spec §29.9). The logical→physical mapping and the monotonic fencing token that
// distinguishes allocations live in the durable store; the pure fence guard is
// packages/state-machines.AcceptFence. This package owns only the on-host filesystem: the
// documented directory layout and the immutable, content-addressed snapshot manifest (§29.10).
package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The documented logical subdirectories under /workspace (spec §29.9). They are created on the
// host allocation directory, so once it is bind-mounted at /workspace the engine sees
// /workspace/repo, /workspace/scratch, and /workspace/artifacts. /runtime and /secrets are
// separate mounts the supervisor owns, never part of a workspace allocation.
const (
	RepoDir      = "repo"
	ScratchDir   = "scratch"
	ArtifactsDir = "artifacts"
)

// secretsDir is a staging subdirectory a snapshot always excludes; combined with the credential
// basenames below it is the create-side exclusion set (spec §29.10). /secrets proper is a sibling
// mount, never inside the allocation, so it cannot enter a snapshot at all.
const secretsDir = "secrets"

// credentialBasenames are files a snapshot never captures even if they surface inside the
// worktree: Git credential stores, netrc, and package-registry tokens (spec §29.10 excludes
// credential helpers and registry tokens).
var credentialBasenames = map[string]bool{
	".git-credentials": true,
	".netrc":           true,
	".npmrc":           true,
}

// Prepare creates the documented workspace layout under root (spec §29.9). It is idempotent.
// ponytail: dir mode is 0o755 here; giving the ephemeral allocation to the sandbox uid is a
// runner-host provisioning concern (ownership/quota) outside this filesystem-layout seam.
func Prepare(root string) error {
	for _, d := range []string{RepoDir, ScratchDir, ArtifactsDir} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return fmt.Errorf("prepare workspace %s: %w", d, err)
		}
	}
	return nil
}

// Manifest is the create-side snapshot of an allocation (spec §29.10): content-addressed tree and
// index checksums, the per-file checksum map required to reconstruct it, and the excluded-path
// manifest. It is immutable and deterministic — the same tree always yields the same TreeChecksum.
// RESTORE (recreating the exclusions as empty, verifying referenced objects) is E10.
type Manifest struct {
	TreeChecksum  string            `json:"tree_checksum"`
	IndexChecksum string            `json:"index_checksum"`
	FileChecksums map[string]string `json:"file_checksums"`
	Exclusions    []string          `json:"exclusions"`
}

// Snapshot walks the allocation at root and computes its create-side manifest. Regular files are
// checksummed and keyed by their slash-relative path; secret and credential paths are recorded in
// Exclusions and never contribute a checksum, so no secret enters the snapshot (spec §29.10,
// SAN-005). Non-regular entries (symlinks, devices, sockets) are not snapshot content and are
// skipped. RESTORE is E10.
func Snapshot(root string) (Manifest, error) {
	files := map[string]string{}
	var exclusions []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if isExcluded(rel) {
			exclusions = append(exclusions, rel)
			return nil
		}
		sum, err := checksumFile(path)
		if err != nil {
			return err
		}
		files[rel] = sum
		return nil
	})
	if err != nil {
		return Manifest{}, fmt.Errorf("snapshot workspace: %w", err)
	}
	sort.Strings(exclusions)
	return Manifest{
		TreeChecksum:  treeChecksum(files),
		IndexChecksum: indexChecksum(root),
		FileChecksums: files,
		Exclusions:    exclusions,
	}, nil
}

// isExcluded reports whether a slash-relative path is a secret or credential path a snapshot must
// never capture (spec §29.10).
func isExcluded(rel string) bool {
	segments := strings.Split(rel, "/")
	if segments[0] == secretsDir {
		return true
	}
	return credentialBasenames[segments[len(segments)-1]]
}

// checksumFile returns the sha256 of a file's contents, digest-prefixed.
func checksumFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("checksum %s: %w", path, err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// treeChecksum is the content address of the included file set: sha256 over the files sorted by
// path, each rendered as "path\x00checksum\n". Deterministic and order-independent of the walk.
func treeChecksum(files map[string]string) string {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	h := sha256.New()
	for _, p := range paths {
		fmt.Fprintf(h, "%s\x00%s\n", p, files[p])
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// indexChecksum checksums the repository index (repo/.git/index) when present, so a snapshot
// records the exact staged state (spec §29.10). An allocation with no checked-out repo yields "".
func indexChecksum(root string) string {
	sum, err := checksumFile(filepath.Join(root, RepoDir, ".git", "index"))
	if err != nil {
		return ""
	}
	return sum
}
