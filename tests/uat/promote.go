package uat

import "encoding/json"

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
		if c.ThreeLanguageEqualityClaim != "" || c.GatewayOffClaim != "" {
			return SDKParityPromoteGate(raw, target)
		}
	}
	for _, c := range m.Cases {
		if c.UpgradeClaim != "" {
			return PromoteGate(raw, target)
		}
	}
	return []Refusal{{Detail: "no promote policy for this release: it carries neither the E16 SDK-parity nor the E15 upgrade claims a promote gate recognizes"}}
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
