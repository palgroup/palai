package recovery

import (
	"os"
	"path/filepath"
	"strings"
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
	// committed bundle carries at least one recovery claim + its §26.12 proof. A clean release with no
	// recovery claim would silently not test the rule this release exists to enforce.
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("read recovery-0.1.0 manifest: %v", err)
	}
	body := string(raw)
	if !strings.Contains(body, `"recovery_claim"`) || !strings.Contains(body, `"recovery_proof"`) {
		t.Fatal("recovery-0.1.0 carries no recovery_claim/recovery_proof: the release does not exercise the §26.12 rule it exists to enforce")
	}
}
