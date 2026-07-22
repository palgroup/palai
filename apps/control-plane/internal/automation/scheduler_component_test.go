//go:build component

// Real-PostgreSQL component tests for the schedule ticker (spec §33, E11 Task 3). They run under
// `make test-component TEST=postgres`, which starts a throwaway container and exports
// PALAI_COMPONENT_POSTGRES_URL. The DST/misfire/jitter LOGIC is proven as pure units (scheduler_plan_test,
// cron_next_test); these prove the DURABLE effects — durable-before-run, 2-replica single-occurrence,
// misfire materialization, jitter gating, pause/delete — against a real spine + admission pipeline.
package automation

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/storage"
)

// wiredScheduleStore returns a schedule store wired to a real trigger store + coordinator admitter over the
// throwaway PG, plus the shared pool.
func wiredScheduleStore(t *testing.T) (*ScheduleStore, *TriggerStore, *pgxpool.Pool) {
	t.Helper()
	ts, pool := wiredTriggerStore(t)
	return NewScheduleStore(pool, ts), ts, pool
}

// seedCronTrigger creates a type='cron' trigger pinned to a freshly published AgentRevision, with a const
// input mapping — everything a scheduled firing needs to admit a run.
func seedCronTrigger(t *testing.T, ts *TriggerStore, pool *pgxpool.Pool, org, project string) string {
	t.Helper()
	ctx := context.Background()
	triggerID, err := ts.CreateTrigger(ctx, org, project, "", randID("cron-trg"), "cron")
	if err != nil {
		t.Fatalf("CreateTrigger error = %v", err)
	}
	revID := seedPublishedAgentRevision(t, pool, org, project)
	if _, err := ts.ReviseTrigger(ctx, org, project, triggerID, TriggerRevisionInput{
		AgentRevisionID: revID,
		InputMapping:    []byte(`{"fields":{"input":{"const":"scheduled work"}}}`),
	}); err != nil {
		t.Fatalf("ReviseTrigger error = %v", err)
	}
	return triggerID
}

// seedScheduleRow inserts a schedule row with full control of next_fire_at / policy / jitter — the ticker
// tests need a schedule already DUE (a past next_fire_at) which CreateSchedule never produces.
func seedScheduleRow(t *testing.T, pool *pgxpool.Pool, org, project, triggerID, principal, cronExpr, tz string, nextFireAt time.Time, policy string, maxCatchUp, jitterSeconds int) string {
	t.Helper()
	id := randID("sch")
	mustExec(t, pool,
		`INSERT INTO schedules (id, organization_id, project_id, name, trigger_id, created_by, kind, cron_expr,
		 timezone, misfire_policy, misfire_grace_seconds, max_catch_up, jitter_seconds, next_fire_at)
		 VALUES ($1,$2,$3,$4,$5,$6,'cron',$7,$8,$9,60,$10,$11,$12)`,
		id, org, project, randID("name"), triggerID, principal, cronExpr, tz, policy, maxCatchUp, jitterSeconds, nextFireAt.UTC())
	return id
}

// occRow is a minimal projection of a schedule_occurrences row for assertions.
type occRow struct {
	id, state, deliveryID, reason string
	admitted                      bool
	plannedAt                     time.Time
}

func occurrencesOf(t *testing.T, pool *pgxpool.Pool, scheduleID string) []occRow {
	t.Helper()
	rows, err := pool.Query(storage.WithSystemScope(context.Background()),
		`SELECT occurrence_id, state, delivery_id, reason, admitted_at IS NOT NULL, planned_at
		 FROM schedule_occurrences WHERE schedule_id=$1 ORDER BY planned_at`, scheduleID)
	if err != nil {
		t.Fatalf("read occurrences error = %v", err)
	}
	defer rows.Close()
	var out []occRow
	for rows.Next() {
		var o occRow
		if err := rows.Scan(&o.id, &o.state, &o.deliveryID, &o.reason, &o.admitted, &o.plannedAt); err != nil {
			t.Fatalf("scan occurrence error = %v", err)
		}
		out = append(out, o)
	}
	return out
}

func countRunCreated(t *testing.T, pool *pgxpool.Pool, triggerID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(*) FROM trigger_deliveries WHERE trigger_id=$1 AND state='run_created'`, triggerID).Scan(&n); err != nil {
		t.Fatalf("count run_created error = %v", err)
	}
	return n
}

// TestOccurrenceDurableBeforeRunCreated proves the §5 durable-before-run + idempotent-handoff invariant:
// the fire phase commits the occurrence 'pending' BEFORE any delivery/run exists; a later sweep hands it
// off exactly once (dedupe_key = occurrence_id); and a re-handoff after a simulated crash (occurrence reset
// to pending while its delivery already exists) is deduped to the SAME run, never a second.
func TestOccurrenceDurableBeforeRunCreated(t *testing.T) {
	ss, ts, pool := wiredScheduleStore(t)
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)
	principal := seedPrincipal(t, pool, org, project)
	triggerID := seedCronTrigger(t, ts, pool, org, project)

	planned := time.Date(2026, 7, 22, 6, 0, 0, 0, time.UTC)
	now := planned.Add(30 * time.Second) // within grace → a normal single fire
	schID := seedScheduleRow(t, pool, org, project, triggerID, principal, "* * * * *", "UTC", planned, "fire_once_now", 0, 0)

	// Phase 1 — fire: the occurrence is durably 'pending' BEFORE any delivery/run.
	if err := ss.fireDueSchedules(ctx, now, 100, t.Logf); err != nil {
		t.Fatalf("fireDueSchedules error = %v", err)
	}
	occs := occurrencesOf(t, pool, schID)
	if len(occs) != 1 || occs[0].state != "pending" || occs[0].admitted || occs[0].deliveryID != "" {
		t.Fatalf("after fire, occurrences = %+v, want one durable 'pending' with no delivery", occs)
	}
	if got := countRunCreated(t, pool, triggerID); got != 0 {
		t.Fatalf("after fire, run_created deliveries = %d, want 0 (occurrence durable BEFORE any run)", got)
	}
	occID := occs[0].id

	// Phase 2 — sweep (the crash-recovery handoff): the pending occurrence admits exactly once.
	if err := ss.sweepPendingOccurrences(ctx, now, 100, t.Logf); err != nil {
		t.Fatalf("sweepPendingOccurrences error = %v", err)
	}
	occs = occurrencesOf(t, pool, schID)
	if len(occs) != 1 || occs[0].state != "admitted" || !occs[0].admitted || occs[0].deliveryID == "" {
		t.Fatalf("after sweep, occurrence = %+v, want 'admitted' with a delivery + admitted_at", occs[0])
	}
	var dedupeKey string
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT dedupe_key FROM trigger_deliveries WHERE id=$1`, occs[0].deliveryID).Scan(&dedupeKey); err != nil {
		t.Fatalf("read delivery dedupe_key error = %v", err)
	}
	if dedupeKey != occID {
		t.Fatalf("delivery dedupe_key = %q, want the occurrence_id %q (the third defense line)", dedupeKey, occID)
	}
	if got := countRunCreated(t, pool, triggerID); got != 1 {
		t.Fatalf("after sweep, run_created deliveries = %d, want exactly 1", got)
	}

	// Phase 3 — a crash between delivery-commit and MarkOccurrenceAdmitted leaves the occurrence 'pending'
	// with its delivery already born; the next sweep re-handoffs and the occurrence_id dedupe collapses it
	// to the SAME run (no second run_created).
	mustExec(t, pool, `UPDATE schedule_occurrences SET state='pending', admitted_at=NULL WHERE occurrence_id=$1`, occID)
	if err := ss.sweepPendingOccurrences(ctx, now, 100, t.Logf); err != nil {
		t.Fatalf("re-sweep error = %v", err)
	}
	if got := countRunCreated(t, pool, triggerID); got != 1 {
		t.Fatalf("after re-handoff, run_created deliveries = %d, want exactly 1 (occurrence_id dedupe collapses the double handoff)", got)
	}
	occs = occurrencesOf(t, pool, schID)
	if occs[0].state != "admitted" {
		t.Fatalf("after re-handoff, occurrence state = %q, want admitted", occs[0].state)
	}
}

// TestOneTimeScheduleFiresAndExhausts proves B11: a one_time schedule fires its single occurrence when due
// and then exhausts (next_fire_at NULL) — it never fires again.
func TestOneTimeScheduleFiresAndExhausts(t *testing.T) {
	ss, ts, pool := wiredScheduleStore(t)
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)
	principal := seedPrincipal(t, pool, org, project)
	triggerID := seedCronTrigger(t, ts, pool, org, project)

	fireAt := time.Now().UTC().Truncate(time.Minute).Add(-2 * time.Minute)
	now := fireAt.Add(30 * time.Second) // within grace → a normal single fire
	id := randID("sch")
	mustExec(t, pool,
		`INSERT INTO schedules (id, organization_id, project_id, name, trigger_id, created_by, kind, timezone,
		 misfire_policy, misfire_grace_seconds, one_time_at, next_fire_at)
		 VALUES ($1,$2,$3,$4,$5,$6,'one_time','UTC','fire_once_now',300,$7,$7)`,
		id, org, project, randID("name"), triggerID, principal, fireAt.UTC())

	if err := ss.fireDueSchedules(ctx, now, 100, t.Logf); err != nil {
		t.Fatalf("fireDueSchedules error = %v", err)
	}
	occs := occurrencesOf(t, pool, id)
	if len(occs) != 1 || occs[0].state != "pending" || !occs[0].plannedAt.Equal(fireAt) {
		t.Fatalf("one_time occurrences = %+v, want one pending at %s", occs, fireAt)
	}
	// The schedule is exhausted: next_fire_at is NULL (it never fires again).
	var nextFire *time.Time
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT next_fire_at FROM schedules WHERE id=$1`, id).Scan(&nextFire); err != nil {
		t.Fatalf("read next_fire_at error = %v", err)
	}
	if nextFire != nil {
		t.Fatalf("one_time next_fire_at = %s, want NULL (exhausted after its single fire)", nextFire)
	}
	// It hands off and never re-fires on a later tick.
	if err := ss.sweepPendingOccurrences(ctx, now, 100, t.Logf); err != nil {
		t.Fatalf("sweep error = %v", err)
	}
	if err := ss.fireDueSchedules(ctx, now.Add(time.Hour), 100, t.Logf); err != nil {
		t.Fatalf("later fireDueSchedules error = %v", err)
	}
	if occs := occurrencesOf(t, pool, id); len(occs) != 1 {
		t.Fatalf("one_time re-fired: %d occurrences, want the unchanged 1", len(occs))
	}
	if occs := occurrencesOf(t, pool, id); occs[0].state != "admitted" {
		t.Fatalf("one_time occurrence state = %q, want admitted (handed off)", occs[0].state)
	}
}

// TestResumeAfterFailRecomputesNextFire proves the policy=fail resume path (review #4, AUT-008): a failed
// schedule's next_fire_at is stuck at the stale missed instant, so a bare status flip to 'active' would let
// the next tick see the same backlog and re-fail (a deadlock — "admission stops until operator resume"
// never holds). Resume must recompute next_fire_at from NOW, so admission resumes cleanly.
func TestResumeAfterFailRecomputesNextFire(t *testing.T) {
	ss, ts, pool := wiredScheduleStore(t)
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)
	principal := seedPrincipal(t, pool, org, project)
	triggerID := seedCronTrigger(t, ts, pool, org, project)

	base := time.Now().UTC().Truncate(time.Minute).Add(-10 * time.Minute)
	now := base.Add(5 * time.Minute) // several instants missed → policy=fail freezes
	schID := seedScheduleRow(t, pool, org, project, triggerID, principal, "* * * * *", "UTC", base, "fail", 0, 0)

	if err := ss.fireDueSchedules(ctx, now, 100, t.Logf); err != nil {
		t.Fatalf("fireDueSchedules error = %v", err)
	}
	var status string
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT status FROM schedules WHERE id=$1`, schID).Scan(&status); err != nil {
		t.Fatalf("read status error = %v", err)
	}
	if status != "failed" {
		t.Fatalf("policy=fail schedule status = %q, want failed", status)
	}

	// Resume: next_fire_at recomputed from now (future), status active, reason cleared.
	if ok, err := ss.SetPaused(ctx, org, project, schID, false); err != nil || !ok {
		t.Fatalf("resume SetPaused = (%v, %v), want (true, nil)", ok, err)
	}
	var next time.Time
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT status, next_fire_at FROM schedules WHERE id=$1`, schID).Scan(&status, &next); err != nil {
		t.Fatalf("read after resume error = %v", err)
	}
	if status != "active" {
		t.Fatalf("after resume status = %q, want active", status)
	}
	if !next.After(now) {
		t.Fatalf("after resume next_fire_at = %s, want a future instant (recomputed from now), not the stale missed instant", next)
	}

	// A subsequent tick must NOT immediately re-fail — admission has resumed cleanly.
	if err := ss.fireDueSchedules(ctx, now, 100, t.Logf); err != nil {
		t.Fatalf("post-resume fireDueSchedules error = %v", err)
	}
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT status FROM schedules WHERE id=$1`, schID).Scan(&status); err != nil {
		t.Fatalf("read status error = %v", err)
	}
	if status != "active" {
		t.Fatalf("after resume + tick status = %q, want active (the fail did not recur)", status)
	}
}

// TestTwoSchedulerReplicasSingleCanonicalOccurrence proves AUT-007: two ticker replicas racing one PG on
// one due instant yield exactly ONE occurrence row and ONE run — correctness pinned to the occurrence
// UNIQUE index (ON CONFLICT DO NOTHING collapses the concurrent inserts), NOT to any row lock.
func TestTwoSchedulerReplicasSingleCanonicalOccurrence(t *testing.T) {
	ss, ts, pool := wiredScheduleStore(t)
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)
	principal := seedPrincipal(t, pool, org, project)
	triggerID := seedCronTrigger(t, ts, pool, org, project)

	planned := time.Date(2026, 7, 22, 7, 0, 0, 0, time.UTC)
	now := planned.Add(10 * time.Second)
	schID := seedScheduleRow(t, pool, org, project, triggerID, principal, "* * * * *", "UTC", planned, "fire_once_now", 0, 0)

	// Two replicas share one clock so they compute the identical plan and genuinely race the claim.
	fixed := func() time.Time { return now }
	r1 := NewScheduleTicker(ss, time.Second, 100, t.Logf).WithClock(fixed)
	r2 := NewScheduleTicker(ss, time.Second, 100, t.Logf).WithClock(fixed)
	var wg sync.WaitGroup
	wg.Add(2)
	for _, r := range []*ScheduleTicker{r1, r2} {
		go func(tk *ScheduleTicker) { defer wg.Done(); _ = tk.Tick(ctx) }(r)
	}
	wg.Wait()

	occs := occurrencesOf(t, pool, schID)
	if len(occs) != 1 {
		t.Fatalf("two replicas produced %d occurrence rows, want exactly 1 (unique index collapses the race)", len(occs))
	}
	if got := countRunCreated(t, pool, triggerID); got != 1 {
		t.Fatalf("two replicas produced %d runs, want exactly 1 (occurrence_id dedupe)", got)
	}
	// next_fire_at advanced exactly once (to the next minute), not twice.
	var next time.Time
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT next_fire_at FROM schedules WHERE id=$1`, schID).Scan(&next); err != nil {
		t.Fatalf("read next_fire_at error = %v", err)
	}
	if !next.Equal(planned.Add(time.Minute)) {
		t.Fatalf("next_fire_at = %s, want %s (advanced once)", next, planned.Add(time.Minute))
	}
}

// TestMisfirePoliciesMaterialized proves the misfire plan materializes as the RIGHT durable rows (§33.3,
// B7): fire_once_now fires the most-recent missed instant + ONE windowed-skip row for the earlier misses;
// skip fires nothing + ONE windowed-skip row; fail freezes the schedule. The window is ONE row, not one
// per missed instant (bounded storage).
func TestMisfirePoliciesMaterialized(t *testing.T) {
	ss, ts, pool := wiredScheduleStore(t)
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)
	principal := seedPrincipal(t, pool, org, project)

	base := time.Date(2026, 7, 22, 8, 0, 0, 0, time.UTC)
	now := base.Add(10 * time.Minute) // 11 per-minute instants missed (08:00..08:10)

	// fire_once_now: one firing occurrence (08:10) + ONE windowed-skip row (08:00..08:09).
	{
		triggerID := seedCronTrigger(t, ts, pool, org, project)
		schID := seedScheduleRow(t, pool, org, project, triggerID, principal, "* * * * *", "UTC", base, "fire_once_now", 0, 0)
		if err := ss.fireDueSchedules(ctx, now, 100, t.Logf); err != nil {
			t.Fatalf("fireDueSchedules error = %v", err)
		}
		occs := occurrencesOf(t, pool, schID)
		var fired, skipped int
		for _, o := range occs {
			switch o.state {
			case "pending":
				fired++
				if !o.plannedAt.Equal(now) {
					t.Fatalf("fire_once_now fired at %s, want the most-recent missed %s", o.plannedAt, now)
				}
			case "skipped":
				skipped++
			}
		}
		if fired != 1 || skipped != 1 {
			t.Fatalf("fire_once_now = %d fired / %d skipped rows, want 1 / 1 (ONE windowed skip, not per-instant)", fired, skipped)
		}
	}

	// skip: zero firing occurrences + ONE windowed-skip row; next_fire_at advances future.
	{
		triggerID := seedCronTrigger(t, ts, pool, org, project)
		schID := seedScheduleRow(t, pool, org, project, triggerID, principal, "* * * * *", "UTC", base, "skip", 0, 0)
		if err := ss.fireDueSchedules(ctx, now, 100, t.Logf); err != nil {
			t.Fatalf("fireDueSchedules error = %v", err)
		}
		occs := occurrencesOf(t, pool, schID)
		if len(occs) != 1 || occs[0].state != "skipped" {
			t.Fatalf("skip = %+v, want exactly one 'skipped' window row", occs)
		}
		var next time.Time
		if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT next_fire_at FROM schedules WHERE id=$1`, schID).Scan(&next); err != nil {
			t.Fatalf("read next_fire_at error = %v", err)
		}
		if !next.After(now) {
			t.Fatalf("skip next_fire_at = %s, want a future instant", next)
		}
	}

	// fail: nothing fires, the schedule is frozen 'failed' with a reason.
	{
		triggerID := seedCronTrigger(t, ts, pool, org, project)
		schID := seedScheduleRow(t, pool, org, project, triggerID, principal, "* * * * *", "UTC", base, "fail", 0, 0)
		if err := ss.fireDueSchedules(ctx, now, 100, t.Logf); err != nil {
			t.Fatalf("fireDueSchedules error = %v", err)
		}
		if occs := occurrencesOf(t, pool, schID); len(occs) != 0 {
			t.Fatalf("fail materialized %d occurrences, want 0", len(occs))
		}
		var status, reason string
		if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT status, status_reason FROM schedules WHERE id=$1`, schID).Scan(&status, &reason); err != nil {
			t.Fatalf("read schedule status error = %v", err)
		}
		if status != "failed" || reason == "" {
			t.Fatalf("fail schedule = status:%q reason:%q, want 'failed' + a reason", status, reason)
		}
	}
}

// TestCatchUpBoundedOldestFirst proves catch_up materializes oldest-first up to max_catch_up firing
// occurrences and windows the remainder — the cap is uncrossable (§33.3).
func TestCatchUpBoundedOldestFirst(t *testing.T) {
	ss, ts, pool := wiredScheduleStore(t)
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)
	principal := seedPrincipal(t, pool, org, project)
	triggerID := seedCronTrigger(t, ts, pool, org, project)

	base := time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC)
	now := base.Add(10 * time.Minute) // 11 missed instants
	schID := seedScheduleRow(t, pool, org, project, triggerID, principal, "* * * * *", "UTC", base, "catch_up", 3, 0)

	if err := ss.fireDueSchedules(ctx, now, 100, t.Logf); err != nil {
		t.Fatalf("fireDueSchedules error = %v", err)
	}
	occs := occurrencesOf(t, pool, schID)
	var fired []time.Time
	skipped := 0
	for _, o := range occs {
		switch o.state {
		case "pending":
			fired = append(fired, o.plannedAt)
		case "skipped":
			skipped++
		}
	}
	if len(fired) != 3 {
		t.Fatalf("catch_up fired %d occurrences, want the max_catch_up cap of 3", len(fired))
	}
	// Oldest-first: the three earliest missed instants (08:00 base .. base+2m equivalents at 09:00).
	for i := 0; i < 3; i++ {
		if !fired[i].Equal(base.Add(time.Duration(i) * time.Minute)) {
			t.Fatalf("catch_up fired[%d] = %s, want %s (oldest-first)", i, fired[i], base.Add(time.Duration(i)*time.Minute))
		}
	}
	if skipped != 1 {
		t.Fatalf("catch_up left %d skip rows, want 1 windowed remainder", skipped)
	}
}

// TestJitterGatesAdmission proves the deterministic bounded jitter (§33.5, B8): a firing occurrence is not
// admitted until now reaches planned_at + jitter, and admitted_at (> planned_at) makes the offset visible.
func TestJitterGatesAdmission(t *testing.T) {
	ss, ts, pool := wiredScheduleStore(t)
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)
	principal := seedPrincipal(t, pool, org, project)
	triggerID := seedCronTrigger(t, ts, pool, org, project)

	// A real-past minute-aligned instant: the tick's `now` is injected (drives the jitter GATE), but
	// admitted_at is stamped by the DB clock (real time), so planned must sit in the real past for the
	// planned_at-vs-admitted_at lateness assertion to hold.
	planned := time.Now().UTC().Truncate(time.Minute).Add(-3 * time.Minute)
	const jitter = 120
	schID := seedScheduleRow(t, pool, org, project, triggerID, principal, "* * * * *", "UTC", planned, "fire_once_now", 0, jitter)

	// Claim the occurrence at a `now` within grace so exactly one fires.
	if err := ss.fireDueSchedules(ctx, planned.Add(30*time.Second), 100, t.Logf); err != nil {
		t.Fatalf("fireDueSchedules error = %v", err)
	}
	occs := occurrencesOf(t, pool, schID)
	if len(occs) != 1 {
		t.Fatalf("jitter fire produced %d occurrences, want 1", len(occs))
	}
	occID := occs[0].id
	off := jitterOffset(occID, jitter)
	if off <= 0 {
		t.Skip("this occurrence id hashed to a zero jitter offset; the gate is a no-op to assert")
	}

	// A sweep BEFORE planned_at + jitter must NOT admit (the gate is closed).
	if err := ss.sweepPendingOccurrences(ctx, planned.Add(off/2), 100, t.Logf); err != nil {
		t.Fatalf("early sweep error = %v", err)
	}
	if o := occurrencesOf(t, pool, schID)[0]; o.state != "pending" {
		t.Fatalf("occurrence admitted before the jitter window (state=%q); admission must wait for planned_at+jitter", o.state)
	}

	// A sweep AT/AFTER planned_at + jitter admits, and admitted_at is later than planned_at (visible offset).
	admitNow := planned.Add(off).Add(time.Second)
	if err := ss.sweepPendingOccurrences(ctx, admitNow, 100, t.Logf); err != nil {
		t.Fatalf("in-window sweep error = %v", err)
	}
	var state string
	var admittedAt time.Time
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT state, admitted_at FROM schedule_occurrences WHERE occurrence_id=$1`, occID).Scan(&state, &admittedAt); err != nil {
		t.Fatalf("read occurrence error = %v", err)
	}
	if state != "admitted" || !admittedAt.After(planned) {
		t.Fatalf("after the jitter window: state=%q admitted_at=%s, want admitted with admitted_at after planned_at %s", state, admittedAt, planned)
	}
}

// TestPauseAndDeleteStopAdmission proves B9: a paused schedule admits no new occurrence (the in-flight
// occurrence is untouched — no cancel), and a soft-deleted schedule admits nothing while its occurrence
// rows stay queryable under retention.
func TestPauseAndDeleteStopAdmission(t *testing.T) {
	ss, ts, pool := wiredScheduleStore(t)
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)
	principal := seedPrincipal(t, pool, org, project)
	triggerID := seedCronTrigger(t, ts, pool, org, project)

	planned := time.Date(2026, 7, 22, 11, 0, 0, 0, time.UTC)
	now := planned.Add(30 * time.Second)

	// Paused: the due-scan skips it → zero occurrences.
	paused := seedScheduleRow(t, pool, org, project, triggerID, principal, "* * * * *", "UTC", planned, "fire_once_now", 0, 0)
	if ok, err := ss.SetPaused(ctx, org, project, paused, true); err != nil || !ok {
		t.Fatalf("SetPaused = (%v, %v), want (true, nil)", ok, err)
	}
	if err := ss.fireDueSchedules(ctx, now, 100, t.Logf); err != nil {
		t.Fatalf("fireDueSchedules error = %v", err)
	}
	if occs := occurrencesOf(t, pool, paused); len(occs) != 0 {
		t.Fatalf("paused schedule fired %d occurrences, want 0", len(occs))
	}

	// Deleted (soft): a fired-then-deleted schedule keeps its occurrence rows queryable, but admits no new one.
	deleted := seedScheduleRow(t, pool, org, project, triggerID, principal, "* * * * *", "UTC", planned, "fire_once_now", 0, 0)
	if err := ss.fireDueSchedules(ctx, now, 100, t.Logf); err != nil {
		t.Fatalf("fireDueSchedules (pre-delete) error = %v", err)
	}
	before := occurrencesOf(t, pool, deleted)
	if len(before) != 1 {
		t.Fatalf("pre-delete occurrences = %d, want 1", len(before))
	}
	if ok, err := ss.DeleteSchedule(ctx, org, project, deleted); err != nil || !ok {
		t.Fatalf("DeleteSchedule = (%v, %v), want (true, nil)", ok, err)
	}
	// GetSchedule hides a soft-deleted schedule, but its occurrences persist (retention).
	if _, found, err := ss.GetSchedule(ctx, org, project, deleted); err != nil || found {
		t.Fatalf("GetSchedule after delete = (found:%v, %v), want (false, nil)", found, err)
	}
	if occs, err := ss.ListOccurrences(ctx, org, project, deleted, 100); err != nil || len(occs) != 1 {
		t.Fatalf("ListOccurrences after delete = (%d, %v), want the 1 preserved row", len(occs), err)
	}
	// Advance the clock a minute and re-fire: the deleted schedule admits no new occurrence.
	if err := ss.fireDueSchedules(ctx, now.Add(2*time.Minute), 100, t.Logf); err != nil {
		t.Fatalf("fireDueSchedules (post-delete) error = %v", err)
	}
	if occs := occurrencesOf(t, pool, deleted); len(occs) != 1 {
		t.Fatalf("deleted schedule occurrences = %d, want the unchanged 1 (no new firing)", len(occs))
	}
}
