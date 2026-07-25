package uat

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// committedBundleSurfaces is the E18 T8 anti-fabrication sweep's expectation over the WHOLE committed corpus
// (plan §T8). Every bundle under evidence/releases is one of two honest states:
//
//   - "recomputed" — every case's checksum RE-DERIVES from its canonical surface (caseChecksumParts), so a
//     fabricated value is caught mechanically. No case may carry a legacy label.
//   - LegacyShapeOnly — the surface its generator hashed is NOT committed in the manifest (a live run's model
//     id, the raw response body, the run's work branch), so the checksum is shape-checked only and EVERY case
//     carries the explicit label. Silence is not historical honesty.
//
// A bundle with NO entry here FAILS the sweep: a new bundle must declare its surface in this table (the CODE
// side), and it can never self-declare legacy from its own manifest.
//
// HONEST CEILING: the raw evidence of the historical runs is NOT re-produced — those runs are history and the
// sweep does not re-run them. What it proves is recompute over the COMMITTED surface plus honest labelling of
// what cannot be recomputed.
var committedBundleSurfaces = map[string]string{
	// Recomputed: the checksum surface is committed in the bundle (see caseChecksumParts).
	"automation-0.1.0":          "recomputed",
	"extensibility-0.1.0":       "recomputed",
	"recovery-0.1.0":            "recomputed",
	"managed-cloud-0.1.0":       "recomputed",
	"self-host-0.1.0":           "recomputed",
	"self-host-0.2.0":           "recomputed",
	"sdk-provider-parity-0.1.0": "recomputed",
	"extensions-0.1.0":          "recomputed",
	// Legacy shape-only: the generator hashed uncommitted runtime bytes.
	"coding-0.1.0":                   LegacyShapeOnly,
	"interactive-0.1.0":              LegacyShapeOnly,
	"local-live-0.1.0":               LegacyShapeOnly,
	"local-live-0.1.0-chaining":      LegacyShapeOnly,
	"local-live-0.1.0-command-spine": LegacyShapeOnly,
	"local-live-0.1.0-config-switch": LegacyShapeOnly,
	"local-live-0.1.0-lifecycle":     LegacyShapeOnly,
	"local-live-0.1.0-subagents":     LegacyShapeOnly,
}

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
		recomputed, labelled := 0, 0
		for _, c := range m.Cases {
			parts := caseChecksumParts(m, c)
			switch want {
			case "recomputed":
				if parts == nil {
					t.Errorf("%s/%s: the sweep table says recomputed but no canonical surface resolves", release, c.ID)
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
			case LegacyShapeOnly:
				if parts != nil {
					t.Errorf("%s/%s is labelled legacy but a canonical surface resolves — recompute it instead", release, c.ID)
				}
				if c.ChecksumSurface != LegacyShapeOnly {
					t.Errorf("%s/%s is a shape-only case with checksum_surface=%q, want %q",
						release, c.ID, c.ChecksumSurface, LegacyShapeOnly)
				}
				labelled++
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
// fact, never an opt-out. Labelling a case whose canonical surface IS committed fails.
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

// TestCorrectedAUT001Recomputes pins the E18 T8 fix itself (plan §T8): AUT-001's committed checksum is
// hashParts(run_id, the dedupe proof's original delivery id, "dedupe") — the construction the §63.4 journey
// writer uses (apps/control-plane/e2e/responses/automation_journey_helpers_test.go). The value it replaced was
// shape-valid and reproduced nothing.
func TestCorrectedAUT001Recomputes(t *testing.T) {
	var m evidenceManifest
	if err := json.Unmarshal(readBundle(t, "automation-0.1.0"), &m); err != nil {
		t.Fatalf("decode automation-0.1.0: %v", err)
	}
	for _, c := range m.Cases {
		if c.ID != "AUT-001" {
			continue
		}
		if c.DedupeProof == nil {
			t.Fatal("AUT-001 carries no dedupe proof")
		}
		want := hashParts(c.RunID, c.DedupeProof.OriginalDeliveryID, "dedupe")
		if c.Checksum != want {
			t.Fatalf("AUT-001 checksum %s is not hashParts(run_id, original_delivery_id, \"dedupe\") %s", c.Checksum, want)
		}
		return
	}
	t.Fatal("automation-0.1.0 carries no AUT-001 case")
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
