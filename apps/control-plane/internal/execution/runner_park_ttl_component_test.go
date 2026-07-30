//go:build component

package execution_test

// THE PARK REAPER (E24 T5), against a REAL Postgres, the REAL orchestrator and the REAL Reconciler.
//
// T4 filed `FLT-P7` in its own words: "a run parked in a pool that will never have a machine waits FOREVER
// … the reaper is T5's, with no default duration". This is that reaper, and the four things it has to get
// right are all things a careless version gets wrong:
//
//   - a park INSIDE the TTL is left alone (a reaper that expired everything would satisfy the expiry leg);
//   - an UNCONFIGURED deployment expires nothing at all, because there is no honest default for how long a
//     rented Mac takes to arrive;
//   - a run waiting for ANY OTHER reason — a human's pause, an approval, a detached child — is untouched;
//   - and the terminal answer NAMES the reason, or the run has died silently with extra steps.
//
// IT DRIVES THE SHIPPED Reconciler RATHER THAN THE STORE METHOD, so the TTL plumbing and the terminal
// PROJECTION under proof are production's own — a test that passed its own projection would prove the sweep
// works and say nothing about what an operator's stack stores. The other three sweeps in that pass are
// stubbed out, and that is not laziness: they are tenant-SPANNING (dead-letter, approval expiry) and this
// database is shared with every other component leg, which is exactly how E19 T9's outbound assertion
// ended up sweeping every tenant.
//
// Named `TestLifecycle*` so scripts/test/component's allow-list runs it — see the header of
// runner_lifecycle_component_test.go.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/storage"
)

// capacityOnlySpine is the real coordinator store with the OTHER three reconcile sweeps stubbed out. What
// stays real is the one under proof, and with it production's TTL argument and production's terminal
// projection.
type capacityOnlySpine struct{ *coordinator.Store }

func (capacityOnlySpine) ReclaimExpired(context.Context, int) (int, error)       { return 0, nil }
func (capacityOnlySpine) SweepDeadLetteredRuns(context.Context) (int, error)     { return 0, nil }
func (capacityOnlySpine) SweepExpiredApprovals(context.Context) (int, error)     { return 0, nil }
func (capacityOnlySpine) SweepExpiredToolApprovals(context.Context) (int, error) { return 0, nil }

// sweepParks runs ONE production reconcile pass with the given park TTL.
func sweepParks(t *testing.T, f *poolKeyFixture, ctx context.Context, ttl time.Duration) {
	t.Helper()
	rec := execution.NewReconciler(capacityOnlySpine{f.spine}, time.Hour, 5).WithCapacityParkTTL(ttl)
	if _, err := rec.Sweep(ctx); err != nil {
		t.Fatalf("reconcile pass with park TTL %v: %v", ttl, err)
	}
}

// parkOneRun drives a real ExecuteAttempt into an EMPTY pool, which is what actually parks a run: the
// marker, the state and the recorded pool all come from production code rather than from seeded rows.
func parkOneRun(t *testing.T, f *poolKeyFixture, ctx context.Context, tenant coordinator.Tenant) (runID, responseID string) {
	t.Helper()
	_, responseID, runID = placementRun(t, f.pool, tenant)
	orch := placementOrchestrator(t, f)
	attempt := f.attempt(runID, poolKeyID("att"), 1)
	attempt.Tenant = tenant
	if err := orch.ExecuteAttempt(ctx, attempt); err != nil {
		t.Fatalf("ExecuteAttempt into an empty pool: %v", err)
	}
	if got := placementRunState(t, f.pool, runID); got != "waiting" {
		t.Fatalf("run state = %q, want waiting (the park did not happen, so there is nothing to reap)", got)
	}
	return runID, responseID
}

// ageThePark moves the parked attempt's clock back, the way E23 T1's own expiry proofs move an approval's
// deadline: the age is a column, so a component test edits it rather than waiting out a TTL.
func ageThePark(t *testing.T, f *poolKeyFixture, runID string, by time.Duration) {
	t.Helper()
	if _, err := f.pool.Exec(storage.WithSystemScope(context.Background()),
		`UPDATE attempts SET updated_at = updated_at - make_interval(secs => $2)
		  WHERE run_id = $1 AND state = $3`, runID, by.Seconds(), coordinator.AttemptAwaitingCapacity); err != nil {
		t.Fatalf("age the park: %v", err)
	}
}

// TestLifecycleParkTTLEndsARunWhosePoolWillNeverHaveAMachine is the reaper's claim. The run reaches a
// TERMINAL, and the answer is asserted as hard as the state: the response body has to say a machine never
// came, because "the run is over" with no reason is the silent death this replaces.
func TestLifecycleParkTTLEndsARunWhosePoolWillNeverHaveAMachine(t *testing.T) {
	f := newLifecycleFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tenant, _, _ := placementTenant(t, f)
	runID, responseID := parkOneRun(t, f, ctx, tenant)

	// Inside the TTL: nothing moves. This leg comes first, because a reaper that expired every park would
	// pass the expiry leg below and fail here — a run whose Mac is still booting must be left alone, and
	// AWS documents that taking 6 to 20 minutes.
	sweepParks(t, f, ctx, 10*time.Minute)
	if got := placementRunState(t, f.pool, runID); got != "waiting" {
		t.Fatalf("run state = %q after a pass INSIDE the TTL, want waiting", got)
	}

	// Past the TTL: the run ends, with an answer.
	ageThePark(t, f, runID, 11*time.Minute)
	sweepParks(t, f, ctx, 10*time.Minute)
	if got := placementRunState(t, f.pool, runID); got != "timed_out" {
		t.Fatalf("run state = %q after its park expired, want timed_out — T4's FLT-P7 says such a run waits FOREVER without this", got)
	}
	// The RESPONSE is what a caller reads, and it has to name the reason. The queue deadline's wording
	// ("exceeded its execution deadline") would send its owner hunting for a slow model.
	var state string
	var body []byte // `responses.output` is the terminal projection column
	if err := f.pool.QueryRow(storage.WithSystemScope(ctx),
		`SELECT state, output FROM responses WHERE id = $1`, responseID).Scan(&state, &body); err != nil {
		t.Fatalf("read the finalized response: %v", err)
	}
	if state != "timed_out" {
		t.Fatalf("response state = %q, want timed_out — a terminal run with a non-terminal response is a stream that never ends", state)
	}
	var view struct {
		Error struct {
			Code   string `json:"code"`
			Detail string `json:"detail"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("decode the response body %s: %v", body, err)
	}
	if view.Error.Code != "operation_timed_out" {
		t.Fatalf("response error code = %q, want operation_timed_out", view.Error.Code)
	}
	if !strings.Contains(view.Error.Detail, "no runner joined") {
		t.Fatalf("response detail = %q — it does not say a machine never came, so the run died silently with extra steps", view.Error.Detail)
	}

	// Idempotent: a second pass finds a terminal run and moves nothing.
	sweepParks(t, f, ctx, 10*time.Minute)
	if got := placementRunState(t, f.pool, runID); got != "timed_out" {
		t.Fatalf("run state = %q after a second pass, want timed_out", got)
	}
}

// TestLifecycleParkTTLUnsetExpiresNothing is the shipped default, and it is a claim rather than an absence:
// a deployment that configures no TTL behaves exactly as it did before this task, however long a run has
// been parked. A default duration here would be this binary guessing how long somebody else's rented Mac
// takes to arrive.
func TestLifecycleParkTTLUnsetExpiresNothing(t *testing.T) {
	f := newLifecycleFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tenant, _, _ := placementTenant(t, f)
	runID, _ := parkOneRun(t, f, ctx, tenant)
	ageThePark(t, f, runID, 72*time.Hour)

	for _, ttl := range []time.Duration{0, -time.Minute} {
		sweepParks(t, f, ctx, ttl)
		if got := placementRunState(t, f.pool, runID); got != "waiting" {
			t.Fatalf("run state = %q after a pass with ttl %v, want waiting — an operator who set nothing must get today's behaviour", got, ttl)
		}
	}
}

// TestLifecycleParkTTLLeavesARunWaitingForAnyOtherReason is the guard that makes this safe on every
// reconcile tick, and it is the same guard T4 needed for the wake: `waiting` is FOUR conditions — a human's
// pause, an approval, a detached child, no capacity — and only the last is this reaper's. A run a person
// paused must not be timed out by a fleet knob.
//
// The distinction is the ATTEMPT's marker, so the test removes exactly that and nothing else: the run stays
// `waiting`, stays ancient, and must be untouched.
func TestLifecycleParkTTLLeavesARunWaitingForAnyOtherReason(t *testing.T) {
	f := newLifecycleFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tenant, _, _ := placementTenant(t, f)
	runID, _ := parkOneRun(t, f, ctx, tenant)
	ageThePark(t, f, runID, 72*time.Hour)
	// Same run, same age, same `waiting` state — parked for a DIFFERENT reason.
	if _, err := f.pool.Exec(storage.WithSystemScope(ctx),
		`UPDATE attempts SET state = 'preempted' WHERE run_id = $1 AND state = $2`,
		runID, coordinator.AttemptAwaitingCapacity); err != nil {
		t.Fatalf("re-mark the attempt: %v", err)
	}
	sweepParks(t, f, ctx, time.Minute)
	if got := placementRunState(t, f.pool, runID); got != "waiting" {
		t.Fatalf("run state = %q, want waiting — a fleet knob must not end a run a person paused", got)
	}
}
