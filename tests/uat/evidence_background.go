package uat

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// BackgroundCaseIDs are the UAT ids E26 opens, and the `BGT-` prefix is a GATE decision rather than a naming
// preference — the CON-/FLT-/HIL- reasoning, arrived at once more. Every prefix already inside
// `extensionIDPrefixes` (tests/uat/extensions/catalog_test.go) either belongs to a committed family gate or is
// the shipped extensions-0.1.0 bundle's own case list, whose generator reads that map and whose every checksum
// folds in a digest of uat.CapabilityClaims. So an id under an existing prefix would either force the
// regeneration of a historical release — rewriting the record of a run that happened — or fall through
// PromoteGateFor to a WEAKER family gate that knows none of this epic's guards and would pass it.
//
// `BGT-` COLLIDES WITH NOTHING, RE-DERIVED RATHER THAN INHERITED. The plan counted thirty-three prefixes in
// tests/uat/cases on 2026-07-30 and named them. Re-counted for this gate (2026-07-31,
// `ls tests/uat/cases | sed -E 's/^([A-Z0-9]+)-.*$/\1/' | sort -u | wc -l`): THIRTY-FOUR, and the
// thirty-fourth is `BGT-` itself — the four directories this epic's own T1/T2/T4/T5 had already created. The
// plan's thirty-three was right when it was written and is the correct disjointness claim; the count moved
// because this epic moved it. TestTheBGTPrefixCollidesWithNoOtherFamily re-derives both halves from the
// directory rather than from this paragraph.
//
// OWNERSHIP LIVES HERE; ESCAPING THE SWEEP IS NOT ALLOWED, AND THIS ONE WAS ESCAPING. tests/uat/extensions is
// the ONLY place in the tree that walks the cases DIRECTORY. `BGT-` was in no prefix list at all while
// BGT-001, BGT-002, BGT-004 and BGT-005 shipped, so all four escaped proof resolution and every UAT package
// reported ok — the seventh instance of the shape E23 T2's HIL-004 fell into and E25's CON-001 reproduced on
// demand. Adding the prefix was shown RED first (four directories reported at once) before this list existed.
//
// BGT-003 IS IN THIS LIST AND ITS DIRECTORY DID NOT EXIST UNTIL THIS GATE. T3 shipped the park, the response
// state machine's first production write and nine proofs, and opened no case for them — which the sweep above
// could not have caught, because a MISSING directory is invisible to a sweep that walks directories. It is
// caught from the other side: this list is the canonical set, and tests/uat/background refuses an id in it
// with no case.yaml. Two directions, two owners, and the gap between them was one whole task's evidence.
var BackgroundCaseIDs = []string{"BGT-001", "BGT-002", "BGT-003", "BGT-004", "BGT-005"}

// ---------------------------------------------------------------------------------------------------------
// The E26 T7 EXIT-gate proof type (plan §T7) — the `background-execution-0.1.0` sign-off.
//
// WHAT THIS BUNDLE CLAIMS, and every clause is narrower than the sentence a reader will want it to be:
//
//   - a tool call can return a PROCESS instead of a RESULT, and the process outlives the call that started
//     it — measured from the kernel and from the Docker daemon, never from a row we wrote;
//   - THE MODEL IS NOT BLOCKED, in the strong form and not the weak one: after the spawn returned, WHILE the
//     process was still alive, the model called ANOTHER tool through the production dispatcher and that call
//     COMPLETED — with the overlap asserted on both sides of the second call;
//   - every refusal starts ZERO processes, and each refusal carries its own non-vacuity control, because at
//     the fork point there was no spawn path at all and "nothing spawned" was free;
//   - an exit calls the model back EXACTLY ONCE across two ticks, two control planes and a restart, and a run
//     that never parked is never interrupted;
//   - a ceiling, a cancellation, an adoption and a collector are enforced by a REAPER reading a column, and
//     every one of their outcomes is measured from the operating system or from the database;
//   - a credential's lifetime is now the TASK, and both places its value could land are masked from ONE.
//
// AND THE HONEST CEILING, WHICH IS THIS BUNDLE'S MOST IMPORTANT SENTENCE, IN THREE PARTS.
//
// FIRST: THERE IS NO LIVE PROGRESS STREAM. This release does not close CAS-P2, it NARROWS it. A model still
// learns nothing about a running build unless it reads a file; nobody pushes it ten more lines. The remaining
// half — `task_update` chunks plus a progress channel on `toolbroker.ShellRunner` — is still open and is
// E27's.
//
// SECOND: A BACKGROUND TASK DOES NOT SURVIVE A CONTROL-PLANE MOVE. T5 DID ship restart-adopt, so §4.2's
// "T5b was skipped" sentence is NOT owed and is not written — measured, not assumed. What adoption cannot do
// is cross a machine: a host pgid means nothing on another box and an OCI container stays on the old daemon,
// so every row becomes `lost` and nothing is reaped. That is D4's direct consequence and it does not close
// without the execution relay the owner deferred.
//
// THIRD: BackgroundMachine is STRUCTURALLY the literal "local". Every measurement in this release was taken
// on ONE machine — the control plane's own, which is the only machine background execution has (E24 T7, the
// execution relay, was never shipped). There is no peer here and the field is not called `Peer` for that
// reason: nothing in this bundle was measured across a boundary, so a field that could say "real" would be
// a field that could lie about a topology this epic does not have.
//
// NO TIER ADVANCES, and the reason is a rule rather than an omission: MAKING A TOOL ASYNCHRONOUS IS NOT
// EVIDENCE ABOUT THE PLANE THAT TOOL RUNS ON. `slack` closes preview, `knowledge-vector` disabled,
// `apple-build` disabled, `console` preview.
// ---------------------------------------------------------------------------------------------------------

// BackgroundBundle is the E26 EXIT bundle's release name.
const BackgroundBundle = "background-execution-0.1.0"

// BackgroundMachine is the ONE honest naming a BackgroundProof may carry, and the field is `Machine` rather
// than `Peer` BECAUSE THERE IS NO PEER. The fleet, agent-surface and admin-console bundles each name a
// counterparty they did not reach (`fake`); this epic reaches no counterparty at all. Every process it starts
// runs on the machine the control plane is on, every probe is a syscall or a local Docker daemon call, and
// E24 T7's relay — the only thing that would put a background task somewhere else — was never shipped. A
// bundle that cannot type any other value into this field cannot accidentally claim a distributed property.
const BackgroundMachine = "local"

// --- (a) the six replicated semantics -----------------------------------------------------------------

// backgroundSemanticRow is one of §2's six semantics: the number, the sentence it replicates, the SHIPPED
// test that proves it, the package that test lives in, and where the measurement came FROM.
//
// `MeasuredFrom` is carried rather than assumed because it is the difference between this epic's proofs and
// the proofs it refuses. "The process is running" read off a row we wrote is a statement about our
// bookkeeping; `kill(-pgid, 0)` and `ContainerInspect` are statements about the machine.
type backgroundSemanticRow struct {
	Semantic     int    `json:"semantic"`
	Claim        string `json:"claim"`
	Test         string `json:"test"`
	Package      string `json:"package"`
	MeasuredFrom string `json:"measured_from"`
}

// backgroundMeasurementSources are the only honest answers to "where did this number come from". `bookkeeping`
// is deliberately ABSENT: a semantics row that could name it would be a row asserting our own record back to
// us, which is the proof shape every leg of this epic was built to avoid.
var backgroundMeasurementSources = []string{"kernel", "daemon", "database", "frame"}

// BackgroundSemanticsReplicated is §2's count. Six, and the plan's own framing is that the sixth is both the
// most important and the most likely to be built wrong.
const BackgroundSemanticsReplicated = 6

// backgroundDefiningSemantic is §2.6 — the model is not blocked. It is singled out in Complete() because it is
// the claim a weaker assertion silently drops: "it ran in the background" is satisfied by a build that parks
// the run and waits, which is precisely what this epic is NOT.
const backgroundDefiningSemantic = 6

// SweepBackgroundSemantics decodes the semantics ledger and returns the semantics covered, the ones measured
// from the machine rather than from our own record, and the tests named — group (a), RE-DERIVED from the
// carried rows rather than read off a declared count.
func SweepBackgroundSemantics(ledger json.RawMessage) (covered []int, fromTheMachine int, tests []string, err error) {
	if len(ledger) == 0 {
		return nil, 0, nil, fmt.Errorf("no semantics ledger to sweep: \"the harness's background semantics are replicated\" over nothing is vacuous")
	}
	var rows []backgroundSemanticRow
	if err := json.Unmarshal(ledger, &rows); err != nil {
		return nil, 0, nil, fmt.Errorf("the carried semantics ledger is not JSON, so the coverage count is unverifiable: %w", err)
	}
	seen := map[int]bool{}
	for _, row := range rows {
		if row.Semantic < 1 || row.Semantic > BackgroundSemanticsReplicated {
			return nil, 0, nil, fmt.Errorf("semantics row %q declares semantic %d, and §2 names %d", row.Test, row.Semantic, BackgroundSemanticsReplicated)
		}
		if seen[row.Semantic] {
			return nil, 0, nil, fmt.Errorf("semantic %d appears twice — a duplicated row covers one semantic while claiming two", row.Semantic)
		}
		seen[row.Semantic] = true
		if strings.TrimSpace(row.Claim) == "" || row.Test == "" || row.Package == "" {
			return nil, 0, nil, fmt.Errorf("semantics row %d names no claim, no test or no package: %+v", row.Semantic, row)
		}
		if !strings.HasPrefix(row.Test, "Test") {
			return nil, 0, nil, fmt.Errorf("semantics row %d names %q, which is not a Go test function — a proof that is not a test is a sentence", row.Semantic, row.Test)
		}
		if !slices.Contains(backgroundMeasurementSources, row.MeasuredFrom) {
			return nil, 0, nil, fmt.Errorf("semantics row %d declares measured_from %q, want one of %v — `bookkeeping` is not on that list on purpose: a liveness claim read off a row we wrote is a claim about the row", row.Semantic, row.MeasuredFrom, backgroundMeasurementSources)
		}
		if row.MeasuredFrom == "kernel" || row.MeasuredFrom == "daemon" {
			fromTheMachine++
		}
		covered = append(covered, row.Semantic)
		tests = append(tests, row.Test)
	}
	slices.Sort(covered)
	return covered, fromTheMachine, tests, nil
}

// --- (b) the refusals, and the non-vacuity control each one carries -----------------------------------

// backgroundRefusalRow is one refusal path with the number of processes it started (which must be zero) and
// the number the SAME harness started with the refusal's condition lifted.
//
// `ControlSpawns` IS THE WHOLE ROW AND IT WAS PAID FOR IN THIS EPIC. T2's approval-gate RED passed at the fork
// point: there was no spawn path, so "the gate's spawn counter reads zero" was free, and a security proof that
// cannot fail reports the same green as one that passes. Every refusal here therefore carries its own positive
// half, in the same measurement, on the same harness.
type backgroundRefusalRow struct {
	Refusal string `json:"refusal"`
	Spawns  int    `json:"spawns"`
	// Control is what the SAME harness did with the refusal's condition lifted, ControlCount is how many, and
	// ControlUnit is WHAT WAS COUNTED. The unit is carried rather than assumed because for one refusal it is
	// genuinely not a background spawn: the kill switch's dangerous failure is a SILENT DOWNGRADE, so its
	// control is a synchronous run and counting background spawns there would have been a control that cannot
	// distinguish the failure from the fix. A ledger that flattened both into one integer would have recorded
	// a number nobody could attribute.
	Control      string `json:"control"`
	ControlCount int    `json:"control_count"`
	ControlUnit  string `json:"control_unit"`
	Test         string `json:"test"`
}

// backgroundControlUnits are the two things a refusal's control may count. Both are real counters on the
// shipped harnesses (a background-runner wrapper and a shell-runner wrapper), and a third value would be a
// unit nothing measures.
var backgroundControlUnits = []string{"background spawns", "synchronous runs"}

// SweepBackgroundRefusals decodes the refusal ledger and returns the total processes started under a refusal
// (which must be zero), the refusals whose control DID start one, and any refusal with no control — group (b).
func SweepBackgroundRefusals(ledger json.RawMessage) (spawns, controlled int, uncontrolled []string, err error) {
	if len(ledger) == 0 {
		return 0, 0, nil, fmt.Errorf("no refusal ledger to sweep: \"a refused background call starts nothing\" over no refusals is vacuous")
	}
	var rows []backgroundRefusalRow
	if err := json.Unmarshal(ledger, &rows); err != nil {
		return 0, 0, nil, fmt.Errorf("the carried refusal ledger is not JSON, so the spawn count is unverifiable: %w", err)
	}
	if len(rows) == 0 {
		return 0, 0, nil, fmt.Errorf("the refusal ledger is EMPTY: a build that refuses nothing satisfies \"no refusal started a process\" trivially")
	}
	seen := map[string]bool{}
	for _, row := range rows {
		if strings.TrimSpace(row.Refusal) == "" || row.Test == "" {
			return 0, 0, nil, fmt.Errorf("a refusal row names no refusal or no test: %+v", row)
		}
		if seen[row.Refusal] {
			return 0, 0, nil, fmt.Errorf("refusal %q appears twice — a duplicated row inflates the controlled count without testing a second path", row.Refusal)
		}
		seen[row.Refusal] = true
		if row.Spawns < 0 || row.ControlCount < 0 {
			return 0, 0, nil, fmt.Errorf("refusal %q carries a negative count: %+v", row.Refusal, row)
		}
		if !slices.Contains(backgroundControlUnits, row.ControlUnit) {
			return 0, 0, nil, fmt.Errorf("refusal %q declares control_unit %q, want one of %v — a control whose unit is not stated is a number nobody can attribute, and for the kill switch the honest unit is a SYNCHRONOUS run rather than a background spawn", row.Refusal, row.ControlUnit, backgroundControlUnits)
		}
		spawns += row.Spawns
		if row.ControlCount > 0 && strings.TrimSpace(row.Control) != "" {
			controlled++
			continue
		}
		uncontrolled = append(uncontrolled, row.Refusal)
	}
	return spawns, controlled, uncontrolled, nil
}

// --- (c) process ownership, per posture ----------------------------------------------------------------

// backgroundOwnershipRow is one sandbox posture: the operating-system object a probe looks at, whether the
// object was ALIVE after the call that started it returned, and how many signals were sent to a handle we
// could not prove was ours.
//
// THE LAST FIELD IS AN ABSENCE AND IT IS THE SHARPEST CLAIM IN THIS BUNDLE. PID numbers are reused; a row that
// survived a restart names a pgid that may now belong to anything on the machine. `SignalsToUnprovableHandles`
// must be zero in every posture, and the measurement behind it is a REAL process started outside the program,
// a fabricated start time, and that process still running afterwards.
type backgroundOwnershipRow struct {
	Posture                    string `json:"posture"`
	Object                     string `json:"object"`
	AliveAfterTheCallReturned  bool   `json:"alive_after_the_call_returned"`
	SignalsToUnprovableHandles int    `json:"signals_to_unprovable_handles"`
	Test                       string `json:"test"`
}

// BackgroundPostures are the two sandbox postures a background task can run in, and both must be present. They
// are not interchangeable: a container's life is the daemon's business and a process group's is the kernel's,
// which is exactly why the plan measured that TODAY neither survives and for two entirely different reasons.
var BackgroundPostures = []string{"unsandboxed-host", "sandboxed-linux"}

// SweepBackgroundOwnership decodes the ownership ledger and returns the postures whose object outlived the
// call, the total signals sent to unprovable handles, and the postures covered — group (c).
func SweepBackgroundOwnership(ledger json.RawMessage) (survived, signals int, postures []string, err error) {
	if len(ledger) == 0 {
		return 0, 0, nil, fmt.Errorf("no ownership ledger to sweep: \"a process outlives the call that started it\" over no postures is vacuous")
	}
	var rows []backgroundOwnershipRow
	if err := json.Unmarshal(ledger, &rows); err != nil {
		return 0, 0, nil, fmt.Errorf("the carried ownership ledger is not JSON, so the survival count is unverifiable: %w", err)
	}
	seen := map[string]bool{}
	for _, row := range rows {
		if !slices.Contains(BackgroundPostures, row.Posture) {
			return 0, 0, nil, fmt.Errorf("ownership row declares posture %q, want one of %v — the probe looks at a DIFFERENT operating-system object per posture, so an unrecognised one would be probing the wrong object", row.Posture, BackgroundPostures)
		}
		if seen[row.Posture] {
			return 0, 0, nil, fmt.Errorf("posture %q appears twice — one row per posture, or one posture's evidence stands in for the other's", row.Posture)
		}
		seen[row.Posture] = true
		if strings.TrimSpace(row.Object) == "" || row.Test == "" {
			return 0, 0, nil, fmt.Errorf("ownership row %q names no operating-system object or no test: %+v", row.Posture, row)
		}
		if row.AliveAfterTheCallReturned {
			survived++
		}
		signals += row.SignalsToUnprovableHandles
		postures = append(postures, row.Posture)
	}
	slices.Sort(postures)
	return survived, signals, postures, nil
}

// --- (d) the exit notification -------------------------------------------------------------------------

// backgroundNoticeRow is one wake scenario: how many notices a SETTLED task produced under it, whether the run
// it belonged to was interrupted, and the mutation that turns the row red.
//
// `Mutation` is carried because exactly-once is the claim most easily satisfied by an accident. The epic
// measured its own: removing `notified_at IS NULL` from the single-winner UPDATE produces FOUR notifications,
// and a row that cannot name what breaks it is a row nobody has broken.
type backgroundNoticeRow struct {
	Scenario    string `json:"scenario"`
	Notices     int    `json:"notices"`
	Interrupted bool   `json:"interrupted_a_running_run"`
	Mutation    string `json:"mutation"`
	Test        string `json:"test"`
}

// SweepBackgroundNotices decodes the notice ledger and returns the scenarios whose settled task produced
// EXACTLY ONE notice, the scenarios that interrupted a running run (which must be none), and the scenarios
// carrying no mutation — group (d).
func SweepBackgroundNotices(ledger json.RawMessage) (exactlyOnce int, interrupted, unmutated []string, err error) {
	if len(ledger) == 0 {
		return 0, nil, nil, fmt.Errorf("no notice ledger to sweep: \"an exit calls the model back exactly once\" over no exits is vacuous")
	}
	var rows []backgroundNoticeRow
	if err := json.Unmarshal(ledger, &rows); err != nil {
		return 0, nil, nil, fmt.Errorf("the carried notice ledger is not JSON, so the exactly-once count is unverifiable: %w", err)
	}
	if len(rows) == 0 {
		return 0, nil, nil, fmt.Errorf("the notice ledger is EMPTY: a build that never notifies satisfies \"never twice\" trivially")
	}
	seen := map[string]bool{}
	for _, row := range rows {
		if strings.TrimSpace(row.Scenario) == "" || row.Test == "" {
			return 0, nil, nil, fmt.Errorf("a notice row names no scenario or no test: %+v", row)
		}
		if seen[row.Scenario] {
			return 0, nil, nil, fmt.Errorf("notice scenario %q appears twice", row.Scenario)
		}
		seen[row.Scenario] = true
		if row.Notices != 1 {
			return 0, nil, nil, fmt.Errorf("scenario %q produced %d notice(s) for a SETTLED task, want exactly 1 — two is the duplicate this epic's single-winner UPDATE exists to prevent, and zero is the silent loss every line of known-gaps refuses", row.Scenario, row.Notices)
		}
		exactlyOnce++
		if row.Interrupted {
			interrupted = append(interrupted, row.Scenario)
		}
		if strings.TrimSpace(row.Mutation) == "" {
			unmutated = append(unmutated, row.Scenario)
		}
	}
	return exactlyOnce, interrupted, unmutated, nil
}

// --- (e) the reaper ------------------------------------------------------------------------------------

// backgroundReaperRow is one thing the reaper does — a ceiling, a cancellation, an adoption, a refusal to
// signal, a concurrency limit, a collection — with the outcome it produced and where that outcome was READ.
//
// `MeasuredFrom` matters more here than anywhere else in this bundle, because a reaper is the component most
// able to satisfy its own tests: "the task was killed" read from the row the killer wrote is not a claim about
// a process. E24 measured `runners.capacity` written, read back and used in no decision, and repeating that
// with a background ceiling would have been the same defect with a new name.
type backgroundReaperRow struct {
	Duty         string `json:"duty"`
	Outcome      string `json:"outcome"`
	MeasuredFrom string `json:"measured_from"`
	Test         string `json:"test"`
}

// backgroundReaperDuties are the six things the reaper owes, and all six must be present. A missing one is not
// a smaller proof: each is a different way an orphan happens, and the plan named the orphan as this epic's
// reason for existing.
var backgroundReaperDuties = []string{"wall-time ceiling", "run cancellation", "restart adoption", "unprovable handle", "concurrency ceiling", "log collection"}

// SweepBackgroundReaper decodes the reaper ledger and returns the duties covered, the ones read from the
// operating system or the database rather than from our own record, and any duty missing — group (e).
func SweepBackgroundReaper(ledger json.RawMessage) (duties []string, fromTheMachine int, missing []string, err error) {
	if len(ledger) == 0 {
		return nil, 0, nil, fmt.Errorf("no reaper ledger to sweep: \"a ceiling, a cancellation, an adoption and a collector\" over nothing is vacuous")
	}
	var rows []backgroundReaperRow
	if err := json.Unmarshal(ledger, &rows); err != nil {
		return nil, 0, nil, fmt.Errorf("the carried reaper ledger is not JSON, so the duty coverage is unverifiable: %w", err)
	}
	seen := map[string]bool{}
	for _, row := range rows {
		if !slices.Contains(backgroundReaperDuties, row.Duty) {
			return nil, 0, nil, fmt.Errorf("reaper row declares duty %q, want one of %v", row.Duty, backgroundReaperDuties)
		}
		if seen[row.Duty] {
			return nil, 0, nil, fmt.Errorf("reaper duty %q appears twice", row.Duty)
		}
		seen[row.Duty] = true
		if strings.TrimSpace(row.Outcome) == "" || row.Test == "" {
			return nil, 0, nil, fmt.Errorf("reaper row %q names no outcome or no test: %+v", row.Duty, row)
		}
		if !slices.Contains(backgroundMeasurementSources, row.MeasuredFrom) {
			return nil, 0, nil, fmt.Errorf("reaper duty %q declares measured_from %q, want one of %v — a reaper is the component most able to satisfy its own tests, so \"the task was killed\" read from the row the killer wrote is refused here", row.Duty, row.MeasuredFrom, backgroundMeasurementSources)
		}
		if row.MeasuredFrom == "kernel" || row.MeasuredFrom == "daemon" || row.MeasuredFrom == "database" {
			fromTheMachine++
		}
		duties = append(duties, row.Duty)
	}
	for _, duty := range backgroundReaperDuties {
		if !seen[duty] {
			missing = append(missing, duty)
		}
	}
	slices.Sort(duties)
	return duties, fromTheMachine, missing, nil
}

// --- (f) the credential's two landing sites -------------------------------------------------------------

// backgroundRedactionRow is one place a background task's environment VALUE could land, with the bytes
// searched, the sentinel hits found, and the harmless token the same scan DID find in the same bytes.
//
// `Decoded` IS REQUIRED TO BE TRUE AND IT IS NOT DECORATION. A raw-byte sweep over gzip, base64 or JSON-escaped
// output can never fail, which this tree measured in E14 T7 and paid for again in E20 T4. Every row here says
// the bytes were DECODED before they were scanned, and the probe says the scan could see something.
type backgroundRedactionRow struct {
	Site       string `json:"site"`
	Bytes      int    `json:"bytes_scanned"`
	Decoded    bool   `json:"decoded"`
	Hits       int    `json:"sentinel_hits"`
	Probe      string `json:"probe"`
	ProbeFound bool   `json:"probe_found"`
}

// backgroundRedactionSites are the landing sites this epic named and closed. The first two are the ones T2 and
// T4 each recorded as OPEN where a reader would meet them, and T6 closed BOTH from one function — two
// redaction points being two chances to diverge. The last three are the durable rows an operator later reads.
var backgroundRedactionSites = []string{"the log the model reads", "the notice excerpt in commands.payload", "delivered_messages", "the background_tasks row", "the session journal"}

// SweepBackgroundRedaction decodes the redaction ledger and returns the sentinel hits, the sites whose probe
// was FOUND, and the sites covered — group (f).
func SweepBackgroundRedaction(ledger json.RawMessage) (hits, probed int, sites []string, err error) {
	if len(ledger) == 0 {
		return 0, 0, nil, fmt.Errorf("no redaction ledger to sweep: \"a credential value lands nowhere\" over no sites is vacuous")
	}
	var rows []backgroundRedactionRow
	if err := json.Unmarshal(ledger, &rows); err != nil {
		return 0, 0, nil, fmt.Errorf("the carried redaction ledger is not JSON, so the leak count is unverifiable: %w", err)
	}
	seen := map[string]bool{}
	for _, row := range rows {
		if !slices.Contains(backgroundRedactionSites, row.Site) {
			return 0, 0, nil, fmt.Errorf("redaction row declares site %q, which is not one of %v", row.Site, backgroundRedactionSites)
		}
		if seen[row.Site] {
			return 0, 0, nil, fmt.Errorf("two rows for site %q — a duplicated probe stands in for a missing one", row.Site)
		}
		seen[row.Site] = true
		if row.Bytes <= 0 {
			return 0, 0, nil, fmt.Errorf("redaction site %q scanned %d bytes — a scan over nothing reports zero hits and proves nothing", row.Site, row.Bytes)
		}
		if !row.Decoded {
			return 0, 0, nil, fmt.Errorf("redaction site %q was scanned WITHOUT decoding — a raw-byte sweep over encoded output can never fail, which is a green that certifies nothing", row.Site)
		}
		if strings.TrimSpace(row.Probe) == "" || !row.ProbeFound {
			return 0, 0, nil, fmt.Errorf("redaction site %q names no probe it actually found — a haystack nobody has shown was read is not a haystack", row.Site)
		}
		if strings.Contains(strings.ToUpper(row.Probe), "SENTINEL") {
			return 0, 0, nil, fmt.Errorf("redaction site %q uses the SENTINEL itself as its probe — the control has to be a token that is ALLOWED to be there, or finding it is the failure", row.Site)
		}
		hits += row.Hits
		probed++
		sites = append(sites, row.Site)
	}
	for _, site := range backgroundRedactionSites {
		if !seen[site] {
			return 0, 0, nil, fmt.Errorf("no row for %q — a site with no row is a site nobody scanned, and the two T2 and T4 each recorded as open are exactly the two a later reader would assume were covered", site)
		}
	}
	slices.Sort(sites)
	return hits, probed, sites, nil
}

// --- (g) the canonical vendor and on-machine ledger ------------------------------------------------------

// BackgroundContracts is the CANONICAL ledger of the published vendor requirements and on-machine measurements
// E26 ACTED ON — the AdminConsoleContracts / FleetContracts / ToolApprovalContracts discipline. A proof's
// contracts must EQUAL this table, so a bundle cannot build a surface while quietly dropping the row that
// named its cost.
//
// THE UNCONFIRMED ROWS ARE DELIBERATELY ABSENT AND THEIR ABSENCE IS THE HONEST ANSWER. §3.5 marks P9 (whether
// Docker keeps a stopped container's logs) and P11 (the harness's own default command timeout) UNCONFIRMED:
// neither could be read on a published page. A ledger of "published requirements we implement" is the wrong
// home for something nobody could read, and neither entered a test, a doc sentence or a bundle field.
// TestTheBackgroundLedgerRefusesTheUnconfirmedRows pins that.
var BackgroundContracts = []ContractRequirement{
	{
		Divergence: "P1",
		SourceURL:  "https://code.claude.com/docs/en/tools-reference (fetched 2026-07-30)",
		Requirement: "⭐⭐ THE SHAPE OF THE WHOLE FEATURE, TAKEN VERBATIM FROM THE VENDOR THIS EPIC REPLICATES: " +
			"\"For long-running processes such as dev servers or watch builds, Claude can set " +
			"`run_in_background: true` to start the command as a background task and continue working while it " +
			"runs.\" So backgrounding is a PARAMETER on the existing command tool, not a second tool — " +
			"`palai.workspace.shell`'s `background` field. The deviation is named: Palai has no `/tasks`, and " +
			"listing is `background_tasks` rows plus a console screen that is E28's",
	},
	{
		Divergence: "P2",
		SourceURL:  "https://code.claude.com/docs/en/tools-reference (fetched 2026-07-30)",
		Requirement: "⭐⭐ THIS ONE LINE DECIDED HALF THE DESIGN: \"`TaskOutput` — Retrieves output from a background " +
			"task. **Deprecated in favor of `Read` on the task's output file path.**\" The vendor BUILT a " +
			"dedicated output-reading tool and then went back to ordinary file reads. So Palai adds no fourth " +
			"output mechanism and no new read tool: the log is written into `.palai-session/bg/<task_id>.log` — " +
			"a subtree the snapshot already skips whole — and read with the `palai.workspace.file` tool that " +
			"already reads files under the allocation root with escape control",
	},
	{
		Divergence: "P3",
		SourceURL:  "https://code.claude.com/docs/en/tools-reference (fetched 2026-07-30)",
		Requirement: "\"`TaskStop` — Stops a running background task by ID.\" Taken verbatim as " +
			"`palai.workspace.background_kill`, whose only argument is `task_id`. The deviation is who may call " +
			"it: in the vendor's harness stopping a task is also a USER command, and in Palai it is the model " +
			"and the operator (CLI) — there is no end-user surface for it",
	},
	{
		Divergence: "P5",
		SourceURL:  "https://code.claude.com/docs/en/tools-reference (fetched 2026-07-30)",
		Requirement: "THE KILL SWITCH IS TAKEN AND ITS SEMANTICS ARE MADE STRICTER THAN THE VENDOR'S WORDING " +
			"REQUIRES: \"setting `CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1` disables auto-backgrounding along with " +
			"the rest of the background task functionality.\" `PALAI_BACKGROUND_DISABLED=1` REFUSES a " +
			"`background: true` call rather than silently running it in the foreground, because a silent " +
			"downgrade means the model believes it backgrounded something that is in fact blocking — the one " +
			"behaviour this epic exists to avoid. The vendor's `sleep` exception is NOT taken: it belongs to " +
			"auto-backgrounding, which Palai does not have",
	},
	{
		Divergence: "P6",
		SourceURL:  "https://code.claude.com/docs/en/tools-reference (fetched 2026-07-30)",
		Requirement: "\"A `cd`, `pushd`, `popd`, or `chdir` inside a command that is moved to the background never " +
			"carries over: `Session cwd remains <dir>; directory changes made by the backgrounded command do not " +
			"apply to subsequent commands.`\" In Palai this is STRUCTURALLY true and it is an accident rather " +
			"than a design — every command runs in its own process or container with its working directory set " +
			"afresh — so it is converted into a DECISION by a test rather than left to be re-derived by the next " +
			"person who changes the executor",
	},
	{
		Divergence: "P7",
		SourceURL:  "https://developers.openai.com/api/docs/guides/background (fetched 2026-07-30)",
		Requirement: "⭐ THE NAME COLLISION, RECORDED BECAUSE CONFUSING THE TWO WOULD BE EXPENSIVE: another vendor " +
			"uses the same word for the opposite scope — \"make an API request with `background` set to `true`\" " +
			"makes the RESPONSE asynchronous and the client polls it. E26's `background` makes ONE TOOL CALL " +
			"asynchronous while the response stream stays open. Palai is a Responses-shaped API, so the field " +
			"name is confined to the tool argument and appears NOWHERE in a response body. What IS taken from " +
			"this page is \"Cancelling twice is idempotent\": killing a task twice is killing it once",
	},
	{
		Divergence: "P8",
		SourceURL:  "https://docs.docker.com/engine/logging/configure/ (fetched 2026-07-30)",
		Requirement: "A PUBLISHED FACT USED TO REFUSE A DESIGN RATHER THAN TO BUILD ONE: \"As a default, Docker uses " +
			"the `json-file` logging driver, which caches container logs as JSON internally\" and \"By default, " +
			"no log-rotation is performed.\" `docker logs` would have been a second read path for a background " +
			"task's output, and two read paths mean two redaction paths of which only one gets maintained. The " +
			"container writes its own file into the bind mount instead — and the unrotated default is the second " +
			"reason: trusting json-file would have meant unbounded disk, where this file's ceiling is the " +
			"task's wall clock",
	},
	{
		Divergence: "P10",
		SourceURL:  "MEASURED on this machine — https://pkg.go.dev/os/exec could not be fetched (ECONNRESET, 2026-07-30), so POSIX reparenting entered this epic as a measurement rather than as a citation (adapters/sandboxes/host/background_test.go)",
		Requirement: "⭐ A VENDOR PAGE THAT WOULD NOT LOAD BECAME A TEST INSTEAD OF AN ASSUMPTION. That `exec.Command` " +
			"plus `Start` leaves a child alive after its parent's context is cancelled — and that the child must " +
			"still be `Wait`ed by somebody or it becomes a zombie — is standard POSIX behaviour and is NOT " +
			"counted as a source here. `TestAHostBackgroundProcessSurvivesTheAttempt` measures it on the machine " +
			"with `kill(-pgid, 0)`, and `TestTodayAHostProcessGroupDiesWithTheAttempt` keeps the OLD rule " +
			"measured beside it so the synchronous posture cannot drift",
	},
	{
		Divergence: "X1",
		SourceURL:  "MEASURED on macOS 26.3 under one uid, three ways, against a live process carrying a sentinel in its environment (E26 T6, 2026-07-31)",
		Requirement: "⭐⭐ THE PLAN'S OWN CEILING DOES NOT WORK ON THE PLATFORM THIS EPIC TARGETS, AND OVERSTATING A " +
			"CEILING IS AS WRONG AS UNDERSTATING ONE. §0.4 wrote that \"the same uid can read the value with " +
			"`ps -E` / `/proc/<pid>/environ`\". On macOS 26.3 `ps -E` and `ps e` disclose NO environment — not " +
			"another process's and not your own — and there is no `/proc`: a sleep started with a sentinel in " +
			"its environment, probed three ways under the same uid, returned ZERO hits. The exposure's DIRECTION " +
			"is unchanged and is what the runbook states — background LENGTHENS the window rather than widening " +
			"it, and its length is `deadline_at` — and the container posture still has `/proc/<pid>/environ`, so " +
			"the runbook states both platforms instead of repeating a demonstration an operator would find does " +
			"not run",
	},
}

// backgroundContractParts flattens the canonical ledger into hashParts input, so the digest is re-derivable
// from the CODE table alone and a bundle cannot present a self-consistent digest over an edited ledger.
func backgroundContractParts() []string {
	parts := make([]string, 0, 3*len(BackgroundContracts))
	for _, req := range BackgroundContracts {
		parts = append(parts, req.Divergence, req.SourceURL, req.Requirement)
	}
	return parts
}

// BackgroundContractsDigest is hashParts over the CANONICAL contract ledger — the E26 bundle's checksum
// anchor. A dropped or reworded §3.5 row, or a deleted on-machine measurement, moves every checksum in the
// release.
func BackgroundContractsDigest() string { return hashParts(backgroundContractParts()...) }

// --- the proof --------------------------------------------------------------------------------------------

// BackgroundProof is the evidence a background_claim requires (plan §T7 — the E26 EXIT anchor). Its groups are
// the plan's (a)..(f) plus the contract ledger, and every counter is RE-DERIVED from the carried bytes:
//
//	(a) SemanticsLedger / SemanticsReplicated / SemanticsMeasuredFromTheMachine — all six of §2, each naming a
//	    SHIPPED test, and the DEFINING one (§2.6, the model is not blocked) measured from the kernel;
//	(b) RefusalLedger / ProcessesStartedUnderARefusal / RefusalsWithANonVacuityControl — the first MUST BE
//	    ZERO and every refusal MUST carry its positive half, because at the fork point zero was free;
//	(c) OwnershipLedger / PosturesThatOutliveTheirCall / SignalsToUnprovableHandles — both postures, and the
//	    second MUST BE ZERO;
//	(d) NoticeLedger / ScenariosNotifyingExactlyOnce — every scenario exactly one, none interrupting a run
//	    that never parked, and each naming the mutation that reddens it;
//	(e) ReaperLedger / ReaperDutiesMeasuredFromTheMachine — all six duties, none read off our own record;
//	(f) RedactionLedger / EnvironmentValuesInAnyLandingSite — MUST BE ZERO, over sites that were DECODED and
//	    each of which names a harmless token the same scan found.
//
// HONEST CEILING, MECHANICALLY ENFORCED: Machine must be the literal "local", and the field is not called
// `Peer` because there is no peer — see this section's header.
type BackgroundProof struct {
	Machine string `json:"machine"`

	// (a) The six semantics of §2, replicated and each proven by a shipped test.
	SemanticsLedger                 json.RawMessage `json:"semantics_ledger"`
	SemanticsReplicated             int             `json:"semantics_replicated"`
	SemanticsMeasuredFromTheMachine int             `json:"semantics_measured_from_the_machine"`

	// (b) Every refusal starts nothing, and every refusal has a control that starts something.
	RefusalLedger                  json.RawMessage `json:"refusal_ledger"`
	ProcessesStartedUnderARefusal  int             `json:"processes_started_under_a_refusal"`
	RefusalsWithANonVacuityControl int             `json:"refusals_with_a_non_vacuity_control"`

	// (c) The process outlives the call, in both postures, and an unprovable handle is never signalled.
	OwnershipLedger              json.RawMessage `json:"ownership_ledger"`
	PosturesThatOutliveTheirCall int             `json:"postures_that_outlive_their_call"`
	SignalsToUnprovableHandles   int             `json:"signals_to_unprovable_handles"`

	// (d) An exit calls the model back exactly once, and never interrupts a run that did not park.
	NoticeLedger                  json.RawMessage `json:"notice_ledger"`
	ScenariosNotifyingExactlyOnce int             `json:"scenarios_notifying_exactly_once"`

	// (e) The reaper's six duties, each read from the machine or the database.
	ReaperLedger                       json.RawMessage `json:"reaper_ledger"`
	ReaperDutiesMeasuredFromTheMachine int             `json:"reaper_duties_measured_from_the_machine"`

	// (f) A credential value lands in none of the places a background task could put one.
	RedactionLedger                   json.RawMessage `json:"redaction_ledger"`
	EnvironmentValuesInAnyLandingSite int             `json:"environment_values_in_any_landing_site"`

	// (g) The published contracts and on-machine measurements, anchored to the code table.
	Contracts       []ContractRequirement `json:"contracts"`
	ContractsDigest string                `json:"contracts_digest"`
}

// Complete reports the groups hold on a LOCAL machine AND re-derives (a) through (f) from the bytes the proof
// carries. A proof declaring six semantics over a ledger holding five, a zero over a refusal ledger containing
// a spawn, one posture standing in for two, a notice count nobody produced, or a zero over a redaction ledger
// with a hit fails HERE — in the shape verifier — rather than in a dedicated test somebody could forget to run.
func (p BackgroundProof) Complete() bool {
	if p.Machine != BackgroundMachine || p.ContractsDigest != BackgroundContractsDigest() ||
		!slices.Equal(p.Contracts, BackgroundContracts) {
		return false
	}
	// (a) All six semantics, and the DEFINING one measured from the kernel rather than from a row.
	covered, fromTheMachine, tests, err := SweepBackgroundSemantics(p.SemanticsLedger)
	if err != nil || len(covered) != BackgroundSemanticsReplicated ||
		p.SemanticsReplicated != len(covered) || p.SemanticsMeasuredFromTheMachine != fromTheMachine {
		return false
	}
	if !slices.Contains(covered, backgroundDefiningSemantic) || len(tests) != len(covered) {
		return false
	}
	if fromTheMachine < 1 {
		return false // every liveness claim read off our own record is a claim about our record
	}
	// (b) Nothing spawned under a refusal, and no refusal without its positive half.
	spawns, controlled, uncontrolled, err := SweepBackgroundRefusals(p.RefusalLedger)
	if err != nil || spawns != 0 || p.ProcessesStartedUnderARefusal != 0 ||
		len(uncontrolled) != 0 || p.RefusalsWithANonVacuityControl != controlled || controlled < 2 {
		return false
	}
	// (c) Both postures outlive their call, and no unprovable handle was signalled.
	survived, signals, postures, err := SweepBackgroundOwnership(p.OwnershipLedger)
	if err != nil || len(postures) != len(BackgroundPostures) || survived != len(postures) ||
		signals != 0 || p.PosturesThatOutliveTheirCall != survived || p.SignalsToUnprovableHandles != 0 {
		return false
	}
	// (d) Exactly one notice per settled task, in every scenario, and no run interrupted.
	exactlyOnce, interrupted, unmutated, err := SweepBackgroundNotices(p.NoticeLedger)
	if err != nil || len(interrupted) != 0 || len(unmutated) != 0 ||
		p.ScenariosNotifyingExactlyOnce != exactlyOnce || exactlyOnce < 3 {
		return false
	}
	// (e) All six reaper duties, none read off our own bookkeeping.
	duties, reaperFromTheMachine, missing, err := SweepBackgroundReaper(p.ReaperLedger)
	if err != nil || len(missing) != 0 || len(duties) != len(backgroundReaperDuties) ||
		reaperFromTheMachine != len(duties) || p.ReaperDutiesMeasuredFromTheMachine != reaperFromTheMachine {
		return false
	}
	// (f) No environment value in any landing site, over decoded bytes with a probe in each.
	hits, probed, sites, err := SweepBackgroundRedaction(p.RedactionLedger)
	if err != nil || hits != 0 || p.EnvironmentValuesInAnyLandingSite != 0 ||
		probed != len(backgroundRedactionSites) || len(sites) != len(backgroundRedactionSites) {
		return false
	}
	return true
}

// carriesE26BackgroundCase reports whether a case is one of the five ids E26 OPENED — the FAMILY marker,
// shared by the manifest verifier and PromoteGateFor so the two can never disagree about what an E26 release
// is.
//
// THE FAMILY IS RECOGNIZED BY THE CASE IDS, NEVER BY THE background_claim THE GATE ENFORCES. Dispatching on
// the claim marker is precisely how a release DROPS it, reroutes to a weaker family and passes — the
// promote-gate-family-dispatch defect this repository has shipped once already.
func carriesE26BackgroundCase(c evidenceCase) bool {
	return slices.Contains(BackgroundCaseIDs, c.ID)
}

// verifyE26BackgroundPresence stops the re-derivations from being OPTIONAL: a manifest carrying ANY of the
// five E26 cases MUST carry EXACTLY ONE background_claim with its proof. "Exactly one" because
// BackgroundPromoteGate judges the first while this verifier checks all of them, so a second fabricated proof
// could ride behind an honest one.
func verifyE26BackgroundPresence(m evidenceManifest) []Finding {
	family, claims, withProof := false, 0, 0
	for _, c := range m.Cases {
		if carriesE26BackgroundCase(c) {
			family = true
		}
		if c.BackgroundClaim != "" {
			claims++
			if c.BackgroundProof != nil {
				withProof++
			}
		}
	}
	if !family {
		return nil
	}
	switch {
	case claims == 0:
		return []Finding{{Kind: "missing", Detail: fmt.Sprintf(
			"background_claim (this manifest carries E26 case(s) from %v, so it must carry the anchor that judges them — without it \"the model is not blocked\", \"a refused call starts nothing\" and \"an exit notifies exactly once\" ship unverified behind five green rows; plan §T7)",
			BackgroundCaseIDs)}}
	case claims > 1:
		return []Finding{{Kind: "invalid", Detail: fmt.Sprintf(
			"%d background_claims in one manifest (want exactly 1): BackgroundPromoteGate judges the FIRST background proof, so a second could ride behind an honest one — one release, one re-derivation (plan §T7)", claims)}}
	case withProof != claims:
		return []Finding{{Kind: "missing", Detail: "background_proof (the background claim carries no proof body)"}}
	}
	return nil
}

// --- the canonical bytes the proof carries ---------------------------------------------------------------
//
// THE LEDGERS ARE AUTHORED, AND WHAT MAKES THEM HONEST IS TWO THINGS THIS FILE DOES NOT DO. First the CO-RUN:
// every row below records an outcome one of the SHIPPED suites produces, and `scripts/uat/background` runs
// those suites in the SAME invocation that verifies this bundle, with tests/uat/background/journey_test.go
// diffing `--- PASS` lines against the legs claimed here. Second the RE-DERIVATION FROM THE TREE:
// tests/uat/background re-derives every `test` field against the tree's own source AND against the `-run`
// allow-list in scripts/test/component — because a test named in a ledger that the shipped selector does not
// select is a test that never runs, and a `-run` matching nothing exits 0 in silence.

// BackgroundSemanticsLedger is group (a): §2's six replicated semantics, each with the shipped test that
// proves it and where its measurement came from.
//
// SEMANTIC 6 IS THE ONE TO READ. "It ran in the background" is satisfied by a build that parks the run and
// waits; what this row claims is that the spawn returned, the process was verified ALIVE from the kernel, the
// model then dispatched `palai.workspace.file` through the production dispatcher, that write COMPLETED and
// landed on disk, and the process was verified alive AGAIN afterwards — so the two genuinely overlapped rather
// than merely both happening.
const BackgroundSemanticsLedger = `[
  {"semantic": 1, "claim": "the tool returns a HANDLE while the process is still running, never a result: {task_id, output_path, status:\"running\"} and no output at all", "test": "TestABackgroundShellCallReturnsAHandleWhileTheProcessIsStillRunning", "package": "apps/control-plane/internal/execution", "measured_from": "kernel"},
  {"semantic": 2, "claim": "the output is readable MID-FLIGHT through the file tool that already reads files: two successive reads of a still-running task's log return a growing prefix", "test": "TestABackgroundTasksOutputIsReadableMidFlight", "package": "apps/control-plane/internal/execution", "measured_from": "frame"},
  {"semantic": 3, "claim": "the exit calls the model back: a parked run re-enters exactly once and the turn it sees carries the task id, the exit code, the output path and a bounded tail", "test": "TestAParkedRunReentersExactlyOnceWhenItsBackgroundTaskFinishes", "package": "apps/control-plane/internal/execution", "measured_from": "database"},
  {"semantic": 4, "claim": "the handle kills: after background_kill the process GROUP is gone, and killing twice is killing once", "test": "TestBackgroundKillStopsTheProcessGroupAndKillingTwiceIsKillingOnce", "package": "apps/control-plane/internal/execution", "measured_from": "kernel"},
  {"semantic": 5, "claim": "three tasks run at the SAME MOMENT and their outputs do not interleave: all three process groups alive together, each log carrying its own marker and NEITHER of the other two", "test": "TestThreeBackgroundTasksRunConcurrentlyWithoutInterleavingTheirOutput", "package": "apps/control-plane/internal/execution", "measured_from": "kernel"},
  {"semantic": 6, "claim": "THE DEFINING PROPERTY: after the spawn returned and while the process was verified alive from the kernel, the model dispatched ANOTHER tool through the production dispatcher, that call COMPLETED and its write landed on disk, and the process was still alive afterwards — the run never left 'running' and two tool.result frames reached the model", "test": "TestTheModelCallsAnotherToolWhileTheBackgroundProcessIsStillRunning", "package": "apps/control-plane/internal/execution", "measured_from": "kernel"}
]`

// BackgroundRefusalLedger is group (b): every path that refuses to start a background process, with the
// spawn counter under the refusal and under the SAME harness with the refusal's condition lifted.
//
// THE CONTROL COLUMN EXISTS BECAUSE THIS EPIC WATCHED THE ZERO BE FREE. T2's approval-gate RED passed at the
// fork point — there was no spawn path, so nothing spawning was guaranteed — and a security proof that cannot
// fail reports the same green as one that passes.
const BackgroundRefusalLedger = `[
  {"refusal": "an approval_required tool called with background:true, before a human decides", "spawns": 0, "control": "the IDENTICAL call on the IDENTICAL harness with the gate OFF", "control_count": 1, "control_unit": "background spawns", "test": "TestAGatedToolCalledWithBackgroundSpawnsNothingUntilAHumanDecides"},
  {"refusal": "PALAI_BACKGROUND_DISABLED=1 — REFUSED rather than silently downgraded to a synchronous run", "spawns": 0, "control": "the same shell call WITHOUT the background flag under the same kill switch, which runs and returns its output: the switch disables background execution, not the shell tool. The unit is a SYNCHRONOUS run because a silent downgrade is this refusal's dangerous failure, and a background-spawn counter cannot tell a downgrade from a refusal", "control_count": 1, "control_unit": "synchronous runs", "test": "TestBackgroundIsRefusedRatherThanDowngradedWhenTheFeatureIsDisabled"},
  {"refusal": "the sixth task of a run at a per-run ceiling of five — refused, never queued", "spawns": 0, "control": "the five tasks below the ceiling, counted from the DATABASE and the PROCESS TABLE rather than from a number we returned", "control_count": 5, "control_unit": "background spawns", "test": "TestTheSixthBackgroundTaskOfARunIsRefusedRatherThanQueued"},
  {"refusal": "a task carrying an environment credential with no deadline — a code gate rather than a CHECK, because unbounded is legitimate for a task carrying none", "spawns": 0, "control": "the identical call with NO environment, accepted with a NULL deadline in the same test", "control_count": 1, "control_unit": "background spawns", "test": "TestACredentialCarryingTaskCannotBeCreatedWithoutADeadline"},
  {"refusal": "an output path that escapes the allocation root — refused before anything starts", "spawns": 0, "control": "the same executor, the same command, a path INSIDE the allocation. ADDED BY THIS EXIT GATE after measuring that the guard had no control at all: an executor whose Start refused EVERY input satisfied its three refusals, demonstrated by mutation", "control_count": 1, "control_unit": "background spawns", "test": "TestAnEscapingOutputPathIsRefusedBeforeAnythingStarts"}
]`

// BackgroundOwnershipLedger is group (c): the two sandbox postures, the operating-system object each probe
// looks at, and the absence that is this bundle's sharpest claim.
const BackgroundOwnershipLedger = `[
  {"posture": "unsandboxed-host", "object": "a process GROUP (Setpgid), probed with kill(-pgid, 0) against the pgid the process itself wrote down", "alive_after_the_call_returned": true, "signals_to_unprovable_handles": 0, "test": "TestAHostBackgroundProcessSurvivesTheAttempt"},
  {"posture": "sandboxed-linux", "object": "a container labelled io.palai.bg=<task_id>, probed with ContainerInspect against the real daemon", "alive_after_the_call_returned": true, "signals_to_unprovable_handles": 0, "test": "TestADetachedContainerIsStillRunningAfterTheCallReturns"}
]`

// BackgroundNoticeLedger is group (d): the wake scenarios, each with the mutation that turns it red.
//
// THE FOURTH ROW IS THE ONE A TIDY BUILD WOULD HAVE DROPPED. A run that is already terminal has no turn left
// for a notice to fold into, and both easy answers are wrong: queued, it sits until a sweep expires it unread;
// dropped, it is the silent loss every line of known-gaps refuses. It is STAMPED, and an operator sees it.
const BackgroundNoticeLedger = `[
  {"scenario": "a parked run whose task exits", "notices": 1, "interrupted_a_running_run": false, "mutation": "removing 'notified_at IS NULL' from the single-winner UPDATE produces FOUR notifications", "test": "TestAParkedRunReentersExactlyOnceWhenItsBackgroundTaskFinishes"},
  {"scenario": "two reconciler ticks, two control planes swept concurrently, and a crash-restart plane that never saw the spawn", "notices": 1, "interrupted_a_running_run": false, "mutation": "the same UPDATE mutation; the scenario is built so a mutex could not pass it", "test": "TestTwoTicksTwoPlanesAndARestartProduceOneBackgroundNotice"},
  {"scenario": "a run that never parked, still running when its task exits", "notices": 1, "interrupted_a_running_run": false, "mutation": "making the ENQUEUE conditional on 'waiting' loses the notification for a run that never stopped", "test": "TestARunningRunIsNotInterruptedAndTheNoticeFoldsAtTheNextBoundary"},
  {"scenario": "a run already terminal when its task exits — the notice is stamped orphaned rather than queued or dropped", "notices": 1, "interrupted_a_running_run": false, "mutation": "deleting the terminal-run branch queues a notice against a run that cannot read it", "test": "TestATerminalRunsBackgroundNoticeIsStampedRatherThanDropped"}
]`

// BackgroundReaperLedger is group (e): the six duties, each read from the machine or the database.
//
// 'restart adoption' IS PRESENT, WHICH SETTLES A QUESTION THE PLAN LEFT OPEN. §4.2 wrote that if T5b were
// skipped one sentence had to be written verbatim — "a control plane restart leaves running background tasks
// 'lost'; the processes live on and nobody reaps them". T5 shipped adoption, so that sentence is NOT owed and
// is not written. What remains is the ceiling one line down: adoption works on the SAME machine only.
const BackgroundReaperLedger = `[
  {"duty": "wall-time ceiling", "outcome": "a task past deadline_at is killed, marked 'expired', and the model LEARNS it — enforced by the reaper reading a COLUMN, because a context does not survive the restart a background process exists to survive. Unset means 60m; unbounded must be written explicitly as 0", "measured_from": "kernel", "test": "TestATaskPastItsDeadlineIsKilledMarkedExpiredAndTheModelIsTold"},
  {"duty": "run cancellation", "outcome": "a cancelled run kills every live task it started — which it did NOT do before this epic: CancelRunReconciled drove the run to canceled and signalled no process at all", "measured_from": "kernel", "test": "TestCancellingARunKillsEveryLiveBackgroundTaskOfIt"},
  {"duty": "restart adoption", "outcome": "after a restart the reaper adopts its 'running' rows from the ROW rather than from memory: alive stays alive, dead-but-unreaped becomes 'exited' with a NULL exit code when unknown rather than an invented one", "measured_from": "database", "test": "TestARestartedControlPlaneAdoptsItsRunningTasksFromTheRowRatherThanFromMemory"},
  {"duty": "unprovable handle", "outcome": "a handle whose recorded start time does not match the live process becomes 'lost' and receives NO SIGNAL AT ALL — measured with a REAL process started outside the program, a fabricated start time, and that process still running afterwards", "measured_from": "kernel", "test": "TestAnUnprovableHandleBecomesLostAndReceivesNoSignal"},
  {"duty": "concurrency ceiling", "outcome": "the machine ceiling is counted ACROSS TENANTS from the database rather than from a number we returned — the E24 'runners.capacity' defect (written, read back, used in no decision) not repeated with a new name", "measured_from": "database", "test": "TestTheMachineCeilingIsCountedAcrossTenantsFromTheDatabase"},
  {"duty": "log collection", "outcome": "a finished task's log is deleted after PALAI_BACKGROUND_LOG_TTL and a running task's is not — without it '.palai-session' grows unbounded INVISIBLY, because the snapshot skips that directory, which is what a silent disk leak looks like", "measured_from": "database", "test": "TestAFinishedTasksLogIsDeletedAfterItsTTLAndARunningTasksIsNot"}
]`

// BackgroundRedactionLedger is group (f): every place a background task's environment VALUE could land,
// scanned after DECODING.
//
// THE FIRST TWO ROWS ARE THE TWO CEILINGS T2 AND T4 EACH WROTE DOWN WHERE THEY WOULD BE MET rather than in a
// plan — the log a process writes straight to disk bypasses the redaction a synchronous result gets on the
// captured Go string, and the exit notice's 2 KiB tail lands in commands.payload and then delivered_messages,
// which the synchronous path does not do. T6 closed BOTH from ONE function, because two redaction points are
// two chances to diverge.
//
// THE BYTE COUNTS ARE A MEASUREMENT OF ONE RUN, NOT A PIN. What the gate diffs is the ZERO, the DECODED flag
// and the probe names — never these totals, which would turn a log-format edit into a red bundle and teach the
// next reader to regenerate rather than to read.
const BackgroundRedactionLedger = `[
  {"site": "the log the model reads", "bytes_scanned": 2048, "decoded": true, "hits": 0, "probe": "PALAI_BG_PROBE", "probe_found": true},
  {"site": "the notice excerpt in commands.payload", "bytes_scanned": 1573, "decoded": true, "hits": 0, "probe": "PALAI_BG_PROBE", "probe_found": true},
  {"site": "delivered_messages", "bytes_scanned": 1698, "decoded": true, "hits": 0, "probe": "PALAI_BG_PROBE", "probe_found": true},
  {"site": "the background_tasks row", "bytes_scanned": 612, "decoded": true, "hits": 0, "probe": "env_keys", "probe_found": true},
  {"site": "the session journal", "bytes_scanned": 4271, "decoded": true, "hits": 0, "probe": "background_notice", "probe_found": true}
]`
