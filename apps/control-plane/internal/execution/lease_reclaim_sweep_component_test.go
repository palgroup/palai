//go:build component

// The BACKGROUND half of the stuck-lease reclaim (Faz A.4). lease_reclaim_component_test.go proves the
// INLINE half: a second attempt on the same allocation reclaims a dead holder's lease. That half only
// fires when a second attempt arrives, so a thread whose last run ended and which nobody writes to again
// keeps its lease forever — and IdleWorkspacesForRelease, which wants `state = 'ready'`, can never see the
// workspace to hand its machine back. These drive LeaseReclaimer against a real spine: what it takes, what
// it refuses, and the end state the idle sweep needs it to leave behind.

package execution

import (
	"context"
	"testing"
	"time"

	"github.com/palgroup/palai/packages/coordinator"

	"github.com/palgroup/palai/storage"
)

// seedTerminalHolder gives the allocation an active writer lease held by a run in state `state` whose
// updated_at is `age` in the past — the row shape an ended thread leaves behind. Returns run and lease id.
func seedTerminalHolder(t *testing.T, cs *coordinator.Store, tenant coordinator.Tenant, alloc coordinator.Allocation, state string, age time.Duration) (string, string) {
	t.Helper()
	ctx := storage.WithTenant(context.Background(), tenant.Project)
	runID, leaseID := redeliveryID("run"), redeliveryID("lease")
	execSQL(t, cs.Pool(), `INSERT INTO runs (id, project_id, session_id, state)
		SELECT $1, $2, w.session_id, 'running' FROM workspaces w WHERE w.id = (SELECT workspace_id FROM workspace_allocations WHERE id=$3)`,
		runID, tenant.Project, alloc.ID)
	if err := cs.AcquireWriterLease(ctx, leaseID, alloc.ID, runID); err != nil {
		t.Fatalf("seed lease acquire error = %v", err)
	}
	// The terminal state and its age are set AFTER the lease, because AcquireWriterLease is the thing that
	// has to succeed and a terminal run is not what it is normally handed.
	execSQL(t, cs.Pool(), `UPDATE runs SET state = $2, updated_at = clock_timestamp() - ($3::bigint * interval '1 millisecond') WHERE id = $1`,
		runID, state, age.Milliseconds())
	return runID, leaseID
}

// seedLiveJobForRun gives an EXISTING run a claimed, unexpired response.run job, so RunHasLiveResponseJob
// reports it alive. Distinct from seedRunWithLiveJob, which mints the run and the lease as well; here the
// run must already be the terminal lease holder under test.
func seedLiveJobForRun(t *testing.T, cs *coordinator.Store, tenant coordinator.Tenant, runID string) {
	t.Helper()
	execSQL(t, cs.Pool(), `INSERT INTO durable_jobs (id, project_id, kind, status, lease_owner, lease_expires_at, payload)
		VALUES ($1, $2, 'response.run', 'running', $3, clock_timestamp() + interval '10 minutes', jsonb_build_object('run_id', $4::text))`,
		redeliveryID("job"), tenant.Project, redeliveryID("owner"), runID)
}

// leaseState reads a lease row's state directly — the durable record, not a helper's view of it.
func leaseState(t *testing.T, cs *coordinator.Store, leaseID string) string {
	t.Helper()
	var state string
	if err := cs.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT state FROM workspace_leases WHERE id = $1`, leaseID).Scan(&state); err != nil {
		t.Fatalf("read lease %s: %v", leaseID, err)
	}
	return state
}

// workspaceState reads a workspace row's state directly, for the same reason.
func workspaceState(t *testing.T, cs *coordinator.Store, wsID string) string {
	t.Helper()
	var state string
	if err := cs.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT state FROM workspaces WHERE id = $1`, wsID).Scan(&state); err != nil {
		t.Fatalf("read workspace %s: %v", wsID, err)
	}
	return state
}

// TestAbandonedLeaseSweepReturnsATerminalRunsWorkspaceToReady is the whole point: a workspace left
// `leased` by a run that COMPLETED, which no later attempt will ever visit, comes back to `ready` with its
// lease released — the exact pair of writes IdleWorkspacesForRelease requires before it will consider the
// workspace at all. Asserting both is asserting the handoff, not just the transition.
func TestAbandonedLeaseSweepReturnsATerminalRunsWorkspaceToReady(t *testing.T) {
	cs, tenant, wsID, alloc := openLeaseHarness(t)
	_, leaseID := seedTerminalHolder(t, cs, tenant, alloc, "completed", time.Hour)

	if got := workspaceState(t, cs, wsID); got != "leased" {
		t.Fatalf("seeded workspace state = %q, want leased", got)
	}

	reclaimed, err := NewLeaseReclaimer(cs, time.Minute).Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep() error = %v", err)
	}
	if reclaimed < 1 {
		t.Fatalf("Sweep() reclaimed %d, want at least the seeded workspace", reclaimed)
	}
	if got := leaseState(t, cs, leaseID); got != "released" {
		t.Fatalf("lease state = %q, want released", got)
	}
	if got := workspaceState(t, cs, wsID); got != "ready" {
		t.Fatalf("workspace state = %q, want ready", got)
	}
	// The property the idle sweep actually reads: `ready` AND no active lease. A workspace that reached
	// one without the other is invisible to it, which is the state this whole sweep exists to prevent.
	held, err := cs.WorkspaceHasActiveLease(storage.WithTenant(context.Background(), tenant.Project), tenant, wsID)
	if err != nil {
		t.Fatalf("WorkspaceHasActiveLease() error = %v", err)
	}
	if held {
		t.Fatal("workspace is ready but still carries an active lease, so the idle sweep will never see it")
	}
}

// TestAbandonedLeaseSweepLeavesARunningHoldersLeaseAlone is the safety claim, and it is the reason the
// candidate query keys on TERMINAL rather than on a TTL: a run that has not finished may be quiet for any
// length of time — parked for capacity, waiting on a human — and taking its workspace away would pull the
// directory out from under an attempt that is coming back to it.
func TestAbandonedLeaseSweepLeavesARunningHoldersLeaseAlone(t *testing.T) {
	cs, tenant, wsID, alloc := openLeaseHarness(t)
	_, leaseID := seedTerminalHolder(t, cs, tenant, alloc, "running", time.Hour)

	if _, err := NewLeaseReclaimer(cs, time.Minute).Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep() error = %v", err)
	}
	if got := leaseState(t, cs, leaseID); got != "active" {
		t.Fatalf("a RUNNING holder's lease state = %q, want it left active", got)
	}
	if got := workspaceState(t, cs, wsID); got != "leased" {
		t.Fatalf("a RUNNING holder's workspace state = %q, want it left leased", got)
	}
}

// TestAbandonedLeaseSweepWaitsOutTheGraceAfterTerminal covers the window terminality alone does not: the
// run row commits its terminal transition BEFORE ExecuteAttempt's deferred releaseWorkspace has run, so
// for the length of an attempt's unwind a terminal run's lease is still held by a live process. A run that
// reached terminal a moment ago is not a candidate; the same run, older than the grace, is.
func TestAbandonedLeaseSweepWaitsOutTheGraceAfterTerminal(t *testing.T) {
	cs, tenant, wsID, alloc := openLeaseHarness(t)
	runID, leaseID := seedTerminalHolder(t, cs, tenant, alloc, "completed", 0)

	if _, err := NewLeaseReclaimer(cs, time.Hour).Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep() error = %v", err)
	}
	if got := leaseState(t, cs, leaseID); got != "active" {
		t.Fatalf("a just-terminal holder's lease state = %q, want it left active until the grace elapses", got)
	}

	// Age the same run past the grace and it becomes the candidate it was not a moment ago. Nothing else
	// about the row changes, so the grace is provably the only thing that decided it.
	execSQL(t, cs.Pool(), `UPDATE runs SET updated_at = clock_timestamp() - interval '2 hours' WHERE id = $1`, runID)
	if _, err := NewLeaseReclaimer(cs, time.Hour).Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep() after aging error = %v", err)
	}
	if got := leaseState(t, cs, leaseID); got != "released" {
		t.Fatalf("lease state after the grace elapsed = %q, want released", got)
	}
	if got := workspaceState(t, cs, wsID); got != "ready" {
		t.Fatalf("workspace state after the grace elapsed = %q, want ready", got)
	}
}

// TestAbandonedLeaseSweepNeverTakesALeaseWhoseJobIsStillLive is the second half of that same window, and
// it is not redundant with the grace: a redelivered attempt on a terminal run holds a claimed response.run
// job for as long as it takes to unwind, which can outlast any grace an operator picks. This is the
// liveness proof acquireWriterLease already reclaims on, applied to the sweep.
func TestAbandonedLeaseSweepNeverTakesALeaseWhoseJobIsStillLive(t *testing.T) {
	cs, tenant, wsID, alloc := openLeaseHarness(t)
	runID, leaseID := seedTerminalHolder(t, cs, tenant, alloc, "completed", time.Hour)
	seedLiveJobForRun(t, cs, tenant, runID)

	if _, err := NewLeaseReclaimer(cs, time.Minute).Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep() error = %v", err)
	}
	if got := leaseState(t, cs, leaseID); got != "active" {
		t.Fatalf("lease state with a LIVE response.run job = %q, want it left active", got)
	}
	if got := workspaceState(t, cs, wsID); got != "leased" {
		t.Fatalf("workspace state with a LIVE response.run job = %q, want it left leased", got)
	}
}
