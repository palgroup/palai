package recovery

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/palgroup/palai/tests/uat"
)

// TestRecoveryReleaseVerifiesClean wires the recovery-0.1.0 bundle into the shared evidence verifier: the
// committed release must verify clean (0 failed, 0 missing, 0 secret findings) with the §26.12 RecoveryProof
// rule ACTIVE on real data — its recovered-run case carries a COMPLETE proof, and its external-receipt case
// proves the approved push landed exactly once despite the kill (duplicate external effect = 0). This is the
// deterministic mirror of `make evidence-verify RELEASE=recovery-0.1.0`; the gated live tier overwrites the
// same bundle with real chatcmpl + real remote receipts.
func TestRecoveryReleaseVerifiesClean(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, "evidence", "releases", "recovery-0.1.0")

	summary, err := uat.VerifyRelease(dir, nil)
	if err != nil {
		t.Fatalf("verify recovery-0.1.0: %v", err)
	}
	if !summary.OK() {
		t.Fatalf("recovery-0.1.0 did not verify clean: %s\n%v", summary.String(), summary.Findings)
	}
	if summary.Passed == 0 {
		t.Fatalf("recovery-0.1.0 verified zero cases; expected the recovered-run + push-once cases (%s)", summary.String())
	}
	if summary.SecretFindings != 0 {
		t.Fatalf("recovery-0.1.0 leaked a credential: %d secret findings", summary.SecretFindings)
	}

	// The RecoveryProof rule must be exercised on REAL release data, not only in the unit fixtures: the
	// committed bundle carries at least one case with a NON-EMPTY recovery_claim AND a recovery_proof
	// block (parsed, not just key-string present). A clean release with no live claim would silently not
	// test the rule this release exists to enforce — and since it verified clean above, that claim's proof
	// is complete.
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("read recovery-0.1.0 manifest: %v", err)
	}
	var parsed struct {
		Cases []struct {
			RecoveryClaim string          `json:"recovery_claim"`
			RecoveryProof json.RawMessage `json:"recovery_proof"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("decode recovery-0.1.0 manifest: %v", err)
	}
	claimed := 0
	for _, c := range parsed.Cases {
		if c.RecoveryClaim != "" && len(c.RecoveryProof) > 0 {
			claimed++
		}
	}
	if claimed == 0 {
		t.Fatal("recovery-0.1.0 carries no case with a non-empty recovery_claim + recovery_proof: the release does not exercise the §26.12 rule it exists to enforce")
	}
}
