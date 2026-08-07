//go:build component

// THE MACHINE HALF OF THE BILL (Faz A.4 T4), against a real PostgreSQL.
//
// lease_lifecycle_test.go holds the arithmetic of an occupancy — what a hold COSTS. These hold the
// settlement — what is actually written down, and charged. The two are separate subjects and the gap
// between them is where this task lived: measured 2026-08-05, before this file,
//
//	grep -rn 'machine\.minutes' --include='*.go' . | grep -v node_modules | wc -l   -> 0
//	for f in AcquireLease TouchLease ReleaseLease SessionOccupancies; do
//	  grep -rn "\.$f(" --include='*.go' apps cmd packages adapters | grep -v _test | grep -v 'func ' | wc -l
//	done                                                                            -> 0 0 0 0
//
// a Mac could be held for a week and the only thing usage_ledger knew about it was the tokens.
//
// THEY ASSERT ON THE LEDGER AND NOT ON `Occupancy.Billed`, which is the whole point of being a separate
// file. `Billed` is derived on every read, so a build that computes it perfectly and settles nothing —
// which is exactly the build that shipped — satisfies every assertion in lease_lifecycle_test.go. What a
// customer is charged is the row, and only the row.
package fleet

import (
	"context"
	"testing"
	"time"

	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/storage"
)

// ledgerRow is one settled usage_ledger entry as these tests read it — straight out of the table, never
// through the code that wrote it.
type ledgerRow struct {
	meter     string
	unit      string
	quantity  float64
	dedupeKey string
	sessionID string
	runID     string
	runnerID  string
}

// machineRows reads every machine-time entry settled for this tenant, oldest first.
//
// IT IS SCOPED TO THE TENANT AND ORDERED, and both matter. This suite's PostgreSQL is shared by every
// other component leg, so an unscoped count would be an assertion about the neighbours; and a result with
// no ORDER BY is not a sequence, which in this tree has decided the wrong thing more than once. Each
// fleetEnv mints its own project, so the scope is exact.
func (e *fleetEnv) machineRows(t *testing.T) []ledgerRow {
	t.Helper()
	rows, err := e.cs.Pool().Query(storage.WithSystemScope(context.Background()),
		`SELECT meter, unit, quantity, dedupe_key, coalesce(session_id, ''), coalesce(run_id, ''),
		        coalesce(runner_id, '')
		   FROM usage_ledger WHERE project_id = $1 AND meter = 'machine.minutes'
		  ORDER BY occurred_at, id`, e.tenant.Project)
	if err != nil {
		t.Fatalf("read settled machine time: %v", err)
	}
	defer rows.Close()
	var out []ledgerRow
	for rows.Next() {
		var r ledgerRow
		if err := rows.Scan(&r.meter, &r.unit, &r.quantity, &r.dedupeKey, &r.sessionID, &r.runID, &r.runnerID); err != nil {
			t.Fatalf("scan settled machine time: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read settled machine time: %v", err)
	}
	return out
}

// mustSettle closes an occupancy and settles it, failing on error and reporting whether THIS call is the
// one that closed it.
func (e *fleetEnv) mustSettle(t *testing.T, leaseID, reason string) bool {
	t.Helper()
	closed, err := e.cs.SettleOccupancy(context.Background(), e.tenant, leaseID, reason)
	if err != nil {
		t.Fatalf("SettleOccupancy(%s, %q) error = %v", leaseID, reason, err)
	}
	return closed
}

// minutes renders a settled quantity as a duration, so a failure message reads in the units the rest of
// this package talks in rather than in fractions of a minute.
func minutes(quantity float64) time.Duration {
	return time.Duration(quantity * float64(time.Minute))
}

func sumQuantities(rows []ledgerRow) float64 {
	total := 0.0
	for _, r := range rows {
		total += r.quantity
	}
	return total
}

// settlementHold is how long each occupancy in these tests is actually held. It is comfortably longer than
// the clock noise between a client-side sleep and a server-side clock_timestamp(), so an assertion about
// the amount is an assertion about the amount.
const settlementHold = 900 * time.Millisecond

// TestTwoOccupanciesSettleTwoLedgerRowsAndTheSumSkipsTheGap — TWO HOLDS, TWO ROWS, AND THE CUSTOMER PAYS
// FOR NEITHER THE GAP NOR THE SESSION.
//
// A session is closed for idleness and resumes on whichever machine is free. That is two holds with real
// time in between in which it held NOTHING. The bill is the sum of the holds. A settlement keyed on the
// SESSION would write one row spanning both and charge the gap; a settlement keyed on the RUN would write
// one per message.
func TestTwoOccupanciesSettleTwoLedgerRowsAndTheSumSkipsTheGap(t *testing.T) {
	env := newFleetEnv(t)
	session := env.seedSession(t)

	first := env.mustAcquire(t, session, env.runnerA)
	time.Sleep(settlementHold)
	env.mustSettle(t, first, "closed")

	// The session holds NOTHING here. Whatever this costs the customer is the defect being measured.
	time.Sleep(occupancyGap)

	second := env.mustAcquire(t, session, env.runnerB)
	time.Sleep(settlementHold)
	env.mustSettle(t, second, "closed")

	rows := env.machineRows(t)
	if len(rows) != 2 {
		t.Fatalf("%d machine.minutes row(s) for two holds, want 2 — a session occupies a machine N times and each hold is billed on its own", len(rows))
	}
	// THE TWO ROWS ARE TWO IDENTITIES. If both settlements derived the same dedupe key the second would
	// have been swallowed by `ON CONFLICT DO NOTHING` and this test would be measuring one hold twice.
	if rows[0].dedupeKey == rows[1].dedupeKey {
		t.Fatalf("both holds settled under dedupe key %q — the key does not identify the OCCUPANCY, so a session's second hold is free", rows[0].dedupeKey)
	}
	for _, r := range rows {
		if r.unit != "minute" {
			t.Errorf("settled unit = %q, want minute — an exporter prices the quantity by its unit", r.unit)
		}
		if r.sessionID != session {
			t.Errorf("settled session = %q, want %q — machine time nobody can attribute is machine time nobody pays for", r.sessionID, session)
		}
		// run_id is EMPTY on purpose: one occupancy hosts many runs, so naming any of them would attribute
		// a whole hold to whichever run happened to be last.
		if r.runID != "" {
			t.Errorf("settled run = %q, want empty — an occupancy is not a run's, it spans every run that landed on it", r.runID)
		}
	}
	if t.Failed() {
		return
	}

	// THE ROWS SAY EXACTLY WHAT THE OCCUPANCIES SAY. The billed interval is derived by one `CASE` in
	// storage/queries/leases.sql; a settlement that recomputed it in Go would be a second answer, and this
	// is where the two would part company.
	wantFirst, wantSecond := env.billed(t, first), env.billed(t, second)
	if got := minutes(rows[0].quantity); !within(got, wantFirst, time.Millisecond) {
		t.Fatalf("first hold settled %s, the occupancy says %s", got, wantFirst)
	}
	if got := minutes(rows[1].quantity); !within(got, wantSecond, time.Millisecond) {
		t.Fatalf("second hold settled %s, the occupancy says %s", got, wantSecond)
	}

	// AND THE GAP IS NOT IN THERE. This is the assertion a session-spanning settlement fails: it would
	// land on the wall clock exactly.
	held := env.occupancies(t, session)
	wall := held[1].ReleasedAt.Sub(held[0].StartedAt)
	settled := minutes(sumQuantities(rows))
	if wall-settled < occupancyGap*3/4 {
		t.Fatalf("the two holds settled %s against a session span of %s — the %s in which this session held no machine was charged to the customer",
			settled, wall, occupancyGap)
	}
}

// TestAnIdleSettlementStopsAtLastActivityAndNotAtTheLateReaper — THE QUIET TAIL IS NOT ON THE INVOICE.
//
// The bill for an idle-closed hold stops at the customer's LAST ACTIVITY. The tail after it is what the
// platform pays for keeping a machine warm in case they came back. lease_lifecycle_test.go already holds
// this for the DERIVED interval; this holds it for the SETTLED one, which is the number that is charged —
// a build that derived it correctly and then settled `released_at - started_at` would pass that test and
// fail this one.
//
// THE REAPER IS DELIBERATELY LATE, because a sweep is late by construction: it runs on a tick, not on an
// alarm. Subtracting a fixed TTL is not a hypothetical mistake, it is what a reaper-shaped implementation
// produces.
func TestAnIdleSettlementStopsAtLastActivityAndNotAtTheLateReaper(t *testing.T) {
	env := newFleetEnv(t)
	session := env.seedSession(t)
	lease := env.mustAcquire(t, session, env.runnerA)

	time.Sleep(600 * time.Millisecond)
	env.mustTouch(t, lease) // the last thing this session ever did

	// The TTL would have expired ~1.6s from now; the reaper does not arrive until well after that.
	time.Sleep(2400 * time.Millisecond)
	if !env.mustSettle(t, lease, coordinator.ReleaseReasonIdle) {
		t.Fatal("SettleOccupancy reported it closed nothing — the hold was open, so this is the call that had to close it")
	}

	row := env.occupancies(t, session)[0]
	// The Go constant is closed over the DATABASE's word here, exactly as the derived test does it: the
	// branch is a literal 'idle' in storage/queries/leases.sql and a CHECK in migration
	// 000003_lease_occupancy, and neither knows coordinator.ReleaseReasonIdle exists.
	if row.ReleaseReason != "idle" {
		t.Fatalf("release reason = %q, want idle — the billing rule keys on that exact word, so a hold closed under any other one bills the whole tail", row.ReleaseReason)
	}

	toLastActivity := row.LastActivityAt.Sub(row.StartedAt) // the right answer
	toRelease := row.ReleasedAt.Sub(row.StartedAt)          // charges the whole idle tail
	toFixedTTL := toRelease - reaperIdleTTL                 // charges however late the reaper was

	// NON-VACUITY: unless the three candidates are genuinely apart, every assertion below is satisfied by
	// all three and this test says nothing.
	if toRelease-toLastActivity < 2*billingTolerance || toFixedTTL-toLastActivity < 2*billingTolerance {
		t.Fatalf("the three candidate answers are %s / %s / %s — they are not separated by more than the %s tolerance, so this test cannot distinguish them",
			toLastActivity, toFixedTTL, toRelease, billingTolerance)
	}

	rows := env.machineRows(t)
	if len(rows) != 1 {
		t.Fatalf("%d machine.minutes row(s), want 1", len(rows))
	}
	settled := minutes(rows[0].quantity)
	if diff := settled - toLastActivity; diff < -billingTolerance || diff > billingTolerance {
		t.Fatalf("settled %s, want %s (last_activity_at - started_at)%s",
			settled, toLastActivity, diagnoseBilling(settled, toRelease, toFixedTTL))
	}
}

// TestARepeatedReleaseSettlesTheSameHoldOnce — A REDELIVERED RELEASE IS NOT A SECOND BILL.
//
// A release is repeated by ordinary means: a retried command, a sweep meeting a session that closed
// itself, a control plane that restarted mid-pass. Two things have to hold, and they are NOT the same
// thing, because they protect against different callers:
//
//	the CLOSE is write-once   — the second call finds `released_at` already set and closes nothing, so it
//	                            never reaches the ledger. This also protects the AMOUNT: a release that
//	                            moved released_at forward would raise a bill that had already settled.
//	the KEY is the occupancy  — even a settle that DID run twice collides on (project_id, dedupe_key) and
//	                            inserts once. This is what covers two paths racing to close one hold.
//
// Asserting only the row count would leave it unknown which of the two did the work, and a build with just
// one of them passes today and breaks the first time the other case happens.
func TestARepeatedReleaseSettlesTheSameHoldOnce(t *testing.T) {
	env := newFleetEnv(t)
	session := env.seedSession(t)
	lease := env.mustAcquire(t, session, env.runnerA)
	time.Sleep(settlementHold)

	if !env.mustSettle(t, lease, "closed") {
		t.Fatal("the first settle closed nothing — the hold was open, so the redelivery below would prove nothing")
	}
	first := env.machineRows(t)
	if len(first) != 1 {
		t.Fatalf("%d machine.minutes row(s) after one release, want 1", len(first))
	}

	// THE REDELIVERY.
	if env.mustSettle(t, lease, "closed") {
		t.Fatal("a repeated release reported that it CLOSED the hold — released_at moved forward, which raises a bill that had already settled")
	}
	after := env.machineRows(t)
	if len(after) != 1 {
		t.Fatalf("%d machine.minutes row(s) after a redelivered release, want 1 — the customer was charged twice for one hold", len(after))
	}
	if after[0].quantity != first[0].quantity {
		t.Fatalf("the settled amount moved from %v to %v minutes on redelivery — a repeat must change nothing at all", first[0].quantity, after[0].quantity)
	}
	// AND THE KEY IS THE HOLD'S OWN, which is what makes the collision above possible at all. Without the
	// lease id in it, two holds of one session would share a key and the SECOND hold would be free — the
	// failure this assertion exists for is the opposite of the one the count above catches.
	if want := "lease:" + lease + ":machine.minutes"; after[0].dedupeKey != want {
		t.Fatalf("dedupe key = %q, want %q — the key must name the OCCUPANCY, because that is the thing being settled exactly once", after[0].dedupeKey, want)
	}
}

// TestAHoldThatMovedMachinesIsClosedAndBilledRatherThanLeftOpen — A HOLD NOBODY CLOSED IS UNBILLED MACHINE
// TIME, so acquiring elsewhere closes it.
//
// HoldMachine is what the attempt path calls, and it is open-or-keep-alive rather than plain acquire: the
// same session on the same machine keeps ONE row alive across every run that lands on it, and the same
// session on a DIFFERENT machine has demonstrably left the first, so that hold ended even though nothing
// witnessed it end. Left open it would be billed to nobody and — from T5 on — would make a machine look
// occupied forever.
func TestAHoldThatMovedMachinesIsClosedAndBilledRatherThanLeftOpen(t *testing.T) {
	env := newFleetEnv(t)
	ctx := context.Background()
	session := env.seedSession(t)

	first, err := env.cs.HoldMachine(ctx, env.tenant, session, env.runnerA)
	if err != nil {
		t.Fatalf("HoldMachine(%s) error = %v", env.runnerA, err)
	}
	time.Sleep(settlementHold)

	// A SECOND RUN OF THE SAME SESSION ON THE SAME MACHINE KEEPS THE SAME HOLD. A row per attempt would
	// bill a conversation once per message.
	again, err := env.cs.HoldMachine(ctx, env.tenant, session, env.runnerA)
	if err != nil {
		t.Fatalf("HoldMachine(%s) a second time error = %v", env.runnerA, err)
	}
	if again != first {
		t.Fatalf("the same session on the same machine opened a second hold (%s then %s) — every message would be billed as its own occupancy", first, again)
	}

	// AND NOW IT IS SOMEWHERE ELSE.
	second, err := env.cs.HoldMachine(ctx, env.tenant, session, env.runnerB)
	if err != nil {
		t.Fatalf("HoldMachine(%s) error = %v", env.runnerB, err)
	}
	if second == first {
		t.Fatal("moving machines reused the same occupancy — one row cannot describe two machines, and the bill would name the wrong one")
	}

	held := env.occupancies(t, session)
	if len(held) != 2 {
		t.Fatalf("%d occupancy row(s), want 2", len(held))
	}
	if held[0].ReleasedAt.IsZero() {
		t.Fatal("the hold on the first machine is still open — it is billed to nobody, and placement will read that machine as occupied forever")
	}
	rows := env.machineRows(t)
	if len(rows) != 1 {
		t.Fatalf("%d machine.minutes row(s), want 1 — the abandoned hold was closed, so its time must be on the invoice", len(rows))
	}
	if got, want := minutes(rows[0].quantity), env.billed(t, first); !within(got, want, time.Millisecond) {
		t.Fatalf("the abandoned hold settled %s, the occupancy says %s", got, want)
	}
	// The hold that is still LIVE settles nothing yet, which is the other half of the same rule: a bill is
	// written when a hold ENDS, and an open hold has no final amount to write.
	if !held[1].ReleasedAt.IsZero() {
		t.Fatal("the new hold was closed as soon as it opened — the session is on that machine right now")
	}
}

// TestEachSettledHoldNamesTheMachineItWasBilledOn — THE METER IS CALLED machine.minutes AND THE ROW
// COULD NOT NAME THE MACHINE.
//
// A settled `machine.minutes` row carries a session, a quantity and a dedupe key. `run_id` is empty on
// purpose (one occupancy hosts many runs) and `model_request_id` means nothing for a hold, so until
// 000011 the only trace of WHICH Mac a customer's minutes ran on was the dedupe key's string shape —
// `lease:<id>:machine.minutes`, joined back to runner_leases. A dedupe key exists to make a settlement
// idempotent. Reading identity out of it is the same defeated design this tree keeps finding in path and
// membership comparisons, and it is why the id is now a column.
//
// THE ASSERTION IS PER-ROW AND ORDER-INDEPENDENT, which is the whole strength of it. Counting distinct
// machines would pass for a ledger that swapped the two ids; asserting "some row names runnerA" would
// pass for a ledger that wrote runnerA twice. Each row is matched to ITS OWN lease through the dedupe
// key and then asked which machine it names, so a constant, a swap and an empty column all fail — and
// the A.4 exit gate's claim ("machine.minutes ledger rows carry runner_id") has a witness that means it.
func TestEachSettledHoldNamesTheMachineItWasBilledOn(t *testing.T) {
	env := newFleetEnv(t)
	session := env.seedSession(t)

	// One session, two machines, in sequence — the shape a customer produces by being moved.
	first := env.mustAcquire(t, session, env.runnerA)
	time.Sleep(settlementHold)
	env.mustSettle(t, first, "closed")

	second := env.mustAcquire(t, session, env.runnerB)
	time.Sleep(settlementHold)
	env.mustSettle(t, second, "closed")

	byLease := map[string]string{}
	for _, r := range env.machineRows(t) {
		byLease[r.dedupeKey] = r.runnerID
	}
	if len(byLease) != 2 {
		t.Fatalf("%d settled hold(s), want 2 — this test needs both to exist before it can ask which machine each names", len(byLease))
	}
	for _, want := range []struct{ lease, machine string }{
		{first, env.runnerA},
		{second, env.runnerB},
	} {
		key := "lease:" + want.lease + ":machine.minutes"
		got, ok := byLease[key]
		if !ok {
			t.Fatalf("no settled row for hold %s — the dedupe key this test matches on is not the one the settlement wrote", want.lease)
		}
		if got == "" {
			t.Errorf("hold %s settled with no machine: a machine.minutes row that cannot say which Mac it billed leaves the id recoverable only by parsing its own dedupe key", want.lease)
			continue
		}
		if got != want.machine {
			t.Errorf("hold %s billed to machine %q, want %q — the ledger attributes this customer's minutes to the wrong Mac", want.lease, got, want.machine)
		}
	}
}
