//go:build component

// THE WAKE IS BOUND TO THE SLOT FREEING, NOT TO A MACHINE CONNECTING (Faz A.4 T6).
//
// Until this file the ONLY capacity wake in the tree fired from the gateway's handleConnect — a machine
// JOINING a pool (runner_gateway.go's wakeParkedRun). That is the wrong event for a ceiling: a fleet whose
// machines are all connected and all full gains no members, so nothing ever fires, and a run parked for
// want of a slot waits for a reconnect that says nothing about whether a slot exists. The event that
// matters is a hold ENDING.
//
// A HOLD ENDS IN EXACTLY ONE PLACE, AND THAT IS A MEASUREMENT RATHER THAN A DESIGN CLAIM:
//
//	grep -rn 'released_at' storage/queries/*.sql | grep -i 'set'
//	→ leases.sql:130                                             (one statement moves it)
//	grep -rn 'storage.Query("ReleaseLease")' --include='*.go' . | grep -v node_modules
//	→ occupancy_billing.go:197 (SettleOccupancy)  occupancy.go:226 (ReleaseLease, no production caller)
//	grep -rn '\.SettleOccupancy(' --include='*.go' . | grep -v node_modules | grep -v _test | grep -v 'func (s \*Store)'
//	→ occupancy_billing.go:156 (HoldMachine's "lost" arm)
//	  idle_release.go:180      (settleStranded, the recovery sweep)
//	  idle_release.go:356      (release()'s own settle, the timely path)
//	                                                             (all measured 2026-08-05)
//
// So the wake belongs to SettleOccupancy — one funnel, three callers — and NOT to two hand-wired call
// sites, which would have covered two of the three and left the "lost" arm silent. What these two tests
// prove is that the two callers a customer's machine actually depends on both reach it: the timely settle
// on the release path, and the recovery sweep for the settle that failed. They are not one test written
// twice — they enter through different code, and the perturbation that removes either caller's settle
// reddens exactly one of them.

package artifacts

import (
	"context"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/packages/coordinator"

	"github.com/palgroup/palai/storage"
)

// poolOfHeldMachine reads the pool the occupied machine belongs to, straight out of the tables rather than
// from anything the fixture remembered — the wake is keyed on a POOL, so a run parked on the wrong one
// would be unreachable for a reason that has nothing to do with what is under test.
func poolOfHeldMachine(t *testing.T, h *artifactsHarness, leaseID string) string {
	t.Helper()
	var poolID string
	if err := h.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT r.pool_id FROM runner_leases l JOIN runners r ON r.id = l.runner_id WHERE l.id = $1`,
		leaseID).Scan(&poolID); err != nil {
		t.Fatalf("read the pool of the held machine: %v", err)
	}
	return poolID
}

// parkARunOnThePool puts a run of this tenant into exactly the state a capacity park leaves behind, using
// the PRODUCTION writer (ParkRunForCapacity) rather than hand-written UPDATEs: `waiting`, with an attempt
// carrying the `awaiting_capacity` marker and `runs.pool_id` naming the pool. Returns the run id.
//
// THE RUN GETS ITS OWN SESSION. Reusing the idle workspace's session would make the parked run part of the
// activity clock the sweep measures, and a fresh run row would push that session's last activity to now —
// so the workspace would stop being an idle candidate and the settle under test would never happen.
func parkARunOnThePool(t *testing.T, h *artifactsHarness, tenant coordinator.Tenant, poolID string) string {
	t.Helper()
	ctx := storage.WithTenant(context.Background(), tenant.Project)
	session, responseID, runID, attemptID := newID("ses"), newID("resp"), newID("run"), newID("att")
	h.exec(t, `INSERT INTO sessions (id, project_id) VALUES ($1,$2)`, session, tenant.Project)
	h.exec(t, `INSERT INTO responses (id, project_id, session_id, state, input)
	           VALUES ($1,$2,$3,'in_progress','"wait for a mac"'::jsonb)`, responseID, tenant.Project, session)
	h.exec(t, `INSERT INTO runs (id, project_id, session_id, response_id, state, pool_id)
	           VALUES ($1,$2,$3,$4,'running',$5)`, runID, tenant.Project, session, responseID, poolID)
	// The attempt row has to exist before the park: MarkAttemptAwaitingCapacity writes the marker onto it,
	// and a park that wrote the transition without the marker is a run no wake can ever find.
	if err := h.repo.Spine().RecordAttempt(ctx, tenant, runID, attemptID); err != nil {
		t.Fatalf("record the attempt that parks: %v", err)
	}
	if err := h.repo.Spine().ParkRunForCapacity(ctx, tenant, runID, attemptID, ""); err != nil {
		t.Fatalf("ParkRunForCapacity: %v", err)
	}
	if got := runStateOf(t, h, runID); got != "waiting" {
		t.Fatalf("the seeded run is %q, want waiting — it never parked, so nothing below is about a wake", got)
	}
	return runID
}

func runStateOf(t *testing.T, h *artifactsHarness, runID string) string {
	t.Helper()
	var state string
	if err := h.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT state FROM runs WHERE id = $1`, runID).Scan(&state); err != nil {
		t.Fatalf("read run state: %v", err)
	}
	return state
}

// wokenRunJobs counts the response.run jobs enqueued for a run. The wake is asserted through the row that
// actually causes a worker to open a fresh attempt, never through "a method was called": a wake that
// transitioned the run and enqueued nothing leaves it `running` with nothing driving it, which is worse
// than leaving it parked.
func wokenRunJobs(t *testing.T, h *artifactsHarness, runID string) int {
	t.Helper()
	var jobs int
	if err := h.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(*) FROM durable_jobs WHERE kind = 'response.run' AND payload->>'run_id' = $1`,
		runID).Scan(&jobs); err != nil {
		t.Fatalf("count the run's response.run jobs: %v", err)
	}
	return jobs
}

// TestASettledOccupancyWakesARunParkedForCapacity — THE TIMELY CLOSER ANNOUNCES THE SLOT.
//
// A session's workspace goes idle, the sweep archives it and hands the machine back, and the hold settles
// on the release path (idle_release.go's settleOccupancy). Another run of the same tenant is parked on
// that machine's pool for want of a slot. The settle must re-enter it.
//
// WITHOUT THIS THE CEILING IS UNSHIPPABLE, and that is the whole of §2b: a declared fleet fills up, every
// later run parks, and the only thing that could wake one is a machine RECONNECTING — an event that says
// nothing about whether a slot exists and which, on a fleet whose machines stay connected, may not happen
// for hours.
func TestASettledOccupancyWakesARunParkedForCapacity(t *testing.T) {
	h := openArtifactsHarness(t)
	tenant, _, workspaceID, _, leaseID := h.seedOccupiedIdleWorkspace(t)
	// A hold worth settling: the interval has to be non-zero or the settle below is arguing with rounding.
	time.Sleep(billingHold)

	poolID := poolOfHeldMachine(t, h, leaseID)
	parked := parkARunOnThePool(t, h, tenant, poolID)
	if jobs := wokenRunJobs(t, h, parked); jobs != 0 {
		t.Fatalf("the parked run already has %d response.run job(s) before any wake — the count below would prove nothing", jobs)
	}

	releaser := execution.NewIdleReleaser(h.repo.Spine(), execution.NewSnapshotSink(h.s3, h.repo.Spine()), idleTestTTL)
	// The sweep is cross-tenant and its error is the FIRST failing candidate's, which may belong to another
	// test sharing this database. Every assertion is about this workspace's own rows.
	_, _ = releaser.Sweep(context.Background())

	// THE PREMISE, ASSERTED: the machine really was handed back and the hold really did close on this path.
	// Without both, a green below would be about a wake that fired for some other reason.
	if state := workspaceState(t, h, workspaceID); state != "paused" {
		t.Fatalf("workspace state = %q, want paused — the release did not stand, so no slot freed", state)
	}
	held := occupancyOf(t, h, tenant, leaseID)
	if held.ReleasedAt.IsZero() {
		t.Fatal("the hold is still open after the sweep — nothing freed a slot, so there was nothing for a wake to announce")
	}

	if got := runStateOf(t, h, parked); got != "running" {
		t.Fatalf("the parked run is %q after a slot freed on its pool, want running — the only wake in the tree fires when a machine CONNECTS, so a fleet that is fully connected and fully occupied never wakes anything and this run waits for a reconnect that says nothing about capacity", got)
	}
	if jobs := wokenRunJobs(t, h, parked); jobs != 1 {
		t.Fatalf("%d response.run job(s) for the woken run, want 1 — a wake that transitions the run and enqueues nothing leaves it `running` with nothing driving it, which is worse than leaving it parked", jobs)
	}
}

// TestAStrandedOccupancySettledBySweepAlsoWakesTheParkedRun — THE SECOND CLOSER ANNOUNCES IT TOO.
//
// release() commits the workspace to `paused` BEFORE it settles, so a settle that fails leaves the machine
// handed back and the hold open; settleStranded is the recovery that closes it on a later tick. That
// closer frees a slot exactly as the timely one does, and a wake bound only to the timely path would leave
// every recovered slot unannounced — the parked run would wait for the NEXT hold to end, on a fleet where
// holds ending is precisely the scarce event.
//
// IT IS NOT THE FIRST TEST RESTATED. The two enter through different code (idle_release.go:356 vs :180)
// and the perturbation that removes either one reddens exactly one of these. What they share is the funnel
// both call, which is the point: coverage that is structural rather than remembered.
func TestAStrandedOccupancySettledBySweepAlsoWakesTheParkedRun(t *testing.T) {
	h := openArtifactsHarness(t)
	// The premise is asserted inside: the workspace is `paused`, the machine is handed back, and the hold
	// is STILL OPEN because the release path's own settle died in the window r.remove leaves.
	tenant, _, leaseID := h.strandByCancellingTheSettle(t)

	poolID := poolOfHeldMachine(t, h, leaseID)
	parked := parkARunOnThePool(t, h, tenant, poolID)
	if jobs := wokenRunJobs(t, h, parked); jobs != 0 {
		t.Fatalf("the parked run already has %d response.run job(s) before any wake", jobs)
	}
	// AND THE FIRST SWEEP WOKE NOTHING, which is what makes the second one the cause. The settle that would
	// have announced this slot never ran.
	if got := runStateOf(t, h, parked); got != "waiting" {
		t.Fatalf("the parked run is %q before the recovery sweep, want waiting", got)
	}

	// A LATER TICK — a fresh releaser with a live context, exactly as the maintenance goroutine builds one
	// every interval. Its idle phase finds nothing (the workspace is `paused`); settleStranded is what runs.
	later := execution.NewIdleReleaser(h.repo.Spine(), execution.NewSnapshotSink(h.s3, h.repo.Spine()), idleTestTTL)
	_, _ = later.Sweep(context.Background())

	held := occupancyOf(t, h, tenant, leaseID)
	if held.ReleasedAt.IsZero() {
		t.Fatal("the stranded hold is still open after the recovery sweep — no slot freed, so nothing below is about a wake")
	}
	if got := runStateOf(t, h, parked); got != "running" {
		t.Fatalf("the parked run is %q after the recovery sweep freed its pool's slot, want running — a wake wired only to the timely settle leaves every RECOVERED slot unannounced, and a recovered slot is the one a fleet at its ceiling is waiting for", got)
	}
	if jobs := wokenRunJobs(t, h, parked); jobs != 1 {
		t.Fatalf("%d response.run job(s) for the woken run, want 1", jobs)
	}
}
