//go:build uat

// The E21 T7 EXIT-gate journey entry point. Like the E15 T6 / E16 T8 / E17 T11 / E19 T9 / E20 T5 gates before
// it, this file is an ORCHESTRATOR rather than a reimplementation: it drives `scripts/uat/tools-memory`,
// which stands up a throwaway Postgres and runs the journey where its seams already live —
//
//	the E21 tools-and-memory journey   apps/control-plane/internal/execution   TestToolsMemoryJourney
//
// It lives THERE rather than in the store package because the compaction fold is `historyMessages`, which is
// unexported: a journey that re-implemented the window would be asserting its own copy rather than the
// shipped code, which is exactly the shape of proof this family exists to refuse.
//
// THE SLACK HALF OF THE NARRATIVE MOVED, 2026-08-05: TLM-003 used to rest on the store tier's
// TestSlackTerminalRunMentionsTheRequesterAndOnlyTheRequester (that the requester id survives admission, the
// enqueue freeze and a RESTARTED reply pump), which is permanently deleted with the in-process Slack bridge
// (81aada0f). What TLM-003 claims today is narrower and lives elsewhere: the requester id is neutralised and
// mentioned correctly by adapters/integrations/slack/mention_test.go (Docker-free, step [2/4] of
// scripts/uat/tools-memory), and migration 000043's column survives a restart via
// TestMigration43SlackRequester (./tests/component/postgres, still co-run below). Neither is a store-package
// journey any more, and this file's checks follow that move rather than the old name.
//
// HONEST CEILING (plan §6): every counterparty is a documented FAKE. No socket reaches slack.com and no MCP
// server exists. The journey proves the surface is CORRECT AGAINST THE PUBLISHED CONTRACT — nothing more,
// and the tier recompute turns that ceiling into a mechanical outcome rather than a promise.
package toolsmemory

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestToolsMemoryJourneyRunsThroughTheOperatorEntryPoint runs the §T7 journey through the shipped target. It
// is Docker-bound (a throwaway Postgres), so it rides the `uat` tag and never `make verify`. It needs NO
// credential: the journey is deterministic against real PostgreSQL and a fake Slack peer.
func TestToolsMemoryJourneyRunsThroughTheOperatorEntryPoint(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available; the E21 journey needs a throwaway Postgres")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "scripts/uat/tools-memory")
	cmd.Dir = repoRoot(t)
	cmd.Env = append(cmd.Environ(), "SKIP_JOURNEYS=0")
	out, err := cmd.CombinedOutput()
	t.Logf("$ scripts/uat/tools-memory\n%s", out)
	if err != nil {
		t.Fatalf("the E21 tools-memory gate failed: %v", err)
	}

	// The journey must have RUN, not skipped. A silent skip (an unset Postgres URL) would leave the exit gate
	// reporting green over nothing — this family's signature failure.
	if !strings.Contains(string(out), "--- PASS: TestToolsMemoryJourney") {
		t.Error("TestToolsMemoryJourney did not report PASS — a skipped journey is not a green exit gate")
	}
	// And the BACKING tests must each have run in FULL. The bundle's per-case status is authored data, so what
	// makes a PASS for TLM-001..005 honest is that the suite backing it ran in this very invocation.
	//
	// EACH LINE IS ONE CASE, and each is named because it is the leg that would rot first if the `-run`
	// selector drifted. TLM-002's own suite is Docker-free and rides step [2/4] of the script rather than the
	// journey tier, which is why it is not in this list — its compose-tier proof is `e2e` and is not run here.
	for _, backing := range []struct{ test, why string }{
		{"TestClearSessionEmptiesHistoryAndKeepsTheSessionAlive", "TLM-001 — the durable half of `clear`"},
		{"TestMigration43SlackRequester", "TLM-003 — the requester column survives a restart (the mention-mint half is Docker-free, step [2/4])"},
		{"TestARegistryToolDispatchesThroughTheRealOrchestratorAndTheResultReachesTheEngine", "TLM-004 — committed, then delivered"},
		{"TestToolsMemoryJourney", "TLM-005 — the stored-search-byte sweep over what the run persisted"},
	} {
		if !strings.Contains(string(out), "--- PASS: "+backing.test) {
			t.Errorf("%s did not report PASS (%s) — the co-run of the suite that BACKS its case did not happen, so the bundle's authored PASS for it is unbacked", backing.test, backing.why)
		}
	}
}

// TestARedBackingSuiteFailsTheToolsMemoryGate is the load-bearing negative: the co-run is only worth anything
// if a RED backing suite actually fails this target. It re-runs the operator entry point with
// PALAI_MEMORY_FAULT_SUITE pointing the execution suite (which holds the journey) at a DEAD Postgres — a
// genuinely red suite, its tests failing on connect, not a stubbed-out command — and asserts a NON-ZERO exit.
//
// Without this, "the gate co-runs the suites" would be a claim about the script's TEXT rather than its
// behaviour: a swallowed status, a missing `set -e`, or a `|| true` would leave the gate green over a red
// suite, which is exactly the shape of hole this family keeps finding.
func TestARedBackingSuiteFailsTheToolsMemoryGate(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available; the fault run needs the throwaway Postgres the script stands up")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "scripts/uat/tools-memory")
	cmd.Dir = repoRoot(t)
	cmd.Env = append(cmd.Environ(), "SKIP_JOURNEYS=0",
		"PALAI_MEMORY_FAULT_SUITE=./apps/control-plane/internal/execution")
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
