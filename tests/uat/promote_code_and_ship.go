package uat

// The E22 T7 promote gate (plan §T7). It lives beside promote.go rather than inside it for the same reason
// evidence_code_and_ship.go lives beside evidence.go, and it follows the family discipline exactly: the gate
// does its OWN re-derivation (never inheriting Complete()'s verdict), and it COMPOSES the epic underneath it
// rather than restating that epic's rules.

import (
	"encoding/json"
	"fmt"
)

// CodeAndShipPromoteGate is the mechanical form of the E22 exit-gate sentence (plan §T7, §7): a release
// cannot be tagged while any of the three crown guards is red. It has five clauses:
//
//  1. the bundle must carry EXACTLY ONE complete CodeAndShipProof;
//
//  2. the UNAPPROVED-PUBLICATION count is RE-DERIVED here, from the ledger the proof carries, and the ledger
//     must show BOTH halves — an approve that published and a deny that withheld. This is the crown claim of
//     the epic: the model writes code and cannot publish it. Every other failure in the chain announces
//     itself (a refused click answers, a failed push warns, an unwired publisher logs); an unapproved push is
//     a branch quietly appearing on somebody's remote, which is why it is re-derived rather than counted;
//
//  3. the MODEL-CHOOSABLE DESTINATION count is RE-DERIVED from the two publish tools' own input schemas. A
//     base the model can choose is a base the approver did not approve, so the destination lives in the
//     binding (rb.default_branch, X17) and appears in no schema at all;
//
//  4. the TYPED-OPERATION CEILING is RECOMPUTED from workers/catalog.go's own SOURCE: still one capability,
//     still one operation, still nothing named `ios.` or `apple`. E22 solved iOS by NOT TYPING IT, and this
//     is the clause that answers the next reader who proposes an `ios.build` operation;
//
//  5. NO TIER MAY ADVANCE — enforced by the composed E21 gate (which composes E20's, which composes E19's
//     mount derivation, E17's tier table and the eval numbers underneath it). Composing rather than
//     re-deriving keeps ONE owner of each rule.
//
// THE TIER DECISION IS ARGUED, NOT ASSUMED, and the counter-argument here is the strongest any of these gates
// has faced: "a real repository is cloned, a real Xcode compiles, a real simulator is driven and a real pull
// request is opened — `apple-build` should be preview." It is REFUSED for four reasons that belong in the
// code and not only in a commit message:
//
//	(1) THE PROOF OF `apple-build` WAS NEVER PRODUCED, and in this version the reason is shorter than ever:
//	    E22 DOES NOT TOUCH THE `workers` PACKAGE. Catalog is bit-unchanged, KnownCapability("apple-build") is
//	    still false, and X7 measured that the simulator path never raises the signing question at all.
//	(2) §6 leg 1 is still OPEN. A real Slack workspace is connected and there is still NO CAPTURED RECEIPT;
//	    the proof's Peer is structurally the literal "fake" and no code in this repository can produce
//	    otherwise. §6 leg 5 (a real GitHub App opening a real draft PR) is open beside it.
//	(3) E22 REMOVES A SECURITY BOUNDARY. The native shell posture has no sandbox and no egress backstop
//	    (T1, §3.5 X22). RAISING A TIER IN THE EPIC THAT DELETES A BOUNDARY IS PRECISELY WHAT THIS GATE EXISTS
//	    TO PREVENT — and it is a stronger reason than E20's and E21's "the surface grows", because a surface
//	    that grows is still watched by everything that watched it before.
//	(4) THE NEWEST DEPENDENCY IS A THIRD-PARTY TOOL ON PRIVATE APPLE APIs (`axe`, X4) — and it is not even in
//	    Palai's code; it is a binary on the host's PATH, which an OS update can break without a line of this
//	    repository changing.
//
// WHAT WOULD HAVE HAD TO BE TRUE to move `apple-build`: (i) an apple-build capability in Catalog with at
// least one typed signing/archiving operation; (ii) a signing identity loaded into an EPHEMERAL keychain,
// resolved from a job-scoped handle, leaking into no receipt; (iii) a provisioning-profile selection policy
// that is not the model; (iv) an .xcarchive + exportOptionsPlist path whose produced .ipa has a VERIFIED
// signature; (v) a UAT case and a §6 leg proving all of it. None of the five exists, and four are not even
// in scope.
//
// A promote BEYOND rc inherits every refusal underneath and adds nothing of its own: `slack` caps at preview
// by construction (CapabilityOperatorLegs §6 leg 1), `knowledge-vector` is `disabled` by construction, and
// `apple-build` is `disabled` by construction (capabilities.go returns the literal). `workspaces` gives a
// DERIVED answer for the first time and the correct word is "available" — which is not a stable/preview
// promotion, because workspacesCapability() does not draw from that vocabulary at all (§3.6 D15).
func CodeAndShipPromoteGate(raw []byte, target string) []Refusal {
	var m evidenceManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return []Refusal{{Detail: "manifest is not valid JSON: " + err.Error()}}
	}

	var refusals []Refusal

	var cs *CodeAndShipProof
	claims := 0
	for _, c := range m.Cases {
		if c.CodeAndShipClaim == "" {
			continue
		}
		claims++
		if cs == nil {
			cs = c.CodeAndShipProof
		}
	}
	switch {
	case claims > 1:
		refusals = append(refusals, Refusal{Detail: fmt.Sprintf("%d code_and_ship_claims in one manifest (want exactly 1): this gate judges the FIRST code-and-ship proof, so a second could ride behind an honest one — one release, one re-derivation (plan §T7)", claims)})
	case cs == nil:
		refusals = append(refusals, Refusal{Detail: codeAndShipIncomplete})
	default:
		// THE GATE'S OWN SWEEPS RUN FIRST, before Complete() is consulted, and the ORDER is the whole point.
		// Complete() re-derives the same counts, so putting it ahead would make every branch below
		// unreachable for exactly the inputs it exists to catch — a guard that cannot fire, which is this
		// repository's most-found defect. Running them first means a tag decision rests on THIS gate's
		// derivation (the E15 SF-4 shape) and the reader is told WHICH claim failed.
		refusals = append(refusals, codeAndShipPublicationRefusals(cs)...)
		refusals = append(refusals, codeAndShipDestinationRefusals(cs)...)
		refusals = append(refusals, codeAndShipCatalogRefusals(cs)...)
		if !cs.Complete() {
			refusals = append(refusals, Refusal{Detail: codeAndShipIncomplete})
		}
	}

	// Clause 5 — composed verbatim. E22 opens no capability and moves no tier, so it owns NO tier rule of its
	// own; it inherits E21's, which inherits E20's, which inherits E19's cross-bundle comparison against the
	// committed extensions-0.1.0 baseline.
	return append(refusals, ToolsMemoryPromoteGate(raw, target)...)
}

const codeAndShipIncomplete = "no COMPLETE CodeAndShipProof (a repository CLONED and CHANGED + ZERO publications published without an approval, re-derived from a ledger that shows both an approve and a deny + ZERO destination fields the model could fill, re-derived from the two publish tools' input schemas + ZERO authority for a ticket body that is findable in what reached the model + the declared shell posture with workers.Catalog RECOMPUTED to one capability and one operation + ZERO Apple signing credentials engaged on a host that holds four + an artifact uploaded with ZERO actionable elements minted + the canonical contract-and-measurement ledger) — a release whose publication boundary or untrusted-input guards are red cannot be tagged (plan §T7, §7 exit gate)"

// codeAndShipPublicationRefusals re-derives the crown negative from the carried ledger: nothing reached a
// remote without an approve, and the ledger DEMONSTRATES that an approve publishes and a deny withholds.
func codeAndShipPublicationRefusals(cs *CodeAndShipProof) []Refusal {
	unapproved, approved, denied, err := SweepPublishedWithoutApproval(cs.PublicationLedger)
	if err != nil {
		return []Refusal{{Detail: "the code-and-ship proof's publication ledger cannot be swept, so \"nothing was published without an approval\" is unverifiable and the tag is REFUSED (fail closed): " + err.Error()}}
	}
	if len(unapproved) != 0 {
		return []Refusal{{Detail: fmt.Sprintf(
			"%d publication(s) reached a remote WITHOUT an approval: %v — the model may ask to publish and may not publish, and a bundle declaring %d is fabricated (plan §2, §T7)",
			len(unapproved), unapproved, cs.PublishedWithoutApproval)}}
	}
	if approved < 1 {
		return []Refusal{{Detail: "the publication ledger contains no publication that an APPROVE actually published — a zero over a run nobody ever approved is VACUOUS, and a guard that has never seen the effect happen cannot certify that the effect needs an approval (plan §T7)"}}
	}
	if denied < 1 {
		return []Refusal{{Detail: "the publication ledger contains no DENIED publication that was withheld — deny must PREVENT the side effect rather than record a verdict about it, and a ledger with no deny in it never demonstrated that (plan §T7)"}}
	}
	return nil
}

// codeAndShipDestinationRefusals re-derives the second crown negative from the publish tools' own schemas.
func codeAndShipDestinationRefusals(cs *CodeAndShipProof) []Refusal {
	if !codeAndShipSchemasCarryBothTools(cs.PublishToolSchemas) {
		return []Refusal{{Detail: "the code-and-ship proof does not carry BOTH publish tools' input schemas (palai.publish.push and palai.publish.pull_request) — a destination sweep over schemas that are not theirs proves nothing about them, and this is the vacuous form of the claim (plan §T7)"}}
	}
	destinations, err := SweepDestinationFields(cs.PublishToolSchemas)
	if err != nil {
		return []Refusal{{Detail: "the code-and-ship proof's publish tool schemas cannot be swept, so \"the model cannot name a destination\" is unverifiable and the tag is REFUSED (fail closed): " + err.Error()}}
	}
	if len(destinations) != 0 {
		return []Refusal{{Detail: fmt.Sprintf(
			"the publish tools expose %d destination field(s) the MODEL could fill: %v — the destination is resolved server-side from the run's binding (rb.default_branch, §3.5 X17), and a base the model can choose is a base the approver did not approve; the proof declares %d (plan §2, §T7)",
			len(destinations), destinations, cs.ModelChosenDestinationFields)}}
	}
	return nil
}

// codeAndShipCatalogRefusals is the cheapest clause in this gate and the one a future reader will meet first.
// It recomputes the no-tunnel allowlist from workers/catalog.go's SOURCE rather than believing the counters,
// so an added `ios.build` operation reddens the tag rather than quietly widening the surface.
func codeAndShipCatalogRefusals(cs *CodeAndShipProof) []Refusal {
	catalog, err := WorkerCatalogOperations()
	if err != nil {
		return []Refusal{{Detail: "the worker capability catalog cannot be re-derived from its source, so the no-tunnel ceiling is unverifiable and the tag is REFUSED (fail closed): " + err.Error()}}
	}
	if len(catalog) != 1 {
		return []Refusal{{Detail: fmt.Sprintf(
			"workers.Catalog types %d capabilities (want exactly 1, swift-toolchain): E22 solved iOS by NOT TYPING IT — `xcodebuild`, `simctl` and `axe` are PATH binaries reached through one shell call — and a typed capability brings back a worker binary, a dispatch tool and a transport, which for free-form argv is a TUNNEL (plan §5, §T7)",
			len(catalog))}}
	}
	for capability, ops := range catalog {
		if capability != "swift-toolchain" || len(ops) != 1 || ops[0] != "swift.build-check" {
			return []Refusal{{Detail: fmt.Sprintf(
				"workers.Catalog now types %q -> %v; this release is certified against exactly {swift-toolchain: [swift.build-check]}. `apple-build` has no entry and that ABSENCE IS STRUCTURAL — it is not a missing credential (this machine holds four valid signing identities and Xcode 26.6, measured 2026-07-28), it is that no apple-build operation is typed at all (plan §3.6 D5, §T7)",
				capability, ops)}}
		}
	}
	digest, err := WorkerCatalogDigest()
	if err != nil || digest != cs.WorkerCatalogDigest {
		return []Refusal{{Detail: fmt.Sprintf(
			"the worker catalog digest recomputes to %q but the proof declares %q — the ceiling is re-derived from the catalog's own source, never read off the manifest (plan §T7)",
			digest, cs.WorkerCatalogDigest)}}
	}
	return nil
}
