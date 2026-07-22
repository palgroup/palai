package mcp

import (
	"testing"
	"time"
)

// TestBreakerTripsAndHalfOpens proves the connection breaker's lifecycle: threshold consecutive failures
// trip it open; it denies during cooldown; after cooldown it admits ONE half-open trial; a failed trial
// re-opens, a successful trial closes.
func TestBreakerTripsAndHalfOpens(t *testing.T) {
	clock := time.Unix(0, 0)
	b := newBreaker(3, time.Minute, func() time.Time { return clock })

	// Under threshold: still allowed.
	b.recordFailure("c1")
	b.recordFailure("c1")
	if !b.allow("c1") {
		t.Fatal("breaker opened before the threshold (2 < 3 failures)")
	}
	// Third failure trips it.
	b.recordFailure("c1")
	if b.allow("c1") {
		t.Fatal("breaker did not open after 3 consecutive failures")
	}
	// A different connection is unaffected (per-connection).
	if !b.allow("c2") {
		t.Fatal("breaker for c1 leaked into c2")
	}
	// Within cooldown: still denied.
	clock = clock.Add(30 * time.Second)
	if b.allow("c1") {
		t.Fatal("breaker admitted a call inside the cooldown window")
	}
	// After cooldown: one half-open trial admitted.
	clock = clock.Add(31 * time.Second)
	if !b.allow("c1") {
		t.Fatal("breaker did not admit a half-open trial after cooldown")
	}
	// A failed trial re-opens immediately.
	b.recordFailure("c1")
	if b.allow("c1") {
		t.Fatal("a failed half-open trial did not re-open the breaker")
	}
	// After cooldown again, a successful trial closes it.
	clock = clock.Add(61 * time.Second)
	if !b.allow("c1") {
		t.Fatal("breaker did not re-admit a half-open trial after the second cooldown")
	}
	b.recordSuccess("c1")
	if !b.allow("c1") {
		t.Fatal("a successful trial did not close the breaker")
	}
}
