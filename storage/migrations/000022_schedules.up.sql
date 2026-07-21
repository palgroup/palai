-- Schedules (spec §33, E11 Task 3): a cron / one-time schedule that fires its trigger on a wall-clock
-- cadence, plus the durable occurrence rows that make each firing exactly-once. Two tables. All
-- CREATE ... IF NOT EXISTS, so the whole chain stays re-runnable (the 000019/000020/000021 pattern).
--
-- CONSCIOUS DECISION — no schedule_revisions table. Unlike triggers/agents (immutable-row lineage), a
-- schedule never REPLAYS an old config: determinism needs only (revision, planned_at). So `revision INT`
-- on the schedule plus `schedule_revision` pinned on each occurrence is the whole reproducibility story;
-- a firing-relevant edit bumps `revision` in place. A full immutable-row lineage would buy nothing here
-- and cost a table, so it is deliberately omitted (§3 rationale).

-- A schedule lineage: a named cron/one-time cadence within a project that fires an existing trigger. The
-- trigger carries the run target (agent/template pin flows from the trigger revision) — a schedule adds
-- NO direct agent FK, it only decides WHEN the trigger's canonical action admits (spec §33.1).
CREATE TABLE IF NOT EXISTS schedules (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    name TEXT NOT NULL,
    -- The trigger a firing admits through (schedules fire triggers — the SAME §20.2.2 admission pipeline a
    -- manual/API delivery takes). The agent/template pin lives in the trigger revision, so there is no
    -- direct agent FK here.
    trigger_id TEXT NOT NULL REFERENCES triggers (id),
    -- The principal a firing admits its run AS (the schedule's creator). Recorded so a scheduled run is
    -- born under a real, auditable, tenant-scoped identity rather than an anonymous system actor. '' only
    -- for a schedule seeded directly in a test.
    created_by TEXT NOT NULL DEFAULT '',
    kind TEXT NOT NULL DEFAULT 'cron' CHECK (kind IN ('cron', 'one_time')),
    cron_expr TEXT NOT NULL DEFAULT '',
    -- Required IANA timezone (spec §33.1). App-validated at create via time.LoadLocation (an unknown name
    -- is a 400, never a stored row); the ticker resolves wall-clock instants in this zone.
    timezone TEXT NOT NULL,
    one_time_at TIMESTAMPTZ,
    -- Misfire policy after downtime (spec §33.3). fire_once_now (default) fires the most-recent missed
    -- instant and windows the rest; skip windows all missed; catch_up materializes oldest-first up to
    -- max_catch_up; fail freezes the schedule for an operator.
    misfire_policy TEXT NOT NULL DEFAULT 'fire_once_now'
        CHECK (misfire_policy IN ('skip', 'fire_once_now', 'catch_up', 'fail')),
    -- A missed instant within this grace of now is a NORMAL late fire, not a misfire (spec §33.3).
    misfire_grace_seconds INTEGER NOT NULL DEFAULT 300,
    -- Hard cap on catch_up materialization (§33.3): catch_up is NEVER unbounded — the DB CHECK makes the
    -- ceiling uncrossable, so a per-minute schedule down for a week can never spawn 10k rows.
    max_catch_up INTEGER NOT NULL DEFAULT 0 CHECK (max_catch_up BETWEEN 0 AND 100),
    -- Bounded per-occurrence admission jitter (spec §33.5): admit at planned_at + hash(occurrence_id) %
    -- jitter_seconds, never beyond ends_at.
    jitter_seconds INTEGER NOT NULL DEFAULT 0 CHECK (jitter_seconds BETWEEN 0 AND 3600),
    starts_at TIMESTAMPTZ,
    ends_at TIMESTAMPTZ,
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'paused', 'failed')),
    status_reason TEXT NOT NULL DEFAULT '',
    -- Bumped in place on a firing-relevant edit; occurrences pin the revision they fired under (the
    -- no-schedule_revisions-table decision above).
    revision INTEGER NOT NULL DEFAULT 1,
    -- The next wall-clock instant due to fire, in UTC. NULL once the schedule is exhausted (a one_time that
    -- fired, or a cron past ends_at). The partial index below makes the due-scan cheap.
    next_fire_at TIMESTAMPTZ,
    deleted_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id),
    UNIQUE (organization_id, project_id, name)
);

-- The due-scan sweep unit: only active, non-deleted schedules with a due instant. A partial index over
-- that predicate keeps the ticker's sweep cheap regardless of how many paused/deleted schedules exist.
CREATE INDEX IF NOT EXISTS schedules_due_scan_idx
    ON schedules (next_fire_at)
    WHERE status = 'active' AND deleted_at IS NULL AND next_fire_at IS NOT NULL;

-- A schedule occurrence: one (schedule, revision, planned instant) firing. occurrence_id is DETERMINISTIC
-- (sha256(schedule_id|revision|RFC3339-UTC planned_at) → occ_<hex>), so N replicas / re-run ticks / NTP
-- jumps all derive the SAME id for the SAME instant. The load-bearing exactly-once invariant is
-- UNIQUE(schedule_id, schedule_revision, planned_at): a claim is INSERT ... ON CONFLICT DO NOTHING and the
-- winner is the row whose insert reported RowsAffected()==1 (replica-safe — correctness lives on this
-- index, NOT on any SELECT ... FOR UPDATE SKIP LOCKED contention optimization).
--
-- state='skipped' is the ONE windowed-skip row a misfire records (from/to/count in reason) — NOT one row
-- per missed instant (the bounded-storage decision, §5). A firing occurrence is born 'pending', becomes
-- 'admitted' once handed to the delivery pipeline (delivery_id links it), or 'failed' with a reason.
CREATE TABLE IF NOT EXISTS schedule_occurrences (
    occurrence_id TEXT PRIMARY KEY,
    schedule_id TEXT NOT NULL REFERENCES schedules (id),
    schedule_revision INTEGER NOT NULL,
    planned_at TIMESTAMPTZ NOT NULL,
    admitted_at TIMESTAMPTZ,
    state TEXT NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending', 'admitted', 'skipped', 'failed')),
    delivery_id TEXT NOT NULL DEFAULT '',
    reason TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    -- The raw exactly-once invariant: at most one occurrence per (schedule, revision, planned instant). The
    -- deterministic PK is derived from the same triple, so the PK and this key can never disagree.
    UNIQUE (schedule_id, schedule_revision, planned_at)
);

-- The handoff sweep unit: occurrences durably committed 'pending' but not yet handed to the delivery
-- pipeline (a crash between claim-commit and admission, or a jitter-delayed admit). A partial index over
-- the pending rows keeps that sweep cheap.
CREATE INDEX IF NOT EXISTS schedule_occurrences_pending_idx
    ON schedule_occurrences (planned_at)
    WHERE state = 'pending';

-- DML-grant the new tables to the application role (000001's blanket GRANT covered only the tables that
-- existed then — the 000019/000020/000021 pattern).
GRANT SELECT, INSERT, UPDATE, DELETE ON schedules, schedule_occurrences TO palai_app;

INSERT INTO schema_migrations (version) VALUES (22) ON CONFLICT DO NOTHING;
