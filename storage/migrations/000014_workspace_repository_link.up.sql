-- The session→binding link (E09 Task 10). A coding session's workspace records WHICH repository
-- binding it clones and the ref it was requested at, so the root run's auto-provisioning
-- (Orchestrator.ExecuteAttempt) resolves the binding + ref from the session's own workspace — never
-- from model output. Attach (POST /v1/responses with the contracted `repository` field) creates the
-- workspaces row carrying these; a plain, non-coding workspace leaves them empty (''), so a legacy
-- row is unaffected. The columns are the missing half of §29.7's session binding: 000008 linked a
-- workspace to a session and (optionally) a run, but not to the repository it prepares.
--
-- Idempotent (ADD COLUMN IF NOT EXISTS), matching the 000008 re-runnable pattern; the DEFAULT '' NOT
-- NULL backfills existing rows without a rewrite of dependent code.

ALTER TABLE workspaces
    ADD COLUMN IF NOT EXISTS repository_binding_id TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS requested_ref TEXT NOT NULL DEFAULT '';

INSERT INTO schema_migrations (version) VALUES (14) ON CONFLICT DO NOTHING;
