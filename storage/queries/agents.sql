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
-- name: InsertAgentRevision
INSERT INTO agent_revisions (id, organization_id, project_id, profile_id, revision_number, model, tools, instructions,
        tool_sets, mcp_connections, skills, hooks)
VALUES ($1, $2, $3, $4,
        (SELECT COALESCE(MAX(revision_number), 0) + 1 FROM agent_revisions WHERE profile_id = $4),
        $5, $6, $7, $8, $9, $10, $11)
RETURNING revision_number;

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
SELECT COALESCE(ar.id, rtr.id)              AS revision_id,
       COALESCE(ar.model, rtr.model, '')    AS model,
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
       ), ARRAY[]::text[])                   AS tool_set_tools
FROM runs r
LEFT JOIN agent_revisions ar ON ar.id = r.agent_revision_id
LEFT JOIN run_template_revisions rtr ON rtr.id = r.run_template_revision_id
WHERE r.id = $1 AND r.organization_id = $2 AND r.project_id = $3;
