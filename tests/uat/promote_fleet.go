package uat

// The E24 T8 promote gate (plan §T8). It lives beside promote.go rather than inside it for the same reason
// evidence_fleet.go lives beside evidence.go, and it follows the family discipline exactly: the gate does
// its OWN re-derivation (never inheriting Complete()'s verdict), and it COMPOSES the epic underneath it
// rather than restating that epic's rules.

import (
	"encoding/json"
	"fmt"
)

// FleetPromoteGate is the mechanical form of the E24 exit-gate sentence (plan §T8, §7): a release cannot be
// tagged while any of the fleet's guards is red. It has six clauses:
//
//  1. the bundle must carry EXACTLY ONE complete FleetProof;
//
//  2. PLACEMENT IS A REFUSAL, RE-DERIVED. The wrong-pool and cross-tenant offer counts are recomputed here
//     from the offer ledger the proof carries, and the ledger must show a machine that was PASSED OVER for
//     its pool and one passed over for its TENANT. The second is the one worth reading: before this epic
//     the runner plane had no tenant on it at ALL — enrolment carried none, AttemptDescriptor carried none,
//     the lease offer carried none and Dial checked none, so any enrolled runner could take ANY tenant's
//     attempt. In a single-runner topology that is a definition; the moment two customers have Macs it is a
//     hole, and this counter is that hole closed;
//
//  3. CAPACITY ABSENCE PARKS A RUN, RE-DERIVED from the run ledger. AWS documents a Mac host as taking "6
//     to 20 minutes" to start and Dial gave it twenty seconds over five attempts, so "bring a Mac up when
//     load arrives" was not an expensive choice but an unreachable one — the run died four times before the
//     machine booted;
//
//  4. THE KEY-REVOCATION FENCE, and it is this epic's cheapest security test. Not "zero machines dropped"
//     but: the renewals AFTER the revocation instant are COUNTED, every one must have SUCCEEDED, and the
//     revocation must still have refused an enrolment. When the next reader says "if we revoked it we
//     should cut the connection too", this clause is the answer — cutting would delete the exact property
//     that makes easy enrolment safe, because a revoke that is also an outage is a revoke nobody performs;
//
//  5. A MACHINE REVOCATION OUTLIVES THE PROCESS, RE-DERIVED across a gateway generation, with the
//     discriminating half: the same restarted process must have SERVED an unrevoked machine, or a gateway
//     that refused everybody would look identical to one that refused the right machine;
//
//  6. NO TIER MAY ADVANCE — enforced by the composed E23 gate (which composes E22's, which composes E21's,
//     E20's, E19's mount derivation, E17's tier table and the eval numbers underneath it). Composing rather
//     than re-deriving keeps ONE owner of each rule.
//
// THERE IS NO CREDENTIAL-BYTES CLAUSE, AND THAT IS THE HONEST ANSWER RATHER THAN A GAP IN THE GATE. Plan
// §T8's counter (f) counted credential bytes in the execution relay's frames. THE OWNER DEFERRED T7, so
// there is no relay; a clause here would be judging a leg that never ran. §3.5 P4 carries the reason where a
// reader meets it.
//
// THE TIER DECISION IS ARGUED, NOT ASSUMED, and this epic's counter-argument is the strongest one these
// gates have faced on capability grounds:
//
//	"There is a real fleet now: machines are registered with server-minted identities, pools are
//	 tenant-scoped, keys are revocable, work lands on the right machine, a run waits instead of dying and a
//	 revocation survives a restart. `workspaces` should be `stable`."
//
// It is REFUSED for three reasons that belong in the code and not only in a commit message:
//
//	(1) §6 LEG 1 IS STILL OPEN AND E24 GREW IT AGAIN. The leg is "a captured, re-derivable receipt from two
//	    PHYSICAL machines", and its scope now includes a real remote enrolment, a real pool-key revocation
//	    and a real machine revocation across a restart. FleetPeer is structurally the literal "fake" and no
//	    code in this repository can produce otherwise. WHEN A LEG GROWS YOU ARE FURTHER FROM THE FLIP, NOT
//	    NEARER — and reason 1 needs updating in one more way this time: WITH T7 DEFERRED THE FLEET HAS NO
//	    REMOTE EXECUTION AT ALL. Every tool runs in the control plane's process, so what this epic proves is
//	    that an ENGINE can be placed, not that work can be done elsewhere.
//	(2) ADDING A PLANE IS NOT EVIDENCE THAT THE PLANE WORKS IN A REAL FLEET. E22 did not advance a tier for
//	    DELETING a boundary, E23 did not advance one for ADDING a boundary, and E24 does not advance one for
//	    adding a PLANE — the symmetry is the argument: all three rest on the same fake peer, and what these
//	    gates measure is evidence, not architecture.
//	(3) T2's POSTURE CEILING IS OPEN BY CONSTRUCTION. A machine STATES its posture at enrolment and the
//	    registry COMPARES it with the pool's; nothing VERIFIES the statement, and nothing can on this wire —
//	    verification needs a TPM or a Secure Enclave measurement, which is a separate design. So a machine
//	    that LIES about what it is enters the pool it claims, and a control with no attestation under it is
//	    not a control you promote a tier on.
//
// WHAT WOULD HAVE HAD TO BE TRUE to move `workspaces` to `stable`: (i) a CAPTURED, re-derivable receipt from
// TWO PHYSICAL MACHINES; (ii) the removal of Peer's structural "fake"; (iii) `linux/amd64` verified — which
// the state document names as the one gap this machine cannot close, and which E24 makes MORE pressing
// rather than less, because a fleet is heterogeneous by definition and `runner_pools.arch` can already name
// an architecture nothing has ever run on. None of the three exists.
func FleetPromoteGate(raw []byte, target string) []Refusal {
	var m evidenceManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return []Refusal{{Detail: "manifest is not valid JSON: " + err.Error()}}
	}

	var refusals []Refusal

	var fp *FleetProof
	claims := 0
	for _, c := range m.Cases {
		if c.FleetClaim == "" {
			continue
		}
		claims++
		if fp == nil {
			fp = c.FleetProof
		}
	}
	switch {
	case claims > 1:
		refusals = append(refusals, Refusal{Detail: fmt.Sprintf("%d fleet_claims in one manifest (want exactly 1): this gate judges the FIRST fleet proof, so a second could ride behind an honest one — one release, one re-derivation (plan §T8)", claims)})
	case fp == nil:
		refusals = append(refusals, Refusal{Detail: fleetIncomplete})
	default:
		// THE GATE'S OWN SWEEPS RUN FIRST, before Complete() is consulted, and the ORDER is the whole point.
		// Complete() re-derives the same counts, so putting it ahead would make every branch below
		// unreachable for exactly the inputs it exists to catch — a guard that cannot fire, which is this
		// repository's most-found defect. Running them first means a tag decision rests on THIS gate's
		// derivation and the reader is told WHICH claim failed.
		refusals = append(refusals, fleetPlacementRefusals(fp)...)
		refusals = append(refusals, fleetCapacityRefusals(fp)...)
		refusals = append(refusals, fleetKeyRevocationRefusals(fp)...)
		refusals = append(refusals, fleetMachineRevocationRefusals(fp)...)
		refusals = append(refusals, fleetRegistryRefusals(fp)...)
		if !fp.Complete() {
			refusals = append(refusals, Refusal{Detail: fleetIncomplete})
		}
	}

	// Clause 6 — composed verbatim. E24 opens no capability and moves no tier, so it owns NO tier rule of
	// its own; it inherits E23's, which inherits E22's, which inherits E21's, E20's and E19's cross-bundle
	// comparison against the committed extensions-0.1.0 baseline.
	return append(refusals, ToolApprovalPromoteGate(raw, target)...)
}

const fleetIncomplete = "no COMPLETE FleetProof (ZERO attempts offered a machine in the wrong pool and ZERO offered another tenant's machine, both re-derived from an offer ledger that shows a machine passed over for each and a machine actually used + ZERO runs dead-lettered because their pool was empty, re-derived beside a run that parked and a run a joining machine woke + ZERO enrolled machines dropped by a key revocation, re-derived with every renewal AFTER that revocation counted and successful and an enrolment it still refused + ZERO revoked machines that came back after a control-plane restart, re-derived beside an unrevoked machine the same new process served + at least two DISTINCT machines whose ids the SERVER minted, including two that asked for the same label + the canonical vendor and measurement ledger) — a release whose placement, capacity, credential-revocation or machine-revocation guard is red cannot be tagged (plan §T8, §7 exit gate)"

// fleetPlacementRefusals re-derives (a) and (b) from the carried offer ledger.
func fleetPlacementRefusals(fp *FleetProof) []Refusal {
	wrongPool, crossTenant, matched, poolRefusals, tenantRefusals, err := SweepFleetOffers(fp.OfferLedger)
	if err != nil {
		return []Refusal{{Detail: "the fleet proof's offer ledger cannot be swept, so \"no attempt was offered the wrong machine\" is unverifiable and the tag is REFUSED (fail closed): " + err.Error()}}
	}
	if len(wrongPool) != 0 {
		return []Refusal{{Detail: fmt.Sprintf(
			"%d attempt(s) were OFFERED a machine in another pool: %v — Dial asks for a pool and offers only that pool's members; a wrong-pool machine is not queued, is not \"nearest\" and is not a fallback, and a bundle declaring %d is fabricated (plan §2, §T8)",
			len(wrongPool), wrongPool, fp.OffersToTheWrongPool)}}
	}
	if len(crossTenant) != 0 {
		return []Refusal{{Detail: fmt.Sprintf(
			"%d attempt(s) crossed a TENANT boundary on the runner plane: %v — before this epic that was not a finding but the definition (enrolment carried no org or project, AttemptDescriptor carried none, the lease offer carried none and Dial checked none, so any enrolled machine could take any tenant's attempt); the rendezvous is keyed by (tenant, pool) now and the proof declares %d (plan §2, §3.6 D8, §T8)",
			len(crossTenant), crossTenant, fp.OffersAcrossTenants)}}
	}
	switch {
	case matched < 1:
		return []Refusal{{Detail: "the offer ledger contains no attempt that was offered a machine AND granted a lease — a rendezvous that refused everything satisfies both zeros above trivially, and a Dial that never succeeds is indistinguishable from placement that works (plan §T8)"}}
	case poolRefusals < 1:
		return []Refusal{{Detail: "the offer ledger contains no machine that was PASSED OVER for its pool — with no wrong-pool candidate present, \"zero wrong-pool offers\" is a statement about a corpus that never had a wrong machine in it, which is the vacuous form of this claim and also the exact shape the tree had before this epic (one unbuffered channel, every parked machine satisfying every attempt) (plan §T8)"}}
	case tenantRefusals < 1:
		return []Refusal{{Detail: "the offer ledger contains no machine that was PASSED OVER for its tenant — the cross-tenant zero is then free, and the hole this counter closes (any enrolled machine taking any tenant's attempt) would still be open behind it (plan §3.6 D8, §T8)"}}
	}
	return nil
}

// fleetCapacityRefusals re-derives (c) from the carried run ledger.
func fleetCapacityRefusals(fp *FleetProof) []Refusal {
	died, parked, woken, err := SweepFleetCapacityDeaths(fp.RunLedger)
	if err != nil {
		return []Refusal{{Detail: "the fleet proof's run ledger cannot be swept, so \"a run parks rather than dying\" is unverifiable and the tag is REFUSED (fail closed): " + err.Error()}}
	}
	if len(died) != 0 {
		return []Refusal{{Detail: fmt.Sprintf(
			"%d run(s) DIED because the target pool held no machine: %v — a 20-second dial deadline over five attempts kills a run in about two and a half minutes while AWS documents a Mac host as taking \"approximately 6 minutes to 20 minutes\" to start, so the run died four times before the machine booted; the repair is E23 T1's park-and-wake choreography used a second time, and the proof declares %d (plan §3.6 D12, §3.5 P10, §T8)",
			len(died), died, fp.RunsDeadLetteredForAbsentCapacity)}}
	}
	switch {
	case parked < 1:
		return []Refusal{{Detail: "the run ledger contains no run that actually PARKED on an empty pool — the zero above is then free, since a corpus where nothing ever waited cannot show that waiting is what happens (plan §T8)"}}
	case woken < 1:
		return []Refusal{{Detail: "the run ledger contains no parked run that a machine JOINING its pool woke and which then ran — without one, parking is only a nicer way to hang, and the wake is the half with no prior art on this plane (plan §T8)"}}
	}
	return nil
}

// fleetKeyRevocationRefusals re-derives (d), AND IT IS THE ANSWER TO THE NEXT READER'S FIRST INSTINCT.
//
// The instinct is "if we revoked the key we should cut the connection too". This is the answer: cutting
// would delete the exact property that makes easy enrolment safe. A pool key is designed to be pasted onto
// ten machines, and that is only tolerable because revoking it costs an operator NOTHING — it stops new
// machines and stops no running work. Make a revoke also a kill switch and every revoke is an outage, so
// nobody revokes, so a leaked key stays live. That is why this gate counts the renewals AFTER the revocation
// and requires every one of them to have SUCCEEDED, rather than merely checking a zero.
func fleetKeyRevocationRefusals(fp *FleetProof) []Refusal {
	dropped, admitted, renewals, refused, leasesIntact, err := SweepFleetKeyRevocation(fp.CredentialLedger)
	if err != nil {
		return []Refusal{{Detail: "the fleet proof's credential ledger cannot be swept, so \"revoking a key stops nobody who is already in\" is unverifiable and the tag is REFUSED (fail closed): " + err.Error()}}
	}
	if len(dropped) != 0 {
		return []Refusal{{Detail: fmt.Sprintf(
			"%d enrolled machine(s) were DROPPED by a key revocation: %v — renewal authenticates with the certificate the machine already holds and the credential chain is not on that path at all, so a key revocation cannot become a back door into SAN-011's machine-level hard stop. IF YOU ARRIVED HERE INTENDING TO CUT THE CONNECTION ON A REVOKE, THIS IS THE LINE THAT ARGUES BACK: a revoke that is also an outage is a revoke nobody performs, and then the leaked key stays live. The proof declares %d (plan §2, §3.6 D5, §T8)",
			len(dropped), dropped, fp.EnrolledRunnersDroppedByAKeyRevocation)}}
	}
	if len(admitted) != 0 {
		return []Refusal{{Detail: fmt.Sprintf(
			"%d enrolment(s) were ADMITTED on a revoked key: %v — revocation has three halves and this is the one that is a feature rather than a fence; the other two (a machine still renewing, its in-flight lease still relaying) are regression fences (plan §T3, §T8)",
			len(admitted), admitted)}}
	}
	switch {
	case renewals < 1:
		return []Refusal{{Detail: "the credential ledger contains no renewal AFTER the revocation instant — \"all of them succeeded\" is then a statement about no rows at all, and the fence that makes easy enrolment safe was never exercised. This is the counter, not the zero: the epic's cheapest security test is that renewals after a revoke are COUNTED and every one worked (plan §T8)"}}
	case refused < 1:
		return []Refusal{{Detail: "the credential ledger contains no enrolment the revoked key was REFUSED — without one the revocation did nothing at all, and \"nobody was dropped\" is satisfied by a revoke that never happened (plan §T3, §T8)"}}
	case leasesIntact < 1:
		return []Refusal{{Detail: "no post-revocation renewal is recorded against a machine whose in-flight lease was still relaying — the third half of the revocation claim, and the one an operator is most likely to assume was cut (plan §T3, §T8)"}}
	case fp.RenewalsAfterTheRevocation != renewals:
		return []Refusal{{Detail: fmt.Sprintf(
			"the credential ledger re-derives %d renewal(s) after the revocation but the proof declares %d — the count comes from the rows, never from the manifest (plan §T8)",
			renewals, fp.RenewalsAfterTheRevocation)}}
	}
	return nil
}

// fleetMachineRevocationRefusals re-derives (e) across a gateway generation.
func fleetMachineRevocationRefusals(fp *FleetProof) []Refusal {
	returned, refusedAfterRestart, servedAfterRestart, err := SweepFleetRevocationSurvival(fp.LifecycleLedger)
	if err != nil {
		return []Refusal{{Detail: "the fleet proof's lifecycle ledger cannot be swept, so \"a revocation outlives the process\" is unverifiable and the tag is REFUSED (fail closed): " + err.Error()}}
	}
	if len(returned) != 0 {
		return []Refusal{{Detail: fmt.Sprintf(
			"%d revoked machine(s) came BACK after a control-plane restart: %v — `revoked` was a process-local atomic.Bool, so a restart erased the decision, and with `restart: always` a VM reboot did that on a schedule; revocation is a row the gateway reads on every connect now, and the proof declares %d (plan §2, §3.6 D15, §T8)",
			len(returned), returned, fp.RevokedRunnersThatCameBack)}}
	}
	switch {
	case refusedAfterRestart < 1:
		return []Refusal{{Detail: "no revoked machine was actually TURNED AWAY by the restarted gateway — \"zero came back\" is then a statement about an experiment nobody ran (plan §T8)"}}
	case servedAfterRestart < 1:
		return []Refusal{{Detail: "the restarted gateway served NO unrevoked machine — a gateway that refuses everybody is indistinguishable from one that refuses the right machine, which is the difference between a revocation and an outage (plan §T5, §T8)"}}
	}
	return nil
}

// fleetRegistryRefusals re-derives (g) from the carried registry ledger. The strong form is the COLLISION
// count rather than the distinct count: two machines with two labels getting two rows was already true, and
// what was not is that two machines asking for the SAME name get two identities.
func fleetRegistryRefusals(fp *FleetProof) []Refusal {
	clientChosen, distinct, collidingLabels, err := SweepFleetRegistry(fp.RegistryLedger)
	if err != nil {
		return []Refusal{{Detail: "the fleet proof's registry ledger cannot be swept, so \"the server mints every id\" is unverifiable and the tag is REFUSED (fail closed): " + err.Error()}}
	}
	if len(clientChosen) != 0 {
		return []Refusal{{Detail: fmt.Sprintf(
			"%d registry row(s) do not carry a SERVER-minted identity: %v — the gateway used to sign whatever name the enrolling party asked for, and in a fleet \"revoke machine X\" is only meaningful if X is the server's word; the certificate's DNS must be derived from the ROW's id, which is also the T1 defect where two id minters that never met left last_seen_at unable to advance for anybody (plan §2, §3.6 D7, §T8)",
			len(clientChosen), clientChosen)}}
	}
	switch {
	case distinct < 2:
		return []Refusal{{Detail: fmt.Sprintf("the registry carries %d distinct machine(s) — an inventory of one is the topology this epic started from, and a fleet claim over it certifies nothing (plan §T8)", distinct)}}
	case collidingLabels < 1:
		return []Refusal{{Detail: "no requested LABEL in the registry was shared by two distinct identities — that is the strong form of this claim and the only one that discriminates: `deploy/compose/runner-entrypoint.sh` hardcodes `PALAI_RUNNER_ID=runner-local`, so `--scale runner=3` was three machines holding one certificate identity while `identity atomic.Pointer` kept a single slot the last writer won. Two machines with two different labels getting two rows was already true before this epic (plan §3.6 D7, D13, §T8)"}}
	case fp.DistinctRunners != distinct:
		return []Refusal{{Detail: fmt.Sprintf(
			"the registry ledger re-derives %d distinct machine(s) but the proof declares %d — the count comes from the rows, never from the manifest (plan §T8)",
			distinct, fp.DistinctRunners)}}
	}
	return nil
}
