package automation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/palgroup/palai/tests/uat"
)

// TestAutomationReleaseVerifiesClean wires the automation-0.1.0 bundle into the shared evidence verifier:
// the committed release must verify clean (0 failed, 0 missing, 0 secret findings) with ALL FOUR automation
// rules ACTIVE on real data — a duplicated event linked to a single canonical action (dedupe), a single
// canonical scheduler occurrence (occurrence), a callback delivered exactly once (callback), and the run's
// §26.12 RecoveryProof (recovery). This is the deterministic mirror of `make evidence-verify
// RELEASE=automation-0.1.0`; the gated live tier overwrites the same bundle with real chatcmpl ids.
func TestAutomationReleaseVerifiesClean(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, "evidence", "releases", "automation-0.1.0")

	summary, err := uat.VerifyRelease(dir, nil)
	if err != nil {
		t.Fatalf("verify automation-0.1.0: %v", err)
	}
	if !summary.OK() {
		t.Fatalf("automation-0.1.0 did not verify clean: %s\n%v", summary.String(), summary.Findings)
	}
	if summary.Passed == 0 {
		t.Fatalf("automation-0.1.0 verified zero cases; expected the dedupe/occurrence/callback/recovery cases (%s)", summary.String())
	}
	if summary.SecretFindings != 0 {
		t.Fatalf("automation-0.1.0 leaked a credential: %d secret findings", summary.SecretFindings)
	}

	// Each of the four automation rules must be exercised on REAL release data, not only in the unit
	// fixtures: the committed bundle carries at least one case with a non-empty claim AND its proof block
	// (parsed, not just key-string present) for each rule. A clean release with no live claim would silently
	// not test the rule — and since it verified clean above, each claim's proof is complete.
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("read automation-0.1.0 manifest: %v", err)
	}
	var parsed struct {
		Cases []struct {
			DedupeClaim     string          `json:"dedupe_claim"`
			DedupeProof     json.RawMessage `json:"dedupe_proof"`
			OccurrenceClaim string          `json:"occurrence_claim"`
			OccurrenceProof json.RawMessage `json:"occurrence_proof"`
			CallbackClaim   string          `json:"callback_claim"`
			CallbackProof   json.RawMessage `json:"callback_proof"`
			RecoveryClaim   string          `json:"recovery_claim"`
			RecoveryProof   json.RawMessage `json:"recovery_proof"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("decode automation-0.1.0 manifest: %v", err)
	}
	var dedupe, occurrence, callback, recovery int
	for _, c := range parsed.Cases {
		if c.DedupeClaim != "" && len(c.DedupeProof) > 0 {
			dedupe++
		}
		if c.OccurrenceClaim != "" && len(c.OccurrenceProof) > 0 {
			occurrence++
		}
		if c.CallbackClaim != "" && len(c.CallbackProof) > 0 {
			callback++
		}
		if c.RecoveryClaim != "" && len(c.RecoveryProof) > 0 {
			recovery++
		}
	}
	if dedupe == 0 || occurrence == 0 || callback == 0 || recovery == 0 {
		t.Fatalf("automation-0.1.0 does not exercise all four rules: dedupe=%d occurrence=%d callback=%d recovery=%d (a release must exercise every rule it exists to enforce)",
			dedupe, occurrence, callback, recovery)
	}
}
