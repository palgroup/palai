// Package snapshot builds and restores the byte-archive of a workspace allocation (spec §29.10, E10
// Task 6). E09 recorded a create-side MANIFEST only (checksums, no bytes, adapters/.../workspace); this
// package is the byte half: Archive tars the allocation and Restore recreates it into a fresh
// allocation, verifying the create-side checksums are re-derived EQUAL (SAN-005). It reuses
// workspace.Snapshot for the include-set decision — the secrets/credential exclusions and the tree/index
// checksums are ONE source of truth, never re-derived here — and INCLUDES .git, because E09 Task 8's
// post-terminal detached publication pushes from the restored repo's own .git.
package snapshot

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
)

// Archive tars the allocation at root into w, capturing EXACTLY the files a create-side snapshot
// includes (workspace.Snapshot's FileChecksums keys) — so the secrets/ staging area and credential
// helpers are excluded by the same predicate the manifest uses, and no secret can enter the archive
// (SAN-005 exclusion). .git IS included (its objects/refs/index/config/HEAD are ordinary files under
// repo/.git, none excluded), so the restored repo can push (E09 Task 8). Entries are written in sorted
// path order, so the archive bytes are deterministic for the same tree. It returns the create-side
// manifest — the tree/index/file checksums a Restore must re-derive EQUAL.
//
// Ceiling: workspace.Snapshot checksums only REGULAR files, so a worktree symlink or an empty directory
// is silently absent from both the manifest and the archive (an E09 create-side decision, not new here).
// This is safe for the restore contract: a capture-order mutation of a captured file is CAUGHT at restore
// as a checksum mismatch, never silent corruption; only never-captured non-regular entries are omitted.
func Archive(root string, w io.Writer) (workspace.Manifest, error) {
	manifest, err := workspace.Snapshot(root)
	if err != nil {
		return workspace.Manifest{}, err
	}
	paths := make([]string, 0, len(manifest.FileChecksums))
	for p := range manifest.FileChecksums {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	tw := tar.NewWriter(w)
	for _, rel := range paths {
		if err := archiveFile(tw, root, rel); err != nil {
			return workspace.Manifest{}, err
		}
	}
	if err := tw.Close(); err != nil {
		return workspace.Manifest{}, fmt.Errorf("close snapshot tar: %w", err)
	}
	return manifest, nil
}

// archiveFile streams one included file into the tar with its content and permission bits. The mode
// does not affect the restore checksums (workspace.Snapshot checksums content only), but preserving the
// executable bit keeps git hooks and scripts runnable in the restored allocation.
func archiveFile(tw *tar.Writer, root, rel string) error {
	abs := filepath.Join(root, filepath.FromSlash(rel))
	info, err := os.Lstat(abs)
	if err != nil {
		return fmt.Errorf("stat snapshot file %s: %w", rel, err)
	}
	f, err := os.Open(abs)
	if err != nil {
		return fmt.Errorf("open snapshot file %s: %w", rel, err)
	}
	defer f.Close()
	if err := tw.WriteHeader(&tar.Header{
		Name:     rel, // slash-relative, matching the manifest key
		Mode:     int64(info.Mode().Perm()),
		Size:     info.Size(),
		Typeflag: tar.TypeReg,
	}); err != nil {
		return fmt.Errorf("write tar header %s: %w", rel, err)
	}
	if _, err := io.Copy(tw, f); err != nil {
		return fmt.Errorf("write tar body %s: %w", rel, err)
	}
	return nil
}
