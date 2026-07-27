//go:build uat

// The E20 T5 EXIT-gate journey entry point. Like the E15 T6 / E16 T8 / E17 T11 / E19 T9 gates before it,
// this file is an ORCHESTRATOR rather than a reimplementation: it drives `scripts/uat/agent-surface`, which
// stands up a throwaway Postgres and runs the journey where its seams already live —
//
//	the E20 agent-surface journey   apps/control-plane/internal/store   TestAgentSurfaceJourney
//
// It lives there because it needs the api package AND two packages under apps/control-plane/internal at once
// (Go's internal rule means only a package rooted there can import them), and because a journey that
// re-implemented the Slack admission bridge, the run follower and the reply pump would be proving its own
// copy rather than the shipped code. This file gives it its canonical `tests/uat/agent-surface` home; the
// Docker-free gates in this same package (bundle, refusal matrix, promote, catalog, live inventory) are what
// `make verify` rides.
//
// HONEST CEILING (plan §6): every counterparty is a documented FAKE. No socket reaches slack.com. The
// journey proves the surface is CORRECT AGAINST THE PUBLISHED CONTRACT and funnelled through ONE admission —
// nothing more, and the tier recompute turns that ceiling into a mechanical outcome rather than a promise.
package agentsurface

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestAgentSurfaceJourneyRunsThroughTheOperatorEntryPoint runs the §T5 journey through the shipped target.
// It is Docker-bound (a throwaway Postgres), so it rides the `uat` tag and never `make verify`. It needs NO
// credential: the journey is deterministic against real PostgreSQL and a fake Slack peer.
func TestAgentSurfaceJourneyRunsThroughTheOperatorEntryPoint(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available; the E20 journey needs a throwaway Postgres")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "scripts/uat/agent-surface")
	cmd.Dir = repoRoot(t)
	cmd.Env = append(cmd.Environ(), "SKIP_JOURNEYS=0")
	out, err := cmd.CombinedOutput()
	t.Logf("$ scripts/uat/agent-surface\n%s", out)
	if err != nil {
		t.Fatalf("the E20 agent-surface gate failed: %v", err)
	}

	// The journey must have RUN, not skipped. A silent skip (an unset Postgres URL) would leave the exit
	// gate reporting green over nothing — this family's signature failure, eleven findings deep.
	if !strings.Contains(string(out), "--- PASS: TestAgentSurfaceJourney") {
		t.Error("TestAgentSurfaceJourney did not report PASS — a skipped journey is not a green exit gate")
	}
	// And the BACKING tests must each have run in FULL. The bundle's per-case status is authored data, so
	// what makes a PASS for SLK-009..012 honest is that the suite backing it ran in this very invocation.
	for _, backing := range []string{
		"TestSlackStreamShowsTheRunWorkingThenClosesWithTheAnswer",  // SLK-009
		"TestSlackDMIsExemptFromTheConfiguredChannelAllowList",      // SLK-010
		"TestSlackContextNeverBecomesAFetchTarget",                  // SLK-011
		"TestSlackStopStreamCarriesRenderedBlocksAndNoForgedButton", // SLK-012
	} {
		if !strings.Contains(string(out), "--- PASS: "+backing) {
			t.Errorf("%s did not report PASS — the co-run of the suite that BACKS its case did not happen, so the bundle's authored PASS for it is unbacked", backing)
		}
	}
}

// TestARedBackingSuiteFailsTheSurfaceGate is the load-bearing negative: the co-run is only worth anything if
// a RED backing suite actually fails this target. It re-runs the operator entry point with
// PALAI_SURFACE_FAULT_SUITE pointing the store suite (which holds the journey) at a DEAD Postgres — a
// genuinely red suite, its tests failing on connect, not a stubbed-out command — and asserts a NON-ZERO exit.
//
// Without this, "the gate co-runs the suites" would be a claim about the script's TEXT rather than its
// behaviour: a swallowed status, a missing `set -e`, or a `|| true` would leave the gate green over a red
// suite, which is exactly the shape of hole this family keeps finding.
func TestARedBackingSuiteFailsTheSurfaceGate(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available; the fault run needs the throwaway Postgres the script stands up")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "scripts/uat/agent-surface")
	cmd.Dir = repoRoot(t)
	cmd.Env = append(cmd.Environ(), "SKIP_JOURNEYS=0",
		"PALAI_SURFACE_FAULT_SUITE=./apps/control-plane/internal/store")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the gate PASSED with the store backing suite pointed at a dead Postgres — a red backing suite must fail this target, or the co-run certifies nothing\n%s", out)
	}
	if !strings.Contains(string(out), "FAULT INJECTED") {
		t.Errorf("the run does not report the injected fault, so the non-zero exit may be unrelated:\n%s", out)
	}
	if !strings.Contains(string(out), "backing suite ./apps/control-plane/internal/store FAILED") {
		t.Errorf("the failure is not attributed to the faulted backing suite:\n%s", out)
	}
	t.Logf("a red backing suite failed the gate as required: %v", err)
}
