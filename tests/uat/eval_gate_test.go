package uat

import (
	"strings"
	"testing"

	"github.com/palgroup/palai/tests/evals"
)

// evalsRoot is the eval fixtures the harness runs. The gate proof is DERIVED from a real run of the runner
// over the held-out split, so these tests exercise the full chain: reference engine -> grader -> runner ->
// EvalGateProof -> EvalPromoteGate. The engine opens no tool to a real provider (E08): these prove the
// GATE MECHANICS, never model quality.
const evalsRoot = "../evals/testdata"

// heldOutScores runs the four suites on the held-out split under a policy and returns per-suite
// {score, security-regressions, digest} — the exact inputs the release gate reads.
func heldOutScores(t *testing.T, policy evals.Policy) map[string]evals.SuiteReport {
	t.Helper()
	reports, err := evals.RunAll(evalsRoot, evals.HeldOut, policy)
	if err != nil {
		t.Fatalf("run held-out suites: %v", err)
	}
	return reports
}

// evalGateProofFrom builds the manifest EvalGateProof from held-out reports with a threshold table. It is
// the T6 bridge T11 will reuse to materialize the extensions bundle.
func evalGateProofFrom(reports map[string]evals.SuiteReport, thresholds map[string]float64) map[string]any {
	var suites []any
	for _, suite := range evals.Suites {
		r := reports[suite]
		suites = append(suites, map[string]any{
			"suite":                suite,
			"held_out_score":       r.Score,
			"threshold":            thresholds[suite],
			"security_regressions": r.SecurityFailures,
			"dataset_digest":       r.Digest,
		})
	}
	return map[string]any{"split": "held-out", "suites": suites}
}

// passThresholds is a threshold table every SafePolicy suite clears (the safe engine scores 1.0).
var passThresholds = map[string]float64{"coding": 0.9, "research": 0.9, "recovery": 0.9, "security": 1.0}

func evalManifest(proof map[string]any) map[string]any {
	m := baseManifest()
	c := caseOf(m)
	c["id"] = "QUA-004"
	c["eval_gate_claim"] = "thresholds-met"
	c["eval_gate_proof"] = proof
	return m
}

// TestEvalPromoteGateAcceptsCleanHeldOut is the positive control: a real SafePolicy held-out run clears
// every threshold with zero security regressions, so the release gate does NOT refuse.
func TestEvalPromoteGateAcceptsCleanHeldOut(t *testing.T) {
	proof := evalGateProofFrom(heldOutScores(t, evals.SafePolicy), passThresholds)
	if r := EvalPromoteGate(marshal(t, evalManifest(proof)), "rc"); len(r) != 0 {
		t.Fatalf("a clean held-out run must promote to rc, got refusals: %v", r)
	}
}

// TestEvalPromoteGateRefusesSubThreshold is QUA-004's threshold half: a deliberately sub-threshold FAKE
// candidate (its held-out coding score forced below the bar) is REFUSED at promotion.
func TestEvalPromoteGateRefusesSubThreshold(t *testing.T) {
	proof := evalGateProofFrom(heldOutScores(t, evals.SafePolicy), passThresholds)
	// Fake a sub-threshold candidate: drop the coding suite's held-out score under its threshold.
	proof["suites"].([]any)[0].(map[string]any)["held_out_score"] = 0.5
	r := EvalPromoteGate(marshal(t, evalManifest(proof)), "rc")
	if len(r) == 0 {
		t.Fatal("a sub-threshold candidate must be REFUSED at promotion (§57.13, QUA-004)")
	}
}

// TestEvalPromoteGateBlocksSecurityRegressionIndependentOfAggregate is QUA-004's gate half: a security
// regression BLOCKS promotion INDEPENDENT of the aggregate score — even a proof whose every suite score is
// a perfect 1.0 is refused when a suite reports a security regression (§57.13).
func TestEvalPromoteGateBlocksSecurityRegressionIndependentOfAggregate(t *testing.T) {
	// Every score is a perfect pass (aggregate is flawless) ...
	proof := evalGateProofFrom(heldOutScores(t, evals.SafePolicy), passThresholds)
	// ... but a security regression is present (sourced as a real count from a RegressedPolicy run).
	regressed := heldOutScores(t, evals.RegressedPolicy)["security"].SecurityFailures
	if regressed == 0 {
		t.Fatal("precondition: RegressedPolicy must produce a real security regression to inject")
	}
	for _, s := range proof["suites"].([]any) {
		sm := s.(map[string]any)
		sm["held_out_score"] = 1.0 // aggregate is perfect
		if sm["suite"] == "security" {
			sm["security_regressions"] = regressed
		}
	}
	r := EvalPromoteGate(marshal(t, evalManifest(proof)), "rc")
	if len(r) == 0 {
		t.Fatal("a security regression must BLOCK promotion regardless of a perfect aggregate score (§57.13)")
	}
	found := false
	for _, ref := range r {
		if strings.Contains(ref.Detail, "security regression") {
			found = true
		}
	}
	if !found {
		t.Fatalf("refusal must name the security regression; got %v", r)
	}
}

// --- Crown fix (E13..E16 MUST-FIX-1 shape): the gate RECOMPUTES from the canonical run, so a shape-
// consistent FABRICATED proof — one that passes Complete() + VerifyManifest — is still REFUSED at promotion.
// Each of these was RED before the recompute + canonical-threshold hoist: the pre-fix gate trusted the
// proof's own digest/score/threshold and its dead `target`, so every fabrication below promoted clean.

// TestEvalPromoteGateRefusesFabricatedDigest: a proof whose dataset_digest is well-formed but does NOT match
// the recomputed fixture digest is REFUSED — the numbers were not produced by the canonical held-out set.
func TestEvalPromoteGateRefusesFabricatedDigest(t *testing.T) {
	proof := evalGateProofFrom(heldOutScores(t, evals.SafePolicy), passThresholds)
	proof["suites"].([]any)[0].(map[string]any)["dataset_digest"] = "sha256:" + strings.Repeat("c", 64)
	if r := EvalPromoteGate(marshal(t, evalManifest(proof)), "rc"); len(r) == 0 {
		t.Fatal("a proof whose dataset_digest does not match the recomputed fixtures must be REFUSED (fabrication)")
	}
}

// TestEvalPromoteGateRefusesLoweredThreshold: a proof cannot self-report a lowered threshold to sneak a weak
// candidate through — the gate uses its own canonical EvalThresholds table, so a 0.001 threshold is REFUSED.
func TestEvalPromoteGateRefusesLoweredThreshold(t *testing.T) {
	proof := evalGateProofFrom(heldOutScores(t, evals.SafePolicy), passThresholds)
	proof["suites"].([]any)[0].(map[string]any)["threshold"] = 0.001
	if r := EvalPromoteGate(marshal(t, evalManifest(proof)), "rc"); len(r) == 0 {
		t.Fatal("a proof self-reporting a lowered threshold must be REFUSED (the gate owns the threshold table)")
	}
}

// TestEvalPromoteGateRefusesStableWithoutAttestation: the eval gate must NOT flip `stable` on the
// deterministic mechanical numbers — a stable promote awaits the real-model quality leg (§6 leg 7 → E18 RC),
// carried as an operator_attestation note, never auto-claimed. Absent it, the stable promote is REFUSED.
func TestEvalPromoteGateRefusesStableWithoutAttestation(t *testing.T) {
	proof := evalGateProofFrom(heldOutScores(t, evals.SafePolicy), passThresholds)
	if r := EvalPromoteGate(marshal(t, evalManifest(proof)), "stable"); len(r) == 0 {
		t.Fatal("a stable promote with no operator_attestation must be REFUSED (mechanical numbers cannot flip stable)")
	}
}

// TestEvalPromoteGateRefusesMissingProof: a bundle with no eval gate proof cannot be promoted through the
// eval gate — the gate never silently passes a release it cannot evaluate.
func TestEvalPromoteGateRefusesMissingProof(t *testing.T) {
	if r := EvalPromoteGate(marshal(t, baseManifest()), "rc"); len(r) == 0 {
		t.Fatal("a bundle with no EvalGateProof must be refused by the eval gate")
	}
}

// TestEvalGateProofCompleteness pins the structural Complete() invariant: a proof must be on the held-out
// split and carry all four suites each with a threshold + a content-address digest.
func TestEvalGateProofCompleteness(t *testing.T) {
	good := evalGateProofFrom(heldOutScores(t, evals.SafePolicy), passThresholds)
	// VerifyManifest turns an eval_gate_claim with an incomplete proof into an "invalid" finding.
	m := evalManifest(good)
	if fs := VerifyManifest(marshal(t, m), nil); len(fs) != 0 {
		t.Fatalf("a complete eval gate proof must verify clean, got: %v", fs)
	}
	// Drop a suite -> incomplete -> invalid finding.
	bad := evalGateProofFrom(heldOutScores(t, evals.SafePolicy), passThresholds)
	bad["suites"] = bad["suites"].([]any)[:2]
	if fs := VerifyManifest(marshal(t, evalManifest(bad)), nil); !hasKind(fs, "invalid") {
		t.Fatal("an incomplete eval gate proof (missing suites) must be an invalid finding")
	}
	// Claim with no proof -> missing finding.
	m2 := baseManifest()
	caseOf(m2)["eval_gate_claim"] = "thresholds-met"
	if fs := VerifyManifest(marshal(t, m2), nil); !hasKind(fs, "missing") {
		t.Fatal("an eval gate claim with no proof must be a missing finding")
	}
}
