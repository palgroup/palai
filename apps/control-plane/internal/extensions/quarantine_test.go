package extensions

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"strings"
	"testing"
)

// tgzEntry is one archive member the test builder emits.
type tgzEntry struct {
	name     string
	typeflag byte
	linkname string
	body     []byte
	size     int64 // when >0 overrides len(body) in the header (a lie the reader must bound)
}

// buildTGZ packs entries into a REAL gzip-compressed tar — the exact wire shape an untrusted skill
// upload arrives as, so the quarantine pipeline is exercised against genuine archives, not mocks.
func buildTGZ(t *testing.T, entries ...tgzEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		tf := e.typeflag
		if tf == 0 {
			tf = tar.TypeReg
		}
		size := e.size
		if size == 0 {
			size = int64(len(e.body))
		}
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Typeflag: tf, Linkname: e.linkname, Mode: 0o644, Size: size}); err != nil {
			t.Fatalf("write header %q: %v", e.name, err)
		}
		if len(e.body) > 0 {
			if _, err := tw.Write(e.body); err != nil {
				t.Fatalf("write body %q: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

func TestSkillArchiveTraversalSymlinkBombRejected(t *testing.T) {
	cases := []struct {
		name    string
		archive []byte
	}{
		{"path traversal", buildTGZ(t, tgzEntry{name: "../escape.md", body: []byte("x")})},
		{"absolute path", buildTGZ(t, tgzEntry{name: "/etc/passwd", body: []byte("x")})},
		{"symlink escape", buildTGZ(t, tgzEntry{name: "link", typeflag: tar.TypeSymlink, linkname: "../../../../etc/passwd"})},
		{"symlink within", buildTGZ(t, tgzEntry{name: "link", typeflag: tar.TypeSymlink, linkname: "SKILL.md"})},
		{"char device", buildTGZ(t, tgzEntry{name: "dev", typeflag: tar.TypeChar})},
		{"block device", buildTGZ(t, tgzEntry{name: "dev", typeflag: tar.TypeBlock})},
		{"fifo", buildTGZ(t, tgzEntry{name: "pipe", typeflag: tar.TypeFifo})},
		{"hardlink", buildTGZ(t, tgzEntry{name: "hl", typeflag: tar.TypeLink, linkname: "SKILL.md"})},
		{"per-file size bomb", buildTGZ(t, tgzEntry{name: "big", body: bytes.Repeat([]byte{0}, maxSkillFileBytes+1)})},
		{"file-count bomb", buildTGZ(t, countBombEntries(maxSkillFiles+1)...)},
		{"total-size bomb", buildTGZ(t, totalBombEntries()...)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Quarantine(tc.archive)
			if err == nil {
				t.Fatalf("%s: Quarantine = nil error, want a hard reject", tc.name)
			}
			if !errors.Is(err, ErrUnsafeArchive) {
				t.Fatalf("%s: Quarantine err = %v, want ErrUnsafeArchive", tc.name, err)
			}
		})
	}
}

func countBombEntries(n int) []tgzEntry {
	out := make([]tgzEntry, n)
	for i := range out {
		out[i] = tgzEntry{name: "f" + itoa(i) + ".txt", body: []byte("x")}
	}
	return out
}

// totalBombEntries sums past the total-uncompressed cap with each file under the per-file cap: a
// classic decompression bomb (a tiny gz that inflates past the ceiling), caught mid-extract.
func totalBombEntries() []tgzEntry {
	perFile := maxSkillFileBytes
	n := (maxSkillArchiveBytes / perFile) + 2
	out := make([]tgzEntry, n)
	for i := range out {
		out[i] = tgzEntry{name: "z" + itoa(i) + ".dat", body: bytes.Repeat([]byte{0}, perFile)}
	}
	return out
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func TestQuarantineCleanArchiveDigestAndRepack(t *testing.T) {
	archive := buildTGZ(t,
		tgzEntry{name: "SKILL.md", body: []byte("---\nname: commit-convention\ndescription: helps write commit messages\n---\nUse conventional commits.\n")},
		tgzEntry{name: "reference/style.md", body: []byte("more text\n")},
	)
	res, err := Quarantine(archive)
	if err != nil {
		t.Fatalf("Quarantine clean archive: %v", err)
	}
	if !strings.HasPrefix(res.Digest, "sha256:") || len(res.Sanitized) == 0 {
		t.Fatalf("clean archive: digest=%q sanitized=%d bytes, want a sha256 digest over a non-empty repack", res.Digest, len(res.Sanitized))
	}
	if len(res.Findings) != 0 {
		t.Fatalf("clean archive: findings=%v, want none", res.Findings)
	}
	// The digest is content-addressed: re-quarantining the SAME archive yields the SAME digest.
	res2, err := Quarantine(archive)
	if err != nil {
		t.Fatalf("Quarantine re-run: %v", err)
	}
	if res2.Digest != res.Digest {
		t.Fatalf("digest not stable: %q vs %q", res.Digest, res2.Digest)
	}
}

func TestQuarantineScanFlagsSecretAndExecutable(t *testing.T) {
	secret := buildTGZ(t, tgzEntry{name: "SKILL.md", body: []byte("name: x\nkey: sk-ABCDEF0123456789\n")})
	res, err := Quarantine(secret)
	if err != nil {
		t.Fatalf("Quarantine secret archive: %v", err)
	}
	if !hasFinding(res.Findings, "secret") {
		t.Fatalf("secret in SKILL.md: findings=%v, want a secret finding", res.Findings)
	}

	elf := buildTGZ(t, tgzEntry{name: "bin/tool", body: []byte("\x7fELF\x02\x01\x01\x00rest")})
	res, err = Quarantine(elf)
	if err != nil {
		t.Fatalf("Quarantine elf archive: %v", err)
	}
	if !hasFinding(res.Findings, "executable") {
		t.Fatalf("ELF payload: findings=%v, want an executable finding", res.Findings)
	}

	shebang := buildTGZ(t, tgzEntry{name: "hook.sh", body: []byte("#!/bin/sh\nrm -rf /\n")})
	res, err = Quarantine(shebang)
	if err != nil {
		t.Fatalf("Quarantine shebang archive: %v", err)
	}
	if !hasFinding(res.Findings, "executable") {
		t.Fatalf("shebang script: findings=%v, want an executable finding", res.Findings)
	}
}

func hasFinding(findings []SkillFinding, kind string) bool {
	for _, f := range findings {
		if f.Kind == kind {
			return true
		}
	}
	return false
}
