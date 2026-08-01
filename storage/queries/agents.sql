-- Automation-agent management + pin resolution (spec §10, §14, §32.2, E11 Task 1). Writes are the
-- management surface (create/revise/publish); reads are admission validation and execution pin
-- resolution. A revise always INSERTs a new draft — no statement here ever rewrites a revision's
-- config columns, so a published revision is immutable by discipline (the only UPDATE is the publish
-- flip). Every statement is tenant-scoped by (organization_id, project_id).

-- name: InsertAgentProfile
INSERT INTO agent_profiles (id, organization_id, project_id, name)
VALUES ($1, $2, $3, $4);

-- AgentProfileExists verifies a profile is in scope before a revision is attached to it.
-- name: AgentProfileExists
SELECT 1 FROM agent_profiles WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- InsertAgentRevision creates a DRAFT revision (published_at NULL). revision_number is the profile's
-- next monotonic number, computed in-statement so a revise never has to read-then-write. Returns it.
-- ponytail: the MAX+1 subselect can race two concurrent inserts to the same number; the
-- UNIQUE(profile_id, revision_number) constraint then rejects the loser (retry on 23505 if it matters).
-- GetAgentProfile reads a profile lineage within scope (spec §10, E13 T4). Foreign/unknown -> no row.
-- name: GetAgentProfile
SELECT id, name, created_at
FROM agent_profiles
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- ListAgentProfiles pages a project's agent-profile lineages newest-first (spec §10, E13 T4).
-- name: ListAgentProfiles
SELECT id, name, created_at
FROM agent_profiles
WHERE organization_id = $1 AND project_id = $2
  AND ($3::timestamptz IS NULL OR created_at >= $3)
  AND ($4::timestamptz IS NULL OR created_at <= $4)
  AND ($5::timestamptz IS NULL OR (created_at, id) < ($5, $6))
ORDER BY created_at DESC, id DESC
LIMIT $7;

-- ListAgentRevisions pages one profile's revisions newest-first (spec §10, E13 T4). Scoped by profile_id
-- ($3) on top of the tenant scope, so an unknown/foreign profile simply yields an empty page.
--
-- tool_sets JOINS THE PROJECTION IN E25 T7, and for the third time the same argument: there is no
-- per-revision GET, so this list is the only read path for a config the API lets you write. It is the
-- half of jira-mcp-connection.md §4 that GRANTS the tools ("tool_sets missing → the tool is never
-- advertised"), while mcp_connections — the CEILING — has read back since E22 T6. A runbook that says
-- both fields are required had one of them readable.
-- name: ListAgentRevisions
SELECT id, revision_number, model, tools, tool_sets, mcp_connections, instructions, environment, published_at IS NOT NULL, created_at
FROM agent_revisions
WHERE organization_id = $1 AND project_id = $2 AND profile_id = $3
  AND ($4::timestamptz IS NULL OR created_at >= $4)
  AND ($5::timestamptz IS NULL OR created_at <= $5)
  AND ($6::timestamptz IS NULL OR (created_at, id) < ($6, $7))
ORDER BY created_at DESC, id DESC
LIMIT $8;

-- name: InsertAgentRevision
INSERT INTO agent_revisions (id, organization_id, project_id, profile_id, revision_number, model, tools, instructions,
        tool_sets, mcp_connections, skills, hooks, environment)
VALUES ($1, $2, $3, $4,
        (SELECT COALESCE(MAX(revision_number), 0) + 1 FROM agent_revisions WHERE profile_id = $4),
        $5, $6, $7, $8, $9, $10, $11, $12)
RETURNING revision_number;

-- AgentRevisionEnvironment reads the environment a revision names (E25 T3), '' for a revision that names
-- none. The publish path re-checks the reference through it, because publish is the last moment at which
-- "this agent has the production credentials" can be refused rather than discovered by a run that had none.
-- name: AgentRevisionEnvironment
SELECT environment FROM agent_revisions WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- PublishAgentRevision is the ONE legitimate mutation: it flips published_at exactly once. The
-- WHERE published_at IS NULL guard makes a re-publish a zero-row no-op, so a published revision's
-- boundary never re-stamps (immutable publish). RETURNING id lets the caller distinguish
-- published-now (one row) from already-published/unknown (no row).
-- name: PublishAgentRevision
UPDATE agent_revisions
SET published_at = clock_timestamp()
WHERE id = $1 AND organization_id = $2 AND project_id = $3 AND published_at IS NULL
RETURNING id;

-- GetAgentRevision reads a revision's full config + publish state (management GET + immutability check).
-- name: GetAgentRevision
SELECT profile_id, revision_number, model, tools, instructions, published_at, created_at
FROM agent_revisions
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- AgentRevisionPublished returns whether a revision exists in scope and is published (admission
-- validation): no rows = unknown (404), false = draft (409, cannot be pinned or run), true = pinnable.
-- name: AgentRevisionPublished
SELECT published_at IS NOT NULL
FROM agent_revisions
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- InsertRunTemplateRevision creates a DRAFT template revision — the same executable config MINUS
-- identity/delegation (a template must not impersonate an agent). revision_number is the template
-- name's next monotonic number. Returns it.
-- name: InsertRunTemplateRevision
INSERT INTO run_template_revisions (id, organization_id, project_id, template_name, revision_number, model, tools, instructions,
        tool_sets, mcp_connections, skills, hooks)
VALUES ($1, $2, $3, $4,
        (SELECT COALESCE(MAX(revision_number), 0) + 1 FROM run_template_revisions
         WHERE organization_id = $2 AND project_id = $3 AND template_name = $4),
        $5, $6, $7, $8, $9, $10, $11)
RETURNING revision_number;

-- PublishRunTemplateRevision mirrors PublishAgentRevision: a once-only conditional flip.
-- name: PublishRunTemplateRevision
UPDATE run_template_revisions
SET published_at = clock_timestamp()
WHERE id = $1 AND organization_id = $2 AND project_id = $3 AND published_at IS NULL
RETURNING id;

-- RunTemplateRevisionPublished is the template admission-validation read (see AgentRevisionPublished).
-- name: RunTemplateRevisionPublished
SELECT published_at IS NOT NULL
FROM run_template_revisions
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- PinnedRunConfig resolves a run's pinned executable config for the resolver (spec §14): the model
-- and tool ceiling of whichever revision the run pinned. A run pins at most one source, so COALESCE
-- picks it; revision_id is NULL for a profile-free run (the resolver then skips the revision layer).
-- The pin is fixed on the run row, so a later revision of the same profile leaves this read unchanged
-- (AGT-001 old-run reproducibility).
-- tool_set_tools resolves the E12 grant (spec §28.2-28.4): the model-visible short names of every tool
-- pinned by a PUBLISHED tool_set_revision the pinned revision names in its tool_sets JSONB array. Walks
-- tool_sets → tool_set_revisions → tool_pins → tool_revisions → tools, tenant-scoped, DISTINCT, and
-- COALESCEd to an empty array so a profile-free / set-free run returns [] (the resolver then unions
-- nothing). The array_agg is ORDER BY the short name so the list is deterministic — it flows into
-- ConfigSnapshot.Hash (checkpoint reproducibility + config.revised), and an unordered DISTINCT aggregate
-- may hash on PG16+ → undefined order → the SAME pinned config hashing differently across two reads.
-- name: PinnedRunConfig
-- skill_pins is the run's FROZEN skill set (E12 Task 7, spec §28.16), pinned once at run-start by
-- PinRunSkills — the resolver reads it back verbatim (never re-resolving "latest"), so a mid-run enable
-- of a new revision leaves this read unchanged. NULL for a skill-less run (the resolver then carries no
-- skills and the config hash + provider request stay bit-identical to a skill-less run).
-- instructions is the pinned revision's own instruction layer (spec §25.12 layer 3). IT WAS THE
-- MISSING COLUMN: agent CRUD has written agent_revisions.instructions since E11 and no query on the
-- execution path ever selected it, so a run could pin a revision, record the pin correctly, and tell
-- the model none of what that revision said. Measured against a real provider on 2026-08-01: a run
-- pinning a revision whose instructions were 320 words measured the same usage.input_tokens (48) as
-- a run pinning nothing. COALESCEd to '' so a profile-free run, or a revision that set no
-- instructions, contributes no layer and its provider request stays bit-identical.
SELECT COALESCE(ar.id, rtr.id)              AS revision_id,
       COALESCE(ar.model, rtr.model, '')    AS model,
       COALESCE(ar.instructions, rtr.instructions, '') AS instructions,
       COALESCE(ar.tools, rtr.tools)        AS tools,
       COALESCE((
           SELECT array_agg(DISTINCT t.model_visible_name ORDER BY t.model_visible_name)
           FROM tool_set_revisions tsr
           CROSS JOIN LATERAL jsonb_array_elements(tsr.tool_pins) AS pin
           JOIN tool_revisions trv ON trv.id = (pin->>'tool_revision_id')
               AND trv.organization_id = r.organization_id AND trv.project_id = r.project_id
           JOIN tools t ON t.id = trv.tool_id
           WHERE tsr.organization_id = r.organization_id AND tsr.project_id = r.project_id
               AND tsr.published_at IS NOT NULL
               AND tsr.id IN (SELECT jsonb_array_elements_text(COALESCE(ar.tool_sets, rtr.tool_sets, '[]'::jsonb)))
       ), ARRAY[]::text[])                   AS tool_set_tools,
       r.skill_pins                          AS skill_pins
FROM runs r
LEFT JOIN agent_revisions ar ON ar.id = r.agent_revision_id
LEFT JOIN run_template_revisions rtr ON rtr.id = r.run_template_revision_id
WHERE r.id = $1 AND r.organization_id = $2 AND r.project_id = $3;

-- RunSkillPinInputs resolves the inputs the run-start pin write needs (E12 Task 7, spec §28.16): the
-- pinned revision's REQUESTED skill names (the opaque JSONB rider T2 stored unvalidated) and whether the
-- run's skill_pins are already frozen. A run pins at most one revision source, so COALESCE picks it. The
-- already_pinned flag makes the pin write idempotent: a resumed attempt sees it true and skips resolution
-- entirely, so a skill disabled mid-run never fails a re-resolution on a run that already froze its pins.
-- name: RunSkillPinInputs
SELECT COALESCE(ar.skills, rtr.skills, '[]'::jsonb) AS requested_skills,
       (r.skill_pins IS NOT NULL)                   AS already_pinned
FROM runs r
LEFT JOIN agent_revisions ar ON ar.id = r.agent_revision_id
LEFT JOIN run_template_revisions rtr ON rtr.id = r.run_template_revision_id
WHERE r.id = $1 AND r.organization_id = $2 AND r.project_id = $3;

-- PinRunSkills freezes a run's resolved skill pins ONCE (E12 Task 7, spec §28.16). The skill_pins IS NULL
-- guard makes it a no-op on a run already pinned (a resumed attempt), so the frozen digest set is
-- immutable for the life of the run — a later enable never re-pins. RETURNING id reports whether it wrote.
-- name: PinRunSkills
UPDATE runs
SET skill_pins = $4
WHERE id = $1 AND organization_id = $2 AND project_id = $3 AND skill_pins IS NULL
RETURNING id;

-- LatestPublishedAgentRevision resolves an AGENT to the revision a run pinning `agent_id` executes:
-- its highest-numbered PUBLISHED revision, in the caller's own scope. Returns no rows when the agent
-- does not exist there (a 404 that discloses nothing about other tenants) — which is DIFFERENT from
-- the agent existing with nothing published, and the caller distinguishes the two by first asking
-- AgentProfileExists.
--
-- ORDER BY revision_number DESC IS LOAD-BEARING, NOT DECORATION. `LIMIT` without a total order has
-- decided an outcome wrongly twice in this tree, and here it would pick an ARBITRARY published
-- revision each time the planner felt like it — two identical requests running two different
-- agents' configurations, non-reproducibly. revision_number is UNIQUE per profile (000019), so this
-- order is total and the row is exactly one.
-- name: LatestPublishedAgentRevision
SELECT id
FROM agent_revisions
WHERE profile_id = $1 AND organization_id = $2 AND project_id = $3 AND published_at IS NOT NULL
ORDER BY revision_number DESC
LIMIT 1;
