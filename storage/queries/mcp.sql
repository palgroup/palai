-- MCP connection management + broker-lookup resolution (spec §28.13-28.14, E12 Task 5). Create is the admin
-- management surface; the reads back a connection for discovery and the per-tenant broker lookup's
-- rider-intersected resolution. Every statement is tenant-scoped by (organization_id, project_id).

-- name: InsertMCPConnection
INSERT INTO mcp_connections (id, organization_id, project_id, name, transport, config, secret_ref, trust_level)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- GetMCPConnection reads a connection for a discover action (admin) — tenant-scoped, disabled or not.
-- name: GetMCPConnection
SELECT id, name, transport, config, secret_ref, trust_level, disabled_at IS NOT NULL
FROM mcp_connections
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- MCPConnectionExists verifies a connection id is in scope (an AgentRevision rider names only real
-- connections — the revision-create validation gate).
-- name: MCPConnectionExists
SELECT 1 FROM mcp_connections WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- DisableMCPConnection flips the admin kill-switch once (a re-disable is a zero-row no-op).
-- name: DisableMCPConnection
UPDATE mcp_connections
SET disabled_at = clock_timestamp()
WHERE id = $1 AND organization_id = $2 AND project_id = $3 AND disabled_at IS NULL
RETURNING id;

-- MCPToolIDByCanonical resolves a discovered tool's lineage id by its canonical name, so re-discovery
-- reuses the existing lineage (a new revision, never a duplicate tool).
-- name: MCPToolIDByCanonical
SELECT id FROM tools WHERE organization_id = $1 AND project_id = $2 AND canonical_name = $3;

-- MCPLatestToolRevisionDigest reads a tool's newest revision digest, so re-discovery skips writing an
-- identical revision (no manifest churn) and only inserts a NEW draft when the digest actually changed.
-- name: MCPLatestToolRevisionDigest
SELECT digest FROM tool_revisions WHERE tool_id = $1 ORDER BY revision_number DESC LIMIT 1;

-- MCPConnectionForRun loads an enabled connection ONLY if the run's pinned AgentRevision (or template)
-- mcp_connections rider names it — the capability ceiling as a single tenant-scoped join. A connection not
-- in the rider, disabled, or out of tenant returns no row, so the broker yields ErrUnknownTool for its
-- tools (no silent capability drift, no cross-tenant reach). $4 is the connection id from the tool
-- revision's executor_config->>'connection_id'.
-- name: MCPConnectionForRun
SELECT c.id, c.name, c.transport, c.config, c.secret_ref, c.trust_level
FROM runs r
LEFT JOIN agent_revisions ar ON ar.id = r.agent_revision_id
LEFT JOIN run_template_revisions rtr ON rtr.id = r.run_template_revision_id
JOIN mcp_connections c ON c.id = $4
    AND c.organization_id = r.organization_id AND c.project_id = r.project_id
    AND c.disabled_at IS NULL
    AND c.id IN (SELECT jsonb_array_elements_text(COALESCE(ar.mcp_connections, rtr.mcp_connections, '[]'::jsonb)))
WHERE r.id = $1 AND r.organization_id = $2 AND r.project_id = $3
LIMIT 1;
