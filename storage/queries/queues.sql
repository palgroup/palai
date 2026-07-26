-- Queue-adapter queries (E17 Task 7, spec §34.1-34.5). The Postgres-durable reference queue: an inbound
-- consumer queue with lease/visibility redelivery + dead-letter, an append-only idempotency ledger, and
-- the outbound result-delivery outbox. Loaded by name via storage.Query (yesql-style "-- name:" blocks).

-- name: CreateQueueConnection
INSERT INTO queue_connections
    (id, organization_id, project_id, name, kind, direction, capacity, visibility_seconds, max_deliveries, config)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb)
RETURNING id;

-- name: GetQueueConnection
SELECT id, organization_id, project_id, name, kind, direction, capacity, visibility_seconds, max_deliveries, enabled, config
  FROM queue_connections
 WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- SweepQueueConnections is the BRIDGE's catalogue scan (E19 T6): every ENABLED connection of one direction,
-- across tenants, so the supervised consumer / outbox pump can tick them all. It runs under the SYSTEM scope
-- (a cross-tenant sweep by construction, the webhook-pump FanOutEndpoints idiom) and each returned row
-- carries its OWN org/project — which is then the ONLY tenant the bridge uses for that row's work. `enabled`
-- is in the WHERE, not a returned column: a disabled connection is not "returned and then filtered", it is
-- never selected, so no later code path can forget the check (§2, and the disabled-admits-nothing claim).
-- name: SweepQueueConnections
SELECT id, organization_id, project_id, capacity, visibility_seconds, max_deliveries, config
  FROM queue_connections
 WHERE direction = $1 AND enabled
 ORDER BY created_at;

-- ListQueueConnections is the tenant-scoped admin page (the ListMCPConnections keyset shape). It returns
-- NON-SECRET metadata only; `config` is returned because it holds the run target (agent_revision_id /
-- principal_id) an operator must be able to read back, and secret material lives in secret_refs, never here.
-- name: ListQueueConnections
SELECT id, name, kind, direction, capacity, visibility_seconds, max_deliveries, enabled, config, created_at
  FROM queue_connections
 WHERE organization_id = $1 AND project_id = $2
   AND ($3::timestamptz IS NULL OR created_at >= $3)
   AND ($4::timestamptz IS NULL OR created_at <= $4)
   AND ($5::timestamptz IS NULL OR (created_at, id) < ($5, $6))
 ORDER BY created_at DESC, id DESC
 LIMIT $7;

-- QueueRunPrincipalInScope confirms the principal named by an inbound connection's config belongs to the
-- connection's OWN org/project (the SlackRunPrincipalInScope twin, kept as its own name so the queue bridge
-- does not read a query the Slack surface owns). Without it a project admin could name a FOREIGN principal
-- and have queue-born runs booked against another tenant's identity: idempotency_records.principal_id is a
-- bare FK to principals(id), so the database alone would accept it.
--
-- The PRIMARY enforcement is RLS: the caller runs this under storage.ScopeToTenant, and principals is
-- FORCE-RLS, so a foreign row is not visible at all. The org/project predicate here is defence in depth —
-- verified by mutation: removing the predicate ALONE still refuses (RLS holds); removing the predicate AND
-- the tenant scope is what admits the foreign principal. Both layers are load-bearing together, and the
-- one that must never be dropped is the caller's scope.
-- name: QueueRunPrincipalInScope
SELECT 1 FROM principals WHERE id = $1 AND organization_id = $2 AND project_id = $3;

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

-- EnqueueTerminalQueueDeliveries is the OUTBOUND BRIDGE (E19 T6, §34.5): it runs INSIDE the run's terminal
-- transaction (applyRunTransitionTx), so the durable result row and the run's terminality commit TOGETHER.
-- That is the whole loss-lessness claim: there is no window in which a run is terminal and its result was
-- never recorded for delivery, however long the publisher stays down. The pump only ever DELIVERS what this
-- statement already committed.
--
-- Destination idempotency: destination_key is the run id and UNIQUE(queue_connection_id, destination_key)
-- + ON CONFLICT DO NOTHING make a second terminal write (a retried transaction, a reconciled cancel) ONE
-- delivery. The payload names the run's canonical coordinates ONLY — a consumer reads the content back
-- through /v1/responses under its own credential, so nothing here widens what a queue subscriber can see.
--
-- The tenant is NOT a parameter of the payload: org/project come from the connection row (c.*), which the
-- caller's tenant scope already confines via RLS. A project with no enabled outbound connection inserts
-- zero rows.
-- ponytail: an unindexed scan of queue_connections on every terminal transition. The table holds a handful
-- of rows per deployment, and RLS confines it to the tenant's own; a deployment with many connections wants
-- a partial index on (organization_id, project_id) WHERE direction='outbound' AND enabled — which is a
-- migration, and E19 takes none.
-- name: EnqueueTerminalQueueDeliveries
INSERT INTO queue_deliveries
    (id, organization_id, project_id, queue_connection_id, destination_key, payload, max_attempts)
SELECT 'qdel_' || replace(gen_random_uuid()::text, '-', ''),
       c.organization_id, c.project_id, c.id, $3,
       convert_to(jsonb_build_object(
           'type', 'run.terminal',
           'run_id', $3::text,
           'response_id', $4::text,
           'session_id', $5::text,
           'state', $6::text)::text, 'UTF8'),
       c.max_deliveries
  FROM queue_connections c
 WHERE c.organization_id = $1 AND c.project_id = $2 AND c.direction = 'outbound' AND c.enabled
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
