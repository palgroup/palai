-- 000027 adds the skills registry (E12 Task 7, spec §28.15-28.16, TOL-011): tenant-scoped skill
-- lineages + immutable SkillRevisions carrying the QUARANTINE-sanitized archive, its content digest,
-- the static-scan findings, and the parsed SKILL.md metadata, plus the runs.skill_pins rider that
-- freezes a run's resolved skill digests at run-start (a run never resolves "latest"). It references
-- projects/runs (000001) and its own new tables, and ALTERs runs (000001), so it opens from the tip of
-- the 000026 chain (T5 landed first at merge — sequential, no gap). A skill is UNTRUSTED content: the
-- archive is the sanitized re-pack (bomb-capped, so bounded — no object store needed), immutable once
-- inserted; the ONLY UPDATE is the state transition. Idempotent (IF NOT EXISTS) so a re-run is safe.

CREATE TABLE IF NOT EXISTS skills (
    id              TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    project_id      TEXT NOT NULL,
    name            TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (organization_id, project_id, name)
);

CREATE TABLE IF NOT EXISTS skill_revisions (
    id              TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    project_id      TEXT NOT NULL,
    skill_id        TEXT NOT NULL REFERENCES skills(id),
    revision_number INTEGER NOT NULL,
    digest          TEXT NOT NULL,
    -- quarantined = has scan findings OR not yet cleared; approved = scan clean, cleared to enable;
    -- enabled = the active revision runs resolve. A findings-bearing revision is stuck at quarantined
    -- (approve/enable never reach it) — "scan FAIL blocks enable" (TOL-011).
    state           TEXT NOT NULL DEFAULT 'quarantined'
                    CHECK (state IN ('quarantined', 'approved', 'enabled')),
    scan_findings   JSONB,
    metadata        JSONB,
    archive         BYTEA NOT NULL,
    source_url      TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (skill_id, revision_number)
);

-- skill_pins freezes the run's resolved skill digests at run-start (spec §28.16): a list of
-- {name, description, digest, path} objects. NULL for a run with no skills — the resolver then returns
-- no skills and the run's config hash + provider request stay bit-identical to a skill-less run. The pin
-- is written ONCE (WHERE skill_pins IS NULL) so a mid-run enable of a new revision never changes it.
ALTER TABLE runs ADD COLUMN IF NOT EXISTS skill_pins JSONB;
