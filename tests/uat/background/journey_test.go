//go:build uat

// The E26 T7 EXIT-gate journey entry point. Like the E15 T6 / E16 T8 / E17 T11 / E19 T9 / E20 T5 / E21 T7 /
// E22 T7 / E23 T7 / E24 T8 / E25 T9 gates before it, this file is an ORCHESTRATOR rather than a
// reimplementation: it drives `scripts/uat/background`, which stands up a throwaway Postgres and a real Docker
// daemon and runs the narrative where its seams already live.
//
// THE NARRATIVE IS THE CO-RUN RATHER THAN A TWENTY-FOURTH TEST, and what this file adds is the thing a co-run
// cannot give itself: the assertion that every leg RAN. The bundle's per-case status and its six ledgers are
// authored data; a `-run` pointed at the wrong package matches nothing at all and exits 0 in silence.
//
// AND THE ASSERTION IS THE STRONG FORM OF EVERY CLAIM, WHICH IS THIS FILE'S WHOLE REASON FOR EXISTING. The
// weak form of this epic's headline — "it ran in the background" — is TRUE of a build that parks the run and
// waits for the process, because that run would resume later and a second tool would eventually execute. So
// the leg named below is not "a background task ran"; it is
// TestTheModelCallsAnotherToolWhileTheBackgroundProcessIsStillRunning, whose own body carries the park assert
// (the spawning dispatch returned nil, the run reads `running`, one tool.result reached the model) AND the
// second-tool assert (a different tool call in the SAME attempt completed and wrote a real file, with the
// kernel confirming the process alive on BOTH sides of it). A leg list that named the weaker test would pass
// on a build that has none of this.
//
// HONEST CEILING (plan §6): every process here is a REAL process and every container a REAL container — but on
// ONE box, in ONE process tree, on darwin/arm64. The "second control plane" is a second store handle and
// orchestrator over the same database rather than a process that actually died, the "restart" is a plane
// constructed afterwards, and there is no xcodebuild anywhere. The ordering below reads as one story on
// purpose while being several independent suites in fact — the story is how the epic is judged, the suites are
// what is measured, and this comment is where the difference is admitted rather than blurred.
package background

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestBackgroundJourneyRunsThroughTheOperatorEntryPoint runs the §T7 narrative through the shipped target. It
// is Docker-bound (a throwaway Postgres and a real daemon), so it rides the `uat` tag and never `make verify`.
// It needs NO credential: every leg is deterministic against real processes, real containers and real
// PostgreSQL.
func TestBackgroundJourneyRunsThroughTheOperatorEntryPoint(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available; the E26 backing suites need a throwaway Postgres and a real daemon")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "scripts/uat/background")
	cmd.Dir = repoRoot(t)
	cmd.Env = append(cmd.Environ(), "SKIP_JOURNEYS=0")
	out, err := cmd.CombinedOutput()
	t.Logf("$ scripts/uat/background\n%s", out)
	if err != nil {
		t.Fatalf("the E26 background gate failed: %v", err)
	}

	// EACH LINE IS ONE LEG OF THE NARRATIVE, and each is named because it is the leg that would rot first if
	// the `-run` selector drifted. Read the diff, never the selector.
	for _, leg := range []struct{ test, why string }{
		// --- T1: ownership moves from the call to the run, in both postures ---
		{"TestTodayAHostProcessGroupDiesWithTheAttempt", "BGT-001 — the OLD rule, measured as it stands: a synchronous host command's whole process GROUP dies with the attempt's context. It stays in the suite because the synchronous posture must keep doing exactly that"},
		{"TestTodayAShellContainerIsGoneWhenTheCallReturns", "BGT-001 — the same measurement in the container posture, and for a completely different reason: an unconditional defer-remove that runs on the error path too"},
		{"TestAHostBackgroundProcessSurvivesTheAttempt", "BGT-001 — the process group is alive after Start returned AND after the attempt's context was cancelled, read from the kernel with kill(-pgid, 0) against a pgid the process itself wrote down. This is also §3.5 P10's measurement, because the os/exec page would not load"},
		{"TestADetachedContainerIsStillRunningAfterTheCallReturns", "BGT-001 — the container posture's half, read from ContainerInspect against a real daemon"},
		{"TestTheDetachedPathBuildsNoContainerOptionsOfItsOwn", "BGT-001 — the hardening is proven IDENTICAL structurally by go/ast rather than by reading it: the detached path hands ContainerCreate the result of createOptions and constructs nothing of its own, which is a stronger claim than the fields currently matching"},
		{"TestDockerDriverRunIsByteUnchanged", "BGT-001 — the synchronous driver did not change by one byte while a new lifetime was added beside it, pinned by the sha256 of its exact source bytes against a COMMITTED word rather than a baseline the guard recomputes for itself"},
		{"TestAPgidWeCannotProveIsOursIsNeverSignalled", "BGT-001 — the sharpest ABSENCE in this release: a real process started outside the program, a fabricated start time, and that process still running afterwards"},
		{"TestADetachedContainerWeCannotProveIsOursIsNeverSignalled", "BGT-001 — the same rule in the container posture, enforced with the evidence it has: no io.palai.bg label, no signal, still running"},
		{"TestAnEscapingOutputPathIsRefusedBeforeAnythingStarts", "BGT-001 — path containment on the spawn, WITH the non-vacuity half this gate added after measuring that an executor refusing every input satisfied it"},
		{"TestTheDetachedCommandFormInterpretsNoMetacharacter", "BGT-001 — the container writes its own log through a positional-parameter form, so a semicolon, a redirect, a $(...) and an && inside one argument all arrive as characters"},

		// --- T2: the tool surface, and the defining property ---
		{"TestABackgroundShellCallReturnsAHandleWhileTheProcessIsStillRunning", "BGT-002 — §2.1: the call returns a handle and no output, measured as a SEQUENCE rather than a stopwatch"},
		{"TestABackgroundTasksOutputIsReadableMidFlight", "BGT-002 — §2.2: two consecutive reads of a still-running task's log return a RISING line count, through the file tool that already reads files. No fourth output mechanism, which is the vendor's own conclusion shipped as a deprecation"},
		{"TestTheModelCallsAnotherToolWhileTheBackgroundProcessIsStillRunning", "BGT-002 — §2.6, THE DEFINING PROPERTY AND THE WHOLE REASON THIS LIST NAMES A TEST RATHER THAN A BEHAVIOUR: the spawning dispatch returned nil, the run reads `running`, and then a DIFFERENT tool call in the SAME attempt completed and wrote a real file while the kernel still listed the background process — asserted on BOTH sides of the second call. A park-and-wait would satisfy any weaker phrasing of this"},
		{"TestThreeBackgroundTasksRunConcurrentlyWithoutInterleavingTheirOutput", "BGT-002 — §2.5: three process groups alive at the same instant, three logs, each containing its own marker and NEITHER of the other two"},
		{"TestAGatedToolCalledWithBackgroundSpawnsNothingUntilAHumanDecides", "BGT-002 — the approval gate is a structural ORDERING claim, and it carries its own non-vacuity half because without it the assertion PASSED at the fork point: the identical call with the gate OFF must read ONE"},
		{"TestBackgroundIsRefusedRatherThanDowngradedWhenTheFeatureIsDisabled", "BGT-002 — the kill switch REFUSES rather than silently running the command in the foreground, counted on the SYNCHRONOUS runner too, because a silent downgrade is a model believing it backgrounded something that is in fact blocking"},
		{"TestABackgroundedDirectoryChangeDoesNotCarryOverToTheNextCommand", "BGT-002 — §3.5 P6, structurally true in Palai by accident and turned into a DECISION by a test"},
		{"TestBackgroundKillStopsTheProcessGroupAndKillingTwiceIsKillingOnce", "BGT-002 — §2.4 and P7's \"cancelling twice is idempotent\", measured from the kernel"},
		{"TestAShellCallWithoutBackgroundIsFieldForFieldUnchanged", "BGT-002 — the §2 invariant this whole epic is negotiated against, pinned against a literal recorded at the fork point and covering both what the model receives and what the ledger row stores"},

		// --- T3: the park, and a response state nothing had ever written ---
		{"TestARunWithALiveBackgroundTaskParksInsteadOfCompleting", "BGT-003 — a run whose model sent run.terminal{completed} while a task it started was still running PARKS, governed by the operating system's answer rather than by a row"},
		{"TestARunWithNoLiveBackgroundTaskStillCompletes", "BGT-003 — and the other direction, because a park that fires too often is the same defect as one that never fires"},
		{"TestAFailedTerminalDoesNotParkOnALiveBackgroundTask", "BGT-003 — a failure is an ANSWER, and holding it back to wait for a build would turn an error into a hang"},
		{"TestALostOrUnprobeableBackgroundTaskDoesNotParkTheRun", "BGT-003 — an unprovable handle already receives no signal; parking on one would strand a run behind a process nobody can prove exists"},
		{"TestAParkedRunsResponseReadsWaitingForTool", "BGT-003 — E26 is the FIRST production caller of a state machine this repository shipped with its spec: ResponseTable had none, and a published schema had advertised waiting_for_tool for three years"},
		{"TestALiveResponseReachesInProgressBeforeItParks", "BGT-003 — and the plan's D3 was wrong twice over: a live response read `queued`, not `in_progress`, so waiting_for_tool was unreachable THROUGH a state nothing wrote. Two writes were needed, not one"},
		{"TestWaitingForToolNeverOverwritesATerminalResponse", "BGT-003 — monotonicity, honestly green at the fork point because nothing wrote the state at all, so its non-vacuity is demonstrated against the FIX rather than claimed at the fork"},
		{"TestTheEventStreamStaysOpenAcrossAParkAndClosesOnATerminal", "BGT-003 — the SSE stream survives the park, which the console's own comment already asserted for waiting_for_approval and which is now a test rather than a comment"},

		// --- T4: the wake ---
		{"TestAParkedRunReentersExactlyOnceWhenItsBackgroundTaskFinishes", "BGT-004 — §2.3: the parked run re-enters and the turn the model sees carries the task id, the exit code, the output path and a bounded tail. The wake is CALLED rather than written — E23 T1's choreography's third user"},
		{"TestTwoTicksTwoPlanesAndARestartProduceOneBackgroundNotice", "BGT-004 — exactly-once is a ROW and not a mutex, and the scenario is built so a mutex could not pass it: two ticks, two control planes swept concurrently, and a plane that never saw the spawn"},
		{"TestARunningRunIsNotInterruptedAndTheNoticeFoldsAtTheNextBoundary", "BGT-004 — a wake fired at a run that is still working is a SECOND attempt of one run; the command is enqueued either way and the wake happens only if the run is waiting, both in ONE transaction"},
		{"TestATerminalRunsBackgroundNoticeIsStampedRatherThanDropped", "BGT-004 — the row a tidy build would have dropped: queued it sits unread, dropped it is the silent loss every line of known-gaps refuses, so it is STAMPED and an operator sees it"},
		{"TestWithoutTheReconcilerSweepNoBackgroundNoticeEverLands", "BGT-004 — D14 pinned as a DECLARATION rather than a surprise, and the runbook's first paragraph says it"},
		{"TestNoDispatchWorkerMeansNoBackgroundExitNotificationEverLands", "BGT-004 — the same declaration at the COMPOSITION ROOT: production's own reading of PALAI_DISPATCH_WORKERS and the shipped compose file's `:-0` have to agree, or an operator's build finishes and nothing ever tells the model"},

		// --- T5: the reaper ---
		{"TestATaskPastItsDeadlineIsKilledMarkedExpiredAndTheModelIsTold", "BGT-005 — the wall clock is a COLUMN and its enforcer is the reaper, because a context does not survive the restart a background task exists to survive"},
		{"TestUnsetMeansSixtyMinutesAndUnboundedMustBeWrittenExplicitly", "BGT-005 — the exact inverse of E24 T5's PALAI_FLEET_PARK_TTL, and for a stated reason: there was no honest default for how long a rented Mac takes to arrive, and there is one for how long a build may hold a machine"},
		{"TestCancellingARunKillsEveryLiveBackgroundTaskOfIt", "BGT-005 — which it did NOWHERE before this epic: CancelRunReconciled drove the run to canceled while the processes kept compiling. The proof is the kernel's answer about pgids the processes wrote down"},
		{"TestARestartedControlPlaneAdoptsItsRunningTasksFromTheRowRatherThanFromMemory", "BGT-005 — ADOPTION SHIPPED, which is why §4.2's \"T5b was skipped\" sentence is not owed and is not written. What adoption cannot do is cross a MACHINE"},
		{"TestADeadTaskNoWatcherEverSawGetsNoInventedExitCode", "BGT-005 — exit_code stays NULL when unknown, with its own non-vacuity half in the same function because a column NOTHING writes also reads NULL"},
		{"TestAnUnprovableHandleBecomesLostAndReceivesNoSignal", "BGT-005 — refused with a TYPED error rather than a lookup miss, since any-error-will-do would have called a plane that lost the handle a proof about signalling"},
		{"TestTheSixthBackgroundTaskOfARunIsRefusedRatherThanQueued", "BGT-005 — concurrency ENFORCED and not declared: E24 opened runners.capacity under a CHECK and no Go expression ever read it. The counts come from the DATABASE and the PROCESS TABLE"},
		{"TestTheMachineCeilingIsCountedAcrossTenantsFromTheDatabase", "BGT-005 — across tenants deliberately: a per-tenant twenty would give one Mac as many concurrent builds as there are tenants"},
		{"TestAFinishedTasksLogIsDeletedAfterItsTTLAndARunningTasksIsNot", "BGT-005 — without it .palai-session grows unbounded and, because Snapshot skips the whole subtree, NOBODY WOULD NOTICE"},
		{"TestKillingATaskThatAlreadyFinishedClaimsNothing", "BGT-005 — found by REVIEW rather than by a RED: recording `killed` for a build that exited on its own puts a false statement in a row an operator reads"},
		{"TestKillingADetachedTaskRemovesTheContainerAndIsIdempotent", "BGT-005 — the container half of the same question on a real daemon, where the suite's own before/after container count is the assertion"},

		// --- T6: the credential ---
		{"TestAnEnvironmentValueOutlivesTheExecuteCallThatHandedItOver", "BGT-002/T6 — the measurement that made a WRITTEN CLAIM in this tree false: E25 T3's \"the value's whole life is one Execute call\" is structurally wrong for a detached task, and the comment is corrected rather than left to rot"},
		{"TestBothTheFileReadAndTheExitNoticeAreRedactedFromOnePlace", "BGT-002/T6 — the two landing sites T2 and T4 each recorded as OPEN where a reader meets them, closed from ONE function, because two redaction points are two chances to diverge"},
		{"TestNoDecodedFieldOfATaskRowItsEventsOrItsCommandsCarriesAnEnvironmentValue", "BGT-002/T6 — measured by DECODING every column and re-walking anything that parses as JSON or base64, never a raw substring scan, which over an encoded field can never fail"},
		{"TestACredentialCarryingTaskCannotBeCreatedWithoutADeadline", "BGT-002/T6 — §0.4's price, enforced as a CODE GATE rather than a CHECK, because unbounded stays legitimate for a task carrying nothing"},
		{"TestARunParkedForApprovalOnABackgroundCallResolvedNoEnvironmentValue", "BGT-002/T6 — the background path did not move resolveEnvValues, so a run parked for a human still holds nothing"},

		// --- the gate's own guards ---
		{"TestEveryE26ExportedSurfaceHasAProductionCaller", "the job a gate exists for — and it found the sixth and seventh instances of \"built, tested, reachable from nothing\", both deleted rather than filed"},
		{"TestEveryE26ComponentTestIsNamedInTheRunAllowList", "D20: a guard this `-run` does not name is a guard this tier never runs"},
		{"TestEveryTestNamedInABundleLedgerExistsAndIsSelected", "the other half of the same rule, owed by the LEDGERS: BGT-002 named a deleted test for two tasks"},
		{"TestDroppingTheDefiningSemanticIsRefused", "the refusal matrix's sharpest row: a bundle that drops §2.6 while lowering its own count honestly is still refused"},
		{"TestThePromoteGateRefusesStable", "no tier advances, mechanically"},
		{"TestADroppedBackgroundClaimStillRoutesHere", "the promote-gate-family-dispatch guard: the family is recognized by the CASE IDS, never by the claim the gate enforces"},
	} {
		if !strings.Contains(string(out), "--- PASS: "+leg.test) {
			t.Errorf("%s did not report PASS (%s) — the co-run of the suite that BACKS its case did not happen, so the bundle's authored PASS for it is unbacked", leg.test, leg.why)
		}
	}

	// AND THE LEGS THAT MUST NOT BE THERE. This release claims no live progress stream and no cross-machine
	// survival; asserting their ABSENCE is what stops a later reader from adding a name here that matches
	// something weaker and calling either one proven.
	for _, absent := range []string{"TestLiveProgress", "TestTaskUpdateChunks", "TestABackgroundTaskSurvivesAControlPlaneMove", "TestBackgroundRelay"} {
		if strings.Contains(string(out), "--- PASS: "+absent) {
			t.Errorf("%s reported PASS — this release claims NO live progress stream and NO cross-machine survival, so a green leg with this name is either a test of something else wearing that name or the feature landed without this gate being updated", absent)
		}
	}
}

// TestARedBackingSuiteFailsTheBackgroundGate is the load-bearing negative: the co-run is only worth anything if
// a RED backing suite actually fails this target. It re-runs the operator entry point with
// PALAI_BACKGROUND_FAULT_SUITE pointing the execution suite (which holds most of the legs) at a DEAD Postgres —
// a genuinely red suite, its tests failing on connect, not a stubbed-out command — and asserts a NON-ZERO exit.
//
// Without this, "the gate co-runs the suites" would be a claim about the script's TEXT rather than its
// behaviour: a swallowed status, a missing `set -e`, or a `|| true` would leave the gate green over a red
// suite, which is exactly the shape of hole this family keeps finding — and which E21 T7 found in its own gate
// script, twice, as green-by-skip.
func TestARedBackingSuiteFailsTheBackgroundGate(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available; the fault run needs the throwaway Postgres the script stands up")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "scripts/uat/background")
	cmd.Dir = repoRoot(t)
	cmd.Env = append(cmd.Environ(), "SKIP_JOURNEYS=0",
		"PALAI_BACKGROUND_FAULT_SUITE=./apps/control-plane/internal/execution")
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
