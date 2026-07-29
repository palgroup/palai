package uat

// The E23 T7 promote gate (plan §T7). It lives beside promote.go rather than inside it for the same reason
// evidence_tool_approval.go lives beside evidence.go, and it follows the family discipline exactly: the gate
// does its OWN re-derivation (never inheriting Complete()'s verdict), and it COMPOSES the epic underneath it
// rather than restating that epic's rules.

import (
	"encoding/json"
	"fmt"
	"slices"
)

// ToolApprovalPromoteGate is the mechanical form of the E23 exit-gate sentence (plan §T7, §7): a release
// cannot be tagged while any of the four crown guards is red. It has five clauses:
//
//  1. the bundle must carry EXACTLY ONE complete ToolApprovalProof;
//
//  2. the UNGOVERNED-SIDE-EFFECT count is RE-DERIVED here, from the call ledger the proof carries, and the
//     ledger must show all four shapes — an approve that RAN, a deny that did not, an EXPIRY that did not,
//     and an ungated call that ran untouched. It is the crown claim: a side-effecting tool call cannot run
//     without a human, and the gate is declared at REGISTRATION rather than guessed per call;
//
//  3. the SCREEN-AUTHORSHIP fence is RE-DERIVED in both directions. The MCP server's `description` and
//     `title` and the model's prose must be findable in what ARRIVED FROM OUTSIDE and findable NOWHERE on
//     the approval screen. This is the cheapest security test in the epic and the answer to the next reader
//     who proposes showing the tool's description "because it helps the user": the vendor's own page
//     recommends those two fields for display and, in the same document, says clients "MUST consider tool
//     annotations to be untrusted unless they come from trusted servers" (§3.5 P3/P4);
//
//  4. the PARK-AND-EXPIRY counts are RE-DERIVED from the run ledger: zero runs went terminal while a human
//     still owed an answer, and zero runs were left WAITING after their approval expired. The second half
//     has no prior art in this tree and is the one worth reading — the worst failure of a gate is not
//     letting something through, it is holding a run open on a question nobody will ever answer;
//
//  5. NO TIER MAY ADVANCE — enforced by the composed E22 gate (which composes E21's, which composes E20's,
//     which composes E19's mount derivation, E17's tier table and the eval numbers underneath it).
//     Composing rather than re-deriving keeps ONE owner of each rule.
//
// THE TIER DECISION IS ARGUED, NOT ASSUMED, and this epic's counter-argument is the most sympathetic one
// these gates have faced, because it is a SECURITY argument rather than a capability one:
//
//	"Every side-effecting tool call now passes through a human's button. The screen is computed rather than
//	 narrated, the run parks instead of answering, the question expires instead of hanging, and the approver
//	 is a project policy rather than a Slack list. That is a security IMPROVEMENT, so `slack` should be
//	 `stable`."
//
// It is REFUSED for three reasons that belong in the code and not only in a commit message:
//
//	(1) §6 LEG 1 IS STILL OPEN AND E23 GREW IT. The leg is "a captured, re-derivable receipt from a REAL
//	    Slack workspace", and its scope now includes the approval message, the modal, an unauthorized click
//	    and a merge. ToolApprovalPeer is structurally the literal "fake" and no code in this repository can
//	    produce otherwise. WHEN A LEG GROWS YOU ARE FURTHER FROM THE FLIP, NOT NEARER.
//	(2) ADDING A CONTROL IS NOT EVIDENCE THAT THE CONTROL WORKS IN A REAL WORKSPACE. E22 did not advance a
//	    tier for DELETING a boundary; E23 does not advance one for ADDING a boundary — and the symmetry is
//	    the argument: both rest on the same fake peer, and what this gate measures is evidence, not claims.
//	(3) T5's CEILING IS OPEN BY CONSTRUCTION. A misconfigured registration — an MCP write tool published
//	    without `approval_required` — silently skips the gate, and NOTHING detects it (it cannot: automatic
//	    classification would have to trust the server's own annotations, which P3 forbids). A control with a
//	    silent bypass is not a control you promote a tier on.
//
// WHAT WOULD HAVE HAD TO BE TRUE to move `slack` to `stable`: (i) a CAPTURED, re-derivable receipt from a
// real workspace; (ii) the removal of Peer's structural "fake"; (iii) §6 leg 1 green in ONE run across
// E19's six, E20's four, E21's, E22's and E23's legs. None of the three exists.
//
// AND ONE MORE THING THIS GATE REFUSES TO LET A READER MISS, because it is the epic's own D1: the generic
// decision surface is INCOMPLETE. `slack.ToolApprovalMessage` and `coordinator.DecideToolApproval` have no
// production caller (measured 2026-07-29) — a gated MCP tool call parks its run and is released by the
// EXPIRY REAPER, because the shipped posting pump asks about PUBLICATIONS. That is carried as HIL-P8 and it
// is a second, independent reason no tier moves here.
func ToolApprovalPromoteGate(raw []byte, target string) []Refusal {
	var m evidenceManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return []Refusal{{Detail: "manifest is not valid JSON: " + err.Error()}}
	}

	var refusals []Refusal

	var ta *ToolApprovalProof
	claims := 0
	for _, c := range m.Cases {
		if c.ToolApprovalClaim == "" {
			continue
		}
		claims++
		if ta == nil {
			ta = c.ToolApprovalProof
		}
	}
	switch {
	case claims > 1:
		refusals = append(refusals, Refusal{Detail: fmt.Sprintf("%d tool_approval_claims in one manifest (want exactly 1): this gate judges the FIRST tool-approval proof, so a second could ride behind an honest one — one release, one re-derivation (plan §T7)", claims)})
	case ta == nil:
		refusals = append(refusals, Refusal{Detail: toolApprovalIncomplete})
	default:
		// THE GATE'S OWN SWEEPS RUN FIRST, before Complete() is consulted, and the ORDER is the whole point.
		// Complete() re-derives the same counts, so putting it ahead would make every branch below
		// unreachable for exactly the inputs it exists to catch — a guard that cannot fire, which is this
		// repository's most-found defect. Running them first means a tag decision rests on THIS gate's
		// derivation (the E15 SF-4 shape) and the reader is told WHICH claim failed.
		refusals = append(refusals, toolApprovalGateRefusals(ta)...)
		refusals = append(refusals, toolApprovalScreenRefusals(ta)...)
		refusals = append(refusals, toolApprovalParkRefusals(ta)...)
		refusals = append(refusals, toolApprovalMintRefusals(ta)...)
		if !ta.Complete() {
			refusals = append(refusals, Refusal{Detail: toolApprovalIncomplete})
		}
	}

	// Clause 5 — composed verbatim. E23 opens no capability and moves no tier, so it owns NO tier rule of
	// its own; it inherits E22's, which inherits E21's, which inherits E20's, which inherits E19's
	// cross-bundle comparison against the committed extensions-0.1.0 baseline.
	return append(refusals, CodeAndShipPromoteGate(raw, target)...)
}

const toolApprovalIncomplete = "no COMPLETE ToolApprovalProof (ZERO side-effecting tool calls that ran without a human, re-derived from a ledger that shows an approve, a deny, an EXPIRY and an ungated call + ZERO characters on the approval screen written by the model or by the server being called, re-derived in both directions over a screen that genuinely shows the identity and the arguments + ZERO runs terminal while a human still owed an answer and ZERO runs left waiting after an expiry + ZERO decisions an unauthorized principal got through, refused on BOTH surfaces + ZERO destination fields the model could fill in the three publish tools' input schemas + every actionable element on the screen built by interactions.go alone, recomputed from the source of two packages + the canonical vendor contract ledger) — a release whose human-in-the-loop gate or untrusted-input fence is red cannot be tagged (plan §T7, §7 exit gate)"

// toolApprovalGateRefusals re-derives the crown negative from the carried call ledger: nothing side-effecting
// ran without a human, and the ledger DEMONSTRATES that an approve runs it, a deny does not, an expiry does
// not, and an ungated tool is untouched by any of it.
func toolApprovalGateRefusals(ta *ToolApprovalProof) []Refusal {
	ungoverned, approved, denied, expired, ungated, err := SweepSideEffectsWithoutAHuman(ta.CallLedger)
	if err != nil {
		return []Refusal{{Detail: "the tool-approval proof's call ledger cannot be swept, so \"nothing side-effecting ran without a human\" is unverifiable and the tag is REFUSED (fail closed): " + err.Error()}}
	}
	if len(ungoverned) != 0 {
		return []Refusal{{Detail: fmt.Sprintf(
			"%d gated tool call(s) RAN with no human decision: %v — a tool declared approval_required at registration may be proposed and may not run, and a bundle declaring %d is fabricated (plan §2, §T7)",
			len(ungoverned), ungoverned, ta.SideEffectsWithoutAHuman)}}
	}
	switch {
	case approved < 1:
		return []Refusal{{Detail: "the call ledger contains no gated call an APPROVE actually ran — a zero over a corpus nobody ever approved is VACUOUS, and a gate that has never let anything through is indistinguishable from a broken tool (plan §T7)"}}
	case denied < 1:
		return []Refusal{{Detail: "the call ledger contains no DENIED call that was withheld — deny must PREVENT the side effect rather than record a verdict about it, and a ledger with no deny in it never demonstrated that (plan §T7)"}}
	case expired < 1:
		return []Refusal{{Detail: "the call ledger contains no EXPIRED call — an approval nobody presses must cancel the call, and a corpus where every question was answered never exercised the half that has no prior art in this tree (plan §T1, §T7)"}}
	case ungated < 1:
		return []Refusal{{Detail: "the call ledger contains no UNGATED call that simply ran — without one, \"a tool that declares no approval is bit-unchanged\" is unproven, and a gate that quietly caught everything would look identical to this one (plan §T1, §T7)"}}
	}
	return nil
}

// toolApprovalScreenRefusals re-derives the (b) fence, and the refusal text is written for the reader who
// arrives here after proposing to put the tool's description on the screen.
func toolApprovalScreenRefusals(ta *ToolApprovalProof) []Refusal {
	if len(ta.UntrustedTextNeedles) == 0 {
		return []Refusal{{Detail: "the tool-approval proof carries no untrusted-text needles, so the screen-authorship fence has nothing to look for and can never fail — the vacuous form of this claim (plan §T7)"}}
	}
	arrived, err := SweepSearchBytes(ta.UntrustedTextNeedles, ta.UntrustedTextArrived)
	if err != nil {
		return []Refusal{{Detail: "the tool-approval proof's arrived-from-outside bytes cannot be swept, so the screen-authorship fence is unverifiable and the tag is REFUSED (fail closed): " + err.Error()}}
	}
	if len(arrived) != len(ta.UntrustedTextNeedles) {
		return []Refusal{{Detail: fmt.Sprintf(
			"only %d of %d untrusted needles are findable in what ARRIVED FROM OUTSIDE — the zero on the screen is then a statement about text that was never delivered, which certifies nothing (plan §T7)",
			len(arrived), len(ta.UntrustedTextNeedles))}}
	}
	onScreen, err := SweepApprovalScreenAuthorship(ta.UntrustedTextNeedles, ta.ApprovalScreen)
	if err != nil {
		return []Refusal{{Detail: "the tool-approval proof's approval screen cannot be swept, so \"no byte from outside reached the screen\" is unverifiable and the tag is REFUSED (fail closed): " + err.Error()}}
	}
	if len(onScreen) != 0 {
		return []Refusal{{Detail: fmt.Sprintf(
			"%d string(s) written by the MODEL or by the SERVER BEING CALLED reached the approval screen: %v — the human decides from the CALL (identity from the executing lookup, label from the operator, arguments verbatim) and never from a description, because the vendor's own page recommends `title`/`description` for display and says in the same document that clients MUST treat a server's annotations as untrusted (§3.5 P3/P4); the proof declares %d (plan §2, §T7)",
			len(onScreen), onScreen, ta.ScreenCharactersFromTheModelOrServer)}}
	}
	if !toolApprovalScreenShowsTheCall(ta.ApprovalScreen) {
		return []Refusal{{Detail: "the approval screen shows neither the resolved identity, nor the operator's label, nor a value from inside the arguments — showing NOTHING is the cheapest way to show nothing forbidden, and a screen a human cannot decide from is the screen this epic exists to prevent (plan §2, §T7)"}}
	}
	return nil
}

// toolApprovalParkRefusals re-derives (c) and (d) from the run ledger.
func toolApprovalParkRefusals(ta *ToolApprovalProof) []Refusal {
	terminal, stillWaiting, parked, released, err := SweepRunsThatDidNotWait(ta.RunLedger)
	if err != nil {
		return []Refusal{{Detail: "the tool-approval proof's run ledger cannot be swept, so \"the run parks and does not wait forever\" is unverifiable and the tag is REFUSED (fail closed): " + err.Error()}}
	}
	if len(terminal) != 0 {
		return []Refusal{{Detail: fmt.Sprintf(
			"%d run(s) went TERMINAL while a human still owed an answer: %v — that is the E22 defect this epic exists to repair (the click then lands on guardRunActive, the route answers 503 and the question waits forever); the proof declares %d (plan §3.6 D2, §T7)",
			len(terminal), terminal, ta.RunsTerminalWhileWaiting)}}
	}
	if len(stillWaiting) != 0 {
		return []Refusal{{Detail: fmt.Sprintf(
			"%d run(s) are still WAITING after their approval expired: %v — expiry has two halves and this is the second, which had no implementation in this tree at all before E23: the worst state of a gate is not letting something through, it is holding a run open on a question nobody will ever answer; the proof declares %d (plan §2, §T1, §T7)",
			len(stillWaiting), stillWaiting, ta.RunsWaitingAfterExpiry)}}
	}
	if parked < 1 {
		return []Refusal{{Detail: "the run ledger contains no run actually PARKED on an open question — both zeros above are then free, since a corpus where nothing ever waited cannot show that waiting is safe (plan §T7)"}}
	}
	if released < 1 {
		return []Refusal{{Detail: "the run ledger contains no run an EXPIRY released — the reaper's second half is exactly what this bundle claims is new, and a ledger where no approval ever lapsed never exercised it (plan §T1, §T7)"}}
	}
	leaked, refusedOn, applied, err := SweepUnauthorizedDecisions(ta.DecisionLedger)
	if err != nil {
		return []Refusal{{Detail: "the tool-approval proof's decision ledger cannot be swept, so \"an unauthorized click decides nothing\" is unverifiable and the tag is REFUSED (fail closed): " + err.Error()}}
	}
	if len(leaked) != 0 {
		return []Refusal{{Detail: fmt.Sprintf(
			"%d decision(s) by an UNAUTHORIZED principal were applied: %v — the approver is a project policy checked in ApplyApprovalDecision, the one throat both surfaces pass through; the proof declares %d (plan §T2, §T7)",
			len(leaked), leaked, ta.UnauthorizedDecisionsApplied)}}
	}
	if applied < 1 || !slices.Contains(refusedOn, "http") || !slices.Contains(refusedOn, "slack") {
		return []Refusal{{Detail: fmt.Sprintf(
			"the decision ledger refuses an unauthorized principal on %v and applies %d authorized decision(s) — it must show BOTH surfaces refusing and at least one legitimate approval, or the check is consistent with a guard bolted onto a single caller rather than placed in the one function both pass through (plan §T2, §T7)",
			refusedOn, applied)}}
	}
	return nil
}

// toolApprovalMintRefusals recomputes the single mint from the SOURCE of two packages. It is wider than the
// adapter's own sweep on purpose (§3.6 D13: that one is `os.ReadDir(".")`, one directory), because the modal
// this epic added is OPENED from `extensions/` and a view built there would leave the narrow sweep green.
func toolApprovalMintRefusals(ta *ToolApprovalProof) []Refusal {
	minted, err := SweepActionableElements(ta.ApprovalScreen)
	if err != nil {
		return []Refusal{{Detail: "the approval screen cannot be swept for actionable elements, so the single-mint claim is unverifiable and the tag is REFUSED (fail closed): " + err.Error()}}
	}
	if len(minted) == 0 {
		return []Refusal{{Detail: "the approval screen carries NO actionable element — a human cannot decide from a screen with no button, and a sweep that finds nothing here cannot discriminate for the claims that rest on it (plan §T4, §T7)"}}
	}
	if ta.ActionableElementsMinted != len(minted) {
		return []Refusal{{Detail: fmt.Sprintf(
			"the approval screen re-derives %d actionable element(s) but the proof declares %d — the count comes from the bytes, never from the manifest (plan §T7)",
			len(minted), ta.ActionableElementsMinted)}}
	}
	mints, err := SweepActionableElementMints()
	if err != nil {
		return []Refusal{{Detail: "the actionable-element mints cannot be re-derived from source, so \"interactions.go is the only mint\" is unverifiable and the tag is REFUSED (fail closed): " + err.Error()}}
	}
	files := make([]string, 0, len(mints))
	for file := range mints {
		files = append(files, file)
	}
	slices.Sort(files)
	if len(files) != 1 || files[0] != toolApprovalMintFile {
		return []Refusal{{Detail: fmt.Sprintf(
			"actionable elements are built in %v; %s is the ONLY mint (plan §2) and every other one needs its own authorization path. This sweep is WIDER than the adapter's own (which reads a single directory, §3.6 D13) precisely so a modal view built under apps/control-plane/internal/extensions cannot pass",
			files, toolApprovalMintFile)}}
	}
	if !slices.Equal(ta.ActionableElementMintFiles, files) {
		return []Refusal{{Detail: fmt.Sprintf(
			"the mint sweep recomputes %v but the proof declares %v — the file list is re-derived from source, never read off the manifest (plan §T7)",
			files, ta.ActionableElementMintFiles)}}
	}
	return nil
}
