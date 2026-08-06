//go:build component

// A SHARED MAC SERVES ONE CUSTOMER AT A TIME (000008), against a real PostgreSQL.
//
// capacity_placement_test.go holds how many SESSIONS a machine takes. This holds whose sessions may be on
// it AT ONCE, and the two are different questions that looked like one until 2026-08-06: both machines
// this plane had enrolled were free-fleet (`project_id IS NULL`) and `capacity = 0` — which the acquire
// reads as "undeclared, admits everything" — so an unlimited number of DIFFERENT customers could hold one
// Mac simultaneously, and every layer would have reported success.
//
// ‼️ THE PLATFORM ALREADY SAID THEY MAY NOT. docs/operations/mac-sessions.md §5: "Different customers can
// share a Mac — **no**", because `sudo` and any local-root escalation defeats a uid boundary and three
// such escalations were patched in 2026 alone. That row was a claim about what the product permits and
// nothing enforced it — the shape this tree keeps finding, where a declaration is read as a happening.
//
// THE CLAIM IS THE REFUSAL, never the count, for capacity_placement_test.go's reason: an implementation
// with no exclusivity whatsoever opens the first tenant's hold exactly as a correct one does.
//
// ‼️ AND THE MACHINE DECLARES CAPACITY 3 ON PURPOSE, WHICH IS WHAT MAKES THIS A TEST OF TENANCY. On a
// one-capacity machine "another tenant holds it" and "it is full" are the same predicate, so the ceiling
// alone would pass every line below and the property would be untested. At three, the machine has ROOM
// when the second customer is turned away — so only a tenancy rule can be what turned them away, and the
// error is asserted by NAME to keep the two apart.
package fleet

import (
	"context"
	"errors"
	"testing"

	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/storage"
)

// seedFreeFleetMachine puts a machine on a PLANE-owned pool — `project_id IS NULL` on both the pool and
// the machine — which is the only arrangement in which this question can arise at all. A machine owned by
// a project is reachable by that project alone, so exclusivity there is a consequence of tenancy rather
// than a rule; the free fleet is where "devices are free, and customers take turns on them" lives.
func seedFreeFleetMachine(t *testing.T, e *fleetEnv, capacity int) string {
	t.Helper()
	pool, runner := newID("pool"), newID("rnr")
	e.exec(t, `INSERT INTO runner_pools (id, project_id, name, posture)
	           VALUES ($1, NULL, $2, 'unsandboxed-host')`, pool, "plane-"+pool)
	e.exec(t, `INSERT INTO runners (id, project_id, pool_id, state, posture, capacity)
	           VALUES ($1, NULL, $2, 'active', 'unsandboxed-host', $3)`, runner, pool, capacity)
	return runner
}

func openHoldsOnMachine(t *testing.T, e *fleetEnv, runnerID string) int {
	t.Helper()
	var open int
	if err := e.cs.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(*) FROM runner_leases WHERE runner_id = $1 AND released_at IS NULL`, runnerID).Scan(&open); err != nil {
		t.Fatalf("count the open holds on %s: %v", runnerID, err)
	}
	return open
}

func TestASecondCustomerIsRefusedAMachineTheFirstIsHolding(t *testing.T) {
	cs := openHarness(t)
	first, second := seedFleet(t, cs), seedFleet(t, cs)
	ctx := context.Background()
	machine := seedFreeFleetMachine(t, first, 3)

	if _, err := first.cs.AcquireLease(ctx, first.tenant, first.seedSession(t), machine); err != nil {
		t.Fatalf("the first customer's hold on a free-fleet machine = %v, want success — the refusal below "+
			"would prove nothing if nobody could take the machine at all", err)
	}

	// NON-VACUITY, and here it carries more weight than a row count usually does: unless the machine still
	// has ROOM, the refusal below is indistinguishable from the ceiling capacity_placement_test.go owns.
	if open := openHoldsOnMachine(t, first, machine); open != 1 {
		t.Fatalf("%d open hold(s) after the first customer, want 1", open)
	}

	_, err := second.cs.AcquireLease(ctx, second.tenant, second.seedSession(t), machine)
	if !errors.Is(err, coordinator.ErrMachineHeldByAnotherTenant) {
		t.Fatalf("a SECOND customer on a machine the first is holding = %v, want ErrMachineHeldByAnotherTenant "+
			"— the machine has two free slots, so a ceiling cannot be what refuses this and nothing else "+
			"would have", err)
	}
	// A REFUSAL THAT STILL WROTE is the failure that reads as working: the error would name a boundary
	// while two customers shared the Mac and the bill carried both. Counted, never inferred.
	if open := openHoldsOnMachine(t, first, machine); open != 1 {
		t.Fatalf("the refused acquire left %d open hold(s), want 1 — the refusal was reported and the row "+
			"was written anyway", open)
	}

	// AND IT IS EXCLUSIVITY RATHER THAN "ONE HOLD PER MACHINE". The FIRST customer takes a second slot on
	// the same Mac immediately. Without this line an implementation that refused every second hold — the
	// cheapest wrong one — passes everything above and silently caps every shared machine at one session.
	if _, err := first.cs.AcquireLease(ctx, first.tenant, first.seedSession(t), machine); err != nil {
		t.Fatalf("the first customer's SECOND hold on the same machine = %v, want success — the rule is one "+
			"customer at a time, not one session at a time, and a machine declaring 3 must serve its own "+
			"tenant three", err)
	}
}

func TestTheMachineOpensToTheNextCustomerWhenTheFirstSettles(t *testing.T) {
	cs := openHarness(t)
	first, second := seedFleet(t, cs), seedFleet(t, cs)
	ctx := context.Background()
	machine := seedFreeFleetMachine(t, first, 3)

	// TWO HOLDS, AND BOTH MUST SETTLE. A rule written as "is any OTHER tenant's hold open" is satisfied by
	// the last one closing; one written against a stale count, or against the first release it sees, opens
	// the machine while the first customer is still on it. Taking two is what tells those apart.
	held := first.mustAcquire(t, first.seedSession(t), machine)
	stillHeld := first.mustAcquire(t, first.seedSession(t), machine)

	if err := first.cs.ReleaseLease(ctx, first.tenant, held, "idle"); err != nil {
		t.Fatalf("settle the first customer's first hold: %v", err)
	}
	if _, err := second.cs.AcquireLease(ctx, second.tenant, second.seedSession(t), machine); !errors.Is(err, coordinator.ErrMachineHeldByAnotherTenant) {
		t.Fatalf("the next customer with ONE of two holds settled = %v, want ErrMachineHeldByAnotherTenant "+
			"— the first customer is still on the machine", err)
	}

	if err := first.cs.ReleaseLease(ctx, first.tenant, stillHeld, "idle"); err != nil {
		t.Fatalf("settle the first customer's last hold: %v", err)
	}
	// THE REFUSAL IS TRANSIENT AND THIS IS THE HALF THAT PROVES IT. A rule that never reopens is not a
	// boundary, it is a machine that serves one customer and then nobody — which on a fleet is every Mac
	// going dark after its first tenant, the same shape the liveness predicate exists to prevent.
	if _, err := second.cs.AcquireLease(ctx, second.tenant, second.seedSession(t), machine); err != nil {
		t.Fatalf("the next customer after the machine emptied = %v, want success — an exclusivity that does "+
			"not reopen has taken the Mac out of the fleet", err)
	}
}
