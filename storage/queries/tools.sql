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

-- ListToolRevisions pages ONE lineage's revisions newest-first (E25 T7). It is the read that makes
-- docs/operations/jira-mcp-connection.md §3c executable: that runbook has shipped since E22 telling an
-- operator to "find the ids with GET /v1/tools, then publish each revision", and until this query existed
-- no /v1 response carried a revision id at all — so the publish, the pin and the grant that follow it all
-- rested on a step nobody could perform.
--
-- description AND input_schema ARE SELECTED DELIBERATELY, and they are the reason this is not just an id
-- list. What an admin approves at publish IS the untrusted description (it enters a model's context) and
-- the input schema (it is the shape the model will be able to call). A projection that showed neither
-- would be asking for approval of an identifier. They are UNTRUSTED BYTES — written by the MCP server —
-- and every consumer treats them as text, never as markup.
--
-- approval_required/approval_label ride along for the symmetric reason: E23 T5 made publish carry a gate
-- declaration, and a declaration that cannot be read back is a gate an operator cannot audit.
-- name: ListToolRevisions
SELECT id, tool_id, revision_number, executor, description, input_schema, digest,
       published_at IS NOT NULL, approval_required, approval_label, created_at
FROM tool_revisions
WHERE tool_id = $1 AND organization_id = $2 AND project_id = $3
  AND ($4::timestamptz IS NULL OR created_at >= $4)
  AND ($5::timestamptz IS NULL OR created_at <= $5)
  AND ($6::timestamptz IS NULL OR (created_at, id) < ($6, $7))
ORDER BY created_at DESC, id DESC
LIMIT $8;

-- GetToolSetRevision reads ONE set revision INCLUDING ITS PINS (E25 T7). The list projection carries a
-- digest and a revision number but not tool_pins, so "which tools did I actually grant?" had no answer on
-- the public API — which the tree recorded as a `ponytail:` note on ListToolSets: "no single-resource GET
-- /v1/tool-sets/{id} … Add one if a console needs it". A console does.
--
-- BOTH set_name AND id ARE IN THE PREDICATE. The route is /v1/tool-sets/{set}/revisions/{revision_id} and
-- a revision belongs to exactly one set, so a revision of set A read under set B's name must be a 404
-- rather than a row: otherwise the path segment would be decorative and a screen could show one set's
-- contents under another set's heading.
-- name: GetToolSetRevision
SELECT id, set_name, revision_number, digest, tool_pins, published_at IS NOT NULL, created_at
FROM tool_set_revisions
WHERE id = $1 AND set_name = $2 AND organization_id = $3 AND project_id = $4;

-- InsertToolRevision creates a DRAFT revision (published_at NULL). revision_number is the tool's next
-- monotonic number, computed in-statement so a revise never has to read-then-write. Returns it.
-- ponytail: the MAX+1 subselect can race two concurrent inserts to the same number; the
-- UNIQUE(tool_id, revision_number) constraint then rejects the loser (retry on 23505 if it ever matters).
-- GetTool reads a tool lineage within scope (spec §28.2, E13 T4). A foreign/unknown id returns no row.
-- name: GetTool
SELECT id, canonical_name, model_visible_name, created_at
FROM tools
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- ListTools pages a project's tool lineages newest-first (spec §28.2, E13 T4). Tenant-scoped by RLS;
-- cursor + created_at bounds only.
-- name: ListTools
SELECT id, canonical_name, model_visible_name, created_at
FROM tools
WHERE organization_id = $1 AND project_id = $2
  AND ($3::timestamptz IS NULL OR created_at >= $3)
  AND ($4::timestamptz IS NULL OR created_at <= $4)
  AND ($5::timestamptz IS NULL OR (created_at, id) < ($5, $6))
ORDER BY created_at DESC, id DESC
LIMIT $7;

-- ListToolSetRevisions pages a project's tool-set revisions newest-first (spec §28.4, E13 T4). A set has
-- no lineage table (it is named directly), so the list is its revisions; published names the flip.
-- name: ListToolSetRevisions
SELECT id, set_name, revision_number, digest, published_at IS NOT NULL, created_at
FROM tool_set_revisions
WHERE organization_id = $1 AND project_id = $2
  AND ($3::timestamptz IS NULL OR created_at >= $3)
  AND ($4::timestamptz IS NULL OR created_at <= $4)
  AND ($5::timestamptz IS NULL OR (created_at, id) < ($5, $6))
ORDER BY created_at DESC, id DESC
LIMIT $7;

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
--
-- E23 T5: the operator's approval declaration ($4/$5, 000044 R3) rides the SAME once-only flip, because
-- publishing IS the per-tool decision an operator already makes (jira-mcp-connection.md §3c) and a second
-- ceremony would be a second thing to forget. Riding the guarded UPDATE has a consequence worth naming:
-- a re-publish changes nothing, so a gate cannot be quietly REMOVED from a published revision by calling
-- this again without the flag. Un-gating a tool means publishing a new revision and re-pinning the set —
-- visible, immutable, and exactly the shape of every other config change here.
-- name: PublishToolRevision
UPDATE tool_revisions
SET published_at = clock_timestamp(),
    approval_required = $4,
    approval_label = $5
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
--
-- E23 T5: LIMIT 2, NOT 1, AND THE SECOND ROW IS A REFUSAL RATHER THAN A DISCARD. The chain can genuinely
-- produce two candidates for one model-visible name — two published revisions of the same tool pinned by
-- two sets the run's revision names, which is what re-discovery plus a second approval leaves behind — and
-- a bare LIMIT 1 picked between them by whatever the planner emitted first. Measured 2026-07-29: two rows,
-- no ORDER BY, no rule. That was survivable while every candidate ran the same way; it stopped being
-- survivable the moment a revision carries approval_required, because the coin flip decides WHETHER A HUMAN
-- IS ASKED. The ORDER BY only makes the reported pair stable — the detection is the second row itself.
-- name: LookupRunTool
SELECT trv.executor, trv.description, trv.input_schema, trv.output_schema, trv.replay_class,
       trv.executor_config, trv.secret_ref, trv.timeout_ms, t.canonical_name, trv.revision_number,
       trv.approval_required, trv.approval_label
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
ORDER BY trv.revision_number DESC, trv.id
LIMIT 2;
