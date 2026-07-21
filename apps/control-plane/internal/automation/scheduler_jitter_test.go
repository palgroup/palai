package automation

import (
	"testing"
	"time"
)

// TestJitterDeterministicBoundedVisible proves the admission jitter is a deterministic, bounded, per-
// occurrence offset (§33.5, brief B8): the same occurrence id yields the same offset (survives a restart),
// it never exceeds jitter_seconds, jitter_seconds=0 is a no-op, and a jittered admit never lands beyond
// ends_at.
func TestJitterDeterministicBoundedVisible(t *testing.T) {
	planned := time.Date(2026, 7, 22, 6, 0, 0, 0, time.UTC)

	// Deterministic + bounded across a spread of ids.
	for _, id := range []string{"occ_a", "occ_b", "occ_c", "occ_deadbeef", "occ_0000"} {
		off := jitterOffset(id, 60)
		if again := jitterOffset(id, 60); again != off {
			t.Fatalf("jitterOffset(%q) not deterministic: %v != %v", id, off, again)
		}
		if off < 0 || off >= 60*time.Second {
			t.Fatalf("jitterOffset(%q) = %v, want within [0, 60s)", id, off)
		}
	}

	// jitter_seconds = 0 is a no-op (admit exactly at planned_at).
	if got := admitAfter("occ_x", planned, 0, time.Time{}); !got.Equal(planned) {
		t.Fatalf("admitAfter with no jitter = %s, want planned_at %s", got, planned)
	}

	// A jittered admit never lands beyond ends_at (clamped).
	ends := planned.Add(5 * time.Second)
	got := admitAfter("occ_late", planned, 3600, ends) // a large jitter window
	if got.After(ends) {
		t.Fatalf("admitAfter = %s, want clamped to ends_at %s", got, ends)
	}

	// planned_at vs admit_after is visible (a non-zero jitter offsets admission for at least some ids), so
	// the row can show lateness. Find one id whose offset is > 0 to prove the offset is applied.
	applied := false
	for _, id := range []string{"occ_1", "occ_2", "occ_3", "occ_4", "occ_5"} {
		if admitAfter(id, planned, 60, time.Time{}).After(planned) {
			applied = true
			break
		}
	}
	if !applied {
		t.Fatal("no id produced a positive jitter offset; jitter is never applied")
	}
}
