//go:build component

package execution

// THE CAPACITY PARK (Faz A.4 T6), driven through the WHOLE of ExecuteAttempt against a REAL Postgres.
//
// tests/component/fleet/capacity_placement_test.go holds the COORDINATOR's half: a machine that declares
// three refuses the fourth hold. This file holds the half that decides what the platform DOES with that
// refusal, and until T6 the answer was "log it and run the attempt anyway, unmetered" — a ceiling
// machine_occupancy.go held deliberately while nothing could wake a run when a slot freed.
//
// EVERY PROOF HERE GOES THROUGH ExecuteAttempt, never through holdMachine called by hand. The claim is
// about a RUN — that it parks rather than running — and the thing that runs a run is ExecuteAttempt: the
// workspace plan, the dial, the realize, the hold and the frame loop are all inside it, and a test that
// reached past them could not tell a park apart from an attempt that never started.
//
// WHAT IS FAKED IS THE TRANSPORT AND NOTHING ELSE. The dialer hands back a channel that NAMES a machine
// (the same MachineIdentified interface production's gatewayChannel satisfies), so `machineOf` answers
// with a real machine id and `holdMachine` asks the real database about a real `runners.capacity`. The
// engine's script is the discriminator: an attempt that is NOT parked drives it to a terminal and the run
// reaches `completed`, which is exactly what the shipped build did on a full machine.

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"

	"github.com/palgroup/palai/storage"
)

// capacityFixture is a real spine, a tenant with its own pool, a run with a WORKSPACE, and one machine
// whose declared capacity the test chooses.
//
// THE WORKSPACE IS NOT OPTIONAL AND ITS ABSENCE WOULD MAKE EVERY CLAIM HERE VACUOUS. An occupancy is the
// ALLOCATION's hold, so holdMachine is reached only inside ExecuteAttempt's `wsPlanned` arm — a run with
// no repository binding never opens a hold and therefore can never be capacity-parked either. A fixture
// without a workspace would drive an attempt that skips the entire subject of this file and completes for
// a reason having nothing to do with capacity.
type capacityFixture struct {
	cs         *coordinator.Store
	repo       *store.Store
	tenant     coordinator.Tenant
	sessionID  string
	responseID string
	runID      string
	poolID     string
	machine    string
	root       string
	wsID       string
	orch       *Orchestrator
}

// newCapacityFixture seeds the tenant, the pool, one machine declaring `capacity`, a session with a bound
// workspace on a real allocation, and the production orchestrator wired to provision into that allocation.
//
// `capacity` is written EXPLICITLY rather than left to the column default, because a test whose subject is
// a ceiling above one cannot take a fixture that only ever produces one — and because zero is the case
// that must still run (an undeclared machine has no ceiling at all).
func newCapacityFixture(t *testing.T, capacity int) *capacityFixture {
	t.Helper()
	return newFleetFixture(t, capacityFleet{capacity: capacity})
}

// capacityFleet says which of the two fleet arrangements the fixture seeds, because the two refusals this
// file drives arise on DIFFERENT ones and neither can be reached from the other's.
//
// A PROJECT-OWNED machine is reachable by its own project alone, so "another customer holds it" is not a
// state that arrangement can be in — a fixture that wrote one anyway would be asserting against a world
// production cannot produce, which is this tree's most expensive test shape.
//
// A PLANE-OWNED one (`project_id IS NULL` on the pool AND the machine) is where "devices are free and
// customers take turns" lives, and it is what every deployment measured so far actually has: the native
// bring-up enrols its Mac into a pool nobody owns, and both customers this file seeds reach it because it
// belongs to neither of them.
type capacityFleet struct {
	capacity   int
	planeOwned bool
}

// newFreeFleetFixture seeds the arrangement above: one plane-owned machine in `pool_default`, which the
// fixture's tenant reaches only because it belongs to nobody.
func newFreeFleetFixture(t *testing.T, capacity int) *capacityFixture {
	t.Helper()
	return newFleetFixture(t, capacityFleet{capacity: capacity, planeOwned: true})
}

func newFleetFixture(t *testing.T, spec capacityFleet) *capacityFixture {
	t.Helper()
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run TEST=postgres scripts/test/component")
	}
	ctx := context.Background()
	cs, err := coordinator.Open(ctx, url)
	if err != nil {
		t.Fatalf("coordinator.Open: %v", err)
	}
	t.Cleanup(cs.Close)
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	repo, err := store.Open(ctx, url)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(repo.Close)

	f := &capacityFixture{
		cs: cs, repo: repo, root: t.TempDir(),
		tenant:     coordinator.Tenant{Project: redeliveryID("prj")},
		sessionID:  redeliveryID("ses"),
		responseID: redeliveryID("resp"),
		runID:      redeliveryID("run"),
		poolID:     redeliveryID("pool"),
		machine:    redeliveryID("rnr"),
		wsID:       redeliveryID("wsp"),
	}
	bindingID := redeliveryID("bnd")
	pool := cs.Pool()
	execSQL(t, pool, `INSERT INTO projects (id) VALUES ($1)`, f.tenant.Project)
	// The pool is NAMED 'default' because `fleet.ResolvePool`'s last-but-one step resolves the tenant's
	// own pool by that name; a randomly-named one would send `place` to the bootstrap CONSTANT and the run
	// would fail as not-servable long before it reached a machine.
	if spec.planeOwned {
		// THE POOL IS RANDOMLY NAMED AND REACHED THROUGH THE PROJECT'S POLICY, not through
		// `fleet.DefaultPoolID`. Production's free-fleet Mac sits in that CONSTANT, and seeding the constant
		// is what a faithful fixture reaches for first — but the component tier runs every case against ONE
		// database, so a well-known id is a row every other test can see. Measured, not reasoned about: the
		// first draft of this fixture did seed `pool_default` and reddened three tests in other packages,
		// one of them by counting pools that had nothing to do with it.
		//
		// The policy pool is ResolvePool's THIRD step and sits ABOVE the tenant-default the constant is the
		// fallback for, so the resolution reaches a plane-owned pool by a route production also has — a
		// project pinned to a pool — and every arrangement below stays what this test is about: the pool and
		// the machine belong to NOBODY, which is the only shape in which two customers can want one Mac.
		execSQL(t, pool, `UPDATE projects SET config_policy = jsonb_build_object('pool', $2::text) WHERE id = $1`,
			f.tenant.Project, f.poolID)
		execSQL(t, pool, `INSERT INTO runner_pools (id, project_id, name, posture)
		                  VALUES ($1,NULL,$2,'unsandboxed-host')`, f.poolID, "plane-"+f.poolID)
		execSQL(t, pool, `INSERT INTO runners (id, project_id, pool_id, state, posture, capacity)
		                  VALUES ($1,NULL,$2,'active','unsandboxed-host',$3)`, f.machine, f.poolID, spec.capacity)
		// ‼️ AND IT TAKES ITS ROWS BACK OUT, which the project-owned arm does not have to and this one does.
		// A pool nobody owns is visible to EVERYONE — the RLS qual on runner_pools and runners admits
		// `project_id IS NULL` to every tenant, which is the whole mechanism a free fleet is made of — so a
		// row left here is a row in every other test's view of its own fleet. Measured: it reddened
		// TestPoolBirthValidatesThePostureOnTheRouteRatherThanAtTheDatabase, whose claim is that four refused
		// creates leave the caller's pool list EMPTY, by putting a fifth pool in a list it had never touched.
		//
		// The order is the foreign keys' and not preference: the run points at the pool, the holds point at
		// the machine, and the machine points at the pool.
		t.Cleanup(func() {
			ctx := storage.WithSystemScope(context.Background())
			for _, stmt := range []struct {
				sql  string
				args []any
			}{
				{`UPDATE runs SET pool_id = NULL WHERE pool_id = $1`, []any{f.poolID}},
				{`DELETE FROM runner_leases WHERE runner_id = $1`, []any{f.machine}},
				{`DELETE FROM runners WHERE id = $1`, []any{f.machine}},
				{`DELETE FROM runner_pools WHERE id = $1`, []any{f.poolID}},
			} {
				if _, err := pool.Exec(ctx, stmt.sql, stmt.args...); err != nil {
					t.Errorf("take the plane-owned fixture back out (%q): %v — every test after this one now sees a pool and a machine that are not theirs", stmt.sql, err)
				}
			}
		})
	} else {
		execSQL(t, pool, `INSERT INTO runner_pools (id, project_id, name, posture)
		                  VALUES ($1,$2,'default','unsandboxed-host')`, f.poolID, f.tenant.Project)
		execSQL(t, pool, `INSERT INTO runners (id, project_id, pool_id, state, posture, capacity)
		                  VALUES ($1,$2,$3,'active','unsandboxed-host',$4)`,
			f.machine, f.tenant.Project, f.poolID, spec.capacity)
	}
	execSQL(t, pool, `INSERT INTO sessions (id, project_id) VALUES ($1,$2)`, f.sessionID, f.tenant.Project)
	execSQL(t, pool, `INSERT INTO responses (id, project_id, session_id, state, input)
	                  VALUES ($1,$2,$3,'queued','"build it"'::jsonb)`, f.responseID, f.tenant.Project, f.sessionID)
	execSQL(t, pool, `INSERT INTO runs (id, project_id, session_id, response_id, state)
	                  VALUES ($1,$2,$3,$4,'queued')`, f.runID, f.tenant.Project, f.sessionID, f.responseID)
	execSQL(t, pool, `INSERT INTO repository_bindings (id, project_id, provider, repository_identity, clone_url)
	                  VALUES ($1,$2,'local','palai/fixture','file:///dev/null')`, bindingID, f.tenant.Project)

	if err := cs.CreateWorkspace(ctx, f.tenant, coordinator.WorkspaceInput{
		WorkspaceID: f.wsID, SessionID: f.sessionID, State: "ready",
	}); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	// WorkspaceForSession requires `repository_binding_id <> ''`, and CreateWorkspace takes no binding —
	// so the attachment is written here. Without it planRootWorkspace answers found=false and the attempt
	// skips the whole workspace arm, which is where the hold lives.
	execSQL(t, pool, `UPDATE workspaces SET repository_binding_id = $2 WHERE id = $1`, f.wsID, bindingID)
	if _, err := cs.AllocateWorkspace(storage.WithTenant(ctx, f.tenant.Project),
		redeliveryID("wal"), f.wsID, f.root); err != nil {
		t.Fatalf("AllocateWorkspace: %v", err)
	}
	capacityGitRepo(t, f.root)

	f.orch = NewOrchestrator(repo, nil, modelbroker.New(modelbroker.Config{}), toolbroker.New())
	// The provisioner is wired because `wsPlanned` is gated on it, and the broker is a real one for the
	// same reason: it is never CALLED on the reuse path (the allocation already exists), but a nil one
	// turns the whole arm off and the attempt would never reach a machine's ceiling.
	f.orch.SetWorkspaceProvisioner(f.root, repositories.NewAnonymousBroker())
	return f
}

// capacityGitRepo makes the allocation's repo directory a real repository with one commit, because
// realizeRootWorkspace's reuse path reads the HEAD it records a preparation receipt from. Seeding a
// receipt instead would skip that read — and would quietly make every attempt below a SECOND attempt,
// which is not the case the ceiling is interesting in.
func capacityGitRepo(t *testing.T, root string) {
	t.Helper()
	repoDir := workspace.RepoPath(root)
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("make the allocation's repo dir: %v", err)
	}
	for _, argv := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.email=fixture@palai.test", "-c", "user.name=fixture", "commit", "-q", "--allow-empty", "-m", "base"},
	} {
		cmd := exec.Command("git", append([]string{"-C", repoDir}, argv...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v in %s: %v\n%s", argv, repoDir, err, out)
		}
	}
}

// capacityDialer hands the run loop a scripted engine on a channel that NAMES the fixture's machine. It is
// scriptedDialer without an executor: nothing below calls a tool, and what every claim here needs from the
// channel is the one thing `machineOf` reads off a real lease.
type capacityDialer struct {
	ch      *scriptedChannel
	machine string
}

func (d *capacityDialer) Dial(context.Context, AttemptDescriptor) (EngineChannel, error) {
	return hostMachineChannel{EngineChannel: d.ch, machineID: d.machine}, nil
}

// runAttempt drives ONE whole attempt whose engine would report `completed` if it were ever asked. It
// returns ExecuteAttempt's error and the channel, so a caller can assert on what the engine was SENT —
// which is what tells a park apart from a run that happened to end in the same state.
func (f *capacityFixture) runAttempt(t *testing.T, jobID string) (*scriptedChannel, error) {
	t.Helper()
	ch := &scriptedChannel{frames: []contracts.EngineFrame{
		engineFrame(1, "engine.ready", map[string]any{
			"selected_protocol": engineProtocol, "engine": map[string]any{"version": "test"},
		}),
		engineFrame(2, "run.terminal", map[string]any{"outcome": "completed"}),
	}}
	f.orch.dialer = &capacityDialer{ch: ch, machine: f.machine}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return ch, f.orch.ExecuteAttempt(ctx, AttemptDescriptor{
		RunID:     contracts.RunID(f.runID),
		AttemptID: contracts.AttemptID(redeliveryID("att")),
		Tenant:    f.tenant,
		Fence:     1,
		JobID:     jobID,
	})
}

// fillMachine opens `n` holds on the fixture's machine by OTHER sessions, through the same HoldMachine the
// attempt path calls. Distinct sessions because an occupancy is a session's hold: repeated holds by one
// session fold into one row and would fill nothing.
func (f *capacityFixture) fillMachine(t *testing.T, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		other := redeliveryID("ses")
		execSQL(t, f.cs.Pool(), `INSERT INTO sessions (id, project_id) VALUES ($1,$2)`, other, f.tenant.Project)
		if _, err := f.cs.HoldMachine(ctx, f.tenant, other, f.machine); err != nil {
			t.Fatalf("fill hold %d of %d: %v — the machine is not full, so nothing below is about capacity", i+1, n, err)
		}
	}
}

func (f *capacityFixture) openHolds(t *testing.T) int {
	t.Helper()
	var open int
	if err := f.cs.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(*) FROM runner_leases WHERE runner_id = $1 AND released_at IS NULL`, f.machine).Scan(&open); err != nil {
		t.Fatalf("count the open holds on %s: %v", f.machine, err)
	}
	return open
}

func (f *capacityFixture) runState(t *testing.T) string {
	t.Helper()
	var state string
	if err := f.cs.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT state FROM runs WHERE id = $1`, f.runID).Scan(&state); err != nil {
		t.Fatalf("read run state: %v", err)
	}
	return state
}

// parkedAttempts counts this run's attempts by the two groups that decide whether anything still believes
// it is driving the run: the live states the one-active-per-run index covers, and the capacity marker.
func (f *capacityFixture) parkedAttempts(t *testing.T) (live, parked int) {
	t.Helper()
	if err := f.cs.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(*) FILTER (WHERE state IN ('assigned','starting','active','draining')),
		        count(*) FILTER (WHERE state = $2)
		   FROM attempts WHERE run_id = $1`, f.runID, coordinator.AttemptAwaitingCapacity).Scan(&live, &parked); err != nil {
		t.Fatalf("read the run's attempts: %v", err)
	}
	return live, parked
}

// TestADeclaredFullMachineParksTheRunInsteadOfRunningItUnmetered — A CEILING THAT DOES NOT STOP ANYTHING
// IS NOT A CEILING.
//
// A machine declares two, two other sessions fill it, and this run's attempt lands on it. Before T6 the
// refusal was logged and the attempt RAN: the engine was driven, the run reached `completed`, and the
// machine carried a third session it had said it would not take — with no row saying so, so the time was
// unbilled as well as unadmitted. The run must PARK instead: `waiting`, with the marker its own wake reads.
//
// THE ENGINE IS THE DISCRIMINATOR AND NOT THE OCCUPANCY COUNT. "No occupancy for this session" is true of
// the broken build too — that is precisely what "runs it unmetered" means — so a test that asserted only
// the absent row would pass on the behaviour it exists to fail. What separates them is whether the ENGINE
// WAS SPOKEN TO at all.
//
// THE SECOND HALF IS THE ONE WITHOUT WHICH THE FIRST IS FREE. `runners.capacity = 0` means a machine that
// declared NOTHING, and it must be admitted exactly as before — an implementation that parked on every
// machine would satisfy every assertion above while taking every deployment in existence offline, because
// no machine in any deployment declares a capacity yet.
func TestADeclaredFullMachineParksTheRunInsteadOfRunningItUnmetered(t *testing.T) {
	f := newCapacityFixture(t, 2)
	f.fillMachine(t, 2)

	// NON-VACUITY: unless the machine is really full, the park below is about something else.
	if open := f.openHolds(t); open != 2 {
		t.Fatalf("%d open hold(s) on the two-capacity machine, want 2 — it is not full, so nothing below is a statement about capacity", open)
	}

	ch, err := f.runAttempt(t, "")
	if err != nil {
		t.Fatalf("ExecuteAttempt onto a full machine returned %v, want nil — a park that returns an error is a FAILED attempt, and five of those dead-letter the run", err)
	}
	if got := f.runState(t); got != "waiting" {
		t.Fatalf("run state = %q after landing on a machine at its declared capacity, want waiting — the machine said no and the run happened anyway, on a machine carrying more sessions than it admitted to and with no row to bill it by", got)
	}
	// THE ENGINE WAS NEVER SPOKEN TO. This is the assertion the shipped build fails: it dialled, held
	// nothing, and drove the whole conversation regardless.
	if len(ch.sent) != 0 {
		t.Fatalf("the parked attempt sent %d frame(s) to the engine (%v), want 0 — the run was executed on a machine that refused to hold it", len(ch.sent), ch.sent)
	}
	// AND IT PARKED WHERE ITS OWN WAKE CAN FIND IT. The marker is the wake's positive predicate; a run
	// `waiting` without it is a run no machine's arrival and no settling hold will ever re-enter.
	live, parked := f.parkedAttempts(t)
	if live != 0 || parked != 1 {
		t.Fatalf("the parked run has %d live attempt(s) and %d awaiting-capacity attempt(s), want 0 and 1 — a live attempt row is a run something still believes it is driving", live, parked)
	}
	// The refusal wrote no hold, so the machine is exactly as full as it declared — not one over.
	if open := f.openHolds(t); open != 2 {
		t.Fatalf("%d open hold(s) after the refused attempt, want 2 — the refusal was reported and a row was opened anyway", open)
	}

	t.Run("a machine that declared no capacity still runs the attempt", func(t *testing.T) {
		undeclared := newCapacityFixture(t, 0)
		// The same three sessions that would have overrun a two-capacity machine. An undeclared machine has
		// no ceiling, so they are simply held.
		undeclared.fillMachine(t, 3)
		ch, err := undeclared.runAttempt(t, "")
		if err != nil {
			t.Fatalf("ExecuteAttempt onto an undeclared machine returned %v, want nil", err)
		}
		if got := undeclared.runState(t); got != "completed" {
			t.Fatalf("run state = %q on a machine that declared NO capacity, want completed — a park here takes every deployment in existence offline, because no machine anywhere declares one yet", got)
		}
		if len(ch.sent) == 0 {
			t.Fatal("the attempt on an undeclared machine sent the engine nothing — the run did not execute, so the park is being applied to a machine with no ceiling at all")
		}
		// AND IT WAS METERED: an undeclared machine admits the hold, which is the row a park would have
		// prevented and the whole reason the ceiling is opt-in.
		if open := undeclared.openHolds(t); open != 4 {
			t.Fatalf("%d open hold(s) on the undeclared machine, want 4 — the attempt ran without opening its own hold, which is the unmetered case wearing a different hat", open)
		}
	})
}

// TestARunParkedForCapacityDoesNotSpendItsRetryBudgetWaiting — WAITING FOR A MACHINE IS NOT FAILING.
//
// A capacity park ends its attempt with nil, so the worker COMPLETES the job, and the wake mints the next
// one seeded with what the run has already spent (EnqueueWokenRunJob, E24 T4). That carry is right and
// stays: a run failing for a reason unrelated to capacity must not get a fresh ladder every time a park
// interrupts it. What it ALSO did was charge the run for the parking attempt itself — so five parks
// dead-lettered a run that had never failed at anything.
//
// THAT IS WHY THIS TEST EXISTS AND WHY IT IS ORDERED AFTER THE WAKE. A pool-keyed wake may re-enter a run
// onto a pool whose other machines are full, and the honest answer to "is that acceptable?" is: only if a
// park costs nothing. If it costs one attempt, every spurious wake is a step toward a dead letter and the
// wake would have to be keyed on the MACHINE instead. It costs nothing, so it is.
//
// THE COUNT IS DRIVEN THROUGH A REAL CLAIM, never written by hand: ClaimJob is what raises attempt_count,
// and a fixture that set the number itself would prove nothing about the row production actually leaves.
func TestARunParkedForCapacityDoesNotSpendItsRetryBudgetWaiting(t *testing.T) {
	f := newCapacityFixture(t, 1)
	f.fillMachine(t, 1)
	ctx := context.Background()

	// The run has genuinely failed three times, and the count is raised the ONLY way production raises it:
	// by claiming the job. Each claim takes a 1ms lease and is then abandoned, which is what a crashed
	// attempt leaves behind and what lets the next claim reclaim the row.
	const failed = 3
	jobID := redeliveryID("job")
	execSQL(t, f.cs.Pool(), `INSERT INTO durable_jobs (id, project_id, kind, status, payload)
	                         VALUES ($1,$2,'response.run','queued',jsonb_build_object('run_id',$3::text))`,
		jobID, f.tenant.Project, f.runID)
	for i := 0; i < failed; i++ {
		if _, err := f.cs.Claim(ctx, f.tenant, jobID, redeliveryID("owner"), time.Millisecond); err != nil {
			t.Fatalf("claim %d of %d: %v", i+1, failed, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := f.jobAttempts(t, jobID); got != failed {
		t.Fatalf("the job has spent %d attempt(s), want %d — the budget below is not the one this test means", got, failed)
	}

	// THE FOURTH CLAIM IS THE ONE THAT PARKS, and it is a real claim: the budget is genuinely spent before
	// the attempt discovers the machine is full, which is the only way the refund can be about anything.
	claim, err := f.cs.Claim(ctx, f.tenant, jobID, redeliveryID("owner"), time.Minute)
	if err != nil {
		t.Fatalf("claim the attempt that parks: %v", err)
	}
	if got := f.jobAttempts(t, jobID); got != failed+1 {
		t.Fatalf("the parking claim left attempt_count %d, want %d — the claim did not spend anything, so a refund below would be measuring nothing", got, failed+1)
	}
	if _, err := f.runAttempt(t, jobID); err != nil {
		t.Fatalf("ExecuteAttempt: %v", err)
	}
	if got := f.runState(t); got != "waiting" {
		t.Fatalf("run state = %q, want waiting — nothing parked, so there is no budget question to ask", got)
	}
	if got := f.jobAttempts(t, jobID); got != failed {
		t.Fatalf("the parked run has spent %d attempt(s), want %d — waiting for a machine cost it an attempt off a ladder it never failed on, and five such waits dead-letter a run that is only queueing",
			got, failed)
	}
	// The worker completes a job whose attempt returned nil, which is exactly what a park does.
	if err := f.cs.Complete(ctx, claim, "parked"); err != nil {
		t.Fatalf("complete the parked job: %v", err)
	}

	// AND THE CARRY SURVIVES THE WAKE: three cycles, and the number does not move. One cycle could be a
	// coincidence of arithmetic; three is the loop the live stack burned twenty-nine attempts in — and it
	// is what makes a POOL-keyed wake safe, because a run woken onto a pool whose other machines are full
	// re-parks for free instead of stepping toward a dead letter.
	seen := []string{jobID}
	for cycle := 1; cycle <= 3; cycle++ {
		woken, werr := f.cs.WakeRunAwaitingCapacity(ctx, f.tenant, f.poolID)
		if werr != nil {
			t.Fatalf("cycle %d: WakeRunAwaitingCapacity: %v", cycle, werr)
		}
		if woken != f.runID {
			t.Fatalf("cycle %d: the wake re-entered %q, want the parked run %q", cycle, woken, f.runID)
		}
		next := f.newestRunJob(t, seen)
		if got := f.jobAttempts(t, next); got != failed {
			t.Fatalf("cycle %d: the woken job starts at attempt_count %d, want %d — every park→wake round trip spends one, so a run waiting for a Mac to boot reaches a dead letter for waiting",
				cycle, got, failed)
		}
		nextClaim, cerr := f.cs.Claim(ctx, f.tenant, next, redeliveryID("owner"), time.Minute)
		if cerr != nil {
			t.Fatalf("cycle %d: claim the woken job: %v", cycle, cerr)
		}
		if _, aerr := f.runAttempt(t, next); aerr != nil {
			t.Fatalf("cycle %d: ExecuteAttempt: %v", cycle, aerr)
		}
		if got := f.runState(t); got != "waiting" {
			t.Fatalf("cycle %d: run state = %q, want waiting", cycle, got)
		}
		if got := f.jobAttempts(t, next); got != failed {
			t.Fatalf("cycle %d: the re-parked job has spent %d attempt(s), want %d", cycle, got, failed)
		}
		if cerr := f.cs.Complete(ctx, nextClaim, "parked"); cerr != nil {
			t.Fatalf("cycle %d: complete the parked job: %v", cycle, cerr)
		}
		seen = append(seen, next)
	}
}

// holdByAnotherCustomer opens a hold on the fixture's machine for a DIFFERENT project, through the same
// HoldMachine the attempt path calls, and returns that occupancy's id so a caller can settle it.
func (f *capacityFixture) holdByAnotherCustomer(t *testing.T) string {
	t.Helper()
	other := coordinator.Tenant{Project: redeliveryID("prj")}
	session := redeliveryID("ses")
	execSQL(t, f.cs.Pool(), `INSERT INTO projects (id) VALUES ($1)`, other.Project)
	execSQL(t, f.cs.Pool(), `INSERT INTO sessions (id, project_id) VALUES ($1,$2)`, session, other.Project)
	occupancy, err := f.cs.HoldMachine(context.Background(), other, session, f.machine)
	if err != nil {
		t.Fatalf("the other customer's hold on the free-fleet machine: %v — nobody is holding it, so nothing below is about tenancy", err)
	}
	return occupancy
}

// TestAMachineHeldByAnotherCustomerParksTheRunInsteadOfSharingIt — THE CROSSING THIS GUARD EXISTS TO STOP
// WAS HAPPENING WHILE THE GUARD WATCHED.
//
// tests/component/fleet/tenant_exclusivity_test.go proves the COORDINATOR refuses the second customer, and
// it passed every day this defect shipped: it drives AcquireLease, and what a store returns is not what a
// platform does. holdMachine let ErrMachineHeldByAnotherTenant fall into its generic `err != nil` arm,
// logged "is being occupied unmetered", and returned nil — so ExecuteAttempt's park arm, which names this
// error and has since A.4 T6, could never be reached with it. MEASURED on the live plane 2026-08-07: five
// such log lines, and ses_a6dc5d6e went on to clone a repository and run a shell command on a Mac another
// customer was holding, with no row anywhere saying it had been there.
//
// THE MACHINE DECLARES THREE, which is what makes this a statement about tenancy at all: with room to
// spare, the ceiling cannot be what turns this run away, and only the exclusivity rule would.
//
// AND THE ENGINE IS THE DISCRIMINATOR, for capacity's reason: "no occupancy for this session" is equally
// true of the broken build — that is what unmetered MEANS — so the assertion that separates them is
// whether the run was executed on the machine at all.
func TestAMachineHeldByAnotherCustomerParksTheRunInsteadOfSharingIt(t *testing.T) {
	f := newFreeFleetFixture(t, 3)
	occupancy := f.holdByAnotherCustomer(t)

	// NON-VACUITY: one hold on a machine that takes three. The machine has ROOM, so a park below cannot be
	// the ceiling answering under another name.
	if open := f.openHolds(t); open != 1 {
		t.Fatalf("%d open hold(s) on the three-capacity machine, want 1 — the other customer is not holding it, or the machine is full for a reason that is not tenancy", open)
	}

	ch, err := f.runAttempt(t, "")
	if err != nil {
		t.Fatalf("ExecuteAttempt onto a machine another customer holds returned %v, want nil — a park that returns an error is a FAILED attempt, and five of those dead-letter a run that only has to wait", err)
	}
	if got := f.runState(t); got != "waiting" {
		t.Fatalf("run state = %q on a Mac another customer is holding, want waiting — two customers were on one machine, which docs/operations/mac-sessions.md §5 says the product does not permit and 000008 exists to enforce", got)
	}
	if len(ch.sent) != 0 {
		t.Fatalf("the parked attempt sent %d frame(s) to the engine (%v), want 0 — the run executed on another customer's machine", len(ch.sent), ch.sent)
	}
	live, parked := f.parkedAttempts(t)
	if live != 0 || parked != 1 {
		t.Fatalf("the parked run has %d live attempt(s) and %d awaiting-capacity attempt(s), want 0 and 1 — a run parked without the marker is one no settling hold will ever wake", live, parked)
	}
	if open := f.openHolds(t); open != 1 {
		t.Fatalf("%d open hold(s) after the refused attempt, want 1 — a second customer's row was opened on a machine that serves one at a time", open)
	}

	// ‼️ AND THE OTHER HALF, WITHOUT WHICH THE FIRST IS FREE. An implementation that parked on every
	// free-fleet machine satisfies every line above while serving nobody — and a single-Mac plane is every
	// deployment measured so far, so "parks always" and "correct" are one commit apart. Settling the other
	// customer's hold makes the machine free, and the SAME run must then take it: this is also the proof
	// that the park drains, because the settle is the wake's own trigger.
	t.Run("the same machine runs the attempt once the other customer's hold settles", func(t *testing.T) {
		if _, err := f.cs.SettleOccupancy(context.Background(),
			coordinator.Tenant{Project: f.holderProject(t, occupancy)}, occupancy, coordinator.ReleaseReasonIdle); err != nil {
			t.Fatalf("settle the other customer's hold: %v", err)
		}
		if open := f.openHolds(t); open != 0 {
			t.Fatalf("%d open hold(s) after the settle, want 0 — the machine is still held, so what runs below would not be about tenancy either", open)
		}
		woken, werr := f.cs.WakeRunAwaitingCapacity(context.Background(), f.tenant, f.poolID)
		if werr != nil {
			t.Fatalf("WakeRunAwaitingCapacity after the hold settled: %v", werr)
		}
		if woken != f.runID {
			t.Fatalf("the settle woke %q, want the parked run %q — a run parked on a tenancy refusal has no route back, which is worse than sharing the Mac because it never ends", woken, f.runID)
		}
		ch, err := f.runAttempt(t, "")
		if err != nil {
			t.Fatalf("ExecuteAttempt on the freed machine: %v", err)
		}
		if got := f.runState(t); got != "completed" {
			t.Fatalf("run state = %q on a machine nobody else holds, want completed — the run is being refused a free Mac", got)
		}
		if len(ch.sent) == 0 {
			t.Fatal("the attempt on the freed machine sent the engine nothing — the park is being applied to a machine with no other customer on it")
		}
		if open := f.openHolds(t); open != 1 {
			t.Fatalf("%d open hold(s) after the run took the freed machine, want 1 — it ran without opening its own hold, which is the unmetered case again", open)
		}
	})
}

// holderProject reads the project an occupancy belongs to, because SettleOccupancy is tenant-scoped and the
// holder here is a project the test generated rather than the fixture's own.
func (f *capacityFixture) holderProject(t *testing.T, occupancyID string) string {
	t.Helper()
	var project string
	if err := f.cs.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT project_id FROM runner_leases WHERE id = $1`, occupancyID).Scan(&project); err != nil {
		t.Fatalf("read the holder of occupancy %s: %v", occupancyID, err)
	}
	return project
}

func (f *capacityFixture) jobAttempts(t *testing.T, jobID string) int {
	t.Helper()
	var count int
	if err := f.cs.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT attempt_count FROM durable_jobs WHERE id = $1`, jobID).Scan(&count); err != nil {
		t.Fatalf("read job %s attempt_count: %v", jobID, err)
	}
	return count
}

// newestRunJob returns this run's newest response.run job that is not one of `seen` — the row the wake
// just enqueued. Ordered TOTALLY (created_at, id), the same order EnqueueWokenRunJob's own seed subquery
// uses: an unordered LIMIT 1 has decided outcomes in this tree twice.
func (f *capacityFixture) newestRunJob(t *testing.T, seen []string) string {
	t.Helper()
	var id string
	if err := f.cs.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT id FROM durable_jobs
		  WHERE kind = 'response.run' AND payload->>'run_id' = $1 AND id <> ALL($2::text[])
		  ORDER BY created_at DESC, id DESC LIMIT 1`, f.runID, seen).Scan(&id); err != nil {
		t.Fatalf("read the job the wake enqueued: %v", err)
	}
	return id
}
