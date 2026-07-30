//go:build uat

// The E24 T8 EXIT-gate journey entry point. Like the E15 T6 / E16 T8 / E17 T11 / E19 T9 / E20 T5 / E21 T7 /
// E22 T7 / E23 T7 gates before it, this file is an ORCHESTRATOR rather than a reimplementation: it drives
// `scripts/uat/fleet`, which stands up a throwaway Postgres and runs the narrative where its seams already
// live —
//
//	two machines are two rows, ids minted by the server   internal/fleet      TestRegistryRecordsEachMachineOnceWithItsOwnJournalEntry
//	...and an upgraded install's runner can still enrol    internal/execution  TestBootstrapInstallEnrollsItsRunnerIntoTheDefaultPool
//	neither tenant can read the other's machines           internal/fleet      TestReadsAreConfinedToTheCallersTenant
//	the Mac key is REFUSED into the Linux pool             internal/execution  TestPoolKeyEnrolsOnlyIntoThePoolItNames
//	a run lands ONLY on its pool's machine                 internal/execution  TestAnUnsandboxedHostAttemptIsNeverOfferedToASandboxedLinuxRunner
//	...and with no pool configured, nothing changed         internal/execution  TestASingleRunnerDeploymentWithNoPoolConfigurationIsBitUnchanged
//	tenant B is never offered tenant A's machine           internal/execution  TestPlacementNeverOffersOneTenantsAttemptToAnothersRunner
//	the pool is EMPTIED and the run PARKS, not dies        internal/execution  TestPlacementParksARunWhosePoolHasNoRunner
//	the machine returns and the run WAKES and runs         internal/execution  TestPlacementWakesAParkedRunWhenAMachineJoinsItsPool
//	...and every other waiting run is left alone           internal/execution  TestPlacementWakeLeavesEveryOtherWaitingRunAlone
//	the key is REVOKED: a new enrolment fails              internal/execution  TestPoolKeyRevocationRefusesANewEnrolment
//	...the enrolled machine goes on RENEWING               internal/execution  TestPoolKeyRevocationLeavesAnEnrolledRunnerRenewing
//	...and its in-flight lease is not cut                  internal/execution  TestPoolKeyRevocationDoesNotCutAnInFlightLease
//	the key value reaches no row, journal or log           internal/execution  TestPoolKeyValueReachesNoRowJournalOrLog
//	ONE machine is cordoned, the OTHER keeps serving       internal/execution  TestLifecycleCordonSurvivesAControlPlaneRestartAndResumeClearsIt
//	the revoked machine cannot reconnect after a RESTART   internal/execution  TestLifecycleRevocationSurvivesAControlPlaneRestart
//	...and revoking is irreversible and journalled once    internal/execution  TestLifecycleRevokeIsIrreversibleAndJournalled
//	a waiting machine can still make its renewal           internal/execution  TestStrictEnrolmentHoldsTheMachineOutOfTheRendezvousUntilAHumanApproves
//	...and with strict off, the default, nothing changed    internal/execution  TestStrictOffIsBitUnchanged
//
// THE NARRATIVE IS THE CO-RUN RATHER THAN A TWENTIETH TEST, and that is a deliberate choice with a reason:
// each leg above drives the SHIPPED path at the seam that owns it, against real PostgreSQL over a real mTLS
// wire, and a journey that re-implemented any of them would be asserting its own copy — exactly the shape of
// proof this family exists to refuse. What this file adds is the thing a co-run cannot give itself: the
// assertion that every leg RAN.
//
// AND THE LEG THAT IS NOT HERE. The plan's journey ended with "a shell call executing on the runner's
// machine". E24 T7 — the execution relay — WAS DEFERRED BY THE OWNER, so that leg is DROPPED rather than
// approximated. There is nothing in this tree that could produce it: every tool still executes in the control
// plane's own process, which means a Mac is only a Mac when the control plane is ON it. A leg asserting
// otherwise would be the one dishonest line in an otherwise measured gate.
//
// HONEST CEILING (plan §6): every machine is a FAKE runner built from the SHIPPED enrollment package against
// the SHIPPED gateway — but on ONE box, in ONE process tree, on darwin/arm64. No second physical machine is
// dialled, no `linux/amd64` host exists, and the "control-plane restart" is a second gateway on the same CA
// and database rather than a process that actually died. The ordering above reads as one story on purpose
// while being several independent suites in fact — the story is how the epic is judged, the suites are what is
// measured, and this comment is where the difference is admitted rather than blurred.
package fleet

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestFleetJourneyRunsThroughTheOperatorEntryPoint runs the §T8 narrative through the shipped target. It is
// Docker-bound (a throwaway Postgres), so it rides the `uat` tag and never `make verify`. It needs NO
// credential: every leg is deterministic against real PostgreSQL and a real mTLS wire.
func TestFleetJourneyRunsThroughTheOperatorEntryPoint(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available; the E24 backing suites need a throwaway Postgres")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "scripts/uat/fleet")
	cmd.Dir = repoRoot(t)
	cmd.Env = append(cmd.Environ(), "SKIP_JOURNEYS=0")
	out, err := cmd.CombinedOutput()
	t.Logf("$ scripts/uat/fleet\n%s", out)
	if err != nil {
		t.Fatalf("the E24 fleet gate failed: %v", err)
	}

	// EACH LINE IS ONE LEG OF THE NARRATIVE, and each is named because it is the leg that would rot first if
	// the `-run` selector drifted. The bundle's per-case status and its five ledgers are authored data, so
	// what makes a PASS for FLT-001..005 honest is that the suite backing it ran in THIS invocation — and a
	// `-run` pointed at the wrong package matches nothing at all and exits 0 in silence, which is how this
	// repository has shipped green-by-omission more than a dozen times, TWICE inside this very epic's T1.
	//
	// AND THIS LIST CAUGHT THE FIFTH INSTANCE, which is the only reason five of its entries exist. The first
	// draft of scripts/uat/fleet copied the component tier's selector verbatim and looked complete. Diffing
	// this list against the run's actual `--- PASS` lines showed that `runner_pool_test.go`'s five tests
	// matched NONE of it — the file is UNTAGGED so they ride `make verify` and were never missing from the
	// tree, but no allow-list in the repository selected them, because not one of their names starts with a
	// word already on the line (`TestPoolKey` does not match `TestAPoolServesItsWaitingRunsOldestFirst`).
	// The two §2 invariant legs among them — a wrong-pool machine never offered an attempt, and a
	// no-pool-configured deployment being bit-unchanged — were being NAMED here as backing FLT-002 by an
	// invocation that did not run them. Read the diff, never the selector.
	for _, leg := range []struct{ test, why string }{
		{"TestRegistryRecordsEachMachineOnceWithItsOwnJournalEntry", "FLT-001 — two machines are two rows with two SERVER-minted ids, each with its own append-only journal entry committed in the same transaction"},
		{"TestBootstrapInstallEnrollsItsRunnerIntoTheDefaultPool", "FLT-001 — and an install that predates the registry can still enrol its own runner, which is what stops 000045 from being an upgrade that bricks a stack"},
		{"TestReadsAreConfinedToTheCallersTenant", "FLT-001 — neither tenant can read or address the other's machines"},
		{"TestPoolKeyEnrolsOnlyIntoThePoolItNames", "FLT-003 — the Mac pool's key is REFUSED into the Linux pool, and the SAME key enrols into the pool it names (the two-sided half: a credential that refused everything would satisfy the refusal alone)"},
		{"TestAnUnsandboxedHostAttemptIsNeverOfferedToASandboxedLinuxRunner", "FLT-002 — a run lands only on its own pool's machine, and the machine that must NOT be used is asserted still parked and still lease-less"},
		{"TestASingleRunnerDeploymentWithNoPoolConfigurationIsBitUnchanged", "FLT-002 — and with no pool configured at all the behaviour is today's, which is the §2 invariant this whole epic is negotiated against"},
		{"TestAPoolServesItsWaitingRunsOldestFirst", "FLT-002 — the order inside a pool is created_at FIFO, CHOSEN rather than inherited: three attempts arrive in one order, carry ids that sort in a second and timestamps in a third, and only the timestamps decide"},
		{"TestDialRefusesAPoolWhoseRunnerNeverComesRatherThanTakingAnother", "FLT-002/FLT-004 — a Dial on a pool whose machine never arrives refuses rather than reaching for another pool's"},
		{"TestAMachineDeclaringAPostureTheDefaultPoolDoesNotHaveIsTurnedAwayAtEnrolment", "FLT-002 — the posture a machine states is COMPARED with the pool's at the door and the mismatch is journalled (it is never VERIFIED — FLT-P2)"},
		{"TestPlacementNeverOffersOneTenantsAttemptToAnothersRunner", "FLT-004 — tenant B's attempt is never offered tenant A's machine; before this epic the runner plane had no tenant on it AT ALL"},
		{"TestPlacementParksARunWhosePoolHasNoRunner", "FLT-004 — a run whose pool holds no machine reaches `waiting` with no live attempt row, instead of dead-lettering in ~2.5 minutes while a Mac takes 6-20 to boot"},
		{"TestPlacementWakesAParkedRunWhenAMachineJoinsItsPool", "FLT-004 — the machine returns and the parked run WAKES and runs, through E23 T1's choreography rather than a second one"},
		{"TestPlacementWakeLeavesEveryOtherWaitingRunAlone", "FLT-004 — and a run waiting for any OTHER reason is untouched, which is the guard that makes waking safe to attempt on every connect"},
		{"TestPoolKeyRevocationRefusesANewEnrolment", "FLT-003 — after the revocation a NEW enrolment is refused and journalled"},
		{"TestPoolKeyRevocationLeavesAnEnrolledRunnerRenewing", "FLT-003 — and the machine that came in on that key renews TWICE afterwards: this epic's cheapest security test, and the reason cutting the connection on a revoke would be a regression rather than a hardening"},
		{"TestPoolKeyRevocationDoesNotCutAnInFlightLease", "FLT-003 — its in-flight lease still relays frames both ways, so a key revocation cannot become a back door into SAN-011's machine-level hard stop"},
		{"TestPoolKeyValueReachesNoRowJournalOrLog", "FLT-003 — the key VALUE appears in no row, no journal detail and no log line, swept by DECODING every JSONB document rather than grepping bytes"},
		{"TestLifecycleCordonSurvivesAControlPlaneRestartAndResumeClearsIt", "FLT-005 — one machine is cordoned and stays cordoned across a restart while still CONNECTED and taking nothing, and a resume puts it back"},
		{"TestLifecycleRevocationSurvivesAControlPlaneRestart", "FLT-005 — the revoked machine cannot reconnect to a second gateway on the same CA and database, while the machine that was NOT revoked still takes a lease through it"},
		{"TestLifecycleRevokeIsIrreversibleAndJournalled", "FLT-005 — a revoke is irreversible and journalled exactly once however many times it is repeated, and another tenant cannot revoke the machine at all"},
		{"TestStrictEnrolmentHoldsTheMachineOutOfTheRendezvousUntilAHumanApproves", "FLT-003 — a machine enrolling into a strict pool is `pending`, RECEIVES its certificate (so the renew path opens at all) and is unreachable from a Dial"},
		{"TestStrictOffIsBitUnchanged", "FLT-003 — and with strict mode off, the default every pool in this tree carries, the machine's life is exactly T3's and T5's"},
	} {
		if !strings.Contains(string(out), "--- PASS: "+leg.test) {
			t.Errorf("%s did not report PASS (%s) — the co-run of the suite that BACKS its case did not happen, so the bundle's authored PASS for it is unbacked", leg.test, leg.why)
		}
	}

	// AND THE LEG THAT MUST NOT BE THERE. The plan's journey ended with a shell call executing on the
	// runner's machine; T7 was deferred, so no such leg can pass and none is claimed. Asserting its ABSENCE
	// is what stops a later reader from adding a name here that matches something weaker and calling the
	// relay proven.
	for _, absent := range []string{"TestRelay", "TestShellRunsOnTheRunner", "TestExecutionRelay"} {
		if strings.Contains(string(out), "--- PASS: "+absent) {
			t.Errorf("%s reported PASS — E24 T7 was DEFERRED and there is no execution relay, so a green leg with this name is either a test of something else wearing the relay's name or the relay landed without this gate being updated", absent)
		}
	}
}

// TestARedBackingSuiteFailsTheFleetGate is the load-bearing negative: the co-run is only worth anything if a
// RED backing suite actually fails this target. It re-runs the operator entry point with
// PALAI_FLEET_FAULT_SUITE pointing the execution suite (which holds most of the legs) at a DEAD Postgres — a
// genuinely red suite, its tests failing on connect, not a stubbed-out command — and asserts a NON-ZERO exit.
//
// Without this, "the gate co-runs the suites" would be a claim about the script's TEXT rather than its
// behaviour: a swallowed status, a missing `set -e`, or a `|| true` would leave the gate green over a red
// suite, which is exactly the shape of hole this family keeps finding — and which T7 of E21 found in its own
// gate script, twice, as green-by-skip.
func TestARedBackingSuiteFailsTheFleetGate(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available; the fault run needs the throwaway Postgres the script stands up")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "scripts/uat/fleet")
	cmd.Dir = repoRoot(t)
	cmd.Env = append(cmd.Environ(), "SKIP_JOURNEYS=0",
		"PALAI_FLEET_FAULT_SUITE=./apps/control-plane/internal/execution")
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
