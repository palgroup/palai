-- Extensibility registry management + broker-lookup resolution (spec §28.2-28.4, E12 Task 2). Writes
-- are the management surface (create/revise/publish); reads back the immutable-check + the per-tenant
-- broker lookup's pin-chain resolution. A revise always INSERTs a new revision — no statement here ever
-- rewrites a revision's config columns, so a published revision is immutable by discipline (the only
-- UPDATE is the publish flip). Every statement is tenant-scoped by (organization_id, project_id).

-- name: InsertTool
INSERT INTO tools (id, organization_id, project_id, canonical_name, model_visible_name)
VALUES ($1, $2, $3, $4, $5);

-- ToolExists verifies a tool is in scope before a revision is attached to it.
-- name: ToolExists
SELECT 1 FROM tools WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- InsertToolRevision creates a DRAFT revision (published_at NULL). revision_number is the tool's next
-- monotonic number, computed in-statement so a revise never has to read-then-write. Returns it.
-- ponytail: the MAX+1 subselect can race two concurrent inserts to the same number; the
-- UNIQUE(tool_id, revision_number) constraint then rejects the loser (retry on 23505 if it ever matters).
-- name: InsertToolRevision
INSERT INTO tool_revisions (id, organization_id, project_id, tool_id, revision_number, executor,
        description, input_schema, output_schema, replay_class, timeout_ms, limits, executor_config, secret_ref, digest)
VALUES ($1, $2, $3, $4,
        (SELECT COALESCE(MAX(revision_number), 0) + 1 FROM tool_revisions WHERE tool_id = $4),
        $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
RETURNING revision_number;

-- PublishToolRevision is the ONE legitimate mutation: it flips published_at exactly once. The
-- WHERE published_at IS NULL guard makes a re-publish a zero-row no-op, so a published revision never
-- re-stamps (immutable publish). RETURNING id distinguishes published-now from already-published/unknown.
-- name: PublishToolRevision
UPDATE tool_revisions
SET published_at = clock_timestamp()
WHERE id = $1 AND organization_id = $2 AND project_id = $3 AND published_at IS NULL
RETURNING id;

-- ToolRevisionPublished returns whether a revision exists in scope and is published (publish
-- disambiguation): no rows = unknown, false = draft, true = published.
-- name: ToolRevisionPublished
SELECT published_at IS NOT NULL
FROM tool_revisions
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- GetToolRevision reads a revision's identity + digest + publish state (management + immutability check).
-- name: GetToolRevision
SELECT tool_id, revision_number, executor, digest, published_at
FROM tool_revisions
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- ToolRevisionForPin reads a pinned revision's publish state + declared timeout so a set create can
-- reject a draft pin and an override that widens the declared limit.
-- name: ToolRevisionForPin
SELECT published_at IS NOT NULL, timeout_ms
FROM tool_revisions
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- InsertToolSetRevision creates a DRAFT set revision. revision_number is the set name's next monotonic
-- number. Returns it.
-- name: InsertToolSetRevision
INSERT INTO tool_set_revisions (id, organization_id, project_id, set_name, revision_number, tool_pins, digest)
VALUES ($1, $2, $3, $4,
        (SELECT COALESCE(MAX(revision_number), 0) + 1 FROM tool_set_revisions
         WHERE organization_id = $2 AND project_id = $3 AND set_name = $4),
        $5, $6)
RETURNING revision_number;

-- PublishToolSetRevision mirrors PublishToolRevision: a once-only conditional flip.
-- name: PublishToolSetRevision
UPDATE tool_set_revisions
SET published_at = clock_timestamp()
WHERE id = $1 AND organization_id = $2 AND project_id = $3 AND published_at IS NULL
RETURNING id;

-- ToolSetRevisionPublished is the set-revision publish-state read (see ToolRevisionPublished).
-- name: ToolSetRevisionPublished
SELECT published_at IS NOT NULL
FROM tool_set_revisions
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- LookupRunTool resolves a registered tool the broker must execute by its model-visible short name ($4),
-- through the run's ($1) pinned revision's tool_sets → PUBLISHED tool_set_revisions → tool_pins →
-- tool_revisions → tools (tenant-scoped by $2/$3). It is the per-tenant broker-lookup chain: only a tool
-- pinned by a published set the run's pinned revision names is resolvable, so nothing outside the pin
-- surface ever reaches the broker. Returns the executor binding + schemas + replay class for the row.
-- POSTURE: the AgentRevision.tools ceiling is NOT re-checked here — capability restriction is an
-- advertisement-side concern (the effective set / request construction), consistent with the static tool
-- broker, which also fences+executes any dispatched call without re-consulting the ceiling.
-- executor_config/timeout_ms are added for the mcp executor (E12 T5): the mcp branch reads the connection_id
-- + remote_name from executor_config and the per-call timeout from timeout_ms. The MCP credential lives on
-- the CONNECTION (mcp_connections.secret_ref), not the revision, so the revision secret_ref is not selected
-- here. The control_plane branch ignores these columns.
-- name: LookupRunTool
SELECT trv.executor, trv.description, trv.input_schema, trv.output_schema, trv.replay_class,
       trv.executor_config, trv.secret_ref, trv.timeout_ms, t.canonical_name, trv.revision_number
FROM runs r
LEFT JOIN agent_revisions ar ON ar.id = r.agent_revision_id
LEFT JOIN run_template_revisions rtr ON rtr.id = r.run_template_revision_id
JOIN tool_set_revisions tsr ON tsr.organization_id = r.organization_id AND tsr.project_id = r.project_id
    AND tsr.published_at IS NOT NULL
    AND tsr.id IN (SELECT jsonb_array_elements_text(COALESCE(ar.tool_sets, rtr.tool_sets, '[]'::jsonb)))
CROSS JOIN LATERAL jsonb_array_elements(tsr.tool_pins) AS pin
JOIN tool_revisions trv ON trv.id = (pin->>'tool_revision_id')
    AND trv.organization_id = r.organization_id AND trv.project_id = r.project_id
JOIN tools t ON t.id = trv.tool_id AND t.model_visible_name = $4
WHERE r.id = $1 AND r.organization_id = $2 AND r.project_id = $3
LIMIT 1;
