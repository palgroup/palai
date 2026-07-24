package automation

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/adapters/integrations/queue"
	"github.com/palgroup/palai/storage"
)

// QueueStore is the Postgres-durable reference queue adapter (E17 Task 7, spec §34.1-34.5): the queue
// tables ARE the broker, so a crash before ack survives and redelivers (the un-acked lease expires and
// re-leases). It provides the inbound consumer queue (PGQueue), the append-only idempotency ledger
// (RecordEffect), and the outbound result-delivery outbox (PGOutbox). A real SQS/PubSub/Kafka adapter
// implements the SAME queue.InboundQueue / queue.Sink contract and is the operator leg (§6).
type QueueStore struct {
	pool *pgxpool.Pool
}

// NewQueueStore wraps the shared connection pool.
func NewQueueStore(pool *pgxpool.Pool) *QueueStore { return &QueueStore{pool: pool} }

// execer is the shared subset of *pgxpool.Pool and pgx.Tx, so RecordEffect can run inside the caller's
// effect transaction (atomic dedupe) or standalone against the pool.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// QueueConnectionInput is the resolved create body for a queue binding (§34.1).
type QueueConnectionInput struct {
	Name          string
	Kind          string // "local" (the reference); a real "sqs"/"pubsub"/"kafka" is the operator leg
	Direction     string // "inbound" | "outbound"
	Capacity      int
	Visibility    time.Duration
	MaxDeliveries int
	Config        []byte // JSON; secret material lives in secret_refs, never here
}

func (in QueueConnectionInput) withDefaults() QueueConnectionInput {
	if in.Kind == "" {
		in.Kind = "local"
	}
	if in.Direction == "" {
		in.Direction = "inbound"
	}
	if in.Capacity <= 0 {
		in.Capacity = 1024
	}
	if in.Visibility <= 0 {
		in.Visibility = 30 * time.Second
	}
	if in.MaxDeliveries <= 0 {
		in.MaxDeliveries = 20
	}
	if len(in.Config) == 0 {
		in.Config = []byte("{}")
	}
	return in
}

// CreateConnection registers a queue binding in the verified scope and returns its server-minted id.
func (s *QueueStore) CreateConnection(ctx context.Context, org, project string, in QueueConnectionInput) (string, error) {
	in = in.withDefaults()
	ctx = storage.ScopeToTenant(ctx, org, project)
	id := newID("qconn")
	var out string
	err := s.pool.QueryRow(ctx, storage.Query("CreateQueueConnection"),
		id, org, project, in.Name, in.Kind, in.Direction,
		in.Capacity, int(in.Visibility.Seconds()), in.MaxDeliveries, string(in.Config)).Scan(&out)
	return out, err
}

// queueConn holds a connection's resolved tuning knobs, loaded once so the hot Publish/Consume path does
// not re-read them.
type queueConn struct {
	id            string
	org, project  string
	capacity      int
	visibility    time.Duration
	maxDeliveries int
}

func (s *QueueStore) loadConn(ctx context.Context, org, project, connID string) (queueConn, error) {
	ctx = storage.ScopeToTenant(ctx, org, project)
	var c queueConn
	var name, kind, direction string
	var enabled bool
	var visSecs int
	if err := s.pool.QueryRow(ctx, storage.Query("GetQueueConnection"), connID, org, project).Scan(
		&c.id, &c.org, &c.project, &name, &kind, &direction, &c.capacity, &visSecs, &c.maxDeliveries, &enabled,
	); err != nil {
		return queueConn{}, fmt.Errorf("load queue connection %s: %w", connID, err)
	}
	if !enabled {
		return queueConn{}, fmt.Errorf("queue connection %s is disabled", connID)
	}
	c.visibility = time.Duration(visSecs) * time.Second
	return c, nil
}

// --- inbound consumer queue ---

// PGQueue implements queue.InboundQueue over one durable queue_connections binding.
type PGQueue struct {
	store *QueueStore
	conn  queueConn
}

// InboundQueue opens the durable consumer queue for a connection.
func (s *QueueStore) InboundQueue(ctx context.Context, org, project, connID string) (*PGQueue, error) {
	c, err := s.loadConn(ctx, org, project, connID)
	if err != nil {
		return nil, err
	}
	return &PGQueue{store: s, conn: c}, nil
}

// Publish enqueues a message, applying backpressure: at capacity it returns queue.ErrQueueFull instead of
// growing the backlog without bound (§34.4). ON CONFLICT DO NOTHING makes a producer's at-least-once
// double-publish a silent dedupe. ponytail: the load check and the insert are separate statements, so two
// concurrent publishers can each pass the check and overshoot capacity by a bounded amount — a real broker
// enforces the ceiling exactly; a SELECT ... FOR UPDATE on a per-connection gauge row removes the race.
func (q *PGQueue) Publish(ctx context.Context, idempotencyKey string, body []byte) error {
	ctx = storage.ScopeToTenant(ctx, q.conn.org, q.conn.project)
	var load int
	if err := q.store.pool.QueryRow(ctx, storage.Query("QueueLoad"), q.conn.id).Scan(&load); err != nil {
		return fmt.Errorf("queue load: %w", err)
	}
	if load >= q.conn.capacity {
		return queue.ErrQueueFull
	}
	if _, err := q.store.pool.Exec(ctx, storage.Query("EnqueueQueueMessage"),
		newID("qmsg"), q.conn.org, q.conn.project, q.conn.id, idempotencyKey, body); err != nil {
		return fmt.Errorf("enqueue queue message: %w", err)
	}
	return nil
}

// Consume dead-letters any exhausted messages, leases up to max deliverable ones (ready, or leased with an
// expired lease = a crash-before-ack redelivery), runs the Handler on each, and applies its Disposition.
// The ack is a SEPARATE statement AFTER the Handler returns Ack, so a crash between the effect and the ack
// redelivers the message — the Handler's idempotency (RecordEffect) makes that redelivery a single effect.
func (q *PGQueue) Consume(ctx context.Context, max int, h queue.Handler) (int, error) {
	ctx = storage.ScopeToTenant(ctx, q.conn.org, q.conn.project)
	if _, err := q.store.pool.Exec(ctx, storage.Query("QueueDeadLetterExhausted"), q.conn.id, q.conn.maxDeliveries); err != nil {
		return 0, fmt.Errorf("dead-letter exhausted: %w", err)
	}
	rows, err := q.store.pool.Query(ctx, storage.Query("LeaseQueueMessages"),
		q.conn.id, max, q.conn.visibility.Seconds(), q.conn.maxDeliveries)
	if err != nil {
		return 0, fmt.Errorf("lease queue messages: %w", err)
	}
	var leased []queue.Message
	for rows.Next() {
		var m queue.Message
		if err := rows.Scan(&m.Handle, &m.IdempotencyKey, &m.Body, &m.Attempt); err != nil {
			rows.Close()
			return 0, err
		}
		leased = append(leased, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	handled := 0
	for _, m := range leased {
		disp, herr := h(ctx, m)
		switch {
		case herr == nil && disp == queue.Ack:
			if _, err := q.store.pool.Exec(ctx, storage.Query("AckQueueMessage"), m.Handle); err != nil {
				return handled, fmt.Errorf("ack queue message: %w", err)
			}
		case disp == queue.DeadLetter:
			if _, err := q.store.pool.Exec(ctx, storage.Query("DeadLetterQueueMessage"), m.Handle); err != nil {
				return handled, fmt.Errorf("dead-letter queue message: %w", err)
			}
		default:
			// Retry (or a Handler error): leave the message leased. Its visibility lease expires and it
			// redelivers on a later Consume, counting toward the dead-letter bound.
		}
		handled++
	}
	return handled, nil
}

// Depth reports the backlog gauge (§34.4): ready, in-flight, dead, and the oldest ready age.
func (q *PGQueue) Depth(ctx context.Context) (queue.Depth, error) {
	ctx = storage.ScopeToTenant(ctx, q.conn.org, q.conn.project)
	var d queue.Depth
	var oldestSecs int64
	if err := q.store.pool.QueryRow(ctx, storage.Query("QueueDepth"), q.conn.id).Scan(
		&d.Ready, &d.InFlight, &d.Dead, &oldestSecs); err != nil {
		return queue.Depth{}, fmt.Errorf("queue depth: %w", err)
	}
	d.OldestAge = time.Duration(oldestSecs) * time.Second
	return d, nil
}

// RecordEffect inserts the append-only idempotency receipt for (connection, key). fresh=true means the
// receipt was newly written and the effect must run; false means the effect already committed for this key
// (a lost-ack redelivery), so the Handler skips the effect and Acks. Pass the caller's effect transaction
// as db so the receipt commits ATOMICALLY with the side effect; a redelivery then cannot observe a
// committed effect without its receipt.
func (s *QueueStore) RecordEffect(ctx context.Context, db execer, org, project, connID, idempotencyKey string) (bool, error) {
	tag, err := db.Exec(ctx, storage.Query("RecordQueueEffect"), newID("qrcpt"), org, project, connID, idempotencyKey)
	if err != nil {
		return false, fmt.Errorf("record queue effect: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// --- outbound result-delivery outbox ---

// PGOutbox implements the durable outbound result delivery (§34.5) over queue_deliveries, modeled on the
// webhook pump: a result is enqueued durably BEFORE any attempt, then delivered to the sink with retry +
// dead-letter, so a publisher-down window never loses it.
type PGOutbox struct {
	store *QueueStore
	conn  queueConn
}

// Outbox opens the outbound outbox for a connection.
func (s *QueueStore) Outbox(ctx context.Context, org, project, connID string) (*PGOutbox, error) {
	c, err := s.loadConn(ctx, org, project, connID)
	if err != nil {
		return nil, err
	}
	return &PGOutbox{store: s, conn: c}, nil
}

// Enqueue durably records a result for delivery. fresh=false means this destination_key was already
// enqueued (a double-enqueue of the same result collapses). The row commits before any delivery attempt,
// so a crash here loses nothing — DeliverDue picks it up.
func (o *PGOutbox) Enqueue(ctx context.Context, destinationKey string, payload []byte, maxAttempts int) (bool, error) {
	if maxAttempts <= 0 {
		maxAttempts = 20
	}
	ctx = storage.ScopeToTenant(ctx, o.conn.org, o.conn.project)
	tag, err := o.store.pool.Exec(ctx, storage.Query("EnqueueQueueDelivery"),
		newID("qdel"), o.conn.org, o.conn.project, o.conn.id, destinationKey, payload, maxAttempts)
	if err != nil {
		return false, fmt.Errorf("enqueue queue delivery: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// DeliverDue attempts each due pending delivery once against sink, applying retry (backoff) and
// dead-letter (after max_attempts). It returns the number newly delivered. A delivery that fails stays
// pending (durable) for a later tick — loss-less. Destination idempotency (the destination_key) means a
// redelivered key the sink already saw is a single effect. ponytail: DueQueueDeliveries FOR UPDATE SKIP
// LOCKED holds its lock only for the SELECT (the UPDATEs run in separate statements), so this is safe for
// a SINGLE pump; a concurrent-pump deployment wraps lease+process in one transaction.
func (o *PGOutbox) DeliverDue(ctx context.Context, sink queue.Sink, max int, backoff time.Duration) (int, error) {
	ctx = storage.ScopeToTenant(ctx, o.conn.org, o.conn.project)
	rows, err := o.store.pool.Query(ctx, storage.Query("DueQueueDeliveries"), o.conn.id, max)
	if err != nil {
		return 0, fmt.Errorf("due queue deliveries: %w", err)
	}
	type due struct {
		id, connID, destKey string
		payload             []byte
		attempt, maxAtt     int
	}
	var batch []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.id, &d.connID, &d.destKey, &d.payload, &d.attempt, &d.maxAtt); err != nil {
			rows.Close()
			return 0, err
		}
		batch = append(batch, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	delivered := 0
	for _, d := range batch {
		attempt := d.attempt + 1
		derr := sink.Deliver(ctx, d.destKey, d.payload)
		switch {
		case derr == nil:
			if _, err := o.store.pool.Exec(ctx, storage.Query("MarkQueueDeliveryDelivered"), d.id, attempt); err != nil {
				return delivered, err
			}
			delivered++
		case attempt >= d.maxAtt:
			if _, err := o.store.pool.Exec(ctx, storage.Query("MarkQueueDeliveryDead"), d.id, attempt); err != nil {
				return delivered, err
			}
		default:
			next := time.Now().Add(backoff)
			if _, err := o.store.pool.Exec(ctx, storage.Query("RescheduleQueueDelivery"), d.id, attempt, next); err != nil {
				return delivered, err
			}
		}
	}
	return delivered, nil
}
