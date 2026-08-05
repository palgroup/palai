//go:build uat

// The E19 T9 EXIT-gate journey entry point. Like the E15 T6 / E16 T8 / E17 T11 gates before it, this file
// is an ORCHESTRATOR rather than a reimplementation: it drives `scripts/uat/wiring`, which stands up a
// throwaway Postgres and runs the FULL backing suites (store, extensions, automation — no -run filter) where
// their seams already live.
//
// THE SINGLE E19 JOURNEY (TestWiringJourney, apps/control-plane/internal/store) IS GONE, permanently
// (81aada0f, 2026-08-05, "carry the publication-approval evidence off Slack, and delete the two exit
// journeys"): it drove four Slack surfaces that no longer mount in this control plane. Slack is
// apps/slack-bot now, reaching this one over the same /v1 any other client uses, and the deletion commit's
// own words are the ceiling here too: "neither can be re-earned against a bridge that is gone." What this
// file still proves is that the SURVIVING suites in those three packages ran clean in one invocation
// (`err != nil` below) plus the one cross-surface outbound claim named explicitly further down.
//
// It lives here because it needs the api package AND three packages under apps/control-plane/internal at
// once (Go's internal rule means only a package rooted there can import them). This file gives it its
// canonical `tests/uat/wiring` home; the Docker-free gates in this same package (bundle, mount refusals,
// promote, live inventory) are what `make verify` rides.
//
// HONEST CEILING (plan §6): every counterparty is a documented FAKE. No socket reaches slack.com, no foreign
// A2A peer is contacted, no broker product is started, and no deployed console gets a screen-reader pass.
// The journey proves the code is MOUNTED and CORRECT AGAINST THE PUBLISHED CONTRACT — nothing more, and the
// tier recompute turns that ceiling into a mechanical outcome rather than a promise.
package wiring

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestWiringJourneyRunsThroughTheOperatorEntryPoint runs the §T9 journey through the shipped target. It is
// Docker-bound (a throwaway Postgres), so it rides the `uat` tag and never `make verify`. It needs NO
// credential: the journey is deterministic against real PostgreSQL.
func TestWiringJourneyRunsThroughTheOperatorEntryPoint(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available; the E19 journey needs a throwaway Postgres")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "scripts/uat/wiring")
	cmd.Dir = repoRoot(t)
	cmd.Env = append(cmd.Environ(), "SKIP_JOURNEYS=0")
	out, err := cmd.CombinedOutput()
	t.Logf("$ scripts/uat/wiring\n%s", out)
	if err != nil {
		t.Fatalf("the E19 wiring gate failed: %v", err)
	}

	// TestWiringJourney ITSELF IS GONE, permanently (81aada0f, 2026-08-05): it drove four Slack surfaces
	// (Socket Mode admission, the HTTP twin's silence, an authorized click, the registration route) that no
	// longer mount in this control plane at all — Slack is apps/slack-bot now, a separate process reaching
	// this one over the same /v1 any other client uses. The commit's own words: "neither can be re-earned
	// against a bridge that is gone." So this co-run no longer checks for a single super-journey; it checks
	// that the FULL backing suites below (no -run filter — see scripts/uat/wiring) ran and stayed green,
	// which `err != nil` above already establishes, plus the one cross-surface claim that DID survive.
	//
	// SLK-001/SLK-007/the T9 registration route are WITHDRAWN from this gate along with TestWiringJourney —
	// their case.yaml proof lists moved off this package (see tests/uat/cases/SLK-001, SLK-007). What is
	// still this gate's to prove is AUT-010: a queue outbound delivery is still lossless and exactly-once,
	// and it never depended on Slack at all.
	for _, backing := range []string{
		"TestQueueTerminalEnqueuesOutboundLosslessExactlyOnce", // AUT-010 outbound
	} {
		if !strings.Contains(string(out), "--- PASS: "+backing) {
			t.Errorf("%s did not report PASS — the co-run of the suite that BACKS its case did not happen, so the bundle's authored PASS for it is unbacked", backing)
		}
	}
}

// TestARedBackingSuiteFailsTheWiringGate is the load-bearing negative: the co-run is only worth anything if
// a RED backing suite actually fails this target. It re-runs the operator entry point with
// PALAI_WIRING_FAULT_SUITE pointing the store suite (which holds the journey) at a DEAD Postgres — a
// genuinely red suite, its tests failing on connect, not a stubbed-out command — and asserts a NON-ZERO exit.
//
// Without this, "the gate co-runs the suites" would be a claim about the script's TEXT rather than its
// behaviour: a swallowed status, a missing `set -e`, or a `|| true` would leave the gate green over a red
// suite, which is exactly the shape of hole this whole family keeps finding.
func TestARedBackingSuiteFailsTheWiringGate(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available; the fault run needs the throwaway Postgres the script stands up")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "scripts/uat/wiring")
	cmd.Dir = repoRoot(t)
	cmd.Env = append(cmd.Environ(), "SKIP_JOURNEYS=0",
		"PALAI_WIRING_FAULT_SUITE=./apps/control-plane/internal/store")
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
