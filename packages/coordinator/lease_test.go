package coordinator

import (
	"testing"
	"time"
)

// TestFullJitterBackoffStaysWithinExponentialCeiling proves the backoff is always in
// [0, min(max, base*2^(attempt-1))]: never negative, never past the per-attempt
// exponential ceiling, and never past the cap. Sampled many times per attempt because
// the schedule is randomized.
func TestFullJitterBackoffStaysWithinExponentialCeiling(t *testing.T) {
	const base = 10 * time.Millisecond
	const max = 1 * time.Second
	for attempt := 1; attempt <= 12; attempt++ {
		ceiling := base
		for i := 1; i < attempt && ceiling < max; i++ {
			ceiling *= 2
		}
		if ceiling > max {
			ceiling = max
		}
		for sample := 0; sample < 200; sample++ {
			got := FullJitterBackoff(attempt, base, max)
			if got < 0 || got > ceiling {
				t.Fatalf("attempt %d backoff = %v, want within [0, %v]", attempt, got, ceiling)
			}
		}
	}
}

// TestFullJitterBackoffDisabledWithoutBase proves a non-positive base or attempt
// yields no backoff (immediate retry), so a caller can opt out.
func TestFullJitterBackoffDisabledWithoutBase(t *testing.T) {
	if got := FullJitterBackoff(3, 0, time.Second); got != 0 {
		t.Fatalf("zero-base backoff = %v, want 0", got)
	}
	if got := FullJitterBackoff(0, 10*time.Millisecond, time.Second); got != 0 {
		t.Fatalf("zero-attempt backoff = %v, want 0", got)
	}
}
