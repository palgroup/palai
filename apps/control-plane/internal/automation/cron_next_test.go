package automation

import (
	"errors"
	"math/rand"
	"strconv"
	"testing"
	"time"
)

// TestNextOccurrenceDSTDuplicateFiresOnceEarlierInstant proves the fall-back (duplicate wall time) rule
// (AUT-006, §33.2): on 2026-11-01 America/New_York falls back 02:00 EDT → 01:00 EST, so wall 01:30 occurs
// TWICE — at 05:30 UTC (EDT, -4) and again at 06:30 UTC (EST, -5). A daily 01:30 cron must fire ONCE, at
// the EARLIER UTC instant. Europe/Istanbul (permanently UTC+3 since 2016) is the no-DST control: the same
// wall time resolves to a single unambiguous instant.
func TestNextOccurrenceDSTDuplicateFiresOnceEarlierInstant(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load America/New_York: %v", err)
	}
	sched, err := ParseCron("30 1 * * *")
	if err != nil {
		t.Fatalf("ParseCron error = %v", err)
	}

	// After midnight local on the fall-back day, the next 01:30 is the EARLIER (EDT) instant, once.
	after := time.Date(2026, 11, 1, 0, 0, 0, 0, ny)
	got, err := sched.Next(after, ny)
	if err != nil {
		t.Fatalf("Next error = %v, want the earlier duplicate instant", err)
	}
	wantEarlier := time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC) // 01:30 EDT
	if !got.Equal(wantEarlier) {
		t.Fatalf("Next = %s, want the EARLIER duplicate instant %s (not the 06:30Z EST repeat)", got.UTC(), wantEarlier)
	}
	// The next fire after that earlier instant is the NEXT DAY's 01:30 — the duplicate never fires twice.
	next, err := sched.Next(got, ny)
	if err != nil {
		t.Fatalf("second Next error = %v", err)
	}
	if !next.After(time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("Next after the duplicate = %s, want the following day (the 06:30Z repeat must not fire)", next.UTC())
	}

	// No-DST control: Istanbul is UTC+3, so 01:30 local is a single 22:30Z instant the prior day.
	ist, err := time.LoadLocation("Europe/Istanbul")
	if err != nil {
		t.Fatalf("load Europe/Istanbul: %v", err)
	}
	gotIst, err := sched.Next(time.Date(2026, 11, 1, 0, 0, 0, 0, ist), ist)
	if err != nil {
		t.Fatalf("Istanbul Next error = %v", err)
	}
	// Istanbul is UTC+3, so 01:30 local on 2026-11-01 is a single 22:30Z instant on 2026-10-31.
	if !gotIst.Equal(time.Date(2026, 10, 31, 22, 30, 0, 0, time.UTC)) {
		t.Fatalf("Istanbul Next = %s, want the single 2026-10-31T22:30Z instant (no DST)", gotIst.UTC())
	}
}

// TestNextOccurrenceDSTGapSkipsToNextValid proves the spring-forward (gap wall time) rule (AUT-006,
// §33.2): on 2026-03-08 America/New_York springs forward 02:00 EST → 03:00 EDT, so wall 02:30 never occurs.
// The raw DST primitive (resolveInstant) flags the gap with ErrNonexistentLocalTime; Next SKIPS that
// nonexistent occurrence and fires the NEXT valid daily instant (2026-03-09 02:30 EDT) — it must never
// resume from time.Date's "moved" normalization (which points BACKWARDS into the gap and permanently
// exhausts the schedule).
func TestNextOccurrenceDSTGapSkipsToNextValid(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load America/New_York: %v", err)
	}
	// The raw primitive still flags the gap (unchanged — the ticker's guarantee is built on it).
	if _, err := resolveInstant(2026, 3, 8, 2, 30, ny); !errors.Is(err, ErrNonexistentLocalTime) {
		t.Fatalf("resolveInstant over the spring-forward gap = %v, want ErrNonexistentLocalTime", err)
	}

	sched, err := ParseCron("30 2 * * *")
	if err != nil {
		t.Fatalf("ParseCron error = %v", err)
	}
	got, err := sched.Next(time.Date(2026, 3, 8, 0, 0, 0, 0, ny), ny)
	if err != nil {
		t.Fatalf("Next over the gap day error = %v, want the next valid occurrence (not exhaustion)", err)
	}
	want := time.Date(2026, 3, 9, 2, 30, 0, 0, ny) // the gap-day 02:30 skipped; next day's 02:30 EDT
	if !got.Equal(want) {
		t.Fatalf("Next over the gap day = %s, want the gap-day 02:30 SKIPPED → %s", got.UTC(), want.UTC())
	}

	// The no-DST control resolves the same wall time cleanly (no gap, no skip).
	ist, _ := time.LoadLocation("Europe/Istanbul")
	if _, err := sched.Next(time.Date(2026, 3, 8, 0, 0, 0, 0, ist), ist); err != nil {
		t.Fatalf("Istanbul Next over the same wall time error = %v, want a clean instant", err)
	}
}

// TestNextMonotonicAcrossFallBackSecondPass proves Next stays strictly monotonic through the fall-back
// repeated hour (§33.2, review #3): `after` = 06:00Z is 01:00 EST — INSIDE the second pass of the repeated
// hour, past the earlier 01:30 EDT instant (05:30Z). Next must NOT return that already-past 05:30Z (which
// would make Create/Revise write a PAST next_fire_at → a spurious immediate misfire); it returns the next
// strictly-later occurrence.
func TestNextMonotonicAcrossFallBackSecondPass(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load America/New_York: %v", err)
	}
	sched, err := ParseCron("30 1 * * *")
	if err != nil {
		t.Fatalf("ParseCron error = %v", err)
	}
	after := time.Date(2026, 11, 1, 6, 0, 0, 0, time.UTC) // 01:00 EST, second pass, after 05:30Z
	got, err := sched.Next(after, ny)
	if err != nil {
		t.Fatalf("Next error = %v", err)
	}
	if !got.After(after) {
		t.Fatalf("Next = %s is not strictly after %s (the fall-back earlier instant leaked as a past next_fire_at)", got.UTC(), after)
	}
}

// TestNextMonotonicWithinBounds is the property check: for a random valid expression, successive Next
// calls strictly increase and every returned instant's wall fields satisfy the expression. Europe/Istanbul
// (no DST) keeps the property clean of gap/duplicate special cases (those have their own tests above).
func TestNextMonotonicWithinBounds(t *testing.T) {
	ist, err := time.LoadLocation("Europe/Istanbul")
	if err != nil {
		t.Fatalf("load Europe/Istanbul: %v", err)
	}
	rng := rand.New(rand.NewSource(20260722))
	for i := 0; i < 300; i++ {
		expr := randomCronExpr(rng)
		sched, err := ParseCron(expr)
		if err != nil {
			t.Fatalf("ParseCron(%q) error = %v (generator emitted an invalid expr)", expr, err)
		}
		cur := time.Date(2026, time.Month(1+rng.Intn(12)), 1+rng.Intn(28), rng.Intn(24), rng.Intn(60), 0, 0, ist)
		for step := 0; step < 5; step++ {
			nxt, err := sched.Next(cur, ist)
			if errors.Is(err, ErrNoCronOccurrence) {
				break // a rare impossible-in-window expr; the bounded-scan ceiling, not a defect
			}
			if err != nil {
				t.Fatalf("Next(%q, %s) error = %v", expr, cur.UTC(), err)
			}
			if !nxt.After(cur) {
				t.Fatalf("Next(%q) = %s is not strictly after %s", expr, nxt.UTC(), cur.UTC())
			}
			w := nxt.In(ist)
			if !sched.matches(mkWall(w.Year(), int(w.Month()), w.Day(), w.Hour(), w.Minute())) {
				t.Fatalf("Next(%q) = %s does not satisfy the expression", expr, w)
			}
			cur = nxt
		}
	}
}

// randomCronExpr emits a random expression drawn only from the documented subset, so the property test
// never has to special-case a rejected input.
func randomCronExpr(rng *rand.Rand) string {
	field := func(min, max int) string {
		switch rng.Intn(4) {
		case 0:
			return "*"
		case 1:
			return strconv.Itoa(min + rng.Intn(max-min+1))
		case 2:
			step := 1 + rng.Intn(max-min+1)
			return "*/" + strconv.Itoa(step)
		default:
			a := min + rng.Intn(max-min+1)
			b := a + rng.Intn(max-a+1)
			return strconv.Itoa(a) + "-" + strconv.Itoa(b)
		}
	}
	return field(0, 59) + " " + field(0, 23) + " " + field(1, 31) + " " + field(1, 12) + " " + field(0, 6)
}
