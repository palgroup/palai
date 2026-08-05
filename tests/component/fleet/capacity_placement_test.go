//go:build component

// PLACEMENT LOOKS AT OCCUPANCY (Faz A.4 T5), against a real PostgreSQL.
//
// lease_lifecycle_test.go holds what a hold COSTS and machine_minutes_test.go holds what is CHARGED.
// These hold the thing neither of them could: that a machine can be FULL, and that a placement onto a
// full machine is refused.
//
// WHY THERE WAS NOTHING TO EXTEND. Measured 2026-08-05, immediately before this file, `runners.capacity`
// had four kinds of toucher and not one comparison:
//
//	grep -rn '\.Capacity\|capacity' --include='*.go' packages/coordinator apps/control-plane/internal/fleet \
//	  | grep -v _test | grep -viE 'capability_workers|queue_connections'
//	→ fleet/store.go:56-57 clamps <=0 to 1, :122/:136 writes it, :315 reads it into a struct,
//	  fleet/api.go:299 and api/runners.go:666 display it. Nothing compares it to anything.
//
// The tree said so in three independent places of its own (leases.sql's AcquireLease note, background.sql,
// fleet.go:144), which is why this is a task and not a bug.
//
// AND `awaiting_capacity` IS NOT THIS. parkForCapacity's own sentence is "releases the run's compute
// because there is no machine to run it on" — the ZERO-machine case, which the gateway answers with
// ErrPoolHasNoRunner when `queue.members() == 0`. "Every machine is busy" is a different question with a
// different answer (the gateway returns ctx.Err() and the attempt rides the retry ladder), and until this
// file nothing in the tree asked it at all.
//
// THE CLAIM IS THE REFUSAL, NEVER THE COUNT. An implementation with no ceiling whatsoever opens the first
// three holds on a three-capacity machine exactly as a correct one does, so "there are three rows" is
// satisfied by the build these tests exist to fail. Every assertion below is about the acquire that must
// NOT succeed — the same lesson this tree recorded as a Limits-less descriptor making every Dial look
// successful.
package fleet

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/storage"
)

// seedMachineWithCapacity puts one more machine in this tenant's pool, declaring the capacity the test
// needs. It writes the column EXPLICITLY rather than relying on the default, because a test whose subject
// is a ceiling above one cannot take a fixture that only ever produces one.
//
// THE POOL IS READ OFF A MACHINE THE SEED ALREADY MADE rather than carried on fleetEnv. That keeps this
// file's fixture entirely inside this file: the shared seed is one the other tasks in this phase are still
// editing, and a field added there for one test here is a change to a file this test does not own.
func (e *fleetEnv) seedMachineWithCapacity(t *testing.T, capacity int) string {
	t.Helper()
	var pool string
	if err := e.cs.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT pool_id FROM runners WHERE id = $1`, e.runnerA).Scan(&pool); err != nil {
		t.Fatalf("read the seeded pool off machine %s: %v", e.runnerA, err)
	}
	runner := newID("rnr")
	e.exec(t, `INSERT INTO runners (id, project_id, pool_id, state, posture, capacity)
	           VALUES ($1, $2, $3, 'active', 'unsandboxed-host', $4)`,
		runner, e.tenant.Project, pool, capacity)
	return runner
}

// openHoldsOn counts the machine's open occupancies straight out of the table, never through the code that
// wrote them. System-scoped and keyed on the machine, so it is an assertion about THIS machine and not
// about whatever else the shared component database is doing.
func (e *fleetEnv) openHoldsOn(t *testing.T, runnerID string) int {
	t.Helper()
	var open int
	if err := e.cs.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(*) FROM runner_leases WHERE runner_id = $1 AND released_at IS NULL`, runnerID).Scan(&open); err != nil {
		t.Fatalf("count the open holds on %s: %v", runnerID, err)
	}
	return open
}

// TestTheFourthLeaseOnAThreeCapacityMachineIsRefused — A MACHINE HOLDS WHAT IT SAYS IT HOLDS, AND THE
// PROOF IS THE ONE THAT IS TURNED AWAY.
//
// Three sessions fill a machine that declares three. The fourth is REFUSED — that refusal is the whole
// test, and counting the first three would prove nothing: a build with no ceiling at all opens them too.
//
// THE CAPACITY IS THREE AND NOT ONE ON PURPOSE. At one, a ceiling and a plain "is anything holding this
// machine" flag are the same predicate, so the cheapest wrong implementation passes. Three tells them
// apart: a boolean refuses the SECOND.
//
// SESSIONS ARE DISTINCT because an occupancy is a session's hold. Four holds by one session would be
// measuring HoldMachine's open-or-keep-alive fold instead (machine_minutes_test.go owns that), and the
// question here is how many DIFFERENT tenants of a machine it will take at once.
func TestTheFourthLeaseOnAThreeCapacityMachineIsRefused(t *testing.T) {
	env := newFleetEnv(t)
	ctx := context.Background()
	machine := env.seedMachineWithCapacity(t, 3)

	for i := 0; i < 3; i++ {
		if _, err := env.cs.AcquireLease(ctx, env.tenant, env.seedSession(t), machine); err != nil {
			t.Fatalf("hold %d of 3 on a three-capacity machine = %v, want success — the refusal below would prove nothing if the machine could not be filled at all", i+1, err)
		}
	}
	// NON-VACUITY, and it is the assertion that makes the fixture honest rather than an assertion about
	// the product: unless the machine is really FULL, the refusal below is about something else.
	if open := env.openHoldsOn(t, machine); open != 3 {
		t.Fatalf("%d open hold(s) on the three-capacity machine, want 3 — it is not full, so nothing below is a statement about capacity", open)
	}

	fourth := env.seedSession(t)
	if _, err := env.cs.AcquireLease(ctx, env.tenant, fourth, machine); !errors.Is(err, coordinator.ErrMachineAtCapacity) {
		t.Fatalf("the FOURTH hold on a three-capacity machine = %v, want ErrMachineAtCapacity — the machine is over the capacity it declared, and a placement that reads capacity is the only thing that could have stopped it", err)
	}
	// A REFUSAL THAT STILL WROTE IS WORSE THAN NO REFUSAL, because the error would say "full" while the
	// row said otherwise and the bill would carry a fourth hold nobody admitted. Counted rather than
	// inferred from the return above.
	if open := env.openHoldsOn(t, machine); open != 3 {
		t.Fatalf("the refused acquire left %d open hold(s) on the machine, want 3 — the refusal was reported and the row was written anyway", open)
	}

	// AND THE REFUSAL IS ABOUT THE MACHINE, NOT ABOUT THE SESSION. The same session that was just turned
	// away takes a machine with a free slot immediately. Without this, an implementation that refused the
	// fourth SESSION — or refused everything after some global count — would pass every line above.
	if _, err := env.cs.AcquireLease(ctx, env.tenant, fourth, env.runnerA); err != nil {
		t.Fatalf("the refused session on a machine with a free slot = %v, want success — the ceiling is refusing sessions rather than counting a machine's occupancies", err)
	}
}

// TestASettledOccupancyFreesTheSlotItHeld — THE CEILING IS A LIVE OCCUPANCY, NOT A COUNTER.
//
// A machine that declares two is filled by two sessions and refuses a third. One hold SETTLES, and the
// third session gets in. That is the difference between reading `released_at IS NULL` and counting rows
// ever written: a monotonic implementation refuses the third for the rest of the machine's life, which on
// a fleet means every Mac becomes permanently unplaceable after its first few sessions.
//
// THE FOURTH REFUSAL IS THE OTHER HALF AND IT IS NOT DECORATION. Settling one hold must return exactly ONE
// slot. Without this line an implementation that stopped counting altogether once anything was released —
// or that treated a settled hold as clearing the machine — would pass on the success above alone.
func TestASettledOccupancyFreesTheSlotItHeld(t *testing.T) {
	env := newFleetEnv(t)
	ctx := context.Background()
	machine := env.seedMachineWithCapacity(t, 2)

	first := env.mustAcquire(t, env.seedSession(t), machine)
	env.mustAcquire(t, env.seedSession(t), machine)

	third := env.seedSession(t)
	if _, err := env.cs.AcquireLease(ctx, env.tenant, third, machine); !errors.Is(err, coordinator.ErrMachineAtCapacity) {
		t.Fatalf("a third hold on a full two-capacity machine = %v, want ErrMachineAtCapacity — the slot cannot come BACK below if it was never taken away", err)
	}

	// THE SLOT COMES BACK. Settled rather than merely released, because settling is what the production
	// reclaim path does (the idle releaser calls SettleOccupancy) and a ceiling that only noticed a bare
	// ReleaseLease would be reading a column nothing on that path writes.
	if !env.mustSettle(t, first, "closed") {
		t.Fatal("settling the first hold closed nothing — it was open, so this is the call that had to close it, and the slot below never freed")
	}
	if _, err := env.cs.AcquireLease(ctx, env.tenant, third, machine); err != nil {
		t.Fatalf("the third hold after a settlement = %v, want success — a settled occupancy still counts against the machine, so its slot never comes back", err)
	}

	// EXACTLY ONE SLOT CAME BACK.
	fourth := env.seedSession(t)
	if _, err := env.cs.AcquireLease(ctx, env.tenant, fourth, machine); !errors.Is(err, coordinator.ErrMachineAtCapacity) {
		t.Fatalf("a fourth hold after ONE settlement = %v, want ErrMachineAtCapacity — one hold ended and more than one slot opened", err)
	}
	if open := env.openHoldsOn(t, machine); open != 2 {
		t.Fatalf("%d open hold(s) on the two-capacity machine, want 2", open)
	}
}

// contenderSettle is how long the two contenders are given to reach the machine before the test lets go of
// it. It cannot flake: every build blocks here until the lock is RELEASED (see the note below on WHERE each
// one blocks), so no amount of waiting makes this pass or fail by accident — it only has to be long enough
// for two goroutines to issue a query, which is microseconds.
const contenderSettle = 300 * time.Millisecond

// TestTwoSessionsRacingForTheLastSlotProduceExactlyOneWinner — THE DECISION IS IN THE WRITE, SO A TIE HAS
// A LOSER, AND THE TIE IS STAGED RATHER THAN HOPED FOR.
//
// A ceiling that COUNTS and then INSERTS is not a ceiling: under READ COMMITTED two transactions take
// their snapshots before either commits, both see the machine one short of full, and both write. The
// machine ends up over the capacity it declared and nothing ever reports an error — the failure is
// invisible precisely because every caller was told yes. What prevents it is AcquireLease taking the
// MACHINE's row `FOR UPDATE` before it writes, so contenders queue instead of overlapping.
//
// THE FIRST VERSION OF THIS TEST JUST STARTED TWO GOROUTINES AT ONCE, AND IT WAS WORTHLESS. Deleting the
// `FOR UPDATE` from LockMachineForPlacement — which genuinely breaks the property — left it GREEN three
// runs out of three: the window between one acquire's snapshot and its commit is microseconds wide, and
// two goroutines released from a channel do not reliably land inside it. A test that cannot fail when the
// property breaks is not a weak test, it is a defeated assertion.
//
// SO THE WINDOW IS HELD OPEN BY THE TEST, which takes the machine's row lock itself before starting the
// contenders. With the lock staged this way the perturbation fails three runs out of three.
//
// WHERE EACH BUILD BLOCKS IS THE WHOLE MECHANISM, AND IT IS NOT WHERE IT LOOKS. Both builds block — that
// was measured, not assumed. `runner_leases.runner_id` references `runners`, so the INSERT's foreign-key
// check takes a KEY SHARE lock on the machine's row, and KEY SHARE conflicts with the FOR UPDATE this test
// holds. What differs is WHEN, relative to the count:
//
//	with the lock    : the acquire blocks in LockMachineForPlacement, BEFORE its INSERT is issued. Its
//	                   INSERT is then a new statement with a new snapshot, taken after the winner committed,
//	                   so the loser counts a FULL machine and is refused.
//	without the lock : the acquire blocks INSIDE its INSERT, at the foreign-key check — after that
//	                   statement's snapshot was taken and its count subquery already answered "one free".
//	                   Both were told yes before either could see the other, and both write.
//
// So the assertion that catches it is the WINNER COUNT and not the blocked check: the blocked check passes
// either way, and it is here to stage the tie rather than to judge it.
func TestTwoSessionsRacingForTheLastSlotProduceExactlyOneWinner(t *testing.T) {
	env := newFleetEnv(t)
	ctx := context.Background()
	machine := env.seedMachineWithCapacity(t, 2)
	env.mustAcquire(t, env.seedSession(t), machine) // one of the two slots is gone

	// The test holds the machine, so both contenders below must wait on it.
	blocker, err := env.cs.Pool().Begin(storage.WithSystemScope(ctx))
	if err != nil {
		t.Fatalf("begin the blocking transaction: %v", err)
	}
	defer func() { _ = blocker.Rollback(context.Background()) }()
	if _, err := blocker.Exec(storage.WithSystemScope(ctx),
		`SELECT capacity FROM runners WHERE id = $1 FOR UPDATE`, machine); err != nil {
		t.Fatalf("hold the machine row: %v", err)
	}

	type outcome struct {
		lease string
		err   error
	}
	contenders := []string{env.seedSession(t), env.seedSession(t)}
	done := make(chan outcome, len(contenders))
	for _, session := range contenders {
		go func(session string) {
			lease, err := env.cs.AcquireLease(ctx, env.tenant, session, machine)
			done <- outcome{lease, err}
		}(session)
	}

	// BOTH ARE WAITING ON THE MACHINE. This STAGES the tie rather than judging it — see the note above:
	// every build blocks here, so this passing says only that the contenders really did reach the machine
	// and the window below is genuinely a contended one. An acquire that finished anyway would mean it
	// never touched the machine's row at all, which is a different defect and worth its own sentence.
	time.Sleep(contenderSettle)
	select {
	case got := <-done:
		t.Fatalf("a contender finished (lease %q, err %v) while the test still held the machine's row — it reached neither the row lock nor the foreign key, so the tie below was never staged",
			got.lease, got.err)
	default:
	}

	// Release, and let the queue drain.
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatalf("release the machine row: %v", err)
	}

	var won, refused int
	for range contenders {
		switch got := <-done; {
		case got.err == nil:
			won++
		case errors.Is(got.err, coordinator.ErrMachineAtCapacity):
			refused++
		default:
			t.Fatalf("a contender for the last slot failed for an unrelated reason: %v", got.err)
		}
	}
	if won != 1 || refused != 1 {
		t.Fatalf("%d contender(s) took the last slot and %d were refused, want exactly 1 and 1 — a count-then-write ceiling lets both in, and neither caller ever learns it", won, refused)
	}
	// THE TABLE IS THE AUTHORITY. Two winners with one of them rolled back would satisfy the counts above
	// while the machine really was over its ceiling.
	if open := env.openHoldsOn(t, machine); open != 2 {
		t.Fatalf("%d open hold(s) on a machine that declared 2 — the race put the machine over the capacity it declared", open)
	}
}
