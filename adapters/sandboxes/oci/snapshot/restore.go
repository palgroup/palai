package snapshot

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
)

// ErrRestoreChecksumMismatch reports that a restored allocation's re-derived manifest does not match
// the one the archive was created with (spec §29.10, SAN-005): the tree, index, or a file checksum
// differs, so the archive is corrupt or tampered and the restore is rejected rather than resumed as
// garbage. A recovering workspace facing this fails EXPLICITLY (recovering→failed), never silently.
var ErrRestoreChecksumMismatch = errors.New("snapshot restore checksum mismatch")

// Restore untars an archive produced by Archive into dest and re-derives the create-side manifest,
// requiring it to EQUAL want (SAN-005) — the tree, index, and per-file checksums, .git tree included.
// dest must be a FRESH allocation directory: the restore writes only the archived files, so a residue
// from a prior tenant would surface as an extra file and a tree-checksum mismatch (SAN-007 leans on
// this). A mismatch returns ErrRestoreChecksumMismatch; the caller fails the recovery explicitly.
func Restore(r io.Reader, dest string, want workspace.Manifest) (workspace.Manifest, error) {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return workspace.Manifest{}, fmt.Errorf("prepare restore dir: %w", err)
	}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return workspace.Manifest{}, fmt.Errorf("read snapshot tar: %w", err)
		}
		if err := restoreEntry(tr, dest, hdr); err != nil {
			return workspace.Manifest{}, err
		}
	}
	got, err := workspace.Snapshot(dest)
	if err != nil {
		return workspace.Manifest{}, err
	}
	if want.TreeChecksum != "" && !manifestChecksumsEqual(want, got) {
		return got, fmt.Errorf("%w: tree %s!=%s index %s!=%s", ErrRestoreChecksumMismatch,
			want.TreeChecksum, got.TreeChecksum, want.IndexChecksum, got.IndexChecksum)
	}
	return got, nil
}

// restoreEntry writes one tar entry under dest, guarding against a path that escapes dest (zip-slip):
// the archive is control-plane-produced, but a corrupt/truncated one must never write outside the
// target allocation. Only regular files are archived, so a non-regular entry is a malformed archive.
func restoreEntry(tr *tar.Reader, dest string, hdr *tar.Header) error {
	clean := filepath.Clean("/" + filepath.FromSlash(hdr.Name)) // anchor, then strip the leading separator
	rel := strings.TrimPrefix(clean, string(os.PathSeparator))
	if rel == "" || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || rel == ".." {
		return fmt.Errorf("snapshot archive entry %q escapes the restore dir", hdr.Name)
	}
	target := filepath.Join(dest, rel)
	if hdr.Typeflag != tar.TypeReg {
		return fmt.Errorf("snapshot archive entry %q is not a regular file (type %d)", hdr.Name, hdr.Typeflag)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("prepare restore path %s: %w", rel, err)
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
	if err != nil {
		return fmt.Errorf("create restored file %s: %w", rel, err)
	}
	defer f.Close()
	if _, err := io.Copy(f, tr); err != nil {
		return fmt.Errorf("write restored file %s: %w", rel, err)
	}
	return nil
}

// manifestChecksumsEqual reports whether two manifests carry the same content-addressed integrity: the
// tree and index checksums and the full per-file checksum map. Exclusions are NOT compared — a restored
// allocation has no secrets to exclude, so its exclusion list is empty by construction (SAN-005), while
// the create-side list named what it dropped; the integrity that must match is the included tree.
func manifestChecksumsEqual(want, got workspace.Manifest) bool {
	if want.TreeChecksum != got.TreeChecksum || want.IndexChecksum != got.IndexChecksum {
		return false
	}
	if len(want.FileChecksums) != len(got.FileChecksums) {
		return false
	}
	for path, sum := range want.FileChecksums {
		if got.FileChecksums[path] != sum {
			return false
		}
	}
	return true
}
