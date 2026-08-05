//go:build uat

// The E22 T7 EXIT-gate journey entry point. Like the E15 T6 / E16 T8 / E17 T11 / E19 T9 / E20 T5 / E21 T7
// gates before it, this file is an ORCHESTRATOR rather than a reimplementation: it drives
// `scripts/uat/code-and-ship`, which stands up a throwaway Postgres and runs the narrative where its seams
// already live —
//
//	the ticket body earns five refusals  apps/control-plane/internal/extensions  TestJiraTicketBodyCannotInstructTheAgent
//	the host answers `Xcode 26.6`        apps/control-plane/internal/execution/tools  TestNativeShellPostureRunsTheHostsOwnToolchain
//	nothing publishes without an approve apps/control-plane/internal/execution  TestPublicationWaitsForAnApproveAndThenPublishes
//	a deny prevents the effect           apps/control-plane/internal/execution  TestPublicationDenialPreventsThePushEntirely
//
// THE NARRATIVE IS THE CO-RUN RATHER THAN A SEVENTH TEST, and that is a deliberate choice with a reason: each
// leg above drives the SHIPPED path at the seam that owns it, against real PostgreSQL, and a journey that
// re-implemented any of them would be asserting its own copy — exactly the shape of proof this family exists
// to refuse. What this file adds is the thing a co-run cannot give itself: the assertion that every leg RAN.
//
// TWO ROWS LEFT THIS TABLE, 2026-08-05 (81aada0f): "a thread reaches a repository" and "the artifact arrives
// as a FILE" were TestSlackRunCarriesTheConnectionsRepositoryBinding and
// TestSlackRunArtifactReachesTheThreadAsAFile, apps/control-plane/internal/store, both permanently deleted
// with the in-process Slack bridge. CAS-001's claim moved to cmd/cli/internal/stack/up_repository_test.go,
// which is UNTAGGED and already rides plain `go test` under `make verify` — it needs no co-run here. CAS-004's
// claim moved to TestReadRunArtifactRefusesForeignRunsForeignTenantsAndOversize
// (apps/control-plane/internal/artifacts), which needs a SeaweedFS container this script does not stand up
// (see scripts/uat/code-and-ship's own comment) — its real home is `make test-component TEST=artifacts`, a
// different gate, and it is named here as an honest ceiling rather than checked for a PASS this tier cannot
// produce.
//
// HONEST CEILING (plan §6): every counterparty is a documented FAKE. No socket reaches slack.com, no GitHub
// App exists, no MCP server is dialled and no Apple signing identity is engaged. The one claim no
// deterministic tier can close — a real Xcode building a real project and a real simulator being driven — is
// the HARDWARE-gated live leg (`make test-live-mac`), and it is never claimed here.
package codeandship

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestCodeAndShipJourneyRunsThroughTheOperatorEntryPoint runs the §T7 narrative through the shipped target.
// It is Docker-bound (a throwaway Postgres), so it rides the `uat` tag and never `make verify`. It needs NO
// credential: every leg is deterministic against real PostgreSQL, a fake Slack peer and a fake publisher.
func TestCodeAndShipJourneyRunsThroughTheOperatorEntryPoint(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available; the E22 backing suites need a throwaway Postgres")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "scripts/uat/code-and-ship")
	cmd.Dir = repoRoot(t)
	cmd.Env = append(cmd.Environ(), "SKIP_JOURNEYS=0")
	out, err := cmd.CombinedOutput()
	t.Logf("$ scripts/uat/code-and-ship\n%s", out)
	if err != nil {
		t.Fatalf("the E22 code-and-ship gate failed: %v", err)
	}

	// EACH LINE IS ONE LEG OF THE NARRATIVE, and each is named because it is the leg that would rot first if
	// the `-run` selector drifted. The bundle's per-case status is authored data, so what makes a PASS for
	// CAS-001..005 honest is that the suite backing it ran in THIS invocation — and a `-run` pointed at the
	// wrong package matches nothing at all and exits 0 in silence, which is how this repository has shipped
	// green-by-omission more than a dozen times.
	for _, leg := range []struct{ test, why string }{
		{"TestJiraTicketBodyCannotInstructTheAgent", "CAS-003 — the ticket body earns five refusals, each re-derived"},
		{"TestNativeShellPostureRunsTheHostsOwnToolchain", "CAS-005 — the host answers where the container says `not found`"},
		{"TestNativeShellPostureSeparatesConcurrentSessionsOnOneMac", "CAS-005 — two concurrent runs, disjoint session directories"},
		{"TestPublicationWaitsForAnApproveAndThenPublishes", "CAS-002 — nothing publishes until a human presses Approve"},
		{"TestPublicationDenialPreventsThePushEntirely", "CAS-002 — a deny PREVENTS the effect rather than recording a verdict"},
		{"TestPublicationTargetsTheBindingsBaseBranch", "CAS-002 — `dev` is a binding value, not a code constant"},
	} {
		if !strings.Contains(string(out), "--- PASS: "+leg.test) {
			t.Errorf("%s did not report PASS (%s) — the co-run of the suite that BACKS its case did not happen, so the bundle's authored PASS for it is unbacked", leg.test, leg.why)
		}
	}
}

// TestARedBackingSuiteFailsTheCodeAndShipGate is the load-bearing negative: the co-run is only worth anything
// if a RED backing suite actually fails this target. It re-runs the operator entry point with
// PALAI_SHIP_FAULT_SUITE pointing the execution suite (which holds the publication legs) at a DEAD Postgres —
// a genuinely red suite, its tests failing on connect, not a stubbed-out command — and asserts a NON-ZERO
// exit.
//
// Without this, "the gate co-runs the suites" would be a claim about the script's TEXT rather than its
// behaviour: a swallowed status, a missing `set -e`, or a `|| true` would leave the gate green over a red
// suite, which is exactly the shape of hole this family keeps finding.
func TestARedBackingSuiteFailsTheCodeAndShipGate(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available; the fault run needs the throwaway Postgres the script stands up")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "scripts/uat/code-and-ship")
	cmd.Dir = repoRoot(t)
	cmd.Env = append(cmd.Environ(), "SKIP_JOURNEYS=0",
		"PALAI_SHIP_FAULT_SUITE=./apps/control-plane/internal/execution")
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
