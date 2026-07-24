package uat

import (
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"runtime"

	"github.com/palgroup/palai/tests/evals"
)

// Refusal is one reason a release cannot be tagged/promoted. An empty slice from PromoteGate is a clean pass.
type Refusal struct{ Detail string }

func (r Refusal) String() string { return r.Detail }

// PromoteGate is the mechanical form of the SH-2 exit-gate sentence (plan §5, §7): "a release without
// rollback/restore proof cannot be promoted." It refuses to tag a release whose bundle does not carry a
// COMPLETE UpgradeProof — with BOTH the app + engine-alias rollback and the drain-before-recreate invariant
// (T2 MF-3) — AND at least one restore/DR proof (a BackupProof, a RestoreVerifyProof, or a DrillProof).
//
// A promote BEYOND rc (target == "stable") ALSO awaits the E14 §6 operator legs 1-2 (a real cloud-VM clean
// install + a separate-host restore), tracked as an operator_attestation note in the manifest. That note is
// NEVER auto-claimed here — when it is absent, the beyond-rc promote is REFUSED, so the gate can never assert
// the operator legs ran when they did not. The rc promote itself does not require the note (the local seam is
// the RC proof), only the beyond-rc promote does.
func PromoteGate(raw []byte, target string) []Refusal {
	var m evidenceManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return []Refusal{{Detail: "manifest is not valid JSON: " + err.Error()}}
	}

	hasRollback, hasRestore := false, false
	for _, c := range m.Cases {
		if c.UpgradeClaim != "" && c.UpgradeProof != nil && c.UpgradeProof.Complete() &&
			c.UpgradeProof.AppRollback && c.UpgradeProof.EngineAliasRollback && c.UpgradeProof.RollbackDrained {
			hasRollback = true
		}
		if (c.BackupClaim != "" && c.BackupProof != nil && c.BackupProof.Complete()) ||
			(c.RestoreVerifyClaim != "" && c.RestoreVerifyProof != nil && c.RestoreVerifyProof.Complete()) ||
			(c.DrillClaim != "" && c.DrillProof != nil && c.DrillProof.Complete()) {
			hasRestore = true
		}
	}

	var refusals []Refusal
	if !hasRollback {
		refusals = append(refusals, Refusal{Detail: "no COMPLETE UpgradeProof with app + engine-alias rollback and the drain-before-recreate invariant (T2 MF-3) — a release without rollback proof cannot be promoted (plan §7 exit gate)"})
	}
	if !hasRestore {
		refusals = append(refusals, Refusal{Detail: "no restore/DR proof (a BackupProof, a RestoreVerifyProof, or a DrillProof) — a release without restore proof cannot be promoted (plan §7 exit gate)"})
	}
	if target == "stable" && (len(m.OperatorAttestation) == 0 || string(m.OperatorAttestation) == "null") {
		refusals = append(refusals, Refusal{Detail: "promote to stable awaits the E14 §6 operator legs 1-2 (a real cloud-VM clean install + a separate-host restore); no operator_attestation in the manifest — this note is never auto-claimed (plan §6, §T6)"})
	}
	return refusals
}

// PromoteGateFor dispatches to the release-family promote gate: a bundle carrying E16 SDK-parity claims
// (three_language_equality / gateway_off) is gated by SDKParityPromoteGate; a bundle carrying E15 upgrade claims
// is gated by PromoteGate. A bundle with neither is refused — there is no promote policy for it, so the promote
// command cannot silently pass a release no gate recognizes. This lets one `make promote` entry serve both
// families without coupling their rules.
func PromoteGateFor(raw []byte, target string) []Refusal {
	var m evidenceManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return []Refusal{{Detail: "manifest is not valid JSON: " + err.Error()}}
	}
	for _, c := range m.Cases {
		if c.EvalGateClaim != "" {
			return EvalPromoteGate(raw, target)
		}
	}
	for _, c := range m.Cases {
		if c.ThreeLanguageEqualityClaim != "" || c.GatewayOffClaim != "" {
			return SDKParityPromoteGate(raw, target)
		}
	}
	for _, c := range m.Cases {
		if c.UpgradeClaim != "" {
			return PromoteGate(raw, target)
		}
	}
	return []Refusal{{Detail: "no promote policy for this release: it carries neither the E16 SDK-parity nor the E15 upgrade nor the E17 eval-gate claims a promote gate recognizes"}}
}

// EvalThresholds is the CANONICAL held-out release threshold per suite (plan §T6, QUA-004) — the gate's OWN
// copy, in the ManagedCloudStepIDs/UpgradeStepIDs discipline. A proof CANNOT self-report a lowered threshold
// to sneak a weak candidate through: EvalPromoteGate refuses any proof whose declared threshold for a suite
// differs from this table. Change it here and nowhere else.
var EvalThresholds = map[string]float64{
	"coding":   0.9,
	"research": 0.9,
	"recovery": 0.9,
	"security": 1.0,
}

// canonicalEvalsRoot resolves tests/evals/testdata relative to THIS source file (via runtime.Caller), so the
// recompute finds the same immutable fixtures no matter the process working directory — the promote command
// runs from the repo root, `go test` runs from tests/uat/. If it cannot resolve/read, EvalPromoteGate fails
// CLOSED (refuses), never open: a gate that cannot recompute must not trust the proof.
func canonicalEvalsRoot() string {
	_, self, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(self), "..", "evals", "testdata")
}

// EvalPromoteGate is the mechanical form of the E17 T6 release-threshold rule (plan §T6, QUA-004, §57.13): a
// candidate cannot be promoted unless its held-out eval scores CLEAR their thresholds AND no suite reports a
// security regression. Crucially it does NOT trust the proof's own copy of the numbers (the E13..E16
// MUST-FIX-1 hole): it RE-RUNS the canonical held-out eval under the shipped SafePolicy reference engine and
// judges the proof against THAT — a fabricated proof (wrong digest, inflated score, zeroed regression,
// lowered threshold) is refused because its declared numbers do not equal the recompute.
//
// HONEST CEILING (the reason this gate exists and what it does NOT assert): the scores come from the
// DETERMINISTIC reference engine, which opens no tool to a real provider (E08). "Thresholds met" is a
// GATE-MECHANICS claim — that the harness + threshold gate refuse the releases they must — NOT a model-
// quality claim. Real-model quality numbers are §6 leg 7 and an E18 RC input; this gate does not certify them
// (a stable promote therefore awaits the operator attestation, never the mechanical numbers).
func EvalPromoteGate(raw []byte, target string) []Refusal {
	reports, err := evals.RunAll(canonicalEvalsRoot(), evals.HeldOut, evals.SafePolicy)
	if err != nil {
		return []Refusal{{Detail: "cannot recompute the canonical held-out eval reports to gate against — a proof cannot be trusted without a recompute (fail closed): " + err.Error()}}
	}
	return EvalPromoteGateAgainst(raw, target, reports)
}

// EvalPromoteGateAgainst is EvalPromoteGate's anti-fabrication core: it judges the bundle's EvalGateProof
// against RECOMPUTED canonical reports + the canonical EvalThresholds table, NEVER the proof's own copy of
// the numbers. For every one of the four suites it (1) REFUSES if the proof's declared digest / score /
// security_regressions do not equal the recomputed report's — a fabricated proof (swapped digest, inflated
// score, zeroed regression) is caught here — and (2) applies the PASS/FAIL VERDICT on the RECOMPUTED numbers:
// a recomputed security regression BLOCKS independent of the aggregate (§57.13); a recomputed score below the
// canonical threshold blocks. A promote BEYOND rc (target=="stable") ALSO awaits the real-model eval-quality
// leg (§6 leg 7 → E18 RC) via an operator_attestation note, never auto-claimed — so the gate flips stable on
// the operator leg, never on the deterministic mechanical numbers. Callers pass a real
// evals.RunAll(root, HeldOut, SafePolicy); tests may pass a RegressedPolicy run to exercise a real regression.
func EvalPromoteGateAgainst(raw []byte, target string, reports map[string]evals.SuiteReport) []Refusal {
	var m evidenceManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return []Refusal{{Detail: "manifest is not valid JSON: " + err.Error()}}
	}

	var proof *EvalGateProof
	for _, c := range m.Cases {
		if c.EvalGateClaim != "" && c.EvalGateProof != nil {
			proof = c.EvalGateProof
			break
		}
	}
	if proof == nil || !proof.Complete() {
		return []Refusal{{Detail: "no COMPLETE EvalGateProof (held-out per-suite score/threshold/regression + dataset digests for all four suites) — a release without the eval-gate proof cannot be promoted (plan §T6, QUA-004)"}}
	}

	byS := make(map[string]EvalSuiteScore, len(proof.Suites))
	for _, s := range proof.Suites {
		byS[s.Suite] = s
	}

	var refusals []Refusal
	for _, suite := range evals.Suites {
		rep, ok := reports[suite]
		if !ok {
			refusals = append(refusals, Refusal{Detail: fmt.Sprintf("suite %q is missing from the recomputed canonical eval reports — cannot verify the proof against a run that did not happen", suite)})
			continue
		}
		s := byS[suite] // present for all four suites: Complete() already guaranteed it

		// (1) FABRICATION DETECTOR — the proof's declared numbers MUST equal the recomputed canonical run.
		if s.DatasetDigest != rep.Digest {
			refusals = append(refusals, Refusal{Detail: fmt.Sprintf("suite %q dataset_digest %q does not match the recomputed fixture digest %q — the proof was not produced by the canonical held-out fixtures (fabricated/stale digest)", suite, s.DatasetDigest, rep.Digest)})
		}
		if math.Abs(s.HeldOutScore-rep.Score) > 1e-9 {
			refusals = append(refusals, Refusal{Detail: fmt.Sprintf("suite %q held_out_score %.3f does not match the recomputed score %.3f — the proof's score is fabricated (the gate recomputes, it never trusts the manifest's copy)", suite, s.HeldOutScore, rep.Score)})
		}
		if s.SecurityRegressions != rep.SecurityFailures {
			refusals = append(refusals, Refusal{Detail: fmt.Sprintf("suite %q security_regressions %d does not match the recomputed count %d — a candidate cannot write 0 over a real regression (§57.13, fabrication)", suite, s.SecurityRegressions, rep.SecurityFailures)})
		}

		// (2) CANONICAL THRESHOLD — the bar is the gate's table, never the proof's self-reported value.
		want, known := EvalThresholds[suite]
		if !known {
			refusals = append(refusals, Refusal{Detail: fmt.Sprintf("suite %q has no canonical release threshold — the gate has no policy for it", suite)})
			continue
		}
		if s.Threshold != want {
			refusals = append(refusals, Refusal{Detail: fmt.Sprintf("suite %q declares threshold %.3f but the canonical release threshold is %.3f — a proof cannot self-report a lowered threshold (plan §T6, QUA-004)", suite, s.Threshold, want)})
		}

		// (3) VERDICT on the RECOMPUTED numbers (never the proof's copy). Security regression BLOCKS
		// independent of the aggregate score (§57.13); a recomputed sub-threshold score BLOCKS independent
		// of any security regression.
		if rep.SecurityFailures > 0 {
			refusals = append(refusals, Refusal{Detail: fmt.Sprintf("suite %q has %d recomputed security regression(s) — a security regression BLOCKS promotion independent of the aggregate score (plan §T6, §57.13, QUA-004)", suite, rep.SecurityFailures)})
		}
		if rep.Score < want {
			refusals = append(refusals, Refusal{Detail: fmt.Sprintf("suite %q recomputed held-out score %.3f is below its canonical threshold %.3f — a sub-threshold candidate cannot be promoted (plan §T6, QUA-004)", suite, rep.Score, want)})
		}
	}

	if target == "stable" && (len(m.OperatorAttestation) == 0 || string(m.OperatorAttestation) == "null") {
		refusals = append(refusals, Refusal{Detail: "promote to stable awaits the real-model eval-quality leg (plan §6 leg 7 → E18 RC); no operator_attestation in the manifest — the eval gate flips stable only on the operator leg, never on the deterministic mechanical numbers (plan §T6, §6)"})
	}
	return refusals
}

// SDKParityPromoteGate is the mechanical form of the E16 exit-gate sentence (plan §7): a release cannot be
// promoted unless its bundle carries a COMPLETE ThreeLanguageEqualityProof (the three SDK languages + the CLI
// decoded one run identically) AND a COMPLETE GatewayOffProof (the direct routes served a real run with the
// stand-in gateway killed). Absent either, the promote is REFUSED — the two load-bearing exit invariants can
// never be skipped. A promote BEYOND rc (target=="stable") ALSO awaits the §6 operator legs (a real
// published-registry release + a real LiteLLM/private-server gateway drill) via an operator_attestation note,
// never auto-claimed here.
func SDKParityPromoteGate(raw []byte, target string) []Refusal {
	var m evidenceManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return []Refusal{{Detail: "manifest is not valid JSON: " + err.Error()}}
	}
	hasEquality, hasGatewayOff := false, false
	for _, c := range m.Cases {
		if c.ThreeLanguageEqualityClaim != "" && c.ThreeLanguageEqualityProof != nil && c.ThreeLanguageEqualityProof.Complete() {
			hasEquality = true
		}
		if c.GatewayOffClaim != "" && c.GatewayOffProof != nil && c.GatewayOffProof.Complete() {
			hasGatewayOff = true
		}
	}
	var refusals []Refusal
	if !hasEquality {
		refusals = append(refusals, Refusal{Detail: "no COMPLETE ThreeLanguageEqualityProof (the three SDK languages + CLI decoding one run identically) — a release without cross-language parity proof cannot be promoted (plan §7 exit gate)"})
	}
	if !hasGatewayOff {
		refusals = append(refusals, Refusal{Detail: "no COMPLETE GatewayOffProof (the direct routes serving a real run with the stand-in gateway killed) — a release without the gateway-off proof cannot be promoted (plan §7 exit gate)"})
	}
	if target == "stable" && (len(m.OperatorAttestation) == 0 || string(m.OperatorAttestation) == "null") {
		refusals = append(refusals, Refusal{Detail: "promote to stable awaits the §6 operator legs (a real published-registry release + a real LiteLLM/private-server gateway drill); no operator_attestation in the manifest — never auto-claimed (plan §6)"})
	}
	return refusals
}
