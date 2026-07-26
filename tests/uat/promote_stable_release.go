package uat

// The E18 T10 stable-release promote family (plan §T10) — the fourth family PromoteGateFor dispatches to.
//
// `rc` passes on the LOCAL seam. `stable` REQUIRES an operator attestation that names every §6 leg one by
// one, and no amount of local evidence substitutes for it: SH-3 Stable IS that attestation. Nothing in this
// file can flip a release to stable on mechanical grounds, which is the point.

import (
	"encoding/json"
	"fmt"
	"strings"
)

// StableAttestationSchema pins the record shape this gate judges. A record of another shape is not the
// attestation the policy describes.
const StableAttestationSchema = "palai.stable-attestation/v1"

// AttestedLegDisposition is the vocabulary a leg may close under. There is no "n/a" and no "skipped": every
// leg is either EXECUTED against real infrastructure, or the operator states out loud that the capability
// it backs stays preview/disabled. Both are decisions; neither is silence.
const (
	LegExecuted             = "executed"
	LegDeliberatelyPreview  = "deliberately-preview"
	LegDeliberatelyDisabled = "deliberately-disabled"
)

var attestedLegDispositions = map[string]bool{
	LegExecuted: true, LegDeliberatelyPreview: true, LegDeliberatelyDisabled: true,
}

// StableAttestationLegs is the CANONICAL §6 operator-leg ledger a `stable` promote must name, ID by ID.
// It is this repository's own copy of docs/operations/known-gaps-1.0.md §5 (the E18 T9 triage's "operator
// legs inherited from earlier epics" table), kept here so the gate cannot be satisfied by an attestation
// that supplies its own shorter list — a record that named its own obligations would discharge itself.
//
// Ordered, because the refusal message enumerates what is missing and an operator reading it should meet
// the legs in the order the triage table lists them.
var StableAttestationLegs = []struct {
	ID  string
	Leg string
}{
	{"L-1", "a real CI release run: protected environment, two maintainers, workflow-identity provenance, a Sigstore/KMS signature and a transparency-log entry (E18 §6 leg 1)"},
	{"L-2", "a real registry publish: npm, PyPI, a Go module tag and GHCR immutable images (E18 §6 leg 2, inherited from E16 §6 leg 3)"},
	{"L-3", "PER-001..004 on reference hardware, and a FULL UAT run on amd64 above the qemu boot-smoke (E18 §6 leg 3)"},
	{"L-4", "a real air-gap facility drill with an operator trust-root ceremony (E18 §6 leg 4, inherited from E15 §6 leg 2)"},
	{"L-5", "real-model eval QUALITY numbers — E08's rule holds, the engine exposes no tools to a real provider, so they cannot be produced locally and are an INPUT to this attestation (E18 §6 leg 5, inherited from E17 §6 leg 7)"},
	{"L-6", "a KMS-backed master-key ceremony above the file seam (E18 §6 leg 6; E13-H SEC-001/003)"},
	{"L-7", "a real RC soak on the target topology — the local soak is a bounded window of MINUTES and is named that way (E18 §6 leg 7)"},
	{"L-8a", "a cloud-VM clean install and a restore onto a SEPARATE physical host; systemd boot-persistence on a real two-VM split runner (E14 §6 legs 1-3)"},
	{"L-8b", "a real restricted managed-Kubernetes install with an ENFORCING CNI; a real second-site DR drill; a published-release-to-release upgrade (E15 §6 legs 1, 3, 4)"},
	{"L-8c", "a real LiteLLM instance and a real private model server (vLLM/Ollama class); journey 63.1 on a Linux and a Windows workstation (E16 §6 legs 1, 2, 4)"},
	{"L-8d", "a real Slack workspace; a foreign A2A peer; real Xcode + Apple signing; pgvector or an external vector engine; a real broker PRODUCT; a real Temporal instance; a deployed console with a manual screen-reader pass (E17 §6 legs 1-6, 8)"},
}

// AttestedLeg is one operator leg's disposition in the attestation.
type AttestedLeg struct {
	ID          string `json:"id"`
	Disposition string `json:"disposition"`
	Detail      string `json:"detail"`
}

// StableAttestation is the record a `stable` promote presents. It is the mechanical form of the plan's
// closing sentence: "SH-3 Stable = o attestation'ın kendisi."
type StableAttestation struct {
	Schema   string        `json:"schema"`
	Release  string        `json:"release"`
	Target   string        `json:"target"`
	Attester string        `json:"attester"`
	Legs     []AttestedLeg `json:"legs"`
}

// StableReleasePromoteGate is the mechanical form of the E18 exit-gate sentence (plan §T10, §7).
//
// For `rc` it has four clauses, and each one RECOMPUTES rather than reads:
//
//  1. a COMPLETE ReleaseIndexProof whose every row re-gathers from the committed per-bundle manifests, and
//     whose RC-blocker count re-reads from the E18 T9 triage table. An open RC-blocker REFUSES the promote —
//     that is §64.15's "zero open P0/P1", read from the table rather than from the release;
//  2. a COMPLETE AggregateTierProof whose product-wide posture recomputes from EVERY committed bundle's
//     claim outcomes and equals the fully-mounted router's /v1/capabilities bit for bit. A fabricated
//     cross-epic "stable" is refused here;
//  3. QUA-003, the eval security suite, PASS in the RECOMPUTED index — the precondition E17 established for
//     any capability's stable flip, re-applied at the product tier;
//  4. SUP-3's RULE, WHICH THIS GATE TAKES: a COMPLETE SupplyChainProof, which by construction NAMES the
//     release directory that was verified and records an OFFLINE verification plus the six-arm tamper
//     matrix. `scripts/release/promote.sh` deliberately does not fence an unnamed PALAI_RELEASE_DIR (a fence
//     there would run before the evidence gate and shadow the E15 T6 operator-leg refusal that
//     scripts/uat/sh2 and scripts/uat/sdk-parity grep for). The E18 T9 triage wrote: "if T10 does not take
//     this rule, no rule enforces it." This clause is T10 taking it — a release in THIS family cannot be
//     promoted at all without a verified artifact set.
//
// For `stable` it adds the only clause that matters: an operator attestation naming EVERY §6 leg. No local
// evidence promotes this release; the attestation is the promotion.
func StableReleasePromoteGate(raw []byte, target string) []Refusal {
	var m evidenceManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return []Refusal{{Detail: "manifest is not valid JSON: " + err.Error()}}
	}

	var refusals []Refusal
	add := func(format string, args ...any) { refusals = append(refusals, Refusal{Detail: fmt.Sprintf(format, args...)}) }

	// (1) the release index. The gate judges ONE index; a manifest carrying two would be judged on the
	// first while VerifyManifest checks all of them, so a fabricated second could ride behind an honest one.
	var index *ReleaseIndexProof
	indexClaims := 0
	for _, c := range m.Cases {
		if c.ReleaseIndexClaim == "" {
			continue
		}
		indexClaims++
		if index == nil {
			index = c.ReleaseIndexProof
		}
	}
	switch {
	case indexClaims > 1:
		add("%d release_index_claims in one manifest (want exactly 1): this gate judges the FIRST index, so a second could ride behind an honest one — one release, one recomputed index (plan §T10)", indexClaims)
	case index == nil || !index.Complete():
		add("no COMPLETE ReleaseIndexProof (one entry per Appendix-A UAT id with its carrying bundle + outcome + disposition, the §64.15 checklist posture, and the RC-blocker count) — a release without the index cannot be tagged (plan §T10, §7 exit gate)")
	default:
		for _, problem := range verifyReleaseIndex(index) {
			refusals = append(refusals, Refusal{Detail: problem})
		}
	}

	// (2) the product-wide posture.
	var tier *AggregateTierProof
	tierClaims := 0
	for _, c := range m.Cases {
		if c.AggregateTierClaim == "" {
			continue
		}
		tierClaims++
		if tier == nil {
			tier = c.AggregateTierProof
		}
	}
	switch {
	case tierClaims > 1:
		add("%d aggregate_tier_claims in one manifest (want exactly 1): a second posture table could ride behind an honest one (plan §T10)", tierClaims)
	case tier == nil || !tier.Complete():
		add("no COMPLETE AggregateTierProof (the per-capability product-wide tier + its canonical claim ledger + the fully-mounted router's /v1/capabilities snapshot, with the EXT-1 fences: the snapshot source NAMES fullyMountedRouter and no deployed config is claimed to serve it) — a release without the product-wide posture cannot be tagged (plan §T10, §7 exit gate)")
	default:
		for _, problem := range verifyAggregateTiers(tier) {
			refusals = append(refusals, Refusal{Detail: problem})
		}
	}

	// (3) the security precondition, re-read from the recomputed index rather than from this manifest.
	if entries, err := RecomputeReleaseIndex(); err != nil {
		add("cannot recompute the release index to check the %s security precondition (fail closed): %v", CapabilitySecurityPrecondition, err)
	} else {
		found := false
		for _, e := range entries {
			if e.ID != CapabilitySecurityPrecondition {
				continue
			}
			found = true
			if e.Disposition != DispositionBundleCarried || e.Outcome != "PASS" {
				add("the eval security suite case %s is %q in %q (disposition %q) — it is the PRECONDITION for every capability's stable flip, so a tag without it green is REFUSED (plan §T6, §T11, §57.13)",
					CapabilitySecurityPrecondition, e.Outcome, e.Bundle, e.Disposition)
			}
		}
		if !found {
			add("the eval security suite case %s is not in the Appendix-A index — the precondition for every capability's stable flip cannot be checked (plan §T6, §T11)", CapabilitySecurityPrecondition)
		}
	}

	// (4) SUP-3's rule.
	verifiedArtifacts := false
	for _, c := range m.Cases {
		if c.SupplyChainClaim != "" && c.SupplyChainProof != nil && c.SupplyChainProof.Complete() {
			verifiedArtifacts = true
			break
		}
	}
	if !verifiedArtifacts {
		add("no COMPLETE SupplyChainProof: this release family must ALWAYS carry a VERIFIED artifact set — a NAMED release directory, its index/artifact digests hanging from an openssl P-256 signed root, an OFFLINE re-verify and the six-arm tamper matrix all rejected. `scripts/release/promote.sh` deliberately verifies nothing when PALAI_RELEASE_DIR is unset (a fence there would shadow the E15 T6 operator-leg refusal scripts/uat/sh2 and scripts/uat/sdk-parity grep for), so the rule lives HERE — SUP-3, docs/operations/known-gaps-1.0.md: \"if T10 does not take this rule, no rule enforces it\"")
	}

	if target == "rc" || target == "" {
		return refusals
	}
	return append(refusals, stableAttestationRefusals(m, target)...)
}

// stableAttestationRefusals is the `stable` clause: SH-3 Stable is the operator attestation, so a promote
// beyond rc is judged on the attestation and nothing else can substitute for it.
//
// HONEST CEILING, and the reason this can never pass in this session: no §6 leg has been executed. An
// attestation written here would be a fabrication, so the gate is proven by its REFUSALS — the missing
// record, the wrong schema, the wrong release, the wrong target, and an attestation that names ten of the
// eleven legs.
func stableAttestationRefusals(m evidenceManifest, target string) []Refusal {
	var refusals []Refusal
	add := func(format string, args ...any) { refusals = append(refusals, Refusal{Detail: fmt.Sprintf(format, args...)}) }

	if len(m.OperatorAttestation) == 0 || string(m.OperatorAttestation) == "null" {
		add("promote to %q awaits the §6 operator legs and there is NO operator_attestation in the manifest. SH-3 Stable IS that attestation, never a mechanical verdict: it must name every leg one by one (%d of them, L-1..L-8d) and this gate never auto-claims one (plan §T10, §6)", target, len(StableAttestationLegs))
		return refusals
	}

	var a StableAttestation
	if err := json.Unmarshal(m.OperatorAttestation, &a); err != nil {
		add("the operator attestation is not valid JSON (an unreadable attestation is NOT an attestation): %v", err)
		return refusals
	}
	if a.Schema != StableAttestationSchema {
		add("attestation schema %q is not %q — this gate judges one record shape", a.Schema, StableAttestationSchema)
	}
	if a.Release != m.Release {
		add("the attestation is for release %q but this manifest is %q — an attestation binds to ONE release, or an rc attestation replays onto anything", a.Release, m.Release)
	}
	if a.Target != target {
		add("the attestation is for target %q but this promote is %q — an attestation binds to ONE target", a.Target, target)
	}
	if strings.TrimSpace(a.Attester) == "" {
		add("the attestation names no attester — an unsigned-off attestation is a way of not deciding (the E18 T9 triage's own rule)")
	}

	declared := make(map[string]AttestedLeg, len(a.Legs))
	for _, leg := range a.Legs {
		id := strings.TrimSpace(leg.ID)
		if _, known := declared[id]; known {
			add("leg %q is attested twice — one leg, one disposition", id)
			continue
		}
		declared[id] = leg
	}
	for _, want := range StableAttestationLegs {
		leg, ok := declared[want.ID]
		if !ok {
			add("the attestation does not name §6 operator leg %s — %s. A stable promote must name EVERY leg one by one; a leg it cannot name is a leg that did not run (plan §T10, §6)", want.ID, want.Leg)
			continue
		}
		if !attestedLegDispositions[leg.Disposition] {
			add("leg %s is attested %q, which is not one of executed / deliberately-preview / deliberately-disabled — there is no \"n/a\" and no \"skipped\": every leg is a decision", want.ID, leg.Disposition)
		}
		if strings.TrimSpace(leg.Detail) == "" {
			add("leg %s carries no detail — a disposition with no account of what was run (or of what stays preview and why) is a checkbox, not an attestation", want.ID)
		}
	}
	for id := range declared {
		known := false
		for _, want := range StableAttestationLegs {
			if want.ID == id {
				known = true
				break
			}
		}
		if !known {
			add("the attestation names leg %q, which is not in the canonical §6 ledger (docs/operations/known-gaps-1.0.md §5) — an attestation may not supply its own obligations", id)
		}
	}
	return refusals
}
