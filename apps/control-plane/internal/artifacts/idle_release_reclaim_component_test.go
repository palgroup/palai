//go:build component

// THE RELEASE PATH'S ONE-WAY DOOR (Faz A.4, closing the §2b gate).
//
// release() hands the machine back BEFORE it settles the hold: the workspace moves to `paused` at
// idle_release.go:183 and the meter runs at idle_release.go:224. That order is deliberate and stays — a
// step that can fail must not abort a release that already happened — but until this file it made the
// failure PERMANENT, because every route back to an open hold runs through a workspace the pause has
// already moved out of reach:
//
//	IdleWorkspacesForRelease  selects `state = 'ready'` (storage/queries/workspaces.sql:258), so a paused
//	                          workspace is never a candidate again.
//	LeaseReclaimer            sweeps dangling WRITER leases (workspace_leases), not occupancies. Different
//	                          table, different question.
//	ReleaseLease              has no production caller, deliberately: a hold must not close unbilled.
//
// So one Mac-slot leaked per failed settle, permanently, and machine_occupancy.go:70-74 already named that
// as the reason a declared capacity would start refusing real work for a reason nobody could see.
//
// THE TWO TESTS WATCH DIFFERENT PROPERTIES AND THE SECOND IS NOT IMPLIED BY THE FIRST. Recoverability says
// a stranded hold can still be closed; exactly-once says making it reclaimable did not make it billable
// twice. An implementation that settles every paused workspace's session — rather than every OPEN hold —
// passes the first and bills the customer twice.

package artifacts

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/packages/coordinator"

	"github.com/palgroup/palai/storage"
)

// strandByCancellingTheSettle drives a REAL settle failure through the REAL release path, and takes no
// shortcut through the fixture.
//
// WHY IT IS NOT HAND-BUILT. Writing `UPDATE workspaces SET state='paused'` and leaving the hold open would
// construct the state this file is about without ever proving the release path produces it — the shape this
// tree keeps recording as a test measuring a world its own setup made up. Instead it uses the window that
// exists in production: release() calls r.remove BETWEEN the pause (idle_release.go:183) and the settle
// (idle_release.go:224), so a teardown that cancels the sweep's context reproduces exactly a shutdown, a
// dropped connection, or any other context death landing in that window. The pause is committed, the bytes
// are gone, settleOccupancy's very first read fails, and release returns (true, billErr).
//
// It returns with the premise ASSERTED rather than assumed: every claim below it is about a machine that
// really was handed back with a hold that really is still open.
func (h *artifactsHarness) strandByCancellingTheSettle(t *testing.T) (tenant coordinator.Tenant, workspaceID, leaseID string) {
	t.Helper()
	tenant, _, workspaceID, _, leaseID = h.seedOccupiedIdleWorkspace(t)

	swept, cancel := context.WithCancel(context.Background())
	defer cancel()
	releaser := execution.NewIdleReleaser(h.repo.Spine(), execution.NewSnapshotSink(h.s3, h.repo.Spine()), idleTestTTL)
	releaser.SetTeardown(func(path string) error {
		cancel()
		return os.RemoveAll(path)
	})
	// The sweep is cross-tenant and its error is the FIRST failing candidate's, which may be another test's.
	// Nothing is asserted about the return; everything is asserted about this workspace's own rows.
	_, _ = releaser.Sweep(swept)

	if state := workspaceState(t, h, workspaceID); state != "paused" {
		t.Fatalf("workspace state = %q, want paused — the release did not stand, so nothing below is about a machine that was handed back", state)
	}
	if held := occupancyOf(t, h, tenant, leaseID); !held.ReleasedAt.IsZero() {
		t.Fatalf("the hold closed at %s — the settle did NOT fail, so this test's premise is gone and every assertion below would hold for the wrong reason",
			held.ReleasedAt)
	}
	return tenant, workspaceID, leaseID
}

// TestASettleFailureLeavesTheOccupancyReclaimable — THE MACHINE WENT BACK, SO SOMETHING MUST STILL BE ABLE
// TO CLOSE THE HOLD.
//
// This is the gate palai-cloud's devredilen-isler §2b holds capacity declaration behind. The release stood:
// the archive exists, the workspace is `paused`, the directory is gone, the machine is free. Only the meter
// did not run. A later tick must be able to reach that hold and settle it, or the slot is occupied forever
// by a session that is not on the machine.
func TestASettleFailureLeavesTheOccupancyReclaimable(t *testing.T) {
	h := openArtifactsHarness(t)
	tenant, _, leaseID := h.strandByCancellingTheSettle(t)

	// A LATER TICK. A fresh releaser with a live context, exactly as the maintenance goroutine builds one
	// every interval — not a special recovery entry point a deployment would have to be told to run.
	later := execution.NewIdleReleaser(h.repo.Spine(), execution.NewSnapshotSink(h.s3, h.repo.Spine()), idleTestTTL)
	_, _ = later.Sweep(context.Background())

	held := occupancyOf(t, h, tenant, leaseID)
	if held.ReleasedAt.IsZero() {
		t.Fatal("the machine was handed back and its hold is STILL open after a later sweep — that slot is occupied forever by a session that is not on the machine, and AcquireLease's ceiling counts it (storage/queries/leases.sql, `released_at IS NULL`)")
	}
	// THE BILL IS THE ONE THE TIMELY SETTLE WOULD HAVE PRODUCED. 'idle' is the one word the interval branches
	// on (GetOccupancy's CASE): it stops the bill at last_activity_at, so a hold settled a tick late costs the
	// customer exactly what a hold settled on time costs. Closing it under any other reason would charge the
	// quiet tail plus however long the recovery took to notice.
	if held.ReleaseReason != "idle" {
		t.Fatalf("release reason = %q, want idle — a recovered hold must bill to last_activity_at, or the customer pays for the tail AND for the time nobody noticed", held.ReleaseReason)
	}
	// NON-VACUITY: settleUsage skips a zero quantity, so "1 row" below would be unreachable for reasons that
	// have nothing to do with recovery if the hold billed nothing.
	if held.Billed < billingHold {
		t.Fatalf("the closed hold bills %s, less than the %s it was held for — a zero-length hold settles no row, so the count below would be about the fixture", held.Billed, billingHold)
	}
	rows := h.settledMachineMinutes(t, tenant.Project)
	if len(rows) != 1 {
		t.Fatalf("%d machine.minutes row(s) after a recovered hold, want 1 — reclaimable means SETTLED, not merely closed; a hold closed without a ledger row is machine time nobody is charged for", len(rows))
	}
	settled := time.Duration(rows[0] * float64(time.Minute))
	if want := held.Billed; settled < want-time.Millisecond || settled > want+time.Millisecond {
		t.Fatalf("settled %s, the occupancy says %s — the amount charged and the amount derived are two answers to one question", settled, want)
	}
}

// TestAReclaimedOccupancySettlesExactlyOnce — RECLAIMABLE MUST NOT MEAN BILLABLE TWICE.
//
// IT IS NOT THE FIRST TEST RESTATED, and the difference is which closer produces the row. Here the release
// path's own settle LANDS — the same coordinator.SettleOccupancy call idle_release.go:280 makes, arriving
// on a retry — and the recovery sweep then runs over the very same paused workspace. A recovery keyed on
// "this workspace is paused, settle its session's hold" rather than on "this hold is OPEN" would find the
// workspace, settle again, and charge the customer twice; it would pass the first test with that bug
// intact. So this test is green before the recovery exists, on purpose: what it guards is the recovery
// being ADDED without a second bill.
//
// It drives BOTH of the guards that exist for exactly this. The write-once UPDATE (`released_at IS NULL` in
// storage/queries/leases.sql ReleaseLease) is driven by the repeated settle below, which must report that it
// closed nothing. The ledger's dedupe key covers the other caller — two paths racing to close one hold,
// where both UPDATEs can see an open row — and it cannot be driven from here, so it is asserted directly:
// the stored key must be the OCCUPANCY's own id, because a key naming anything else (the session, the
// sweep's tick) would collide on the wrong thing or not at all.
func TestAReclaimedOccupancySettlesExactlyOnce(t *testing.T) {
	h := openArtifactsHarness(t)
	ctx := context.Background()
	tenant, workspaceID, leaseID := h.strandByCancellingTheSettle(t)

	// THE RELEASE PATH'S SETTLE FINALLY LANDS. This is idle_release.go:280's call, with its constant.
	closed, err := h.repo.Spine().SettleOccupancy(ctx, tenant, leaseID, coordinator.ReleaseReasonIdle)
	if err != nil {
		t.Fatalf("SettleOccupancy() error = %v", err)
	}
	if !closed {
		t.Fatal("SettleOccupancy() reported it closed nothing on an OPEN hold — the premise of everything below is gone")
	}
	if rows := h.settledMachineMinutes(t, tenant.Project); len(rows) != 1 {
		t.Fatalf("%d machine.minutes row(s) after the first settle, want 1", len(rows))
	}
	first := occupancyOf(t, h, tenant, leaseID)

	// THE RECOVERY SWEEP NOW RUNS OVER A PAUSED WORKSPACE WHOSE HOLD IS ALREADY SETTLED. Asserted rather than
	// assumed, because if the workspace were not paused the sweep would have nothing to be tempted by and this
	// test would pass without ever reaching the bug it exists for.
	if state := workspaceState(t, h, workspaceID); state != "paused" {
		t.Fatalf("workspace state = %q, want paused — the recovery sweep below would not consider it, so the double-bill this test guards could not occur either way", state)
	}
	later := execution.NewIdleReleaser(h.repo.Spine(), execution.NewSnapshotSink(h.s3, h.repo.Spine()), idleTestTTL)
	_, _ = later.Sweep(ctx)

	// WRITE-ONCE: a repeated settle must close nothing and must not move the bill.
	again, err := h.repo.Spine().SettleOccupancy(ctx, tenant, leaseID, coordinator.ReleaseReasonIdle)
	if err != nil {
		t.Fatalf("repeated SettleOccupancy() error = %v — a repeat is ordinary (a retried tick, two closers meeting) and must not be a failure", err)
	}
	if again {
		t.Fatal("a repeated SettleOccupancy() reported it closed the hold a second time — released_at moved, which EXTENDS a bill that was already settled")
	}

	after := occupancyOf(t, h, tenant, leaseID)
	if !after.ReleasedAt.Equal(first.ReleasedAt) {
		t.Fatalf("released_at moved from %s to %s — the hold was re-closed, and every later reading of this bill is of the second close", first.ReleasedAt, after.ReleasedAt)
	}
	rows := h.settledMachineMinutes(t, tenant.Project)
	if len(rows) != 1 {
		t.Fatalf("%d machine.minutes row(s) for one hold, want 1 — the customer is charged twice for one occupancy of one machine (%v)", len(rows), rows)
	}

	// THE SECOND GUARD, ARMED. The dedupe key is what stops two racing closers — a case the repeated call
	// above cannot reach, because there the UPDATE has already excluded the second one. It only works if the
	// key names the OCCUPANCY.
	if key := h.machineMinutesDedupeKey(t, tenant.Project); key != "lease:"+leaseID+":machine.minutes" {
		t.Fatalf("usage_ledger dedupe_key = %q, want %q — keyed on anything but the occupancy, two closers racing on one hold insert two rows",
			key, "lease:"+leaseID+":machine.minutes")
	}
}

// machineMinutesDedupeKey reads the key the machine-time entry was written under, so the assertion about
// the second guard reads the stored value rather than trusting the constant that produced it.
func (h *artifactsHarness) machineMinutesDedupeKey(t *testing.T, project string) string {
	t.Helper()
	var key string
	if err := h.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT dedupe_key FROM usage_ledger WHERE project_id = $1 AND meter = 'machine.minutes'`, project).Scan(&key); err != nil {
		t.Fatalf("read the machine.minutes dedupe key: %v", err)
	}
	return key
}
