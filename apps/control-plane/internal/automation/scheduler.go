package automation

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"time"
)

// scheduleSpec is a schedule's firing config, resolved once so the pure planner (enumerate + misfire) is
// independent of the DB row shape and unit-testable without Postgres.
type scheduleSpec struct {
	kind          string // "cron" | "one_time"
	cron          CronSchedule
	loc           *time.Location
	oneTimeAt     time.Time // zero unless kind == one_time
	startsAt      time.Time // zero unless set
	endsAt        time.Time // zero unless set
	misfirePolicy string
	grace         time.Duration
	maxCatchUp    int
}

// tickPlan is the pure outcome of evaluating one due schedule at `now`: the firing instants to materialize
// (oldest-first), an optional windowed-skip {from,to,count}, the new next_fire_at (hasNext=false ⇒ NULL,
// the schedule is exhausted), and the fail decision. The store applies it in one guarded transaction.
type tickPlan struct {
	fire       []time.Time
	skipFrom   time.Time
	skipTo     time.Time
	skipCount  int
	nextFireAt time.Time
	hasNext    bool
	fail       bool
	failReason string
}

// firstFireAt computes the first fire instant strictly after `base` (create + advance), or false when the
// schedule is exhausted (a one_time already past, or a cron whose next instant is beyond ends_at / has no
// occurrence in the 5-year lookahead). A cron instant that falls in a DST gap is skipped by Next.
func (s scheduleSpec) firstFireAt(base time.Time) (time.Time, bool) {
	var next time.Time
	if s.kind == "one_time" {
		if !s.oneTimeAt.After(base) {
			return time.Time{}, false
		}
		next = s.oneTimeAt.UTC()
	} else {
		n, err := s.cron.Next(base, s.loc)
		if err != nil {
			return time.Time{}, false
		}
		next = n
	}
	if !s.endsAt.IsZero() && next.After(s.endsAt) {
		return time.Time{}, false
	}
	return next, true
}

// enumerateDue collects every scheduled instant from currentNextFireAt through now (the missed set,
// oldest-first) plus the first instant after now (the candidate next_fire_at). A DST-gap instant is
// skipped (it never occurs). The scan is bounded by cronScanLimit so a pathological zone/expr terminates.
func (s scheduleSpec) enumerateDue(currentNextFireAt, now time.Time) (missed []time.Time, next time.Time, hasNext bool) {
	if s.kind == "one_time" {
		if !currentNextFireAt.After(now) {
			missed = []time.Time{currentNextFireAt.UTC()}
		}
		return missed, time.Time{}, false // a one_time never has a next
	}
	cur := currentNextFireAt.UTC()
	if !cur.After(now) {
		missed = append(missed, cur)
	}
	for i := 0; i < cronScanLimit; i++ {
		nxt, err := s.cron.Next(cur, s.loc)
		if err != nil {
			return missed, time.Time{}, false // no further occurrence
		}
		if !s.endsAt.IsZero() && nxt.After(s.endsAt) {
			return missed, time.Time{}, false // past the end boundary → exhausted
		}
		if nxt.After(now) {
			return missed, nxt, true
		}
		missed = append(missed, nxt)
		cur = nxt
	}
	return missed, time.Time{}, false
}

// planTick is the PURE per-schedule plan: enumerate the due instants, then apply the misfire policy. A
// single instant that is at most `grace` late is a NORMAL fire (not a misfire); otherwise the policy
// decides. It reads no clock and no DB, so the branching is exhaustively unit-tested (B7).
func planTick(spec scheduleSpec, currentNextFireAt, now time.Time) tickPlan {
	missed, next, hasNext := spec.enumerateDue(currentNextFireAt, now)
	p := tickPlan{nextFireAt: next, hasNext: hasNext}
	if len(missed) == 0 {
		return p
	}
	latest := missed[len(missed)-1]

	// A misfire exists only when more than one instant was missed, or the single missed instant is older
	// than the grace window. Within grace = a normal (slightly late) fire — never the misfire machinery.
	//
	// Conscious (m-grace): grace applies ONLY to the single-missed case. ≥2 missed instants ALWAYS enter
	// the misfire machine, even if all are within grace — because >1 missed period means the ticker
	// genuinely lagged more than one cadence (a real backlog), so fire_once_now firing only the latest +
	// windowing the rest is correct; grace is for absorbing sub-period ticker jitter, not a multi-instant
	// catch-up window.
	if len(missed) == 1 && now.Sub(latest) <= spec.grace {
		p.fire = []time.Time{latest}
		return p
	}

	switch spec.misfirePolicy {
	case "skip":
		p.skipFrom, p.skipTo, p.skipCount = missed[0], latest, len(missed)
	case "catch_up":
		n := spec.maxCatchUp
		if n > len(missed) {
			n = len(missed)
		}
		p.fire = append([]time.Time(nil), missed[:n]...)
		if rem := missed[n:]; len(rem) > 0 {
			p.skipFrom, p.skipTo, p.skipCount = rem[0], rem[len(rem)-1], len(rem)
		}
	case "fail":
		p.fail = true
		p.failReason = fmt.Sprintf("missed %d occurrence(s) from %s to %s; misfire_policy=fail",
			len(missed), missed[0].Format(time.RFC3339), latest.Format(time.RFC3339))
	default: // fire_once_now (the default)
		p.fire = []time.Time{latest}
		if earlier := missed[:len(missed)-1]; len(earlier) > 0 {
			p.skipFrom, p.skipTo, p.skipCount = earlier[0], earlier[len(earlier)-1], len(earlier)
		}
	}
	return p
}

// jitterOffset is the deterministic per-occurrence admission jitter (spec §33.5): hash(occurrence_id) mod
// jitterSeconds. Deterministic per occurrence (so a restart computes the same offset), bounded by the
// schedule's jitter_seconds. A firing is admitted at planned_at + this offset (clamped to ends_at by the
// caller). jitterSeconds <= 0 ⇒ no jitter.
func jitterOffset(occurrenceID string, jitterSeconds int) time.Duration {
	if jitterSeconds <= 0 {
		return 0
	}
	sum := sha256.Sum256([]byte(occurrenceID))
	n := binary.BigEndian.Uint64(sum[:8]) % uint64(jitterSeconds)
	return time.Duration(n) * time.Second
}

// admitAfter is the instant a firing occurrence becomes eligible for admission: planned_at + jitter, never
// beyond ends_at (spec §33.5 — a jittered fire never lands past the schedule's end boundary).
func admitAfter(occurrenceID string, plannedAt time.Time, jitterSeconds int, endsAt time.Time) time.Time {
	at := plannedAt.Add(jitterOffset(occurrenceID, jitterSeconds))
	if !endsAt.IsZero() && at.After(endsAt) {
		return endsAt
	}
	return at
}

// The schedule.occurrence.{created,admitted,skipped,failed}.v1 event types are registered in the contract
// (event-types.json + asyncapi) but DECLARED-BUT-UNEMITTED here — the T2 trigger.delivery.* precedent: an
// occurrence has no session before it admits (events are session-scoped, NOT NULL session_id), so the
// durable fact is the schedule_occurrences ROW (queryable at GET /v1/schedules/{id}/occurrences), and the
// run-born events ride the trigger delivery's own session once the firing admits.

// OccurrenceID is the DETERMINISTIC id of a schedule firing: sha256(schedule_id | revision | RFC3339-UTC
// planned instant) → "occ_" + the first 32 hex chars (spec §33.5, E11 Task 3). Because it is a pure
// function of the (schedule, revision, instant) triple — never of the wall clock — N replicas, re-run
// ticks, and NTP jumps all derive the SAME id for the SAME planned instant. It is BOTH the occurrence
// primary key AND the delivery dedupe_key, so a double handoff collapses to one canonical delivery. The
// planned instant is normalized to UTC and formatted at second precision (planned instants are
// minute-aligned), so the same instant in any zone yields the same id.
func OccurrenceID(scheduleID string, revision int, plannedAt time.Time) string {
	seed := fmt.Sprintf("%s|%d|%s", scheduleID, revision, plannedAt.UTC().Format(time.RFC3339))
	sum := sha256.Sum256([]byte(seed))
	return "occ_" + hex.EncodeToString(sum[:])[:32]
}
