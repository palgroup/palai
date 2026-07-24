-- A2A 1.0 server-projection persistence (spec §38, E17 Task 2, A2A-001..003). a2a_interfaces is the
-- published Agent Card projection (card columns are SAFE — the source revision's model/tools/instructions
-- were dropped at publish). a2a_task_refs bridges an EXTERNAL A2A task/context id to the CANONICAL
-- run/session id (§38.2 — the canonical id is never replaced by an A2A-supplied id).
--
-- ResolveA2AInterfacePublic is the ONLY system-scoped read: it serves the UNAUTHENTICATED public Agent Card,
-- keyed by the server-minted interface id. It returns only card-visible SAFE columns, so a public read
-- leaks nothing internal; the interface was explicitly published for discovery. Every other query is
-- tenant-scoped (RLS + the org/project predicate as defence in depth): the authenticated bearer scope
-- governs (§38.6), never anything a client supplies.

-- name: InsertA2AInterface
INSERT INTO a2a_interfaces (
    id, organization_id, project_id, name, description, version,
    agent_profile_id, agent_revision_id, streaming, push_notifications, extended_card,
    input_modes, output_modes, skills, auth_scheme, published, etag)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17);

-- ResolveA2AInterfacePublic serves the UNAUTHENTICATED public card, keyed by the server-minted interface id.
-- System-scoped: there is no bearer scope on the public card route. It returns ONLY the card-visible SAFE
-- columns the public card actually renders — NOT the owning org/project or the agent_profile/agent_revision
-- provenance pins (the public card path does no follow-on tenant work and never renders provenance), so a
-- public read reaches nothing beyond what the card shows (M-5).
-- name: ResolveA2AInterfacePublic
SELECT id, name, description, version,
       streaming, push_notifications, extended_card, input_modes, output_modes, skills, auth_scheme, etag
FROM a2a_interfaces
WHERE id = $1 AND published = true;

-- GetA2AInterface reads an interface within the authenticated scope (the extended card + all authed ops).
-- RLS confines the row; the org/project predicate is defence in depth. A foreign scope finds nothing (404,
-- no existence oracle).
-- name: GetA2AInterface
SELECT id, organization_id, project_id, name, description, version, agent_profile_id, agent_revision_id,
       streaming, push_notifications, extended_card, input_modes, output_modes, skills, auth_scheme, etag
FROM a2a_interfaces
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- ListA2AInterfaces pages a project's published interfaces newest-first (admin ListView envelope).
-- name: ListA2AInterfaces
SELECT id, name, version, agent_profile_id, agent_revision_id, created_at
FROM a2a_interfaces
WHERE organization_id = $1 AND project_id = $2
ORDER BY created_at DESC, id DESC
LIMIT $3;

-- InsertA2ATaskRef records the external->canonical bridge for a newly-admitted A2A task. run_id/session_id
-- are the platform-minted canonical ids; a2a_task_id/a2a_context_id are the external ids the client sees.
-- name: InsertA2ATaskRef
INSERT INTO a2a_task_refs (
    id, organization_id, project_id, interface_id, a2a_task_id, a2a_context_id, run_id, session_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- GetA2ATaskRef resolves a task within scope by its external a2a_task_id under an interface. It returns the
-- canonical run_id/session_id so the caller can project the live run state — the canonical id is READ here,
-- never overwritten by anything the client passes.
-- name: GetA2ATaskRef
SELECT id, a2a_task_id, a2a_context_id, run_id, session_id, push_configs
FROM a2a_task_refs
WHERE interface_id = $1 AND a2a_task_id = $2 AND organization_id = $3 AND project_id = $4;

-- GetA2ATaskRefByRun resolves an existing task ref within scope by its canonical run reference under an
-- interface (the A2A-retry dedupe seam — a replayed messageId re-admits to the SAME canonical response, so
-- the external task minted the first time is reused, not duplicated). At most one ref exists per
-- (interface_id, run_id) once dedupe holds; LIMIT 1 is defensive.
-- name: GetA2ATaskRefByRun
SELECT id, a2a_task_id, a2a_context_id, run_id, session_id, push_configs
FROM a2a_task_refs
WHERE interface_id = $1 AND run_id = $2 AND organization_id = $3 AND project_id = $4
LIMIT 1;

-- ListA2ATaskRefs pages an interface's tasks newest-first (the tasks list endpoint).
-- name: ListA2ATaskRefs
SELECT id, a2a_task_id, a2a_context_id, run_id, session_id, push_configs
FROM a2a_task_refs
WHERE interface_id = $1 AND organization_id = $2 AND project_id = $3
ORDER BY created_at DESC, id DESC
LIMIT $4;

-- UpdateA2ATaskPushConfigs replaces a task's push-config array (set/delete both write the whole array). The
-- ACTUAL secret posture (M-3): each entry's bearer token is stored in the JSONB and REDACTED on every read
-- (server.redactPush), with RLS confining the row to its tenant — it is NOT a secret_ref handle. Adopting the
-- webhook store's secret_ref indirection for push tokens is later hardening (§6). updated_at bumps for audit.
-- name: UpdateA2ATaskPushConfigs
UPDATE a2a_task_refs
SET push_configs = $5, updated_at = clock_timestamp()
WHERE interface_id = $1 AND a2a_task_id = $2 AND organization_id = $3 AND project_id = $4;
