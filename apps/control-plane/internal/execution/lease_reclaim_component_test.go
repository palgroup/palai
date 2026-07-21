//go:build component

// E09 Task 10 devir proofs (spec §29.8, E10 Task 6): a hard crash leaves a TTL-less active writer
// lease that today bricks every later attempt, and a run cut mid-provision must recover rather than
// stick in a half-state. White-box, against a real spine (no object store needed) — the postgres tier.

package execution

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/palgroup/palai/packages/coordinator"
	statemachines "github.com/palgroup/palai/packages/state-machines"
)

// leaseHarness is a real spine + a seeded workspace with one allocation. It returns the store, tenant,
// workspace, and allocation the lease tests operate on.
func openLeaseHarness(t *testing.T) (*coordinator.Store, coordinator.Tenant, string, coordinator.Allocation) {
	t.Helper()
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-component TEST=postgres")
	}
	ctx := context.Background()
	cs, err := coordinator.Open(ctx, url)
	if err != nil {
		t.Fatalf("coordinator.Open() error = %v", err)
	}
	t.Cleanup(cs.Close)
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	tenant := coordinator.Tenant{Organization: redeliveryID("org"), Project: redeliveryID("prj")}
	sessionID := redeliveryID("ses")
	pool := cs.Pool()
	execSQL(t, pool, `INSERT INTO organizations (id) VALUES ($1)`, tenant.Organization)
	execSQL(t, pool, `INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, tenant.Project, tenant.Organization)
	execSQL(t, pool, `INSERT INTO sessions (id, organization_id, project_id) VALUES ($1, $2, $3)`, sessionID, tenant.Organization, tenant.Project)
	wsID := redeliveryID("wsp")
	if err := cs.CreateWorkspace(ctx, tenant, coordinator.WorkspaceInput{WorkspaceID: wsID, SessionID: sessionID, State: "leased"}); err != nil {
		t.Fatalf("CreateWorkspace() error = %v", err)
	}
	alloc, err := cs.AllocateWorkspace(ctx, redeliveryID("wal"), wsID, t.TempDir())
	if err != nil {
		t.Fatalf("AllocateWorkspace() error = %v", err)
	}
	return cs, tenant, wsID, alloc
}

// seedRunHoldingLease creates a run and gives it the active writer lease on the allocation — the state a
// crashed attempt leaves behind (an active lease with no live process). Returns the run id.
func seedRunHoldingLease(t *testing.T, cs *coordinator.Store, tenant coordinator.Tenant, alloc coordinator.Allocation) string {
	t.Helper()
	ctx := context.Background()
	runID := redeliveryID("run")
	execSQL(t, cs.Pool(), `INSERT INTO runs (id, organization_id, project_id, session_id, state)
		SELECT $1, $2, $3, w.session_id, 'running' FROM workspaces w WHERE w.id = (SELECT workspace_id FROM workspace_allocations WHERE id=$4)`,
		runID, tenant.Organization, tenant.Project, alloc.ID)
	if err := cs.AcquireWriterLease(ctx, redeliveryID("lease"), alloc.ID, runID); err != nil {
		t.Fatalf("seed lease acquire error = %v", err)
	}
	return runID
}

// TestStuckWriterLeaseReclaimedAfterCrash: a crash left an active lease held by a DEAD run (no live
// response.run job). A new attempt reclaims it and acquires its own lease. NEGATIVE: when the holder is
// LIVE (a claimed response.run job), the lease is NEVER stolen — the single-writer invariant holds.
func TestStuckWriterLeaseReclaimedAfterCrash(t *testing.T) {
	cs, tenant, _, alloc := openLeaseHarness(t)
	ctx := context.Background()
	o := &Orchestrator{spine: cs}

	seedRunHoldingLease(t, cs, tenant, alloc)
	newRun := freshRun(t, cs, tenant)

	// The dead holder has NO live response.run job, so the reclaim releases its lease and re-acquires.
	leaseID, err := o.acquireWriterLease(ctx, tenant, alloc.ID, newRun, "")
	if err != nil {
		t.Fatalf("acquireWriterLease() error = %v, want a reclaimed lease", err)
	}
	if leaseID == "" {
		t.Fatal("acquireWriterLease returned no lease id after a valid reclaim")
	}
	holder, found, err := cs.WorkspaceLeaseHolder(ctx, alloc.ID)
	if err != nil || !found {
		t.Fatalf("WorkspaceLeaseHolder() = found %v err %v", found, err)
	}
	if holder.RunID != newRun {
		t.Fatalf("active lease holder = %q, want the reclaiming run %q", holder.RunID, newRun)
	}

	// NEGATIVE: a LIVE holder's lease is never stolen. Give a fresh holder a live claimed response.run
	// job, then a competing acquire must fail with ErrWriterLeaseHeld (not reclaim).
	_, _, _, alloc2 := openLeaseHarness(t)
	seedRunWithLiveJob(t, cs, tenant, alloc2)
	competitor := freshRun(t, cs, tenant)
	if _, err := o.acquireWriterLease(ctx, tenant, alloc2.ID, competitor, ""); err != coordinator.ErrWriterLeaseHeld {
		t.Fatalf("acquireWriterLease against a LIVE holder = %v, want ErrWriterLeaseHeld (never steal)", err)
	}
}

// freshRun creates a run in its OWN new session (so it is not a second active root in an existing
// session) and returns its id — the reclaiming attempt in the lease tests.
func freshRun(t *testing.T, cs *coordinator.Store, tenant coordinator.Tenant) string {
	t.Helper()
	sessionID, runID := redeliveryID("ses"), redeliveryID("run")
	execSQL(t, cs.Pool(), `INSERT INTO sessions (id, organization_id, project_id) VALUES ($1, $2, $3)`, sessionID, tenant.Organization, tenant.Project)
	execSQL(t, cs.Pool(), `INSERT INTO runs (id, organization_id, project_id, session_id, state) VALUES ($1, $2, $3, $4, 'running')`, runID, tenant.Organization, tenant.Project, sessionID)
	return runID
}

// seedRunWithLiveJob creates a run holding the allocation's lease AND a live (claimed, unexpired)
// response.run durable job whose payload names the run, so RunHasLiveResponseJob reports it alive — a
// writer that must never be reclaimed.
func seedRunWithLiveJob(t *testing.T, cs *coordinator.Store, tenant coordinator.Tenant, alloc coordinator.Allocation) string {
	t.Helper()
	runID := seedRunHoldingLease(t, cs, tenant, alloc)
	execSQL(t, cs.Pool(), `INSERT INTO durable_jobs (id, organization_id, project_id, kind, status, lease_owner, lease_expires_at, payload)
		VALUES ($1, $2, $3, 'response.run', 'running', $4, clock_timestamp() + interval '10 minutes', jsonb_build_object('run_id', $5::text))`,
		redeliveryID("job"), tenant.Organization, tenant.Project, redeliveryID("owner"), runID)
	return runID
}

// TestProvisioningInterruptedMidStateRecovers: a workspace left in a mid-provisioning state by a crash
// re-enters the lifecycle idempotently on the next attempt and reaches ready — it does not stick in the
// half-state (the E09 T10 blocker-2 re-entry, pinned here at the SM level; the full clone-restart is the
// gated fault-live half). Every mid-state (requested/provisioning/preparing) recovers to ready.
func TestProvisioningInterruptedMidStateRecovers(t *testing.T) {
	cs, tenant, _, _ := openLeaseHarness(t)
	ctx := context.Background()

	for _, stuck := range []string{"requested", "provisioning", "preparing"} {
		sessionID := redeliveryID("ses")
		execSQL(t, cs.Pool(), `INSERT INTO sessions (id, organization_id, project_id) VALUES ($1, $2, $3)`, sessionID, tenant.Organization, tenant.Project)
		wsID := redeliveryID("wsp")
		if err := cs.CreateWorkspace(ctx, tenant, coordinator.WorkspaceInput{WorkspaceID: wsID, SessionID: sessionID, State: stuck}); err != nil {
			t.Fatalf("CreateWorkspace(%s) error = %v", stuck, err)
		}
		// The next attempt re-drives the lifecycle idempotently — an already-applied transition is
		// tolerated (ErrInvalidState), exactly as provisionFreshAllocation does around the clone.
		for _, cmd := range []statemachines.WorkspaceCommand{statemachines.WorkspaceCmdProvision, statemachines.WorkspaceCmdPrepare, statemachines.WorkspaceCmdMarkReady} {
			if err := cs.AdvanceWorkspace(ctx, tenant, wsID, cmd); err != nil && !errors.Is(err, statemachines.ErrInvalidState) {
				t.Fatalf("re-enter %s from %s: %v", cmd, stuck, err)
			}
		}
		var state string
		if err := cs.Pool().QueryRow(ctx, `SELECT state FROM workspaces WHERE id=$1`, wsID).Scan(&state); err != nil {
			t.Fatalf("read state: %v", err)
		}
		if state != "ready" {
			t.Fatalf("workspace stuck at %q from mid-state %q — did not recover to ready", state, stuck)
		}
	}
}
