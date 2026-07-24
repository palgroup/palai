-- Queue-adapter queries (E17 Task 7, spec §34.1-34.5). The Postgres-durable reference queue: an inbound
-- consumer queue with lease/visibility redelivery + dead-letter, an append-only idempotency ledger, and
-- the outbound result-delivery outbox. Loaded by name via storage.Query (yesql-style "-- name:" blocks).

-- name: CreateQueueConnection
INSERT INTO queue_connections
    (id, organization_id, project_id, name, kind, direction, capacity, visibility_seconds, max_deliveries, config)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb)
RETURNING id;

-- name: GetQueueConnection
SELECT id, organization_id, project_id, name, kind, direction, capacity, visibility_seconds, max_deliveries, enabled
  FROM queue_connections
 WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- EnqueueQueueMessage inserts a message; the UNIQUE(queue_connection_id, idempotency_key) collapses a
-- producer's at-least-once double-publish (ON CONFLICT DO NOTHING -> RowsAffected()==0 signals a dedupe).
-- name: EnqueueQueueMessage
INSERT INTO queue_messages
    (id, organization_id, project_id, queue_connection_id, idempotency_key, body)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (queue_connection_id, idempotency_key) DO NOTHING;

-- QueueLoad is the backpressure gauge input: ready + in-flight (leased) messages count against capacity.
-- name: QueueLoad
SELECT count(*)
  FROM queue_messages
 WHERE queue_connection_id = $1 AND state IN ('ready', 'leased');

-- QueueDepth is the observability gauge (§34.4): ready backlog, in-flight, dead, and the oldest ready age.
-- name: QueueDepth
SELECT
    count(*) FILTER (WHERE state = 'ready' OR (state = 'leased' AND lease_expires_at < clock_timestamp())) AS ready,
    count(*) FILTER (WHERE state = 'leased' AND lease_expires_at >= clock_timestamp())                    AS inflight,
    count(*) FILTER (WHERE state = 'dead')                                                                 AS dead,
    COALESCE(EXTRACT(EPOCH FROM (clock_timestamp() - min(enqueued_at) FILTER (
        WHERE state = 'ready' OR (state = 'leased' AND lease_expires_at < clock_timestamp())))), 0)::bigint AS oldest_age_seconds
  FROM queue_messages
 WHERE queue_connection_id = $1;

-- QueueDeadLetterExhausted moves messages that have been delivered max_deliveries times without an ack to
-- the dead-letter state, BEFORE the next lease scan — so a poison message stops redelivering (§34.3).
-- name: QueueDeadLetterExhausted
UPDATE queue_messages
   SET state = 'dead', updated_at = clock_timestamp()
 WHERE queue_connection_id = $1
   AND state IN ('ready', 'leased')
   AND attempt_count >= $2
   AND (state = 'ready' OR lease_expires_at < clock_timestamp());

-- LeaseQueueMessages atomically leases up to $2 deliverable messages (ready, or leased with an expired
-- lease = a crash-before-ack redelivery), incrementing attempt_count and setting the visibility deadline.
-- FOR UPDATE SKIP LOCKED lets concurrent consumers take disjoint batches. It leases only messages still
-- under the dead-letter bound ($4 = max_deliveries); QueueDeadLetterExhausted retires the rest first.
-- name: LeaseQueueMessages
WITH deliverable AS (
    SELECT id
      FROM queue_messages
     WHERE queue_connection_id = $1
       AND attempt_count < $4
       AND (state = 'ready' OR (state = 'leased' AND lease_expires_at < clock_timestamp()))
     ORDER BY enqueued_at
     FOR UPDATE SKIP LOCKED
     LIMIT $2
)
UPDATE queue_messages m
   SET state = 'leased',
       attempt_count = m.attempt_count + 1,
       lease_expires_at = clock_timestamp() + make_interval(secs => $3),
       updated_at = clock_timestamp()
  FROM deliverable
 WHERE m.id = deliverable.id
RETURNING m.id, m.idempotency_key, m.body, m.attempt_count;

-- AckQueueMessage retires a leased message. The `state = 'leased'` fence stops a zombie consumer whose
-- lease already expired from acking a row a NEW consumer has since re-leased: the stale ack no-ops
-- (RowsAffected()==0) instead of retiring another consumer's in-flight work.
-- name: AckQueueMessage
UPDATE queue_messages
   SET state = 'acked', updated_at = clock_timestamp()
 WHERE id = $1 AND state = 'leased';

-- DeadLetterQueueMessage moves a leased poison message to dead. Same lease fence as Ack: a zombie consumer
-- must not mark DEAD a message a new consumer has re-leased (whose effect may already be committing).
-- name: DeadLetterQueueMessage
UPDATE queue_messages
   SET state = 'dead', updated_at = clock_timestamp()
 WHERE id = $1 AND state = 'leased';

-- RecordQueueEffect inserts the append-only idempotency receipt IN THE SAME TRANSACTION as the effect.
-- ON CONFLICT DO NOTHING -> RowsAffected()==0 means the effect already committed for this key (a lost-ack
-- redelivery), so the caller skips the effect and acks.
-- name: RecordQueueEffect
INSERT INTO queue_effect_receipts
    (id, organization_id, project_id, queue_connection_id, idempotency_key)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (queue_connection_id, idempotency_key) DO NOTHING;

-- name: CountQueueEffects
SELECT count(*) FROM queue_effect_receipts
 WHERE queue_connection_id = $1 AND idempotency_key = $2;

-- EnqueueQueueDelivery durably records an outbound result BEFORE any delivery attempt (loss-less, §34.5).
-- UNIQUE(queue_connection_id, destination_key) collapses a double-enqueue of the same result.
-- name: EnqueueQueueDelivery
INSERT INTO queue_deliveries
    (id, organization_id, project_id, queue_connection_id, destination_key, payload, max_attempts)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (queue_connection_id, destination_key) DO NOTHING;

-- DueQueueDeliveries returns pending outbound deliveries whose backoff clock has elapsed. DeliverDue runs
-- this as a standalone auto-commit SELECT, so the FOR UPDATE SKIP LOCKED row locks die at statement end and
-- do NOT reserve the rows for the pump — the rows stay 'pending' for the next scan. This is safe for a
-- SINGLE pump only; a concurrent-pump deploy MUST hold the lease and the process/UPDATE in ONE transaction
-- (FOR UPDATE spanning the mutation), or two pumps read the same due row and double-send it.
-- name: DueQueueDeliveries
SELECT id, queue_connection_id, destination_key, payload, attempt_count, max_attempts
  FROM queue_deliveries
 WHERE queue_connection_id = $1 AND state = 'pending' AND next_attempt_at <= clock_timestamp()
 ORDER BY next_attempt_at
 FOR UPDATE SKIP LOCKED
 LIMIT $2;

-- name: MarkQueueDeliveryDelivered
UPDATE queue_deliveries
   SET state = 'delivered', attempt_count = $2, updated_at = clock_timestamp(),
       first_attempt_at = COALESCE(first_attempt_at, clock_timestamp())
 WHERE id = $1;

-- name: RescheduleQueueDelivery
UPDATE queue_deliveries
   SET attempt_count = $2, next_attempt_at = $3, updated_at = clock_timestamp(),
       first_attempt_at = COALESCE(first_attempt_at, clock_timestamp())
 WHERE id = $1;

-- name: MarkQueueDeliveryDead
UPDATE queue_deliveries
   SET state = 'dead', attempt_count = $2, updated_at = clock_timestamp(),
       first_attempt_at = COALESCE(first_attempt_at, clock_timestamp())
 WHERE id = $1;

-- name: GetQueueDelivery
SELECT id, state, attempt_count FROM queue_deliveries
 WHERE id = $1 AND organization_id = $2 AND project_id = $3;
