//go:build uat

// The E17 T11 EXIT-gate journey entry point. Like the E15 T6 SH-2 and E16 T8 SDK-parity journeys it is an
// ORCHESTRATOR, not a reimplementation: it drives `scripts/uat/extensions`, which stands up a throwaway
// Postgres and runs the three journeys where their seams already live —
//
//	spec §63.3 SLACK journey   apps/control-plane/internal/extensions  TestSlackJourneyOnFakePeer
//	KNOWLEDGE journey          apps/control-plane/internal/knowledge   TestKnowledgeJourney
//	WORKER journey             apps/control-plane/internal/workers     TestWorkerJourney
//
// They live there because each needs three real seams at once from packages under apps/control-plane/internal
// (Go's internal rule means only a package rooted there can import them), and because a journey that
// re-implemented the Slack store / FTS spine / worker gateway would be proving its own copy rather than the
// shipped code. This file gives the journeys their canonical `tests/uat/extensions` home and one runnable
// entry point; the Docker-free gates in this same package (catalog, tier anchor, bundle, promote) are what
// `make verify` rides.
//
// HONEST CEILING (plan §6): every journey leg is LOCAL. The Slack journey's peer is FAKE, the A2A exchange is
// LOOPBACK, the worker is a FIXTURE with no Apple signing material anywhere, and there is no vector store. The
// tier recompute turns those ceilings into mechanical outcomes: slack + a2a close PREVIEW, knowledge-vector +
// apple-build close DISABLED. Nothing here claims a real Slack workspace, a foreign A2A peer, or a signed
// Apple build.
package extensions

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestExtensionsJourneys runs the three §T11 journeys through the shipped operator entry point. It is
// Docker-bound (a throwaway Postgres), so it rides the `uat` tag and never `make verify`. It needs NO
// credential: the journeys are deterministic against real PostgreSQL.
func TestExtensionsJourneys(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available; the three E17 journeys need a throwaway Postgres")
	}
	root := repoRoot(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	// SKIP_JOURNEYS is deliberately unset here (that flag exists for the Docker-free core alone) and PROVIDER
	// stays fake: the live smoke is the operator's `make uat-extensions PROVIDER=provider-one`, not this test.
	cmd := exec.CommandContext(ctx, "scripts/uat/extensions")
	cmd.Dir = root
	cmd.Env = append(cmd.Environ(), "PROVIDER=fake", "SKIP_JOURNEYS=0")
	out, err := cmd.CombinedOutput()
	t.Logf("$ scripts/uat/extensions\n%s", out)
	if err != nil {
		t.Fatalf("the E17 extensions journeys failed: %v", err)
	}

	// The three journeys must each have RUN, not skipped: a silent skip (an unset Postgres URL) would leave the
	// exit gate reporting green on nothing.
	for _, journey := range []string{"TestSlackJourneyOnFakePeer", "TestKnowledgeJourney", "TestWorkerJourney"} {
		if !strings.Contains(string(out), "--- PASS: "+journey) {
			t.Errorf("%s did not report PASS — a skipped journey is not a green exit gate", journey)
		}
	}

	// The BACKING suites must each have run IN FULL. The bundle's per-case status is authored data, so what
	// makes a "PASS" for KNO-007 / WRK-004 / AUT-010 honest is that its suite ran in this very invocation. A
	// -run filter creeping back onto one of these packages is the regression this asserts against: each must
	// report at least one PASS the journey list does not account for.
	for _, backing := range []string{
		"TestRestrictedSourceNotEmbeddedToDisallowedRegion", // knowledge (KNO-007)
		"TestSecretHandleScopeAndExpiry",                    // workers (WRK-004)
		"TestQueueAdapterFloodAppliesBackpressureNoDrop",    // automation queue leg (AUT-010)
	} {
		if !strings.Contains(string(out), "--- PASS: "+backing) {
			t.Errorf("%s did not report PASS — the co-run of the suite that BACKS its case did not happen, so the bundle's authored PASS for it is unbacked (review MUST-FIX 1)", backing)
		}
	}
}

// TestARedBackingSuiteFailsTheGate is MUST-FIX 1's negative, and it is the load-bearing one: the co-run is only
// worth anything if a RED backing suite actually fails this target. It re-runs the operator entry point with
// PALAI_EXTENSIONS_FAULT_SUITE pointing the knowledge suite at a DEAD Postgres — a genuinely red suite, its
// tests failing on connect, not a stubbed-out command — and asserts a NON-ZERO exit.
//
// Without this, "the gate co-runs the suites" would be a claim about the script's text rather than its
// behaviour: a swallowed status, a missing `set -e`, or a `|| true` would leave the gate green over a red suite,
// which is exactly the shape of the hole the review found in the first place.
func TestARedBackingSuiteFailsTheGate(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available; the fault run needs the throwaway Postgres the script stands up")
	}
	root := repoRoot(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "scripts/uat/extensions")
	cmd.Dir = root
	cmd.Env = append(cmd.Environ(), "PROVIDER=fake", "SKIP_JOURNEYS=0",
		"PALAI_EXTENSIONS_FAULT_SUITE=./apps/control-plane/internal/knowledge")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the gate PASSED with the knowledge backing suite pointed at a dead Postgres — a red backing suite must fail this target, or the co-run certifies nothing (review MUST-FIX 1)\n%s", out)
	}
	if !strings.Contains(string(out), "FAULT INJECTED") {
		t.Errorf("the run does not report the injected fault, so the non-zero exit may be unrelated:\n%s", out)
	}
	if !strings.Contains(string(out), "backing suite ./apps/control-plane/internal/knowledge FAILED") {
		t.Errorf("the failure is not attributed to the faulted backing suite:\n%s", out)
	}
	t.Logf("a red backing suite failed the gate as required: %v", err)
}
