package uat

import (
	"encoding/json"
	"fmt"
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

// EvalPromoteGate is the mechanical form of the E17 T6 release-threshold rule (plan §T6, QUA-004, §57.13): a
// candidate cannot be promoted unless its held-out eval scores CLEAR their thresholds AND no suite reports a
// security regression. The two refusals are INDEPENDENT: a security regression BLOCKS the tag even when the
// aggregate score is flawless, and a sub-threshold score BLOCKS even when there is no security regression.
// A bundle with no (or an incomplete) EvalGateProof is refused — the gate never silently passes a release it
// cannot evaluate.
//
// HONEST CEILING (the reason this gate exists and what it does NOT assert): the scores come from the
// DETERMINISTIC reference engine, which opens no tool to a real provider (E08). "Thresholds met" is a
// GATE-MECHANICS claim — that the harness + threshold gate refuse the releases they must — NOT a model-
// quality claim. Real-model quality numbers are §6 leg 7 and an E18 RC input; this gate does not certify them.
func EvalPromoteGate(raw []byte, target string) []Refusal {
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

	var refusals []Refusal
	for _, s := range proof.Suites {
		// Security regression BLOCKS independent of the aggregate score (§57.13) — checked FIRST and never
		// waived by a passing score.
		if s.SecurityRegressions > 0 {
			refusals = append(refusals, Refusal{Detail: fmt.Sprintf("suite %q has %d security regression(s) — a security regression BLOCKS promotion independent of the aggregate score (plan §T6, §57.13, QUA-004)", s.Suite, s.SecurityRegressions)})
		}
		if s.HeldOutScore < s.Threshold {
			refusals = append(refusals, Refusal{Detail: fmt.Sprintf("suite %q held-out score %.3f is below its threshold %.3f — a sub-threshold candidate cannot be promoted (plan §T6, QUA-004)", s.Suite, s.HeldOutScore, s.Threshold)})
		}
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
