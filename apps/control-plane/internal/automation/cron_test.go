package automation

import (
	"testing"
	"time"
)

// mkWall builds a wall-clock instant (the matcher reads Y-M-D-H-M + weekday from it; the zone is
// irrelevant to matching, so UTC stands in for "wall fields").
func mkWall(y, mo, d, h, mi int) time.Time {
	return time.Date(y, time.Month(mo), d, h, mi, 0, 0, time.UTC)
}

// TestCronParserAcceptsOnlyDocumentedFiveField pins the documented subset (§33.2, brief B2): five numeric
// fields (minute hour dom month dow), the term grammar *, N, a-b, a,b,c, */n, a-b/n — and rejects
// everything outside it (a 6-field seconds form, @macros, month/day NAMES, out-of-range values, empty).
func TestCronParserAcceptsOnlyDocumentedFiveField(t *testing.T) {
	accept := []string{
		"* * * * *",           // every minute
		"0 0 * * *",           // midnight
		"30 2 * * *",          // 02:30 daily
		"0-29 9-17 * * 1-5",   // ranges + weekday range
		"0,15,30,45 * * * *",  // comma list
		"*/15 * * * *",        // step
		"0 */2 * * *",         // step on hour
		"0-30/10 * * * *",     // range with step
		"59 23 31 12 6",       // upper bounds (dow 6 = Saturday)
		"0 0 1 1 0",           // lower bounds (dow 0 = Sunday)
		"0 0 1,15 * 1,3,5",    // dom + dow both restricted (Vixie OR)
	}
	for _, expr := range accept {
		if _, err := ParseCron(expr); err != nil {
			t.Errorf("ParseCron(%q) error = %v, want accept", expr, err)
		}
	}

	reject := []string{
		"",                    // empty
		"* * * *",             // four fields
		"* * * * * *",         // six fields (seconds form is NOT supported)
		"@daily",              // macro
		"0 0 * * MON",         // weekday NAME
		"0 0 1 JAN *",         // month NAME
		"60 * * * *",          // minute out of range (0-59)
		"* 24 * * *",          // hour out of range (0-23)
		"* * 0 * *",           // dom out of range (1-31)
		"* * 32 * *",          // dom out of range
		"* * * 13 *",          // month out of range (1-12)
		"* * * * 7",           // dow out of range (0-6)
		"*/0 * * * *",         // zero step
		"5-3 * * * *",         // inverted range
		"1..5 * * * *",        // malformed separator
		"a * * * *",           // non-numeric
	}
	for _, expr := range reject {
		if _, err := ParseCron(expr); err == nil {
			t.Errorf("ParseCron(%q) = nil error, want reject", expr)
		}
	}
}

// TestCronMatchesFields spot-checks the compiled matcher: a field bitset matches the right wall values and
// Vixie OR-semantics apply when both DOM and DOW are restricted.
func TestCronMatchesFields(t *testing.T) {
	// 30 2 * * * — 02:30 every day.
	s, err := ParseCron("30 2 * * *")
	if err != nil {
		t.Fatalf("ParseCron error = %v", err)
	}
	if !s.matches(mkWall(2026, 7, 22, 2, 30)) { // a Wednesday
		t.Fatal("30 2 * * * should match 02:30")
	}
	if s.matches(mkWall(2026, 7, 22, 2, 31)) {
		t.Fatal("30 2 * * * should not match 02:31")
	}

	// Vixie OR: dom=15 OR dow=1(Mon). 2026-07-15 is a Wednesday (dom matches, dow does not) → matches.
	or, err := ParseCron("0 0 15 * 1")
	if err != nil {
		t.Fatalf("ParseCron error = %v", err)
	}
	if !or.matches(mkWall(2026, 7, 15, 0, 0)) {
		t.Fatal("dom=15 OR dow=Mon should match the 15th even on a Wednesday")
	}
	// 2026-07-20 is a Monday (dow matches, dom does not) → matches.
	if !or.matches(mkWall(2026, 7, 20, 0, 0)) {
		t.Fatal("dom=15 OR dow=Mon should match a Monday even when dom != 15")
	}
	// 2026-07-16 is a Thursday, dom 16 — neither matches → no match.
	if or.matches(mkWall(2026, 7, 16, 0, 0)) {
		t.Fatal("dom=15 OR dow=Mon should not match the 16th on a Thursday")
	}
}
