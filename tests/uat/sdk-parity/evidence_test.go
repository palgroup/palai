// Package sdkparity holds the E16 T8 SDK-parity + provider-completeness EXIT gate: the Docker-free evidence
// anchor (this file) + the catalog gate (catalog_test.go) + the promote-refusal unit (promote_test.go) + the
// host-agnostic live journey (journey_test.go, //go:build uat). The first three ride `make verify` (no
// credential, no stack); the journey is Docker- + provider-bound.
//
// This file is the ANTI-FABRICATION ANCHOR (plan §2, the crown jewel) for the sdk-provider-parity-0.1.0 bundle.
// It verifies the committed bundle clean through the shared verifier AND re-derives every anchored value from its
// CANONICAL source — the cross-language equality digest recomputed from the four clients' RAW normalized outputs
// (a fabricated "equal" over divergent outputs fails), the gateway-off route-config digest, the provider
// conformance facet set, the packaging manifest digest, and the per-case checksum. A committed digest/checksum
// that does not reproduce is a fabricated value the shape-checked verifier alone cannot see — exactly the
// E13/E14/E15 managed-cloud/self-host/upgrade MUST-FIX-1 defect this catches, now applied to cross-language
// equality (the E16 crown).
//
// The committed bundle is authored deterministic data; the separate live journey (make uat-sdk-parity
// PROVIDER=provider-one) captures the REAL four-client parity + gateway-off + provider runs independently. This
// is the deterministic mirror of `make evidence-verify RELEASE=sdk-provider-parity-0.1.0`.
package sdkparity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"

	"github.com/palgroup/palai/tests/uat"
)

// hashParts reproduces the tests/uat hashParts construction (sha256 of each part followed by a NUL, hex,
// sha256:-prefixed) so this gate can recompute the committed equality/config/packaging digests + per-case
// checksums. ponytail: a 4-line copy, not a shared export (the upgrade/self-host anchors make the same copy).
func hashParts(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// canonical renders a raw JSON value in canonical form (sorted keys), the same construction evidence.go's
// canonicalJSON and the T2 harness's canon() use — so two structurally-equal decodes compare byte-for-byte.
func canonical(raw json.RawMessage) (string, bool) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", false
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// equalityDigestAnchored reports whether the four clients' RAW outputs all re-canonicalize to ONE shared form AND
// the equality_digest is hashParts of that agreed form. Anchoring to the recomputed equality (not a stored "equal"
// boolean) closes the fabrication hole — a bundle carrying four divergent outputs with a self-consistent digest is
// NOT cross-language parity. This is the E16 crown, the SF-4 shape applied to the mechanical cross-language diff.
func equalityDigestAnchored(clientOutputs map[string]json.RawMessage, digest string) bool {
	if len(clientOutputs) != len(uat.EqualityClients) {
		return false
	}
	var agreed string
	for i, client := range uat.EqualityClients {
		raw, ok := clientOutputs[client]
		if !ok {
			return false
		}
		canon, ok := canonical(raw)
		if !ok || canon == "" {
			return false
		}
		if i == 0 {
			agreed = canon
		} else if canon != agreed {
			return false
		}
	}
	return digest == hashParts(agreed)
}

// configDigestAnchored reports whether a GatewayOffProof's config_digest is hashParts of the CANONICAL
// GatewayOffRouteConfig — so a bundle that drops a direct route cannot keep a matching digest.
func configDigestAnchored(digest string) bool {
	return digest == hashParts(uat.GatewayOffRouteConfig...)
}

// facetsAnchored reports whether a ProviderConformanceProof's facets are the CANONICAL conformance surface — a
// dropped facet (e.g. quietly removing "tool") cannot pass.
func facetsAnchored(facets []string) bool {
	return slices.Equal(facets, uat.ProviderConformanceFacets)
}

// packagingDigestAnchored reports whether a PackagingProof's manifest_digest is hashParts of its SORTED package
// names — re-derivable, so a fabricated digest over an unstated package set is caught.
func packagingDigestAnchored(packages []string, digest string) bool {
	sorted := slices.Clone(packages)
	sort.Strings(sorted)
	return digest == hashParts(sorted...)
}

// spCase is the subset of the manifest a case carries for the anchored re-derivation.
type spCase struct {
	ID                         string `json:"id"`
	RunID                      string `json:"run_id"`
	Checksum                   string `json:"checksum"`
	ThreeLanguageEqualityClaim string `json:"three_language_equality_claim"`
	ThreeLanguageEqualityProof *struct {
		RunID          string                     `json:"run_id"`
		ClientOutputs  map[string]json.RawMessage `json:"client_outputs"`
		EqualityDigest string                     `json:"equality_digest"`
	} `json:"three_language_equality_proof"`
	ProviderConformanceClaim string `json:"provider_conformance_claim"`
	ProviderConformanceProof *struct {
		Provider    string   `json:"provider"`
		Facets      []string `json:"facets"`
		ProbeDigest string   `json:"probe_digest"`
	} `json:"provider_conformance_proof"`
	GatewayOffClaim string `json:"gateway_off_claim"`
	GatewayOffProof *struct {
		ConfigDigest string `json:"config_digest"`
	} `json:"gateway_off_proof"`
	PackagingClaim string `json:"packaging_claim"`
	PackagingProof *struct {
		ManifestDigest string   `json:"manifest_digest"`
		Packages       []string `json:"packages"`
	} `json:"packaging_proof"`
}

// TestSDKParityReleaseVerifiesClean wires the sdk-provider-parity-0.1.0 bundle into the shared evidence verifier:
// it must verify clean (0 failed, 0 missing, 0 secret) with every E16 rule ACTIVE on real data — four clients
// decoded one run identically (three-language equality), two provider families + the openai-compatible probe
// passed conformance (provider-conformance), the stand-in gateway was killed and the direct routes served a real
// run (gateway-off), and the SDK packages built + signed + re-verified (packaging). It also re-derives every
// anchored digest/checksum from its canonical source. It fails (bundle absent) until the bundle is committed.
func TestSDKParityReleaseVerifiesClean(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, "evidence", "releases", "sdk-provider-parity-0.1.0")

	summary, err := uat.VerifyRelease(dir, nil)
	if err != nil {
		t.Fatalf("verify sdk-provider-parity-0.1.0: %v", err)
	}
	if !summary.OK() {
		t.Fatalf("sdk-provider-parity-0.1.0 did not verify clean: %s\n%v", summary.String(), summary.Findings)
	}
	if summary.Passed == 0 {
		t.Fatalf("sdk-provider-parity-0.1.0 verified zero cases (%s)", summary.String())
	}

	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("read sdk-provider-parity-0.1.0 manifest: %v", err)
	}
	var parsed struct {
		Maturity string   `json:"maturity"`
		Cases    []spCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("decode sdk-provider-parity-0.1.0 manifest: %v", err)
	}
	if parsed.Maturity != "rc" {
		t.Fatalf("sdk-provider-parity-0.1.0 maturity = %q, want \"rc\" (an RC bundle, not a stable sign-off)", parsed.Maturity)
	}

	// The per-case checksum anchor for this release: hashParts(id, run_id, the crown equality digest). It is
	// resolved from the bundle's OWN equality proof (re-derived below), so a checksum that does not reproduce is
	// fabricated.
	equalityDigest := ""
	for _, c := range parsed.Cases {
		if c.ThreeLanguageEqualityProof != nil {
			equalityDigest = c.ThreeLanguageEqualityProof.EqualityDigest
		}
	}
	if equalityDigest == "" {
		t.Fatal("sdk-provider-parity-0.1.0 carries no ThreeLanguageEqualityProof — the crown case is missing")
	}

	var equality, conformance, gatewayOff, packaging int
	for _, c := range parsed.Cases {
		if want := hashParts(c.ID, c.RunID, equalityDigest); c.Checksum != want {
			t.Fatalf("%s checksum %q is not hashParts(id, run_id, equality digest) %q — an authored checksum must be re-derivable", c.ID, c.Checksum, want)
		}
		if c.ThreeLanguageEqualityClaim != "" && c.ThreeLanguageEqualityProof != nil {
			equality++
			// THE CROWN: the equality digest must reproduce from the four clients' RAW outputs re-canonicalized equal.
			if !equalityDigestAnchored(c.ThreeLanguageEqualityProof.ClientOutputs, c.ThreeLanguageEqualityProof.EqualityDigest) {
				t.Fatalf("%s three_language_equality_proof is not anchored: the four clients' outputs do not re-canonicalize equal, or equality_digest is not hashParts(agreed output)", c.ID)
			}
		}
		if c.ProviderConformanceClaim != "" && c.ProviderConformanceProof != nil {
			conformance++
			if !facetsAnchored(c.ProviderConformanceProof.Facets) {
				t.Fatalf("%s provider_conformance_proof facets %v are not the canonical set %v", c.ID, c.ProviderConformanceProof.Facets, uat.ProviderConformanceFacets)
			}
		}
		if c.GatewayOffClaim != "" && c.GatewayOffProof != nil {
			gatewayOff++
			if !configDigestAnchored(c.GatewayOffProof.ConfigDigest) {
				t.Fatalf("%s gateway_off_proof config_digest %q is not hashParts(canonical GatewayOffRouteConfig)", c.ID, c.GatewayOffProof.ConfigDigest)
			}
		}
		if c.PackagingClaim != "" && c.PackagingProof != nil {
			packaging++
			if !packagingDigestAnchored(c.PackagingProof.Packages, c.PackagingProof.ManifestDigest) {
				t.Fatalf("%s packaging_proof manifest_digest %q is not hashParts(sorted packages %v)", c.ID, c.PackagingProof.ManifestDigest, c.PackagingProof.Packages)
			}
		}
	}
	// The RC bundle must exercise EVERY E16 proof rule — a bundle missing any would silently not test that
	// invariant (the all-rules-exercised loop, the E14/E15 precedent).
	if equality == 0 || conformance == 0 || gatewayOff == 0 || packaging == 0 {
		t.Fatalf("sdk-provider-parity-0.1.0 does not exercise all E16 rules: equality=%d conformance=%d gateway_off=%d packaging=%d",
			equality, conformance, gatewayOff, packaging)
	}
}

// TestSDKParityAnchorsRejectFabrication pins the anti-fabrication anchors: each rejects a self-consistent-but-
// invented value the shape-checked verifier would pass. Without these, a fabricated "equal" over divergent
// outputs / a dropped facet / a fabricated config digest with a matching-looking checksum would verify green
// while claiming a bogus fact (the managed-cloud/self-host/upgrade MUST-FIX-1 precedent, now on cross-language
// equality — the E16 crown).
func TestSDKParityAnchorsRejectFabrication(t *testing.T) {
	agreed := json.RawMessage(`{"id":"r","output_text":"ok","status":"response.completed"}`)
	canon, _ := canonical(agreed)
	realDigest := hashParts(canon)

	// The crown: four identical decodes with the real digest anchors.
	good := map[string]json.RawMessage{"typescript": agreed, "python": agreed, "go": agreed, "cli": agreed}
	if !equalityDigestAnchored(good, realDigest) {
		t.Fatal("four identical decodes with the real equality digest must anchor")
	}
	// A fabricated "equal": one client diverges but the digest is self-consistent with the majority — must NOT anchor.
	divergent := map[string]json.RawMessage{
		"typescript": agreed, "python": agreed, "go": agreed,
		"cli": json.RawMessage(`{"id":"r","output_text":"DIFFERENT","status":"response.completed"}`),
	}
	if equalityDigestAnchored(divergent, realDigest) {
		t.Fatal("a divergent client output with a self-consistent digest must NOT anchor (the fabricated-equality hole — the E16 crown)")
	}
	// A dropped client (only three of the four) — must NOT anchor.
	dropped := map[string]json.RawMessage{"typescript": agreed, "python": agreed, "go": agreed}
	if equalityDigestAnchored(dropped, realDigest) {
		t.Fatal("a bundle missing one of the four clients must NOT anchor")
	}
	// A hand-edited equality digest the outputs do not reproduce — must NOT anchor.
	if equalityDigestAnchored(good, hashParts("not-the-agreed-output")) {
		t.Fatal("a fabricated equality digest the four outputs do not reproduce must NOT anchor")
	}

	// The config digest anchors to the canonical route set; a dropped direct route with a self-consistent digest does not.
	if !configDigestAnchored(hashParts(uat.GatewayOffRouteConfig...)) {
		t.Fatal("the canonical gateway-off route config must anchor")
	}
	droppedRoute := slices.Clone(uat.GatewayOffRouteConfig)[:len(uat.GatewayOffRouteConfig)-1]
	if configDigestAnchored(hashParts(droppedRoute...)) {
		t.Fatal("a dropped direct route with a self-consistent digest must NOT anchor")
	}

	// The facet set anchors; a dropped facet (removing "tool") does not.
	if !facetsAnchored(slices.Clone(uat.ProviderConformanceFacets)) {
		t.Fatal("the canonical conformance facet set must anchor")
	}
	if facetsAnchored([]string{"text", "stream", "schema"}) {
		t.Fatal("a dropped-facet set must NOT anchor")
	}

	// The packaging digest anchors to the sorted package names; a fabricated digest does not.
	if !packagingDigestAnchored([]string{"go", "python", "typescript"}, hashParts("go", "python", "typescript")) {
		t.Fatal("a real packaging digest over the sorted packages must anchor")
	}
	if packagingDigestAnchored([]string{"go", "python", "typescript"}, hashParts("only-go")) {
		t.Fatal("a fabricated packaging digest must NOT anchor")
	}
}

// repoRoot walks up to the module root (the dir holding go.mod).
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}
