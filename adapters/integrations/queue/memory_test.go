package queue

import (
	"context"
	"testing"
	"time"
)

// clock is an injectable, hand-advanced time source so visibility/redelivery is deterministic.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newClock() *clock { return &clock{t: time.Unix(1_700_000_000, 0)} }

// TestMemoryRedeliversUntilAcked pins ack-after-commit / at-least-once (§34.2): a message the Handler does
// not Ack stays in the queue and redelivers when its lease expires — it is never lost — and once Acked it
// leaves. This is the crux a crash-before-ack relies on: an un-acked message comes back.
func TestMemoryRedeliversUntilAcked(t *testing.T) {
	clk := newClock()
	q := NewMemory(MemoryConfig{Capacity: 8, Visibility: time.Minute, MaxDeliveries: 10}, clk.now)
	ctx := context.Background()

	if err := q.Publish(ctx, "k1", []byte("body")); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	deliveries := 0
	handler := func(_ context.Context, m Message) (Disposition, error) {
		deliveries++
		if m.Attempt != deliveries {
			t.Fatalf("attempt = %d, want %d (attempt must count redeliveries)", m.Attempt, deliveries)
		}
		if deliveries < 3 {
			return Retry, nil // not acked: must come back
		}
		return Ack, nil
	}

	// First delivery, then two redeliveries after each visibility expiry, then the Ack removes it.
	for i := 0; i < 3; i++ {
		if _, err := q.Consume(ctx, 1, handler); err != nil {
			t.Fatalf("Consume: %v", err)
		}
		clk.advance(time.Minute + time.Second) // expire the lease so the un-acked message is deliverable again
	}
	if deliveries != 3 {
		t.Fatalf("deliveries = %d, want 3 (un-acked message must redeliver, never be lost)", deliveries)
	}
	d, _ := q.Depth(ctx)
	if d.Ready != 0 || d.InFlight != 0 || d.Dead != 0 {
		t.Fatalf("after Ack depth = %+v, want empty (acked message leaves the queue)", d)
	}
}

// TestMemoryDedupeRedeliveredEffectRunsOnce pins dedupe (§34.2): after the effect commits, its ack is lost
// (ackFailures), the message redelivers, and an idempotent Handler recognises the duplicate key and runs
// the effect ONCE. At-least-once delivery + an idempotency key = effectively-once effect.
func TestMemoryDedupeRedeliveredEffectRunsOnce(t *testing.T) {
	clk := newClock()
	q := NewMemory(MemoryConfig{Capacity: 8, Visibility: time.Minute, MaxDeliveries: 10}, clk.now)
	q.ackFailures = 1 // drop the first ack: the effect commits but the broker never hears the ack
	ctx := context.Background()

	if err := q.Publish(ctx, "order-42", []byte("charge")); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	effects := 0
	seen := map[string]bool{}
	handler := func(_ context.Context, m Message) (Disposition, error) {
		if seen[m.IdempotencyKey] {
			return Ack, nil // duplicate: skip the effect, still ack (honest at-least-once)
		}
		seen[m.IdempotencyKey] = true
		effects++ // the (idempotent) side effect
		return Ack, nil
	}

	// First Consume: effect runs, ack is dropped -> the message is immediately redeliverable.
	if _, err := q.Consume(ctx, 1, handler); err != nil {
		t.Fatalf("Consume 1: %v", err)
	}
	// Second Consume: the redelivered message hits the same key -> no second effect, and the ack sticks.
	if _, err := q.Consume(ctx, 1, handler); err != nil {
		t.Fatalf("Consume 2: %v", err)
	}
	if effects != 1 {
		t.Fatalf("effects = %d, want 1 (a redelivered message must run the effect exactly once)", effects)
	}
	d, _ := q.Depth(ctx)
	if d.Ready != 0 || d.InFlight != 0 {
		t.Fatalf("after dedupe depth = %+v, want drained", d)
	}
}

// TestMemoryBackpressureFullDoesNotDrop pins backpressure (§34.4): a full queue rejects the producer with
// ErrQueueFull (the producer waits/retries) rather than silently dropping the message. Draining one frees
// exactly one slot.
func TestMemoryBackpressureFullDoesNotDrop(t *testing.T) {
	clk := newClock()
	q := NewMemory(MemoryConfig{Capacity: 2, Visibility: time.Minute, MaxDeliveries: 10}, clk.now)
	ctx := context.Background()

	if err := q.Publish(ctx, "a", []byte("1")); err != nil {
		t.Fatalf("Publish a: %v", err)
	}
	if err := q.Publish(ctx, "b", []byte("2")); err != nil {
		t.Fatalf("Publish b: %v", err)
	}
	// Third publish at capacity: backpressure, not a drop.
	if err := q.Publish(ctx, "c", []byte("3")); err != ErrQueueFull {
		t.Fatalf("Publish c err = %v, want ErrQueueFull (full queue applies backpressure, never drops)", err)
	}
	d, _ := q.Depth(ctx)
	if d.Ready != 2 {
		t.Fatalf("ready = %d, want 2 (the two enqueued survive; nothing was dropped)", d.Ready)
	}

	// Drain one, and the shed producer now succeeds.
	if _, err := q.Consume(ctx, 1, func(_ context.Context, _ Message) (Disposition, error) { return Ack, nil }); err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if err := q.Publish(ctx, "c", []byte("3")); err != nil {
		t.Fatalf("Publish c after drain: %v, want success (a freed slot admits the waiting producer)", err)
	}
}

// TestMemoryDeadLettersAfterMaxDeliveries pins dead-letter (§34.3): a message that fails MaxDeliveries
// times stops redelivering and moves to the dead-letter view — a poison message never loops forever.
func TestMemoryDeadLettersAfterMaxDeliveries(t *testing.T) {
	clk := newClock()
	q := NewMemory(MemoryConfig{Capacity: 8, Visibility: time.Minute, MaxDeliveries: 3}, clk.now)
	ctx := context.Background()

	if err := q.Publish(ctx, "poison", []byte("bad")); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	handler := func(_ context.Context, _ Message) (Disposition, error) { return Retry, nil } // never succeeds

	// Deliver until it dead-letters. MaxDeliveries=3 means attempts 1..3 deliver; the 4th lease dead-letters.
	for i := 0; i < 6; i++ {
		if _, err := q.Consume(ctx, 1, handler); err != nil {
			t.Fatalf("Consume: %v", err)
		}
		clk.advance(time.Minute + time.Second)
	}
	d, _ := q.Depth(ctx)
	if d.Dead != 1 {
		t.Fatalf("dead = %d, want 1 (a message past MaxDeliveries dead-letters)", d.Dead)
	}
	if d.Ready != 0 || d.InFlight != 0 {
		t.Fatalf("after dead-letter depth = %+v, want no live copy (poison stops redelivering)", d)
	}
}
