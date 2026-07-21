package automation

import (
	"context"
	"time"
)

// ScheduleTicker is the supervised sweep that fires schedules (spec §33, E11 Task 3). It is a SIBLING of
// the delivery-reconciler, not an extension: the reconciler's domain is trigger_deliveries state remnants,
// the ticker's is schedules/occurrences — the due-scan (claim durable occurrences) and the pending-
// occurrence handoff sweep, both inside its Run. It runs under one supervised loop named "schedule-ticker"
// (main.go), the coordinator reconciler's ticker shape; the delivery-reconciler is untouched.
type ScheduleTicker struct {
	store    *ScheduleStore
	interval time.Duration
	limit    int
	now      func() time.Time
	log      func(string, ...any)
}

// NewScheduleTicker builds the ticker over a wired schedule store. limit caps each sweep's batch. AUT-007
// wants two instances racing one PG, so the constructor is dedicated (each replica gets its own loop).
func NewScheduleTicker(store *ScheduleStore, interval time.Duration, limit int, log func(string, ...any)) *ScheduleTicker {
	if log == nil {
		log = func(string, ...any) {}
	}
	if limit <= 0 {
		limit = 100
	}
	if interval <= 0 {
		interval = time.Second
	}
	return &ScheduleTicker{
		store:    store,
		interval: interval,
		limit:    limit,
		now:      func() time.Time { return time.Now().UTC() },
		log:      log,
	}
}

// WithClock injects the clock the ticker reads (the fault/component tiers drive misfire + jitter without
// waiting on real wall-clock time). Returns the ticker for chaining.
func (t *ScheduleTicker) WithClock(now func() time.Time) *ScheduleTicker {
	t.now = now
	return t
}

// Run drives the ticker on its interval until ctx is cancelled (the delivery-reconciler loop shape). A
// transient tick error is returned so the supervisor restarts the loop; the durable rows are the source of
// truth, so a missed tick just resumes next pass.
func (t *ScheduleTicker) Run(ctx context.Context) error {
	ticker := time.NewTicker(t.interval)
	defer ticker.Stop()
	for {
		if err := t.Tick(ctx); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Tick runs one sweep: fire due schedules (claim durable 'pending' occurrences + advance next_fire_at),
// then hand off pending occurrences to the delivery pipeline. Fire-then-sweep so a fresh occurrence admits
// the same tick; a crash between the two phases is recovered by the next tick's sweep — the occurrence is
// durable BEFORE any run is born (§5). Both phases read ONE `now`, so a schedule's plan and its jitter gate
// are evaluated against a consistent clock.
func (t *ScheduleTicker) Tick(ctx context.Context) error {
	now := t.now()
	if err := t.store.fireDueSchedules(ctx, now, t.limit, t.log); err != nil {
		return err
	}
	return t.store.sweepPendingOccurrences(ctx, now, t.limit, t.log)
}
