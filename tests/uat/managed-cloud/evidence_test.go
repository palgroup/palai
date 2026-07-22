package managedcloud

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/palgroup/palai/tests/uat"
)

// managedCloudArtifactContent is the exact byte string the deterministic journey writes as its artifact, so
// this gate can recompute the committed artifact content_digest from it — a committed digest that does not
// reproduce from these bytes is fabricated. The live tier overwrites the case with the real run's artifact
// bytes + digest (verified against the workspace file inside the journey). Keep this in lockstep with the
// content the journey writes.
const managedCloudArtifactContent = "MANAGED-CLOUD-ARTIFACT\n"

// hashParts reproduces the journey's hashParts (sha256 of each part followed by a NUL, hex, sha256:-prefixed
// — the tests/uat.hashParts / hashBundle construction) so this gate can recompute a journey_digest from the
// manifest's own step_ids. A committed digest that does not reproduce is a fabricated hash, the exact defect
// the shape-checked verifier cannot see. ponytail: a 4-line copy, not a shared export (the journey helper is
// uat-tagged).
func hashParts(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// TestManagedCloudReleaseVerifiesClean wires the managed-cloud-0.1.0 bundle into the shared evidence
// verifier: the committed release must verify clean (0 failed, 0 missing, 0 secret findings) with the E13
// managed-cloud rules ACTIVE on real data — a second tenant provisioned restart-less with its config_policy
// applied (provisioning + the restart-less spine), a secret-ref resolved without restart and never surfaced
// (secret-resolve), a cross-tenant read denied with zero leaked rows (isolation), an artifact downloaded with
// a re-derivable content digest (artifact), an admission refused before compute (refusal), two projects'
// distinct per-project routes (route), a binding connection_ref resolved (binding), and an SDK-driven steer
// applied (steer). This is the deterministic mirror of `make evidence-verify RELEASE=managed-cloud-0.1.0`; the
// gated live tier overwrites the same bundle with real chatcmpl ids. It fails (bundle absent) until the
// bundle is committed.
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
	var provisioning, secret, isolation, artifact, refusal, route, binding, steer int
	for _, c := range parsed.Cases {
		if c.ProvisioningClaim != "" && c.ProvisioningProof != nil {
			provisioning++
			// Anti-fabrication (mirrors the E12 advertised_schema_hash gate): the journey_digest MUST be the
			// real hash of the manifest's OWN step_ids — a value that does not reproduce is a fabricated spine
			// digest, the exact defect a reviewer caught with shasum on a prior release.
			if got := hashParts(c.ProvisioningProof.StepIDs...); got != c.ProvisioningProof.JourneyDigest {
				t.Fatalf("journey_digest %q is not hashParts(step_ids) %q — a committed spine digest must be the real value the journey produces",
					c.ProvisioningProof.JourneyDigest, got)
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
			// Anti-fabrication: the artifact content_digest MUST be sha256 of the exact bytes the journey
			// wrote, and its byte length must match — a committed digest that does not reproduce from the known
			// content is fabricated.
			wantDigest := "sha256:" + hex.EncodeToString(sha256Sum(managedCloudArtifactContent))
			if c.ArtifactProof.ContentDigest != wantDigest {
				t.Fatalf("artifact content_digest %q is not sha256 of the journey's artifact bytes %q — a committed content digest must be the real value",
					c.ArtifactProof.ContentDigest, wantDigest)
			}
			if c.ArtifactProof.ByteLen != len(managedCloudArtifactContent) {
				t.Fatalf("artifact byte_len %d does not match the journey's artifact length %d", c.ArtifactProof.ByteLen, len(managedCloudArtifactContent))
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

// sha256Sum returns the raw sha256 of s (helper so the anti-fabrication digest reads as one line).
func sha256Sum(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}
