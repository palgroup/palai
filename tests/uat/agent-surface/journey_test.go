//go:build uat

// The E20 T5 EXIT-gate journey entry point. Like the E15 T6 / E16 T8 / E17 T11 / E19 T9 gates before it,
// this file is an ORCHESTRATOR rather than a reimplementation: it drives `scripts/uat/agent-surface`, which
// stands up a throwaway Postgres and runs the tenancy corpus, the one thing left in that tier that needs it.
//
// THE E20 T5 JOURNEY (TestAgentSurfaceJourney, apps/control-plane/internal/store) IS GONE, permanently
// (81aada0f, 2026-08-05, "carry the publication-approval evidence off Slack, and delete the two exit
// journeys"): it drove the panel/DM/Socket-Mode admission bridge, the HTTP-redelivery twin and an approval
// click against the in-process Slack bridge, which this control plane no longer has — "neither can be
// re-earned against a bridge that is gone" is the deletion commit's own ceiling and it applies here too.
// SLK-009..012 moved with it: their case.yaml proof lists are now entirely `proof_class: unit` against
// adapters/integrations/slack (the Docker-free step scripts/uat/agent-surface already runs), so nothing this
// gate still certifies was backed by the deleted journey.
//
// This file gives the tenancy corpus its canonical `tests/uat/agent-surface` home; the Docker-free gates in
// this same package (bundle, refusal matrix, promote, catalog, live inventory) are what `make verify` rides.
//
// HONEST CEILING (plan §6): every counterparty is a documented FAKE. No socket reaches slack.com. The
// journey proves the surface is CORRECT AGAINST THE PUBLISHED CONTRACT — nothing more, and the tier
// recompute turns that ceiling into a mechanical outcome rather than a promise.
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

	// TestAgentSurfaceJourney and its four SLK-009..012 backing legs are GONE (see the header comment) and
	// are not checked for any more. What replaces them: the tenancy corpus must have RUN, not skipped. A
	// silent skip (an unset Postgres URL) would leave the exit gate reporting green over nothing — this
	// family's signature failure, eleven findings deep — and the store package's Slack sub-step already fell
	// into exactly that trap once this session (a `-run` matching zero deleted tests, exiting 0 in silence)
	// before scripts/uat/agent-surface removed it.
	for _, backing := range []string{
		"TestEveryTenantTableIsRowLevelSecured", // every RLS table, re-walked after the Slack table drops
		"TestConnectionWithoutTenantContextSeesNoTenantRows",
	} {
		if !strings.Contains(string(out), "--- PASS: "+backing) {
			t.Errorf("%s did not report PASS — the co-run of the suite that BACKS its case did not happen, so the bundle's authored PASS for it is unbacked", backing)
		}
	}
}

// TestARedBackingSuiteFailsTheSurfaceGate is the load-bearing negative: the co-run is only worth anything if
// a RED backing suite actually fails this target. It re-runs the operator entry point with
// PALAI_SURFACE_FAULT_SUITE pointing the tenancy corpus — the only real-Postgres user left in this tier
// since the store package's Slack sub-step was removed (see the header comment) — at a DEAD Postgres: a
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
		"PALAI_SURFACE_FAULT_SUITE=./tests/security/tenancy")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the gate PASSED with the tenancy corpus pointed at a dead Postgres — a red backing suite must fail this target, or the co-run certifies nothing\n%s", out)
	}
	if !strings.Contains(string(out), "FAULT INJECTED") {
		t.Errorf("the run does not report the injected fault, so the non-zero exit may be unrelated:\n%s", out)
	}
	if !strings.Contains(string(out), "the tenancy corpus FAILED") {
		t.Errorf("the failure is not attributed to the faulted tenancy corpus:\n%s", out)
	}
	t.Logf("a red backing suite failed the gate as required: %v", err)
}
