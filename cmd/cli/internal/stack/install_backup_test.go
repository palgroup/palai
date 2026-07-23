package stack

import (
	"archive/tar"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"strings"
	"testing"
)

// sealForTest AES-256-GCM-seals plaintext as nonce||ciphertext — the exact format the control-plane's
// secret store writes, so openSealed is exercised against a real-shape blob.
func sealForTest(t *testing.T, key, plaintext []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("gcm: %v", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("nonce: %v", err)
	}
	return gcm.Seal(nonce, nonce, plaintext, nil)
}

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

// The M1 empty-target gate is data-driven over EVERY org-bearing (FORCE-RLS) table, so provisioned
// data UNDER org_local (projects, api_keys, secret_refs, …) is counted — not just orgs/responses/runs.
// buildExcessQuery must exclude the 4 boot-seed rows by id, skip the runner-enrollment tables, and
// count everything else in full.
func TestBuildExcessQuery(t *testing.T) {
	q := buildExcessQuery([]string{"organizations", "projects", "principals", "api_keys", "runners", "runner_leases", "secret_refs", "responses"})
	mustContain := []string{
		"FROM organizations WHERE id <> 'org_local'",
		"FROM projects WHERE id <> 'prj_local'",
		"FROM principals WHERE id <> 'prin_local'",
		"FROM api_keys WHERE id <> 'key_local'",
		"FROM secret_refs", // provisioned-under-org_local data IS counted (the M1 hole)
		"FROM responses",
		"n > 0",
	}
	for _, s := range mustContain {
		if !strings.Contains(q, s) {
			t.Fatalf("query missing %q:\n%s", s, q)
		}
	}
	// runner-enrollment tables fill on a fresh boot — they must NOT be counted (else a fresh target
	// false-positives once the runner enrolls).
	for _, banned := range []string{"FROM runners", "FROM runner_leases"} {
		if strings.Contains(q, banned) {
			t.Fatalf("query must skip boot-infra table but has %q:\n%s", banned, q)
		}
	}
	if buildExcessQuery(nil) != "" {
		t.Fatalf("empty table list must yield an empty query")
	}
}

// openSealed is the M3 canary's AES-256-GCM decrypt, byte-compatible with the control-plane's
// secret seal (nonce||ciphertext). Right key decrypts; wrong key / short input fail closed.
func TestOpenSealedRoundTripAndWrongKey(t *testing.T) {
	key := bytes.Repeat([]byte{0x11}, 32)
	sealed := sealForTest(t, key, []byte("sk-live-secret"))

	got, err := openSealed(key, sealed)
	if err != nil || string(got) != "sk-live-secret" {
		t.Fatalf("right key must decrypt: got %q err %v", got, err)
	}
	wrong := bytes.Repeat([]byte{0x22}, 32)
	if _, err := openSealed(wrong, sealed); err == nil {
		t.Fatalf("wrong master key must fail closed")
	}
	if _, err := openSealed(key, []byte("x")); err == nil {
		t.Fatalf("ciphertext shorter than the nonce must error")
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
