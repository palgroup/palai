//go:build e2e

package responses

import (
	"context"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/packages/coordinator"
)

// blockingDialer simulates a runner that connects but whose dial/handshake wedges (or a
// gateway with no available runner): every Dial blocks until its context is done. It is
// ctx-honest — exactly like the real gateway's `case <-ctx.Done()` — so the orchestrator's
// attempt-scoped dial deadline is what must end it. Without that deadline the ctx handed to
// Dial is the worker's, which never fires mid-run, and the attempt hangs forever.
type blockingDialer struct{}

func (blockingDialer) Dial(ctx context.Context, _ execution.AttemptDescriptor) (execution.EngineChannel, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestBlockedDialFailsAttemptWithinDeadlineAndRetries proves H1's control-plane guarantee:
// a dial that never completes is not a silent hang but a classified attempt failure routed
// through the retry / dead-letter path. Each attempt's dial is bounded by the attempt-scoped
// deadline; the failure retries until the ceiling dead-letters the durable job. Without the
// deadline the first attempt blocks forever and the job never dead-letters — this test's
// awaitJobStatus times out (the RED).
func TestBlockedDialFailsAttemptWithinDeadlineAndRetries(t *testing.T) {
	h := newHarness(t)
	_, _, runID := h.admit()

	orch := h.newOrchestrator(blockingDialer{})
	orch.DialHandshakeDeadline = 400 * time.Millisecond // injected short bound
	stop := h.runWorkerWithRetry(orch, coordinator.RetryPolicy{
		MaxAttempts: 3, BaseBackoff: 5 * time.Millisecond, MaxBackoff: 20 * time.Millisecond,
	})
	defer stop()

	// Three bounded dials (~400ms each) plus small backoffs dead-letter the job well under
	// this budget; an unbounded dial never gets here.
	h.awaitJobStatus(runID, "dead", 15*time.Second)
	stop()

	// The ledger records one attempt per bounded-and-failed dial, proving the retries — not
	// a single hang — drained the ceiling.
	attempts := h.count(
		`SELECT count(*) FROM job_attempts WHERE job_id = (
			SELECT id FROM durable_jobs WHERE payload->>'run_id'=$1 AND organization_id=$2 AND project_id=$3)`,
		runID, h.tenant.Organization, h.tenant.Project)
	if attempts < 3 {
		t.Fatalf("recorded %d job attempts, want >= 3 (each blocked dial failed and retried)", attempts)
	}
}
