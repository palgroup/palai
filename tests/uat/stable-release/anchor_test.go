package stablerelease

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/palgroup/palai/tests/uat"
)

// The E18 T10 ANTI-FABRICATION ANCHORS — the RED half of the exit gate.
//
// Every negative here hands the SHIPPED verifier a shape-consistent manifest that hand-writes something it
// did not earn, and asserts it is REFUSED. A gate that has never refused is not a gate, and this epic
// family has produced five green-by-skip findings, so each case below drives the real committed bundle
// (mutated in memory) rather than a hand-built fixture that could drift into agreeing with itself.

// committedManifest reads the committed RC bundle and decodes it into a generic map so a test can mutate
// exactly one field and hand the bytes back to the verifier.
func committedManifest(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "evidence", "releases", uat.StableReleaseBundle, "manifest.json"))
	if err != nil {
		t.Fatalf("read the committed bundle: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode the committed bundle: %v", err)
	}
	return m
}

func marshal(t *testing.T, m map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return raw
}

// caseWith returns the first case carrying the named claim field, so a mutation targets the entry that
// actually holds the proof rather than a positional guess.
func caseWith(t *testing.T, m map[string]any, claim string) map[string]any {
	t.Helper()
	for _, raw := range m["cases"].([]any) {
		c := raw.(map[string]any)
		if v, ok := c[claim]; ok && v != "" {
			return c
		}
	}
	t.Fatalf("no case carries %q", claim)
	return nil
}

func findingsMention(findings []uat.Finding, substr string) bool {
	for _, f := range findings {
		if strings.Contains(f.Detail, substr) {
			return true
		}
	}
	return false
}

// refuse drives VerifyManifest over a mutated bundle and asserts it was refused for the right reason.
func refuse(t *testing.T, what string, m map[string]any, wantDetail string) {
	t.Helper()
	findings := uat.VerifyManifest(marshal(t, m), nil)
	if len(findings) == 0 {
		t.Fatalf("the verifier ACCEPTED %s — the anchor is vacuous", what)
	}
	if !findingsMention(findings, wantDetail) {
		t.Fatalf("%s was refused, but not for the expected reason (want a finding mentioning %q):\n%v", what, wantDetail, findings)
	}
	t.Logf("REFUSED %s", what)
}

// TestTheCommittedBundleIsCleanBeforeAnyMutation is the control. Without it every negative below could be
// passing because the bundle was already broken.
func TestTheCommittedBundleIsCleanBeforeAnyMutation(t *testing.T) {
	if findings := uat.VerifyManifest(marshal(t, committedManifest(t)), nil); len(findings) != 0 {
		t.Fatalf("the committed bundle does not verify clean before mutation: %v", findings)
	}
}

// --- the release index -----------------------------------------------------------------------------------

func TestIndexAnchorRefusesAFabricatedOutcome(t *testing.T) {
	m := committedManifest(t)
	proof := caseWith(t, m, "release_index_claim")["release_index_proof"].(map[string]any)
	entries := proof["entries"].([]any)
	// Find a bundle-carried row and rewrite the bundle that carries it.
	for _, raw := range entries {
		e := raw.(map[string]any)
		if e["disposition"] == uat.DispositionBundleCarried {
			e["bundle"] = "a-bundle-that-does-not-exist-0.0.0"
			break
		}
	}
	refuse(t, "an index row naming a bundle that does not carry it", m, "RE-GATHERED from the per-bundle manifests")
}

func TestIndexAnchorRefusesAnUpgradedDisposition(t *testing.T) {
	m := committedManifest(t)
	proof := caseWith(t, m, "release_index_claim")["release_index_proof"].(map[string]any)
	for _, raw := range proof["entries"].([]any) {
		e := raw.(map[string]any)
		if e["disposition"] == uat.DispositionUnmaterialized {
			// The tempting lie: an id nothing proves, indexed as if a bundle carried it green.
			e["disposition"] = uat.DispositionBundleCarried
			e["bundle"] = "extensions-0.1.0"
			e["outcome"] = "PASS"
			break
		}
	}
	refuse(t, "an unmaterialized id upgraded to bundle-carried PASS", m, "RE-GATHERED from the per-bundle manifests")
}

func TestIndexAnchorRefusesADroppedID(t *testing.T) {
	m := committedManifest(t)
	proof := caseWith(t, m, "release_index_claim")["release_index_proof"].(map[string]any)
	entries := proof["entries"].([]any)
	proof["entries"] = entries[1:]
	refuse(t, "an index that dropped an Appendix-A id", m, "release_index_proof is incomplete")
}

func TestIndexAnchorRefusesAFabricatedAnchor(t *testing.T) {
	m := committedManifest(t)
	proof := caseWith(t, m, "release_index_claim")["release_index_proof"].(map[string]any)
	proof["index_anchor"] = "sha256:" + strings.Repeat("a", 64)
	refuse(t, "a fabricated index anchor", m, "does not reproduce from the recomputed index")
}

func TestIndexAnchorRefusesAnEditedRCBlockerCount(t *testing.T) {
	m := committedManifest(t)
	proof := caseWith(t, m, "release_index_claim")["release_index_proof"].(map[string]any)
	proof["rc_blockers"] = float64(7)
	refuse(t, "an rc_blockers count the triage table does not declare", m, "the count is read from the TABLE")
}

// TestIndexAnchorRefusesAnUpgradedChecklistItem is the §64.15 posture's own negative: a checklist item
// declared `evidenced` when the recompute says otherwise is the exact shape of a release rounding its own
// posture up.
func TestIndexAnchorRefusesAnUpgradedChecklistItem(t *testing.T) {
	m := committedManifest(t)
	proof := caseWith(t, m, "release_index_claim")["release_index_proof"].(map[string]any)
	for _, raw := range proof["checklist"].([]any) {
		item := raw.(map[string]any)
		if item["status"] != uat.ChecklistEvidenced && item["status"] != uat.ChecklistNotClaimed {
			item["status"] = uat.ChecklistEvidenced
			delete(item, "missing")
			break
		}
	}
	refuse(t, "a §64.15 checklist item rounded up to evidenced", m, "is a FUNCTION of the index, never a declaration")
}

// TestIndexAnchorRefusesAManagedItemPromoted: the two managed-scope items must stay `not-claimed`. A
// release that promoted one would be claiming a topology this program never had.
func TestIndexAnchorRefusesAManagedItemPromoted(t *testing.T) {
	m := committedManifest(t)
	proof := caseWith(t, m, "release_index_claim")["release_index_proof"].(map[string]any)
	promoted := false
	for _, raw := range proof["checklist"].([]any) {
		item := raw.(map[string]any)
		if item["status"] == uat.ChecklistNotClaimed {
			item["status"] = uat.ChecklistEvidenced
			promoted = true
			break
		}
	}
	if !promoted {
		t.Fatal("no §64.15 item closes not-claimed — the managed-scope split is gone")
	}
	refuse(t, "a managed-scope §64.15 item promoted to evidenced", m, "is a FUNCTION of the index, never a declaration")
}

// TestAnOpenRCBlockerRefusesTheGate drives the clause that cannot be exercised against the committed tree,
// because the tree declares zero. It hands the pure core a NON-ZERO count and asserts the refusal — the
// gate's "zero open P0/P1" is a rule that fires, not a number that happens to be right today.
func TestAnOpenRCBlockerRefusesTheGate(t *testing.T) {
	index, err := uat.RecomputeReleaseIndex()
	if err != nil {
		t.Fatalf("recompute the index: %v", err)
	}
	proof := committedIndexProof(t)
	proof.RCBlockers = 1
	problems := uat.VerifyReleaseIndexAgainst(&proof, index, 1, nil)
	if len(problems) == 0 {
		t.Fatal("an open RC-blocker did NOT refuse the gate — §64.15's zero-open-P0/P1 clause is vacuous")
	}
	if !mentions(problems, "open RC-blocker") {
		t.Fatalf("the refusal does not name the open blocker: %v", problems)
	}
	// ...and the honest zero still passes, so the negative above is not passing for a shared reason.
	proof.RCBlockers = 0
	if problems := uat.VerifyReleaseIndexAgainst(&proof, index, 0, nil); len(problems) != 0 {
		t.Fatalf("the committed index does not verify against its own recompute: %v", problems)
	}
}

// TestAnUnreadableTriageTableFailsClosed: a gate that cannot read the blocker count must REFUSE, never
// default to zero.
func TestAnUnreadableTriageTableFailsClosed(t *testing.T) {
	index, err := uat.RecomputeReleaseIndex()
	if err != nil {
		t.Fatalf("recompute the index: %v", err)
	}
	proof := committedIndexProof(t)
	problems := uat.VerifyReleaseIndexAgainst(&proof, index, 0, os.ErrNotExist)
	if !mentions(problems, "fail closed") {
		t.Fatalf("an unreadable triage table did not fail closed: %v", problems)
	}
}

func mentions(problems []string, substr string) bool {
	for _, p := range problems {
		if strings.Contains(p, substr) {
			return true
		}
	}
	return false
}

// committedIndexProof decodes the committed bundle's ReleaseIndexProof.
func committedIndexProof(t *testing.T) uat.ReleaseIndexProof {
	t.Helper()
	raw, err := json.Marshal(caseWith(t, committedManifest(t), "release_index_claim")["release_index_proof"])
	if err != nil {
		t.Fatal(err)
	}
	var p uat.ReleaseIndexProof
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode the committed index proof: %v", err)
	}
	return p
}

// --- the product-wide posture ------------------------------------------------------------------------------

// TestPostureAnchorRefusesAFabricatedCrossEpicStable is THE sentence of this gate, mechanically: a
// fabricated cross-epic "stable" is a FAIL.
func TestPostureAnchorRefusesAFabricatedCrossEpicStable(t *testing.T) {
	m := committedManifest(t)
	proof := caseWith(t, m, "aggregate_tier_claim")["aggregate_tier_proof"].(map[string]any)
	flipped := false
	for _, raw := range proof["capabilities"].([]any) {
		d := raw.(map[string]any)
		if d["declared_tier"] != "stable" {
			d["declared_tier"] = "stable"
			flipped = true
			break
		}
	}
	if !flipped {
		t.Fatal("every capability already declares stable — the posture is not what this gate was built for")
	}
	refuse(t, "a hand-written cross-epic \"stable\"", m, "a fabricated cross-epic tier is a FAIL")
}

func TestPostureAnchorRefusesADivergingSnapshot(t *testing.T) {
	m := committedManifest(t)
	proof := caseWith(t, m, "aggregate_tier_claim")["aggregate_tier_proof"].(map[string]any)
	snapshot := proof["snapshot"].(map[string]any)
	for name, tier := range snapshot {
		if tier != "stable" {
			snapshot[name] = "stable"
			break
		}
	}
	refuse(t, "a /v1/capabilities snapshot that disagrees with the recompute", m, "must be BIT-EQUAL to the recomputed posture")
}

// TestPostureAnchorRefusesADeployedClaim is EXT-1's fence: the E18 T9 triage said this proof must NOT claim
// a deployed binary serves the map, and this is that sentence refusing.
func TestPostureAnchorRefusesADeployedClaim(t *testing.T) {
	m := committedManifest(t)
	proof := caseWith(t, m, "aggregate_tier_claim")["aggregate_tier_proof"].(map[string]any)
	proof["served_by_deployed_config"] = true
	refuse(t, "a claim that a DEPLOYED config serves the capability map (EXT-1)", m, "aggregate_tier_proof is incomplete")
}

func TestPostureAnchorRefusesAnUnnamedSnapshotSource(t *testing.T) {
	m := committedManifest(t)
	proof := caseWith(t, m, "aggregate_tier_claim")["aggregate_tier_proof"].(map[string]any)
	// The wording EXT-1 objected to: true as far as it goes, and it invites the deployed reading.
	proof["snapshot_source"] = "GET /v1/capabilities served by the real api.NewRouter"
	refuse(t, "a snapshot source that does not NAME the fully-mounted router (EXT-1)", m, "aggregate_tier_proof is incomplete")
}

func TestPostureAnchorRefusesADroppedUnmountedReason(t *testing.T) {
	m := committedManifest(t)
	proof := caseWith(t, m, "aggregate_tier_claim")["aggregate_tier_proof"].(map[string]any)
	proof["unmounted_reason"] = "the capability-worker gateway is not always mounted"
	refuse(t, "an unmounted reason that does not name the env var no shipped config sets", m, "aggregate_tier_proof is incomplete")
}

func TestPostureAnchorRefusesAShrunkenClaimLedger(t *testing.T) {
	m := committedManifest(t)
	proof := caseWith(t, m, "aggregate_tier_claim")["aggregate_tier_proof"].(map[string]any)
	for _, raw := range proof["capabilities"].([]any) {
		d := raw.(map[string]any)
		ids := d["claim_case_ids"].([]any)
		if len(ids) > 1 {
			d["claim_case_ids"] = ids[:1] // hide a red case by shrinking the ledger it is judged over
			break
		}
	}
	refuse(t, "a shrunken capability claim ledger", m, "aggregate_tier_proof is incomplete")
}

// --- the family markers ------------------------------------------------------------------------------------

// TestDroppingTheIndexMarkerIsRefused is the load-bearing structural negative: both cross-bundle recomputes
// only run for a case whose marker is non-empty, so a bundle that DROPPED the marker while keeping the proof
// body would verify 0 findings with the anchor silently switched off.
func TestDroppingTheIndexMarkerIsRefused(t *testing.T) {
	for _, marker := range []string{"release_index_claim", "aggregate_tier_claim"} {
		m := committedManifest(t)
		delete(caseWith(t, m, marker), marker)
		refuse(t, "a bundle that dropped its "+marker+" marker", m, marker)
	}
}

func TestTwoIndexTablesInOneManifestAreRefused(t *testing.T) {
	for _, marker := range []string{"release_index_claim", "aggregate_tier_claim"} {
		m := committedManifest(t)
		original := caseWith(t, m, marker)
		clone := map[string]any{}
		for k, v := range original {
			clone[k] = v
		}
		clone["id"] = original["id"].(string) + "-SECOND"
		m["cases"] = append(m["cases"].([]any), clone)
		refuse(t, "two "+marker+"s in one manifest", m, "want exactly 1")
	}
}

// --- the four per-case proofs --------------------------------------------------------------------------------

func TestSupplyChainProofRefusalMatrix(t *testing.T) {
	for _, tc := range []struct {
		what   string
		mutate func(map[string]any)
	}{
		{"an UNNAMED release directory (SUP-3's rule)", func(p map[string]any) { p["release_dir"] = "" }},
		{"a claimed transparency-log entry", func(p map[string]any) { p["transparency_log"] = true }},
		{"a CI workflow identity as the builder", func(p map[string]any) {
			p["provenance_builder"] = "https://github.com/palgroup/palai/.github/workflows/release.yml@refs/tags/v1.0.0"
		}},
		{"a signer that is not the one openssl signer", func(p map[string]any) { p["signature_algorithm"] = "cosign-keyless" }},
		{"the word Sigstore in the offline evidence", func(p map[string]any) {
			p["offline_evidence"] = "verified offline against the Sigstore transparency log"
		}},
		{"a five-arm tamper matrix", func(p map[string]any) {
			arms := p["tamper_arms"].([]any)
			p["tamper_arms"] = arms[:len(arms)-1]
			p["tamper_rejected"] = float64(len(arms) - 1)
		}},
		{"a tamper arm that was not rejected", func(p map[string]any) { p["tamper_rejected"] = float64(5) }},
		{"a verify that was not offline", func(p map[string]any) { p["offline_verified"] = false }},
		{"one SBOM format where §51.2 requires two", func(p map[string]any) { p["sbom_formats"] = []any{"spdx"} }},
		{"a duplicated artifact digest inflating the set", func(p map[string]any) {
			d := p["artifact_digests"].([]any)
			p["artifact_digests"] = []any{d[0], d[0]}
		}},
	} {
		m := committedManifest(t)
		tc.mutate(caseWith(t, m, "supply_chain_claim")["supply_chain_proof"].(map[string]any))
		refuse(t, "a supply-chain proof with "+tc.what, m, "supply_chain_proof is incomplete")
	}
}

func TestPerformanceProofRefusalMatrix(t *testing.T) {
	firstMetric := func(p map[string]any) map[string]any {
		return p["metrics"].([]any)[0].(map[string]any)
	}
	for _, tc := range []struct {
		what   string
		mutate func(map[string]any)
	}{
		{"no machine in the profile", func(p map[string]any) { p["machine"] = "" }},
		{"a whitespace load shape", func(p map[string]any) { p["load_shape"] = "   " }},
		{"no Docker version", func(p map[string]any) { p["docker"] = "" }},
		{"a dropped honest ceiling", func(p map[string]any) { p["ceiling"] = "" }},
		{"a dropped no-SLO stamp", func(p map[string]any) { p["no_slo_claim"] = false }},
		{"a reference-hardware claim", func(p map[string]any) { p["reference_hardware"] = true }},
		{"a percentile method that is not the harness's", func(p map[string]any) { p["percentile_method"] = "linear interpolation" }},
		{"a FABRICATED p95", func(p map[string]any) { firstMetric(p)["p95"] = float64(1) }},
		{"a fabricated gate value", func(p map[string]any) { firstMetric(p)["gate_value"] = float64(1) }},
		{"a pass verdict the samples do not support", func(p map[string]any) {
			mm := firstMetric(p)
			mm["gate_max"] = float64(1) // every sample now exceeds it
		}},
		{"a metric with its raw samples removed", func(p map[string]any) { firstMetric(p)["samples"] = []any{} }},
		{"a sample count larger than the samples carried", func(p map[string]any) {
			p["sample_count"] = p["sample_count"].(float64) + 100
		}},
		{"a run sample count smaller than the part carried", func(p map[string]any) { p["run_sample_count"] = float64(1) }},
	} {
		m := committedManifest(t)
		tc.mutate(caseWith(t, m, "performance_profile_claim")["performance_profile_proof"].(map[string]any))
		refuse(t, "a performance proof with "+tc.what, m, "performance_profile_proof is incomplete")
	}
}

// TestSandboxEscapeProofRefusalMatrix drives uat.SandboxEscapeProof.Complete() directly rather than through
// the committed bundle, because SEC-102 is DELIBERATELY ABSENT from it: `make uat-escape` reports
// no_escape=false at this commit (the SAN-006 arm's test skips for want of a Postgres URL its harness never
// supplies), so there is no honest proof to mutate. The type still has to refuse every shape a future green
// run could get wrong, and the FIRST case below is the one this session actually met.
func TestSandboxEscapeProofRefusalMatrix(t *testing.T) {
	good := func() uat.SandboxEscapeProof {
		return uat.SandboxEscapeProof{
			Arms: []string{
				"file-tool-confinement", "oci-sandbox-isolation", "cgroup-resource-exhaustion",
				"snapshot-integrity-and-secret-exclusion", "host-kill-fences-stale-writer",
				"allocation-hygiene-and-substrate-quarantine", "runner-cordon-drain-revoke",
				"uncertain-failure-job-quarantine",
			},
			CasesCovered:   append([]string(nil), uat.SandboxEscapeSuiteCases...),
			CasesUnowned:   append([]string(nil), uat.SandboxEscapeUnownedCases...),
			QuarantineArms: append([]string(nil), uat.SandboxEscapeQuarantineArms...),
			NoEscape:       true, QuarantineWorks: true, LocalOCIOnly: true,
		}
	}
	if !good().Complete() {
		t.Fatal("the reference escape proof is not complete — every negative below would pass for the wrong reason")
	}

	for _, tc := range []struct {
		what   string
		mutate func(*uat.SandboxEscapeProof)
	}{
		// THE ONE THIS SESSION MET: the suite really did report an arm as NOT ATTEMPTED, and a proof that
		// could carry that alongside no_escape=true would have let it through.
		{"an arm that was NOT ATTEMPTED (SAN-006 skipped for want of a Postgres URL)", func(p *uat.SandboxEscapeProof) {
			p.ArmsNotAttempted = []string{"host-kill-fences-stale-writer"}
		}},
		{"a shrunken covered-case set", func(p *uat.SandboxEscapeProof) {
			p.CasesCovered = p.CasesCovered[:len(p.CasesCovered)-1]
		}},
		{"the unowned SAN ids quietly dropped", func(p *uat.SandboxEscapeProof) { p.CasesUnowned = nil }},
		{"a quarantine arm that is not in the suite", func(p *uat.SandboxEscapeProof) {
			kept := []string{}
			for _, a := range p.Arms {
				if a != "uncertain-failure-job-quarantine" {
					kept = append(kept, a)
				}
			}
			p.Arms = kept
		}},
		{"quarantine_works declared over a failure", func(p *uat.SandboxEscapeProof) {
			p.Failures = []string{"an arm failed"}
		}},
		{"no_escape false", func(p *uat.SandboxEscapeProof) { p.NoEscape = false }},
		{"quarantine_works false", func(p *uat.SandboxEscapeProof) { p.QuarantineWorks = false }},
		{"a microVM claim (local_oci_only false)", func(p *uat.SandboxEscapeProof) { p.LocalOCIOnly = false }},
	} {
		p := good()
		tc.mutate(&p)
		if p.Complete() {
			t.Errorf("SandboxEscapeProof.Complete() ACCEPTED %s", tc.what)
			continue
		}
		t.Logf("REFUSED an escape-suite proof with %s", tc.what)
	}
}

// TestNoBundleCarriesAnEscapeClaim pins the deliberate absence. If a future session adds SEC-102 back with a
// SandboxEscapeProof, this fails and forces the author to confirm the suite really went green — which is the
// whole point of removing it rather than quietly weakening the proof type.
func TestNoBundleCarriesAnEscapeClaim(t *testing.T) {
	for _, raw := range committedManifest(t)["cases"].([]any) {
		c := raw.(map[string]any)
		if v, ok := c["sandbox_escape_claim"]; ok && v != "" {
			t.Fatalf("%v carries a sandbox_escape_claim — `make uat-escape` reports no_escape=false at this "+
				"commit (ESC-1 in the RC triage). Re-add it only with a suite run that reports no_escape=true "+
				"and an EMPTY arms_not_attempted", c["id"])
		}
	}
}

func TestAuditIntegrityProofRefusalMatrix(t *testing.T) {
	for _, tc := range []struct {
		what   string
		mutate func(map[string]any)
	}{
		{"a recomputed head that does not equal the checkpoint's", func(p map[string]any) {
			p["recomputed_head"] = "sha256:" + strings.Repeat("b", 64)
		}},
		{"only three of the four typed alerts", func(p map[string]any) {
			a := p["alerts_raised"].([]any)
			p["alerts_raised"] = a[:3]
		}},
		{"a checkpoint claimed to live inside the mutable store", func(p map[string]any) {
			p["checkpoint_outside_store"] = false
		}},
		{"the AUD-1 purge admission DENIED", func(p map[string]any) {
			p["purge_indistinguishable_from_tamper"] = false
		}},
		{"zero anchored rows", func(p map[string]any) { p["anchored_rows"] = float64(0) }},
	} {
		m := committedManifest(t)
		tc.mutate(caseWith(t, m, "audit_integrity_claim")["audit_integrity_proof"].(map[string]any))
		refuse(t, "an audit-integrity proof with "+tc.what, m, "audit_integrity_proof is incomplete")
	}
}

// TestAFabricatedChecksumInThisBundleIsCaught proves the RC bundle's own anchor is load-bearing: its case
// checksums recompute from the RELEASE INDEX, which is derived from OTHER committed bytes, so a
// shape-valid value that reproduces nothing fails here exactly as E18 T8's sweep demands of a new bundle.
func TestAFabricatedChecksumInThisBundleIsCaught(t *testing.T) {
	m := committedManifest(t)
	m["cases"].([]any)[0].(map[string]any)["checksum"] = "sha256:" + strings.Repeat("f", 64)
	refuse(t, "a fabricated checksum in the RC bundle", m, "does not recompute from its canonical surface")
}
