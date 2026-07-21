package automation

import (
	"errors"
	"testing"
	"time"
)

// TestValidateFiringRejectsOutOfRange proves the app-side range validation (m-api): max_catch_up>100,
// jitter>3600, and negatives are a typed ErrScheduleInvalid (→ 400), not a DB-CHECK 500.
func TestValidateFiringRejectsOutOfRange(t *testing.T) {
	base := ScheduleInput{Kind: "cron", CronExpr: "* * * * *", Timezone: "UTC"}
	bad := []ScheduleInput{
		func() ScheduleInput { i := base; i.MaxCatchUp = 101; return i }(),
		func() ScheduleInput { i := base; i.MaxCatchUp = -1; return i }(),
		func() ScheduleInput { i := base; i.JitterSeconds = 3601; return i }(),
		func() ScheduleInput { i := base; i.JitterSeconds = -1; return i }(),
		func() ScheduleInput { i := base; i.MisfireGraceSeconds = -1; return i }(),
	}
	for _, in := range bad {
		if _, err := validateFiring(in); !errors.Is(err, ErrScheduleInvalid) {
			t.Errorf("validateFiring(%+v) err = %v, want ErrScheduleInvalid", in, err)
		}
	}
	// A well-formed config still validates.
	if _, err := validateFiring(func() ScheduleInput { i := base; i.MaxCatchUp = 100; i.JitterSeconds = 3600; return i }()); err != nil {
		t.Fatalf("validateFiring on in-range knobs err = %v, want nil", err)
	}
}

// cronSpec builds a per-minute cron scheduleSpec in UTC for the pure planning tests.
func cronSpec(policy string, grace time.Duration, maxCatchUp int) scheduleSpec {
	c, err := ParseCron("* * * * *")
	if err != nil {
		panic(err)
	}
	return scheduleSpec{
		kind: "cron", cron: c, loc: time.UTC,
		misfirePolicy: policy, grace: grace, maxCatchUp: maxCatchUp,
	}
}

// TestPlanMisfirePoliciesBounded proves the four misfire policies as PURE, deterministic plans (§33.3,
// brief B7): skip windows every missed instant into one record; fire_once_now fires the MOST RECENT and
// windows the earlier ones; catch_up fires oldest-first up to max_catch_up and windows the remainder
// (the cap is uncrossable); fail freezes; and a single missed instant within grace is a NORMAL late fire,
// not a misfire.
func TestPlanMisfirePoliciesBounded(t *testing.T) {
	base := time.Date(2026, 7, 22, 6, 0, 0, 0, time.UTC)
	now := base.Add(10 * time.Minute) // 10 minutes of per-minute instants missed (06:00..06:10)
	grace := 90 * time.Second

	// skip → zero firing occurrences, ONE windowed skip {from, to, count}, next advances future.
	{
		p := planTick(cronSpec("skip", grace, 0), base, now)
		if len(p.fire) != 0 {
			t.Fatalf("skip fired %d occurrences, want 0", len(p.fire))
		}
		if p.skipCount == 0 || p.skipFrom.IsZero() || p.skipTo.IsZero() {
			t.Fatalf("skip window = {from:%s to:%s count:%d}, want a populated window", p.skipFrom, p.skipTo, p.skipCount)
		}
		if !p.hasNext || !p.nextFireAt.After(now) {
			t.Fatalf("skip next_fire_at = %s (hasNext=%v), want a future instant", p.nextFireAt, p.hasNext)
		}
	}

	// fire_once_now → ONE occurrence at the MOST RECENT missed instant; the earlier misses are ONE window.
	{
		p := planTick(cronSpec("fire_once_now", grace, 0), base, now)
		if len(p.fire) != 1 {
			t.Fatalf("fire_once_now fired %d occurrences, want exactly 1", len(p.fire))
		}
		latest := p.fire[0]
		if p.skipCount == 0 || !p.skipTo.Before(latest) {
			t.Fatalf("fire_once_now: earlier misses window = {to:%s count:%d}, want a window ending before the fired %s", p.skipTo, p.skipCount, latest)
		}
	}

	// catch_up → oldest-first up to max_catch_up; the remainder is one window; the cap is uncrossable.
	{
		p := planTick(cronSpec("catch_up", grace, 3), base, now)
		if len(p.fire) != 3 {
			t.Fatalf("catch_up fired %d occurrences, want exactly max_catch_up=3", len(p.fire))
		}
		// Oldest-first: the fired instants are the three earliest missed.
		for i := 1; i < len(p.fire); i++ {
			if !p.fire[i-1].Before(p.fire[i]) {
				t.Fatalf("catch_up fired out of order: %s !< %s", p.fire[i-1], p.fire[i])
			}
		}
		if p.skipCount == 0 {
			t.Fatal("catch_up left a remainder but recorded no skip window")
		}
	}

	// fail → nothing fires, the schedule is frozen with a reason.
	{
		p := planTick(cronSpec("fail", grace, 0), base, now)
		if len(p.fire) != 0 || !p.fail || p.failReason == "" {
			t.Fatalf("fail plan = fire:%d fail:%v reason:%q, want 0 / true / a reason", len(p.fire), p.fail, p.failReason)
		}
	}

	// Within grace → a NORMAL late fire of the single due instant, not a misfire (no skip window).
	{
		onTime := base.Add(30 * time.Second) // the single 06:00 instant, 30s late — within a 90s grace
		p := planTick(cronSpec("skip", grace, 0), base, onTime)
		if len(p.fire) != 1 || p.skipCount != 0 {
			t.Fatalf("within-grace plan = fire:%d skip:%d, want 1 fired / 0 skipped (a normal late fire, not a misfire)", len(p.fire), p.skipCount)
		}
	}
}

// TestPlanNormalSingleFire proves the common case: exactly one instant is due, on time → one occurrence,
// no skip, next advances to the following instant.
func TestPlanNormalSingleFire(t *testing.T) {
	base := time.Date(2026, 7, 22, 6, 0, 0, 0, time.UTC)
	now := base.Add(2 * time.Second) // essentially on time
	p := planTick(cronSpec("fire_once_now", 90*time.Second, 0), base, now)
	if len(p.fire) != 1 || !p.fire[0].Equal(base) {
		t.Fatalf("normal fire = %v, want a single occurrence at %s", p.fire, base)
	}
	if p.skipCount != 0 {
		t.Fatalf("normal fire recorded a skip window (count=%d), want none", p.skipCount)
	}
	if !p.hasNext || !p.nextFireAt.Equal(base.Add(time.Minute)) {
		t.Fatalf("normal fire next_fire_at = %s, want %s", p.nextFireAt, base.Add(time.Minute))
	}
}
