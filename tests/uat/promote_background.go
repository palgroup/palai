package uat

// The E26 T7 promote gate (plan §T7). It lives beside promote.go rather than inside it for the same reason
// evidence_background.go lives beside evidence.go, and it follows the family discipline exactly: the gate does
// its OWN re-derivation (never inheriting Complete()'s verdict), and it COMPOSES the epic underneath it rather
// than restating that epic's rules.

import (
	"encoding/json"
	"fmt"
	"slices"
)

// BackgroundPromoteGate is the mechanical form of the E26 exit-gate sentence (plan §T7, §7): a release cannot
// be tagged while any of background execution's guards is red. It has eight clauses:
//
//  1. the bundle must carry EXACTLY ONE complete BackgroundProof;
//
//  2. ALL SIX OF §2's SEMANTICS ARE REPLICATED, AND THE SIXTH IS PRESENT BY NAME. §2.6 — the model is not
//     blocked — is singled out because it is the claim a weaker assertion silently drops: "it ran in the
//     background" is satisfied by a build that parks the run and waits for the process, which is exactly what
//     this epic is not. The strong form is that after the spawn returned, with the process verified ALIVE
//     from the kernel, the model dispatched another tool through the production dispatcher, that call
//     completed, its write landed on disk, and the process was still alive afterwards. And no semantics row
//     may be measured from our own bookkeeping — a liveness claim read off a row we wrote is a claim about
//     the row;
//
//  3. EVERY REFUSAL STARTS ZERO PROCESSES AND EVERY REFUSAL CARRIES ITS POSITIVE HALF. The second half is
//     this clause's whole content and this epic paid for it: T2's approval-gate RED PASSED AT THE FORK POINT,
//     because there was no spawn path at all and "the gate's spawn counter reads zero" was free. A security
//     proof that cannot fail reports the same green as one that passes, so each refusal must name what the
//     SAME harness did with the refusal's condition lifted;
//
//  4. THE PROCESS OUTLIVES THE CALL IN BOTH POSTURES, AND AN UNPROVABLE HANDLE IS NEVER SIGNALLED. Both
//     postures, because a container's lifetime is the daemon's business and a process group's is the
//     kernel's — the plan measured that TODAY neither survives, for two entirely different reasons, so one
//     posture's evidence is not the other's. The zero is the sharper half: PID numbers are reused, so a row
//     that survived a restart names a pgid that may now belong to anything on the machine, and NOT killing a
//     stranger's process matters more than reaping our own;
//
//  5. AN EXIT NOTIFIES EXACTLY ONCE AND NEVER INTERRUPTS A RUN THAT DID NOT PARK. Every scenario — two
//     ticks, two control planes, a crash-restart, a still-running run and an already-terminal run — produces
//     exactly one notice for a settled task, and each names the mutation that reddens it, because
//     exactly-once is the property most easily satisfied by an accident;
//
//  6. THE REAPER'S SIX DUTIES ARE ALL PRESENT AND NONE IS READ OFF OUR OWN RECORD. A reaper is the component
//     most able to satisfy its own tests. E24 measured `runners.capacity` written, read back and used in no
//     decision, and repeating that with a background ceiling would have been the same defect with a new name;
//
//  7. A CREDENTIAL VALUE LANDS IN NONE OF THE FIVE SITES, MEASURED AFTER DECODING. A raw-byte sweep over
//     encoded output can never fail — this tree measured that in E14 T7 and paid again in E20 T4 — so every
//     site declares it was decoded and names a harmless token the same scan DID find;
//
//  8. NO TIER MAY ADVANCE — enforced by the composed E25 gate (which composes E23's, which composes E22's,
//     E21's, E20's, E19's mount derivation, E17's tier table and the eval numbers underneath it). Composing
//     rather than re-deriving keeps ONE owner of each rule.
//
// WHY IT COMPOSES E25. E26 forked from `main` after `admin-console-0.1.0` merged and derives its inherited
// case set from that release, so an E26 bundle also carries the E25 admin-console claim, the E23
// tool-approval claim, the E22 code-and-ship claim, the E21 tools-memory claim, the E20 agent-surface claim
// and the E17 area claims. Without this clause it would reroute to AdminConsolePromoteGate, which knows
// nothing about the semantics coverage, the refusal controls, the ownership postures, the exactly-once
// notice, the reaper duties or the redaction sites, and would pass it: every background guard would be
// optional in practice.
//
// THE TIER DECISION IS ARGUED, NOT ASSUMED, and this epic's counter-argument is a strong one:
//
//	"A tool call can now return a process. A model starts a five-minute build, keeps working, and is called
//	 back when it finishes. That is a real capability on a real machine — `apple-build` should move."
//
// It is REFUSED for one reason, and the reason is a RULE rather than a judgement:
//
//	MAKING A TOOL ASYNCHRONOUS IS NOT EVIDENCE ABOUT THE PLANE THAT TOOL RUNS ON. Every tier in this tree
//	is a statement about a PLANE — that a provider was reached, that a signing identity exists, that an
//	index was served, that a console was deployed. E26 changed the SHAPE of a call on a plane that is
//	unchanged: the same host executor, the same OCI driver, the same allocation, the same machine. Nothing
//	this epic did makes a claim about a plane truer than it was, and `apple-build` in particular is
//	`disabled` because there is no signing identity anywhere in this project — a fact backgrounding an
//	`xcodebuild` does not touch.
//
// WHAT WOULD HAVE HAD TO BE TRUE to move a tier here: a background task running somewhere the control plane
// is not (the execution relay, E24 T7, never shipped), or a real signed build on a real Mac with a captured
// transcript. Neither exists, and the §6 legs that would produce them are open.
func BackgroundPromoteGate(raw []byte, target string) []Refusal {
	var m evidenceManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return []Refusal{{Detail: "manifest is not valid JSON: " + err.Error()}}
	}

	var refusals []Refusal

	var bp *BackgroundProof
	claims := 0
	for _, c := range m.Cases {
		if c.BackgroundClaim == "" {
			continue
		}
		claims++
		if bp == nil {
			bp = c.BackgroundProof
		}
	}
	switch {
	case claims > 1:
		refusals = append(refusals, Refusal{Detail: fmt.Sprintf("%d background_claims in one manifest (want exactly 1): this gate judges the FIRST background proof, so a second could ride behind an honest one — one release, one re-derivation (plan §T7)", claims)})
	case bp == nil:
		refusals = append(refusals, Refusal{Detail: backgroundIncomplete})
	default:
		// THE GATE'S OWN SWEEPS RUN FIRST, before Complete() is consulted, and the ORDER is the whole point.
		// Complete() re-derives the same counts, so putting it ahead would make every branch below
		// unreachable for exactly the inputs it exists to catch — a guard that cannot fire, which is this
		// repository's most-found defect. Running them first means a tag decision rests on THIS gate's
		// derivation and the reader is told WHICH claim failed.
		refusals = append(refusals, backgroundSemanticsRefusals(bp)...)
		refusals = append(refusals, backgroundRefusalRefusals(bp)...)
		refusals = append(refusals, backgroundOwnershipRefusals(bp)...)
		refusals = append(refusals, backgroundNoticeRefusals(bp)...)
		refusals = append(refusals, backgroundReaperRefusals(bp)...)
		refusals = append(refusals, backgroundRedactionRefusals(bp)...)
		if !bp.Complete() {
			refusals = append(refusals, Refusal{Detail: backgroundIncomplete})
		}
	}

	// Clause 8 — composed verbatim. E26 opens no capability and moves no tier, so it owns NO tier rule of its
	// own; it inherits E25's, which inherits E23's, which inherits E22's, E21's, E20's and E19's cross-bundle
	// comparison against the committed extensions-0.1.0 baseline.
	return append(refusals, AdminConsolePromoteGate(raw, target)...)
}

const backgroundIncomplete = "no COMPLETE BackgroundProof (all six of §2's semantics replicated, each naming a shipped test and a measurement source that is the kernel, the daemon, the database or a frame — never our own bookkeeping — and §2.6 present, which is the claim a weaker assertion silently drops + ZERO processes started under every refusal, each refusal carrying the count the SAME harness started with the refusal lifted + BOTH sandbox postures outliving the call that started them and ZERO signals to a handle we could not prove was ours + exactly ONE notice per settled task in every wake scenario, none interrupting a run that never parked, each naming the mutation that reddens it + all six reaper duties, none read off our own record + ZERO environment values in any of the five landing sites, each DECODED before it was scanned and each naming a harmless token it DID find + the canonical vendor and on-machine measurement ledger) — a release whose non-blocking, refusal, ownership, notification, reaper or credential guard is red cannot be tagged (plan §T7, §7 exit gate)"

// backgroundSemanticsRefusals re-derives (a) from the carried semantics ledger.
func backgroundSemanticsRefusals(bp *BackgroundProof) []Refusal {
	covered, fromTheMachine, tests, err := SweepBackgroundSemantics(bp.SemanticsLedger)
	if err != nil {
		return []Refusal{{Detail: "the background proof's semantics ledger cannot be swept, so \"the harness's background semantics are replicated\" is unverifiable and the tag is REFUSED (fail closed): " + err.Error()}}
	}
	switch {
	case !slices.Contains(covered, backgroundDefiningSemantic):
		return []Refusal{{Detail: fmt.Sprintf(
			"the semantics ledger covers %v and does NOT carry §2.%d — the model not being blocked is the DEFINING property of this epic and the one a weaker assertion silently drops. \"It ran in the background\" is satisfied by a build that parks the run and waits for the process; what has to be proven is that the model called ANOTHER tool after the spawn and that call COMPLETED while the process was still running (plan §2, §T7)",
			covered, backgroundDefiningSemantic)}}
	case len(covered) != BackgroundSemanticsReplicated:
		return []Refusal{{Detail: fmt.Sprintf(
			"the semantics ledger covers %d of §2's %d semantics (%v) — the six are not a checklist, they are what \"the same as the harness, exactly the same\" was ordered to mean, and a release covering five of them replicates something else (plan §2, §T7)",
			len(covered), BackgroundSemanticsReplicated, covered)}}
	case fromTheMachine < 1:
		return []Refusal{{Detail: "no semantics row was measured from the KERNEL or the DAEMON — every liveness claim in this bundle would then rest on a row we wrote, and a process's existence is not something our own bookkeeping can answer (plan §2, §T7)"}}
	case bp.SemanticsReplicated != len(covered) || bp.SemanticsMeasuredFromTheMachine != fromTheMachine:
		return []Refusal{{Detail: fmt.Sprintf(
			"the semantics ledger re-derives %d replicated / %d measured from the machine but the proof declares %d / %d — the counts come from the rows, never from the manifest (plan §T7)",
			len(covered), fromTheMachine, bp.SemanticsReplicated, bp.SemanticsMeasuredFromTheMachine)}}
	case len(tests) != len(covered):
		return []Refusal{{Detail: fmt.Sprintf("the semantics ledger names %d test(s) for %d semantics — a semantic with no test is a sentence (plan §T7)", len(tests), len(covered))}}
	}
	return nil
}

// backgroundRefusalRefusals re-derives (b). This is the clause the epic's own fork point taught.
func backgroundRefusalRefusals(bp *BackgroundProof) []Refusal {
	spawns, controlled, uncontrolled, err := SweepBackgroundRefusals(bp.RefusalLedger)
	if err != nil {
		return []Refusal{{Detail: "the background proof's refusal ledger cannot be swept, so \"a refused background call starts nothing\" is unverifiable and the tag is REFUSED (fail closed): " + err.Error()}}
	}
	switch {
	case spawns != 0:
		return []Refusal{{Detail: fmt.Sprintf(
			"%d process(es) were started under a refusal — a gated tool that spawns before a human decides has performed the side effect the gate exists to hold back, and a disabled feature that spawns is a model believing it backgrounded something that is in fact blocking. The proof declares %d (plan §2, §T7)",
			spawns, bp.ProcessesStartedUnderARefusal)}}
	case len(uncontrolled) != 0:
		return []Refusal{{Detail: fmt.Sprintf(
			"%d refusal(s) carry NO non-vacuity control: %v — this epic watched exactly that zero be free. T2's approval-gate RED PASSED at the fork point, because there was no spawn path at all, and a security proof that cannot fail reports the same green as one that passes. Each refusal owes the count the SAME harness produced with its condition lifted (plan §T2, §T7)",
			len(uncontrolled), uncontrolled)}}
	case controlled < 2:
		return []Refusal{{Detail: "fewer than two refusals carry a control — \"every refusal starts nothing\" over one refusal is a statement about that refusal, and this epic ships five distinct ones (the approval gate, the kill switch, the per-run ceiling, the credential deadline and an escaping output path) (plan §T7)"}}
	case bp.ProcessesStartedUnderARefusal != spawns || bp.RefusalsWithANonVacuityControl != controlled:
		return []Refusal{{Detail: fmt.Sprintf(
			"the refusal ledger re-derives %d spawn(s) / %d controlled refusal(s) but the proof declares %d / %d — the counts come from the rows, never from the manifest (plan §T7)",
			spawns, controlled, bp.ProcessesStartedUnderARefusal, bp.RefusalsWithANonVacuityControl)}}
	}
	return nil
}

// backgroundOwnershipRefusals re-derives (c): the whole reason this epic was a rewrite rather than a feature.
func backgroundOwnershipRefusals(bp *BackgroundProof) []Refusal {
	survived, signals, postures, err := SweepBackgroundOwnership(bp.OwnershipLedger)
	if err != nil {
		return []Refusal{{Detail: "the background proof's ownership ledger cannot be swept, so \"a process outlives the call that started it\" is unverifiable and the tag is REFUSED (fail closed): " + err.Error()}}
	}
	switch {
	case signals != 0:
		return []Refusal{{Detail: fmt.Sprintf(
			"%d signal(s) were sent to a handle we could not prove was ours — PID numbers are reused, so a row that survived a restart names a pgid that may now belong to ANY process on the machine, and the rule this whole design rests on is that not killing a stranger's process matters more than reaping our own. The proof declares %d (plan §2, §T1, §T7)",
			signals, bp.SignalsToUnprovableHandles)}}
	case len(postures) != len(BackgroundPostures):
		return []Refusal{{Detail: fmt.Sprintf(
			"the ownership ledger covers %d of %d postures (%v) — a container's lifetime is the DAEMON's business and a process group's is the KERNEL's, and the plan measured that before this epic NEITHER survived its call, for two entirely different reasons. One posture's evidence is not the other's (plan §3.1, §T7)",
			len(postures), len(BackgroundPostures), postures)}}
	case survived != len(postures):
		return []Refusal{{Detail: fmt.Sprintf(
			"%d of %d postures had their operating-system object still alive after the call returned — that is the ONE thing this epic exists to make true, and a posture where it is false is a posture where a background task is still the contradiction it was at the fork point (plan §3.6 D2, §T7)",
			survived, len(postures))}}
	case bp.PosturesThatOutliveTheirCall != survived:
		return []Refusal{{Detail: fmt.Sprintf(
			"the ownership ledger re-derives %d surviving posture(s) but the proof declares %d — the count comes from the rows, never from the manifest (plan §T7)",
			survived, bp.PosturesThatOutliveTheirCall)}}
	}
	return nil
}

// backgroundNoticeRefusals re-derives (d).
func backgroundNoticeRefusals(bp *BackgroundProof) []Refusal {
	exactlyOnce, interrupted, unmutated, err := SweepBackgroundNotices(bp.NoticeLedger)
	if err != nil {
		return []Refusal{{Detail: "the background proof's notice ledger cannot be swept, so \"an exit calls the model back exactly once\" is unverifiable and the tag is REFUSED (fail closed): " + err.Error()}}
	}
	switch {
	case len(interrupted) != 0:
		return []Refusal{{Detail: fmt.Sprintf(
			"%d scenario(s) interrupted a run that never parked: %v — a wake fired at a run that is still working is a SECOND attempt of one run, which is the failure the enqueue-either-way/wake-only-if-waiting pair exists to prevent (plan §T4, §T7)",
			len(interrupted), interrupted)}}
	case len(unmutated) != 0:
		return []Refusal{{Detail: fmt.Sprintf(
			"%d notice scenario(s) name no mutation that reddens them: %v — exactly-once is the property most easily satisfied by an accident, and this epic measured its own (removing `notified_at IS NULL` from the single-winner UPDATE produces FOUR notifications). A row nobody has broken is a row nobody has tested (plan §T4, §T7)",
			len(unmutated), unmutated)}}
	case exactlyOnce < 3:
		return []Refusal{{Detail: fmt.Sprintf(
			"only %d wake scenario(s) in the ledger — exactly-once over one scenario says nothing about two ticks, two control planes, a crash-restart, a run that never parked or a run that is already terminal, and the last of those is the one both easy answers get wrong (plan §T4, §T7)",
			exactlyOnce)}}
	case bp.ScenariosNotifyingExactlyOnce != exactlyOnce:
		return []Refusal{{Detail: fmt.Sprintf(
			"the notice ledger re-derives %d exactly-once scenario(s) but the proof declares %d — the count comes from the rows, never from the manifest (plan §T7)",
			exactlyOnce, bp.ScenariosNotifyingExactlyOnce)}}
	}
	return nil
}

// backgroundReaperRefusals re-derives (e).
func backgroundReaperRefusals(bp *BackgroundProof) []Refusal {
	duties, fromTheMachine, missing, err := SweepBackgroundReaper(bp.ReaperLedger)
	if err != nil {
		return []Refusal{{Detail: "the background proof's reaper ledger cannot be swept, so \"a ceiling, a cancellation, an adoption and a collector\" is unverifiable and the tag is REFUSED (fail closed): " + err.Error()}}
	}
	switch {
	case len(missing) != 0:
		return []Refusal{{Detail: fmt.Sprintf(
			"%d reaper duty(ies) are absent: %v — each is a DIFFERENT way an orphan happens, and the plan named the orphan (\"a process compiling for an hour on an operator's Mac\") as this epic's reason for existing, so a missing duty is not a smaller proof (plan §0.2, §T5, §T7)",
			len(missing), missing)}}
	case fromTheMachine != len(duties):
		return []Refusal{{Detail: fmt.Sprintf(
			"%d of %d reaper duties were read from the operating system or the database — a reaper is the component most able to satisfy its own tests, and \"the task was killed\" read from the row the killer wrote is not a claim about a process. E24 measured `runners.capacity` written, read back and used in no decision; repeating that here would be the same defect with a new name (plan §3.6 D12, §T5, §T7)",
			fromTheMachine, len(duties))}}
	case bp.ReaperDutiesMeasuredFromTheMachine != fromTheMachine:
		return []Refusal{{Detail: fmt.Sprintf(
			"the reaper ledger re-derives %d duty(ies) measured from the machine but the proof declares %d — the count comes from the rows, never from the manifest (plan §T7)",
			fromTheMachine, bp.ReaperDutiesMeasuredFromTheMachine)}}
	}
	return nil
}

// backgroundRedactionRefusals re-derives (f) — the answer to §0.4, which the plan called its sharpest question.
func backgroundRedactionRefusals(bp *BackgroundProof) []Refusal {
	hits, probed, sites, err := SweepBackgroundRedaction(bp.RedactionLedger)
	if err != nil {
		return []Refusal{{Detail: "the background proof's redaction ledger cannot be swept, so \"a credential value lands nowhere\" is unverifiable and the tag is REFUSED (fail closed): " + err.Error()}}
	}
	switch {
	case hits != 0:
		return []Refusal{{Detail: fmt.Sprintf(
			"an environment VALUE was found %d time(s) in a landing site — a background task's log is written by the process itself and bypasses the redaction a synchronous result gets on the captured Go string, and the exit notice's tail lands in commands.payload and delivered_messages, which the synchronous path never does. Both were recorded OPEN by T2 and T4 where a reader meets them; a hit here means one of them is open again. The proof declares %d (plan §3.6 D8, §T6, §T7)",
			hits, bp.EnvironmentValuesInAnyLandingSite)}}
	case probed != len(sites) || len(sites) != len(backgroundRedactionSites):
		return []Refusal{{Detail: fmt.Sprintf(
			"the redaction sweep covers %d of %d landing sites — a site with no row is a site nobody scanned, and the two T2 and T4 each recorded as open are exactly the two a later reader would assume were covered (plan §T6, §T7)",
			len(sites), len(backgroundRedactionSites))}}
	case bp.EnvironmentValuesInAnyLandingSite != hits:
		return []Refusal{{Detail: fmt.Sprintf(
			"the redaction ledger re-derives %d hit(s) but the proof declares %d — the count comes from the rows, never from the manifest (plan §T7)",
			hits, bp.EnvironmentValuesInAnyLandingSite)}}
	}
	return nil
}
