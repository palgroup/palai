//go:build component

package execution_test

// RUNS THAT NEVER SETTLE, against a REAL Postgres, the REAL gateway and the REAL orchestrator.
//
// Both proofs here were born RED against a live stack on 2026-08-02, and both are the same family: a
// run whose durable state was written by one path while the row that would move it onward was written
// by another path, or by none.
//
//   - THE UNWAKEABLE PARK. Two runs on the live stack had been `waiting` with an `awaiting_capacity`
//     attempt for THIRTY-ONE HOURS. Their `runs.pool_id` was NULL, and `OldestRunAwaitingCapacity`
//     — the only wake — matches on `r.pool_id = $3`. NULL matches nothing, so no machine's arrival
//     could ever reach them. The park wrote two of the three facts a wake needs and reported success.
//
//   - THE LADDER THE PARK RESETS. One run opened TWENTY-NINE attempts across EIGHT durable jobs in six
//     minutes. Its engine died at the first model step every time; four job attempts failed, and the
//     fifth found the pool momentarily memberless, PARKED, and therefore returned nil — so the job
//     COMPLETED, and the wake minted a fresh job with a fresh five-attempt budget. Measured
//     `attempt_count` across those eight jobs: 5,5,5,5,5,5,5,6. The run reached a terminal only when
//     one job's five attempts happened not to be interrupted by a park. Three other runs on the same
//     stack burned 60, 44 and 40 attempts the same way.
//
// Both test names are spelled out in scripts/test/component's postgres `-run` allow-list. A component
// test whose name that list does not match never runs and reports the same green as one that passes,
// and that trap has been sprung in this repository more than once.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/packages/coordinator"

	"github.com/palgroup/palai/storage"
)

// stuckRunTenantWithoutAPool seeds an organization and a project and DELIBERATELY NO POOL — which is
// every project a console or an API creates that is not the bootstrap one. It is the whole precondition
// of the first proof: `fleet.ResolvePool`'s last resort is the CONSTANT `pool_default`, which belongs to
// the bootstrap tenant, so a tenant that has configured nothing resolves to a pool it does not own.
func stuckRunTenantWithoutAPool(t *testing.T, pool *pgxpool.Pool) coordinator.Tenant {
	t.Helper()
	ctx := storage.WithSystemScope(context.Background())
	project := poolKeyID("prj")
	for _, stmt := range [][]any{
		{`INSERT INTO projects (id) VALUES ($1)`, project},
	} {
		if _, err := pool.Exec(ctx, stmt[0].(string), stmt[1:]...); err != nil {
			t.Fatalf("seed a tenant with no pool: %v", err)
		}
	}
	return coordinator.Tenant{Project: project}
}

// stuckRunPoolID reads runs.pool_id as it really is — NULL included, which is the entire point.
func stuckRunPoolID(t *testing.T, pool *pgxpool.Pool, runID string) *string {
	t.Helper()
	var out *string
	if err := pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT pool_id FROM runs WHERE id = $1`, runID).Scan(&out); err != nil {
		t.Fatalf("read runs.pool_id: %v", err)
	}
	return out
}

// TestPlacementNeverParksARunWhereItsOwnWakeCannotFindIt WAS RED (unwakeable park) AND IS GREEN AS OF
// 2026-08-05 — re-measured, not assumed, by the shipped selector quoted at the state read below.
//
// WHICH ARM MAKES IT GREEN IS THE PART TO CARRY, because the test accepts two different answers and only
// one of them happens: the run SETTLES (`failed`) instead of parking. The park arm — a parked run must
// carry a wakeable pool id — is therefore not exercised by anything today. It stays, because it states
// what must hold if a park becomes reachable again, which is the regression this test exists to catch.
//
// A run whose tenant owns no pool resolves to the CONSTANT default pool, which belongs to somebody
// else. `RecordRunPool` correctly refuses to record another tenant's pool — its EXISTS excludes the
// row — and correctly returns nil rather than failing the run over a configuration mistake. But the
// dial then parks the run anyway, and the park's own wake predicate reads the column that was never
// written. The result is a run that is `waiting` forever with nothing in the tree able to move it: not
// a machine (the wake filters on `pool_id`), not the reaper (`SweepExpiredCapacityParks` returns 0
// unless `PALAI_FLEET_PARK_TTL` is set, and the shipped default is unset), not the person who asked.
//
// THE ASSERTION IS THE WAKE'S OWN PREDICATE, not a state name. Parking is a legitimate outcome and
// this test must not forbid it — what it forbids is parking without writing the one fact the wake
// searches by. So a parked run must carry a pool id, and a run that cannot be parked reachably must
// reach a TERMINAL instead, with a reason on its response that names the problem. Either answer ends
// the person's wait; the shipped third answer does not.
func TestPlacementNeverParksARunWhereItsOwnWakeCannotFindIt(t *testing.T) {
	f := newPlacementFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tenant := stuckRunTenantWithoutAPool(t, f.pool)
	_, responseID, runID := placementRun(t, f.pool, tenant)

	orch := placementOrchestrator(t, f)
	attempt := f.attempt(runID, poolKeyID("att"), 1)
	attempt.Tenant = tenant
	if err := orch.ExecuteAttempt(ctx, attempt); err != nil {
		t.Fatalf("ExecuteAttempt for a tenant with no pool of its own returned %v, want nil — an error here is a FAILED attempt that spends the retry ladder on a configuration mistake", err)
	}

	state := placementRunState(t, f.pool, runID)
	// THE `waiting` ARM BELOW IS NOT ENTERED TODAY, measured 2026-08-05 rather than assumed:
	//
	//	PALAI_SUITE_RUN='TestPlacementNeverParksARunWhereItsOwnWakeCannotFindIt' \
	//	PALAI_SUITE_PKG=./apps/control-plane/internal/execution TEST=postgres scripts/test/component
	//	-> run state = "failed", --- PASS
	//
	// The tenant-with-no-pool case now settles terminal instead of parking, so the test passes through
	// the switch below. That is the fix this test was written RED against, not a reason to delete the
	// arm: the arm states what must hold IF a park happens, and a park becoming reachable again is
	// exactly the regression it exists to catch. It is kept, and its SQL is kept CORRECT — an arm that
	// nothing reaches is the one place a defect can sit unmeasured, which is how the missing AND below
	// survived. The state is logged so the next reader re-measures instead of trusting this comment.
	t.Logf("bu ağaçta havuzsuz kiracının run durumu: %q (park kolu girilmiyor)", state)
	if state == "waiting" {
		// Parked. Then the wake must be able to find it, and the wake reads exactly this column.
		poolID := stuckRunPoolID(t, f.pool, runID)
		if poolID == nil {
			t.Fatal("the run parked with runs.pool_id NULL: OldestRunAwaitingCapacity filters on `r.pool_id = $3`, so NO machine joining ANY pool can ever wake it, and SweepExpiredCapacityParks is a no-op unless PALAI_FLEET_PARK_TTL is set — this run waits forever with no error for anyone to act on")
		}
		// And the recorded pool must be one this tenant can actually be served by, or the column is
		// present and still points nowhere a machine of theirs will ever join.
		var owned int
		if err := f.pool.QueryRow(storage.WithSystemScope(ctx),
			`SELECT count(*) FROM runner_pools
			  WHERE id = $1 AND (project_id IS NULL OR project_id = $2)`,
			*poolID, tenant.Project).Scan(&owned); err != nil {
			t.Fatalf("check the parked pool belongs to the tenant: %v", err)
		}
		if owned != 1 {
			t.Fatalf("the run parked on pool %q, which is not a pool this tenant can be served by: a machine of theirs can never join it, so the wake has a column to read and still nothing to find", *poolID)
		}
		return
	}

	// Not parked, so it must have SETTLED — and settled with something a person can act on.
	switch state {
	case "failed", "timed_out", "cancelled", "expired":
	default:
		t.Fatalf("run state = %q: neither parked-and-wakeable nor terminal, so nothing in the tree will ever move it", state)
	}
	var responseState, projection string
	if err := f.pool.QueryRow(storage.WithSystemScope(ctx),
		`SELECT state, coalesce(output, '{}'::jsonb)::text FROM responses WHERE id = $1`,
		responseID).Scan(&responseState, &projection); err != nil {
		t.Fatalf("read the response: %v", err)
	}
	if responseState == "queued" || responseState == "in_progress" || responseState == "provisioning" {
		t.Fatalf("the run reached %q but its response is still %q: the caller polling it sees a live run forever", state, responseState)
	}
	if !strings.Contains(projection, "pool") {
		t.Fatalf("the run failed with response body %q, which does not name the pool that could not serve it: an ordinary failure with no representation is the defect, not the fix", projection)
	}
}

// TestPlacementAWokenRunDoesNotGetAFreshRetryBudget WAS RED (the ladder the park resets) AND IS GREEN AS
// OF 2026-08-05: the carry it demands is shipped, so the wake's new job starts at the spent count rather
// than at zero. Re-measured with the same selector as its sibling above, not assumed:
//
//	PALAI_SUITE_RUN='TestPlacementAWokenRunDoesNotGetAFreshRetryBudget' \
//	PALAI_SUITE_PKG=./apps/control-plane/internal/execution TEST=postgres scripts/test/component
//	-> --- PASS (1.32s)
//
// Unlike its sibling this one drives its whole path: the park, the enrolment, the wake and the carry are
// all reached, so its green is the property's and not an arm's absence.
//
// A capacity park ends its attempt with nil, so the worker COMPLETES the job — which is right, the
// attempt did not fail. The wake then mints a NEW job (placement.go's `newJobID()` + `EnqueueJob`),
// and a new durable_jobs row starts at `attempt_count` 0. So every park→wake cycle hands the run a
// FRESH five-attempt budget, and a run failing for a reason that has nothing to do with capacity can
// be retried without bound. On the live stack one such run produced eight jobs and twenty-nine
// attempts before a job's five failures happened to land without a park in between.
//
// The property is not "parks are bad" — a Mac takes six to twenty minutes to boot and the park is what
// makes that reachable. The property is that a PARK IS NOT PROGRESS: what the run has already spent
// must survive the wake, so the retry ceiling still binds and the run reaches a terminal with a stated
// reason instead of looping until a coincidence ends it.
//
// The precondition is written as the row the loop actually leaves behind — a completed response.run
// job whose attempt budget is spent — rather than by driving five real engine deaths, because what is
// under proof is the CARRY, and a proof that spent five real attempts first would be measuring the
// engine.
func TestPlacementAWokenRunDoesNotGetAFreshRetryBudget(t *testing.T) {
	f := newPlacementFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tenant, poolID, key := placementTenant(t, f)
	_, _, runID := placementRun(t, f.pool, tenant)

	// The job the loop leaves behind: five attempts spent, completed by the park that ended the fifth.
	const spent = 5
	spentJobID := poolKeyID("job")
	if _, err := f.pool.Exec(storage.WithSystemScope(ctx),
		`INSERT INTO durable_jobs (id, project_id, kind, status, attempt_count, payload)
		 VALUES ($1,$2,'response.run','completed',$3,jsonb_build_object('run_id', $4::text))`,
		spentJobID, tenant.Project, spent, runID); err != nil {
		t.Fatalf("seed the exhausted job the park completed: %v", err)
	}

	orch := placementOrchestrator(t, f)
	attempt := f.attempt(runID, poolKeyID("att"), 1)
	attempt.Tenant = tenant
	if err := orch.ExecuteAttempt(ctx, attempt); err != nil {
		t.Fatalf("ExecuteAttempt: %v", err)
	}
	if got := placementRunState(t, f.pool, runID); got != "waiting" {
		t.Fatalf("run state = %q before the wake, want waiting", got)
	}

	// A machine joins the pool — the production wake, and the only thing this test does.
	identity, err := f.enrolAsking(ctx, key, poolID)
	if err != nil {
		t.Fatalf("enrol: %v", err)
	}
	parkIdentity(t, f.gatewayFixture, identity)
	waitConnected(t, f.gateway, 1)

	var wokenID string
	var woken int
	deadline := time.Now().Add(20 * time.Second)
	for {
		err := f.pool.QueryRow(storage.WithSystemScope(ctx),
			`SELECT id, attempt_count FROM durable_jobs
			  WHERE kind = 'response.run' AND payload->>'run_id' = $1 AND id <> $2`,
			runID, spentJobID).Scan(&wokenID, &woken)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no job was enqueued for the woken run within 20s (%v)", err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	if woken < spent {
		t.Fatalf("the woken run's new job %s starts at attempt_count %d, but the run had already spent %d: every park hands it a FRESH ladder, so a run failing for a reason unrelated to capacity is retried without bound and reaches a terminal only by coincidence — measured on the live stack as eight jobs and twenty-nine attempts for one run", wokenID, woken, spent)
	}
}
