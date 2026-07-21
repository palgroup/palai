package automation

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// In-house five-field cron parser (spec §33.2, E11 Task 3). NO new dependency — the plan pins an in-house
// parser (robfig/cron is deliberately absent from go.mod). The DOCUMENTED SUBSET, and only it:
//
//	field order: minute hour day-of-month month day-of-week
//	ranges:      minute 0-59, hour 0-23, dom 1-31, month 1-12, dow 0-6 (0 = Sunday)
//	term grammar per field: "*", "N", "a-b", "a,b,c" (comma list of terms), "*/n", "a-b/n"
//	NUMERIC ONLY — no seconds field, no @macros (@daily/@hourly/…), no month/day NAMES (JAN/MON).
//	Anything outside this grammar is rejected at parse (fail-closed), so a schedule create is a 400.
//
// DAY-OF-MONTH vs DAY-OF-WEEK — Vixie OR-semantics: when BOTH dom and dow are restricted (neither is "*"),
// a day matches if EITHER field matches (the classic crontab behaviour). When only one is restricted, that
// one must match; when neither is, every day matches. See matchesDay.
//
// Adding names/@macros/seconds is a spec change, not a bug — deferred until §33.2 documents them.

// ErrInvalidCron is the sentinel a malformed/unsupported cron expression maps to (the create handler turns
// it into a 400). Callers use errors.Is; the wrapped message names the exact field/term at fault.
var ErrInvalidCron = errors.New("automation: invalid cron expression")

// ErrNonexistentLocalTime is returned by Next/resolveInstant when a cron-matched wall time falls in a DST
// spring-forward GAP (the local time never occurs). The ticker maps it to the schedule's misfire policy —
// it is never silently resolved to a guessed instant (spec §33.2: time.Date's gap choice is "not
// well-defined", so it is never trusted).
var ErrNonexistentLocalTime = errors.New("automation: nonexistent local time (DST gap)")

// ErrNoCronOccurrence is returned when no matching instant exists within the bounded 5-year lookahead — an
// impossible-in-window spec (e.g. "0 0 30 2 *", Feb 30). The create handler rejects it; the ticker
// exhausts the schedule (next_fire_at NULL).
var ErrNoCronOccurrence = errors.New("automation: cron expression has no occurrence in the 5-year lookahead")

// cronScanLimit bounds the minute-step forward scan at ~5 years so an impossible spec terminates with
// ErrNoCronOccurrence instead of looping forever.
// ponytail: minute scan, field-jumping math if this profiles hot — an impossible cron is rejected at
// create (one 5y scan), and a live schedule matches within a day/year, so the naive scan never runs hot.
const cronScanLimit = 366 * 24 * 60 * 5

// CronSchedule is a parsed five-field expression as five bitsets plus the dom/dow restriction flags the
// Vixie OR-semantics needs.
type CronSchedule struct {
	minute        uint64 // bits 0..59
	hour          uint64 // bits 0..23
	dom           uint64 // bits 1..31
	month         uint64 // bits 1..12
	dow           uint64 // bits 0..6 (0 = Sunday)
	domRestricted bool
	dowRestricted bool
}

// cronField bounds a field's legal numeric range.
type cronField struct {
	min, max int
}

var (
	fieldMinute = cronField{0, 59}
	fieldHour   = cronField{0, 23}
	fieldDOM    = cronField{1, 31}
	fieldMonth  = cronField{1, 12}
	fieldDOW    = cronField{0, 6}
)

// ParseCron compiles a five-field numeric cron expression into a CronSchedule, or a wrapped ErrInvalidCron
// for anything outside the documented subset.
func ParseCron(expr string) (CronSchedule, error) {
	parts := strings.Fields(strings.TrimSpace(expr))
	if len(parts) != 5 {
		return CronSchedule{}, fmt.Errorf("%w: want 5 fields (minute hour dom month dow), got %d", ErrInvalidCron, len(parts))
	}
	minute, _, err := parseField(parts[0], fieldMinute)
	if err != nil {
		return CronSchedule{}, err
	}
	hour, _, err := parseField(parts[1], fieldHour)
	if err != nil {
		return CronSchedule{}, err
	}
	dom, domRestricted, err := parseField(parts[2], fieldDOM)
	if err != nil {
		return CronSchedule{}, err
	}
	month, _, err := parseField(parts[3], fieldMonth)
	if err != nil {
		return CronSchedule{}, err
	}
	dow, dowRestricted, err := parseField(parts[4], fieldDOW)
	if err != nil {
		return CronSchedule{}, err
	}
	return CronSchedule{
		minute: minute, hour: hour, dom: dom, month: month, dow: dow,
		domRestricted: domRestricted, dowRestricted: dowRestricted,
	}, nil
}

// parseField compiles one field (a comma-separated list of terms) into its bitset. restricted reports
// whether the field is anything other than a bare "*" (the Vixie OR-semantics input).
func parseField(field string, b cronField) (bits uint64, restricted bool, err error) {
	if field == "" {
		return 0, false, fmt.Errorf("%w: empty field", ErrInvalidCron)
	}
	restricted = field != "*"
	for _, term := range strings.Split(field, ",") {
		set, err := parseTerm(term, b)
		if err != nil {
			return 0, false, err
		}
		bits |= set
	}
	return bits, restricted, nil
}

// parseTerm compiles a single term: "*", "N", "a-b", "*/n", "a-b/n".
func parseTerm(term string, b cronField) (uint64, error) {
	base, step := term, 1
	if slash := strings.IndexByte(term, '/'); slash >= 0 {
		base = term[:slash]
		n, err := strconv.Atoi(term[slash+1:])
		if err != nil || n < 1 {
			return 0, fmt.Errorf("%w: bad step in %q", ErrInvalidCron, term)
		}
		step = n
	}

	var lo, hi int
	switch {
	case base == "*":
		lo, hi = b.min, b.max
	case strings.IndexByte(base, '-') >= 0:
		dash := strings.IndexByte(base, '-')
		var err1, err2 error
		lo, err1 = strconv.Atoi(base[:dash])
		hi, err2 = strconv.Atoi(base[dash+1:])
		if err1 != nil || err2 != nil {
			return 0, fmt.Errorf("%w: bad range %q", ErrInvalidCron, base)
		}
		if lo > hi {
			return 0, fmt.Errorf("%w: inverted range %q", ErrInvalidCron, base)
		}
	default:
		v, err := strconv.Atoi(base)
		if err != nil {
			return 0, fmt.Errorf("%w: %q is not a number", ErrInvalidCron, base)
		}
		lo, hi = v, v
	}
	if lo < b.min || hi > b.max {
		return 0, fmt.Errorf("%w: %q out of range [%d,%d]", ErrInvalidCron, term, b.min, b.max)
	}

	var bits uint64
	for v := lo; v <= hi; v += step {
		bits |= 1 << uint(v)
	}
	return bits, nil
}

// matches reports whether a wall-clock instant (its Y-M-D-H-M + weekday) satisfies every field. The zone
// carried by t is irrelevant — the caller supplies the wall fields; the ticker resolves the real instant
// separately (DST — see resolveInstant).
func (s CronSchedule) matches(t time.Time) bool {
	if s.minute&(1<<uint(t.Minute())) == 0 {
		return false
	}
	if s.hour&(1<<uint(t.Hour())) == 0 {
		return false
	}
	if s.month&(1<<uint(t.Month())) == 0 {
		return false
	}
	return s.matchesDay(t)
}

// matchesDay applies the Vixie OR-semantics: both dom+dow restricted → either matches; otherwise the
// restricted field(s) must match (a "*" field always matches).
func (s CronSchedule) matchesDay(t time.Time) bool {
	domHit := s.dom&(1<<uint(t.Day())) != 0
	dowHit := s.dow&(1<<uint(int(t.Weekday()))) != 0
	if s.domRestricted && s.dowRestricted {
		return domHit || dowHit
	}
	return domHit && dowHit
}

// Next returns the first matching instant strictly after `after`, resolved in loc. It scans wall-clock
// minutes (calendar-iterated in UTC so +1 minute is always +1 WALL minute, undistorted by DST), and
// resolves each matching wall time to a real instant via resolveInstant:
//   - a duplicate wall time (fall-back) resolves to the EARLIER of its two instants (fires once);
//   - a nonexistent wall time (spring-forward gap) returns ErrNonexistentLocalTime (caller → misfire);
//   - no match within the 5-year bound returns ErrNoCronOccurrence.
func (s CronSchedule) Next(after time.Time, loc *time.Location) (time.Time, error) {
	la := after.In(loc)
	// The wall cursor is a UTC time carrying `after`'s wall fields; advancing it by a minute always steps
	// the wall clock by a minute (no DST distortion), and time.Date normalization keeps its calendar valid
	// (so an impossible day like Feb 30 is simply never generated).
	cursor := time.Date(la.Year(), la.Month(), la.Day(), la.Hour(), la.Minute(), 0, 0, time.UTC).Add(time.Minute)
	for i := 0; i < cronScanLimit; i++ {
		if s.matches(cursor) {
			return resolveInstant(cursor.Year(), int(cursor.Month()), cursor.Day(), cursor.Hour(), cursor.Minute(), loc)
		}
		cursor = cursor.Add(time.Minute)
	}
	return time.Time{}, ErrNoCronOccurrence
}

// resolveInstant maps a wall-clock Y-M-D-H-M in loc to a real UTC instant, applying the §33.2 DST rules
// WITHOUT trusting time.Date's "not well-defined" gap/duplicate choice:
//   - GAP (spring-forward): time.Date normalizes the nonexistent wall time to a different clock value —
//     detected by the round-trip mismatch — so it returns ErrNonexistentLocalTime.
//   - DUPLICATE (fall-back): a dual-offset probe builds the two candidate instants (using the zone offset
//     an hour before and an hour after); if both map back to the same wall time, the EARLIER is returned.
//   - otherwise the single unambiguous instant.
func resolveInstant(y, mo, d, h, mi int, loc *time.Location) (time.Time, error) {
	t := time.Date(y, time.Month(mo), d, h, mi, 0, 0, loc)
	if t.Year() != y || int(t.Month()) != mo || t.Day() != d || t.Hour() != h || t.Minute() != mi {
		return time.Time{}, ErrNonexistentLocalTime // normalization moved it → the wall time never occurs
	}
	wallSecs := time.Date(y, time.Month(mo), d, h, mi, 0, 0, time.UTC).Unix()
	_, offBefore := t.Add(-time.Hour).Zone()
	_, offAfter := t.Add(time.Hour).Zone()
	if offBefore != offAfter {
		candBefore := time.Unix(wallSecs-int64(offBefore), 0).UTC()
		candAfter := time.Unix(wallSecs-int64(offAfter), 0).UTC()
		if sameWall(candBefore, loc, y, mo, d, h, mi) && sameWall(candAfter, loc, y, mo, d, h, mi) {
			if candBefore.Before(candAfter) {
				return candBefore, nil
			}
			return candAfter, nil
		}
	}
	return t.UTC(), nil
}

// sameWall reports whether inst, read in loc, has exactly the given wall fields — the confirmation both
// dual-offset candidates land on the ambiguous local time (a true fall-back duplicate).
func sameWall(inst time.Time, loc *time.Location, y, mo, d, h, mi int) bool {
	l := inst.In(loc)
	return l.Year() == y && int(l.Month()) == mo && l.Day() == d && l.Hour() == h && l.Minute() == mi
}
