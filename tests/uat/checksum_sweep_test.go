package uat

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// releasesDir is the committed evidence corpus, relative to this package.
func releasesDir() string { return filepath.Join("..", "..", "evidence", "releases") }

// readBundle reads one committed bundle's manifest bytes.
func readBundle(t *testing.T, release string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(releasesDir(), release, "manifest.json"))
	if err != nil {
		t.Fatalf("read %s manifest: %v", release, err)
	}
	return raw
}

// TestCommittedBundleChecksumSweep is the E18 T8 sweep (plan §T8): EVERY committed bundle goes through the
// strengthened verifier, and every case is either recompute-verified or explicitly labelled legacy
// shape-only. This is the gate that catches a fabricated checksum in history — a value that is sha256:<64 hex>
// but reproduces nothing (the automation-0.1.0 AUT-001 defect, deferred since E11).
//
// The sweep CONSUMES committedBundleSurfaces (evidence.go); it does not own it. That matters: the table is the
// same one VerifyManifest keys off, so the sweep's directory-name check and the verifier's release-name check
// can never disagree — which is what let a renamed or shrunk manifest slip past VerifyManifest while the sweep
// stayed green.
func TestCommittedBundleChecksumSweep(t *testing.T) {
	entries, err := os.ReadDir(releasesDir())
	if err != nil {
		t.Fatalf("read evidence/releases: %v", err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		release := e.Name()
		want, declared := committedBundleSurfaces[release]
		if !declared {
			t.Errorf("%s is committed but declares no checksum surface — add it to committedBundleSurfaces "+
				"(a new bundle must recompute, and it cannot self-declare legacy)", release)
			continue
		}
		seen[release] = true

		summary, err := VerifyRelease(filepath.Join(releasesDir(), release), nil)
		if err != nil {
			t.Errorf("verify %s: %v", release, err)
			continue
		}
		if !summary.OK() {
			t.Errorf("%s did not verify clean: %s\n%v", release, summary.String(), summary.Findings)
		}

		var m evidenceManifest
		if err := json.Unmarshal(readBundle(t, release), &m); err != nil {
			t.Errorf("decode %s manifest: %v", release, err)
			continue
		}
		// The manifest's declared release must BE the directory it ships in, or the table's decision is
		// keyed to one name and the verifier's to another.
		if m.Release != release {
			t.Errorf("%s ships a manifest declaring release %q — the surface table keys off the release name", release, m.Release)
		}
		// A corrected or shape-only bundle owes its note in the manifest, not only in this tree's comments.
		if checksumNoteRequired(want) && strings.TrimSpace(m.ChecksumNote) == "" {
			t.Errorf("%s is declared %q but carries no checksum_note", release, want)
		}
		recomputed, labelled := 0, 0
		for _, c := range m.Cases {
			parts := caseChecksumParts(m, c)
			switch want {
			case LegacyShapeOnly:
				if parts != nil {
					t.Errorf("%s/%s is labelled legacy but a canonical surface resolves — recompute it instead", release, c.ID)
				}
				if c.ChecksumSurface != LegacyShapeOnly {
					t.Errorf("%s/%s is a shape-only case with checksum_surface=%q, want %q",
						release, c.ID, c.ChecksumSurface, LegacyShapeOnly)
				}
				labelled++
			default: // SurfaceRecomputed, SurfaceCorrected
				if parts == nil {
					t.Errorf("%s/%s: the sweep table says %s but no canonical surface resolves", release, c.ID, want)
					continue
				}
				if c.ChecksumSurface != "" {
					t.Errorf("%s/%s carries checksum_surface=%q although its surface IS committed — recompute, never label",
						release, c.ID, c.ChecksumSurface)
				}
				if got, expect := c.Checksum, hashParts(parts...); got != expect {
					t.Errorf("%s/%s checksum %s does not recompute from %v (want %s)", release, c.ID, got, parts, expect)
				}
				recomputed++
			}
		}
		t.Logf("SWEEP %-32s %2d cases: %d recomputed, %d legacy shape-only", release, len(m.Cases), recomputed, labelled)
	}
	for release := range committedBundleSurfaces {
		if !seen[release] {
			t.Errorf("%s is in the sweep table but is not committed under evidence/releases", release)
		}
	}
}

// TestSweepCatchesFabricatedChecksum is RED-first negative 1 (plan §T8): a bundle whose checksum is
// shape-valid but reproduces NOTHING fails the sweep. It fabricates over the real automation-0.1.0 bundle, so
// the test also proves the corrected AUT-001 value is load-bearing — flip it and the bundle fails.
func TestSweepCatchesFabricatedChecksum(t *testing.T) {
	raw := readBundle(t, "automation-0.1.0")
	if findings := VerifyManifest(raw, nil); len(findings) != 0 {
		t.Fatalf("automation-0.1.0 must verify clean before it is fabricated: %v", findings)
	}
	var m evidenceManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode automation-0.1.0: %v", err)
	}
	real := m.Cases[0].Checksum
	fabricated := "sha256:" + strings.Repeat("f", 64)
	tampered := bytes.Replace(raw, []byte(real), []byte(fabricated), 1)
	if bytes.Equal(tampered, raw) {
		t.Fatalf("fabrication did not apply (checksum %q not found)", real)
	}
	findings := VerifyManifest(tampered, nil)
	if !hasDetail(findings, "does not recompute") {
		t.Fatalf("a fabricated checksum was not caught: %v", findings)
	}
	// The sweep's operator surface fails too: the bundle is no longer clean.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), tampered, 0o644); err != nil {
		t.Fatalf("write fabricated bundle: %v", err)
	}
	summary, err := VerifyRelease(dir, nil)
	if err != nil {
		t.Fatalf("verify fabricated bundle: %v", err)
	}
	if summary.OK() {
		t.Fatal("a fabricated-checksum bundle verified clean — the sweep is vacuous")
	}
}

// TestSweepCatchesUnlabelledLegacyCase is RED-first negative 2 (plan §T8): a case whose recompute surface is
// NOT committed must not pass SILENTLY — dropping the explicit legacy label fails the bundle.
func TestSweepCatchesUnlabelledLegacyCase(t *testing.T) {
	raw := readBundle(t, "local-live-0.1.0")
	if findings := VerifyManifest(raw, nil); len(findings) != 0 {
		t.Fatalf("local-live-0.1.0 must verify clean while labelled: %v", findings)
	}
	stripped := bytes.ReplaceAll(raw, []byte(`"checksum_surface": "`+LegacyShapeOnly+`",`), nil)
	if bytes.Equal(stripped, raw) {
		t.Fatal("local-live-0.1.0 carries no legacy label to strip")
	}
	findings := VerifyManifest(stripped, nil)
	if !hasKind(findings, "missing") || !hasDetail(findings, "checksum_surface") {
		t.Fatalf("an unlabelled shape-only case was not caught: %v", findings)
	}
}

// TestLegacyLabelCannotDodgeRecompute is RED-first negative 3: the legacy label is an admission of historical
// fact, never an opt-out. Labelling a case on a release the table declares recomputable fails.
func TestLegacyLabelCannotDodgeRecompute(t *testing.T) {
	raw := readBundle(t, "automation-0.1.0")
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode automation-0.1.0: %v", err)
	}
	first := m["cases"].([]any)[0].(map[string]any)
	first["checksum"] = "sha256:" + strings.Repeat("f", 64)
	first["checksum_surface"] = LegacyShapeOnly
	findings := VerifyManifest(marshal(t, m), nil)
	if !hasDetail(findings, "surface IS committed") {
		t.Fatalf("a legacy label dodged the recompute: %v", findings)
	}
}

// TestVerifyManifestRefusesShrunkCaseSurface is the E18 T8 "ledger must not be SHRINKABLE" negative, at the
// VerifyManifest layer (T11's invariant; T10's release index rests on this call, not on the sweep).
//
// The dodge: caseChecksumParts resolves the E11/E12 journey families by PROOF-BLOCK PRESENCE, so a case that
// deletes its dedupe_claim + dedupe_proof has no surface left to recompute — and before the surface table was
// hoisted into the verifier, that case could then label itself legacy shape-only and carry ANY checksum. The
// bundle's own manifest decided it had nothing to prove. Now the release's state is read from the CODE side, so
// the missing surface is a finding instead of an escape.
func TestVerifyManifestRefusesShrunkCaseSurface(t *testing.T) {
	raw := readBundle(t, "automation-0.1.0")
	if findings := VerifyManifest(raw, nil); len(findings) != 0 {
		t.Fatalf("automation-0.1.0 must verify clean before it is shrunk: %v", findings)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode automation-0.1.0: %v", err)
	}
	first := m["cases"].([]any)[0].(map[string]any)
	if first["id"] != "AUT-001" {
		t.Fatalf("expected AUT-001 first, got %v", first["id"])
	}
	// Shrink: the claim AND its proof go, so no "claim without proof" finding fires — the case simply stops
	// covering the delivery id its checksum was over.
	delete(first, "dedupe_claim")
	delete(first, "dedupe_proof")
	first["checksum"] = "sha256:" + strings.Repeat("f", 64)
	first["checksum_surface"] = LegacyShapeOnly

	findings := VerifyManifest(marshal(t, m), nil)
	if len(findings) == 0 {
		t.Fatal("a case that DELETED its proof block, fabricated its checksum and labelled itself legacy " +
			"verified clean — the ledger is shrinkable at the VerifyManifest layer")
	}
	if !hasDetail(findings, "surface IS committed") && !hasDetail(findings, "may not shrink away") {
		t.Fatalf("the shrunk case was refused for the wrong reason: %v", findings)
	}
}

// TestVerifyManifestRefusesUndeclaredRelease is the E18 T8 "ledger must not be RENAMEABLE" negative. Every
// recompute branch used to be selected by m.Release, a MANIFEST-controlled string: bumping automation-0.1.0 to
// automation-0.1.1 matched no family, so every case became surface-less, and labelling them all legacy let four
// fabricated checksums verify clean. An undeclared release is now refused outright — the table is the authority
// on which releases exist and what they owe.
func TestVerifyManifestRefusesUndeclaredRelease(t *testing.T) {
	raw := readBundle(t, "automation-0.1.0")
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode automation-0.1.0: %v", err)
	}
	m["release"] = "automation-0.1.1"
	for _, c := range m["cases"].([]any) {
		cs := c.(map[string]any)
		cs["checksum"] = "sha256:" + strings.Repeat("f", 64)
		cs["checksum_surface"] = LegacyShapeOnly
	}
	findings := VerifyManifest(marshal(t, m), nil)
	if len(findings) == 0 {
		t.Fatal("a bundle that RENAMED its release, fabricated every checksum and labelled every case " +
			"verified clean — the ledger is shrinkable by rename")
	}
	if !hasDetail(findings, "declares no checksum surface") {
		t.Fatalf("the renamed release was refused for the wrong reason: %v", findings)
	}
}

// TestChecksumNoteIsRequiredWhereHistoryIsClaimed pins SHOULD-FIX 2: the honest record is ENFORCED, not
// optional metadata. Drop the note from a corrected bundle or a shape-only bundle and verification fails, so
// the NEXT correction cannot land silently the way this one nearly did.
func TestChecksumNoteIsRequiredWhereHistoryIsClaimed(t *testing.T) {
	for _, release := range []string{"automation-0.1.0", "recovery-0.1.0", "local-live-0.1.0", "coding-0.1.0"} {
		var m map[string]any
		if err := json.Unmarshal(readBundle(t, release), &m); err != nil {
			t.Fatalf("decode %s: %v", release, err)
		}
		if note, _ := m["checksum_note"].(string); strings.TrimSpace(note) == "" {
			t.Errorf("%s carries no checksum_note to begin with", release)
			continue
		}
		delete(m, "checksum_note")
		findings := VerifyManifest(marshal(t, m), nil)
		if !hasDetail(findings, "checksum_note") {
			t.Errorf("%s verified without its checksum_note: %v", release, findings)
		}
	}
}

// hasDetail reports whether any finding's detail contains substr.
func hasDetail(findings []Finding, substr string) bool {
	for _, f := range findings {
		if strings.Contains(f.Detail, substr) {
			return true
		}
	}
	return false
}
