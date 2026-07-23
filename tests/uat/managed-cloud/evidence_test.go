package managedcloud

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/palgroup/palai/tests/uat"
)

// managedCloudArtifactContent is the fixture artifact content the committed bundle records its content_digest
// over, so this gate can recompute that digest and fail if it does not reproduce — a committed digest that
// does not equal sha256 of these bytes is fabricated. The committed bundle is authored deterministic data (no
// live journey writes this file); the separate live journey (TestManagedCloudJourney) proves the real
// restart-less spine independently. Keep this in lockstep with the byte_len/content_digest the bundle records.
const managedCloudArtifactContent = "MANAGED-CLOUD-ARTIFACT\n"

// hashParts reproduces the tests/uat hashParts / hashBundle construction (sha256 of each part followed by a
// NUL, hex, sha256:-prefixed) so this gate can recompute the committed journey_digest and per-case checksums.
// A committed value that does not reproduce is a fabricated hash, the exact defect the shape-checked verifier
// cannot see. ponytail: a 4-line copy, not a shared export (the journey helper is uat-tagged).
func hashParts(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// spineAnchored reports whether a ProvisioningProof's step_ids + journey_digest are the CANONICAL restart-less
// spine: the step list must equal uat.ManagedCloudStepIDs exactly AND the digest must be hashParts of that
// canonical list. Anchoring to the canonical set (not the manifest's own step_ids) is what closes the
// fabrication hole — a bundle carrying 8 invented step ids + a self-consistent digest is NOT the real spine.
func spineAnchored(stepIDs []string, digest string) bool {
	return slices.Equal(stepIDs, uat.ManagedCloudStepIDs) && digest == hashParts(uat.ManagedCloudStepIDs...)
}

// TestManagedCloudReleaseVerifiesClean wires the managed-cloud-0.1.0 bundle into the shared evidence
// verifier: the committed release must verify clean (0 failed, 0 missing, 0 secret findings) with the E13
// managed-cloud rules ACTIVE on real data — a second tenant provisioned restart-less with its config_policy
// applied (provisioning + the restart-less spine), a secret-ref resolved without restart and never surfaced
// (secret-resolve), a cross-tenant read denied with zero leaked rows (isolation), an artifact downloaded with
// a re-derivable content digest (artifact), an admission refused before compute (refusal), two projects'
// distinct per-project routes (route), a binding connection_ref resolved (binding), and a steer driven over
// the public API (steer). The committed bundle is authored deterministic data; the separate live journey
// (make uat-managed-cloud PROVIDER=provider-one) proves the restart-less spine + a real provider run
// independently. This is the deterministic mirror of `make evidence-verify RELEASE=managed-cloud-0.1.0`. It
// fails (bundle absent) until the bundle is committed.
func TestManagedCloudReleaseVerifiesClean(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, "evidence", "releases", "managed-cloud-0.1.0")

	summary, err := uat.VerifyRelease(dir, nil)
	if err != nil {
		t.Fatalf("verify managed-cloud-0.1.0: %v", err)
	}
	if !summary.OK() {
		t.Fatalf("managed-cloud-0.1.0 did not verify clean: %s\n%v", summary.String(), summary.Findings)
	}
	if summary.Passed == 0 {
		t.Fatalf("managed-cloud-0.1.0 verified zero cases; expected the eight MCI-00N cases (%s)", summary.String())
	}
	if summary.SecretFindings != 0 {
		t.Fatalf("managed-cloud-0.1.0 leaked a credential: %d secret findings", summary.SecretFindings)
	}

	// Each E13 managed-cloud rule must be exercised on REAL release data, not only in the unit fixtures: the
	// committed bundle carries at least one case with a non-empty claim AND its proof block for each rule. A
	// bundle missing any rule would silently not test that invariant, so it fails here (the extensibility
	// all-rules-exercised loop copy). Since it verified clean above, each present claim's proof is complete.
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("read managed-cloud-0.1.0 manifest: %v", err)
	}
	var parsed struct {
		Cases []struct {
			ID                string `json:"id"`
			RunID             string `json:"run_id"`
			Checksum          string `json:"checksum"`
			ProvisioningClaim string `json:"provisioning_claim"`
			ProvisioningProof *struct {
				StepIDs       []string `json:"step_ids"`
				JourneyDigest string   `json:"journey_digest"`
			} `json:"provisioning_proof"`
			SecretResolveClaim string          `json:"secret_resolve_claim"`
			SecretResolveProof json.RawMessage `json:"secret_resolve_proof"`
			IsolationClaim     string          `json:"isolation_claim"`
			IsolationProof     json.RawMessage `json:"isolation_proof"`
			ArtifactClaim      string          `json:"artifact_claim"`
			ArtifactProof      *struct {
				ContentDigest string `json:"content_digest"`
				ByteLen       int    `json:"byte_len"`
			} `json:"artifact_proof"`
			RefusalClaim string          `json:"refusal_claim"`
			RefusalProof json.RawMessage `json:"refusal_proof"`
			RouteClaim   string          `json:"route_claim"`
			RouteProof   json.RawMessage `json:"route_proof"`
			BindingClaim string          `json:"binding_claim"`
			BindingProof json.RawMessage `json:"binding_proof"`
			SteerClaim   string          `json:"steer_claim"`
			SteerProof   json.RawMessage `json:"steer_proof"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("decode managed-cloud-0.1.0 manifest: %v", err)
	}

	// canonicalDigest anchors both the spine and the per-case checksums: an authored bundle cannot invent a
	// digest or a checksum, because both are re-derived from the CANONICAL spine + the case's own id/run.
	canonicalDigest := hashParts(uat.ManagedCloudStepIDs...)

	var provisioning, secret, isolation, artifact, refusal, route, binding, steer int
	for _, c := range parsed.Cases {
		// Per-case checksum is re-derivable: hashParts(id, run_id, canonical journey_digest). A checksum that
		// does not reproduce from the case's own id + run + the canonical spine digest is fabricated (SHOULD 5:
		// the field is a real re-derived value, not just a sha256-shaped string).
		if want := hashParts(c.ID, c.RunID, canonicalDigest); c.Checksum != want {
			t.Fatalf("%s checksum %q is not hashParts(id, run_id, journey_digest) %q — an authored checksum must be re-derivable", c.ID, c.Checksum, want)
		}
		if c.ProvisioningClaim != "" && c.ProvisioningProof != nil {
			provisioning++
			// Anti-fabrication, ANCHORED: the step_ids must be the CANONICAL restart-less spine and the
			// journey_digest hashParts of it — not merely self-consistent with a fabricated step list.
			if !spineAnchored(c.ProvisioningProof.StepIDs, c.ProvisioningProof.JourneyDigest) {
				t.Fatalf("%s provisioning_proof is not anchored to the canonical spine: step_ids=%v digest=%q, want %v / %q",
					c.ID, c.ProvisioningProof.StepIDs, c.ProvisioningProof.JourneyDigest, uat.ManagedCloudStepIDs, canonicalDigest)
			}
		}
		if c.SecretResolveClaim != "" && len(c.SecretResolveProof) > 0 {
			secret++
		}
		if c.IsolationClaim != "" && len(c.IsolationProof) > 0 {
			isolation++
		}
		if c.ArtifactClaim != "" && c.ArtifactProof != nil {
			artifact++
			// Anti-fabrication: the artifact content_digest MUST be sha256 of the exact fixture bytes, and its
			// byte length must match — a committed digest that does not reproduce from the known content is
			// fabricated.
			wantDigest := "sha256:" + hex.EncodeToString(sha256Sum(managedCloudArtifactContent))
			if c.ArtifactProof.ContentDigest != wantDigest {
				t.Fatalf("artifact content_digest %q is not sha256 of the fixture artifact bytes %q — a committed content digest must be the real value",
					c.ArtifactProof.ContentDigest, wantDigest)
			}
			if c.ArtifactProof.ByteLen != len(managedCloudArtifactContent) {
				t.Fatalf("artifact byte_len %d does not match the fixture artifact length %d", c.ArtifactProof.ByteLen, len(managedCloudArtifactContent))
			}
		}
		if c.RefusalClaim != "" && len(c.RefusalProof) > 0 {
			refusal++
		}
		if c.RouteClaim != "" && len(c.RouteProof) > 0 {
			route++
		}
		if c.BindingClaim != "" && len(c.BindingProof) > 0 {
			binding++
		}
		if c.SteerClaim != "" && len(c.SteerProof) > 0 {
			steer++
		}
	}
	if provisioning == 0 || secret == 0 || isolation == 0 || artifact == 0 || refusal == 0 || route == 0 || binding == 0 || steer == 0 {
		t.Fatalf("managed-cloud-0.1.0 does not exercise all E13 managed-cloud rules: provisioning=%d secret=%d isolation=%d artifact=%d refusal=%d route=%d binding=%d steer=%d",
			provisioning, secret, isolation, artifact, refusal, route, binding, steer)
	}
}

// TestManagedCloudSpineAnchorRejectsFabricatedSteps pins MUST-FIX 1: the anchor rejects a bundle whose
// step_ids differ from the canonical spine even when the digest is self-consistent with those (fabricated)
// steps. Without this, 8 invented step ids + a matching digest would pass green while claiming a bogus spine.
func TestManagedCloudSpineAnchorRejectsFabricatedSteps(t *testing.T) {
	// The canonical spine + its real digest passes.
	if !spineAnchored(uat.ManagedCloudStepIDs, hashParts(uat.ManagedCloudStepIDs...)) {
		t.Fatal("the canonical spine must anchor")
	}
	// A fabricated step list with a SELF-CONSISTENT digest (digest = hashParts(fabricated)) must FAIL — the
	// old gate that recomputed from the manifest's own step_ids would have passed this.
	fabricated := []string{"step-1", "step-2", "step-3", "step-4", "step-5", "step-6", "step-7", "step-8"}
	if spineAnchored(fabricated, hashParts(fabricated...)) {
		t.Fatal("a fabricated step list with a self-consistent digest must NOT anchor (that is the fabrication hole)")
	}
	// A canonical list with a wrong digest also fails.
	if spineAnchored(uat.ManagedCloudStepIDs, "sha256:"+string(make([]byte, 0))) {
		t.Fatal("the canonical list with an empty/wrong digest must NOT anchor")
	}
}

// sha256Sum returns the raw sha256 of s (helper so the anti-fabrication digest reads as one line).
func sha256Sum(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}
