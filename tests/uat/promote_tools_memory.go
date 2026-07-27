package uat

// The E21 T7 promote gate (plan §T7). It lives beside promote.go rather than inside it for the same reason
// evidence_tools_memory.go lives beside evidence.go, and it follows the family discipline exactly: the gate
// does its OWN re-derivation (never inheriting Complete()'s verdict), and it COMPOSES the epic underneath it
// rather than restating that epic's rules.

import (
	"encoding/json"
	"fmt"
)

// ToolsMemoryPromoteGate is the mechanical form of the E21 exit-gate sentence (plan §T7, §7): a release
// cannot be tagged while either crown guard is red. It has four clauses:
//
//  1. the bundle must carry EXACTLY ONE complete ToolsMemoryProof;
//
//  2. the STORED-SEARCH-BYTE count is RE-DERIVED here, from the persisted surface the proof carries, in both
//     directions — the search results must be findable in what reached the MODEL (or a zero over an empty
//     search certifies nothing) and findable NOWHERE in what the run WROTE DOWN. This is M5, and M5 is not
//     our preference: "You must not store or copy any of the data retrieved from this API";
//
//  3. the MENTION tokens are RE-DERIVED from the answer's own blocks: every `<@…>` on the wire must be the
//     delivery row's frozen requester id. A model that talked our renderer into minting somebody else's id
//     is E20 T1's broadcast hole in its most targeted form, and it is refused from the bytes rather than
//     from the counter;
//
//  4. NO TIER MAY ADVANCE, and no `mcp` capability is opened — enforced by the composed E20 gate (which
//     composes E19's mount derivation, E17's tier table and the eval numbers underneath it). Composing
//     rather than re-deriving keeps ONE owner of each rule.
//
// THE TIER DECISION IS ARGUED, NOT ASSUMED. The counter-argument is real — "tools work now and the workspace
// is searchable, so the surface is ready for stable" — and it is REFUSED for three reasons that belong in
// the code and not only in a commit message:
//
//	(1) §6 leg 1 is still OPEN. A real workspace is connected, and there is still NO CAPTURED RECEIPT; the
//	    proof's Peer is structurally the literal "fake" and no code in this repository can produce otherwise.
//	(2) E21's newest dependency is SLACK'S NEWEST API. The Real-time Search API went GA in February 2026 and
//	    the fake standing in for it is the YOUNGEST fake in this tree — the least-weathered contract carrying
//	    the crown feature.
//	(3) E21 GROWS THE SURFACE AGAIN. A tool surface and a search surface are a wider attack area than a
//	    render surface. Raising a tier in the epic that grows the surface is precisely what this gate exists
//	    to prevent, and it is the same reasoning E20 recorded one epic earlier.
//
// A promote BEYOND rc inherits every refusal underneath and adds nothing of its own: `slack` caps at preview
// by construction (CapabilityOperatorLegs §6 leg 1), `knowledge-vector` is `disabled` by construction
// (capabilityUnservable — and M5 is now a SECOND, independent reason it must stay there), and no `mcp` key
// exists in CapabilityTierOrder at all. Opening one would move CapabilityClaimsDigest and redden every case
// checksum in nineteen committed bundles (§3.6 D9) to advertise something that is not a discovery surface.
func ToolsMemoryPromoteGate(raw []byte, target string) []Refusal {
	var m evidenceManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return []Refusal{{Detail: "manifest is not valid JSON: " + err.Error()}}
	}

	var refusals []Refusal

	var tm *ToolsMemoryProof
	claims := 0
	for _, c := range m.Cases {
		if c.ToolsMemoryClaim == "" {
			continue
		}
		claims++
		if tm == nil {
			tm = c.ToolsMemoryProof
		}
	}
	switch {
	case claims > 1:
		refusals = append(refusals, Refusal{Detail: fmt.Sprintf("%d tools_memory_claims in one manifest (want exactly 1): this gate judges the FIRST tools-memory proof, so a second could ride behind an honest one — one release, one re-derivation (plan §T7)", claims)})
	case tm == nil:
		refusals = append(refusals, Refusal{Detail: toolsMemoryIncomplete})
	default:
		// THE GATE'S OWN SWEEPS RUN FIRST, before Complete() is consulted, and the ORDER is the whole point.
		// Complete() re-derives the same counts, so putting it ahead would make every branch below
		// unreachable for exactly the inputs it exists to catch — a guard that cannot fire, which is this
		// repository's most-found defect. Running them first means a tag decision rests on THIS gate's
		// derivation (the E15 SF-4 shape) and the reader is told WHICH claim failed.
		refusals = append(refusals, toolsMemorySearchRefusals(tm)...)
		refusals = append(refusals, toolsMemoryMentionRefusals(tm)...)
		if !tm.Complete() {
			refusals = append(refusals, Refusal{Detail: toolsMemoryIncomplete})
		}
	}

	// Clause 4 — composed verbatim. E21 opens no capability and moves no tier, so it owns NO tier rule of its
	// own; it inherits E20's, which inherits E19's cross-bundle comparison against the committed E17 baseline.
	return append(refusals, AgentSurfacePromoteGate(raw, target)...)
}

const toolsMemoryIncomplete = "no COMPLETE ToolsMemoryProof (a deterministic, VISIBLE history fold with two bit-equal digests + a tool advertised and dispatched through the REAL Orchestrator + external output that gained ZERO authority + ZERO search bytes stored, re-derived from the persisted surface + every mention token minted by OUR renderer from the frozen requester id + the canonical vendor contract ledger) — a release whose untrusted-input guards are red cannot be tagged (plan §T7, §7 exit gate)"

// toolsMemorySearchRefusals re-derives M5 from the carried bytes: the search results must be findable in what
// reached the model and findable NOWHERE in what the run persisted.
func toolsMemorySearchRefusals(tm *ToolsMemoryProof) []Refusal {
	reached, err := SweepSearchBytes(tm.SearchResultNeedles, tm.SearchReachedTheModel)
	if err != nil {
		return []Refusal{{Detail: "the tools-memory proof's search transcript cannot be swept, so \"nothing was stored\" is unverifiable and the tag is REFUSED (fail closed): " + err.Error()}}
	}
	if len(reached) != len(tm.SearchResultNeedles) {
		return []Refusal{{Detail: fmt.Sprintf(
			"only %d of %d search result(s) can be found in what reached the model — a bundle claiming ZERO stored bytes over a search that produced nothing findable is VACUOUS, and a guard that has never found anything is not a guard (plan §T7)",
			len(reached), len(tm.SearchResultNeedles))}}
	}
	stored, err := SweepSearchBytes(tm.SearchResultNeedles, tm.PersistedSurface)
	if err != nil {
		return []Refusal{{Detail: "the tools-memory proof's persisted surface cannot be swept, so the stored-byte count is unverifiable and the tag is REFUSED (fail closed): " + err.Error()}}
	}
	if len(stored) != 0 {
		return []Refusal{{Detail: fmt.Sprintf(
			"%d search result(s) were found in what the run PERSISTED: %v — the Real-time Search API's own terms say \"You must not store or copy any of the data retrieved from this API\", and a bundle declaring %d stored bytes over these bytes is fabricated (§3.5 M5, plan §2)",
			len(stored), stored, tm.StoredSearchBytes)}}
	}
	return nil
}

// toolsMemoryMentionRefusals re-derives the mention singularity from the answer's blocks. It decodes first,
// because encoding/json escapes `<` and a raw-substring scan over marshalled blocks could never fire.
func toolsMemoryMentionRefusals(tm *ToolsMemoryProof) []Refusal {
	mentions, err := SweepMentions(tm.AnswerBlocks)
	if err != nil {
		return []Refusal{{Detail: "the tools-memory proof's answer blocks cannot be swept, so the mention count is unverifiable and the tag is REFUSED (fail closed): " + err.Error()}}
	}
	if len(mentions) == 0 {
		return []Refusal{{Detail: "the tools-memory proof's answer blocks carry NO mention at all, so the sweep never demonstrated it could FIND one — the \"only our renderer mints a mention\" claim would be certified by an answer that never mentioned anybody (plan §T7)"}}
	}
	ours := mentionToken + tm.RequesterUserID + ">"
	for _, mention := range mentions {
		if mention != ours {
			return []Refusal{{Detail: fmt.Sprintf(
				"the answer carries the mention %q, which is not the delivery row's frozen requester id %q — the renderer holds exactly ONE identity and takes none from the model, so a second one on the wire means the model chose who to summon (plan §2, §T7)",
				mention, ours)}}
		}
	}
	if len(mentions) != tm.MentionsMinted {
		return []Refusal{{Detail: fmt.Sprintf(
			"the answer blocks carry %d mention(s) but the proof declares %d — the count is RE-DERIVED from the bytes, never read off the manifest (plan §T7)",
			len(mentions), tm.MentionsMinted)}}
	}
	return nil
}
