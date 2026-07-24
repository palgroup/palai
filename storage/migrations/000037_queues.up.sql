-- 000037 adds the queue-adapter tables (E17 Task 7, spec §34.1-34.5): a durable SQS/PubSub/Kafka-class
-- consumer queue, its append-only idempotency ledger, and the outbound result-delivery outbox. It follows
-- the SAME durable-delivery discipline as 000020's webhook tables (durable-record-before-ack, dedupe,
-- retry/dead-letter state) — the reference adapter is a Postgres-durable queue, so the tables ARE the
-- broker; a real SQS/PubSub/Kafka plugs in behind the same Go contract and is the operator leg (§6).
--
-- All CREATE ... IF NOT EXISTS, so the whole chain stays re-runnable. Four tables, each RLS'd: three carry
-- organization_id + project_id and take the standard tenant policy; queue_effect_receipts is append-only
-- (the idempotency ledger) and additionally re-asserts a REVOKE so the process can neither rewrite nor
-- erase a receipt — a deletable receipt would let a redelivered message re-run its effect.
--
-- M3 RULE (storage/migrations/000030): a new tenant-scoped table asserts its OWN policy here rather than
-- leaning on 000029's boot sweep; tests/security/tenancy fails a table that ships without ENABLE+FORCE.

-- A registered queue binding (§34.1). kind names the broker class ('local' = the Postgres reference; a real
-- 'sqs'/'pubsub'/'kafka' is the operator leg and is NOT advertised in discovery until wired). direction is
-- 'inbound' (a consumer queue) or 'outbound' (a result-delivery destination). The three tuning knobs shape
-- the reference queue: capacity (backpressure ceiling), visibility_seconds (lease timeout — a message not
-- acked within it redelivers), max_deliveries (dead-letter bound). config carries broker-specific settings;
-- secret material lives in secret_refs (000031), never here.
CREATE TABLE IF NOT EXISTS queue_connections (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    name TEXT NOT NULL,
    kind TEXT NOT NULL DEFAULT 'local',
    direction TEXT NOT NULL DEFAULT 'inbound'
        CHECK (direction IN ('inbound', 'outbound')),
    capacity INTEGER NOT NULL DEFAULT 1024 CHECK (capacity > 0),
    visibility_seconds INTEGER NOT NULL DEFAULT 30 CHECK (visibility_seconds > 0),
    max_deliveries INTEGER NOT NULL DEFAULT 20 CHECK (max_deliveries > 0),
    enabled BOOLEAN NOT NULL DEFAULT true,
    config JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id)
);

-- The durable inbound queue: one row per enqueued message. state moves ready -> leased -> acked (or ->
-- dead). A consumer leases a ready row (or one whose lease_expires_at has passed — a crash-before-ack
-- redelivery), runs the effect, then acks. UNIQUE(queue_connection_id, idempotency_key) dedupes an
-- at-least-once PRODUCER (a double-publish of the same logical message collapses to one row); the
-- consume-side lost-ack dedupe is the effect-receipts ledger below. attempt_count drives the dead-letter
-- bound. A retention sweep of acked/dead rows is a later concern.
-- ponytail: acked/dead rows are retained (auditable, and no DELETE grant needed); a retention sweep like
-- the trigger-delivery scrub trims them if the table grows.
CREATE TABLE IF NOT EXISTS queue_messages (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    queue_connection_id TEXT NOT NULL REFERENCES queue_connections (id) ON DELETE CASCADE,
    idempotency_key TEXT NOT NULL,
    body BYTEA NOT NULL,
    state TEXT NOT NULL DEFAULT 'ready'
        CHECK (state IN ('ready', 'leased', 'acked', 'dead')),
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    lease_expires_at TIMESTAMPTZ,
    enqueued_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id),
    UNIQUE (queue_connection_id, idempotency_key)
);

-- The lease scan (ready or lease-expired), ordered by enqueue for FIFO delivery.
CREATE INDEX IF NOT EXISTS queue_messages_deliverable_idx
    ON queue_messages (queue_connection_id, enqueued_at)
    WHERE state IN ('ready', 'leased');

-- Append-only idempotency ledger: one row per (connection, idempotency_key) whose effect has committed. A
-- consumer inserts it IN THE SAME TRANSACTION as the effect; a redelivery (lost ack) finds the row (23505)
-- and skips the effect, then acks. This is the load-bearing dedupe anchor for at-least-once consumers — so
-- it must be tamper-evident: a process that could DELETE a receipt could force an effect to re-run. Hence
-- the REVOKE below, self-re-asserting the same way usage_ledger (000032) and secret_refs (000031) do.
CREATE TABLE IF NOT EXISTS queue_effect_receipts (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    queue_connection_id TEXT NOT NULL REFERENCES queue_connections (id) ON DELETE CASCADE,
    idempotency_key TEXT NOT NULL,
    committed_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id),
    UNIQUE (queue_connection_id, idempotency_key)
);

-- The outbound result-delivery outbox (§34.5), modeled on 000020's webhook_deliveries: a run result is
-- enqueued here durably, and a publisher pump delivers it to the destination sink with retry + dead-letter.
-- Loss-less: the row commits before any delivery attempt, so a publisher-down window never loses a result.
-- destination_key is the destination idempotency key — UNIQUE(connection, destination_key) collapses a
-- double-enqueue, and the sink dedupes a redelivered key, so a result is delivered exactly once end to end.
CREATE TABLE IF NOT EXISTS queue_deliveries (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    queue_connection_id TEXT NOT NULL REFERENCES queue_connections (id) ON DELETE CASCADE,
    destination_key TEXT NOT NULL,
    payload BYTEA NOT NULL,
    state TEXT NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending', 'delivered', 'dead')),
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    max_attempts INTEGER NOT NULL DEFAULT 20 CHECK (max_attempts > 0),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    first_attempt_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id),
    UNIQUE (queue_connection_id, destination_key)
);

CREATE INDEX IF NOT EXISTS queue_deliveries_due_idx
    ON queue_deliveries (next_attempt_at) WHERE state = 'pending';

-- Each tenant table asserts its own policy (M3). All three carry project_id, so has_project=true; the CALL
-- is idempotent (the procedure DROPs+CREATEs the policy).
CALL palai_apply_tenant_policy('queue_connections', 'organization_id', true);
CALL palai_apply_tenant_policy('queue_messages', 'organization_id', true);
CALL palai_apply_tenant_policy('queue_effect_receipts', 'organization_id', true);
CALL palai_apply_tenant_policy('queue_deliveries', 'organization_id', true);

-- These tables were created AFTER 000029's blanket `GRANT ... ON ALL TABLES`, so that sweep never saw
-- them: a new table needs its own grant or the runtime role fails closed with "permission denied" instead
-- of the row-scoped policy. queue_messages/queue_deliveries need UPDATE (lease/ack/retry state); the
-- connections registry gets full CRUD; the effect ledger is append-only.
GRANT SELECT, INSERT, UPDATE, DELETE ON queue_connections TO palai_app;
GRANT SELECT, INSERT, UPDATE ON queue_messages, queue_deliveries TO palai_app;
GRANT SELECT, INSERT ON queue_effect_receipts TO palai_app;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO palai_app;

-- The load-bearing REVOKE (the usage_ledger/secret_refs precedent): 000001's and 000029's blanket
-- `GRANT ... ON ALL TABLES` re-run on every boot and — now that queue_effect_receipts EXISTS — re-hand
-- palai_app UPDATE/DELETE on it. 000037 runs AFTER both grants in the chain (37 > 29 > 1), and no later
-- migration re-grants this table or does an ALL-TABLES grant, so this REVOKE re-asserts every boot and
-- keeps the idempotency ledger append-only. A ledger the process can rewrite is not a ledger.
REVOKE UPDATE, DELETE ON queue_effect_receipts FROM palai_app;

INSERT INTO schema_migrations (version) VALUES (37) ON CONFLICT DO NOTHING;
