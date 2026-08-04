//go:build component

// Package fleet holds the real-PostgreSQL component tests for a MACHINE OCCUPANCY: the interval in which
// one session held one machine. They run under `make test-component TEST=postgres`, which starts a
// throwaway container and exports PALAI_COMPONENT_POSTGRES_URL; the build tag keeps them out of the
// credential-free, Docker-free unit tier.
//
// WHY THIS PACKAGE EXISTS. `runner_leases` is in the baseline's table list and has never had a writer:
//
//	grep -rniE 'INSERT INTO runner_leases' --include='*.go' --include='*.sql' . | grep -v node_modules | wc -l
//	→ 0   (2026-08-04, before this task)
//
// Two things are billed and only one of them is measured. Model tokens settle into usage_ledger; the
// MACHINE half has no row anywhere. These tests drive the writer that closes that, and they drive the
// arithmetic rather than the schema — a column that exists and is filled with the wrong number is worse
// than a column that is missing, because the missing one is visible.
//
// THE BILLING RULE THEY HOLD:
//
//	closed by idle : last_activity_at - started_at
//	any other way  : released_at      - started_at
//
// A SESSION IS NOT AN OCCUPANCY. A session is closed for idleness, its machine account is destroyed, and
// the next approval resumes it on a DIFFERENT machine — so one session holds N machines over its life and
// the bill is the SUM of those holds, never the session's wall clock. That is why the row is per
// occupancy, and TestOneSessionTwoOccupanciesTwoRows measures the gap between two of them going unbilled.
package fleet

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/storage"
)

func openHarness(t *testing.T) *coordinator.Store {
	t.Helper()
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-component TEST=postgres")
	}
	cs, err := coordinator.Open(context.Background(), url)
	if err != nil {
		t.Fatalf("coordinator.Open() error = %v", err)
	}
	t.Cleanup(cs.Close)
	if err := cs.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	return cs
}

func newID(prefix string) string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return prefix + "_" + hex.EncodeToString(raw[:])
}

// fleetEnv is one tenant with a pool and TWO machines in it. Two rather than one is not spare fixture:
// the first test's whole subject is a session that resumes somewhere ELSE, and a fixture with one machine
// would let a build that reuses a single open row pass it.
type fleetEnv struct {
	cs      *coordinator.Store
	tenant  coordinator.Tenant
	runnerA string
	runnerB string
}

func newFleetEnv(t *testing.T) *fleetEnv {
	t.Helper()
	cs := openHarness(t)
	env := &fleetEnv{
		cs:      cs,
		tenant:  coordinator.Tenant{Project: newID("prj")},
		runnerA: newID("rnr"),
		runnerB: newID("rnr"),
	}
	pool := newID("pool")
	env.exec(t, `INSERT INTO projects (id) VALUES ($1)`, env.tenant.Project)
	env.exec(t, `INSERT INTO runner_pools (id, project_id, name, posture)
	             VALUES ($1, $2, 'default', 'unsandboxed-host')`, pool, env.tenant.Project)
	for _, runner := range []string{env.runnerA, env.runnerB} {
		env.exec(t, `INSERT INTO runners (id, project_id, pool_id, state, posture)
		             VALUES ($1, $2, $3, 'active', 'unsandboxed-host')`, runner, env.tenant.Project, pool)
	}
	return env
}

func (e *fleetEnv) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := e.cs.Pool().Exec(storage.WithSystemScope(context.Background()), sql, args...); err != nil {
		t.Fatalf("exec %q error = %v", sql, err)
	}
}

func (e *fleetEnv) seedSession(t *testing.T) string {
	t.Helper()
	sessionID := newID("ses")
	e.exec(t, `INSERT INTO sessions (id, project_id) VALUES ($1, $2)`, sessionID, e.tenant.Project)
	return sessionID
}

func (e *fleetEnv) mustAcquire(t *testing.T, sessionID, runnerID string) string {
	t.Helper()
	leaseID, err := e.cs.AcquireLease(context.Background(), e.tenant, sessionID, runnerID)
	if err != nil {
		t.Fatalf("AcquireLease(%s, %s) error = %v", sessionID, runnerID, err)
	}
	return leaseID
}

func (e *fleetEnv) mustTouch(t *testing.T, leaseID string) {
	t.Helper()
	if err := e.cs.TouchLease(context.Background(), e.tenant, leaseID); err != nil {
		t.Fatalf("TouchLease(%s) error = %v", leaseID, err)
	}
}

func (e *fleetEnv) mustRelease(t *testing.T, leaseID, reason string) {
	t.Helper()
	if err := e.cs.ReleaseLease(context.Background(), e.tenant, leaseID, reason); err != nil {
		t.Fatalf("ReleaseLease(%s, %q) error = %v", leaseID, reason, err)
	}
}

func (e *fleetEnv) occupancies(t *testing.T, sessionID string) []coordinator.Occupancy {
	t.Helper()
	rows, err := e.cs.SessionOccupancies(context.Background(), e.tenant, sessionID)
	if err != nil {
		t.Fatalf("SessionOccupancies(%s) error = %v", sessionID, err)
	}
	return rows
}

func (e *fleetEnv) billed(t *testing.T, leaseID string) time.Duration {
	t.Helper()
	occupancy, err := e.cs.Occupancy(context.Background(), e.tenant, leaseID)
	if err != nil {
		t.Fatalf("Occupancy(%s) error = %v", leaseID, err)
	}
	return occupancy.Billed
}

// occupancyGap is the time this session holds NO machine at all, between releasing the first and
// acquiring the second. It is the interval the bill must not contain.
const occupancyGap = 400 * time.Millisecond

// TestOneSessionTwoOccupanciesTwoRows — ONE SESSION, TWO OCCUPANCIES, TWO ROWS.
//
// A session is closed for idleness and its machine account destroyed; the human approves and it resumes
// where it left off, on whichever machine is free. That is two holds of two machines with a gap in
// between, and the bill is the SUM of the two holds. A row per SESSION could not express it: it would
// have one started_at and one released_at, and everything between them would be billed — including the
// interval in which the customer held nothing at all.
func TestOneSessionTwoOccupanciesTwoRows(t *testing.T) {
	env := newFleetEnv(t)
	session := env.seedSession(t)

	first := env.mustAcquire(t, session, env.runnerA)
	env.mustRelease(t, first, "idle")

	// The session holds NOTHING here. Whatever this costs the customer is the defect being measured.
	time.Sleep(occupancyGap)

	second := env.mustAcquire(t, session, env.runnerB)
	env.mustRelease(t, second, "closed")

	rows := env.occupancies(t, session)
	if len(rows) != 2 {
		t.Fatalf("%d lease row(s) for one session, want 2 — a session holds a machine N times and each hold is its own row", len(rows))
	}
	// FIXTURE NON-VACUITY. If both holds landed on the same machine the assertion above would also pass
	// for a build that ignores the runner argument entirely, and the "resumes somewhere else" property
	// this test is named for would never have been exercised.
	if rows[0].RunnerID == rows[1].RunnerID {
		t.Fatalf("both occupancies name machine %q — the fixture handed out one machine twice, so this test proved nothing about resuming elsewhere", rows[0].RunnerID)
	}
	for _, row := range rows {
		if row.ReleasedAt.IsZero() {
			t.Errorf("occupancy %s was never released — an open hold bills forever", row.ID)
		}
	}
	if t.Failed() {
		return
	}

	// THE BILL IS THE SUM OF THE HOLDS, NOT THE SESSION'S WALL CLOCK. The gap above is real time in which
	// no machine was held, so the two billed intervals together must fall short of the session's span by
	// at least it. A build that timed the SESSION would land on the wall clock exactly.
	sum := env.billed(t, first) + env.billed(t, second)
	wall := rows[1].ReleasedAt.Sub(rows[0].StartedAt)
	if wall-sum < occupancyGap*3/4 {
		t.Fatalf("the two occupancies bill %s against a session span of %s — the %s in which this session held no machine was charged to the customer",
			sum, wall, occupancyGap)
	}
}

// reaperIdleTTL is the TTL a reaper would have been configured with in this test. It is DELIBERATELY not
// passed to anything: the point is that no implementation may subtract it.
const reaperIdleTTL = 1 * time.Second

// billingTolerance bounds clock and scheduling noise between a client-side sleep and a server-side
// clock_timestamp(). The three candidate answers this test distinguishes are separated by more than a
// second, so this can be generous without weakening anything — and the test asserts that separation
// rather than assuming it.
const billingTolerance = 400 * time.Millisecond

// TestAnIdleClosedOccupancyBillsToLastActivityNotToTheReaper — THE TAIL IS OURS, AND WHAT IS SUBTRACTED
// IS `last_activity_at`, NOT A CONSTANT.
//
// A session goes quiet, the idle TTL expires, and a reaper eventually ends the hold. The customer is
// billed to their LAST ACTIVITY; the quiet tail is the platform's cost of holding the machine open in
// case they came back.
//
// THE REAPER RUNS LATE HERE ON PURPOSE. Two implementations answer this correctly only if they read the
// column: `released_at - started_at` charges the whole tail, and `released_at - TTL - started_at` charges
// however long the reaper's own tick was late by. A sweep is late by definition — it runs on a tick, not
// on an alarm — so the constant-subtraction form is not a hypothetical, it is what a reaper produces.
func TestAnIdleClosedOccupancyBillsToLastActivityNotToTheReaper(t *testing.T) {
	env := newFleetEnv(t)
	session := env.seedSession(t)
	lease := env.mustAcquire(t, session, env.runnerA)

	time.Sleep(300 * time.Millisecond)
	env.mustTouch(t, lease) // the last thing this session ever did

	// The TTL expires ~1.2s from now and the reaper does not arrive until well after that.
	time.Sleep(2200 * time.Millisecond)
	env.mustRelease(t, lease, "idle")

	rows := env.occupancies(t, session)
	if len(rows) != 1 {
		t.Fatalf("%d lease row(s), want 1", len(rows))
	}
	row := rows[0]
	if row.ReleaseReason != "idle" {
		t.Fatalf("release reason = %q, want idle — the billing rule keys on it, so a hold closed under any other word is billed to released_at", row.ReleaseReason)
	}

	toLastActivity := row.LastActivityAt.Sub(row.StartedAt) // the right answer
	toRelease := row.ReleasedAt.Sub(row.StartedAt)          // charges the whole idle tail
	toFixedTTL := toRelease - reaperIdleTTL                 // charges the reaper's lateness

	// NON-VACUITY: unless the three candidates are genuinely apart, this test cannot tell them apart and
	// every assertion below is satisfied by all three.
	if toRelease-toLastActivity < 2*billingTolerance || toFixedTTL-toLastActivity < 2*billingTolerance {
		t.Fatalf("the three candidate answers are %s / %s / %s — they are not separated by more than the %s tolerance, so this test cannot distinguish them",
			toLastActivity, toFixedTTL, toRelease, billingTolerance)
	}

	billed := env.billed(t, lease)
	if diff := billed - toLastActivity; diff < -billingTolerance || diff > billingTolerance {
		t.Fatalf("billed %s, want %s (last_activity_at - started_at)", billed, toLastActivity)
	}
	if diff := billed - toRelease; diff > -billingTolerance && diff < billingTolerance {
		t.Fatalf("billed %s, which is released_at - started_at (%s) — the idle tail was charged to the customer", billed, toRelease)
	}
	if diff := billed - toFixedTTL; diff > -billingTolerance && diff < billingTolerance {
		t.Fatalf("billed %s, which is released_at - a fixed %s TTL (%s) — a constant was subtracted instead of reading last_activity_at, so the customer pays for however late the reaper was",
			billed, reaperIdleTTL, toFixedTTL)
	}
}

// TestAParkedRunTakesNoLeaseAndIsNeverBilled — A PARKED RUN HOLDS NOTHING, SO IT IS BILLED NOTHING.
//
// A run whose pool has no free machine PARKS: it goes waiting and its attempt carries the
// `awaiting_capacity` marker until a machine arrives. It is holding no hardware while it waits, so it
// must own no occupancy row — a park that took a lease would bill the customer for a queue.
//
// THIS TEST HAS NO SUBJECT IN THE ACQUIRE PATH AND THAT IS WHY IT IS HERE. Nothing in this task calls
// AcquireLease from the park; the tasks that wire acquisition into the dispatch path come next, and this
// is the fence they have to stay behind. It is only worth anything because of the two assertions BEFORE
// the row count: a run that never parked would satisfy "no lease rows" trivially.
func TestAParkedRunTakesNoLeaseAndIsNeverBilled(t *testing.T) {
	env := newFleetEnv(t)
	ctx := context.Background()
	session := env.seedSession(t)

	response, runID, attemptID := newID("resp"), newID("run"), newID("att")
	env.exec(t, `INSERT INTO responses (id, project_id, session_id, state, input)
	             VALUES ($1, $2, $3, 'in_progress', '"hi"'::jsonb)`, response, env.tenant.Project, session)
	env.exec(t, `INSERT INTO runs (id, project_id, session_id, response_id, state)
	             VALUES ($1, $2, $3, $4, 'running')`, runID, env.tenant.Project, session, response)
	env.exec(t, `INSERT INTO attempts (id, project_id, run_id, fence, state)
	             VALUES ($1, $2, $3, 1, 'assigned')`, attemptID, env.tenant.Project, runID)

	if err := env.cs.ParkRunForCapacity(ctx, env.tenant, runID, attemptID); err != nil {
		t.Fatalf("ParkRunForCapacity() error = %v", err)
	}

	// The run REALLY parked. Without these two the row count below is an assertion about nothing.
	var runState, attemptState string
	sys := storage.WithSystemScope(ctx)
	if err := env.cs.Pool().QueryRow(sys, `SELECT state FROM runs WHERE id = $1`, runID).Scan(&runState); err != nil {
		t.Fatalf("read run state: %v", err)
	}
	if runState != "waiting" {
		t.Fatalf("run state = %q, want waiting — the run did not park, so the absence of a lease below proves nothing", runState)
	}
	if err := env.cs.Pool().QueryRow(sys, `SELECT state FROM attempts WHERE id = $1`, attemptID).Scan(&attemptState); err != nil {
		t.Fatalf("read attempt state: %v", err)
	}
	if attemptState != coordinator.AttemptAwaitingCapacity {
		t.Fatalf("attempt state = %q, want %q — the run is waiting for some other reason, so this is not the park being measured",
			attemptState, coordinator.AttemptAwaitingCapacity)
	}

	if rows := env.occupancies(t, session); len(rows) != 0 {
		t.Fatalf("a parked run owns %d occupancy row(s), want 0 — it is holding no machine, so billing it charges the customer for waiting in a queue (machine %q)",
			len(rows), rows[0].RunnerID)
	}
}
