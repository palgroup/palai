// Package extensions holds the E17 EXIT gate's Docker-free half: the UAT catalog gate, the
// extensions-0.1.0 evidence-release verification, and — the crown — the CapabilityTierProof
// ANTI-FABRICATION ANCHOR. All of it rides `make verify` (no Docker, no credential); the three
// integration journeys (§63.3 Slack on the fake peer, knowledge, worker) are the uat-tagged
// orchestrator in journey_test.go.
//
// The anchor's shape is the E13/E14/E15 MUST-FIX-1 discipline: an evidence bundle's load-bearing
// number must be RECOMPUTED from a canonical source, never read from the manifest's own copy. Here
// the number is a capability's MATURITY TIER and the canonical source is the bundle's per-case
// OUTCOMES plus the code tables in tests/uat/evidence.go. The tests below are deliberately written
// as NEGATIVES first: each hands the verifier a manifest that is perfectly shape-consistent and
// hand-writes a tier it did not earn, and asserts the verifier REFUSES it. A gate that only checks
// shape passes every one of them, which is exactly the failure mode this file exists to prevent.
package extensions

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/palgroup/palai/tests/evals"
	"github.com/palgroup/palai/tests/uat"
)

// tierCase is one case entry in a synthetic manifest: only the fields the tier recompute and the
// shared verifier read. Written as a Go literal (not a fixture file) so a reader can see the exact
// fabrication each negative performs.
type tierCase struct {
	ID                  string                   `json:"id"`
	Status              string                   `json:"status"`
	ProofClass          string                   `json:"proof_class"`
	RunID               string                   `json:"run_id"`
	ImageDigest         string                   `json:"image_digest"`
	ProviderRequestID   string                   `json:"provider_request_id"`
	MTLSEnroll          string                   `json:"mtls_enroll"`
	Terminal            map[string]any           `json:"terminal"`
	DBAssertions        []string                 `json:"db_assertions"`
	Checksum            string                   `json:"checksum"`
	CapabilityTierClaim string                   `json:"capability_tier_claim,omitempty"`
	CapabilityTierProof *uat.CapabilityTierProof `json:"capability_tier_proof,omitempty"`
	EvalGateClaim       string                   `json:"eval_gate_claim,omitempty"`
	EvalGateProof       *uat.EvalGateProof       `json:"eval_gate_proof,omitempty"`
}

// heldOutEvalProof builds the QUA-004 EvalGateProof from a REAL held-out run of the shipped suites under the
// SafePolicy reference engine, with the gate's own canonical threshold table. ExtensionsPromoteGate composes
// EvalPromoteGate, which RE-RUNS the same suites and refuses any declared number that does not reproduce — so
// this must be a real run, not hand-written figures.
func heldOutEvalProof(t *testing.T) *uat.EvalGateProof {
	t.Helper()
	reports, err := evals.RunAll(filepath.Join("..", "..", "evals", "testdata"), evals.HeldOut, evals.SafePolicy)
	if err != nil {
		t.Fatalf("run the canonical held-out eval suites: %v", err)
	}
	proof := &uat.EvalGateProof{Split: "held-out"}
	for _, suite := range evals.Suites {
		r := reports[suite]
		proof.Suites = append(proof.Suites, uat.EvalSuiteScore{
			Suite: suite, HeldOutScore: r.Score, Threshold: uat.EvalThresholds[suite],
			SecurityRegressions: r.SecurityFailures, DatasetDigest: r.Digest,
		})
	}
	return proof
}

// greenCase is a clean PASS case carrying no E17 claim — the shape every claim case must also satisfy.
func greenCase(id string) tierCase {
	return tierCase{
		ID: id, Status: "PASS", ProofClass: "component-real",
		RunID: "run_" + strings.ToLower(id), ImageDigest: "sha256:" + strings.Repeat("a", 64),
		ProviderRequestID: "det-" + strings.ToLower(id), MTLSEnroll: "enrolled",
		Terminal:     map[string]any{"type": "response.completed", "count": 1},
		DBAssertions: []string{"one canonical row"},
		Checksum:     "sha256:" + strings.Repeat("b", 64),
	}
}

// allClaimCases is every UAT case id the canonical capability ledger owns, plus the QUA-003 security
// precondition — the full set a bundle needs for the four stable candidates to close stable.
func allClaimCases() []string {
	ids := []string{uat.CapabilitySecurityPrecondition}
	seen := map[string]bool{uat.CapabilitySecurityPrecondition: true}
	for _, capability := range uat.CapabilityTierOrder {
		for _, id := range uat.CapabilityClaims[capability] {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// tierManifest builds a shape-consistent manifest whose cases are all PASS except those named in
// `notPass` (mapped to their fabricated status; the empty string DROPS the case entirely, modelling an
// ABSENT claim). declaredTiers/snapshotTiers override the honest recompute so a caller can hand-write a
// tier the outcomes do not support. Everything else — the claim ledger, the ledger digest, the case
// checksums — is left canonical, so a refusal can only come from the recompute, never from a shape slip.
func tierManifest(t *testing.T, notPass map[string]string, declaredTiers, snapshotTiers map[string]string) []byte {
	t.Helper()

	honest := map[string]string{}
	cases := []tierCase{}
	for _, id := range allClaimCases() {
		status, fabricated := notPass[id]
		if fabricated && status == "" {
			continue // an ABSENT claim
		}
		c := greenCase(id)
		if fabricated {
			c.Status = status
		}
		cases = append(cases, c)
		honest[id] = c.Status
	}
	recomputed := uat.RecomputeCapabilityTiers(honest)

	decls := make([]uat.CapabilityTierDeclaration, 0, len(uat.CapabilityTierOrder))
	snapshot := map[string]string{}
	for _, capability := range uat.CapabilityTierOrder {
		tier := recomputed[capability]
		if override, ok := declaredTiers[capability]; ok {
			tier = override
		}
		decls = append(decls, uat.CapabilityTierDeclaration{
			Capability:   capability,
			DeclaredTier: tier,
			ClaimCaseIDs: uat.CapabilityClaims[capability],
		})
		snap := recomputed[capability]
		if override, ok := snapshotTiers[capability]; ok {
			snap = override
		}
		snapshot[capability] = snap
	}

	// QUA-004 carries the held-out eval-gate proof ExtensionsPromoteGate composes. It is not a capability
	// claim (evals are release machinery, not a discovery capability — plan §T6), so it rides alongside.
	evalCase := greenCase("QUA-004")
	evalCase.EvalGateClaim = "thresholds-met"
	evalCase.EvalGateProof = heldOutEvalProof(t)
	cases = append(cases, evalCase)

	anchor := greenCase("EXT-TIER")
	anchor.CapabilityTierClaim = "tiers-recomputed"
	anchor.CapabilityTierProof = &uat.CapabilityTierProof{
		Capabilities:   decls,
		Snapshot:       snapshot,
		SnapshotSource: "GET /v1/capabilities on the journey stack",
		ClaimsDigest:   uat.CapabilityClaimsDigest(),
	}
	cases = append(cases, anchor)

	raw, err := json.Marshal(map[string]any{
		"release": "extensions-0.1.0-anchor-fixture", "git_sha": strings.Repeat("c", 40),
		"api_version": "v1", "migration": "000040", "captured_at": "2026-07-25T00:00:00Z",
		"cases": cases,
	})
	if err != nil {
		t.Fatalf("marshal synthetic manifest: %v", err)
	}
	return raw
}

// tierFindings returns the tier-related findings the shared verifier reports for a manifest.
func tierFindings(raw []byte) []uat.Finding {
	var out []uat.Finding
	for _, f := range uat.VerifyManifest(raw, nil) {
		if strings.Contains(f.Detail, "capability") {
			out = append(out, f)
		}
	}
	return out
}

// TestHonestTierManifestVerifiesClean is the baseline: a manifest whose declared tiers and snapshot BOTH
// equal the recompute over all-green outcomes verifies with no finding. Without this, a gate that refused
// everything would pass the negatives below for the wrong reason.
func TestHonestTierManifestVerifiesClean(t *testing.T) {
	raw := tierManifest(t, nil, nil, nil)
	if findings := uat.VerifyManifest(raw, nil); len(findings) != 0 {
		t.Fatalf("an honest tier manifest must verify clean, got %d finding(s): %v", len(findings), findings)
	}
	if refusals := uat.PromoteGateFor(raw, "rc"); len(refusals) != 0 {
		t.Fatalf("an honest tier manifest must pass the rc promote gate, got %v", refusals)
	}
}

// TestFabricatedStableWithRedClaimIsRefused is THE anchor (plan §T11). The manifest is perfectly
// shape-consistent — canonical capability set, canonical claim ledger, anchored ledger digest, a declared
// tier from the vocabulary, a snapshot entry for every capability — and hand-writes "stable" for
// `knowledge` while KNO-004 is FAIL. Only a verifier that RECOMPUTES the tier from the per-case outcomes
// can catch this; a shape-only gate passes it. Both the manifest verifier and the promote gate must refuse.
func TestFabricatedStableWithRedClaimIsRefused(t *testing.T) {
	raw := tierManifest(t,
		map[string]string{"KNO-004": "FAIL"},
		map[string]string{"knowledge": "stable"},
		map[string]string{"knowledge": "stable"})

	findings := tierFindings(raw)
	if len(findings) == 0 {
		t.Fatal("a hand-written \"stable\" for a capability with a FAILED claim (KNO-004) was ACCEPTED — the verifier is reading the manifest's own tier copy instead of recomputing from the per-case outcomes (plan §T11 anchor)")
	}
	joined := renderFindings(findings)
	if !strings.Contains(joined, "knowledge") || !strings.Contains(joined, "KNO-004") {
		t.Errorf("the refusal must name the capability AND the red claim so an operator knows what is outstanding, got: %s", joined)
	}
	if refusals := uat.PromoteGateFor(raw, "rc"); len(refusals) == 0 {
		t.Fatal("the promote gate ACCEPTED a fabricated stable tier — the recompute must be hoisted into the gate, not only the verifier (plan §T11)")
	}
}

// TestFabricatedStableWithAbsentClaimIsRefused is the anchor's second half: a capability cannot reach
// stable by simply OMITTING one of its claims from the bundle. The claim ledger stays canonical (so the
// declaration cannot shrink), UI-002 is dropped from the cases, and "stable" is hand-written for `console`.
func TestFabricatedStableWithAbsentClaimIsRefused(t *testing.T) {
	raw := tierManifest(t,
		map[string]string{"UI-002": ""},
		map[string]string{"console": "stable"},
		map[string]string{"console": "stable"})

	findings := tierFindings(raw)
	if len(findings) == 0 {
		t.Fatal("a hand-written \"stable\" for a capability whose claim UI-002 is ABSENT from the bundle was ACCEPTED — an omitted claim must never count as green (plan §T11 anchor)")
	}
	if joined := renderFindings(findings); !strings.Contains(joined, "ABSENT") {
		t.Errorf("the refusal must say the claim is ABSENT, got: %s", joined)
	}
	if refusals := uat.PromoteGateFor(raw, "rc"); len(refusals) == 0 {
		t.Fatal("the promote gate ACCEPTED a capability whose claim is absent from the bundle")
	}
}

// TestFabricatedStableForAnOperatorLegCapabilityIsRefused pins the §6 honesty rule mechanically: `slack`
// and `a2a` have every LOCAL claim green, yet their stable flip awaits a real workspace / a foreign peer.
// Declaring them stable off green local claims is the precise overclaim this epic's honest ceiling forbids.
func TestFabricatedStableForAnOperatorLegCapabilityIsRefused(t *testing.T) {
	for _, capability := range []string{"slack", "a2a"} {
		raw := tierManifest(t, nil,
			map[string]string{capability: "stable"},
			map[string]string{capability: "stable"})
		findings := tierFindings(raw)
		if len(findings) == 0 {
			t.Fatalf("%q was declared stable off GREEN LOCAL claims and ACCEPTED — the §6 operator leg must cap it at preview (plan §6, §T11)", capability)
		}
		if joined := renderFindings(findings); !strings.Contains(joined, "§6 leg") {
			t.Errorf("%s: the refusal must name the outstanding §6 leg, got: %s", capability, joined)
		}
	}
}

// TestFabricatedStableForAnUnservableCapabilityIsRefused: knowledge-vector and apple-build have no backing
// store / no signing material, so no claim outcome can advertise them. Discovery never claims what the
// deployment cannot serve (plan §2).
func TestFabricatedStableForAnUnservableCapabilityIsRefused(t *testing.T) {
	for _, capability := range []string{"knowledge-vector", "apple-build"} {
		for _, tier := range []string{"stable", "preview"} {
			raw := tierManifest(t, nil,
				map[string]string{capability: tier},
				map[string]string{capability: tier})
			if findings := tierFindings(raw); len(findings) == 0 {
				t.Fatalf("%q declared %q was ACCEPTED — an unservable capability must recompute to disabled (plan §2)", capability, tier)
			}
		}
	}
}

// TestSnapshotDivergingFromTheRecomputeIsRefused is the BIT-EQUALITY half: the declarations may be honest
// while the RUNNING stack advertises something else. Discovery drifting from the verifier is exactly how a
// deployment ends up serving "stable" for a capability the evidence never earned, so it must fail.
func TestSnapshotDivergingFromTheRecomputeIsRefused(t *testing.T) {
	raw := tierManifest(t, nil, nil, map[string]string{"knowledge": "stable-ish"})
	findings := tierFindings(raw)
	if len(findings) == 0 {
		t.Fatal("a /v1/capabilities snapshot that DIVERGES from the recomputed tier table was ACCEPTED — the snapshot must be bit-equal to the recompute (plan §T11)")
	}
	if joined := renderFindings(findings); !strings.Contains(joined, "BIT-EQUAL") {
		t.Errorf("the refusal must name the bit-equality invariant, got: %s", joined)
	}
}

// TestRedSecurityPreconditionCapsEveryCapability: QUA-003 (the eval security/red-team suite) is the
// precondition for ANY stable flip (plan §T6/§T11) — the red-team surface runs across all four extension
// areas, so a regression there invalidates every stable claim regardless of that capability's own cases.
func TestRedSecurityPreconditionCapsEveryCapability(t *testing.T) {
	honest := map[string]string{}
	for _, id := range allClaimCases() {
		honest[id] = "PASS"
	}
	honest[uat.CapabilitySecurityPrecondition] = "FAIL"
	for _, capability := range uat.CapabilityTierOrder {
		got := uat.RecomputeCapabilityTier(capability, honest)
		if got == "stable" {
			t.Errorf("with %s FAILED, capability %q still recomputed stable — the security suite is a precondition for every flip", uat.CapabilitySecurityPrecondition, capability)
		}
	}

	raw := tierManifest(t,
		map[string]string{uat.CapabilitySecurityPrecondition: "FAIL"},
		map[string]string{"knowledge": "stable"},
		map[string]string{"knowledge": "stable"})
	if findings := tierFindings(raw); len(findings) == 0 {
		t.Fatal("a stable tier declared with a FAILED eval security suite was ACCEPTED (plan §T6, §T11)")
	}
	if refusals := uat.PromoteGateFor(raw, "rc"); len(refusals) == 0 {
		t.Fatal("the promote gate ACCEPTED a tag whose eval security suite is not green — plan §T11 requires the REFUSE")
	}
}

// TestShrunkenClaimLedgerIsRefused closes the last fabrication route: rather than lie about a tier, a
// bundle could declare that `knowledge` owns only its green claims. The claim ledger is anchored to the
// CODE table, so a shrunken (or padded) list fails the structural gate before the recompute even runs.
func TestShrunkenClaimLedgerIsRefused(t *testing.T) {
	raw := tierManifest(t, map[string]string{"KNO-004": "FAIL"}, nil, nil)
	var m struct {
		Cases []map[string]any `json:"cases"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, c := range m.Cases {
		proof, ok := c["capability_tier_proof"].(map[string]any)
		if !ok {
			continue
		}
		for _, d := range proof["capabilities"].([]any) {
			decl := d.(map[string]any)
			if decl["capability"] == "knowledge" {
				decl["claim_case_ids"] = []string{"KNO-001", "KNO-002", "KNO-003"} // drop the red claim from the ledger
				decl["declared_tier"] = "stable"
			}
		}
	}
	edited, err := json.Marshal(map[string]any{
		"release": "extensions-0.1.0-anchor-fixture", "git_sha": strings.Repeat("c", 40),
		"api_version": "v1", "migration": "000040", "captured_at": "2026-07-25T00:00:00Z",
		"cases": m.Cases,
	})
	if err != nil {
		t.Fatalf("marshal edited manifest: %v", err)
	}
	if findings := tierFindings(edited); len(findings) == 0 {
		t.Fatal("a SHRUNKEN claim ledger (dropping the red claim so the remainder is green) was ACCEPTED — the ledger must be anchored to the canonical code table (plan §T11)")
	}
}

// renderFindings joins findings for a readable assertion message.
func renderFindings(findings []uat.Finding) string {
	parts := make([]string, 0, len(findings))
	for _, f := range findings {
		parts = append(parts, f.String())
	}
	return strings.Join(parts, " | ")
}
