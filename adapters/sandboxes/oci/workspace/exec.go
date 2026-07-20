package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ErrPathEscape is returned when a requested path resolves outside the workspace root — a traversal,
// an absolute path, or a symlink whose target escapes (spec §28.7, SAN-001). Its message never
// includes the host path, only the workspace-relative request.
var ErrPathEscape = errors.New("path escapes the workspace")

// ErrNotRegular is returned when a path resolves to a device, socket, or fifo rather than a regular
// file or directory (spec §28.7, SAN-001). A workspace tool operates on files, never on the kernel
// interfaces a hostile checkout might plant.
var ErrNotRegular = errors.New("path is not a regular file")

// WorkspaceFS confines every file operation to one allocation root (spec §28.7, SAN-001). Paths are
// resolved relative to the root; an absolute path, a `..` traversal, or a symlink whose target
// escapes the root is denied, as is a non-regular target. It owns containment only — read-only
// policy and likely-secret redaction are layered by the file tool above it.
type WorkspaceFS struct {
	root string // absolute, symlink-resolved
}

// NewWorkspaceFS binds a confinement to a real allocation root, resolving it through any symlinks so
// containment checks compare real paths. The root must exist.
func NewWorkspaceFS(root string) (*WorkspaceFS, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("workspace root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace root: %w", err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace root: %w", err)
	}
	return &WorkspaceFS{root: real}, nil
}

// WriteReport is the before/after summary of one write (spec §28.7): the workspace-relative path,
// the content hash before and after (before is empty for a new file), and whether the file was
// created. It is the changeset-event source the accrual layer consumes (T5).
type WriteReport struct {
	Path       string
	BeforeHash string
	AfterHash  string
	Created    bool
}

// Stat is the confined metadata of a path.
type Stat struct {
	Path  string
	IsDir bool
	Size  int64
}

// DirEntry is one entry of a confined directory listing.
type DirEntry struct {
	Name  string
	IsDir bool
	Size  int64
}

// resolve maps a workspace-relative request to an absolute host path, denying every escape: an
// absolute input, a `..` traversal past the root, or a symlink whose target leaves the root (spec
// §28.7). The returned path is safe to open; the target itself may not exist yet (a new write).
func (w *WorkspaceFS) resolve(rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: %q is absolute", ErrPathEscape, rel)
	}
	abs := filepath.Join(w.root, rel)
	within, err := filepath.Rel(w.root, abs)
	if err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q", ErrPathEscape, rel)
	}
	if err := w.assertNoSymlinkEscape(abs); err != nil {
		return "", err
	}
	return abs, nil
}

// assertNoSymlinkEscape resolves the deepest existing ancestor of abs through symlinks and verifies
// it still sits under the real root, so a symlink planted in the workspace cannot redirect a read or
// write outside it (spec §28.7, SAN-001). The non-existing tail of a new path cannot be a symlink.
func (w *WorkspaceFS) assertNoSymlinkEscape(abs string) error {
	p := abs
	for {
		real, err := filepath.EvalSymlinks(p)
		if err == nil {
			within, rerr := filepath.Rel(w.root, real)
			if rerr != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
				return fmt.Errorf("%w: symlink target escapes", ErrPathEscape)
			}
			return nil
		}
		if !os.IsNotExist(err) {
			return fmt.Errorf("resolve path: %w", err)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return fmt.Errorf("%w: no in-workspace ancestor", ErrPathEscape)
		}
		p = parent
	}
}

// assertRegularOrAbsent denies a path that exists as a device, socket, or fifo — a workspace tool
// touches regular files and directories only (spec §28.7). An absent path is fine (a new write).
func assertRegularOrAbsent(abs string) error {
	info, err := os.Lstat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat path: %w", err)
	}
	if info.IsDir() || info.Mode().IsRegular() {
		return nil
	}
	// A symlink here resolved to an in-root target (resolve already proved containment); a device,
	// socket, or fifo is refused.
	if info.Mode()&os.ModeSymlink != 0 {
		if resolved, err := os.Stat(abs); err == nil && (resolved.IsDir() || resolved.Mode().IsRegular()) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s", ErrNotRegular, filepath.Base(abs))
}

// Read returns the confined file's bytes, bounded to maxBytes, denying an escape or a non-regular
// target. A file larger than maxBytes is read up to the bound and reported truncated.
func (w *WorkspaceFS) Read(rel string, maxBytes int64) (data []byte, truncated bool, err error) {
	abs, err := w.resolve(rel)
	if err != nil {
		return nil, false, err
	}
	if err := assertRegularOrAbsent(abs); err != nil {
		return nil, false, err
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, false, fmt.Errorf("read %q: %w", rel, err)
	}
	defer f.Close()
	// Read one byte past the bound so an oversize file is detected and reported truncated.
	data, err = io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, false, fmt.Errorf("read %q: %w", rel, err)
	}
	if int64(len(data)) > maxBytes {
		return data[:maxBytes], true, nil
	}
	return data, false, nil
}

// Write atomically replaces the confined file with data (temp file + rename in the same directory),
// creating parent directories inside the root as needed, and reports the before/after content hash
// and whether it created the file (spec §28.7). A rename is atomic, so a concurrent reader never
// sees a partial write.
func (w *WorkspaceFS) Write(rel string, data []byte) (WriteReport, error) {
	abs, err := w.resolve(rel)
	if err != nil {
		return WriteReport{}, err
	}
	if err := assertRegularOrAbsent(abs); err != nil {
		return WriteReport{}, err
	}

	before := ""
	created := true
	if existing, err := os.ReadFile(abs); err == nil {
		before = hashBytes(existing)
		created = false
	} else if !os.IsNotExist(err) {
		return WriteReport{}, fmt.Errorf("read prior %q: %w", rel, err)
	}

	dir := filepath.Dir(abs)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return WriteReport{}, fmt.Errorf("prepare %q: %w", rel, err)
	}
	tmp, err := os.CreateTemp(dir, ".palai-write-*")
	if err != nil {
		return WriteReport{}, fmt.Errorf("stage %q: %w", rel, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return WriteReport{}, fmt.Errorf("stage %q: %w", rel, err)
	}
	if err := tmp.Close(); err != nil {
		return WriteReport{}, fmt.Errorf("stage %q: %w", rel, err)
	}
	if err := os.Rename(tmpName, abs); err != nil {
		return WriteReport{}, fmt.Errorf("commit %q: %w", rel, err)
	}
	return WriteReport{Path: filepath.ToSlash(rel), BeforeHash: before, AfterHash: hashBytes(data), Created: created}, nil
}

// Stat returns the confined metadata of a path.
func (w *WorkspaceFS) Stat(rel string) (Stat, error) {
	abs, err := w.resolve(rel)
	if err != nil {
		return Stat{}, err
	}
	if err := assertRegularOrAbsent(abs); err != nil {
		return Stat{}, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return Stat{}, fmt.Errorf("stat %q: %w", rel, err)
	}
	return Stat{Path: filepath.ToSlash(rel), IsDir: info.IsDir(), Size: info.Size()}, nil
}

// List returns the confined directory listing at rel, sorted by name.
func (w *WorkspaceFS) List(rel string) ([]DirEntry, error) {
	abs, err := w.resolve(rel)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, fmt.Errorf("list %q: %w", rel, err)
	}
	out := make([]DirEntry, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, DirEntry{Name: e.Name(), IsDir: e.IsDir(), Size: info.Size()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Checksum returns the confined file's content hash.
func (w *WorkspaceFS) Checksum(rel string) (string, error) {
	abs, err := w.resolve(rel)
	if err != nil {
		return "", err
	}
	if err := assertRegularOrAbsent(abs); err != nil {
		return "", err
	}
	return checksumFile(abs)
}

// hashBytes is the digest-prefixed sha256 of in-memory content — the write-path counterpart of
// checksumFile, which hashes a file already on disk.
func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
