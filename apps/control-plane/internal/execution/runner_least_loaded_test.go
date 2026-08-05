package execution_test

// THE EMPTIEST MAC GETS THE SESSION (Faz A.5 T6), over the real mTLS wire, Docker-free.
//
// The claim these hold is about which machine crosses the WIRE with the lease on it, which is why they
// drive the gateway rather than the ranking function: a ranking nothing consults is the shape this tree has
// shipped repeatedly, and `emptiestFirst` returning the right answer says nothing about `Dial` asking it.
//
// WHAT THEY CANNOT HOLD, stated so neither over-claims. The CEILING is a SQL statement — a subquery inside
// AcquireLease's INSERT, serialized by a row lock — so "the ceiling still refuses, and the table is not over
// its declared capacity" is not provable against a fake view. That half lives in
// tests/component/fleet/least_loaded_placement_test.go, against a real PostgreSQL. What these prove is the
// half that belongs here: which machine is TRIED, and that a machine looking full is passed over rather
// than refused.
//
// PARK ORDER IS STAGED, NOT HOPED FOR, and it is load-bearing in one direction only. Each machine is parked
// and awaited before the next one enrols, so "A parked first" is a fact of the fixture. It matters because
// a build that ignores the preference takes the head of the parked list — so a proof that the SECOND-parked
// machine won is a proof the preference decided. If a machine were somehow not parked yet, the remaining
// one would be the only candidate and the subtest demanding the other would FAIL; the staging cannot turn a
// broken build green, only a working one red.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/apps/control-plane/internal/fleet"
	"github.com/palgroup/palai/packages/coordinator"
)

// parkSettle is how long both machines are given to reach their queue before a dial chooses between them.
// It stages the condition the way the component tier's contenderSettle does — see the file header for why
// it cannot make a broken build pass.
const parkSettle = 250 * time.Millisecond

// fakeMachineLoads is the in-memory execution.MachineLoadView the proofs below drive. It answers what a
// real pool's machines WOULD weigh, which is enough for every claim about the choice; the claim about the
// numbers themselves is a database question and is held in the component tier.
//
// It records the pool it was asked about and how many times, because "the gateway does not ask when there
// is nothing to choose between" is one of the properties under proof and is invisible in the outcome.
type fakeMachineLoads struct {
	mu     sync.Mutex
	loads  map[string]coordinator.PoolMachineLoad
	asked  []string
	tenant coordinator.Tenant
}

func newFakeMachineLoads() *fakeMachineLoads {
	return &fakeMachineLoads{loads: map[string]coordinator.PoolMachineLoad{}}
}

// declare records what one machine looks like to a placement: its open holds and the ceiling it declared.
// Capacity 0 is a machine that declared NOTHING, which is every machine in every shipped deployment.
func (f *fakeMachineLoads) declare(runnerID string, open, capacity int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loads[runnerID] = coordinator.PoolMachineLoad{RunnerID: runnerID, Open: open, Capacity: capacity}
}

func (f *fakeMachineLoads) PoolMachineLoads(_ context.Context, tenant coordinator.Tenant, poolID string) ([]coordinator.PoolMachineLoad, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, poolID)
	f.tenant = tenant
	out := make([]coordinator.PoolMachineLoad, 0, len(f.loads))
	for _, load := range f.loads {
		out = append(out, load)
	}
	return out, nil
}

func (f *fakeMachineLoads) timesAsked() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.asked)
}

// twoParkedMachines enrols two machines into the default pool of the fake registry's tenant and holds both
// parked, with the returned ids in PARK ORDER: first is the one a build with no preference would take.
func twoParkedMachines(t *testing.T, f *gatewayFixture, tokenA, tokenB string) (first, second *parkedRunner, idFirst, idSecond string) {
	t.Helper()
	first = park(t, f, tokenA)
	waitConnected(t, f.gateway, 1)
	second = park(t, f, tokenB)
	waitConnected(t, f.gateway, 2)
	time.Sleep(parkSettle)
	idFirst, idSecond = runnerIDOf(t, first.identity), runnerIDOf(t, second.identity)
	if idFirst == idSecond {
		t.Fatalf("both machines enrolled as %q: two machines with one identity cannot be told apart by a placement", idFirst)
	}
	return first, second, idFirst, idSecond
}

// leaseAttempt dials one attempt into the fake registry's tenant and default pool.
func leaseAttempt(t *testing.T, f *gatewayFixture, runID string) execution.EngineChannel {
	t.Helper()
	attempt := f.attempt(runID, "att_"+runID, 1)
	attempt.PoolID = fleet.DefaultPoolID
	attempt.Tenant = registryTenant()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ch, err := f.gateway.Dial(ctx, attempt)
	if err != nil {
		t.Fatalf("Dial %s = %v, want a lease: two machines are parked in this pool", runID, err)
	}
	return ch
}

// wantLeased asserts the lease crossed the wire to the machine named `wantName` and not to the other one.
// It reads BOTH channels rather than only the expected one, so a build that offered the run to the wrong
// machine fails with the wrong machine named instead of timing out on the right one.
func wantLeased(t *testing.T, wantName string, want, other *parkedRunner) {
	t.Helper()
	select {
	case <-want.lease:
	case lease := <-other.lease:
		t.Fatalf("lease %s went to the OTHER machine: the run was placed on a Mac the placement had a reason not to choose, want %s", lease.LeaseID, wantName)
	case <-time.After(10 * time.Second):
		t.Fatalf("neither machine was offered the lease the gateway said it granted (wanted %s)", wantName)
	}
}

// TestASessionLandsOnTheMachineWithTheFewestOpenOccupancies — THE OWNER'S SENTENCE, MEASURED.
//
// "Ten Macs, a session request arrives, open the session on the emptiest Mac." Two machines are parked in
// one pool; one is carrying open holds and the other is carrying none. The session goes to the empty one.
//
// THE TWO SUBTESTS ARE NOT THE SAME TEST TWICE. The second is the one that can fail for the reason this
// task exists: the emptiest machine is the one that parked SECOND, so a build that hands over the head of
// the parked list — which is every build before this file — offers the run to the loaded Mac. The first is
// the control that keeps the second honest: without it, a "always take the last one parked" build would
// pass, and that is not a placement either.
//
// THE COUNT IS OPEN OCCUPANCIES AND NOT IN-FLIGHT LEASES, which is the whole reason a PARKED machine can be
// loaded at all. Both machines here are idle on the wire — neither is serving a lease — and one of them is
// still carrying two sessions' allocations, which is exactly what a Mac between messages looks like.
func TestASessionLandsOnTheMachineWithTheFewestOpenOccupancies(t *testing.T) {
	t.Run("the emptiest machine parked first", func(t *testing.T) {
		f := newGatewayFixture(t, newOneUseTokens("empty-first-a", "empty-first-b"))
		f.gateway.SetRegistry(newFakeRegistry())
		loads := newFakeMachineLoads()
		f.gateway.SetMachineLoadView(loads)

		first, second, idFirst, idSecond := twoParkedMachines(t, f, "empty-first-a", "empty-first-b")
		loads.declare(idFirst, 0, 0)
		loads.declare(idSecond, 2, 0)

		ch := leaseAttempt(t, f, "run_empty_first")
		defer ch.Close()
		wantLeased(t, "the machine holding no open occupancy", first, second)
	})

	t.Run("the emptiest machine parked second", func(t *testing.T) {
		f := newGatewayFixture(t, newOneUseTokens("empty-second-a", "empty-second-b"))
		f.gateway.SetRegistry(newFakeRegistry())
		loads := newFakeMachineLoads()
		f.gateway.SetMachineLoadView(loads)

		first, second, idFirst, idSecond := twoParkedMachines(t, f, "empty-second-a", "empty-second-b")
		loads.declare(idFirst, 2, 0)
		loads.declare(idSecond, 0, 0)

		ch := leaseAttempt(t, f, "run_empty_second")
		defer ch.Close()
		// This is the assertion the task is for: the loaded machine is at the head of the parked list, so
		// the only thing that can pass it over is a placement that read the fleet's occupancies.
		wantLeased(t, "the machine holding no open occupancy", second, first)

		// The pool was weighed, and it was weighed as the pool and tenant the attempt named. A build that
		// asked about the wrong pool would answer the right machine here only because the fake knows two.
		if got := loads.timesAsked(); got != 1 {
			t.Fatalf("the pool's machines were weighed %d time(s) for one dial, want exactly 1", got)
		}
		if loads.asked[0] != fleet.DefaultPoolID || loads.tenant != registryTenant() {
			t.Fatalf("the placement weighed pool %q of tenant %+v, want %q of %+v — a preference read from another pool is a preference about other machines",
				loads.asked[0], loads.tenant, fleet.DefaultPoolID, registryTenant())
		}
	})
}

// TestAMachineThatDeclaredNothingIsStillAChoice — `capacity = 0` MEANS UNDECLARED, WHICH MEANS UNLIMITED,
// AND NEVER "FULL".
//
// It is the one real trap in that column: written as a bare `open >= capacity`, a machine that simply never
// said anything ranks as permanently full. And because NO machine in any shipped deployment declares a
// capacity — the runner sends the field only when an operator configures one — that inversion would rank
// the ENTIRE fleet as full, which is not a subtle degradation: every machine would be equally unpreferred
// and the ordering this file exists for would silently stop existing everywhere it runs.
//
// The first subtest is the sharp form: an undeclared machine carrying FIVE open holds beats a machine that
// declared two and holds two. More work, and still the better choice, because the one that declared a
// ceiling has reached it and the one that declared nothing cannot.
//
// The second is what stops that being read as "undeclared always wins": two undeclared machines are ranked
// against EACH OTHER by their open holds, so an unlimited machine is a ranked candidate and not a wildcard.
func TestAMachineThatDeclaredNothingIsStillAChoice(t *testing.T) {
	t.Run("an undeclared machine carrying more work beats a machine at its ceiling", func(t *testing.T) {
		f := newGatewayFixture(t, newOneUseTokens("undeclared-a", "undeclared-b"))
		f.gateway.SetRegistry(newFakeRegistry())
		loads := newFakeMachineLoads()
		f.gateway.SetMachineLoadView(loads)

		first, second, idFirst, idSecond := twoParkedMachines(t, f, "undeclared-a", "undeclared-b")
		loads.declare(idFirst, 2, 2)  // declared two, holding two: no room
		loads.declare(idSecond, 5, 0) // declared nothing: no ceiling, so room whatever it holds

		ch := leaseAttempt(t, f, "run_undeclared")
		defer ch.Close()
		wantLeased(t, "the machine that declared no ceiling", second, first)
	})

	t.Run("two undeclared machines are ranked against each other", func(t *testing.T) {
		f := newGatewayFixture(t, newOneUseTokens("both-undeclared-a", "both-undeclared-b"))
		f.gateway.SetRegistry(newFakeRegistry())
		loads := newFakeMachineLoads()
		f.gateway.SetMachineLoadView(loads)

		first, second, idFirst, idSecond := twoParkedMachines(t, f, "both-undeclared-a", "both-undeclared-b")
		loads.declare(idFirst, 5, 0)
		loads.declare(idSecond, 1, 0)

		ch := leaseAttempt(t, f, "run_both_undeclared")
		defer ch.Close()
		wantLeased(t, "the emptier of two machines that declared nothing", second, first)
	})
}

// TestAMachineAtItsCeilingIsPassedOverAndAFullPoolIsStillHandedOne — A SELECTOR MUST NOT SOFTEN THE
// CEILING, AND MUST NOT BECOME ONE EITHER. This is the wire half of the third claim; the half about what
// the ceiling itself then does is in the component tier, where the ceiling is a real statement.
//
// TRYING THE NEXT ONE IS THIS LAYER'S JOB and it is free here: a machine already at its declared ceiling is
// ranked below every machine with room, so the run is offered to the one that can take it without a dial, a
// workspace or a park being spent on the one that cannot.
//
// REFUSING IS NOT THIS LAYER'S JOB, and the second subtest is the one that matters. When every parked
// machine looks full, the dial still hands one over. That looks like the weaker behaviour and is the
// correct one: this snapshot is stale by the time it is read — a hold may have settled since — and the only
// thing entitled to say no is the statement that writes the row, from behind the machine's own row lock. A
// preference that refused here would be a SECOND ceiling: weaker than the real one, disagreeing with it
// under contention, and standing between a free machine and the run that could have used it.
func TestAMachineAtItsCeilingIsPassedOverAndAFullPoolIsStillHandedOne(t *testing.T) {
	t.Run("the machine with room is tried, the one at its ceiling is not", func(t *testing.T) {
		f := newGatewayFixture(t, newOneUseTokens("ceiling-a", "ceiling-b"))
		f.gateway.SetRegistry(newFakeRegistry())
		loads := newFakeMachineLoads()
		f.gateway.SetMachineLoadView(loads)

		first, second, idFirst, idSecond := twoParkedMachines(t, f, "ceiling-a", "ceiling-b")
		// The full machine is also the EMPTIER one by raw count, so only the ceiling can pass it over.
		loads.declare(idFirst, 1, 1)
		loads.declare(idSecond, 3, 10)

		ch := leaseAttempt(t, f, "run_ceiling")
		defer ch.Close()
		wantLeased(t, "the machine with a slot free", second, first)
	})

	t.Run("a pool whose every machine is full is still handed a machine", func(t *testing.T) {
		f := newGatewayFixture(t, newOneUseTokens("allfull-a", "allfull-b"))
		f.gateway.SetRegistry(newFakeRegistry())
		loads := newFakeMachineLoads()
		f.gateway.SetMachineLoadView(loads)

		first, second, idFirst, idSecond := twoParkedMachines(t, f, "allfull-a", "allfull-b")
		loads.declare(idFirst, 4, 4)
		loads.declare(idSecond, 1, 1)

		ch := leaseAttempt(t, f, "run_allfull")
		defer ch.Close()
		// Whichever machine it is, one of them got the lease — the dial did not refuse, and it did not
		// block until its budget expired. The emptier of two full machines is the named winner, so the
		// ranking is still total when every candidate is full.
		wantLeased(t, "the emptier of two machines that both look full", second, first)
	})
}

// TestTwoEquallyEmptyMachinesGoInParkOrder — THE TIE-BREAK IS THE IDLE FLEET'S ORDINARY PATH, NOT AN EDGE
// CASE, WHICH IS THE ONLY REASON IT IS WORTH A TEST OF ITS OWN.
//
// Every machine in a quiet pool is weighed at zero, so on a fleet that is doing nothing the tie is what
// decides EVERY placement — the preference has nothing to separate the candidates with until the first
// hold opens. An unnamed tie is then not a rare coin toss, it is the whole behaviour of an idle fleet, and
// this tree has twice recorded what an unordered pick decides when nobody writes the order down.
//
// The order is the one the queue already had: longest parked first. That makes this claim conservative
// rather than new — with nothing to prefer, the machine chosen is the machine that shipped before any of
// this existed.
func TestTwoEquallyEmptyMachinesGoInParkOrder(t *testing.T) {
	f := newGatewayFixture(t, newOneUseTokens("tie-a", "tie-b"))
	f.gateway.SetRegistry(newFakeRegistry())
	loads := newFakeMachineLoads()
	f.gateway.SetMachineLoadView(loads)

	first, second, idFirst, idSecond := twoParkedMachines(t, f, "tie-a", "tie-b")
	loads.declare(idFirst, 0, 0)
	loads.declare(idSecond, 0, 0)

	ch := leaseAttempt(t, f, "run_tie")
	defer ch.Close()
	wantLeased(t, "the machine parked longest", first, second)
}

// TestOneMachineIsChosenWithoutWeighingAnything — NOTHING TO CHOOSE, NOTHING ASKED.
//
// A pool with a single machine has no placement decision in it, and a stack with no tenant on the attempt
// (the pre-E24 posture, and every wire proof that runs without a registry) has nothing to weigh with. Both
// take the path that shipped before this file, and the proof that they do is that the view is never asked:
// the OUTCOME cannot tell the two paths apart, because with one candidate every ranking returns it.
//
// It is not only a cost argument. A dial that issued a database query per handover on a single-machine
// deployment would be a new failure mode for the posture §2 protects — the query is on the path between a
// run and its machine, and the deployment it would newly be able to fail on is the one that has no second
// machine to benefit from any of this.
func TestOneMachineIsChosenWithoutWeighingAnything(t *testing.T) {
	f := newGatewayFixture(t, newOneUseTokens("lonely-token"))
	f.gateway.SetRegistry(newFakeRegistry())
	loads := newFakeMachineLoads()
	f.gateway.SetMachineLoadView(loads)

	lonely := park(t, f, "lonely-token")
	waitConnected(t, f.gateway, 1)
	time.Sleep(parkSettle)
	loads.declare(runnerIDOf(t, lonely.identity), 9, 9) // full, and it must still be handed the run

	ch := leaseAttempt(t, f, "run_lonely")
	defer ch.Close()
	select {
	case <-lonely.lease:
	case <-time.After(10 * time.Second):
		t.Fatal("the only machine in the pool was never offered the lease the gateway said it granted")
	}
	if got := loads.timesAsked(); got != 0 {
		t.Fatalf("the pool's machines were weighed %d time(s) with one machine parked, want 0 — there is nothing to choose between, and a query on the path from a run to its machine is a new way for a single-machine deployment to fail", got)
	}
}
