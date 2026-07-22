package extensions

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/palgroup/palai/adapters/repositories"
)

// ErrUnsafeArchive marks a skill upload the quarantine pipeline HARD-rejects at extract: a path
// traversal, an absolute path, a symlink/hardlink, a special file (device/fifo), or a decompression
// bomb (per-file, total-size, or file-count cap). It is untrusted content, so a structurally unsafe
// archive is refused before a single byte is materialized — never sanitized-and-kept (TOL-011).
var ErrUnsafeArchive = errors.New("extensions: unsafe skill archive")

// Skill-archive extraction caps (spec §28.15, TOL-011). A skill is untrusted content, so extraction
// is bounded on every axis a decompression bomb can push: one file's bytes, the whole archive's bytes,
// and the file count. ponytail: fixed literals sized for a documentation-shaped skill (markdown +
// reference files); a larger legitimate skill raises these, but the bound is never removed.
const (
	maxSkillFileBytes    = 8 << 20  // one member's uncompressed bytes
	maxSkillArchiveBytes = 32 << 20 // the whole archive's uncompressed bytes (subsumes a compression-ratio cap)
	maxSkillFiles        = 2000     // member count
)

// SkillFinding is one static-scan hit over an archive member (spec §28.15). Kind is "secret" (a
// likely-committed credential shape) or "executable" (an ELF/shebang payload — a skill is prose, not a
// program). It carries the member path and the rule, never a captured value, so it is safe to persist,
// display, and put in an evidence manifest. A non-empty finding set keeps a revision quarantined:
// enable is blocked until the finding is resolved (a new clean revision), never silently accepted.
type SkillFinding struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
	Rule string `json:"rule"`
}

// QuarantineResult is the vetted output of one archive (spec §28.15): Sanitized is a re-packed PLAIN
// tar of only the safe regular files (bomb-capped, so bounded — no object store needed), Digest is the
// SHA-256 over that sanitized content (the run pin addresses THIS, never the raw upload), and Findings
// are the static-scan hits the enable gate consults.
type QuarantineResult struct {
	Sanitized []byte
	Digest    string
	Findings  []SkillFinding
}

// Quarantine unpacks an untrusted gzip-tar skill archive, HARD-rejecting every unsafe entry class and
// bomb, statically scanning each member, and re-packing the safe regular files into a sanitized tar
// with a content digest. The traversal/symlink guard reuses the snapshot restoreEntry idiom
// (adapters/sandboxes/oci/snapshot/restore.go); the secret scan reuses the changeset committed-secret
// scanner (adapters/repositories.ScanSecrets). It NEVER writes to disk — extraction is in-memory and
// bounded, so a malicious archive cannot escape here.
func Quarantine(gzipped []byte) (QuarantineResult, error) {
	gzr, err := gzip.NewReader(bytes.NewReader(gzipped))
	if err != nil {
		return QuarantineResult{}, fmt.Errorf("%w: not a gzip stream: %v", ErrUnsafeArchive, err)
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	var (
		out      bytes.Buffer
		tw       = tar.NewWriter(&out)
		total    int64
		files    int
		findings []SkillFinding
		seen     = map[string]bool{}
	)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// A truncated/oversized/corrupt stream is not a legitimate skill — reject rather than
			// materialize a partial tree.
			return QuarantineResult{}, fmt.Errorf("%w: read: %v", ErrUnsafeArchive, err)
		}
		if hdr.Typeflag == tar.TypeDir {
			continue // directories carry no content and are re-created implicitly on materialization
		}
		if hdr.Typeflag != tar.TypeReg {
			// Symlinks, hardlinks, devices, and fifos are never skill content; a symlink is an escape
			// primitive, a device/fifo a sandbox-escape shape. Refuse the whole archive.
			return QuarantineResult{}, fmt.Errorf("%w: entry %q is not a regular file (type %d)", ErrUnsafeArchive, hdr.Name, hdr.Typeflag)
		}
		rel, err := safeArchivePath(hdr.Name)
		if err != nil {
			return QuarantineResult{}, err
		}
		files++
		if files > maxSkillFiles {
			return QuarantineResult{}, fmt.Errorf("%w: more than %d files", ErrUnsafeArchive, maxSkillFiles)
		}
		// Read at most one-over the per-file cap: a member lying about its size in the header cannot
		// inflate memory, and an over-cap body is detected exactly.
		body, err := io.ReadAll(io.LimitReader(tr, maxSkillFileBytes+1))
		if err != nil {
			return QuarantineResult{}, fmt.Errorf("%w: read %q: %v", ErrUnsafeArchive, rel, err)
		}
		if len(body) > maxSkillFileBytes {
			return QuarantineResult{}, fmt.Errorf("%w: file %q exceeds %d bytes", ErrUnsafeArchive, rel, maxSkillFileBytes)
		}
		total += int64(len(body))
		if total > maxSkillArchiveBytes {
			return QuarantineResult{}, fmt.Errorf("%w: archive exceeds %d uncompressed bytes", ErrUnsafeArchive, maxSkillArchiveBytes)
		}
		findings = append(findings, scanSkillMember(rel, body, seen)...)
		if err := tw.WriteHeader(&tar.Header{Name: rel, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}); err != nil {
			return QuarantineResult{}, fmt.Errorf("repack %q: %w", rel, err)
		}
		if _, err := tw.Write(body); err != nil {
			return QuarantineResult{}, fmt.Errorf("repack %q: %w", rel, err)
		}
	}
	if err := tw.Close(); err != nil {
		return QuarantineResult{}, fmt.Errorf("close sanitized tar: %w", err)
	}
	sum := sha256.Sum256(out.Bytes())
	return QuarantineResult{Sanitized: out.Bytes(), Digest: "sha256:" + hex.EncodeToString(sum[:]), Findings: findings}, nil
}

// ExtractSanitizedArchive writes a QUARANTINE-sanitized tar (Quarantine's Sanitized output) into destDir
// (spec §28.16 workspace materialization). The tar is already vetted (only safe regular files), but the
// path guard is re-applied as defense-in-depth so a corrupted stored archive can never write outside
// destDir. Directories are created as needed; the caller (workspace materialization) confines destDir to
// the run's allocation, so the files land under <alloc>/.palai/skills/<name>/.
func ExtractSanitizedArchive(sanitizedTar []byte, destDir string) error {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("prepare skill dir: %w", err)
	}
	tr := tar.NewReader(bytes.NewReader(sanitizedTar))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read sanitized archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue // the sanitized tar holds only regular files; skip anything else defensively
		}
		rel, err := safeArchivePath(hdr.Name)
		if err != nil {
			return err
		}
		target := filepath.Join(destDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("prepare skill path %s: %w", rel, err)
		}
		body, err := io.ReadAll(io.LimitReader(tr, maxSkillFileBytes+1))
		if err != nil {
			return fmt.Errorf("read skill file %s: %w", rel, err)
		}
		if err := os.WriteFile(target, body, 0o644); err != nil {
			return fmt.Errorf("write skill file %s: %w", rel, err)
		}
	}
	return nil
}

// safeArchivePath rejects any escape in an UNTRUSTED skill entry name. Unlike the snapshot restoreEntry
// idiom (which CLAMPS a trusted control-plane archive into the root), an untrusted upload that names an
// absolute path or a `..` traversal is REFUSED outright, never silently clamped — the escape attempt is
// itself the signal. It returns the cleaned slash-relative safe path.
func safeArchivePath(name string) (string, error) {
	slashed := filepath.ToSlash(name)
	if slashed == "" || strings.HasPrefix(slashed, "/") || filepath.IsAbs(name) {
		return "", fmt.Errorf("%w: entry %q is an absolute path", ErrUnsafeArchive, name)
	}
	clean := filepath.ToSlash(filepath.Clean(slashed))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%w: entry %q escapes the skill root", ErrUnsafeArchive, name)
	}
	return clean, nil
}

// scanSkillMember flags likely-secret and executable content in one member. Secrets reuse the changeset
// committed-secret scanner (same shapes, detection not masking); executables are matched by magic (ELF)
// or a shebang — a skill is prose + reference text, so a compiled binary or a script is a finding, not
// skill content. Findings are deduped by (kind, path, rule) via seen.
func scanSkillMember(path string, body []byte, seen map[string]bool) []SkillFinding {
	var out []SkillFinding
	add := func(kind, rule string) {
		key := kind + "\x00" + path + "\x00" + rule
		if !seen[key] {
			seen[key] = true
			out = append(out, SkillFinding{Kind: kind, Path: path, Rule: rule})
		}
	}
	for _, hit := range repositories.ScanSecrets(string(body)) {
		add("secret", hit.Rule)
	}
	switch {
	case bytes.HasPrefix(body, []byte("\x7fELF")):
		add("executable", "elf")
	case bytes.HasPrefix(body, []byte("#!")):
		add("executable", "shebang")
	}
	// ponytail: ELF + shebang cover the executable shapes a documentation skill would never carry;
	// add Mach-O / PE magic here if a cross-platform binary smuggle ever needs flagging.
	return out
}
