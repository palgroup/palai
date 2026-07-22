//go:build component

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/palgroup/palai/storage"

	"github.com/palgroup/palai/packages/coordinator"
)

// queueTimedOutProjection is a minimal timed_out Response body the test hands the coordinator; the
// production caller (the orchestrator) supplies its canonical RFC 9457 shape.
var queueTimedOutProjection = []byte(`{"output":[],"usage":{},"error":{"type":"https://docs.palai.dev/problems/operation_timed_out","title":"Operation timed out","status":504,"code":"operation_timed_out","detail":"the run exceeded its queue deadline","retryable":true}}`)

func runState(t *testing.T, cs *coordinator.Store, runID string) string {
	t.Helper()
	var state string
	if err := cs.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT state FROM runs WHERE id=$1`, runID).Scan(&state); err != nil {
		t.Fatalf("read run state: %v", err)
	}
	return state
}

func admitQueuedRun(t *testing.T, cs *coordinator.Store, tenant coordinator.Tenant, principal, key string) coordinator.AdmissionInput {
	t.Helper()
	in := admissionInput(principal, key, "h-"+key, `{"id":"resp"}`)
	if _, err := cs.AdmitResponse(context.Background(), tenant, in); err != nil {
		t.Fatalf("AdmitResponse(%s) error = %v", key, err)
	}
	return in
}

// TestTimeoutQueuedIfExpiredTimesOutBeforeCompute proves the §20.12 queue-deadline: a root run that
// waits in the queue past its deadline terminates as timed_out WITHOUT starting billable compute
// (no attempt is ever recorded), and its Response is finalized to timed_out. A run still within its
// deadline, and a run that has already left the queue (dispatched), are both left untouched.
func TestTimeoutQueuedIfExpiredTimesOutBeforeCompute(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	tenant, principalID := seedTenantWithKey(t, cs.Pool(), "qd-tok")

	// A fresh queued run within its deadline is not touched.
	fresh := admitQueuedRun(t, cs, tenant, principalID, "qd-fresh")
	if out, err := cs.TimeoutQueuedIfExpired(ctx, tenant, fresh.RunID, fresh.ResponseID, time.Hour, queueTimedOutProjection); err != nil || out {
		t.Fatalf("TimeoutQueuedIfExpired(fresh) = (%v, %v), want (false, nil)", out, err)
	}
	if s := runState(t, cs, fresh.RunID); s != "queued" {
		t.Fatalf("fresh run state = %q, want queued", s)
	}

	// A run that has waited past the deadline is timed out before compute.
	expired := admitQueuedRun(t, cs, tenant, principalID, "qd-expired")
	exec(t, cs.Pool(), `UPDATE runs SET created_at = clock_timestamp() - interval '10 seconds' WHERE id=$1`, expired.RunID)
	out, err := cs.TimeoutQueuedIfExpired(ctx, tenant, expired.RunID, expired.ResponseID, time.Second, queueTimedOutProjection)
	if err != nil {
		t.Fatalf("TimeoutQueuedIfExpired(expired) error = %v", err)
	}
	if !out {
		t.Fatal("TimeoutQueuedIfExpired(expired) = false, want true")
	}
	if s := runState(t, cs, expired.RunID); s != "timed_out" {
		t.Fatalf("expired run state = %q, want timed_out", s)
	}
	// The Response projection is finalized terminal, and the run.timed_out.v1 event was written.
	assertCount(t, cs.Pool(), 1, `SELECT count(*) FROM responses WHERE id=$1 AND state='timed_out'`, expired.ResponseID)
	assertCount(t, cs.Pool(), 1, `SELECT count(*) FROM events WHERE type='run.timed_out.v1' AND organization_id=$1 AND project_id=$2`, tenant.Organization, tenant.Project)
	// No billable compute started: no attempt was ever recorded for the timed-out run.
	assertCount(t, cs.Pool(), 0, `SELECT count(*) FROM attempts WHERE run_id=$1`, expired.RunID)

	// A run that already left the queue (dispatched to running) is past the pre-compute window: the
	// deadline path leaves it alone even when its created_at is old.
	dispatched := admitQueuedRun(t, cs, tenant, principalID, "qd-running")
	exec(t, cs.Pool(), `UPDATE runs SET state='running', created_at = clock_timestamp() - interval '10 seconds' WHERE id=$1`, dispatched.RunID)
	if out, err := cs.TimeoutQueuedIfExpired(ctx, tenant, dispatched.RunID, dispatched.ResponseID, time.Second, queueTimedOutProjection); err != nil || out {
		t.Fatalf("TimeoutQueuedIfExpired(running) = (%v, %v), want (false, nil)", out, err)
	}
	if s := runState(t, cs, dispatched.RunID); s != "running" {
		t.Fatalf("dispatched run state = %q, want running (untouched)", s)
	}

	// A non-positive deadline disables the check entirely.
	another := admitQueuedRun(t, cs, tenant, principalID, "qd-disabled")
	exec(t, cs.Pool(), `UPDATE runs SET created_at = clock_timestamp() - interval '10 seconds' WHERE id=$1`, another.RunID)
	if out, err := cs.TimeoutQueuedIfExpired(ctx, tenant, another.RunID, another.ResponseID, 0, queueTimedOutProjection); err != nil || out {
		t.Fatalf("TimeoutQueuedIfExpired(deadline=0) = (%v, %v), want (false, nil)", out, err)
	}
}
