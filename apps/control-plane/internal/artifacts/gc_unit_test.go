package artifacts

import (
	"testing"
	"time"
)

// TestWithinGraceFailsClosedOnUnknownTimestamp is the delete-safety guard: an object whose
// listing carries no last-modified time (zero) must be treated as IN-GRACE, so a missing
// timestamp fails CLOSED (never reclaimed) instead of reading as infinitely old and being
// deleted on sight. The known-time cases pin the ordinary boundary either side of the cutoff.
func TestWithinGraceFailsClosedOnUnknownTimestamp(t *testing.T) {
	cutoff := time.Now()

	if !withinGrace(time.Time{}, cutoff) {
		t.Fatal("unknown (zero) last-modified must be in-grace — fail closed, never delete")
	}
	if withinGrace(cutoff.Add(-time.Hour), cutoff) {
		t.Fatal("an object older than the cutoff must be reclaimable (not in-grace)")
	}
	if !withinGrace(cutoff.Add(time.Minute), cutoff) {
		t.Fatal("an object newer than the cutoff must be in-grace")
	}
}
