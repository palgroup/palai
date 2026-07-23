package selfhost

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

// selfHostBackupManifestParts is the declared canonical install-backup identity the committed bundle records its
// manifest_digest over — hashParts(kind, migration, org ids, sample response). This gate recomputes that
// digest and fails if it does not reproduce, so a committed manifest_digest that is not the real value is a
// fabricated hash (the managed-cloud artifact-content-digest precedent, adapted to the install backup). The
// committed bundle is authored deterministic data; the separate live journey (make uat-self-host
// PROVIDER=provider-one) captures a REAL backup and restores it into a separate stack independently. Keep this
// in lockstep with the manifest_digest the bundle's BackupProof records.
var selfHostBackupManifestParts = []string{
	"palai-install-backup", "32", "org_local", "org_self_host_journey", "resp_self_host_journey",
}

// hashParts reproduces the tests/uat hashParts construction (sha256 of each part followed by a NUL, hex,
// sha256:-prefixed) so this gate can recompute the committed journey_digest, manifest_digest, and per-case
// checksums. A committed value that does not reproduce is a fabricated hash, the exact defect the shape-checked
// verifier cannot see. ponytail: a 4-line copy, not a shared export (the journey helper is uat-tagged).
func hashParts(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// spineAnchored reports whether an InstallProof's step_ids + journey_digest are the CANONICAL restart-less
// install spine: the step list must equal uat.SelfHostStepIDs exactly AND the digest must be hashParts of that
// canonical list. Anchoring to the canonical set (not the manifest's own step_ids) is what closes the
// fabrication hole — a bundle carrying 10 invented step ids + a self-consistent digest is NOT the real spine.
func spineAnchored(stepIDs []string, digest string) bool {
	return slices.Equal(stepIDs, uat.SelfHostStepIDs) && digest == hashParts(uat.SelfHostStepIDs...)
}

// TestSelfHostReleaseVerifiesClean wires the self-host-0.1.0 bundle into the shared evidence verifier: the
// committed release must verify clean (0 failed, 0 missing, 0 secret findings) with the E14 self-host rules
// ACTIVE on real data — a clean production install came up hardened and resolved the restart-less install
// spine ending in a REAL provider run (install + the restart-less spine, OPS-002), an installation backup
// restored into a SEPARATE clean stack (backup, DR-002), and `restore verify` matched the manifest across all
// six checks (restore-verify, DR-004..006). The committed bundle is authored deterministic data; the separate
// live journey (make uat-self-host PROVIDER=provider-one) proves the real restart-less install + backup/restore
// + a real provider run independently. This is the deterministic mirror of `make evidence-verify
// RELEASE=self-host-0.1.0`. It fails (bundle absent) until the bundle is committed.
func TestSelfHostReleaseVerifiesClean(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, "evidence", "releases", "self-host-0.1.0")

	summary, err := uat.VerifyRelease(dir, nil)
	if err != nil {
		t.Fatalf("verify self-host-0.1.0: %v", err)
	}
	if !summary.OK() {
		t.Fatalf("self-host-0.1.0 did not verify clean: %s\n%v", summary.String(), summary.Findings)
	}
	if summary.Passed == 0 {
		t.Fatalf("self-host-0.1.0 verified zero cases; expected OPS-002 + DR-002 + DR-004..006 (%s)", summary.String())
	}
	if summary.SecretFindings != 0 {
		t.Fatalf("self-host-0.1.0 leaked a credential: %d secret findings", summary.SecretFindings)
	}

	// Each E14 self-host rule must be exercised on REAL release data, not only in the unit fixtures: the
	// committed bundle carries at least one case with a non-empty claim AND its proof block for each rule. A
	// bundle missing any rule would silently not test that invariant, so it fails here (the managed-cloud
	// all-rules-exercised loop copy). Since it verified clean above, each present claim's proof is complete.
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("read self-host-0.1.0 manifest: %v", err)
	}
	var parsed struct {
		Cases []struct {
			ID           string `json:"id"`
			RunID        string `json:"run_id"`
			Checksum     string `json:"checksum"`
			InstallClaim string `json:"install_claim"`
			InstallProof *struct {
				StepIDs       []string `json:"step_ids"`
				JourneyDigest string   `json:"journey_digest"`
			} `json:"install_proof"`
			BackupClaim string `json:"backup_claim"`
			BackupProof *struct {
				ManifestDigest string `json:"manifest_digest"`
			} `json:"backup_proof"`
			RestoreVerifyClaim string          `json:"restore_verify_claim"`
			RestoreVerifyProof json.RawMessage `json:"restore_verify_proof"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("decode self-host-0.1.0 manifest: %v", err)
	}

	// canonicalDigest anchors both the spine and the per-case checksums: an authored bundle cannot invent a
	// digest or a checksum, because both are re-derived from the CANONICAL spine + the case's own id/run.
	canonicalDigest := hashParts(uat.SelfHostStepIDs...)
	wantManifestDigest := hashParts(selfHostBackupManifestParts...)

	var install, backup, restoreVerify int
	for _, c := range parsed.Cases {
		// Per-case checksum is re-derivable: hashParts(id, run_id, canonical journey_digest). A checksum that
		// does not reproduce from the case's own id + run + the canonical spine digest is fabricated.
		if want := hashParts(c.ID, c.RunID, canonicalDigest); c.Checksum != want {
			t.Fatalf("%s checksum %q is not hashParts(id, run_id, journey_digest) %q — an authored checksum must be re-derivable", c.ID, c.Checksum, want)
		}
		if c.InstallClaim != "" && c.InstallProof != nil {
			install++
			// Anti-fabrication, ANCHORED: the step_ids must be the CANONICAL restart-less install spine and the
			// journey_digest hashParts of it — not merely self-consistent with a fabricated step list.
			if !spineAnchored(c.InstallProof.StepIDs, c.InstallProof.JourneyDigest) {
				t.Fatalf("%s install_proof is not anchored to the canonical spine: step_ids=%v digest=%q, want %v / %q",
					c.ID, c.InstallProof.StepIDs, c.InstallProof.JourneyDigest, uat.SelfHostStepIDs, canonicalDigest)
			}
		}
		if c.BackupClaim != "" && c.BackupProof != nil {
			backup++
			// Anti-fabrication: the backup manifest_digest MUST be hashParts of the declared canonical
			// backup identity — a committed digest that does not reproduce from that identity is fabricated.
			if c.BackupProof.ManifestDigest != wantManifestDigest {
				t.Fatalf("%s backup manifest_digest %q is not hashParts(canonical backup identity) %q — a committed manifest digest must be the real value",
					c.ID, c.BackupProof.ManifestDigest, wantManifestDigest)
			}
		}
		if c.RestoreVerifyClaim != "" && len(c.RestoreVerifyProof) > 0 {
			restoreVerify++
		}
	}
	if install == 0 || backup == 0 || restoreVerify == 0 {
		t.Fatalf("self-host-0.1.0 does not exercise all E14 self-host rules: install=%d backup=%d restore_verify=%d",
			install, backup, restoreVerify)
	}
}

// TestSelfHostSpineAnchorRejectsFabricatedSteps pins the anti-fabrication anchor: the anchor rejects a bundle
// whose step_ids differ from the canonical spine even when the digest is self-consistent with those
// (fabricated) steps. Without this, 10 invented step ids + a matching digest would pass green while claiming a
// bogus spine (the managed-cloud MUST-FIX-1 precedent).
func TestSelfHostSpineAnchorRejectsFabricatedSteps(t *testing.T) {
	// The canonical spine + its real digest passes.
	if !spineAnchored(uat.SelfHostStepIDs, hashParts(uat.SelfHostStepIDs...)) {
		t.Fatal("the canonical install spine must anchor")
	}
	// A fabricated step list with a SELF-CONSISTENT digest (digest = hashParts(fabricated)) must FAIL — the
	// old gate that recomputed from the manifest's own step_ids would have passed this.
	fabricated := []string{"s1", "s2", "s3", "s4", "s5", "s6", "s7", "s8", "s9", "s10"}
	if spineAnchored(fabricated, hashParts(fabricated...)) {
		t.Fatal("a fabricated step list with a self-consistent digest must NOT anchor (that is the fabrication hole)")
	}
	// A canonical list with a wrong digest also fails.
	if spineAnchored(uat.SelfHostStepIDs, "sha256:"+string(make([]byte, 0))) {
		t.Fatal("the canonical list with an empty/wrong digest must NOT anchor")
	}
}
