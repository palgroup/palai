//go:build component

package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/palgroup/palai/packages/coordinator"

	"github.com/palgroup/palai/storage"
)

// seedWorkspace opens a logical workspace bound to the session/run and mints its first physical
// allocation. It returns the stable logical workspace id and that allocation.
func seedWorkspace(t *testing.T, cs *coordinator.Store, tenant coordinator.Tenant, sessionID, runID string) (string, coordinator.Allocation) {
	t.Helper()
	// AllocateWorkspace is keyed by the opaque workspace id rather than a tenant, so the CONTEXT
	// carries the scope — the same way the run worker scopes a claimed job (migration 000029).
	ctx := storage.WithTenant(context.Background(), tenant.Organization, tenant.Project)
	wsID := newID("wsp")
	if err := cs.CreateWorkspace(ctx, tenant, coordinator.WorkspaceInput{
		WorkspaceID: wsID, SessionID: sessionID, RunID: runID, State: "ready",
	}); err != nil {
		t.Fatalf("CreateWorkspace() error = %v", err)
	}
	alloc, err := cs.AllocateWorkspace(ctx, newID("wal"), wsID, "/host/ws/"+wsID)
	if err != nil {
		t.Fatalf("AllocateWorkspace() error = %v", err)
	}
	return wsID, alloc
}

// TestWorkspacesMigration proves 000008 adds its four workspace tables and the single-writer
// partial index idempotently and reverses cleanly: present after apply (a re-apply is a clean
// no-op), gone after rollback, and back after reapply (spec §29.7-29.10; the 000005/000007
// re-run-safety pattern).
func TestWorkspacesMigration(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()

	tables := []string{"workspaces", "workspace_allocations", "workspace_leases", "workspace_snapshots"}

	// Present after apply, and a second Migrate is a clean no-op (CREATE ... IF NOT EXISTS).
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	for _, tbl := range tables {
		if !tableExists(t, pool, tbl) {
			t.Fatalf("after apply, %s is missing", tbl)
		}
	}
	if !indexExists(t, pool, "workspace_leases_one_active_writer") {
		t.Fatal("after apply, workspace_leases_one_active_writer is missing")
	}

	if err := cs.Rollback(ctx); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	for _, tbl := range tables {
		if tableExists(t, pool, tbl) {
			t.Fatalf("after rollback, %s still exists", tbl)
		}
	}

	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate() error = %v", err)
	}
	for _, tbl := range tables {
		if !tableExists(t, pool, tbl) {
			t.Fatalf("after reapply, %s is missing", tbl)
		}
	}
	if !indexExists(t, pool, "workspace_leases_one_active_writer") {
		t.Fatal("after reapply, workspace_leases_one_active_writer is missing")
	}
}

// TestSingleWriterLeaseRejectsSecondWriter proves single-writer ownership is a DB constraint, not
// an app-code race (spec §29.8): a mutable workspace holds at most one active writer lease, so a
// second concurrent active lease is a unique_violation (23505) at the partial unique index — the
// LP Task 3 / active-attempt-fence SQLSTATE pattern. The slot frees on release.
func TestSingleWriterLeaseRejectsSecondWriter(t *testing.T) {
	cs := openHarness(t)
	pool := cs.Pool()
	ctx := context.Background()
	tenant, sessionID, rootRun := seedRun(t, pool)
	// AllocateWorkspace / GetPreparationReceipt are keyed by an opaque id, not by a tenant, so under
	// migration 000029 the CONTEXT is what scopes them — the same way the run worker scopes a claimed
	// job. Declaring it here is what a production caller already does.
	ctx = storage.WithTenant(ctx, tenant.Organization, tenant.Project)
	wsID, alloc := seedWorkspace(t, cs, tenant, sessionID, rootRun)

	// The root run normally owns the single writer lease (spec §29.8).
	leaseA := newID("wls")
	if err := cs.AcquireWriterLease(ctx, leaseA, alloc.ID, rootRun); err != nil {
		t.Fatalf("first writer lease error = %v", err)
	}

	// A second writer — a child run of the root, which the one-active-root index admits — is
	// rejected: the workspace already has an active writer lease.
	childRun := newID("run")
	exec(t, pool,
		`INSERT INTO runs (id, organization_id, project_id, session_id, state, parent_run_id, depth)
		 VALUES ($1, $2, $3, $4, 'running', $5, 1)`,
		childRun, tenant.Organization, tenant.Project, sessionID, rootRun)
	if err := cs.AcquireWriterLease(ctx, newID("wls"), alloc.ID, childRun); !errors.Is(err, coordinator.ErrWriterLeaseHeld) {
		t.Fatalf("second writer lease = %v, want ErrWriterLeaseHeld", err)
	}

	// The reject is at the SQLSTATE level: a raw second active-lease insert is 23505, so the
	// single-writer invariant is the partial unique index, not an app check-then-insert race.
	_, rawErr := pool.Exec(storage.WithSystemScope(ctx),
		`INSERT INTO workspace_leases (id, workspace_id, allocation_id, organization_id, project_id, run_id, state, fence)
		 VALUES ($1, $2, $3, $4, $5, $6, 'active', $7)`,
		newID("wls"), wsID, alloc.ID, tenant.Organization, tenant.Project, childRun, alloc.Fence)
	if got := pgCode(rawErr); got != "23505" {
		t.Fatalf("raw second active lease code = %q, want 23505 unique_violation", got)
	}

	// Releasing the holder frees the single-writer slot: the next writer may take it.
	if err := cs.ReleaseWriterLease(ctx, leaseA); err != nil {
		t.Fatalf("release writer lease error = %v", err)
	}
	if err := cs.AcquireWriterLease(ctx, newID("wls"), alloc.ID, childRun); err != nil {
		t.Fatalf("writer lease after release error = %v", err)
	}
}

// TestAllocationCarriesFencingTokenStableLogicalID proves the §29.7 last paragraph: the logical
// workspace id is stable across host movement, while each physical allocation carries its own
// distinct allocation id and a strictly higher fencing token.
func TestAllocationCarriesFencingTokenStableLogicalID(t *testing.T) {
	cs := openHarness(t)
	pool := cs.Pool()
	ctx := context.Background()
	tenant, sessionID, runID := seedRun(t, pool)
	// AllocateWorkspace / GetPreparationReceipt are keyed by an opaque id, not by a tenant, so under
	// migration 000029 the CONTEXT is what scopes them — the same way the run worker scopes a claimed
	// job. Declaring it here is what a production caller already does.
	ctx = storage.WithTenant(ctx, tenant.Organization, tenant.Project)

	wsID := newID("wsp")
	if err := cs.CreateWorkspace(ctx, tenant, coordinator.WorkspaceInput{
		WorkspaceID: wsID, SessionID: sessionID, RunID: runID, State: "ready",
	}); err != nil {
		t.Fatalf("CreateWorkspace() error = %v", err)
	}

	a1, err := cs.AllocateWorkspace(ctx, newID("wal"), wsID, "/host/a1")
	if err != nil {
		t.Fatalf("first AllocateWorkspace() error = %v", err)
	}
	// Simulate a host move: a fresh physical allocation of the SAME logical workspace.
	a2, err := cs.AllocateWorkspace(ctx, newID("wal"), wsID, "/host/a2")
	if err != nil {
		t.Fatalf("second AllocateWorkspace() error = %v", err)
	}

	if a1.ID == a2.ID {
		t.Fatalf("allocation ids must differ across a host move: both %s", a1.ID)
	}
	if a2.Fence <= a1.Fence {
		t.Fatalf("fencing token must strictly increase: a1=%d a2=%d", a1.Fence, a2.Fence)
	}

	// The current allocation is the higher-fence one, under the unchanged logical workspace id.
	cur, err := cs.CurrentAllocation(ctx, wsID)
	if err != nil {
		t.Fatalf("CurrentAllocation() error = %v", err)
	}
	if cur.ID != a2.ID || cur.Fence != a2.Fence {
		t.Fatalf("current allocation = %s (fence %d), want the max-fence %s (fence %d)", cur.ID, cur.Fence, a2.ID, a2.Fence)
	}

	// Both allocations belong to the one stable logical workspace.
	var count int
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT count(*) FROM workspace_allocations WHERE workspace_id = $1`, wsID).Scan(&count); err != nil {
		t.Fatalf("count allocations error = %v", err)
	}
	if count != 2 {
		t.Fatalf("allocations under logical workspace %s = %d, want 2", wsID, count)
	}
}

// TestFencedStaleWriterSnapshotRejected proves the SAN-006 fence half (spec §29.8 line 3070): once
// fencing advances, a stale allocation can no longer upload an authoritative snapshot. The guarded
// insert affects zero rows for a non-current allocation, so the reject is at the DB, not app code.
// The real host-kill fault that advances the fence is E10; here the fence advance is a deterministic
// new allocation.
func TestFencedStaleWriterSnapshotRejected(t *testing.T) {
	cs := openHarness(t)
	pool := cs.Pool()
	ctx := context.Background()
	tenant, sessionID, runID := seedRun(t, pool)
	// AllocateWorkspace / GetPreparationReceipt are keyed by an opaque id, not by a tenant, so under
	// migration 000029 the CONTEXT is what scopes them — the same way the run worker scopes a claimed
	// job. Declaring it here is what a production caller already does.
	ctx = storage.WithTenant(ctx, tenant.Organization, tenant.Project)
	wsID := newID("wsp")
	if err := cs.CreateWorkspace(ctx, tenant, coordinator.WorkspaceInput{
		WorkspaceID: wsID, SessionID: sessionID, RunID: runID, State: "ready",
	}); err != nil {
		t.Fatalf("CreateWorkspace() error = %v", err)
	}
	a1, err := cs.AllocateWorkspace(ctx, newID("wal"), wsID, "/host/a1")
	if err != nil {
		t.Fatalf("first AllocateWorkspace() error = %v", err)
	}

	// The current allocation may record an authoritative snapshot.
	if err := cs.CreateWorkspaceSnapshot(ctx, coordinator.SnapshotInput{
		SnapshotID: newID("wss"), AllocationID: a1.ID, TreeChecksum: "sha256:tree1",
	}); err != nil {
		t.Fatalf("current-allocation snapshot error = %v", err)
	}

	// Fencing advances: a host move mints a2 with a strictly higher fence.
	a2, err := cs.AllocateWorkspace(ctx, newID("wal"), wsID, "/host/a2")
	if err != nil {
		t.Fatalf("second AllocateWorkspace() error = %v", err)
	}

	// The stale a1 can no longer upload an authoritative snapshot (SAN-006).
	if err := cs.CreateWorkspaceSnapshot(ctx, coordinator.SnapshotInput{
		SnapshotID: newID("wss"), AllocationID: a1.ID, TreeChecksum: "sha256:stale",
	}); !errors.Is(err, coordinator.ErrStaleAllocation) {
		t.Fatalf("stale-allocation snapshot = %v, want ErrStaleAllocation (rejected at DB)", err)
	}

	// The new current allocation still can.
	if err := cs.CreateWorkspaceSnapshot(ctx, coordinator.SnapshotInput{
		SnapshotID: newID("wss"), AllocationID: a2.ID, TreeChecksum: "sha256:tree2",
	}); err != nil {
		t.Fatalf("current a2 snapshot error = %v", err)
	}

	// No stale row landed: exactly the two authoritative snapshots persist.
	var count int
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT count(*) FROM workspace_snapshots WHERE workspace_id = $1`, wsID).Scan(&count); err != nil {
		t.Fatalf("count snapshots error = %v", err)
	}
	if count != 2 {
		t.Fatalf("persisted snapshots = %d, want 2 (the stale upload was rejected)", count)
	}
}

// TestAcquireWriterLeaseRejectsNonCurrentAllocation proves the writer-lease fence-currency guard
// (spec §29.8): once a host move advances the fence, a writer lease on the superseded allocation is
// rejected at the DB — a fenced-out writer cannot re-acquire authority. The current allocation still
// leases. This is the lease counterpart of the snapshot fence guard, landed now that the real tool
// round-trip acquires a writer lease (E09 Task 4).
func TestAcquireWriterLeaseRejectsNonCurrentAllocation(t *testing.T) {
	cs := openHarness(t)
	pool := cs.Pool()
	ctx := context.Background()
	tenant, sessionID, runID := seedRun(t, pool)
	// AllocateWorkspace / GetPreparationReceipt are keyed by an opaque id, not by a tenant, so under
	// migration 000029 the CONTEXT is what scopes them — the same way the run worker scopes a claimed
	// job. Declaring it here is what a production caller already does.
	ctx = storage.WithTenant(ctx, tenant.Organization, tenant.Project)
	wsID := newID("wsp")
	if err := cs.CreateWorkspace(ctx, tenant, coordinator.WorkspaceInput{
		WorkspaceID: wsID, SessionID: sessionID, RunID: runID, State: "ready",
	}); err != nil {
		t.Fatalf("CreateWorkspace() error = %v", err)
	}
	a1, err := cs.AllocateWorkspace(ctx, newID("wal"), wsID, "/host/a1")
	if err != nil {
		t.Fatalf("first AllocateWorkspace() error = %v", err)
	}
	// A host move advances the fence: a2 supersedes a1.
	a2, err := cs.AllocateWorkspace(ctx, newID("wal"), wsID, "/host/a2")
	if err != nil {
		t.Fatalf("second AllocateWorkspace() error = %v", err)
	}

	// The stale a1 can no longer take a writer lease.
	if err := cs.AcquireWriterLease(ctx, newID("wls"), a1.ID, runID); !errors.Is(err, coordinator.ErrStaleAllocation) {
		t.Fatalf("stale-allocation writer lease = %v, want ErrStaleAllocation (rejected at DB)", err)
	}
	// The current a2 still can.
	if err := cs.AcquireWriterLease(ctx, newID("wls"), a2.ID, runID); err != nil {
		t.Fatalf("current-allocation writer lease error = %v", err)
	}
}
