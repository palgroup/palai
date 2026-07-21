-- Automation agents (spec §10, §32.2, E11 Task 1). An AgentProfile is a named lineage; an
-- AgentRevision is an IMMUTABLE, publishable snapshot of the executable config the run resolves
-- from (this slice's enforced subset: model route, tool ceiling, instructions). A RunTemplateRevision
-- is the same executable config MINUS agent identity/delegation — profile-free automation (AGT-003).
--
-- Immutability is by DISCIPLINE, not a REVOKE: publish is the ONE legitimate mutation (a conditional
-- published_at flip), so — unlike the checkpoints table (000015) — UPDATE stays granted. No query ever
-- rewrites a revision's config columns; a "revise" always INSERTs a NEW draft revision. The immutability
-- test (automation package) proves the published row's config bytes never change. A DB trigger would be
-- overkill here (spec §10 edit-is-a-new-revision).
--
-- Every CREATE TABLE / ADD COLUMN IF NOT EXISTS keeps the migration idempotent (Migrate is re-run per
-- boot), matching the 000015/000018 pattern. Tenant scope is the composite (organization_id, project_id)
-- FK to projects every execution row carries (spec §39.2).

CREATE TABLE IF NOT EXISTS agent_profiles (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    -- The profile's human name; the lineage its revisions belong to (spec §10). Unique per project.
    name TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id),
    UNIQUE (organization_id, project_id, name)
);

CREATE TABLE IF NOT EXISTS agent_revisions (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    profile_id TEXT NOT NULL REFERENCES agent_profiles (id),
    -- Monotonic per profile: a revise is a new revision_number, never an in-place edit.
    revision_number INTEGER NOT NULL,
    -- The enforced executable-config subset (spec §10, §2 E11 include list). model '' inherits the
    -- deployment default; tools NULL imposes NO ceiling (the run keeps its project/session tools);
    -- a non-null tools array is the capability CEILING the resolver intersects. The E12 fields
    -- (mcp/skills/hooks/knowledge) are deliberately ABSENT — dead config is not stored (honest naming).
    model TEXT NOT NULL DEFAULT '',
    tools JSONB,
    instructions TEXT NOT NULL DEFAULT '',
    -- NULL = draft (cannot be pinned or run); a conditional flip publishes it exactly once. Publish is
    -- irreversible (rollback = re-pin an earlier revision), so there is no unpublish path.
    published_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id),
    UNIQUE (profile_id, revision_number)
);

-- Read a profile's revisions newest-first (the management surface lists them; pin resolution reads one).
CREATE INDEX IF NOT EXISTS agent_revisions_by_profile ON agent_revisions (profile_id, revision_number DESC);

CREATE TABLE IF NOT EXISTS run_template_revisions (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    -- A template is named directly (no AgentProfile lineage — a template must NOT impersonate an agent
    -- identity, spec §32.2). Its revisions share the template_name; identity/delegation fields are
    -- rejected at the management layer, so the shape carries only reusable executable config.
    template_name TEXT NOT NULL,
    revision_number INTEGER NOT NULL,
    model TEXT NOT NULL DEFAULT '',
    tools JSONB,
    instructions TEXT NOT NULL DEFAULT '',
    published_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id),
    UNIQUE (organization_id, project_id, template_name, revision_number)
);

-- The run's pinned executable-config source (spec §14, AGT-001). At most one is set: a run pins EITHER
-- an AgentRevision OR a RunTemplateRevision, resolved as the same config layer. Both are NULLable — a
-- profile-free run pins neither and resolves exactly as before (regression-safe). The pin is immutable
-- for the run's life: a later revision of the same profile does NOT change a running/finished run's
-- resolved config, so an old run stays reproducible. Single-column inline FKs (revision ids are opaque
-- and globally unique) keep the ALTER idempotent via ADD COLUMN IF NOT EXISTS.
ALTER TABLE runs
    ADD COLUMN IF NOT EXISTS agent_revision_id TEXT REFERENCES agent_revisions (id);
ALTER TABLE runs
    ADD COLUMN IF NOT EXISTS run_template_revision_id TEXT REFERENCES run_template_revisions (id);

-- The new tables are DML-granted to the application role explicitly (000001's blanket GRANT covered
-- only the tables that existed then, and it re-runs each Migrate). UPDATE stays granted: publish is a
-- legitimate conditional flip, and config immutability is enforced by query discipline, not a REVOKE
-- (see the header). DELETE stays for retention/GC of unpublished drafts.
GRANT SELECT, INSERT, UPDATE, DELETE ON agent_profiles, agent_revisions, run_template_revisions TO palai_app;

INSERT INTO schema_migrations (version) VALUES (19) ON CONFLICT DO NOTHING;
