package stack

import (
	"archive/tar"
	"bytes"
	"strings"
	"testing"
)

// buildObjectStoreTar packs name->content into a tar the same shape the object-store copy
// produces, so objectChecksums and the archive round-trip exercise real tar bytes.
func buildObjectStoreTar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatalf("write header %s: %v", name, err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	return buf.Bytes()
}

func TestObjectChecksumsPerFile(t *testing.T) {
	objTar := buildObjectStoreTar(t, map[string]string{
		"volume.dat": "needle-bytes",
		"volume.idx": "index-bytes",
	})
	sums, err := objectChecksums(objTar)
	if err != nil {
		t.Fatalf("objectChecksums: %v", err)
	}
	if len(sums) != 2 {
		t.Fatalf("want 2 per-file checksums, got %d", len(sums))
	}
	byPath := map[string]objectChecksum{}
	for _, s := range sums {
		byPath[s.Path] = s
	}
	got := byPath["volume.dat"]
	if got.SHA256 != sha256Hex([]byte("needle-bytes")) {
		t.Fatalf("volume.dat checksum = %q, want hash of its content", got.SHA256)
	}
	if got.Size != int64(len("needle-bytes")) {
		t.Fatalf("volume.dat size = %d, want %d", got.Size, len("needle-bytes"))
	}
}

func TestBackupArchiveRoundTrip(t *testing.T) {
	dbDump := []byte("PGDMP-custom-format-bytes")
	objTar := buildObjectStoreTar(t, map[string]string{"volume.dat": "needle-bytes"})
	objSums, err := objectChecksums(objTar)
	if err != nil {
		t.Fatalf("objectChecksums: %v", err)
	}
	m := BackupManifest{
		Kind:              backupKind,
		Version:           backupManifestVersion,
		Project:           "palai-abcd",
		MigrationVersion:  32,
		OrganizationIDs:   []string{"org_a", "org_b"},
		ProjectIDs:        []string{"prj_a"},
		SampleResponseID:  "resp_xyz",
		DBDumpSHA256:      sha256Hex(dbDump),
		ObjectStoreSHA256: sha256Hex(objTar),
		Objects:           objSums,
	}

	var arc bytes.Buffer
	if err := writeBackupArchive(&arc, m, dbDump, objTar); err != nil {
		t.Fatalf("writeBackupArchive: %v", err)
	}

	gotM, gotDump, gotObj, err := readBackupArchive(bytes.NewReader(arc.Bytes()))
	if err != nil {
		t.Fatalf("readBackupArchive: %v", err)
	}
	if !bytes.Equal(gotDump, dbDump) {
		t.Fatalf("db.dump round-trip mismatch")
	}
	if !bytes.Equal(gotObj, objTar) {
		t.Fatalf("object-store.tar round-trip mismatch")
	}
	if gotM.MigrationVersion != 32 || gotM.SampleResponseID != "resp_xyz" {
		t.Fatalf("manifest round-trip mismatch: %+v", gotM)
	}
	if len(gotM.OrganizationIDs) != 2 {
		t.Fatalf("org ids round-trip mismatch: %v", gotM.OrganizationIDs)
	}
}

// A corrupted archive member must be rejected by the manifest checksum, not silently restored.
func TestBackupArchiveRejectsCorruptedMember(t *testing.T) {
	dbDump := []byte("PGDMP-custom-format-bytes")
	objTar := buildObjectStoreTar(t, map[string]string{"volume.dat": "x"})
	m := BackupManifest{
		Kind:              backupKind,
		Version:           backupManifestVersion,
		DBDumpSHA256:      sha256Hex([]byte("a-different-dump")), // wrong on purpose
		ObjectStoreSHA256: sha256Hex(objTar),
	}
	var arc bytes.Buffer
	if err := writeBackupArchive(&arc, m, dbDump, objTar); err != nil {
		t.Fatalf("writeBackupArchive: %v", err)
	}
	if _, _, _, err := readBackupArchive(bytes.NewReader(arc.Bytes())); err == nil {
		t.Fatalf("expected a checksum-mismatch error, got nil")
	} else if !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("expected a checksum error, got %v", err)
	}
}

// The fail-closed gate: restore refuses any target that already holds tenant rows, so a
// restore can never overwrite live data. Zero rows (a fresh migrated stack) is allowed.
func TestAssertEmptyTargetRefusesNonEmpty(t *testing.T) {
	if err := assertEmptyTarget(0); err != nil {
		t.Fatalf("empty target must be allowed, got %v", err)
	}
	err := assertEmptyTarget(1)
	if err == nil {
		t.Fatalf("non-empty target must be refused")
	}
	if !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("refusal must name the reason, got %v", err)
	}
}

// The manifest carries ids + checksums, never a secret. A defensive scan over the marshalled
// manifest keeps a future field from leaking a password/token into the backup archive.
func TestManifestCarriesNoSecretShaped(t *testing.T) {
	m := BackupManifest{
		Kind:             backupKind,
		Version:          backupManifestVersion,
		Project:          "palai-abcd",
		OrganizationIDs:  []string{"org_a"},
		SampleResponseID: "resp_xyz",
		DBDumpSHA256:     sha256Hex([]byte("d")),
	}
	raw, err := marshalManifest(m)
	if err != nil {
		t.Fatalf("marshalManifest: %v", err)
	}
	for _, banned := range []string{"password", "PGPASSWORD", "master-key", "master_key", "secret", "Bearer", "api-key", "apikey"} {
		if bytes.Contains(bytes.ToLower(raw), bytes.ToLower([]byte(banned))) {
			t.Fatalf("manifest leaked a secret-shaped token %q: %s", banned, raw)
		}
	}
}
