//go:build component

// THE NUMBER A PLACEMENT PREFERS BY, against a real PostgreSQL (Faz A.5 T6).
//
// capacity_placement_test.go holds what a machine REFUSES. These hold the read that decides which machine
// is asked in the first place — and, in the second test, that the two never trade places: a preference may
// choose, and only the acquire may refuse.
//
// WHY THE READ NEEDS A DATABASE AT ALL. Its whole content is three things Postgres decides and a fake
// cannot: that the open holds are counted PER MACHINE, that `released_at IS NULL` is what "occupied" means
// here exactly as it does in the ceiling, and that row-level security confines the answer to one tenant.
// The ORDERING built on top of it is a wire question and is proved Docker-free in
// apps/control-plane/internal/execution/runner_least_loaded_test.go; neither file proves the other's half.
package fleet

import (
	"context"
	"errors"
	"testing"

	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/storage"
)

// poolOf reads the pool the seed put this tenant's machines in. Kept in this file for the reason
// seedMachineWithCapacity is: the shared seed is one several tasks in this phase are editing, and a field
// added there for one test here is a change to a file this test does not own.
func (e *fleetEnv) poolOf(t *testing.T) string {
	t.Helper()
	var pool string
	if err := e.cs.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT pool_id FROM runners WHERE id = $1`, e.runnerA).Scan(&pool); err != nil {
		t.Fatalf("read the seeded pool off machine %s: %v", e.runnerA, err)
	}
	return pool
}

// weigh reads the pool's machine loads the way a placement does.
func (e *fleetEnv) weigh(t *testing.T, pool string) map[string]coordinator.PoolMachineLoad {
	t.Helper()
	loads, err := e.cs.PoolMachineLoads(context.Background(), e.tenant, pool)
	if err != nil {
		t.Fatalf("PoolMachineLoads(%s) error = %v", pool, err)
	}
	byMachine := make(map[string]coordinator.PoolMachineLoad, len(loads))
	for _, load := range loads {
		if _, already := byMachine[load.RunnerID]; already {
			t.Fatalf("machine %s was weighed twice in one answer — a placement summing a machine's row more than once ranks it as loaded for a reason that is not work", load.RunnerID)
		}
		byMachine[load.RunnerID] = load
	}
	return byMachine
}

// loadOf takes one machine's weight out of an answer, failing loudly when it is absent — absence and a
// zero weight are opposite facts, and a test that let them collapse would read a machine the tenant cannot
// see as the emptiest machine on the fleet.
func loadOf(t *testing.T, byMachine map[string]coordinator.PoolMachineLoad, runnerID string) coordinator.PoolMachineLoad {
	t.Helper()
	load, present := byMachine[runnerID]
	if !present {
		t.Fatalf("machine %s is missing from its own pool's weights — a placement reads that as 'nothing known', which ranks it EMPTY", runnerID)
	}
	return load
}

// TestThePlacementWeighsEachMachineOfAPoolByItsOwnOpenHolds — THE NUMBER IS PER MACHINE, IT IS THE OPEN
// HOLDS, AND THE DECLARED CEILING COMES BACK RAW.
//
// Three machines in one pool carrying different work. Each is weighed by ITS OWN open holds — a total that
// happened to be right for the pool would rank every machine identically, which is the placement that
// shipped before this — and a settled hold drops the count it was in, because the ceiling this preference
// exists to stay ahead of counts `released_at IS NULL` and not holds ever taken.
//
// `capacity` COMES BACK AS ZERO AND THAT IS THE LOAD-BEARING ASSERTION, not a detail of the fixture. Zero
// means the machine declared nothing, migration 000005 made it the column's DEFAULT, and no machine in any
// shipped deployment declares anything else. A read that clamped it to 1 on the way out — which is what
// `fleet.Store.Register` does on the way IN — would hand the preference a fleet in which every machine is
// full at its first session, and the ordering would collapse for every deployment that exists.
func TestThePlacementWeighsEachMachineOfAPoolByItsOwnOpenHolds(t *testing.T) {
	env := newFleetEnv(t)
	pool := env.poolOf(t)
	declared := env.seedMachineWithCapacity(t, 3)

	busy := env.mustAcquire(t, env.seedSession(t), env.runnerA)
	env.mustAcquire(t, env.seedSession(t), env.runnerA)
	env.mustAcquire(t, env.seedSession(t), declared)

	weights := env.weigh(t, pool)
	if got := loadOf(t, weights, env.runnerA).Open; got != 2 {
		t.Fatalf("the machine holding two sessions was weighed at %d open hold(s), want 2", got)
	}
	if got := loadOf(t, weights, env.runnerB).Open; got != 0 {
		t.Fatalf("the machine holding nothing was weighed at %d open hold(s), want 0 — a count that is not per-machine ranks an idle Mac with the busy one", got)
	}
	if got := loadOf(t, weights, declared).Open; got != 1 {
		t.Fatalf("the machine holding one session was weighed at %d open hold(s), want 1", got)
	}

	// The ceiling, raw. Undeclared stays undeclared and therefore has room whatever it holds; a declared
	// three has room at one.
	if got := loadOf(t, weights, env.runnerA); got.Capacity != 0 || !got.HasRoom() {
		t.Fatalf("the undeclared machine came back capacity=%d hasRoom=%v, want 0 and true — an undeclared machine has no ceiling, and reading it as a zero-slot machine reads the whole shipped fleet as full",
			got.Capacity, got.HasRoom())
	}
	if got := loadOf(t, weights, declared); got.Capacity != 3 || !got.HasRoom() {
		t.Fatalf("the machine that declared three came back capacity=%d hasRoom=%v while holding 1, want 3 and true", got.Capacity, got.HasRoom())
	}

	// A SETTLED HOLD FREES THE WEIGHT IT CARRIED. Settled rather than merely released, because settling is
	// what the production reclaim path does — a count that noticed only a bare ReleaseLease would be
	// reading a column nothing on that path writes, and every Mac would look permanently loaded.
	if !env.mustSettle(t, busy, "closed") {
		t.Fatal("settling the first hold closed nothing — it was open, so this is the call that had to close it")
	}
	if got := loadOf(t, env.weigh(t, pool), env.runnerA).Open; got != 1 {
		t.Fatalf("after one settlement the machine was weighed at %d open hold(s), want 1 — a weight that counts holds ever taken makes every Mac permanently unpreferred", got)
	}

	// AND THE ANSWER IS ONE TENANT'S. Another tenant asking about THIS pool by name is told nothing, so a
	// placement can never be steered by — or informed about — machines it may not use.
	theirs := seedFleet(t, env.cs)
	foreign, err := theirs.cs.PoolMachineLoads(context.Background(), theirs.tenant, pool)
	if err != nil {
		t.Fatalf("another tenant weighing this pool = %v, want an empty answer rather than an error", err)
	}
	if len(foreign) != 0 {
		t.Fatalf("another tenant weighing this pool saw %d machine(s), want 0 — a placement that can see another tenant's machines can be steered by them", len(foreign))
	}
}

// TestWhenTheChosenMachineLosesTheRaceTheNextIsTriedAndTheCeilingStillHolds — A SELECTOR MUST NOT SOFTEN
// THE CEILING, AND THE CEILING MUST NOT BE MOVED INTO THE SELECTOR.
//
// This is the test the whole task turns on, and it walks the exact sequence a fleet produces:
//
//  1. the pool is weighed and a machine with room is chosen;
//  2. THE RACE — another session takes that machine's last slot in between;
//  3. the acquire the placement was about to make is refused, by the STATEMENT and not by the snapshot;
//  4. weighed again, no machine has room, so there is no next one to try and the run parks;
//  5. the table is not one row over what the two machines declared.
//
// STEP 3 IS WHERE A SELECTOR EARNS OR LOSES ITS KEEP. Everything the preference reads is stale the moment
// it is read: it is a plain SELECT outside any transaction, deliberately, because a placement that took a
// lock and reserved a slot would be a second ceiling — weaker than the real one, disagreeing with it under
// contention, and standing between a machine that just freed a slot and the run that could have used it.
// So the snapshot being WRONG is ordinary rather than exceptional, and what must hold is that being wrong
// costs a park and never an over-allocation.
//
// STEP 4 IS THE OTHER HALF AND IT IS NOT DECORATION. A pool where every machine is full must still WEIGH,
// and must still return every machine: a read that filtered full machines out would hand the caller an
// empty answer, and an empty answer is indistinguishable from a pool with no machines in it — which is a
// different park with a different wake, and getting it wrong strands the run.
//
// THE CEILINGS ARE ONE EACH ON PURPOSE. At one, the last slot and the first are the same slot, which is the
// sharpest form of the race: there is no arithmetic left for a wrong implementation to be nearly right in.
func TestWhenTheChosenMachineLosesTheRaceTheNextIsTriedAndTheCeilingStillHolds(t *testing.T) {
	env := newFleetEnv(t)
	ctx := context.Background()
	pool := env.poolOf(t)
	machineA := env.seedMachineWithCapacity(t, 1)
	machineB := env.seedMachineWithCapacity(t, 1)

	// A takes the one session it declared it could hold. The placement now has a real choice to make.
	env.mustAcquire(t, env.seedSession(t), machineA)

	// THE NEXT ONE IS TRIED, and this is the preference doing its whole job: the machine at its ceiling is
	// not the one a placement reaches for, even though its open count (1) is no worse than plenty of
	// machines that would be fine.
	weights := env.weigh(t, pool)
	if loadOf(t, weights, machineA).HasRoom() {
		t.Fatalf("the machine holding the one session it declared reports room — a placement would send the next run to the one Mac that cannot take it")
	}
	if !loadOf(t, weights, machineB).HasRoom() {
		t.Fatalf("the empty machine reports no room — with nothing to prefer, the placement is back to whichever machine is at the head of the queue")
	}

	// THE RACE. Between the weighing above and the acquire below, another session takes B's last slot. The
	// placement's answer is now wrong, and nothing about it can know that.
	env.mustAcquire(t, env.seedSession(t), machineB)

	stale := env.seedSession(t)
	if _, err := env.cs.AcquireLease(ctx, env.tenant, stale, machineB); !errors.Is(err, coordinator.ErrMachineAtCapacity) {
		t.Fatalf("the acquire on the machine the STALE weighing chose = %v, want ErrMachineAtCapacity — the preference is not allowed to be the thing that admits a session, and here it would have admitted one", err)
	}

	// AND THERE IS NO NEXT ONE. Every machine in the pool is at its ceiling, the pool still weighs, every
	// machine is still in the answer, and none of them has room: the run parks.
	weights = env.weigh(t, pool)
	if len(weights) < 4 {
		t.Fatalf("a pool whose machines are all full weighed %d machine(s), want every machine in it — an empty answer is indistinguishable from a pool with nobody in it, which is a different park with a different wake", len(weights))
	}
	if loadOf(t, weights, machineA).HasRoom() || loadOf(t, weights, machineB).HasRoom() {
		t.Fatal("a machine at its declared ceiling still reports room after the race — the placement would keep choosing machines that refuse")
	}
	if _, err := env.cs.AcquireLease(ctx, env.tenant, stale, machineA); !errors.Is(err, coordinator.ErrMachineAtCapacity) {
		t.Fatalf("the acquire on the other full machine = %v, want ErrMachineAtCapacity — with no machine able to take it the run must park, not land somewhere", err)
	}

	// NO OVER-ALLOCATION, COUNTED IN THE TABLE. Both refusals above could be reported and the rows written
	// anyway, which is the failure that reports success to everybody: each machine declared one, so each
	// machine holds one.
	if open := env.openHoldsOn(t, machineA); open != 1 {
		t.Fatalf("the machine that declared 1 holds %d open hold(s) — the placement put a session on a Mac that had said it would not take one", open)
	}
	if open := env.openHoldsOn(t, machineB); open != 1 {
		t.Fatalf("the machine that declared 1 holds %d open hold(s) after losing the race — the stale weighing was allowed to decide, and the overrun is over the ceiling AND unbilled", open)
	}
}
