//go:build component

package execution_test

// PLACEMENT, TENANT AND THE CAPACITY PARK, against a REAL Postgres, the REAL gateway and two fake
// machines (E24 T4).
//
// This file exists because of two measurements, and neither of them is a design opinion:
//
//   - §3.6 D8: the runner plane had NO tenant concept at all. Enrolment carried none,
//     AttemptDescriptor carried none, the lease offer carried none, Dial checked none. Any enrolled
//     runner could take ANY tenant's attempt. The proof needs TWO tenants with TWO machines, so a fake
//     registry that hardcodes one tenant (the wire proofs' one) cannot state the claim at all.
//   - §3.6 D12: a run placed in an empty pool DIED IN ~2.5 MINUTES — a 20s dial budget times a
//     five-attempt retry ladder — while AWS documents a Mac host taking 6 to 20 minutes to start. So
//     "bring a Mac up when load arrives" was not an economic choice, it was an unreachable behaviour.
//
// Every test here is named with the `TestPlacement` prefix on purpose: scripts/test/component's
// postgres leg is an ALLOW-LIST, and a component test whose name it does not match never runs and
// reports the same green as one that passes. That trap has been sprung in this repository more than
// once.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
	"github.com/palgroup/palai/packages/coordinator"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
	"github.com/palgroup/palai/packages/runner"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"

	"github.com/palgroup/palai/storage"
)

// parkIdentity holds an already-enrolled machine open waiting for a lease, the way a real runner's
// park loop does. It is park()'s sibling for a machine that enrolled with a POOL KEY rather than the
// file token, which is the only way a machine lands in a tenant of its own.
func parkIdentity(t *testing.T, f *gatewayFixture, identity runner.Identity) *parkedRunner {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pr := &parkedRunner{identity: identity, lease: make(chan runner.Lease, 1)}
	go func() {
		if lease, err := f.session(identity).ReceiveLease(ctx); err == nil {
			pr.lease <- lease
		}
	}()
	return pr
}

// newPlacementFixture is newPoolKeyFixture plus the migration chain. The pool-key proofs rely on the
// tier's first leg having migrated the shared database; these tests do it themselves so a single
// PALAI_SUITE_RUN selection of this file is runnable on its own, which is what made each RED below
// observable before its fix.
// It also accepts PALAI_DATABASE_URL, which is this file's ONE deliberate departure from §T4: the plan
// asks for a separate `//go:build live` leg proving park + wake "on a real Postgres". These tests ARE on a
// real Postgres — same migration chain, same RLS, same app role — so a live-tagged twin would differ only
// in which environment variable names the database, and NO live driver in this tree would run a new file
// under internal/execution (the live tier is explicit `-run`/package targets, not a sweep). A test nothing
// executes is precisely the failure this task was warned about twice. So the same proofs point at an
// operator's own stack when they set PALAI_DATABASE_URL, and there is no second, unexecuted copy.
func newPlacementFixture(t *testing.T) *poolKeyFixture {
	t.Helper()
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		// The operator-supplied database, named so the skip below says which variable to set.
		if url = os.Getenv("PALAI_DATABASE_URL"); url != "" {
			t.Setenv("PALAI_COMPONENT_POSTGRES_URL", url)
		}
	}
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL (or PALAI_DATABASE_URL, to run against your own stack) is required; run TEST=postgres scripts/test/component")
	}
	cs, err := coordinator.Open(context.Background(), url)
	if err != nil {
		t.Fatalf("coordinator.Open: %v", err)
	}
	t.Cleanup(cs.Close)
	if err := cs.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	f := newPoolKeyFixture(t)
	// The wake, wired exactly as main.go wires it (gateway.SetCapacityWaker(repo.Spine())). Without this
	// line a machine joining a pool wakes nothing — which is the posture every Docker-free wire proof
	// runs in, and the reason the production call site is fenced by name in a test of its own.
	f.gateway.SetCapacityWaker(cs)
	// Kept so E24 T5's park-reaper proofs can drive the same spine the waker uses: the park and the thing
	// that expires it have to be the same store, or the test proves two halves of two systems.
	f.spine = cs
	return f
}

// placementTenant seeds a tenant the way identity.Store.provision does — an organization, a project and
// a pool NAMED 'default' — and returns the tenant, that pool's id and a live enrolment key for it.
//
// The name matters and is not decoration: `fleet.ResolvePool`'s last-but-one step is the TENANT's own
// default pool, resolved by that name, and the whole point of it is that the CONSTANT `pool_default`
// belongs to the bootstrap tenant alone. A fixture that seeded a randomly-named pool would have made
// every test here pass a pool id in by hand and prove nothing about resolution.
func placementTenant(t *testing.T, f *poolKeyFixture) (coordinator.Tenant, string, string) {
	t.Helper()
	ctx := storage.WithSystemScope(context.Background())
	org, project, poolID := poolKeyID("org"), poolKeyID("prj"), poolKeyID("pool")
	stmts := [][]any{
		{`INSERT INTO organizations (id) VALUES ($1)`, org},
		{`INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, project, org},
		{`INSERT INTO runner_pools (id, organization_id, project_id, name, posture)
		  VALUES ($1,$2,$3,'default','sandboxed-linux')`, poolID, org, project},
	}
	for _, stmt := range stmts {
		if _, err := f.pool.Exec(ctx, stmt[0].(string), stmt[1:]...); err != nil {
			t.Fatalf("seed the tenant: %v", err)
		}
	}
	_, key := mintPoolKey(t, f.keys, org, project, poolID, nil)
	return coordinator.Tenant{Project: project}, poolID, key
}

// TestPlacementNeverOffersOneTenantsAttemptToAnothersRunner is RED (1), TENANT: tenant B's attempt must
// not be offered tenant A's machine. Before this task there was no check of any kind, so it was born
// red — and the shape it is red in matters, which is why the intruding attempt names tenant A's POOL
// ID rather than a pool of its own: `fleet.ResolvePool` falls back to a CONSTANT default pool id, so a
// tenant that has configured nothing resolves to a string that may belong to somebody else. A test
// that gave tenant B its own pool id would have been satisfied by the per-pool queue T2 already built
// and would have proved nothing about tenants.
//
// The assertion is two-sided, because a Dial that refused everything would pass the refusal half: the
// machine that must NOT be used is asserted still parked and still lease-less, and then the SAME
// machine takes its OWN tenant's run.
func TestPlacementNeverOffersOneTenantsAttemptToAnothersRunner(t *testing.T) {
	f := newPlacementFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tenantA, poolA, keyA := placementTenant(t, f)
	tenantB, poolB, keyB := placementTenant(t, f)

	identityA, err := f.enrolAsking(ctx, keyA, poolA)
	if err != nil {
		t.Fatalf("enrol tenant A's machine: %v", err)
	}
	identityB, err := f.enrolAsking(ctx, keyB, poolB)
	if err != nil {
		t.Fatalf("enrol tenant B's machine: %v", err)
	}
	machineA := parkIdentity(t, f.gatewayFixture, identityA)
	machineB := parkIdentity(t, f.gatewayFixture, identityB)
	waitConnected(t, f.gateway, 2)

	intruder := f.attempt("run_placement_x", "att_placement_x", 1)
	intruder.PoolID = poolA
	intruder.Tenant = tenantB
	dialCtx, cancelDial := context.WithTimeout(ctx, 2*time.Second)
	defer cancelDial()
	if ch, err := f.gateway.Dial(dialCtx, intruder); err == nil {
		_ = ch.Close()
		t.Fatalf("tenant %s's attempt was offered a machine enrolled by tenant %s: today ANY enrolled runner can take ANY tenant's attempt (§3.6 D8)",
			tenantB.Organization, tenantA.Organization)
	}
	select {
	case lease := <-machineA.lease:
		t.Fatalf("tenant A's machine received lease %s for tenant B's run", lease.LeaseID)
	case lease := <-machineB.lease:
		t.Fatalf("tenant B's machine received lease %s for a run placed in tenant A's pool", lease.LeaseID)
	default:
	}
	// The refusal must not have cost a machine: a "refusal" that drops a connection is an outage
	// wearing a policy's name.
	if got := f.gateway.Connected(); got != 2 {
		t.Fatalf("gateway Connected() = %d after the cross-tenant refusal, want 2", got)
	}

	own := f.attempt("run_placement_own", "att_placement_own", 2)
	own.PoolID = poolA
	own.Tenant = tenantA
	ownCtx, cancelOwn := context.WithTimeout(ctx, 10*time.Second)
	defer cancelOwn()
	ch, err := f.gateway.Dial(ownCtx, own)
	if err != nil {
		t.Fatalf("tenant A's own attempt could not reach tenant A's machine: %v", err)
	}
	defer ch.Close()
	select {
	case lease := <-machineA.lease:
		if lease.RunID != own.RunID {
			t.Fatalf("tenant A's machine was leased %s, want %s", lease.RunID, own.RunID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("tenant A's machine was never offered its own tenant's run")
	}
}

// placementRun seeds a session, a response and a queued run for a tenant, and returns the run id. The
// response row is required: RunContext JOINs it, so a run without one cannot be executed at all.
func placementRun(t *testing.T, pool *pgxpool.Pool, tenant coordinator.Tenant) (sessionID, responseID, runID string) {
	t.Helper()
	ctx := storage.WithSystemScope(context.Background())
	sessionID, responseID, runID = poolKeyID("ses"), poolKeyID("resp"), poolKeyID("run")
	stmts := [][]any{
		{`INSERT INTO sessions (id, organization_id, project_id) VALUES ($1,$2,$3)`, sessionID, tenant.Project},
		{`INSERT INTO responses (id, organization_id, project_id, session_id, state, input)
		  VALUES ($1,$2,$3,$4,'queued','"say hello"'::jsonb)`, responseID, tenant.Project, sessionID},
		{`INSERT INTO runs (id, organization_id, project_id, session_id, response_id, state)
		  VALUES ($1,$2,$3,$4,$5,'queued')`, runID, tenant.Project, sessionID, responseID},
	}
	for _, stmt := range stmts {
		if _, err := pool.Exec(ctx, stmt[0].(string), stmt[1:]...); err != nil {
			t.Fatalf("seed the run: %v", err)
		}
	}
	return sessionID, responseID, runID
}

// placementOrchestrator builds the PRODUCTION orchestrator over the component database with the REAL
// gateway as its dialer, so what parks a run here is the same code path a dispatch worker drives.
func placementOrchestrator(t *testing.T, f *poolKeyFixture) *execution.Orchestrator {
	t.Helper()
	repo, err := store.Open(context.Background(), os.Getenv("PALAI_COMPONENT_POSTGRES_URL"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(repo.Close)
	orch := execution.NewOrchestrator(repo, f.gateway, modelbroker.New(modelbroker.Config{}), toolbroker.New())
	// A short dial budget: the production one is 20s and the claim under proof is WHAT HAPPENS when it
	// expires on an empty pool, not how long it waits first.
	orch.DialHandshakeDeadline = 500 * time.Millisecond
	return orch
}

func placementRunState(t *testing.T, pool *pgxpool.Pool, runID string) string {
	t.Helper()
	var state string
	if err := pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT state FROM runs WHERE id = $1`, runID).Scan(&state); err != nil {
		t.Fatalf("read run state: %v", err)
	}
	return state
}

// TestPlacementParksARunWhosePoolHasNoRunner is RED (2), PARK. A run placed in a pool with no machine
// in it must reach `waiting`, and ExecuteAttempt must return NIL — the dispatch worker is released and
// the run costs no compute while nothing can run it.
//
// It was red for the reason §3.6 D12 measured: Dial blocked until its 20s deadline, returned
// context.DeadlineExceeded wrapped in "dial engine", and ExecuteAttempt returned that error — which is
// a FAILED attempt, so the retry ladder spent five of them in about two and a half minutes and
// dead-lettered the job. A Mac takes six to twenty minutes to boot.
//
// The nil is asserted as hard as the state is, and that is the whole of "holds no dispatch worker": a
// park that returned an error would leave the worker's handler reporting a failure, and the job would
// be retried rather than left for the wake.
func TestPlacementParksARunWhosePoolHasNoRunner(t *testing.T) {
	f := newPlacementFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tenant, _, _ := placementTenant(t, f)
	_, _, runID := placementRun(t, f.pool, tenant)

	orch := placementOrchestrator(t, f)
	attempt := f.attempt(runID, poolKeyID("att"), 1)
	attempt.Tenant = tenant
	if err := orch.ExecuteAttempt(ctx, attempt); err != nil {
		t.Fatalf("ExecuteAttempt on an empty pool returned %v, want nil — a dial that fails is a FAILED attempt, and five of those dead-letter the run in ~2.5 minutes while a Mac takes 6-20 to boot", err)
	}
	if got := placementRunState(t, f.pool, runID); got != "waiting" {
		t.Fatalf("run state = %q after being placed in an empty pool, want waiting", got)
	}
	// AND NO ATTEMPT IS DRIVING IT, stated durably rather than inferred from the nil above. The parked
	// attempt carries the `awaiting_capacity` marker and therefore sits OUTSIDE the one-active-per-run
	// index's state set — which is the same fact as "this run holds no compute": there is no assigned,
	// starting, active or draining attempt left on it, so nothing is holding a worker or an engine.
	var active, parked int
	if err := f.pool.QueryRow(storage.WithSystemScope(ctx),
		`SELECT count(*) FILTER (WHERE state IN ('assigned','starting','active','draining')),
		        count(*) FILTER (WHERE state = $2)
		   FROM attempts WHERE run_id = $1`, runID, coordinator.AttemptAwaitingCapacity).Scan(&active, &parked); err != nil {
		t.Fatalf("read the run's attempts: %v", err)
	}
	if active != 0 || parked != 1 {
		t.Fatalf("the parked run has %d live attempt(s) and %d awaiting-capacity attempt(s), want 0 and 1 — a live attempt row is a run something still believes it is driving", active, parked)
	}
}

// TestPlacementWakesAParkedRunWhenAMachineJoinsItsPool is RED (3), WAKE. A parked run must run when a
// machine connects to its pool — and the red form of that is a deadline: FAIL if it is still `waiting`
// after the wake should have happened.
//
// The wake is asserted through its OBSERVABLE consequence rather than through an internal call: the run
// leaves `waiting` and a response.run job exists for it, which is what actually causes a worker to open
// a fresh attempt. Asserting a method was called would pass on a wake that enqueued nothing.
func TestPlacementWakesAParkedRunWhenAMachineJoinsItsPool(t *testing.T) {
	f := newPlacementFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tenant, poolID, key := placementTenant(t, f)
	_, _, runID := placementRun(t, f.pool, tenant)

	orch := placementOrchestrator(t, f)
	attempt := f.attempt(runID, poolKeyID("att"), 1)
	attempt.Tenant = tenant
	if err := orch.ExecuteAttempt(ctx, attempt); err != nil {
		t.Fatalf("ExecuteAttempt: %v", err)
	}
	if got := placementRunState(t, f.pool, runID); got != "waiting" {
		t.Fatalf("run state = %q before the wake, want waiting", got)
	}
	// The park recorded WHERE it parked, which is the only reason the wake can find it.
	var parked *string
	if err := f.pool.QueryRow(storage.WithSystemScope(ctx), `SELECT pool_id FROM runs WHERE id = $1`, runID).Scan(&parked); err != nil {
		t.Fatalf("read runs.pool_id: %v", err)
	}
	if parked == nil || *parked != poolID {
		t.Fatalf("runs.pool_id = %v, want the pool the run parked in %q", parked, poolID)
	}

	// A machine joins the pool. This is the ONLY thing the test does — no resume command, no reaper.
	identity, err := f.enrolAsking(ctx, key, poolID)
	if err != nil {
		t.Fatalf("enrol: %v", err)
	}
	parkIdentity(t, f.gatewayFixture, identity)
	waitConnected(t, f.gateway, 1)

	deadline := time.Now().Add(20 * time.Second)
	for {
		if state := placementRunState(t, f.pool, runID); state != "waiting" {
			if state != "running" {
				t.Fatalf("the woken run is %q, want running", state)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the parked run is STILL waiting after a machine joined its pool: nothing wakes it, so a Mac that comes up serves nothing")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// The wake without the job is a state change nobody acts on.
	var jobs int
	if err := f.pool.QueryRow(storage.WithSystemScope(ctx),
		`SELECT count(*) FROM durable_jobs WHERE kind = 'response.run' AND payload->>'run_id' = $1`, runID).Scan(&jobs); err != nil {
		t.Fatalf("count response.run jobs: %v", err)
	}
	if jobs != 1 {
		t.Fatalf("the woken run has %d response.run job(s), want exactly 1 — a wake that enqueues nothing is a run nobody opens an attempt for", jobs)
	}

	// EXACTLY ONCE, and this is the half a state assertion cannot make. A runner re-dials after every
	// single lease, so the wake runs on a hot path; a second machine joining the same pool must not
	// enqueue a second job for a run that is already running, or one park would turn into an attempt per
	// connect for the rest of the run's life.
	second, err := f.enrolAsking(ctx, key, poolID)
	if err != nil {
		t.Fatalf("enrol a second machine: %v", err)
	}
	parkIdentity(t, f.gatewayFixture, second)
	waitConnected(t, f.gateway, 2)
	if err := f.pool.QueryRow(storage.WithSystemScope(ctx),
		`SELECT count(*) FROM durable_jobs WHERE kind = 'response.run' AND payload->>'run_id' = $1`, runID).Scan(&jobs); err != nil {
		t.Fatalf("re-count response.run jobs: %v", err)
	}
	if jobs != 1 {
		t.Fatalf("after a second machine joined the pool the run has %d response.run job(s), want still exactly 1", jobs)
	}
}

// TestPlacementWakeDoesNotReserveTheMachineItWokeFor is the BENIGN RACE, stated as a test because the
// design deliberately allows it: a wake does NOT hold a machine for the run it woke. The woken run
// dials like any other, and another run may take that machine first — in which case the woken one
// simply parks again. Reserving would have meant a second, parallel notion of "assigned to", and the
// tree already has one that is durable.
//
// So this asserts BOTH halves: exactly one of the two runs gets the single machine, and the other is
// back in `waiting` rather than failed or dead-lettered.
func TestPlacementWakeDoesNotReserveTheMachineItWokeFor(t *testing.T) {
	f := newPlacementFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tenant, poolID, key := placementTenant(t, f)
	_, _, firstRun := placementRun(t, f.pool, tenant)
	_, _, secondRun := placementRun(t, f.pool, tenant)

	orch := placementOrchestrator(t, f)
	for _, runID := range []string{firstRun, secondRun} {
		attempt := f.attempt(runID, poolKeyID("att"), 1)
		attempt.Tenant = tenant
		if err := orch.ExecuteAttempt(ctx, attempt); err != nil {
			t.Fatalf("ExecuteAttempt for %s: %v", runID, err)
		}
		if got := placementRunState(t, f.pool, runID); got != "waiting" {
			t.Fatalf("run %s is %q, want waiting", runID, got)
		}
	}

	// ONE machine for TWO parked runs.
	identity, err := f.enrolAsking(ctx, key, poolID)
	if err != nil {
		t.Fatalf("enrol: %v", err)
	}
	parkIdentity(t, f.gatewayFixture, identity)
	waitConnected(t, f.gateway, 1)

	// Exactly one run is woken by the single connect: the wake takes the pool's OLDEST waiting run and
	// takes ONE of them, so the other is untouched and stays parked for the next machine.
	deadline := time.Now().Add(20 * time.Second)
	woken := ""
	for woken == "" {
		for _, runID := range []string{firstRun, secondRun} {
			if placementRunState(t, f.pool, runID) != "waiting" {
				woken = runID
			}
		}
		if woken == "" && time.Now().After(deadline) {
			t.Fatal("neither parked run was woken by a machine joining the pool")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if woken != firstRun {
		t.Fatalf("the wake took run %s, want the pool's OLDEST waiting run %s", woken, firstRun)
	}
	if got := placementRunState(t, f.pool, secondRun); got != "waiting" {
		t.Fatalf("the run the wake did NOT take is %q, want waiting — a wake must not disturb the runs it did not take", got)
	}
}

// TestPlacementWakeLeavesEveryOtherWaitingRunAlone is the guard that makes the wake SAFE to run on a
// hot path, and it is the one this design could most easily have got wrong.
//
// `waiting` is not one condition, it is four: a human's pause, a gated tool call awaiting approval, a
// detached child, and no capacity. Each has its own waker with its own predicate. A capacity wake fires
// on every machine that connects — which for a runner is after EVERY lease — so a predicate of
// `state = 'waiting' AND pool_id = …` would resume a paused run against the decision of the person who
// paused it, and would re-drive an approval-parked run in a loop for as long as the fleet is busy.
//
// So the marker is POSITIVE and lives on the ATTEMPT, and this test is what says so out loud: a run in
// the SAME pool, waiting, whose attempt carries no capacity marker, is untouched by a machine's arrival.
func TestPlacementWakeLeavesEveryOtherWaitingRunAlone(t *testing.T) {
	f := newPlacementFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tenant, poolID, key := placementTenant(t, f)
	_, _, pausedRun := placementRun(t, f.pool, tenant)
	// A run parked for SOME OTHER reason, in the same pool: waiting, placed, with an ordinary attempt
	// row. This is the shape a user's pause, an approval and a detached child all leave behind.
	sys := storage.WithSystemScope(ctx)
	for _, stmt := range [][]any{
		{`UPDATE runs SET state = 'waiting', pool_id = $2 WHERE id = $1`, pausedRun, poolID},
		{`INSERT INTO attempts (id, organization_id, project_id, run_id, fence, state)
		  VALUES ($1,$2,$3,$4,1,'assigned')`, poolKeyID("att"), tenant.Project, pausedRun},
	} {
		if _, err := f.pool.Exec(sys, stmt[0].(string), stmt[1:]...); err != nil {
			t.Fatalf("seed the otherwise-waiting run: %v", err)
		}
	}

	identity, err := f.enrolAsking(ctx, key, poolID)
	if err != nil {
		t.Fatalf("enrol: %v", err)
	}
	parkIdentity(t, f.gatewayFixture, identity)
	waitConnected(t, f.gateway, 1)

	// Give the wake every chance to misfire: it runs before the park, so by the time the machine is
	// counted as connected it has already run once.
	time.Sleep(500 * time.Millisecond)
	if got := placementRunState(t, f.pool, pausedRun); got != "waiting" {
		t.Fatalf("a run waiting for a reason that is NOT capacity is %q after a machine joined its pool, want waiting — this wake would override a human's pause and re-drive an approval-parked run on every lease", got)
	}
	var jobs int
	if err := f.pool.QueryRow(sys,
		`SELECT count(*) FROM durable_jobs WHERE kind = 'response.run' AND payload->>'run_id' = $1`, pausedRun).Scan(&jobs); err != nil {
		t.Fatalf("count response.run jobs: %v", err)
	}
	if jobs != 0 {
		t.Fatalf("the wake enqueued %d job(s) for a run it does not own, want 0", jobs)
	}
}

// TestPlacementRecordsThePoolItChose is R5: `runs.pool_id` carries the placement decision, so it is
// auditable and a resume returns to the SAME pool. A run waking in a different posture after a kill is
// a run that cannot find its workspace.
//
// It also pins the reason the column is nullable: the write is guarded on the pool actually being this
// tenant's, so a project policy naming a pool that does not exist records NO decision rather than
// claiming one — and a run in that state parks, which is the honest outcome for a pool that will never
// have a machine.
func TestPlacementRecordsThePoolItChose(t *testing.T) {
	f := newPlacementFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tenant, poolID, _ := placementTenant(t, f)
	_, _, runID := placementRun(t, f.pool, tenant)
	// The project's policy is the third placement step and the only one an operator can set today.
	if _, err := f.pool.Exec(storage.WithSystemScope(ctx),
		`UPDATE projects SET config_policy = $2::jsonb WHERE id = $1`, tenant.Project,
		`{"pool":"`+poolID+`"}`); err != nil {
		t.Fatalf("publish the project pool policy: %v", err)
	}

	orch := placementOrchestrator(t, f)
	attempt := f.attempt(runID, poolKeyID("att"), 1)
	attempt.Tenant = tenant
	// NO PoolID on the descriptor: the point is that the orchestrator RESOLVES one. Before this task
	// fleet.ResolvePool had no production caller at all, so the policy was a field nothing read.
	if attempt.PoolID != "" {
		t.Fatalf("the fixture attempt is pool-configured (%q); this proof is about resolution", attempt.PoolID)
	}
	if err := orch.ExecuteAttempt(ctx, attempt); err != nil {
		t.Fatalf("ExecuteAttempt: %v", err)
	}
	var recorded *string
	if err := f.pool.QueryRow(storage.WithSystemScope(ctx),
		`SELECT pool_id FROM runs WHERE id = $1`, runID).Scan(&recorded); err != nil {
		t.Fatalf("read runs.pool_id: %v", err)
	}
	if recorded == nil || *recorded != poolID {
		t.Fatalf("runs.pool_id = %v, want the project policy's pool %q", recorded, poolID)
	}
	// And it parked in that pool, which is the only reason the recorded value is load-bearing.
	if got := placementRunState(t, f.pool, runID); got != "waiting" {
		t.Fatalf("run state = %q, want waiting", got)
	}
}
