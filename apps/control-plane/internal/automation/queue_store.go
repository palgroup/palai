package automation

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
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

// CreateQueueConnection is the api.QueueConnectionAPI create seam — CreateConnection under the name the
// HTTP surface's interface uses, so the admin route needs no adapter type.
func (s *QueueStore) CreateQueueConnection(ctx context.Context, project string, in QueueConnectionInput) (string, error) {
	return s.CreateConnection(ctx, project, in)
}

// CreateConnection registers a queue binding in the verified scope and returns its server-minted id.
// Organization is resolved fresh from project (A.2 Task 3): the request scope no longer carries one.
func (s *QueueStore) CreateConnection(ctx context.Context, project string, in QueueConnectionInput) (string, error) {
	in = in.withDefaults()
	ctx = storage.ScopeToTenant(ctx, project)
	id := newID("qconn")
	var out string
	err := s.pool.QueryRow(ctx, storage.Query("CreateQueueConnection"),
		id, project, in.Name, in.Kind, in.Direction,
		in.Capacity, int(in.Visibility.Seconds()), in.MaxDeliveries, string(in.Config)).Scan(&out)
	return out, err
}

// QueueConnectionItem is one row of the admin list: the binding's non-secret configuration. Config is
// included because it holds the run target an operator must be able to read back; secret material lives in
// secret_refs and the create surface strict-decodes config precisely so it cannot hold a credential.
type QueueConnectionItem struct {
	ID            string
	Name          string
	Kind          string
	Direction     string
	Capacity      int
	Visibility    int
	MaxDeliveries int
	Enabled       bool
	Config        json.RawMessage
	CreatedAt     time.Time
}

// ListQueueConnections returns a tenant-scoped page of queue bindings, newest-first.
func (s *QueueStore) ListQueueConnections(ctx context.Context, project string, w ListWindow) ([]QueueConnectionItem, error) {
	ctx = storage.ScopeToTenant(ctx, project)
	rows, err := s.pool.Query(ctx, storage.Query("ListQueueConnections"),
		project, w.CreatedGTE, w.CreatedLTE, w.AfterCreatedAt, w.AfterID, w.Limit)
	if err != nil {
		return nil, fmt.Errorf("list queue connections: %w", err)
	}
	defer rows.Close()
	var out []QueueConnectionItem
	for rows.Next() {
		it, err := scanQueueConnectionItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// GetQueueConnectionItem returns one connection in the SAME projection the list serves, or found=false for
// an unknown or foreign id (E29 T2: the address the create's 201 Location has always named). RLS confines
// the read to the caller's tenant, and the query's own org/project predicate is defence in depth behind it;
// a foreign id is therefore indistinguishable from an absent one, which is the intended answer.
func (s *QueueStore) GetQueueConnectionItem(ctx context.Context, project, connID string) (QueueConnectionItem, bool, error) {
	ctx = storage.ScopeToTenant(ctx, project)
	rows, err := s.pool.Query(ctx, storage.Query("GetQueueConnectionItem"), connID, project)
	if err != nil {
		return QueueConnectionItem{}, false, fmt.Errorf("get queue connection: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return QueueConnectionItem{}, false, rows.Err()
	}
	it, err := scanQueueConnectionItem(rows)
	if err != nil {
		return QueueConnectionItem{}, false, err
	}
	return it, true, rows.Err()
}

// scanQueueConnectionItem is the ONE place a connection row becomes an item. The list and the singular read
// share it so their projections cannot drift into disagreeing about what a connection is.
func scanQueueConnectionItem(rows pgx.Rows) (QueueConnectionItem, error) {
	var it QueueConnectionItem
	if err := rows.Scan(&it.ID, &it.Name, &it.Kind, &it.Direction, &it.Capacity, &it.Visibility,
		&it.MaxDeliveries, &it.Enabled, &it.Config, &it.CreatedAt); err != nil {
		return QueueConnectionItem{}, fmt.Errorf("scan queue connection row: %w", err)
	}
	return it, nil
}

// queueConn holds a connection's resolved tuning knobs, loaded once so the hot Publish/Consume path does
// not re-read them.
type queueConn struct {
	id            string
	project       string
	capacity      int
	visibility    time.Duration
	maxDeliveries int
	// config is the connection's own JSONB. On an inbound binding it carries the RUN TARGET the bridge
	// admits with (agent_revision_id + principal_id); on an outbound one, the destination. It is
	// connection-scoped configuration, never message content — see queue_bridge.go.
	config []byte
}

func (s *QueueStore) loadConn(ctx context.Context, project, connID string) (queueConn, error) {
	ctx = storage.ScopeToTenant(ctx, project)
	var c queueConn
	var name, kind, direction string
	var enabled bool
	var visSecs int
	if err := s.pool.QueryRow(ctx, storage.Query("GetQueueConnection"), connID, project).Scan(
		&c.id, &c.project, &name, &kind, &direction, &c.capacity, &visSecs, &c.maxDeliveries, &enabled, &c.config,
	); err != nil {
		return queueConn{}, fmt.Errorf("load queue connection %s: %w", connID, err)
	}
	if !enabled {
		return queueConn{}, fmt.Errorf("queue connection %s is disabled", connID)
	}
	c.visibility = time.Duration(visSecs) * time.Second
	return c, nil
}

// sweepConnections returns every ENABLED connection of one direction across tenants — the supervised
// bridge's catalogue scan. It runs under the SYSTEM scope because it spans tenants by construction (the
// webhook pump's FanOutEndpoints precedent); each row then carries the org/project that is the ONLY tenant
// the bridge does that row's work under. A disabled connection is filtered in SQL, so no caller can forget.
func (s *QueueStore) sweepConnections(ctx context.Context, direction string) ([]queueConn, error) {
	rows, err := s.pool.Query(storage.WithSystemScope(ctx), storage.Query("SweepQueueConnections"), direction)
	if err != nil {
		return nil, fmt.Errorf("sweep %s queue connections: %w", direction, err)
	}
	defer rows.Close()
	var out []queueConn
	for rows.Next() {
		var c queueConn
		var visSecs int
		if err := rows.Scan(&c.id, &c.project, &c.capacity, &visSecs, &c.maxDeliveries, &c.config); err != nil {
			return nil, err
		}
		c.visibility = time.Duration(visSecs) * time.Second
		out = append(out, c)
	}
	return out, rows.Err()
}

// inboundQueueFor opens a PGQueue over an already-swept connection row, WITHOUT re-reading it. The sweep's
// WHERE clause is the enabled check, so this cannot be handed a disabled binding.
func (s *QueueStore) inboundQueueFor(c queueConn) *PGQueue { return &PGQueue{store: s, conn: c} }

// outboxFor is inboundQueueFor's outbound twin.
func (s *QueueStore) outboxFor(c queueConn) *PGOutbox { return &PGOutbox{store: s, conn: c} }

// --- inbound consumer queue ---

// PGQueue implements queue.InboundQueue over one durable queue_connections binding.
type PGQueue struct {
	store *QueueStore
	conn  queueConn
}

// InboundQueue opens the durable consumer queue for a connection.
func (s *QueueStore) InboundQueue(ctx context.Context, project, connID string) (*PGQueue, error) {
	c, err := s.loadConn(ctx, project, connID)
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
	ctx = storage.ScopeToTenant(ctx, q.conn.project)
	var load int
	if err := q.store.pool.QueryRow(ctx, storage.Query("QueueLoad"), q.conn.id).Scan(&load); err != nil {
		return fmt.Errorf("queue load: %w", err)
	}
	if load >= q.conn.capacity {
		return queue.ErrQueueFull
	}
	if _, err := q.store.pool.Exec(ctx, storage.Query("EnqueueQueueMessage"),
		newID("qmsg"), q.conn.project, q.conn.id, idempotencyKey, body); err != nil {
		return fmt.Errorf("enqueue queue message: %w", err)
	}
	return nil
}

// Consume dead-letters any exhausted messages, leases up to max deliverable ones (ready, or leased with an
// expired lease = a crash-before-ack redelivery), runs the Handler on each, and applies its Disposition.
// The ack is a SEPARATE statement AFTER the Handler returns Ack, so a crash between the effect and the ack
// redelivers the message — the Handler's idempotency (RecordEffect) makes that redelivery a single effect.
func (q *PGQueue) Consume(ctx context.Context, max int, h queue.Handler) (int, error) {
	ctx = storage.ScopeToTenant(ctx, q.conn.project)
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
	ctx = storage.ScopeToTenant(ctx, q.conn.project)
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
func (s *QueueStore) RecordEffect(ctx context.Context, db execer, project, connID, idempotencyKey string) (bool, error) {
	tag, err := db.Exec(ctx, storage.Query("RecordQueueEffect"), newID("qrcpt"), project, connID, idempotencyKey)
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
func (s *QueueStore) Outbox(ctx context.Context, project, connID string) (*PGOutbox, error) {
	c, err := s.loadConn(ctx, project, connID)
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
	ctx = storage.ScopeToTenant(ctx, o.conn.project)
	tag, err := o.store.pool.Exec(ctx, storage.Query("EnqueueQueueDelivery"),
		newID("qdel"), o.conn.project, o.conn.id, destinationKey, payload, maxAttempts)
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
	ctx = storage.ScopeToTenant(ctx, o.conn.project)
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
