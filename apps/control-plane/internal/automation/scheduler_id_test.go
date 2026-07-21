package automation

import (
	"strings"
	"testing"
	"time"
)

// TestOccurrenceIDDeterministicAcrossClockChange proves the occurrence id is a pure function of
// (schedule_id, revision, planned UTC instant) (§33.5, brief B4): recomputing after a simulated NTP jump
// yields the SAME id (the id never depends on wall-clock-now), the same instant in a different zone
// collapses to the same id (UTC-normalized), and a revision bump yields a DIFFERENT id.
func TestOccurrenceIDDeterministicAcrossClockChange(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	planned := time.Date(2026, 7, 22, 6, 30, 0, 0, time.UTC)

	id := OccurrenceID("sch_orders", 1, planned)
	if !strings.HasPrefix(id, "occ_") || len(id) != len("occ_")+32 {
		t.Fatalf("OccurrenceID = %q, want occ_ + 32 hex chars", id)
	}

	// A recompute "after an NTP jump" — the inputs are unchanged, so the id is byte-identical (it never
	// reads the current clock).
	if again := OccurrenceID("sch_orders", 1, planned); again != id {
		t.Fatalf("OccurrenceID recomputed = %q, want the same %q (id must not depend on now)", again, id)
	}

	// The SAME instant expressed in another zone normalizes to the same UTC → the same id.
	if zoned := OccurrenceID("sch_orders", 1, planned.In(ny)); zoned != id {
		t.Fatalf("OccurrenceID for the same instant in another zone = %q, want the UTC-normalized %q", zoned, id)
	}

	// A revision bump pins a distinct occurrence — a different id, so a revised schedule never collides
	// with its old occurrences on the same instant.
	if bumped := OccurrenceID("sch_orders", 2, planned); bumped == id {
		t.Fatal("OccurrenceID with a bumped revision matched revision 1 (revisions must not collide)")
	}
	// A distinct instant is a distinct id.
	if other := OccurrenceID("sch_orders", 1, planned.Add(time.Minute)); other == id {
		t.Fatal("OccurrenceID for a distinct instant collided with the original")
	}
}
