//go:build fault

// Package scheduler holds the fault-injection proof for the schedule ticker's outage recovery (spec §33.3,
// AUT-008). It runs only under `make test-fault CASE=scheduler` (TEST is honored too), which starts a
// throwaway PostgreSQL container and exports PALAI_FAULT_POSTGRES_URL. The build tag keeps it out of the
// credential-free, Docker-free unit tier.
//
// It exercises the REAL supervised ticker loop against real Postgres: the loop is STOPPED (context cancel,
// the loop fully drains — NOT a sleep simulation) across a planned per-minute fire window whose passage is
// modelled by an injected clock jump, then RESTARTED. The recovery must yield exactly the misfire policy's
// occurrences (fire_once_now → the single latest missed; catch_up → bounded oldest-first), with ZERO
// duplicate occurrence ids and the planned_at-vs-admitted_at lateness visible.
package fault

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/apps/control-plane/internal/automation"
	"github.com/palgroup/palai/packages/coordinator"
)

func faultURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("PALAI_FAULT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_FAULT_POSTGRES_URL is required; run make test-fault CASE=scheduler")
	}
	return url
}

func randID(prefix string) string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return prefix + "_" + hex.EncodeToString(raw[:])
}

// fakeClock is the injected clock the ticker reads; the test advances it to model the outage window
// passing WITHOUT waiting on real wall-clock minutes.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

// harness stands up the durable spine + a schedule store wired to fire through the real trigger-delivery
// pipeline, and seeds a scope + a published-revision cron trigger.
type harness struct {
	pool      *pgxpool.Pool
	store     *automation.ScheduleStore
	org, proj string
	principal string
	triggerID string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	spine, err := coordinator.Open(ctx, faultURL(t))
	if err != nil {
		t.Fatalf("coordinator.Open() error = %v", err)
	}
	t.Cleanup(spine.Close)
	if err := spine.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	pool := spine.Pool()
	org, proj, principal := randID("org"), randID("prj"), randID("prin")
	exec := func(sql string, args ...any) {
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed exec %q error = %v", sql, err)
		}
	}
	exec(`INSERT INTO organizations (id) VALUES ($1)`, org)
	exec(`INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, proj, org)
	exec(`INSERT INTO principals (id, organization_id, project_id, kind) VALUES ($1, $2, $3, 'service')`, principal, org, proj)

	// A published AgentRevision + a type='cron' trigger pinned to it (the run target a firing admits).
	agents := automation.New(pool)
	profileID, err := agents.CreateProfile(ctx, org, proj, randID("profile"))
	if err != nil {
		t.Fatalf("CreateProfile error = %v", err)
	}
	rev, err := agents.CreateRevision(ctx, org, proj, profileID, []byte(`{"model":"gpt-4o-mini","instructions":"scheduled work"}`))
	if err != nil {
		t.Fatalf("CreateRevision error = %v", err)
	}
	if _, _, err := agents.PublishRevision(ctx, org, proj, rev.ID); err != nil {
		t.Fatalf("PublishRevision error = %v", err)
	}
	triggers := automation.NewTriggerStore(pool).WithAdmitter(spine)
	triggerID, err := triggers.CreateTrigger(ctx, org, proj, randID("cron-trg"), "cron")
	if err != nil {
		t.Fatalf("CreateTrigger error = %v", err)
	}
	if _, err := triggers.ReviseTrigger(ctx, org, proj, triggerID, automation.TriggerRevisionInput{
		AgentRevisionID: rev.ID,
		InputMapping:    []byte(`{"fields":{"input":{"const":"scheduled work"}}}`),
	}); err != nil {
		t.Fatalf("ReviseTrigger error = %v", err)
	}
	return &harness{pool: pool, store: automation.NewScheduleStore(pool, triggers), org: org, proj: proj, principal: principal, triggerID: triggerID}
}

// seedSchedule inserts a per-minute cron schedule already due at `nextFireAt`.
func (h *harness) seedSchedule(t *testing.T, nextFireAt time.Time, policy string, maxCatchUp int) string {
	t.Helper()
	id := randID("sch")
	if _, err := h.pool.Exec(context.Background(),
		`INSERT INTO schedules (id, organization_id, project_id, name, trigger_id, created_by, kind, cron_expr,
		 timezone, misfire_policy, misfire_grace_seconds, max_catch_up, jitter_seconds, next_fire_at)
		 VALUES ($1,$2,$3,$4,$5,$6,'cron','* * * * *','UTC',$7,60,$8,0,$9)`,
		id, h.org, h.proj, randID("name"), h.triggerID, h.principal, policy, maxCatchUp, nextFireAt.UTC()); err != nil {
		t.Fatalf("seed schedule error = %v", err)
	}
	return id
}

// runLoopUntil starts the REAL ticker loop and blocks until pred() holds or a deadline elapses, then
// cancels and waits for the loop to FULLY stop (the outage — a clean context-cancel stop, not a sleep).
func runLoopUntil(t *testing.T, store *automation.ScheduleStore, clock *fakeClock, pred func() bool) {
	t.Helper()
	ticker := automation.NewScheduleTicker(store, 20*time.Millisecond, 100, nil).WithClock(clock.now)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = ticker.Run(ctx); close(done) }()
	deadline := time.Now().Add(10 * time.Second)
	for !pred() {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("ticker never reached the expected state within the deadline")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done // the REAL loop has fully drained — the outage begins
}

// firingOccurrences returns the schedule's firing (pending|admitted) occurrence planned_at instants,
// oldest-first, plus the count of skipped-window rows and the distinct-occurrence-id invariant.
func (h *harness) occurrenceState(t *testing.T, scheduleID string) (firing []time.Time, skipRows, distinctIDs, totalRows int) {
	t.Helper()
	rows, err := h.pool.Query(context.Background(),
		`SELECT planned_at, state FROM schedule_occurrences WHERE schedule_id=$1 ORDER BY planned_at`, scheduleID)
	if err != nil {
		t.Fatalf("read occurrences error = %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var planned time.Time
		var state string
		if err := rows.Scan(&planned, &state); err != nil {
			t.Fatalf("scan occurrence error = %v", err)
		}
		totalRows++
		switch state {
		case "skipped":
			skipRows++
		default:
			firing = append(firing, planned)
		}
	}
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(DISTINCT occurrence_id) FROM schedule_occurrences WHERE schedule_id=$1`, scheduleID).Scan(&distinctIDs); err != nil {
		t.Fatalf("count distinct occurrence ids error = %v", err)
	}
	return firing, skipRows, distinctIDs, totalRows
}

func (h *harness) admitted(t *testing.T, scheduleID string, planned time.Time) bool {
	t.Helper()
	var n int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM schedule_occurrences WHERE schedule_id=$1 AND planned_at=$2 AND state='admitted'`,
		scheduleID, planned.UTC()).Scan(&n); err != nil {
		t.Fatalf("count admitted error = %v", err)
	}
	return n == 1
}

// TestSchedulerOutageFireOnceNowLatestMissed proves AUT-008 fire_once_now recovery: the base instant fires,
// the loop is stopped across a 5-minute missed window (clock jump), and on restart exactly ONE occurrence
// for the LATEST missed instant fires — earlier misses collapse to ONE skip row, occurrence ids stay
// unique, and the recovered occurrence's admitted_at is later than its planned_at (lateness visible).
func TestSchedulerOutageFireOnceNowLatestMissed(t *testing.T) {
	h := newHarness(t)
	base := time.Now().UTC().Truncate(time.Minute).Add(-10 * time.Minute)
	schID := h.seedSchedule(t, base, "fire_once_now", 0)
	clock := &fakeClock{t: base.Add(10 * time.Second)}

	// The loop runs, fires the base instant, then is STOPPED (the outage).
	runLoopUntil(t, h.store, clock, func() bool { return h.admitted(t, schID, base) })

	// The outage window passes: 5 per-minute instants are missed (base+1 .. base+5).
	latest := base.Add(5 * time.Minute)
	clock.set(latest.Add(10 * time.Second))

	// Restart the REAL loop: fire_once_now recovers the LATEST missed instant, once.
	runLoopUntil(t, h.store, clock, func() bool { return h.admitted(t, schID, latest) })

	firing, skipRows, distinctIDs, totalRows := h.occurrenceState(t, schID)
	if len(firing) != 2 || !firing[0].Equal(base) || !firing[1].Equal(latest) {
		t.Fatalf("firing occurrences = %v, want exactly [base, base+5m] (fire_once_now: base then the latest missed)", firing)
	}
	if skipRows != 1 {
		t.Fatalf("skip-window rows = %d, want exactly 1 (the earlier misses collapse to ONE row, not per-instant)", skipRows)
	}
	if distinctIDs != totalRows {
		t.Fatalf("distinct occurrence ids = %d but total rows = %d — a duplicate occurrence id leaked", distinctIDs, totalRows)
	}
	// Lateness visible: the recovered occurrence admitted well after its planned instant.
	var planned, admitted time.Time
	if err := h.pool.QueryRow(context.Background(),
		`SELECT planned_at, admitted_at FROM schedule_occurrences WHERE schedule_id=$1 AND planned_at=$2`,
		schID, latest.UTC()).Scan(&planned, &admitted); err != nil {
		t.Fatalf("read recovered occurrence error = %v", err)
	}
	if !admitted.After(planned) {
		t.Fatalf("recovered occurrence admitted_at=%s not after planned_at=%s — lateness is not visible", admitted, planned)
	}
}

// TestSchedulerOutageCatchUpBoundedOldestFirst proves the catch_up variant: across the same outage, the
// restart materializes the OLDEST missed instants up to max_catch_up (the cap is uncrossable), windows the
// remainder, and never duplicates an occurrence id.
func TestSchedulerOutageCatchUpBoundedOldestFirst(t *testing.T) {
	h := newHarness(t)
	base := time.Now().UTC().Truncate(time.Minute).Add(-10 * time.Minute)
	schID := h.seedSchedule(t, base, "catch_up", 2)
	clock := &fakeClock{t: base.Add(10 * time.Second)}

	runLoopUntil(t, h.store, clock, func() bool { return h.admitted(t, schID, base) })

	// 5 instants missed (base+1 .. base+5); catch_up cap is 2.
	clock.set(base.Add(5 * time.Minute).Add(10 * time.Second))
	runLoopUntil(t, h.store, clock, func() bool { return h.admitted(t, schID, base.Add(2*time.Minute)) })

	firing, skipRows, distinctIDs, totalRows := h.occurrenceState(t, schID)
	// base (ticker1) + the two OLDEST missed (base+1, base+2) — the cap is 2.
	want := []time.Time{base, base.Add(time.Minute), base.Add(2 * time.Minute)}
	if len(firing) != len(want) {
		t.Fatalf("catch_up firing occurrences = %v, want %v (base + 2 oldest, cap uncrossable)", firing, want)
	}
	for i := range want {
		if !firing[i].Equal(want[i]) {
			t.Fatalf("catch_up firing[%d] = %s, want %s (oldest-first, bounded)", i, firing[i], want[i])
		}
	}
	if skipRows != 1 {
		t.Fatalf("catch_up skip-window rows = %d, want 1 (the remainder is ONE window row)", skipRows)
	}
	if distinctIDs != totalRows {
		t.Fatalf("distinct occurrence ids = %d but total rows = %d — a duplicate occurrence id leaked", distinctIDs, totalRows)
	}
}
