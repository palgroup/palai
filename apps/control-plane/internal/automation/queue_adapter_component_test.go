//go:build component

// Component test for the E17 T7 queue adapter against REAL PostgreSQL (migration 000037). It proves the
// durable-delivery contract the reference adapter must hold: a crash before ack redelivers (never loses)
// an un-acked message; a redelivered message runs its effect ONCE (the append-only idempotency ledger);
// a full queue applies backpressure instead of dropping; a message that fails N times dead-letters; and
// an outbound result survives a publisher-down window + a process restart and is delivered exactly once.
//
// These are the AUT-009 (redelivery-no-duplicate), AUT-010 (flood backpressure), and AUT-013
// (idempotency-key -> single effect) QUEUE proof legs — the SAME crux the webhook inbound seam proves,
// now via the queue adapter. All durability is deterministic (a forced lease-expiry, no sleeps, no real
// broker); real SQS/PubSub/Kafka are the operator leg (§6).
package automation

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/adapters/integrations/queue"
	"github.com/palgroup/palai/storage"
)

func mustCreateQueueConn(t *testing.T, store *QueueStore, org, project string, in QueueConnectionInput) string {
	t.Helper()
	id, err := store.CreateConnection(context.Background(), org, project, in)
	if err != nil {
		t.Fatalf("CreateConnection error = %v", err)
	}
	return id
}

// expireLease forces every leased message on a connection to have an expired visibility lease, WITHOUT
// sleeping — it models "time passed after the consumer crashed" so a restarted consumer re-leases the
// un-acked message. It runs under the system scope (a cross-tenant fixture write), the automation
// component idiom.
func expireLease(t *testing.T, pool *pgxpool.Pool, connID string) {
	t.Helper()
	mustExec(t, pool,
		`UPDATE queue_messages SET lease_expires_at = clock_timestamp() - interval '1 hour'
		  WHERE queue_connection_id = $1 AND state = 'leased'`, connID)
}

// TestQueueAdapterRedeliversAfterLostAckSingleEffect is the AUT-009 / AUT-013 queue leg: a message whose
// effect committed but whose ack was lost (a crash) redelivers — it is NOT lost — and the append-only
// idempotency ledger makes the redelivery a single effect. At-least-once delivery + an idempotency key =
// effectively-once effect, over real Postgres.
func TestQueueAdapterRedeliversAfterLostAckSingleEffect(t *testing.T) {
	pool := componentPool(t)
	store := NewQueueStore(pool)
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)
	connID := mustCreateQueueConn(t, store, org, project,
		QueueConnectionInput{Name: "orders", Visibility: time.Minute, MaxDeliveries: 10})

	q, err := store.InboundQueue(ctx, org, project, connID)
	if err != nil {
		t.Fatalf("InboundQueue error = %v", err)
	}
	if err := q.Publish(ctx, "order-42", []byte("charge")); err != nil {
		t.Fatalf("Publish error = %v", err)
	}

	effects := 0
	effect := func(after queue.Disposition) queue.Handler {
		return func(ctx context.Context, m queue.Message) (queue.Disposition, error) {
			// The idempotency ledger is the dedupe anchor: fresh means this key's effect has not committed.
			fresh, err := store.RecordEffect(ctx, pool, org, project, connID, m.IdempotencyKey)
			if err != nil {
				return queue.Retry, err
			}
			if fresh {
				effects++ // the (idempotent) side effect runs exactly once per key
			}
			return after, nil
		}
	}

	// First delivery: the effect commits, but the ack is LOST (a crash) — modeled by returning Retry, so
	// Consume does NOT ack; the message stays leased.
	if n, err := q.Consume(ctx, 10, effect(queue.Retry)); err != nil || n != 1 {
		t.Fatalf("first Consume = (%d, %v), want (1, nil)", n, err)
	}
	// The crashed consumer's lease expires; a restarted consumer re-leases the un-acked message.
	expireLease(t, pool, connID)

	// Redelivery: the key's receipt already exists -> the effect does NOT run again; this time it acks.
	if n, err := q.Consume(ctx, 10, effect(queue.Ack)); err != nil || n != 1 {
		t.Fatalf("redelivery Consume = (%d, %v), want (1, nil)", n, err)
	}

	if effects != 1 {
		t.Fatalf("effects = %d, want 1 (a redelivered message must run the effect exactly once)", effects)
	}
	d, err := q.Depth(ctx)
	if err != nil {
		t.Fatalf("Depth error = %v", err)
	}
	if d.Ready != 0 || d.InFlight != 0 {
		t.Fatalf("depth = %+v, want drained (the message was acked, never lost)", d)
	}
	// The durable idempotency ledger holds exactly one receipt for the key.
	var receipts int
	if err := pool.QueryRow(storage.ScopeToTenant(ctx, org, project),
		storage.Query("CountQueueEffects"), connID, "order-42").Scan(&receipts); err != nil {
		t.Fatalf("CountQueueEffects error = %v", err)
	}
	if receipts != 1 {
		t.Fatalf("durable receipts = %d, want 1", receipts)
	}
}

// TestQueueAdapterFloodAppliesBackpressureNoDrop is the AUT-010 queue leg: at capacity the producer is
// shed with ErrQueueFull (it waits/retries) rather than the queue silently dropping the message, and the
// backlog gauge stays visible. Draining one frees exactly one slot.
func TestQueueAdapterFloodAppliesBackpressureNoDrop(t *testing.T) {
	pool := componentPool(t)
	store := NewQueueStore(pool)
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)
	connID := mustCreateQueueConn(t, store, org, project,
		QueueConnectionInput{Name: "flood", Capacity: 3, Visibility: time.Minute, MaxDeliveries: 10})

	q, err := store.InboundQueue(ctx, org, project, connID)
	if err != nil {
		t.Fatalf("InboundQueue error = %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := q.Publish(ctx, fmt.Sprintf("m-%d", i), []byte("x")); err != nil {
			t.Fatalf("Publish m-%d error = %v", i, err)
		}
	}
	// Fourth publish at capacity: backpressure, not a drop.
	if err := q.Publish(ctx, "m-overflow", []byte("x")); err != queue.ErrQueueFull {
		t.Fatalf("overflow Publish err = %v, want ErrQueueFull (a full queue applies backpressure, never drops)", err)
	}
	d, err := q.Depth(ctx)
	if err != nil {
		t.Fatalf("Depth error = %v", err)
	}
	if d.Ready != 3 {
		t.Fatalf("ready = %d, want 3 (the enqueued messages survive; nothing dropped)", d.Ready)
	}
	if d.OldestAge < 0 {
		t.Fatalf("oldest age = %v, want a non-negative backlog age", d.OldestAge)
	}

	// Drain (ack) one, and the shed producer now fits.
	ack := func(ctx context.Context, _ queue.Message) (queue.Disposition, error) { return queue.Ack, nil }
	if n, err := q.Consume(ctx, 1, ack); err != nil || n != 1 {
		t.Fatalf("Consume = (%d, %v), want (1, nil)", n, err)
	}
	if err := q.Publish(ctx, "m-overflow", []byte("x")); err != nil {
		t.Fatalf("Publish after drain error = %v, want success (a freed slot admits the waiting producer)", err)
	}
}

// TestQueueAdapterDeadLettersPoison pins dead-letter (§34.3): a message that fails MaxDeliveries times
// stops redelivering and moves to the dead-letter view — a poison message never loops forever.
func TestQueueAdapterDeadLettersPoison(t *testing.T) {
	pool := componentPool(t)
	store := NewQueueStore(pool)
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)
	connID := mustCreateQueueConn(t, store, org, project,
		QueueConnectionInput{Name: "poison", Visibility: time.Minute, MaxDeliveries: 3})

	q, err := store.InboundQueue(ctx, org, project, connID)
	if err != nil {
		t.Fatalf("InboundQueue error = %v", err)
	}
	if err := q.Publish(ctx, "poison", []byte("bad")); err != nil {
		t.Fatalf("Publish error = %v", err)
	}
	retry := func(ctx context.Context, _ queue.Message) (queue.Disposition, error) { return queue.Retry, nil }

	// Deliver+expire until the dead-letter bound (3) is reached, then one more Consume retires it.
	for i := 0; i < 5; i++ {
		if _, err := q.Consume(ctx, 10, retry); err != nil {
			t.Fatalf("Consume error = %v", err)
		}
		expireLease(t, pool, connID)
	}
	d, err := q.Depth(ctx)
	if err != nil {
		t.Fatalf("Depth error = %v", err)
	}
	if d.Dead != 1 {
		t.Fatalf("dead = %d, want 1 (a message past MaxDeliveries dead-letters)", d.Dead)
	}
	if d.Ready != 0 || d.InFlight != 0 {
		t.Fatalf("after dead-letter depth = %+v, want no live copy (poison stops redelivering)", d)
	}
}

// recordingSink is a test Sink that records every destination key it is handed and can be toggled "down"
// (it still RECEIVES the delivery — modeling a publisher that got the message but could not ack — then
// errors, so the outbox retries the SAME destination key). unique() proves destination idempotency.
type recordingSink struct {
	mu       sync.Mutex
	down     bool
	received []string
}

func (s *recordingSink) Deliver(_ context.Context, destKey string, _ []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.received = append(s.received, destKey)
	if s.down {
		return fmt.Errorf("sink down")
	}
	return nil
}

func (s *recordingSink) total() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.received) }
func (s *recordingSink) unique() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	set := map[string]bool{}
	for _, k := range s.received {
		set[k] = true
	}
	return len(set)
}

// TestQueueOutboxDeliversLosslesslyExactlyOnce is the §34.5 outbound result-delivery proof: a result is
// enqueued durably BEFORE any attempt, so it survives the publisher being down AND a process restart, and
// is delivered exactly once — the destination idempotency key collapses the at-least-once retry to a
// single logical delivery.
func TestQueueOutboxDeliversLosslesslyExactlyOnce(t *testing.T) {
	pool := componentPool(t)
	store := NewQueueStore(pool)
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)
	connID := mustCreateQueueConn(t, store, org, project,
		QueueConnectionInput{Name: "results", Direction: "outbound"})

	outbox, err := store.Outbox(ctx, org, project, connID)
	if err != nil {
		t.Fatalf("Outbox error = %v", err)
	}
	// The result commits durably before any delivery attempt (loss-less).
	fresh, err := outbox.Enqueue(ctx, "result-7", []byte("done"), 5)
	if err != nil || !fresh {
		t.Fatalf("Enqueue = (%v, %v), want (true, nil)", fresh, err)
	}

	sink := &recordingSink{down: true}
	// Publisher down: the delivery is attempted but stays pending — never lost.
	if n, err := outbox.DeliverDue(ctx, sink, 10, -time.Second); err != nil || n != 0 {
		t.Fatalf("DeliverDue while down = (%d, %v), want (0, nil)", n, err)
	}
	if got := queueDeliveryState(t, pool, org, project, "result-7", connID); got != "pending" {
		t.Fatalf("delivery state after down attempt = %q, want pending (durable, not lost)", got)
	}

	// A fresh Outbox instance proves the pending row is durable across a process restart.
	outbox2, err := store.Outbox(ctx, org, project, connID)
	if err != nil {
		t.Fatalf("Outbox (restart) error = %v", err)
	}
	sink.down = false // publisher recovers
	if n, err := outbox2.DeliverDue(ctx, sink, 10, -time.Second); err != nil || n != 1 {
		t.Fatalf("DeliverDue after recovery = (%d, %v), want (1, nil)", n, err)
	}
	if got := queueDeliveryState(t, pool, org, project, "result-7", connID); got != "delivered" {
		t.Fatalf("delivery state after recovery = %q, want delivered", got)
	}

	// Exactly once: the sink received the SAME destination key twice (down then up), but the key is unique
	// -> one logical delivery (destination idempotency), and no further ticks re-deliver it.
	if sink.unique() != 1 {
		t.Fatalf("unique destination keys delivered = %d, want 1 (single logical delivery)", sink.unique())
	}
	if sink.total() < 1 {
		t.Fatalf("sink received %d attempts, want at least 1", sink.total())
	}
	if n, err := outbox2.DeliverDue(ctx, sink, 10, -time.Second); err != nil || n != 0 {
		t.Fatalf("second DeliverDue = (%d, %v), want (0, nil) — a delivered result is not re-sent", n, err)
	}
}

// deliveryState reads the outbound delivery row's state by its destination key.
func queueDeliveryState(t *testing.T, pool *pgxpool.Pool, org, project, destKey, connID string) string {
	t.Helper()
	var state string
	if err := pool.QueryRow(storage.ScopeToTenant(context.Background(), org, project),
		`SELECT state FROM queue_deliveries WHERE queue_connection_id = $1 AND destination_key = $2`,
		connID, destKey).Scan(&state); err != nil {
		t.Fatalf("read delivery state error = %v", err)
	}
	return state
}
