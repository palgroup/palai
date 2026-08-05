//go:build uat

// The E28 T4 EXIT-gate journey entry point. Like the E15 T6 / E16 T8 / E17 T11 / E19 T9 / E20 T5 / E21 T7 /
// E22 T7 / E23 T7 / E24 T8 / E25 T9 / E26 T7 gates before it, this file is an ORCHESTRATOR rather than a
// reimplementation: it drives `scripts/uat/fleet-console`, which stands up a throwaway Postgres, builds the
// console and runs the narrative where its seams already live.
//
// THE NARRATIVE IS THE CO-RUN RATHER THAN AN EXTRA TEST, and what this file adds is the thing a co-run cannot
// give itself: the assertion that every leg RAN. The bundle's per-case status and its eight ledgers are
// authored data; a `-run` pointed at the wrong package matches nothing at all and exits 0 in silence.
//
// AND THE LEG LIST IS DIFFED AGAINST THE RUN'S OWN PASS MARKS, NEVER READ OUT OF THE SCRIPT. That distinction
// is why this file exists in this shape from birth: E26 T7 declared a leg its own gate script never ran, and
// found it exactly this way — the test was UNTAGGED, so it rode `make verify` and was never absent from the
// TREE; what it was absent from was the invocation that claimed it, and reading the script or reading the leg
// list would have found nothing, because both looked complete. So nothing below parses
// scripts/uat/fleet-console. Every assertion is against a mark THIS RUN printed.
//
// THE TWO MARK SHAPES ARE DIFFERENT AND BOTH ARE LOAD-BEARING. Go's `-v` prints `--- PASS: TestName`;
// Playwright's `--reporter=list` prints `✓  <n> [project] › tests/file.spec.ts:<line>:<col> › <title>`. The
// script overrides the config's `line` reporter for precisely this reason: `line` prints a title when a test
// STARTS, so a title in that output proves nothing about whether it passed.
//
// HONEST CEILING (plan §6): every machine here is a FAKE runner built from the shipped enrolment package, and
// every browser leg drives Chromium against a deterministic fixture upstream on ONE box. There is no rented
// Mac, no deployed console and no screen reader. `FLT-P15` stands over all of it.
package fleetconsole

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/tests/uat"
)

// TestFleetConsoleJourneyRunsThroughTheOperatorEntryPoint runs the §T4 narrative through the shipped target.
// It is Docker-bound (a throwaway Postgres) and toolchain-bound (the console build), so it rides the `uat` tag
// and never `make verify`. It needs NO credential: every leg is deterministic against a fixture upstream, real
// processes and real PostgreSQL.
func TestFleetConsoleJourneyRunsThroughTheOperatorEntryPoint(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available; the E28 backing suites need a throwaway Postgres")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "scripts/uat/fleet-console")
	cmd.Dir = repoRoot(t)
	cmd.Env = append(cmd.Environ(), "SKIP_JOURNEYS=0")
	out, err := cmd.CombinedOutput()
	t.Logf("$ scripts/uat/fleet-console\n%s", out)
	if err != nil {
		t.Fatalf("the E28 fleet-console gate failed: %v", err)
	}
	run := string(out)

	// --- the GO legs, diffed against `--- PASS` -----------------------------------------------------------
	//
	// EACH LINE IS ONE LEG OF THE NARRATIVE, and each is named because it is the leg that would rot first if a
	// `-run` selector drifted. Read the diff, never the selector.
	for _, leg := range []struct{ test, why string }{
		// --- T1: a pool's birth path, and the state E24 T6 could decide but nobody could produce ---
		{"TestPoolBirthReachesTheWaitingRoomAndTheApproveRoute", "FLC-001 — THE WHOLE EPIC IN ONE TEST: a pool created with posture `unsandboxed-host` and strict enrolment ON through the PUBLIC route, a real machine enrolling into it over real mTLS against real PostgreSQL, landing `pending`, a Dial answering ErrPoolHasNoRunner so the waiting room is STRUCTURAL rather than a comparison, and the approve route reached over HTTP against a machine an operator's own pool produced — which had never happened in this tree"},
		{"TestPoolBirthRendersTheSameShapeAsTheListing", "FLC-001 — ONE projection serves the create, the patch and the list, because a create answering a shape the read does not repeat makes the screen code against two shapes"},
		{"TestPoolBirthRefusesADuplicateNameAsAConflictNotAnError", "FLC-001 — a 409 built from 000045's UNIQUE index rather than a 500 telling an operator their control plane is broken"},
		{"TestPoolBirthValidatesThePostureOnTheRouteRatherThanAtTheDatabase", "FLC-001 — the database's CHECK is the LAST defence, not the first"},
		{"TestPoolBirthStrictSwitchIsTheOnlyPatchableField", "FLC-001 — posture is NOT patchable, and that is correctness rather than restriction: a machine INHERITS its pool's posture at enrolment, so moving it would retroactively change what the machines already in it ARE"},
		{"TestPoolBirthIsProvisionGatedAndTenantScoped", "FLC-001 — a run-only key gets 403 on both writes, and a foreign tenant's pool is not there"},
		{"TestPoolBirthLeavesTheBornWithPoolBitUnchanged", "FLC-001 — the seed every tenant gets at birth is byte-unchanged, which is what makes the new path an ADDITION rather than a replacement"},
		{"TestTheDefaultPoolSeedStatementIsUnchanged", "FLC-001 — InsertDefaultRunnerPool still writes its three literals, pinned so the birth path cannot quietly become the only path"},
		{"TestTheSeedStatementTakesExactlyTwoParameters", "FLC-001 — the other direction of the same pin. It was ...ThreeParameters until `2dec6c83` dropped organization_id from the statement, and the bound moved DOWN with the column rather than the pin being relaxed"},
		{"TestPoolCreateReachesTheRealRouter", "FLC-001 — the CLI half, against the real mux rather than a stub: `palai pool create` is the verb the E24 handover block promised and never had"},
		{"TestPoolSetStrictReachesTheRealRouter", "FLC-001 — the switch FLT-P12 said an operator could throw, which no operator could throw"},
		{"TestE24HandoverBlockStillDoesNotWork", "FLC-001 — §3.6 D2 pinned as a TEST: `palai admin pool create …` is what a shipped plan's copy-paste block told an owner to run, and it still errors, so the runbook's correction is the shipped one"},
		{"TestPoolNeedsTheProvisionCapability", "FLC-001 — the CLI's own gate, asserted rather than inherited"},
		{"TestPoolCreateNeedsAProjectScope", "FLC-001 — a pool is a tenant-scoped name"},
		{"TestEveryE24ExportedSurfaceHasAProductionCaller", "FLC-001's RIDER — FLT-P14 closes: `RunnerGateway.Waiting` had no reader for a whole epic, and E24's own sweep now reports it REACHED with its named exception DELETED rather than kept green"},
		{"TestForeignPoolWriteAndUpdateAreRejected", "FLC-001 — the tenancy corpus over the two write routes E28 opened on a tenant table"},

		// --- the gate's own guards ---
		{"TestEveryE28ExportedSurfaceHasAProductionCaller", "the job a gate exists for — five symbols, and the epic that closed the FIFTH instance of \"built, tested, reachable from nothing\" is not allowed to ship the sixth"},
		{"TestTheReachabilitySweepCanActuallyFail", "the guard for the guard: a collector that found a caller for everything would report exactly the same green"},
		{"TestEveryE28ComponentTestIsNamedInTheRunAllowList", "a guard this `-run` does not name is a guard this tier never runs"},
		{"TestEveryTestNamedInABundleLedgerExistsAndIsSelected", "the other half of the same rule, owed by the LEDGERS: E26's gate found a ledger naming a DELETED test that had stood for two tasks"},
		{"TestEveryIrreversibleActionInTheConsoleIsBehindAnAlertDialog", "FLC-004 — the claim neither T2's spec nor T3's could make, because it is a property of the SET rather than of the actions a spec named"},
		{"TestTheActionSweepRefusesBothDirections", "FLC-004 — and it refuses the direction a one-way check misses: a REVERSIBLE action promoted into a dialog, which is the change the next reader makes for consistency"},
		{"TestEveryCeilingTheBundleClaimsIsInThePageSource", "group (g) re-derived from the page sources: the gap id is what makes a ceiling countable"},
		{"TestTheCeilingSweepFailsWhenAPageStopsSayingIt", "the mutation that makes the line above load-bearing"},
		{"TestTheRouteLedgerIsTheRouteTable", "group (e) re-derived from lib/routes.ts, in both directions"},
		{"TestAShrunkenApproverListIsRefused", "the refusal matrix's sharpest row: an empty approver list PERMITS EVERY PRINCIPAL, so the most innocent button an admin console can carry would have opened the approval gate to everyone"},
		{"TestAPartialPolicyRequestIsRefusedEvenWhenTheOutcomeSurvived", "the half a stored-outcome assertion cannot see — a server that MERGED would pass the outcome alone"},
		{"TestAPoolLedgerWithNoUnsandboxedHostPostureIsRefused", "a pool COUNT alone is satisfied by the seed; the posture SET is the birth path"},
		{"TestAnAdmissionFromTheCLIAloneIsRefused", "an epic whose crown claim is a screen cannot certify it from a CLI transcript"},
		{"TestThePromoteGateRefusesStable", "no tier advances, mechanically"},
		{"TestADroppedClaimMarkerStillRoutesHere", "the promote-gate-family-dispatch guard: the family is recognized by the CASE IDS, never by the claim the gate enforces"},
		{"TestTheE2xFamiliesAreDisjoint", "and the premise that guard rests on, asserted rather than assumed"},
		{"TestCommittedFleetConsoleBundleIsTheGeneratorOutput", "the bundle is BIT-EQUAL to its generator's output"},
		{"TestFleetConsoleReleaseVerifiesClean", "0 failed / 0 missing / 0 secret through the SHIPPED verifier"},
		{"TestTheBundleClaimsNoRentedMacAndNoDeployedConsole", "the absence guard, structural rather than a byte scan — a scan for \"Mac\" would fail on this bundle's honest paragraphs explaining there was not one"},
		{"TestNoTierAdvancesInThisRelease", "`console` closes PREVIEW, asserted against the SHIPPED capability table and the claims digest rather than against a sentence"},
	} {
		if !strings.Contains(run, "--- PASS: "+leg.test) {
			t.Errorf("%s did not report PASS (%s) — the co-run of the suite that BACKS its case did not happen, so the bundle's authored PASS for it is unbacked", leg.test, leg.why)
		}
	}

	// --- the BROWSER legs, diffed against Playwright's PASS mark -------------------------------------------
	//
	// A DIFFERENT MARK, AND THE REASON IS IN THE SCRIPT'S REPORTER OVERRIDE. `--reporter=list` prints `✓` on
	// the line that COMPLETES; the config's default `line` reporter prints a title when a test STARTS, which
	// would let a title in the output vouch for a test that hung or failed.
	for _, leg := range []struct{ title, why string }{
		{"a pool is created from the console with the posture it was given", "FLC-003 / group (a) — the console half of the birth path, and it prints the POOL CREATED ledger row this gate diffs below"},
		{"an existing pool's waiting room is switched on from the console", "FLC-003 — FLT-P12's switch, from a screen"},
		{"a machine waiting in a strict pool is on this page and is admitted from it", "FLC-003 / group (b) — the RED this whole epic was written against: a machine was waiting where nothing could see it, and /fleet answered 404"},
		{"a pool key is shown once, and nothing reads it back", "FLC-003 — the server's `poolKeyView(item, false)` mirrored in the browser"},
		{"revoking a pool key shows the machines it already admitted and does not stop", "FLC-004 — the review half of SC 3.3.4 leg 3 for a key: revoking stops the NEXT machine, not the last"},
		{"cordon goes through the native confirmation and revoke does not", "FLC-004 — the split, on the page where both live"},
		{"the revoke dialog cannot open without the single read that carries the lease count", "FLC-004 — \"reviewing\" is a DATA CALL: ActiveLeases exists only on the single read, so the dialog asserts the network call rather than the rendered text"},
		{"the review names the machine's label and the count the gateway gave for it", "FLC-004 — and telling an operator a decision is irreversible without telling them what it costs is worse than not asking"},
		{"a machine whose lease count could not be read says so rather than showing a zero", "FLC-003 — the *int64 distinction kept ON SCREEN: could-not-ask and none-waiting are different answers"},
		{"the page writes the three things a fleet screen must not let an operator assume", "group (g) — FLT-P15 and FLT-P12 on screen, which is the only mitigation available for a ceiling this epic cannot close"},
		{"a pending machine is not on /approvals, and /approvals says which screen holds it", "FLC-003 — the two screens name each other, because a machine enrolment has no request_hash to bind to"},
		{"setting only the pool from the console leaves the approver list intact", "FLC-002 / group (d) — the security row, and it prints the POLICY WRITE ledger line this gate diffs below"},
		{"the request body carries all five policy fields, not the one that changed", "FLC-002 — the WIRE, because the outcome alone passes on a server that merged"},
		{"an empty approver list is shown as permissive, in words, before it is saved", "group (g) — HIL-P11 where the operator can act on it"},
		{"the revoke dialog is an alertdialog that reviews what is about to die", "FLC-002 / FLC-004 — APG's four conditions, on the policy page"},
		{"Tab cannot leave the open dialog, and Escape cancels it", "FLC-004 — the focus trap a hand-rolled dialog has to re-earn, measured as DOM focus order (see this file's ceiling: that is not what an assistive technology announces)"},
		{"window.confirm is still the confirmation for the REVERSIBLE actions", "FLC-004 — the direction that stops the next reader converting them all"},
		{"a minted key survives nowhere: not the DOM, not either storage, not the URL, not a later response", "FLC-002 / group (c) — and it prints the five KEY VALUE SCAN ledger rows this gate diffs below"},
		{"the value is readable and copyable by hand in a context with no clipboard API", "§3.5 W3/W4 — the carrying fallback, because an operator on http://<host>:3000 has no clipboard API and 40 characters is a transcription task"},
		{"a refused clipboard write is shown, not swallowed", "§3.5 W3 — NotAllowedError is not swallowed"},
		{"every route lib/routes.ts declares was actually scanned by axe", "group (e) — an unscanned page is structurally impossible, in both colour schemes"},
	} {
		if !strings.Contains(run, "› "+leg.title) {
			t.Errorf("the spec %q never ran (%s) — Playwright collects new spec files automatically, so a title absent from this output is a test that did not execute rather than one that was not written", leg.title, leg.why)
		}
		if !strings.Contains(run, "✓") {
			t.Fatal("the run printed no Playwright PASS mark at all — the reporter override is gone, and `line` prints a title when a test STARTS, which would make every assertion above vacuous")
		}
	}

	// --- the LEDGER numbers, diffed against what the run PRINTED -------------------------------------------
	//
	// This is the half that stops the bundle's counters being authored fiction. Each canonical ledger is swept
	// with the same function the gate uses, and the result is required to appear in THIS run's output.

	// (a) the pool birth path. The pool NAME is timestamped by the spec, so what is diffed is the posture and
	// the surface — a name pin would be a clock pin.
	if !strings.Contains(run, "POOL CREATED — ") || !strings.Contains(run, "posture=unsandboxed-host strict=true via=console") {
		t.Error("the run never printed a pool created from the console with posture `unsandboxed-host` and strict enrolment on — that is the value NO code path in this tree could write before T1, and the bundle's pool ledger claims it")
	}

	// (b) the waiting room, and `from=console` by value.
	if !strings.Contains(run, "WAITING ROOM — ") || !strings.Contains(run, "pending=true admitted from=console") {
		t.Error("the run never printed a machine admitted FROM THE CONSOLE — E24 T6 already proved the approve route from a component test, so a bundle whose only admission came from elsewhere certifies E24 again rather than E28")
	}

	// (c) the five key-scan sites. The BYTE counts are deliberately NOT diffed: a DOM's size is a property of
	// the build, and pinning it would make this gate a version pin that reddens on a dependency bump for a
	// reason that is not a security fact. What is diffed is the site, the ZERO and the probe.
	var scan []struct {
		Site  string `json:"site"`
		Probe string `json:"probe"`
	}
	if err := json.Unmarshal([]byte(uat.FleetConsoleKeyScanLedger), &scan); err != nil {
		t.Fatalf("decode the canonical key-scan ledger: %v", err)
	}
	for _, row := range scan {
		want := fmt.Sprintf("KEY VALUE SCAN — site=%s", row.Site)
		if !strings.Contains(run, want) {
			t.Errorf("the run never scanned the %q site — the bundle's key-scan ledger carries a row for it, and a site with no row in the RUN is a zero nobody produced", row.Site)
		}
	}
	// THE ZERO IS READ OFF THE `KEY VALUE SCAN` LINES AND NOTHING ELSE, and that scoping is not tidiness —
	// this assertion's first draft searched the WHOLE run for "hits=1" and went red on E25's sentinel sweep,
	// whose own line reports `probe=SWEEP_TOKEN hits=2` for a probe it was SUPPOSED to find. A substring
	// assertion over an unrelated haystack is the defect this repository files under "path and membership
	// comparisons are guilty"; here it fired in the safe direction, which is the only reason it was cheap.
	scanned := 0
	for _, line := range strings.Split(run, "\n") {
		idx := strings.Index(line, "KEY VALUE SCAN — ")
		if idx < 0 {
			continue
		}
		row := line[idx:]
		scanned++
		if !strings.Contains(row, " hits=0 ") {
			t.Errorf("a key-value scan reported a non-zero count: %s — the minted value must be found in NO site", row)
		}
		if !strings.Contains(row, "probe_found=true") {
			t.Errorf("a key-value scan reported no probe it found: %s — a haystack nothing was ever located in is a haystack nobody has shown was read", row)
		}
	}
	if scanned < len(uat.FleetConsoleKeyScanSites) {
		t.Errorf("the run printed %d key-value scan row(s) and the bundle's ledger carries %d sites — a site with no row in the RUN is a zero nobody produced", scanned, len(uat.FleetConsoleKeyScanSites))
	}

	// (d) the approver equality, from the run rather than from the manifest.
	before, after, _, err := uat.SweepFleetConsolePolicyWrites(json.RawMessage(uat.FleetConsolePolicyLedger))
	if err != nil {
		t.Fatalf("the canonical policy ledger cannot be swept: %v", err)
	}
	if before != after {
		t.Fatalf("the canonical policy ledger is itself unequal (%d in, %d out) — the bundle would not have been writable", before, after)
	}
	// Two rows in the ledger, two writes in the run, and the whole suite runs in BOTH colour schemes.
	if !strings.Contains(run, "POLICY WRITE — changed=pool approvers_before=1 approvers_after=1") {
		t.Error("the run never printed a policy write whose approver count came out equal to what it went in with — HIL-P11 measured that an empty list permits every principal, so this equality is the security claim")
	}
	if !strings.Contains(run, fmt.Sprintf("fields_in_request=%d", uat.FleetConsolePolicyFields)) {
		t.Errorf("the run never printed a policy request carrying all %d fields — a server that MERGED would let the outcome assertion above pass over a form still sending one", uat.FleetConsolePolicyFields)
	}

	// (e) the axe coverage, once per colour scheme.
	declared, scanned, _, err := uat.SweepFleetConsoleRoutes(json.RawMessage(uat.FleetConsoleRouteLedger))
	if err != nil {
		t.Fatalf("the canonical route ledger cannot be swept: %v", err)
	}
	wantAxe := fmt.Sprintf("AXE ROUTE COVERAGE — %d/%d declared route(s) scanned", scanned, declared)
	if coverage := strings.Count(run, wantAxe); coverage < len(uat.AdminConsoleColourSchemes) {
		t.Errorf("the run printed %q %d time(s), want at least %d — one per colour scheme, and a single occurrence means the second Playwright project did not run", wantAxe, coverage, len(uat.AdminConsoleColourSchemes))
	}

	// AND THE LEGS THAT MUST NOT BE THERE. This release claims no rented Mac and no remote execution;
	// asserting their ABSENCE is what stops a later reader from adding a name here that matches something
	// weaker and calling either one proven.
	for _, absent := range []string{"TestARealMacEnrols", "TestXcodebuildRunsOnTheMac", "TestTheExecutionRelay", "TestADeployedConsole"} {
		if strings.Contains(run, "--- PASS: "+absent) {
			t.Errorf("%s reported PASS — this release claims NO rented Mac, NO remote execution and NO deployed console, so a green leg with this name is either a test of something else wearing that name or the feature landed without this gate being updated", absent)
		}
	}
}

// TestARedBackingSuiteFailsTheFleetConsoleGate is the load-bearing negative: the co-run is only worth anything
// if a RED backing suite actually fails this target. It re-runs the operator entry point with
// PALAI_FLEET_CONSOLE_FAULT_SUITE pointing the execution suite (which holds T1's whole birth path) at a DEAD
// Postgres — a genuinely red suite, its tests failing on connect, not a stubbed-out command — and asserts a
// NON-ZERO exit.
//
// Without this, "the gate co-runs the suites" would be a claim about the script's TEXT rather than its
// behaviour: a swallowed status, a missing `set -e`, or a `|| true` would leave the gate green over a red
// suite, which is exactly the shape of hole this family keeps finding — and which E21 T7 found in its own gate
// script, twice, as green-by-skip.
func TestARedBackingSuiteFailsTheFleetConsoleGate(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available; the fault run needs the throwaway Postgres the script stands up")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "scripts/uat/fleet-console")
	cmd.Dir = repoRoot(t)
	cmd.Env = append(cmd.Environ(), "SKIP_JOURNEYS=0",
		"PALAI_FLEET_CONSOLE_FAULT_SUITE=./apps/control-plane/internal/execution")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the gate PASSED with the execution backing suite pointed at a dead Postgres — a red backing suite must fail this target, or the co-run certifies nothing\n%s", out)
	}
	if !strings.Contains(string(out), "FAULT INJECTED") {
		t.Errorf("the run does not report the injected fault, so the non-zero exit may be unrelated:\n%s", out)
	}
	if !strings.Contains(string(out), "backing suite ./apps/control-plane/internal/execution FAILED") {
		t.Errorf("the failure is not attributed to the faulted backing suite:\n%s", out)
	}
	t.Logf("a red backing suite failed the gate as required: %v", err)
}
