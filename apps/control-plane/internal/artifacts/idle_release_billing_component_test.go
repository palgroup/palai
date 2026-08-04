//go:build component

// TWO FAILURE POINTS ON ONE RELEASE PATH, WITH OPPOSITE BILLING ANSWERS (Faz A.4 T4).
//
// The idle sweep archives a workspace, hands its uid back, deletes its bytes, and settles the machine time
// the session occupied. Two steps of that can fail, both read as `err != nil`, and the right answer to
// them is not the same answer:
//
//	CAPTURE fails  the release is CANCELLED — returnClaim → finish_snapshot → `ready`, the bytes are
//	               untouched, released=false. The machine is still this session's. THE HOLD DID NOT END,
//	               so it must not be closed and must not be billed.
//	REMOVE fails   the release STANDS — the archive exists, the workspace is `paused`, and the next
//	               allocation is a fresh directory nothing will be placed on these bytes. The machine IS
//	               handed back. THE HOLD ENDED, so it is closed and billed; what leaked is a directory.
//
// A build that folded them into one arm would answer one of them wrong, and which one is only visible on a
// customer's invoice. That is why they are two tests and not one table.
//
// AND THE ABSENCE IS MADE NON-VACUOUS. "No ledger row" means EITHER not settled OR settled zero —
// settleUsage skips a zero quantity. So each test holds the machine for real time first, and each asserts
// the OCCUPANCY's own state (open / closed) beside the row count, which the two branches disagree about
// for a reason no rounding can produce.

package artifacts

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/packages/coordinator"

	"github.com/palgroup/palai/storage"
)

// billingHold is how long the session occupies its machine before the sweep arrives. It is real time, and
// it is what makes "no ledger row" mean "nothing was settled" rather than "an interval of zero rounded
// away".
const billingHold = 700 * time.Millisecond

// seedOccupiedIdleWorkspace is seedIdleWorkspace plus the thing the sweep is now expected to settle: a
// machine in this workspace's own project, and an OPEN occupancy of it by this workspace's session, held
// for long enough to be worth money.
func (h *artifactsHarness) seedOccupiedIdleWorkspace(t *testing.T) (tenant coordinator.Tenant, session, workspaceID, hostPath, leaseID string) {
	t.Helper()
	project, session, workspaceID, _, hostPath := h.seedIdleWorkspace(t)
	tenant = coordinator.Tenant{Project: project}

	pool, runner := newID("pool"), newID("rnr")
	h.exec(t, `INSERT INTO runner_pools (id, project_id, name, posture)
	           VALUES ($1, $2, 'default', 'unsandboxed-host')`, pool, project)
	h.exec(t, `INSERT INTO runners (id, project_id, pool_id, state, posture)
	           VALUES ($1, $2, $3, 'active', 'unsandboxed-host')`, runner, project, pool)

	// HoldMachine rather than AcquireLease, because HoldMachine is what the attempt path calls — a fixture
	// that reached past it would be proving the sweep against a row production does not write.
	ctx := context.Background()
	leaseID, err := h.repo.Spine().HoldMachine(ctx, tenant, session, runner)
	if err != nil {
		t.Fatalf("HoldMachine() error = %v", err)
	}
	time.Sleep(billingHold)
	// THE ATTEMPT ENDS AND STAMPS THE HOLD, which is what keepMachineHeld does on every exit — and the
	// fixture has to do it because WITHOUT it this session did nothing at all after taking the machine.
	// An idle-closed hold bills `last_activity_at - started_at`, so an unstamped hold bills ZERO, settles
	// no row (settleUsage skips a zero quantity), and every "no row" assertion below would be satisfied by
	// the fixture rather than by the code. That is not hypothetical: it is how the remove-failure test
	// first failed.
	if err := h.repo.Spine().TouchLease(ctx, tenant, leaseID); err != nil {
		t.Fatalf("TouchLease() error = %v", err)
	}
	return tenant, session, workspaceID, hostPath, leaseID
}

// settledMachineMinutes reads what this tenant has actually been charged for machine time, straight out of
// usage_ledger. Scoped to the project because this suite's PostgreSQL is shared with every other leg, so
// an unscoped count would be an assertion about the neighbours.
func (h *artifactsHarness) settledMachineMinutes(t *testing.T, project string) []float64 {
	t.Helper()
	rows, err := h.pool.Query(storage.WithSystemScope(context.Background()),
		`SELECT quantity FROM usage_ledger WHERE project_id = $1 AND meter = 'machine.minutes'
		  ORDER BY occurred_at, id`, project)
	if err != nil {
		t.Fatalf("read settled machine time: %v", err)
	}
	defer rows.Close()
	var out []float64
	for rows.Next() {
		var quantity float64
		if err := rows.Scan(&quantity); err != nil {
			t.Fatalf("scan settled machine time: %v", err)
		}
		out = append(out, quantity)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read settled machine time: %v", err)
	}
	return out
}

// occupancyOf reads one hold directly, so an assertion about whether it is open never goes through the
// code under test.
func occupancyOf(t *testing.T, h *artifactsHarness, tenant coordinator.Tenant, leaseID string) coordinator.Occupancy {
	t.Helper()
	held, err := h.repo.Spine().Occupancy(context.Background(), tenant, leaseID)
	if err != nil {
		t.Fatalf("Occupancy(%s) error = %v", leaseID, err)
	}
	return held
}

// TestACaptureThatFailedLeavesTheMachineHeldAndBillsNothing — THE RELEASE WAS CANCELLED, SO THE HOLD GOES
// ON.
//
// The archive is what a release is FOR: without it the bytes cannot come back. So a capture that fails
// gives the claim back, leaves the directory alone and returns released=false — the workspace is exactly
// where it started and the machine is still this session's. Billing here would charge for a hold that has
// not ended, and closing it would make the next sweep open a second row for the same continuous hold.
//
// The capture is made to fail the way production fails it: the allocation's directory is gone, so there is
// nothing to archive.
func TestACaptureThatFailedLeavesTheMachineHeldAndBillsNothing(t *testing.T) {
	h := openArtifactsHarness(t)
	ctx := context.Background()
	tenant, session, workspaceID, hostPath, leaseID := h.seedOccupiedIdleWorkspace(t)

	// The bytes this sweep would have archived are not there.
	if err := os.RemoveAll(hostPath); err != nil {
		t.Fatalf("remove the allocation directory: %v", err)
	}

	releaser := execution.NewIdleReleaser(h.repo.Spine(), execution.NewSnapshotSink(h.s3, h.repo.Spine()), idleTestTTL)
	// The sweep is cross-tenant and its error is the FIRST candidate's, which may be another test's — so
	// nothing is asserted about the return, and everything is asserted about this workspace's own rows.
	_, _ = releaser.Sweep(ctx)

	// THE RELEASE REALLY WAS CANCELLED. Without this the billing assertions below hold trivially for a
	// sweep that never reached this candidate at all.
	if state := workspaceState(t, h, workspaceID); state != "ready" {
		t.Fatalf("workspace state = %q, want ready — the capture did not fail the way this test needs it to, so nothing below is about a cancelled release", state)
	}

	held := occupancyOf(t, h, tenant, leaseID)
	if !held.ReleasedAt.IsZero() {
		t.Fatalf("the hold was closed at %s after a release that was cancelled — the machine is still this session's, and the next sweep will open a SECOND row for one continuous hold",
			held.ReleasedAt)
	}
	if held.Billed < billingHold {
		t.Fatalf("the open hold reads %s, which is less than the %s it was held for — the amount below being zero would then say nothing", held.Billed, billingHold)
	}
	if rows := h.settledMachineMinutes(t, tenant.Project); len(rows) != 0 {
		t.Fatalf("a cancelled release settled %d machine.minutes row(s), want 0 — the customer was billed for a hold that is still running (%v)", len(rows), rows)
	}
	// Put the fixture back where production would leave it, so the next sweep in this shared database does
	// not pick up a candidate whose directory this test deleted.
	h.touchSession(t, session)
}

// TestAReleaseWhoseBytesLeakedStillClosesAndBillsTheHold — THE MACHINE WENT BACK, SO THE HOLD IS OVER.
//
// This is the OTHER failure and the opposite answer. The archive is written, the account is handed back
// and the workspace is `paused`; only the directory removal failed. The machine IS free — the next
// allocation is a fresh directory and nothing will ever be placed on these bytes — so the hold ended and
// must be billed. Leaving it open would leave machine time nobody is charged for and a machine that looks
// occupied forever.
//
// A meter must never abort a release that already happened, which is why the settlement is the last thing
// on the path: a late row is recoverable, a workspace whose bytes are archived and whose hold is still
// open is not.
func TestAReleaseWhoseBytesLeakedStillClosesAndBillsTheHold(t *testing.T) {
	h := openArtifactsHarness(t)
	ctx := context.Background()
	tenant, _, workspaceID, _, leaseID := h.seedOccupiedIdleWorkspace(t)

	releaser := execution.NewIdleReleaser(h.repo.Spine(), execution.NewSnapshotSink(h.s3, h.repo.Spine()), idleTestTTL)
	releaser.SetTeardown(func(string) error { return errors.New("device busy") })
	_, _ = releaser.Sweep(ctx)

	// THE RELEASE REALLY STOOD. `paused` is the state the release commits to, reached only after the
	// archive exists — so this is what says the machine went back rather than the sweep having skipped
	// this candidate.
	if state := workspaceState(t, h, workspaceID); state != "paused" {
		t.Fatalf("workspace state = %q, want paused — the release did not stand, so the settlement below is not the one this test is about", state)
	}

	held := occupancyOf(t, h, tenant, leaseID)
	if held.ReleasedAt.IsZero() {
		t.Fatal("the machine was handed back and the hold is still open — that time is billed to nobody, and placement will read the machine as occupied forever")
	}
	if held.ReleaseReason != "idle" {
		t.Fatalf("release reason = %q, want idle — this is the idle sweep, and 'idle' is the one word that stops the bill at last_activity_at instead of at the sweep", held.ReleaseReason)
	}
	// NON-VACUITY, and it is the assertion this test needed to be written twice to get: a hold whose billed
	// interval is zero settles NO row, so "1 row" below would be unreachable for reasons that have nothing
	// to do with the release standing. The interval is only non-zero because the fixture stamps the hold the
	// way an ending attempt does.
	if held.Billed < billingHold {
		t.Fatalf("the closed hold bills %s, less than the %s it was held for — a zero-length hold settles nothing, so the row count below would be about the fixture", held.Billed, billingHold)
	}
	rows := h.settledMachineMinutes(t, tenant.Project)
	if len(rows) != 1 {
		t.Fatalf("%d machine.minutes row(s) after a release that stood, want 1 — the bytes leaking is a disk problem, not a reason to give the machine away", len(rows))
	}
	settled := time.Duration(rows[0] * float64(time.Minute))
	if want := held.Billed; settled < want-time.Millisecond || settled > want+time.Millisecond {
		t.Fatalf("settled %s, the occupancy says %s — the amount charged and the amount derived are two answers to one question", settled, want)
	}
}
