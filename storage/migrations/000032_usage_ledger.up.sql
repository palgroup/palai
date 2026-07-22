-- 000032 opens the metering half of E13 (Task 6, BIL-001/BIL-003/QUO-001): the append-only
-- `usage_ledger` every settled meter lands in, and the two DURABLE admission limits read against it —
-- `budgets` (cumulative spend since a period start) and `quotas` (a rolling window). Until now the
-- platform metered nothing: §43's ledger existed as a 000001 table with no writer, and the only
-- admission limits were T7's in-process request-rate bucket and the per-project run counters, both of
-- which forget everything on restart.
--
-- HONEST CEILING — METERING ONLY. There is no invoice, no price revision, no adjustment/compensating
-- entry, no BYOK platform-vs-provider split, and no billing exporter here: those are E13-H/SaaS
-- (BIL-004/005/006). The core learns no billing concept; it records WHAT was consumed, self-sufficiently
-- enough that an external exporter can price it by READING this table alone.
--
-- WHY A NEW TABLE AND NOT 000001's usage_events: usage_events has had a dedupe uniqueness since LP-0 but
-- never had a single writer, and its shape predates §43.1 — no run/session dimension, no unit, no schema
-- version, so a row cannot be attributed to the run that caused it or reconciled against a provider
-- receipt. usage_ledger is its §43.1-shaped successor. usage_events is deliberately left in place and
-- unwritten rather than dropped here (proving nothing external reads it is not this task's job).

-- The settled meter facts. Append-only by grant (see the REVOKE below), deterministic by dedupe_key:
-- the writer derives BOTH the id and the dedupe key from the settling operation's own identity, so a
-- redelivered model step re-derives the same row and ON CONFLICT DO NOTHING settles it exactly once
-- (BIL-001, replay without double settlement).
--
-- schema_version is the versioned-schema promise: an exporter reading rows it did not write can tell
-- which field contract produced them. It is 1 for every row this migration's writers emit; a later
-- shape change bumps it rather than reinterpreting old rows.
--
-- session_id/run_id carry NO foreign key ON PURPOSE. The retention reaper deletes runs and sessions
-- (spec §8.3), and a settlement record MUST outlive the run it settles — a cascade or a blocked delete
-- would either erase billing history or wedge retention. They are correlation dimensions, not links.
CREATE TABLE IF NOT EXISTS usage_ledger (
    id TEXT PRIMARY KEY,
    schema_version INTEGER NOT NULL DEFAULT 1,
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    session_id TEXT,
    run_id TEXT,
    meter TEXT NOT NULL,
    quantity NUMERIC NOT NULL CHECK (quantity >= 0),
    unit TEXT NOT NULL,
    dedupe_key TEXT NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id),
    UNIQUE (organization_id, project_id, dedupe_key)
);

-- The shape both admission limits query: everything for one tenant, one meter family, newest first.
CREATE INDEX IF NOT EXISTS usage_ledger_tenant_meter_idx
    ON usage_ledger (organization_id, project_id, meter, occurred_at DESC);
-- The keyset page GET /v1/usage/ledger walks (created order is occurred_at, id — the shared cursor).
CREATE INDEX IF NOT EXISTS usage_ledger_tenant_keyset_idx
    ON usage_ledger (organization_id, occurred_at DESC, id DESC);

-- A cumulative spend cap since period_start. project_id = '' means the limit covers the WHOLE
-- organization; a concrete project narrows it. meter_prefix matches by PREFIX, so 'model.' caps every
-- model meter, 'model.output_tokens' caps exactly one, and '' caps everything — one regular rule instead
-- of a special "meter group" vocabulary.
CREATE TABLE IF NOT EXISTS budgets (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL REFERENCES organizations (id),
    project_id TEXT NOT NULL DEFAULT '',
    meter_prefix TEXT NOT NULL,
    limit_quantity NUMERIC NOT NULL CHECK (limit_quantity > 0),
    period_start TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (organization_id, project_id, meter_prefix)
);

-- The same cap over a ROLLING window instead of a period — that is the whole difference, and it is why
-- a quota carries stable reset information a budget cannot: window usage ages out on its own, so an
-- exhausted quota can tell the caller when the oldest in-window row releases capacity.
CREATE TABLE IF NOT EXISTS quotas (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL REFERENCES organizations (id),
    project_id TEXT NOT NULL DEFAULT '',
    meter_prefix TEXT NOT NULL,
    limit_quantity NUMERIC NOT NULL CHECK (limit_quantity > 0),
    window_seconds BIGINT NOT NULL CHECK (window_seconds > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (organization_id, project_id, meter_prefix)
);

-- RLS: ORGANIZATION-level for all three, has_project=false, and that is a decision — not the default.
--
-- 000029's catalogue sweep applies a PROJECT-aware policy to any table carrying a project_id column, and
-- these three do. A project-aware policy would be wrong here: a run admits under the project-narrowed
-- scope its API key publishes, and an ORGANIZATION-wide budget must sum the sibling projects' usage from
-- exactly that connection. Under a project-aware policy those rows are invisible and the org budget
-- silently under-counts — a limit that fails OPEN. The organization boundary, which is the tenant
-- boundary, is unchanged: a caller still sees only its own org's meters.
--
-- The intra-organization narrowing that RLS therefore does not perform is done explicitly in SQL: every
-- read path filters project_id (the API's list/summary by the caller's scope, the limit queries by the
-- limit's own project_id). 000032 runs LAST in the chain, so these three CALLs re-assert after 000029's
-- sweep on every boot — the same self-re-assertion the REVOKE below relies on.
CALL palai_apply_tenant_policy('usage_ledger', 'organization_id', false);
CALL palai_apply_tenant_policy('budgets', 'organization_id', false);
CALL palai_apply_tenant_policy('quotas', 'organization_id', false);

-- These tables are created after 000029's blanket `GRANT ... ON ALL TABLES`, so that sweep never saw
-- them: without an explicit grant the runtime role fails closed with a blunt "permission denied for
-- table" instead of the row-scoped policy.
--
-- The REVOKE is load-bearing, not decoration (the 000015/000031 precedent): main.go re-runs the WHOLE
-- chain on every boot, so on boot #2 both 000001's and 000029's blanket grants re-run and — now that
-- usage_ledger EXISTS — re-hand palai_app UPDATE+DELETE on it. A settlement ledger the writing process
-- can restate or erase is not a settlement ledger; an adjustment is a compensating INSERT (§43.1), never
-- an edit. 000032 runs LAST, so this REVOKE re-asserts after them every boot.
--
-- budgets/quotas are mutable CONFIG, not a ledger — a POST re-setting a limit is an UPDATE — so they
-- keep the full DML the blanket grant hands them and are deliberately absent from the REVOKE.
GRANT SELECT, INSERT ON usage_ledger TO palai_app;
REVOKE UPDATE, DELETE ON usage_ledger FROM palai_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON budgets, quotas TO palai_app;

INSERT INTO schema_migrations (version) VALUES (32) ON CONFLICT DO NOTHING;
