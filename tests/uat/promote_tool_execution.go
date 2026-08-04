package uat

// The Faz A.3 promote gate. It lives beside promote.go for the same reason evidence_tool_execution.go lives
// beside evidence.go, and it follows the family discipline exactly: the gate does its OWN re-derivation
// (never inheriting Complete()'s verdict), and it COMPOSES the release underneath it rather than restating
// that release's rules.

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// ToolExecutionPromoteGate is the mechanical form of the Faz A.3 exit-gate sentence: a release cannot be
// tagged while any of this phase's guards is red. It has eight clauses, and the last two are the ones that
// make this gate different from every other in the tree.
//
//  1. the bundle must carry EXACTLY ONE complete ToolExecutionProof;
//
//  2. SOMETHING OTHER THAN THIS PROCESS RAN THE COMMAND. The placement ledger must put at least one verb on
//     the machine, and its declared counts must equal what its rows hold. The control-plane count is NOT
//     required to be zero: A.3 left named surfaces behind (approval push, changeset, finalize, snapshot) and
//     a gate that could only accept "all of it moved" would force the next release to drop them from the
//     record rather than carry them;
//
//  3. NOTHING FELL BACK TO THIS HOST'S DISK, AND EVERY ROW WAS PERTURBED. The zero is the claim; the
//     perturbation is what makes the zero mean something. This repository measured five assertions in one
//     day that passed or failed for reasons unrelated to what they claimed, and the sharpest of them was a
//     refusal test that stayed GREEN because its own fixture made the case it existed to catch unreachable.
//     A fallback row with no perturbation and no line it reddened is that shape exactly;
//
//  4. ALL THREE BACKGROUND VERBS ARE ADDRESSED TO THE MACHINE, and no signal is sent to one that could not
//     be reached. The second half is a security clause rather than a correctness one: a pgid is a small
//     integer and this reaper spans tenants, so a signal on a handle we cannot prove is ours need not have
//     landed on a Palai process at all;
//
//  5. THE RUNNER'S COMPOSITION ROOT CAN BUILD BOTH POSTURES. With one, a Linux pool's commands are still
//     executing beside the control plane and this phase's own exit criterion — `faz-0-uname-proof.md`'s
//     second row answering `Linux` — is unreachable BY CONSTRUCTION rather than merely unmeasured. T1
//     shipped in exactly that state and said so; T3 closed it;
//
//  6. THE `uname` LEDGER MUST BE HONEST ABOUT WHAT IS MISSING. At least one leg measured, and every
//     OUTSTANDING leg carrying the reason it is absent. THIS GATE DOES NOT REQUIRE THE LEGS TO BE GREEN and
//     that is a deliberate refusal to overstate in the other direction: A.3's own exit criterion is NOT met,
//     the release says so, and a gate that refused the bundle for it would leave the phase with no record at
//     all — which is worse than a record that names its own hole;
//
//  7. THE SUPERSEDED LEDGER IS NOT EMPTY, AND EVERY SYMBOL IT NAMES IS ACTUALLY GONE. This is the clause
//     this bundle exists for. A release that supersedes a PUBLISHED ceiling owes a record naming it — and
//     the record is checkable rather than assertable because each row names the symbol the old reasoning
//     RESTED ON. `runner-fleet-0.1.0` grounded "every tool still runs in the control plane's own process" on
//     `orch.SetShellRunner(shellRunnerFromEnv())`; A.3 T3 deleted it. A row claiming a supersession whose
//     symbol is still in the tree is refused, because then the old ceiling may well still stand;
//
//  8. NO TIER MAY ADVANCE — enforced by the composed E28 gate (which composes E26's, E25's, E23's, E22's,
//     E21's, E20's, E19's mount derivation, E17's tier table and the eval numbers underneath it). Composing
//     rather than re-deriving keeps ONE owner of each rule.
//
// WHY IT COMPOSES E28. A.3 forked from `main` after `fleet-console-0.1.0` merged and derives its inherited
// case set from that release, so an A.3 bundle also carries the E28 fleet-console claim, the E26 background
// claim, the E25 admin-console claim, the E23 tool-approval claim and everything under them. Without this
// clause it would reroute to FleetConsolePromoteGate, which knows nothing about the placement ledger, the
// no-fallback half, the background addressing, the outstanding `uname` legs or the superseded ceilings, and
// would pass a bundle that dropped all five: every guard in this file would be optional in practice.
//
// THE TIER DECISION IS ARGUED, NOT ASSUMED, and this phase's counter-argument is the strongest one any
// release in this tree has had for moving `apple-build`:
//
//	"The whole reason `apple-build` was disabled is that a Mac pool routed a run's ENGINE and every tool
//	 still executed in the control plane. A.3 fixed exactly that, and the runner now runs natively on a
//	 Mac. The blocker named in the tier table is gone."
//
// It is REFUSED for three reasons:
//
//	(1) NOTHING WAS PROVEN THROUGH A RUN. Every leg of the chain is proven by composition; no single
//	    transcript goes model → engine → `tool.request` → dispatch → `exec.request` → machine. A tier is a
//	    statement about what a user gets, and a user gets runs;
//	(2) `xcodebuild` WAS NEVER EXECUTED ON A MAC BY THIS PHASE. The `uname` ledger's Mac leg answers
//	    `Darwin` through the posture the native runner builds — which is the executor, not a build — and the
//	    Linux leg was never taken at all;
//	(3) A DECLARED POSTURE IS STILL COMPARED AND NEVER ATTESTED (`FLT-P2`). A.3 moved WHERE a command runs;
//	    it added no attestation of WHAT the machine is, so an `unsandboxed-host` pool still does not stop a
//	    Linux container calling itself one.
//
// WHAT WOULD HAVE HAD TO BE TRUE to move it: one run, on a Mac pool, whose `xcodebuild` receipt came back
// from the machine — the leg this release names as outstanding rather than reports as zero.
func ToolExecutionPromoteGate(raw []byte, target string) []Refusal {
	var m evidenceManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return []Refusal{{Detail: "manifest is not valid JSON: " + err.Error()}}
	}

	var refusals []Refusal

	var tp *ToolExecutionProof
	claims := 0
	for _, c := range m.Cases {
		if c.ToolExecutionClaim == "" {
			continue
		}
		claims++
		if tp == nil {
			tp = c.ToolExecutionProof
		}
	}
	switch {
	case claims > 1:
		refusals = append(refusals, Refusal{Detail: fmt.Sprintf("%d tool_execution_claims in one manifest (want exactly 1): this gate judges the FIRST tool-execution proof, so a second could ride behind an honest one — one release, one re-derivation", claims)})
	case tp == nil:
		refusals = append(refusals, Refusal{Detail: toolExecutionIncomplete})
	default:
		// THE GATE'S OWN SWEEPS RUN FIRST, before Complete() is consulted, and the ORDER is the whole point.
		// Complete() re-derives the same counts, so putting it ahead would make every branch below
		// unreachable for exactly the inputs it exists to catch — a guard that cannot fire, which is this
		// repository's most-found defect. Running them first means a tag decision rests on THIS gate's
		// derivation and the reader is told WHICH claim failed.
		refusals = append(refusals, toolExecutionPlacementRefusals(tp)...)
		refusals = append(refusals, toolExecutionFallbackRefusals(tp)...)
		refusals = append(refusals, toolExecutionBackgroundRefusals(tp)...)
		refusals = append(refusals, toolExecutionPostureRefusals(tp)...)
		refusals = append(refusals, toolExecutionUnameRefusals(tp)...)
		refusals = append(refusals, toolExecutionSupersededRefusals(tp)...)
		if !tp.Complete() {
			refusals = append(refusals, Refusal{Detail: toolExecutionIncomplete})
		}
	}

	// Clause 8 — composed verbatim. A.3 opens no capability and moves no tier, so it owns NO tier rule of
	// its own; it inherits E28's, which inherits E26's, E25's, E23's, E22's, E21's, E20's and E19's
	// cross-bundle comparison against the committed extensions-0.1.0 baseline.
	return append(refusals, FleetConsolePromoteGate(raw, target)...)
}

const toolExecutionIncomplete = "no COMPLETE ToolExecutionProof (a placement ledger putting at least one tool verb on the machine, with its declared machine and control-plane counts equal to its rows + ZERO tools that fell back to this host's disk under a withheld machine answer, over rows that EACH name the perturbation that reddened their own line + all three background verbs addressed to the machine and ZERO signals to a machine that could not be reached + BOTH shell postures on the runner's composition root, or a Linux pool's commands never leave this process + at least one MEASURED `uname` leg and a reason on every OUTSTANDING one + the seven tasks' own ceilings + at least one SUPERSEDED published ceiling with the symbol its reasoning rested on + the canonical contract ledger) — a release whose placement, no-fallback, background-addressing, posture, honesty or supersession guard is red cannot be tagged"

// toolExecutionPlacementRefusals re-derives (a). The claim is that something OTHER THAN THIS PROCESS ran the
// command; a ledger with no machine row does not make it.
func toolExecutionPlacementRefusals(tp *ToolExecutionProof) []Refusal {
	onMachine, inCP, tests, err := SweepToolExecutionPlacement(tp.PlacementLedger)
	if err != nil {
		return []Refusal{{Detail: "the tool-execution proof's placement ledger cannot be swept, so \"the tool runs on the attempt's machine\" is unverifiable and the tag is REFUSED (fail closed): " + err.Error()}}
	}
	switch {
	case onMachine < 1:
		return []Refusal{{Detail: "the placement ledger puts NO tool verb on the machine — this phase's entire subject is WHERE a command executes, and a release claiming the move with every verb still in the control plane certifies the state E24 already published"}}
	case tp.VerbsOnTheMachine != onMachine || tp.VerbsInTheControlPlane != inCP:
		return []Refusal{{Detail: fmt.Sprintf(
			"the placement proof DECLARES %d verbs on the machine and %d in the control plane while its own ledger holds %d and %d — a declared count that the carried bytes do not support is the one number in this bundle nobody re-derived",
			tp.VerbsOnTheMachine, tp.VerbsInTheControlPlane, onMachine, inCP)}}
	case len(tests) != onMachine+inCP:
		return []Refusal{{Detail: "a placement row names no shipped test, so it records a belief about where a verb runs rather than a measurement of it"}}
	}
	return nil
}

// toolExecutionFallbackRefusals re-derives (b) — the negative half, and the perturbation requirement is the
// clause rather than a nicety.
func toolExecutionFallbackRefusals(tp *ToolExecutionProof) []Refusal {
	fellBack, perturbed, rows, err := SweepToolExecutionFallbacks(tp.FallbackLedger)
	if err != nil {
		return []Refusal{{Detail: "the tool-execution proof's fallback ledger cannot be swept, so \"nothing falls back to this host's disk\" is unverifiable and the tag is REFUSED (fail closed): " + err.Error()}}
	}
	switch {
	case rows < 1:
		return []Refusal{{Detail: "the fallback ledger is EMPTY. A green \"the tool ran on the machine\" says a tool was called; it does not say the tool would have refused had the machine not answered. Without this half the placement ledger is satisfied by a build that tries the machine and quietly uses the local disk when it does not reply"}}
	case fellBack != 0:
		return []Refusal{{Detail: fmt.Sprintf(
			"%d tool(s) FELL BACK to this host's disk when the machine's answer was withheld. That is the defect this phase exists to remove, not a degraded mode: a run whose files silently landed beside the control plane would look successful and would have built nothing on the machine the operator chose",
			fellBack)}}
	case perturbed != rows:
		return []Refusal{{Detail: fmt.Sprintf(
			"%d of %d fallback rows carry a perturbation and the line it reddened. A refusal nobody perturbed may be answering for a reason unrelated to what it claims — this repository measured five such assertions in one day, and the sharpest stayed GREEN under the very perturbation it existed to catch because its own fixture had made that case unreachable",
			perturbed, rows)}}
	}
	return nil
}

// toolExecutionBackgroundRefusals re-derives (c).
func toolExecutionBackgroundRefusals(tp *ToolExecutionProof) []Refusal {
	onMachine, signals, err := SweepToolExecutionBackground(tp.BackgroundLedger)
	if err != nil {
		return []Refusal{{Detail: "the tool-execution proof's background ledger cannot be swept, so \"background is addressed to the machine\" is unverifiable and the tag is REFUSED (fail closed): " + err.Error()}}
	}
	switch {
	case signals != 0:
		return []Refusal{{Detail: fmt.Sprintf(
			"%d signal(s) were sent to a machine that could not be reached. A pgid is a small integer and this reaper spans tenants, so a signal on a handle we cannot prove is ours need not have landed on a Palai process at all — an orphan of ours is a better outcome than a stranger's dead process",
			signals)}}
	case onMachine < ToolExecutionBackgroundVerbs:
		return []Refusal{{Detail: fmt.Sprintf(
			"only %d of the %d background verbs (start, probe, kill) are addressed to the machine. A HALF-MOVED background is worse than an unmoved one: the row names a machine the signal never reached, so a model is told a build finished while it is still compiling",
			onMachine, ToolExecutionBackgroundVerbs)}}
	case tp.BackgroundVerbsOnTheMachine != onMachine || tp.SignalsToAnUnreachableMachine != signals:
		return []Refusal{{Detail: "the background proof's declared counters disagree with its own ledger"}}
	}
	return nil
}

// toolExecutionPostureRefusals re-derives (d).
func toolExecutionPostureRefusals(tp *ToolExecutionProof) []Refusal {
	postures, err := SweepToolExecutionPostures(tp.PostureLedger)
	if err != nil {
		return []Refusal{{Detail: "the tool-execution proof's posture ledger cannot be swept, so the tag is REFUSED (fail closed): " + err.Error()}}
	}
	if len(postures) != ToolExecutionPosturesRequired || tp.PosturesTheRunnerCanBuild != len(postures) {
		return []Refusal{{Detail: fmt.Sprintf(
			"the runner's composition root builds %v (%d of %d postures). With one, a Linux pool's commands are still executing beside the control plane and this phase's exit criterion — `faz-0-uname-proof.md`'s second row answering `Linux` — is unreachable BY CONSTRUCTION rather than merely unmeasured. T1 shipped in exactly that state and recorded it; a release may not un-record it",
			postures, len(postures), ToolExecutionPosturesRequired)}}
	}
	return nil
}

// toolExecutionUnameRefusals re-derives (e).
//
// IT DOES NOT REQUIRE THE LEGS TO BE GREEN, and that is the honest direction. A.3's own exit criterion is
// not met; refusing the bundle for it would leave the phase with no record at all, which is worse than a
// record naming its own hole. What IS refused is a rounded absence: an outstanding leg that says only that
// it is missing invites a reader to assume it was tried.
func toolExecutionUnameRefusals(tp *ToolExecutionProof) []Refusal {
	measured, outstanding, err := SweepToolExecutionUname(tp.UnameLedger)
	if err != nil {
		return []Refusal{{Detail: "the tool-execution proof's `uname` ledger cannot be swept, so this release's own honesty about what it did not measure is unverifiable and the tag is REFUSED (fail closed): " + err.Error()}}
	}
	if measured < 1 {
		return []Refusal{{Detail: "the `uname` ledger carries NO measured leg. This phase was cut to turn that proof file green; a release that measured none of it has no measurement to report, and an all-outstanding ledger is a plan rather than evidence"}}
	}
	if tp.UnameLegsMeasured != measured || tp.UnameLegsOutstanding != outstanding {
		return []Refusal{{Detail: fmt.Sprintf(
			"the `uname` proof DECLARES %d measured and %d outstanding while its own ledger holds %d and %d",
			tp.UnameLegsMeasured, tp.UnameLegsOutstanding, measured, outstanding)}}
	}
	return nil
}

// toolExecutionSupersededRefusals re-derives (g), AND IT IS THE CLAUSE THIS BUNDLE EXISTS FOR.
//
// A published ceiling is never edited — `runner-fleet-0.1.0`'s sentence was TRUE when it shipped, and
// rewriting it would move an anchor 63 case checksums recompute against and forge a record besides. What a
// later release owes is a NEW record naming the old one. The field that makes that record checkable rather
// than assertable is `rested_on`: the symbol the old reasoning stood on. If it is still in the tree, the old
// ceiling may well still stand, and a supersession claiming otherwise is the exact defect this repository
// keeps finding — a sentence a reader accepts INSTEAD of reading the caller.
func toolExecutionSupersededRefusals(tp *ToolExecutionProof) []Refusal {
	symbols, err := SweepToolExecutionSuperseded(tp.SupersededLedger)
	if err != nil {
		return []Refusal{{Detail: "the tool-execution proof's superseded ledger cannot be swept, so the record this release exists to write is unverifiable and the tag is REFUSED (fail closed): " + err.Error()}}
	}
	if len(symbols) < 1 {
		return []Refusal{{Detail: "the superseded ledger is EMPTY, which leaves this bundle with no reason to be a bundle. The record it is cut to write is that `runner-fleet-0.1.0`'s published ceiling — \"every tool still runs in the control plane's own process\" — no longer holds; a release that moved the execution and did not say which published sentence it falsified leaves the tree with two records that disagree and no note saying which is later"}}
	}
	if tp.CeilingsSuperseded != len(symbols) {
		return []Refusal{{Detail: fmt.Sprintf("the proof DECLARES %d superseded ceilings while its ledger holds %d", tp.CeilingsSuperseded, len(symbols))}}
	}
	// A supersession must not name a symbol it left behind. The check is on the DECLARED flag rather than a
	// tree walk, because this gate is pure and runs over committed bytes — the tree walk lives in
	// tests/uat/tool-execution, where it can grep, and it is the half that would catch a stale `true`.
	var rows []toolExecutionSupersededRow
	if err := json.Unmarshal(tp.SupersededLedger, &rows); err != nil {
		return []Refusal{{Detail: "the superseded ledger decoded for the sweep and not for this clause: " + err.Error()}}
	}
	var refusals []Refusal
	for _, r := range rows {
		if !r.RestedOnGone {
			refusals = append(refusals, Refusal{Detail: fmt.Sprintf(
				"the supersession of %s names `%s` as the symbol its old reasoning rested on and does NOT claim it is gone. Then the earlier ceiling may still stand, and this row is an opinion about a published record rather than a supersession of it",
				r.Release, r.RestedOn)})
		}
		if strings.TrimSpace(r.Evidence) == "" {
			refusals = append(refusals, Refusal{Detail: "a superseded row carries no evidence, so its claim about a shipped release rests on this sentence alone"})
		}
	}
	return refusals
}

// ToolExecutionSupersededSymbols is the list of symbols the superseded ledger claims are gone, exported so
// the release package can walk the TREE for them. The gate above is pure over committed bytes; the walk is
// the other direction, and this repository's own rule is that an inward-facing absence needs the canonical
// list while an outward-facing one needs the walk. This is both, which is why it is checked twice.
func ToolExecutionSupersededSymbols() ([]string, error) {
	symbols, err := SweepToolExecutionSuperseded(json.RawMessage(ToolExecutionSupersededLedger))
	if err != nil {
		return nil, err
	}
	slices.Sort(symbols)
	return slices.Compact(symbols), nil
}
