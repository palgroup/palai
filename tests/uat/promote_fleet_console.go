package uat

// The E28 T4 promote gate (plan §T4). It lives beside promote.go rather than inside it for the same reason
// evidence_fleet_console.go lives beside evidence.go, and it follows the family discipline exactly: the gate
// does its OWN re-derivation (never inheriting Complete()'s verdict), and it COMPOSES the epic underneath it
// rather than restating that epic's rules.

import (
	"encoding/json"
	"fmt"
	"slices"
)

// FleetConsolePromoteGate is the mechanical form of the E28 exit-gate sentence (plan §T4, §7): a release
// cannot be tagged while any of the fleet console's guards is red. It has nine clauses:
//
//  1. the bundle must carry EXACTLY ONE complete FleetConsoleProof;
//
//  2. A FLEET HAS A BIRTH PATH, AND THE POSTURE SET IS THE PROOF OF IT. A pool count alone would be
//     satisfied by the seed every tenant gets at birth — `InsertDefaultRunnerPool` wrote 'default',
//     'sandboxed-linux' and false as LITERALS for this repository's whole life. So the ledger must carry a
//     pool created through a PUBLIC surface with posture `unsandboxed-host`, which is the value the rented
//     Mac column was added for and which no code path in this tree could write before T1;
//
//  3. A MACHINE WAITED AND WAS ADMITTED FROM A SCREEN. E24 T6 shipped the approve route and proved it from a
//     component test; the thing that had never happened is an operator admitting a machine from the console,
//     into a pool the console created. `admitted_from=console` is required BY VALUE, because an epic whose
//     crown claim is a screen cannot certify it from a CLI transcript;
//
//  4. THE MINTED VALUE LANDS IN NONE OF FIVE SITES, MEASURED AFTER DECODING, AND EACH SITE NAMES A TOKEN THE
//     SAME SCAN DID FIND. A raw-byte sweep over encoded output can never fail — E14 T7 measured it, E20 T4
//     paid again — and a scan whose haystack was never read reports the same zero as one that was clean;
//
//  5. THE APPROVER LIST SURVIVES, AND THE REQUEST IS CHECKED BESIDE THE OUTCOME. This is the sharpest clause
//     in the gate: `PATCH /v1/projects/{id}` is an ASSIGNMENT (identity/store.go), so a request naming only
//     `pool` stores `approvers: null`, and `HIL-P11` measured that an empty approver list is PERMISSIVE —
//     the most innocent button an admin console can carry would open the approval gate to everyone. Checking
//     only the STORED outcome would pass on a server that merged, over a form still sending one field, so
//     the field count of the REQUEST is checked too;
//
//  6. EVERY DECLARED ROUTE IS SCANNED, and the two pages this epic opened are IN the ledger by name — a
//     count that rose says nothing about which page rose with it;
//
//  7. THE CONFIRMATION SPLIT IS REFUSED IN BOTH DIRECTIONS. WCAG 2.2 SC 3.3.4 offers three legs and asks for
//     one; a reversible action satisfies leg 1 and owes no dialog. A build that put every action behind an
//     alertdialog would not be more compliant, it would have stopped distinguishing — so an irreversible
//     action outside a dialog AND a reversible one inside it are both refusals;
//
//  8. THE SCREENS STATE THEIR OWN CEILINGS BY GAP ID, all four of them. The largest is `FLT-P15`, which
//     known-gaps itself says to read before any other `FLT-P*` because it bounds what all of them are worth:
//     a fleet screen showing three active machines and saying nothing about where a tool executes is silent
//     truncation on the fleet surface;
//
//  9. NO TIER MAY ADVANCE — enforced by the composed E26 gate (which composes E25's, which composes E23's,
//     E22's, E21's, E20's, E19's mount derivation, E17's tier table and the eval numbers underneath it).
//     Composing rather than re-deriving keeps ONE owner of each rule.
//
// WHY IT COMPOSES E26. E28 forked from `main` after `background-execution-0.1.0` merged and derives its
// inherited case set from that release, so an E28 bundle also carries the E26 background claim, the E25
// admin-console claim, the E23 tool-approval claim, the E22 code-and-ship claim, the E21 tools-memory claim,
// the E20 agent-surface claim and the E17 area claims. Without this clause it would reroute to
// BackgroundPromoteGate, which knows nothing about the birth path, the waiting room, the key scan, the policy
// equality, the route coverage, the confirmation split or the ceiling ids, and would pass it: every fleet
// console guard would be optional in practice.
//
// THE TIER DECISION IS ARGUED, NOT ASSUMED, and this epic's counter-argument is a strong one:
//
//	"The console now manages the fleet AND the policy document, proven on both the fake and the real
//	 profile, with every page axe-clean in both colour schemes. `console` should move."
//
// It is REFUSED for four reasons, and the fourth is specific to this epic:
//
//	(1) §6 leg 1 is still open — THERE IS NO DEPLOYED CONSOLE (capabilities_mount_test.go makes the console
//	    being a separate deployable the basis of this tier's derivation, and compose is not a deployment);
//	(2) THERE IS NO MANUAL SCREEN-READER PASS, and E28 added exactly the two things axe is weakest at — a
//	    MODAL DIALOG, whose focus trap is a FEEL, and a ONE-TIME VALUE ANNOUNCEMENT, whose correctness is a
//	    TIMING. Neither is visible to any scanner;
//	(3) E28 made the surface BIGGER: two pages, two components, three new routes — the same shape as E25
//	    closing preview in the epic that grew it;
//	(4) AND WITH `FLT-P15` OPEN, A FLEET SCREEN IMPLIES MORE THAN IT PROVES. A page listing pools, keys and
//	    machines reads as "these machines do the work"; every tool still executes in the control plane's own
//	    process. The screens say so in words, which is the honest mitigation — RAISING THE TIER WOULD
//	    FORMALISE THE IMPLICATION the sentences exist to prevent.
//
// WHAT WOULD HAVE HAD TO BE TRUE to move a tier here: a deployed console behind a real origin with a manual
// assistive-technology pass, or the execution relay that makes a Mac pool actually run `xcodebuild` on a Mac.
// Neither exists, and the §6 legs that would produce them are open.
func FleetConsolePromoteGate(raw []byte, target string) []Refusal {
	var m evidenceManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return []Refusal{{Detail: "manifest is not valid JSON: " + err.Error()}}
	}

	var refusals []Refusal

	var fp *FleetConsoleProof
	claims := 0
	for _, c := range m.Cases {
		if c.FleetConsoleClaim == "" {
			continue
		}
		claims++
		if fp == nil {
			fp = c.FleetConsoleProof
		}
	}
	switch {
	case claims > 1:
		refusals = append(refusals, Refusal{Detail: fmt.Sprintf("%d fleet_console_claims in one manifest (want exactly 1): this gate judges the FIRST fleet-console proof, so a second could ride behind an honest one — one release, one re-derivation (plan §T4)", claims)})
	case fp == nil:
		refusals = append(refusals, Refusal{Detail: fleetConsoleIncomplete})
	default:
		// THE GATE'S OWN SWEEPS RUN FIRST, before Complete() is consulted, and the ORDER is the whole point.
		// Complete() re-derives the same counts, so putting it ahead would make every branch below
		// unreachable for exactly the inputs it exists to catch — a guard that cannot fire, which is this
		// repository's most-found defect. Running them first means a tag decision rests on THIS gate's
		// derivation and the reader is told WHICH claim failed.
		refusals = append(refusals, fleetConsolePoolRefusals(fp)...)
		refusals = append(refusals, fleetConsoleWaitingRefusals(fp)...)
		refusals = append(refusals, fleetConsoleKeyScanRefusals(fp)...)
		refusals = append(refusals, fleetConsolePolicyRefusals(fp)...)
		refusals = append(refusals, fleetConsoleRouteRefusals(fp)...)
		refusals = append(refusals, fleetConsoleActionRefusals(fp)...)
		refusals = append(refusals, fleetConsoleCeilingRefusals(fp)...)
		refusals = append(refusals, fleetConsoleConformanceRefusals(fp)...)
		if !fp.Complete() {
			refusals = append(refusals, Refusal{Detail: fleetConsoleIncomplete})
		}
	}

	// Clause 9 — composed verbatim. E28 opens no capability and moves no tier, so it owns NO tier rule of its
	// own; it inherits E26's, which inherits E25's, which inherits E23's, E22's, E21's, E20's and E19's
	// cross-bundle comparison against the committed extensions-0.1.0 baseline.
	return append(refusals, BackgroundPromoteGate(raw, target)...)
}

const fleetConsoleIncomplete = "no COMPLETE FleetConsoleProof (a pool created through a PUBLIC surface with posture `unsandboxed-host` — the value no code path in this tree could write before E28 T1, and a SEEDED pool is evidence of the hole rather than of the fix + a machine that actually reached `pending` in a strict pool and at least one ADMITTED FROM THE CONSOLE + ZERO minted key values across five sites, each DECODED before it was scanned and each naming a harmless token it DID find + as many approver entries AFTER a policy write as before it, over requests that each carried all five policy fields, because asserting only the stored outcome passes on a server that merged + every route lib/routes.ts declares axe-scanned in every colour scheme, with the two pages this epic opened named in the ledger + every IRREVERSIBLE action behind an alertdialog and every REVERSIBLE one left on the native confirmation, refused in both directions because the claim is a DIFFERENCE + all four ceiling gap ids stated on screen + a conformance sweep comparing more collections than before this epic + the canonical published-contract ledger) — a release whose birth path, waiting room, minted-value, policy, accessibility, confirmation or ceiling guard is red cannot be tagged (plan §T4, §7 exit gate)"

// fleetConsolePoolRefusals re-derives (a). The posture set is the clause; the count alone is satisfied by a seed.
func fleetConsolePoolRefusals(fp *FleetConsoleProof) []Refusal {
	created, postures, surfaces, err := SweepFleetConsolePools(fp.PoolLedger)
	if err != nil {
		return []Refusal{{Detail: "the fleet-console proof's pool ledger cannot be swept, so \"a fleet has a birth path\" is unverifiable and the tag is REFUSED (fail closed): " + err.Error()}}
	}
	switch {
	case !slices.Contains(postures, "unsandboxed-host"):
		return []Refusal{{Detail: fmt.Sprintf(
			"the pool ledger created postures %v and NOT `unsandboxed-host` — that is the value migration 000045's CHECK declared and NO code path in this tree could write, so the column the rented Mac was added for had exactly one attainable value. A release that creates only `sandboxed-linux` pools certifies the hole this epic was opened to close (plan §0.2, §3.6 D1/D4, §T4)",
			postures)}}
	case len(surfaces) == 0 || created < 1:
		return []Refusal{{Detail: fmt.Sprintf(
			"the pool ledger records %d pool(s) over %d creation surface(s) — a birth path with no birth is the state this epic found the tree in (plan §T4)",
			created, len(surfaces))}}
	case fp.PoolsCreated != created || !slices.Equal(fp.PosturesCreated, postures):
		return []Refusal{{Detail: fmt.Sprintf(
			"the pool ledger re-derives %d pool(s) / postures %v but the proof declares %d / %v — the counts come from the rows, never from the manifest (plan §T4)",
			created, postures, fp.PoolsCreated, fp.PosturesCreated)}}
	}
	return nil
}

// fleetConsoleWaitingRefusals re-derives (b): the clause that makes this E28's claim rather than E24's.
func fleetConsoleWaitingRefusals(fp *FleetConsoleProof) []Refusal {
	pending, fromConsole, neverWaited, err := SweepFleetConsoleWaitingRoom(fp.WaitingRoomLedger)
	if err != nil {
		return []Refusal{{Detail: "the fleet-console proof's waiting-room ledger cannot be swept, so \"a machine waits where something can see it\" is unverifiable and the tag is REFUSED (fail closed): " + err.Error()}}
	}
	switch {
	case len(neverWaited) != 0:
		return []Refusal{{Detail: fmt.Sprintf(
			"%d machine(s) never actually reached `pending`: %v — a waiting room proven by a machine that walked straight through is a waiting room proven by nothing, and strict enrolment's whole content is that the machine is UNREACHABLE FROM A DIAL until a human answers (plan §T1, §T4)",
			len(neverWaited), neverWaited)}}
	case fromConsole < 1:
		return []Refusal{{Detail: fmt.Sprintf(
			"%d machine(s) waited and NONE was admitted from the console — E24 T6 already proved the approve route from a component test; what had never happened is an operator admitting a machine FROM A SCREEN, into a pool that screen created. An epic whose crown claim is a console cannot certify it from a CLI transcript (plan §T3, §T4)",
			pending)}}
	case fp.MachinesPending != pending || fp.MachinesAdmittedFromTheConsole != fromConsole:
		return []Refusal{{Detail: fmt.Sprintf(
			"the waiting-room ledger re-derives %d pending / %d admitted from the console but the proof declares %d / %d — the counts come from the rows, never from the manifest (plan §T4)",
			pending, fromConsole, fp.MachinesPending, fp.MachinesAdmittedFromTheConsole)}}
	}
	return nil
}

// fleetConsoleKeyScanRefusals re-derives (c).
func fleetConsoleKeyScanRefusals(fp *FleetConsoleProof) []Refusal {
	hits, probed, sites, err := SweepFleetConsoleKeyScan(fp.KeyScanLedger)
	if err != nil {
		return []Refusal{{Detail: "the fleet-console proof's key-scan ledger cannot be swept, so \"a minted value survives nowhere\" is unverifiable and the tag is REFUSED (fail closed): " + err.Error()}}
	}
	switch {
	case hits != 0:
		return []Refusal{{Detail: fmt.Sprintf(
			"a minted key VALUE was found %d time(s) — the server shows it in the 201 body and NOWHERE ELSE (`poolKeyView(item, false)` on every other read), and a browser that kept a copy would make that server-side rule decorative. The proof declares %d (plan §2, §T2, §T4)",
			hits, fp.KeyValuesFound)}}
	case probed != len(FleetConsoleKeyScanSites) || len(sites) != len(FleetConsoleKeyScanSites):
		return []Refusal{{Detail: fmt.Sprintf(
			"the key scan covers %d of %d sites — a site with no row is a site nobody scanned, and `later-response` in particular is the one the other four pass over: a value absent from the DOM can still be served back by a list call (plan §T2, §T4)",
			len(sites), len(FleetConsoleKeyScanSites))}}
	case fp.KeyValuesFound != hits:
		return []Refusal{{Detail: fmt.Sprintf(
			"the key-scan ledger re-derives %d hit(s) but the proof declares %d — the count comes from the rows, never from the manifest (plan §T4)",
			hits, fp.KeyValuesFound)}}
	}
	return nil
}

// fleetConsolePolicyRefusals re-derives (d) — the sharpest clause, and the one a security incident sits behind.
func fleetConsolePolicyRefusals(fp *FleetConsoleProof) []Refusal {
	before, after, partial, err := SweepFleetConsolePolicyWrites(fp.PolicyLedger)
	if err != nil {
		return []Refusal{{Detail: "the fleet-console proof's policy ledger cannot be swept, so \"a policy form writes the whole document\" is unverifiable and the tag is REFUSED (fail closed): " + err.Error()}}
	}
	switch {
	case before != after:
		return []Refusal{{Detail: fmt.Sprintf(
			"%d approver entries went into the policy writes and %d came out — `UpdateProjectPolicy` marshals the input struct and hands the bytes over with NO merge, so a request naming only `pool` stores `approvers: null`, and `HIL-P11` measured that an empty approver list PERMITS EVERY PRINCIPAL. The most innocent button an admin console can carry would have opened the approval gate to everyone, and the operator's own action would have SUCCEEDED while it happened (plan §3.6 D9, §T2, §T4)",
			before, after)}}
	case len(partial) != 0:
		return []Refusal{{Detail: fmt.Sprintf(
			"%d policy write(s) sent fewer than all %d fields: %v — and this is the half a stored-outcome assertion cannot see. A server that MERGED would let \"the approvers survived\" pass over a form still sending one field, so the next server change would break the screen silently (plan §T2, §T4)",
			len(partial), FleetConsolePolicyFields, partial)}}
	case fp.ApproverEntriesBefore != before || fp.ApproverEntriesAfter != after:
		return []Refusal{{Detail: fmt.Sprintf(
			"the policy ledger re-derives %d approver entries before / %d after but the proof declares %d / %d — an EQUALITY written by hand is an equality nobody checked (plan §T4)",
			before, after, fp.ApproverEntriesBefore, fp.ApproverEntriesAfter)}}
	}
	return nil
}

// fleetConsoleRouteRefusals re-derives (e).
func fleetConsoleRouteRefusals(fp *FleetConsoleProof) []Refusal {
	declared, scanned, unscanned, err := SweepFleetConsoleRoutes(fp.RouteLedger)
	if err != nil {
		return []Refusal{{Detail: "the fleet-console proof's route ledger cannot be swept, so \"every page is scanned\" is unverifiable and the tag is REFUSED (fail closed): " + err.Error()}}
	}
	switch {
	case len(unscanned) != 0:
		return []Refusal{{Detail: fmt.Sprintf(
			"%d declared route(s) were not axe-scanned in every colour scheme: %v — lib/routes.ts is the single source of the navigation AND of the sweep, so an unscanned page is meant to be structurally impossible; one appearing here means that derivation was broken rather than that a scan was forgotten (plan §T4)",
			len(unscanned), unscanned)}}
	case declared != scanned:
		return []Refusal{{Detail: fmt.Sprintf("the route ledger declares %d route(s) and %d were scanned (plan §T4)", declared, scanned)}}
	case fp.RoutesDeclared != declared || fp.RoutesAxeScanned != scanned:
		return []Refusal{{Detail: fmt.Sprintf(
			"the route ledger re-derives %d declared / %d scanned but the proof declares %d / %d — the counts come from the rows, never from the manifest (plan §T4)",
			declared, scanned, fp.RoutesDeclared, fp.RoutesAxeScanned)}}
	}
	return nil
}

// fleetConsoleActionRefusals re-derives (f), in BOTH directions.
func fleetConsoleActionRefusals(fp *FleetConsoleProof) []Refusal {
	guarded, native, misplaced, err := SweepFleetConsoleActions(fp.ActionLedger)
	if err != nil {
		return []Refusal{{Detail: "the fleet-console proof's action ledger cannot be swept, so \"an irreversible action gets a different confirmation\" is unverifiable and the tag is REFUSED (fail closed): " + err.Error()}}
	}
	switch {
	case len(misplaced) != 0:
		return []Refusal{{Detail: fmt.Sprintf(
			"%d action(s) are on the wrong side of the confirmation split: %v — WCAG 2.2 SC 3.3.4 offers THREE legs and asks for ONE, so a reversible action satisfies leg 1 (Reversible) and owes no dialog while an irreversible one can only reach leg 3 (Confirmed), whose word REVIEWING is a data call. A build that put everything behind an alertdialog would not be more compliant; it would have stopped distinguishing, which is why this refuses both directions (plan §3.5 W1/W2, §T4)",
			len(misplaced), misplaced)}}
	case fp.IrreversibleActionsBehindAnAlertDialog != guarded || fp.ReversibleActionsOnTheNativeConfirm != native:
		return []Refusal{{Detail: fmt.Sprintf(
			"the action ledger re-derives %d irreversible action(s) behind an alertdialog / %d reversible on the native confirm but the proof declares %d / %d — the counts come from the rows, never from the manifest (plan §T4)",
			guarded, native, fp.IrreversibleActionsBehindAnAlertDialog, fp.ReversibleActionsOnTheNativeConfirm)}}
	}
	return nil
}

// fleetConsoleCeilingRefusals re-derives (g).
func fleetConsoleCeilingRefusals(fp *FleetConsoleProof) []Refusal {
	onScreen, missing, err := SweepFleetConsoleCeilings(fp.CeilingLedger)
	if err != nil {
		return []Refusal{{Detail: "the fleet-console proof's ceiling ledger cannot be swept, so \"a screen says what it does not show\" is unverifiable and the tag is REFUSED (fail closed): " + err.Error()}}
	}
	switch {
	case len(missing) != 0:
		return []Refusal{{Detail: fmt.Sprintf(
			"%d ceiling(s) this epic owes are on no screen: %v — a fleet page showing three active machines and saying nothing about where a tool EXECUTES is silent truncation on the fleet surface, and `FLT-P15` is the row known-gaps itself says to read before any other `FLT-P*` because it bounds what all of them are worth (plan §2, §3.6 D17, §T4)",
			len(missing), missing)}}
	case !slices.Equal(fp.CeilingsOnScreen, onScreen):
		return []Refusal{{Detail: fmt.Sprintf(
			"the ceiling ledger re-derives %v but the proof declares %v — the ids come from the rows, never from the manifest (plan §T4)",
			onScreen, fp.CeilingsOnScreen)}}
	}
	return nil
}

// fleetConsoleConformanceRefusals re-derives (h).
func fleetConsoleConformanceRefusals(fp *FleetConsoleProof) []Refusal {
	compared, err := SweepFleetConsoleConformance(fp.ConformanceLedger)
	if err != nil {
		return []Refusal{{Detail: "the fleet-console proof's conformance ledger cannot be swept, so \"the fake still mirrors the real one\" is unverifiable and the tag is REFUSED (fail closed): " + err.Error()}}
	}
	if fp.ConformanceCollectionsCompared != len(compared) {
		return []Refusal{{Detail: fmt.Sprintf(
			"the conformance ledger names %d compared collection(s) but the proof declares %d — E25 T6 made these SUBJECTS rather than a bare number precisely so a sweep that lost one and gained another could not look identical (plan §T4)",
			len(compared), fp.ConformanceCollectionsCompared)}}
	}
	return nil
}
