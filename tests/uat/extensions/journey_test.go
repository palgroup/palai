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
}
