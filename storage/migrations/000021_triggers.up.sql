-- Triggers (spec §20.2.2, E11 Task 2): a versioned source-event → canonical-action binding plus the
-- TriggerDelivery record its ingestion advances through. Three tables. All CREATE ... IF NOT EXISTS, so
-- the whole chain stays re-runnable (the 000019/000020 pattern).
--
-- CONSCIOUS DECISION — trigger_revisions has NO publish flag (AGT-002, pin-at-accept). Unlike
-- agent_revisions (which publish, because they carry an externally-referenced published-contract
-- lifecycle), a trigger revision is internal: the ACTIVE revision is simply the highest revision_number,
-- and a delivery PINS trigger_revision_id at accept time. There is no draft/published lifecycle to model,
-- so a publish flag would be dead state. Revise = a new immutable INSERT (never an in-place UPDATE), so a
-- delivery already pinned to revision N is reproducible after an edit lands revision N+1.

-- A trigger lineage: a named source-event binding within a project. type enumerates the source families;
-- 'cron' and 'queue'/'integration_event' are enumerate-only here (their connectors land in E16/E17), so a
-- row may exist but only manual_api/webhook deliveries are wired in this task.
CREATE TABLE IF NOT EXISTS triggers (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    name TEXT NOT NULL,
    type TEXT NOT NULL DEFAULT 'manual_api'
        CHECK (type IN ('manual_api', 'webhook', 'cron', 'queue', 'integration_event')),
    enabled BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id),
    UNIQUE (organization_id, project_id, name)
);

-- An IMMUTABLE trigger revision (revise = a new INSERT; the config columns are never rewritten, the
-- 000019 discipline). It pins AT MOST ONE run target — an agent_revision_id (identity-carrying) OR a
-- run_template_revision_id (profile-free) — the exact pin a delivery flows through the §20.9 admission
-- path. input_mapping is the bounded declarative mapping language; dedupe_key_expr / correlation_key_expr
-- are exprs in that SAME language. concurrency_policy + correlation_mode drive the delivery pipeline.
--
-- T6-owned columns (callback execution + output shaping, spec §20.2.2): output_mapping and
-- callback_endpoint_id are pre-provisioned here so T6 adds behavior without a migration, but NOTHING in
-- Task 2 reads or writes them beyond the default.
CREATE TABLE IF NOT EXISTS trigger_revisions (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    trigger_id TEXT NOT NULL REFERENCES triggers (id) ON DELETE CASCADE,
    revision_number INTEGER NOT NULL,
    agent_revision_id TEXT REFERENCES agent_revisions (id),
    run_template_revision_id TEXT REFERENCES run_template_revisions (id),
    input_mapping JSONB NOT NULL DEFAULT '{}'::jsonb,
    dedupe_key_expr TEXT NOT NULL DEFAULT '',
    correlation_mode TEXT NOT NULL DEFAULT 'per_event'
        CHECK (correlation_mode IN ('per_event', 'bounded_key_reuse', 'named_session', 'reject_if_active')),
    correlation_key_expr TEXT NOT NULL DEFAULT '',
    concurrency_policy TEXT NOT NULL DEFAULT 'allow'
        CHECK (concurrency_policy IN ('allow', 'queue', 'replace', 'drop_if_running', 'coalesce', 'singleton')),
    output_mapping JSONB NOT NULL DEFAULT '{}'::jsonb,       -- T6 (callback output shaping) — unused in T2
    callback_endpoint_id TEXT REFERENCES webhook_endpoints (id), -- T6 (callback delivery) — unused in T2
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id),
    UNIQUE (trigger_id, revision_number),
    -- At most one run target is pinned (a template is profile-free; an agent carries identity — one
    -- revision cannot mean both, spec §10).
    CHECK (agent_revision_id IS NULL OR run_template_revision_id IS NULL)
);

-- A TriggerDelivery: one source event's journey through the ingestion pipeline (received → authenticated
-- → deduplicated → mapped → admitted → run_created, with rejected/duplicate/failed/deferred/skipped
-- branches — the §20.2.2 TriggerDelivery state machine). state defaults to the born-into 'received'. A
-- duplicate links duplicate_of to the surviving canonical row (AUT-001 original-linkage); a coalesced/
-- subsumed row links the same way to its survivor. correlation_key_hash is a bounded SHA-256 (only the
-- hash is stored, never the raw correlation key). mapped_input is the canonical action the mapping
-- produced. response_id/run_id/session_id record the born run (the SAME admission path as /v1/responses).
--
-- T5-owned columns (signed inbound HTTP + durable ack + source-dedupe, spec §20.2.2): source /
-- source_tenant / source_event_id / raw_payload are pre-provisioned for the inbound webhook receiver.
-- raw_payload is short-retention (encryption-at-rest is E13); NOTHING in Task 2 populates them beyond the
-- default (manual/api deliveries carry no signed source envelope).
-- T6-owned column: callback_state tracks the post-run callback delivery — unused in T2.
CREATE TABLE IF NOT EXISTS trigger_deliveries (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    trigger_id TEXT NOT NULL REFERENCES triggers (id) ON DELETE CASCADE,
    trigger_revision_id TEXT NOT NULL REFERENCES trigger_revisions (id),
    state TEXT NOT NULL DEFAULT 'received'
        CHECK (state IN ('received', 'authenticated', 'deduplicated', 'mapped', 'admitted',
                         'run_created', 'rejected', 'duplicate', 'failed', 'deferred', 'skipped')),
    dedupe_key TEXT NOT NULL DEFAULT '',
    duplicate_of TEXT REFERENCES trigger_deliveries (id),
    correlation_key_hash TEXT NOT NULL DEFAULT '',
    mapped_input JSONB,
    response_id TEXT NOT NULL DEFAULT '',
    run_id TEXT NOT NULL DEFAULT '',
    session_id TEXT NOT NULL DEFAULT '',
    reason TEXT NOT NULL DEFAULT '',
    source TEXT NOT NULL DEFAULT '',            -- T5 (inbound source ingestion) — unused in T2
    source_tenant TEXT NOT NULL DEFAULT '',     -- T5 (inbound source ingestion) — unused in T2
    source_event_id TEXT NOT NULL DEFAULT '',   -- T5 (inbound source-dedupe) — unused in T2
    raw_payload JSONB,                          -- T5 (inbound raw envelope, short-retention; E13 seals) — unused in T2
    callback_state TEXT NOT NULL DEFAULT ''
        CHECK (callback_state IN ('', 'pending', 'delivered', 'dead')), -- T6 (callback delivery) — unused in T2
    received_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id)
);

-- Canonical-dedupe (AUT-001): at most one LIVE canonical delivery (duplicate_of IS NULL) per
-- (trigger, dedupe_key) for a non-empty key. A second event with the same key loses the ON CONFLICT and
-- is linked as a duplicate; a duplicate row (duplicate_of set) is exempt so linkage never self-conflicts.
CREATE UNIQUE INDEX IF NOT EXISTS trigger_deliveries_dedupe_canonical_idx
    ON trigger_deliveries (trigger_id, dedupe_key)
    WHERE dedupe_key <> '' AND duplicate_of IS NULL;

-- T5 source-dedupe: at most one LIVE canonical delivery per (trigger, source, source_tenant,
-- source_event_id). Pre-provisioned index; T2 never writes source_event_id, so it is inert here.
CREATE UNIQUE INDEX IF NOT EXISTS trigger_deliveries_source_dedupe_idx
    ON trigger_deliveries (trigger_id, source, source_tenant, source_event_id)
    WHERE source_event_id <> '' AND duplicate_of IS NULL;

-- FIFO gate scan (AUT-004): the reconciler admits deferred deliveries per correlation key in received_at
-- order, so a partial index over the deferred rows keeps that sweep cheap.
CREATE INDEX IF NOT EXISTS trigger_deliveries_deferred_fifo_idx
    ON trigger_deliveries (trigger_id, correlation_key_hash, received_at)
    WHERE state = 'deferred';

-- DML-grant the new tables to the application role (000001's blanket GRANT covered only the tables that
-- existed then — the 000019/000020 pattern).
GRANT SELECT, INSERT, UPDATE, DELETE ON triggers, trigger_revisions, trigger_deliveries TO palai_app;

INSERT INTO schema_migrations (version) VALUES (21) ON CONFLICT DO NOTHING;
