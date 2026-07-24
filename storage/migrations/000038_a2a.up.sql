-- 000038 adds the A2A 1.0 server-projection tables (E17 Task 2, spec §38.1-38.6): a published A2A interface
-- (the Agent Card revision + auth policy) and the external A2A task/context ref that bridges an A2A id to a
-- CANONICAL run/session. The bridge is the load-bearing §38.2 invariant: an A2A-supplied task/context id is
-- an EXTERNAL reference stored BESIDE the canonical run_id/session_id — it never replaces them, so a client
-- can never rebind a run to an id it controls.
--
-- All CREATE ... IF NOT EXISTS, so the whole chain stays re-runnable. Both tables carry organization_id +
-- project_id and take the standard tenant policy (M3: a new tenant-scoped table asserts its OWN policy here
-- rather than leaning on 000029's boot sweep; tests/security/tenancy fails a table that ships without
-- ENABLE+FORCE). They were created AFTER 000029's blanket GRANT, so they need their own grant too.

-- A published A2A interface: the projection of a published AgentRevision to an A2A Agent Card (§38.1). The
-- card-visible columns (name/description/version/modes/skills/auth) are the SAFE projection — the source
-- revision's provider model name, internal tool inventory, and instructions are NEVER stored here (the
-- projection dropped them at publish, ProjectInterface in adapters/integrations/a2a). agent_revision_id is a
-- provenance pin only; it is never rendered onto a card. etag is the cacheable card revision tag.
CREATE TABLE IF NOT EXISTS a2a_interfaces (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    version TEXT NOT NULL DEFAULT '1',
    agent_profile_id TEXT NOT NULL,
    agent_revision_id TEXT NOT NULL,
    streaming BOOLEAN NOT NULL DEFAULT true,
    push_notifications BOOLEAN NOT NULL DEFAULT true,
    extended_card BOOLEAN NOT NULL DEFAULT true,
    input_modes TEXT[] NOT NULL DEFAULT ARRAY['text/plain']::text[],
    output_modes TEXT[] NOT NULL DEFAULT ARRAY['application/json']::text[],
    skills JSONB NOT NULL DEFAULT '[]'::jsonb,
    auth_scheme TEXT NOT NULL DEFAULT 'bearer',
    published BOOLEAN NOT NULL DEFAULT true,
    etag TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id)
);

-- The external A2A task/context ref (§38.2). One row per A2A task: the EXTERNAL a2a_task_id / a2a_context_id
-- the client sees, bridged to the CANONICAL run_id / session_id the platform minted (never replaced by an
-- A2A id). UNIQUE(interface_id, a2a_task_id) makes the external id stable per interface. push_configs holds
-- the task's A2A push-notification targets (§38, A2A-003) as a JSONB array — a set/get/list/delete surface
-- without a third table. HONEST posture (M-3): each entry's bearer token is stored here and REDACTED on every
-- read, with RLS confining the row — it is NOT a secret_ref handle, and DELIVERY is not wired in this phase.
CREATE TABLE IF NOT EXISTS a2a_task_refs (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    interface_id TEXT NOT NULL REFERENCES a2a_interfaces (id) ON DELETE CASCADE,
    a2a_task_id TEXT NOT NULL,
    a2a_context_id TEXT NOT NULL,
    run_id TEXT NOT NULL,
    session_id TEXT NOT NULL,
    push_configs JSONB NOT NULL DEFAULT '[]'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id),
    UNIQUE (interface_id, a2a_task_id)
);

-- List an interface's tasks newest-first (the tasks list endpoint), and resolve a run's task-ref.
CREATE INDEX IF NOT EXISTS a2a_task_refs_interface_idx
    ON a2a_task_refs (interface_id, created_at DESC);

-- Each tenant table asserts its own policy (M3). Both carry project_id, so has_project=true; the CALL is
-- idempotent (the procedure DROPs+CREATEs the policy).
CALL palai_apply_tenant_policy('a2a_interfaces', 'organization_id', true);
CALL palai_apply_tenant_policy('a2a_task_refs', 'organization_id', true);

-- These tables were created AFTER 000029's blanket `GRANT ... ON ALL TABLES`, so that sweep never saw them:
-- a new table needs its own grant or the runtime role fails closed with "permission denied" instead of the
-- row-scoped policy. a2a_task_refs needs UPDATE (push_configs edits, updated_at); interfaces get full CRUD.
GRANT SELECT, INSERT, UPDATE, DELETE ON a2a_interfaces TO palai_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON a2a_task_refs TO palai_app;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO palai_app;

INSERT INTO schema_migrations (version) VALUES (38) ON CONFLICT DO NOTHING;
