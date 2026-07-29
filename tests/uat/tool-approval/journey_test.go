//go:build uat

// The E23 T7 EXIT-gate journey entry point. Like the E15 T6 / E16 T8 / E17 T11 / E19 T9 / E20 T5 / E21 T7 /
// E22 T7 gates before it, this file is an ORCHESTRATOR rather than a reimplementation: it drives
// `scripts/uat/tool-approval`, which stands up a throwaway Postgres and runs the narrative where its seams
// already live —
//
//	the ticket arrives and buys nothing      internal/execution  TestAJiraTicketBodyCannotApproveItself
//	a gated call never reaches Execute       internal/execution  TestToolApprovalGateNeverReachesExecuteWithoutAHumanDecision
//	an UNgated call is bit-unchanged         internal/execution  TestToolWithoutApprovalDeclaredIsBitUnchanged
//	the run PARKS instead of answering       internal/execution  TestToolApprovalParksTheRunRatherThanAnsweringWithoutADecision
//	a real message lands in the thread       internal/execution  TestPublicationFromSlackPostsAnApprovalMessageIntoTheThread
//	the modal READS and writes nothing       internal/extensions TestSlackApprovalModalReadsTheLedgerAndWritesNothing
//	an unauthorized click decides nothing    internal/store      TestSlackApproverBothListsMustBePassed
//	...and the HTTP key too, same function   internal/execution  TestApproverAKeyOutsideTheProjectListDecidesNothing
//	the GENERIC ask, and a click that lands   internal/execution  TestSlackToolApprovalAsksInTheThreadAndAnAuthorizedClickReleasesTheRun
//	...refused for the unauthorized clicker   internal/execution  TestSlackToolApprovalRefusesAnUnauthorizedClicker
//	...carrying the operator's own sentence   internal/execution  TestSlackToolApprovalCarriesTheOperatorsOwnLabelForARegisteredTool
//	...and a deny that runs nothing           internal/execution  TestSlackToolApprovalDenyReleasesTheRunAndRunsNothing
//	the approved push drives once            internal/execution  TestToolApprovalApprovedCallRunsExactlyOnceAndWakesTheRun
//	merge sends the APPROVED head            internal/execution  TestMergePullRequestRefusesWhenTheHeadMovedAfterTheApproval
//	a second approval gates the Jira write   internal/execution  TestApprovedMCPArgumentsReachThePeerByteForByte
//	nobody presses: it expires, run wakes    internal/execution  TestExpiredToolApprovalCancelsTheCallAndWakesTheParkedRun
//
// THE NARRATIVE IS THE CO-RUN RATHER THAN A THIRTEENTH TEST, and that is a deliberate choice with a reason:
// each leg above drives the SHIPPED path at the seam that owns it, against real PostgreSQL, and a journey
// that re-implemented any of them would be asserting its own copy — exactly the shape of proof this family
// exists to refuse. What this file adds is the thing a co-run cannot give itself: the assertion that every
// leg RAN.
//
// HONEST CEILING (plan §6): every counterparty is a documented FAKE. No socket reaches slack.com, no
// Atlassian tenant is dialled, no GitHub App exists. And the ordering above reads as one story on purpose
// while being twelve independent suites in fact — the story is how the epic is judged, the suites are what
// is measured, and this comment is where the difference is admitted rather than blurred.
package toolapproval

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestToolApprovalJourneyRunsThroughTheOperatorEntryPoint runs the §T7 narrative through the shipped target.
// It is Docker-bound (a throwaway Postgres), so it rides the `uat` tag and never `make verify`. It needs NO
// credential: every leg is deterministic against real PostgreSQL, a fake Slack peer, a fake MCP server and a
// fake publisher.
func TestToolApprovalJourneyRunsThroughTheOperatorEntryPoint(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available; the E23 backing suites need a throwaway Postgres")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "scripts/uat/tool-approval")
	cmd.Dir = repoRoot(t)
	cmd.Env = append(cmd.Environ(), "SKIP_JOURNEYS=0")
	out, err := cmd.CombinedOutput()
	t.Logf("$ scripts/uat/tool-approval\n%s", out)
	if err != nil {
		t.Fatalf("the E23 tool-approval gate failed: %v", err)
	}

	// EACH LINE IS ONE LEG OF THE NARRATIVE, and each is named because it is the leg that would rot first if
	// the `-run` selector drifted. The bundle's per-case status is authored data, so what makes a PASS for
	// HIL-001..005 honest is that the suite backing it ran in THIS invocation — and a `-run` pointed at the
	// wrong package matches nothing at all and exits 0 in silence, which is how this repository has shipped
	// green-by-omission more than a dozen times, once inside this very epic (E23 T1's bit-unchanged test
	// matched no alternative in the component allow-list and never ran until T7 measured it).
	for _, leg := range []struct{ test, why string }{
		{"TestAJiraTicketBodyCannotApproveItself", "HIL-005 — the ticket body demands its own approval and buys nothing"},
		{"TestToolApprovalGateNeverReachesExecuteWithoutAHumanDecision", "HIL-001 — a gated call never reaches Execute without a human"},
		{"TestToolWithoutApprovalDeclaredIsBitUnchanged", "HIL-001 — and an UNGATED tool is bit-unchanged, which is what stops the gate from being a wall"},
		{"TestToolApprovalParksTheRunRatherThanAnsweringWithoutADecision", "HIL-003 — the run PARKS instead of answering"},
		{"TestPublicationFromSlackPostsAnApprovalMessageIntoTheThread", "HIL-003 — a real approval message lands in the thread (E19's button gets its first production caller)"},
		{"TestSlackApprovalModalReadsTheLedgerAndWritesNothing", "HIL-002 — the modal READS the ledger row and writes nothing"},
		{"TestSlackApproverBothListsMustBePassed", "HIL-004 — an unauthorized Slack click decides nothing"},
		{"TestApproverAKeyOutsideTheProjectListDecidesNothing", "HIL-004 — and an unlisted API key decides nothing, in the SAME function"},
		{"TestSlackToolApprovalAsksInTheThreadAndAnAuthorizedClickReleasesTheRun", "HIL-003 — the GENERIC half's ask and answer: a gated non-publication call produces a real three-button question in the thread and an authorized click releases the parked run (E23 T8; through T7 neither existed, which is what HIL-P8 recorded)"},
		{"TestSlackToolApprovalRefusesAnUnauthorizedClicker", "HIL-004 — and an unauthorized click on a TOOL approval decides nothing, under BOTH lists, in the same function that refuses a publication"},
		{"TestSlackToolApprovalCarriesTheOperatorsOwnLabelForARegisteredTool", "HIL-002 — the operator's own sentence reaches the screen for a REGISTERED revision, through the same lookup the executor uses"},
		{"TestSlackToolApprovalDenyReleasesTheRunAndRunsNothing", "HIL-001 — a deny releases the run with an answer the model can act on, and the effect never happens"},
		{"TestToolApprovalApprovedCallRunsExactlyOnceAndWakesTheRun", "HIL-001 — an authorized decision drives the call ONCE and wakes the run"},
		{"TestMergePullRequestRefusesWhenTheHeadMovedAfterTheApproval", "HIL-005 — the merge sends the APPROVED head, so a moved head does not merge"},
		{"TestApprovedMCPArgumentsReachThePeerByteForByte", "HIL-005 — a second approval, and the peer receives the bytes the screen showed"},
		{"TestExpiredToolApprovalCancelsTheCallAndWakesTheParkedRun", "HIL-003 — nobody presses, it expires, and the parked run is woken"},
	} {
		if !strings.Contains(string(out), "--- PASS: "+leg.test) {
			t.Errorf("%s did not report PASS (%s) — the co-run of the suite that BACKS its case did not happen, so the bundle's authored PASS for it is unbacked", leg.test, leg.why)
		}
	}
}

// TestARedBackingSuiteFailsTheToolApprovalGate is the load-bearing negative: the co-run is only worth
// anything if a RED backing suite actually fails this target. It re-runs the operator entry point with
// PALAI_APPROVAL_FAULT_SUITE pointing the execution suite (which holds every gate leg) at a DEAD Postgres —
// a genuinely red suite, its tests failing on connect, not a stubbed-out command — and asserts a NON-ZERO
// exit.
//
// Without this, "the gate co-runs the suites" would be a claim about the script's TEXT rather than its
// behaviour: a swallowed status, a missing `set -e`, or a `|| true` would leave the gate green over a red
// suite, which is exactly the shape of hole this family keeps finding.
func TestARedBackingSuiteFailsTheToolApprovalGate(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available; the fault run needs the throwaway Postgres the script stands up")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "scripts/uat/tool-approval")
	cmd.Dir = repoRoot(t)
	cmd.Env = append(cmd.Environ(), "SKIP_JOURNEYS=0",
		"PALAI_APPROVAL_FAULT_SUITE=./apps/control-plane/internal/execution")
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
