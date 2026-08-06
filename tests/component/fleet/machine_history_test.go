//go:build component

package fleet

// ONE MACHINE'S SESSION HISTORY, against real PostgreSQL — the read behind
// GET /v1/runners/{id}/occupancies (device plan T6, DoD 10).
//
// ‼️ THEY EXIST BECAUSE THE ROUTE SHIPPED WITH NOTHING DRIVING ITS BEHAVIOUR. The only thing that
// touched it was TestEveryFleetRouteRefusesATenantKey, which sweeps every fleet route for its AUTH gate
// — a guard that would stay green while the page dropped rows, ordered them oldest-first, or answered
// another tenant's machine. A sweep proves the property it sweeps for and nothing else.

import (
	"context"
	"testing"
	"time"

	"github.com/palgroup/palai/packages/coordinator"
)

// machineHistory reads one page the way the handler does.
func (e *fleetEnv) machineHistory(t *testing.T, runnerID string, before time.Time, beforeID string, limit int) []coordinator.Occupancy {
	t.Helper()
	rows, err := e.cs.MachineOccupancies(context.Background(), e.tenant, runnerID, before, beforeID, limit)
	if err != nil {
		t.Fatalf("MachineOccupancies(%s) error = %v", runnerID, err)
	}
	return rows
}

// TestAMachinesHistoryIsNewestFirstAndPagesWithoutLosingAHold is the ordering and the cursor in one
// measurement, because they are one property: a page is only correct if the next page continues from it.
//
// ‼️ THE CURSOR IS (started_at, id) AND THE PAIR IS THE POINT. Three holds seeded back to back can share
// a clock tick, and an ordering on time alone would let a LIMIT drop or repeat one between requests.
// This tree has had an unordered LIMIT decide a security outcome twice; a paginated history is the same
// hazard wearing a UI.
func TestAMachinesHistoryIsNewestFirstAndPagesWithoutLosingAHold(t *testing.T) {
	env := newFleetEnv(t)
	var ids []string
	for i := 0; i < 3; i++ {
		lease := env.mustAcquire(t, env.seedSession(t), env.runnerA)
		env.mustRelease(t, lease, "closed")
		ids = append(ids, lease)
	}

	all := env.machineHistory(t, env.runnerA, time.Time{}, "", 50)
	if len(all) != 3 {
		t.Fatalf("history has %d holds, want 3", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i-1].StartedAt.Before(all[i].StartedAt) {
			t.Fatalf("hold %d started before hold %d: the page is not newest-first", i-1, i)
		}
	}

	// ‼️ THE TIE IS FORCED, BECAUSE THE FIXTURE COULD NOT PRODUCE ONE. Three holds acquired in sequence
	// get distinct microsecond timestamps, so a cursor with no tie-breaker paged them correctly and the
	// perturbation that removes it stayed GREEN — the test was measuring a world its own setup made
	// unreachable. Stamping two rows to the SAME started_at is what makes the case exist at all.
	env.exec(t, `UPDATE runner_leases SET started_at = (SELECT max(started_at) FROM runner_leases WHERE runner_id = $1)
	             WHERE runner_id = $1`, env.runnerA)
	tied := env.machineHistory(t, env.runnerA, time.Time{}, "", 50)
	if len(tied) != 3 {
		t.Fatalf("after forcing the tie the history has %d holds, want 3", len(tied))
	}

	// Page of one, then continue from it. Every hold must appear exactly once across the two reads —
	// which is what a tie-broken cursor buys and a timestamp-only one does not.
	first := env.machineHistory(t, env.runnerA, time.Time{}, "", 1)
	if len(first) != 1 {
		t.Fatalf("limit 1 returned %d rows", len(first))
	}
	rest := env.machineHistory(t, env.runnerA, first[0].StartedAt, first[0].ID, 50)
	seen := map[string]int{first[0].ID: 1}
	for _, o := range rest {
		seen[o.ID]++
	}
	for _, id := range ids {
		if seen[id] != 1 {
			t.Fatalf("hold %s appears %d times across the two pages, want exactly 1: a cursor that cannot break a tie drops or repeats rows", id, seen[id])
		}
	}
}

// TestAnOpenHoldAndAClosedOneAreDifferentAnswers guards what the panel renders. An open hold has no
// release: reporting one would tell an operator a machine is free when it is serving a session.
func TestAnOpenHoldAndAClosedOneAreDifferentAnswers(t *testing.T) {
	env := newFleetEnv(t)
	closed := env.mustAcquire(t, env.seedSession(t), env.runnerA)
	env.mustRelease(t, closed, "closed")
	open := env.mustAcquire(t, env.seedSession(t), env.runnerA)

	byID := map[string]coordinator.Occupancy{}
	for _, o := range env.machineHistory(t, env.runnerA, time.Time{}, "", 50) {
		byID[o.ID] = o
	}
	if got := byID[open]; !got.ReleasedAt.IsZero() || got.ReleaseReason != "" {
		t.Fatalf("an OPEN hold reports released_at=%v reason=%q: the panel would show a machine as free while it is serving", got.ReleasedAt, got.ReleaseReason)
	}
	if got := byID[closed]; got.ReleasedAt.IsZero() || got.ReleaseReason == "" {
		t.Fatalf("a CLOSED hold reports released_at=%v reason=%q: a released occupancy must stay in history WITH its reason", got.ReleasedAt, got.ReleaseReason)
	}
	// An open hold still bills the time it has held so far — "in progress" is not "free".
	if byID[open].Billed <= 0 {
		t.Fatalf("an open hold bills %v, want a positive elapsed interval", byID[open].Billed)
	}
}

// TestAMachineOfAnotherTenantHasNoHistoryHere is the scope in the QUERY, not only in the handler. The
// route resolves the machine first and answers 404, and this asserts the second, independent refusal:
// even asked directly, the read returns nothing for a machine this tenant does not own.
func TestAMachineOfAnotherTenantHasNoHistoryHere(t *testing.T) {
	env := newFleetEnv(t)
	lease := env.mustAcquire(t, env.seedSession(t), env.runnerA)
	env.mustRelease(t, lease, "closed")

	stranger := seedFleet(t, env.cs)
	if rows := stranger.machineHistory(t, env.runnerA, time.Time{}, "", 50); len(rows) != 0 {
		t.Fatalf("another tenant read %d holds of this machine, want 0: the WHERE is the boundary, not the handler alone", len(rows))
	}
}
