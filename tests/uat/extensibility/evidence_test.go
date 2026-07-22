package extensibility

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/palgroup/palai/tests/uat"
)

// hashOf reproduces the journey's hashCoding (sha256 of each part followed by a NUL, hex-encoded) so this
// gate can recompute an advertised_schema_hash from the manifest's own tool_names — a committed hash that
// does not reproduce is fabricated. ponytail: a 4-line copy, not a shared export (hashCoding is e2e-tagged).
func hashOf(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// TestExtensibilityReleaseVerifiesClean wires the extensibility-0.1.0 bundle into the shared evidence
// verifier: the committed release must verify clean (0 failed, 0 missing, 0 secret findings) with the E12
// extension rules ACTIVE on real data — the run's effective tool set advertised to the provider
// (advertising), an enabled skill pinned by digest + scan with no authority (skill), a signed remote-tool
// callback delivered exactly once (callback), and an extension crash ISOLATED to the breaker + tool_unavailable
// while the control-plane stayed up and another run flowed (crash-isolation, the EXT-005 exit gate). This is
// the deterministic mirror of `make evidence-verify RELEASE=extensibility-0.1.0`; the gated live tier
// overwrites the same bundle with real chatcmpl ids. It fails (bundle absent) until G5 commits it.
func TestExtensibilityReleaseVerifiesClean(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, "evidence", "releases", "extensibility-0.1.0")

	summary, err := uat.VerifyRelease(dir, nil)
	if err != nil {
		t.Fatalf("verify extensibility-0.1.0: %v", err)
	}
	if !summary.OK() {
		t.Fatalf("extensibility-0.1.0 did not verify clean: %s\n%v", summary.String(), summary.Findings)
	}
	if summary.Passed == 0 {
		t.Fatalf("extensibility-0.1.0 verified zero cases; expected the advertising/skill/callback/crash-isolation cases (%s)", summary.String())
	}
	if summary.SecretFindings != 0 {
		t.Fatalf("extensibility-0.1.0 leaked a credential: %d secret findings", summary.SecretFindings)
	}

	// Each E12 rule must be exercised on REAL release data, not only in the unit fixtures: the committed
	// bundle carries at least one case with a non-empty claim AND its proof block (parsed, not just the
	// key-string present) for each rule. An EXT-005-claimless release would silently not test crash
	// isolation — the E12 exit gate — so a bundle missing any rule fails here (the automation
	// four-rules-exercised loop copy). Since it verified clean above, each present claim's proof is complete.
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("read extensibility-0.1.0 manifest: %v", err)
	}
	var parsed struct {
		Cases []struct {
			AdvertisingClaim string `json:"advertising_claim"`
			AdvertisingProof *struct {
				AdvertisedSchemaHash string   `json:"advertised_schema_hash"`
				ToolNames            []string `json:"tool_names"`
			} `json:"advertising_proof"`
			SkillClaim          string          `json:"skill_claim"`
			SkillProof          json.RawMessage `json:"skill_proof"`
			CallbackClaim       string          `json:"callback_claim"`
			CallbackProof       json.RawMessage `json:"callback_proof"`
			CrashIsolationClaim string          `json:"crash_isolation_claim"`
			CrashIsolationProof json.RawMessage `json:"crash_isolation_proof"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("decode extensibility-0.1.0 manifest: %v", err)
	}
	var advertising, skill, callback, crashIsolation int
	for _, c := range parsed.Cases {
		if c.AdvertisingClaim != "" && c.AdvertisingProof != nil {
			advertising++
			// Anti-fabrication: the advertised_schema_hash MUST be the real hash of the manifest's own
			// tool_names (the journey's hashCoding over its advertised set) — a value that does not reproduce
			// from the tool_names is a fabricated hash, the exact defect the shape-checked verifier can't see.
			if got := hashOf(c.AdvertisingProof.ToolNames...); got != c.AdvertisingProof.AdvertisedSchemaHash {
				t.Fatalf("advertised_schema_hash %q is not the hash of the manifest's tool_names %v (want %q) — a committed hash must be the real value the journey produces",
					c.AdvertisingProof.AdvertisedSchemaHash, c.AdvertisingProof.ToolNames, got)
			}
		}
		if c.SkillClaim != "" && len(c.SkillProof) > 0 {
			skill++
		}
		if c.CallbackClaim != "" && len(c.CallbackProof) > 0 {
			callback++
		}
		if c.CrashIsolationClaim != "" && len(c.CrashIsolationProof) > 0 {
			crashIsolation++
		}
	}
	if advertising == 0 || skill == 0 || callback == 0 || crashIsolation == 0 {
		t.Fatalf("extensibility-0.1.0 does not exercise all E12 rules: advertising=%d skill=%d callback=%d crash-isolation=%d (no PASS without EXT-005 crash-isolation proof)",
			advertising, skill, callback, crashIsolation)
	}
}
