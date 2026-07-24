-- 000039 adds the CapabilityWorker contract tables (E17 Task 9, spec §31.2-31.6, WRK-001..007). It models
-- the SAME outbound-enrolled, lease/fenced execution surface a runner uses (packages/runner + coordinator),
-- but for typed, out-of-process CAPABILITY jobs (e.g. a macOS `swift.build-check` on a host toolchain the
-- container cannot reach). Two tenant-scoped tables:
--   * capability_workers — an enrolled worker's typed capability/version, os/arch, toolchain digests,
--                          capacity, pool/trust labels, and health (§31.2). MUTABLE: health, heartbeat, and
--                          the lease_fence change over a worker's life, so it takes the full DML grant.
--   * capability_jobs    — the APPEND-ONLY job journal (§31.3). One IMMUTABLE row per lifecycle entry
--                          (dispatched -> leased -> progress -> completed/failed/quarantined), keyed
--                          (job_id, entry_seq). A job's CURRENT state is its highest-seq entry. Append-only
--                          is load-bearing: a process that could rewrite or erase an entry could forge a
--                          receipt, un-fence a stale worker, or hide a quarantine — so it carries a
--                          self-re-asserting REVOKE (the usage_ledger/queue_effect_receipts precedent).
--
-- MIGRATION NUMBER: built as 000039 in the E17 T9 worktree so this worktree's
-- TestOrderedMigrationsIsContiguousVersionOrder (strict, no gaps off the 000038 head) stays green in
-- isolation. Its PLAN-assigned number is 000040 (§1 table); T3 a2a-client holds 000039 and merges first.
-- The integrator RENUMBERS to 000040 at merge: rename the up/down files, bump the schema_migrations
-- VALUES/DELETE markers to 40, the embed var (migrationUp39 -> 40 + concat), and the migrations_test
-- head-pin. See the E17 T9 report.
--
-- All CREATE ... IF NOT EXISTS, so the whole chain stays re-runnable. Both carry organization_id +
-- project_id, so per the M3 rule (storage/migrations/000030) each asserts its OWN project-aware tenant
-- policy here (ENABLE + FORCE) rather than leaning on 000029's boot sweep — tests/security/tenancy fails a
-- table that ships without it. Both were created AFTER 000029's blanket `GRANT ... ON ALL TABLES`, so each
-- needs its own grant or the runtime role fails closed with "permission denied".

-- An enrolled capability worker (§31.2). trust_label is 'sandbox' by default: an ORDINARY sandbox worker is
-- exactly the one that must NOT be usable as a general tunnel (§31.5) — its typed capability/operation is the
-- whole of what it may run. lease_fence is the enrollment fence: it starts at 1 and a re-enrollment or a
-- health/capability change bumps it, cutting any older lease (the runner fence precedent). toolchain_digests
-- pins what the worker actually has (e.g. {"swiftc":"..."}) so a job can be matched to a compatible worker;
-- NO signing/provisioning material is or ever will be stored here — a real signed Apple build is a separate
-- capability (apple-build, discovery=disabled) and the §6 leg 3 operator work.
CREATE TABLE IF NOT EXISTS capability_workers (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    capability TEXT NOT NULL,                             -- typed capability, e.g. 'swift-toolchain'
    capability_version TEXT NOT NULL DEFAULT '0.1.0',
    os TEXT NOT NULL DEFAULT '',
    arch TEXT NOT NULL DEFAULT '',
    toolchain_digests JSONB NOT NULL DEFAULT '{}'::jsonb, -- {"swiftc":"sha256:..."} — matched, never signing keys
    capacity INTEGER NOT NULL DEFAULT 1 CHECK (capacity > 0),
    pool_label TEXT NOT NULL DEFAULT '',
    trust_label TEXT NOT NULL DEFAULT 'sandbox',
    health TEXT NOT NULL DEFAULT 'healthy'
        CHECK (health IN ('healthy', 'draining', 'unhealthy')),
    lease_fence BIGINT NOT NULL DEFAULT 1 CHECK (lease_fence > 0),
    enrolled_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    last_heartbeat_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id)
);

-- Match ready jobs to a worker's typed capability, healthy first.
CREATE INDEX IF NOT EXISTS capability_workers_capability_idx
    ON capability_workers (organization_id, project_id, capability) WHERE health = 'healthy';

CALL palai_apply_tenant_policy('capability_workers', 'organization_id', true);
GRANT SELECT, INSERT, UPDATE, DELETE ON capability_workers TO palai_app;

-- The append-only job journal (§31.3). One IMMUTABLE row per lifecycle entry of a job; the job's current
-- state is the entry with the highest entry_seq for its job_id. The dispatch entry (entry_seq = 1) carries
-- the full immutable job SPEC: idempotency key, run/attempt identity, exact capability + operation, input
-- artifact refs, job-scoped secret-HANDLE refs (NAMES into secret_refs (000031) — NEVER values), deadline,
-- resource limits, output schema, network policy, and the fence token. Later entries (leased/progress/
-- terminal) carry the delta (kind, worker_id, receipt, fence) for that same job_id; spec columns repeat from
-- the dispatch row is unnecessary, so they are nullable/defaulted on non-dispatch entries.
--
-- fence_token is the load-bearing §31.6 anchor: a lease is fenced at the dispatch fence, and a re-dispatch
-- (read-only retry on another worker, or a health/capability change that cuts the lease) appends a NEW
-- dispatched entry at fence+1. A result submitted under a superseded fence is a STALE FENCE and is rejected
-- (the CommitToolResult precedent). secret_handle_refs holds only the ref NAMES: the value is resolved
-- job-scoped and short-lived at redeem time and never lands in a row, a log, or an evidence bundle.
CREATE TABLE IF NOT EXISTS capability_jobs (
    id TEXT PRIMARY KEY,                                  -- per-ENTRY id (a journal row), not the job id
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    job_id TEXT NOT NULL,                                 -- stable job identity, shared across its entries
    entry_seq INTEGER NOT NULL CHECK (entry_seq > 0),
    entry_kind TEXT NOT NULL
        CHECK (entry_kind IN ('dispatched', 'leased', 'progress', 'completed', 'failed', 'quarantined')),
    idempotency_key TEXT NOT NULL DEFAULT '',
    run_id TEXT NOT NULL DEFAULT '',
    attempt_id TEXT NOT NULL DEFAULT '',
    worker_id TEXT NOT NULL DEFAULT '',                   -- '' on the dispatch entry; the leaseholder after
    capability TEXT NOT NULL DEFAULT '',
    operation TEXT NOT NULL DEFAULT '',                   -- the EXACT typed operation, e.g. 'swift.build-check'
    input_refs JSONB NOT NULL DEFAULT '[]'::jsonb,        -- input artifact refs
    secret_handle_refs JSONB NOT NULL DEFAULT '[]'::jsonb,-- secret_refs NAMES only, never values
    deadline_at TIMESTAMPTZ,                              -- the job deadline; the secret handle expires with it
    resource_limits JSONB NOT NULL DEFAULT '{}'::jsonb,
    output_schema JSONB NOT NULL DEFAULT '{}'::jsonb,
    network_policy JSONB NOT NULL DEFAULT '{}'::jsonb,
    side_effect_key TEXT NOT NULL DEFAULT '',             -- destination idempotency for a side-effecting op
    fence_token BIGINT NOT NULL DEFAULT 1 CHECK (fence_token > 0),
    receipt JSONB NOT NULL DEFAULT '{}'::jsonb,           -- structured result / execution receipt on terminals
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id),
    -- The journal's spine: entries of a job are strictly ordered, and two writers computing the same next
    -- seq collide here (23505) so a claim/append is at-most-once — the queue_messages fence precedent.
    UNIQUE (job_id, entry_seq)
);

-- Resolve a job's CURRENT entry (highest seq), and scan a job's whole history newest-first.
CREATE INDEX IF NOT EXISTS capability_jobs_job_seq_idx
    ON capability_jobs (job_id, entry_seq DESC);

-- Idempotent dispatch: the FIRST entry of a job is entry_seq = 1, so a partial-unique index on the dispatch
-- entry makes (org, project, idempotency_key) admit exactly one job. A re-dispatch (entry_seq >= 2) reuses
-- the job_id and is exempt, so a retry does not trip it.
CREATE UNIQUE INDEX IF NOT EXISTS capability_jobs_idempotency_idx
    ON capability_jobs (organization_id, project_id, idempotency_key)
    WHERE entry_seq = 1 AND idempotency_key <> '';

CALL palai_apply_tenant_policy('capability_jobs', 'organization_id', true);

-- capability_jobs is APPEND-ONLY: a consumer inserts an entry and reads the journal, never rewrites or erases
-- one. Grant only SELECT + INSERT.
GRANT SELECT, INSERT ON capability_jobs TO palai_app;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO palai_app;

-- The load-bearing REVOKE (the usage_ledger (000032) / queue_effect_receipts (000037) precedent): 000001's
-- and 000029's blanket `GRANT ... ON ALL TABLES` re-run on every boot and — now that capability_jobs EXISTS
-- — re-hand palai_app UPDATE/DELETE on it. 000039 runs AFTER both grants in the chain (39 > 29 > 1) and no
-- later migration re-grants this table, so this REVOKE re-asserts every boot and keeps the journal append-
-- only. A journal a stale worker could rewrite (to un-fence its result) or delete (to hide a quarantine) is
-- not a journal.
REVOKE UPDATE, DELETE ON capability_jobs FROM palai_app;

INSERT INTO schema_migrations (version) VALUES (39) ON CONFLICT DO NOTHING;
