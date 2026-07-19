-- ChildRun subagents (spec §11, §25.18-19). A run may delegate bounded sub-work to a
-- ChildRun: a runs row that carries parent_run_id (its delegating run) and depth (0 for a
-- root run, parent.depth+1 for a child). The delegation column carries the run's delegation
-- context as JSON — on a root run configured with required delegations, {"emit":[<specs>]}
-- (the child.request specs its engine emits); on a child run, {"spec":{...}} (its own
-- role/objective/model/tools/budget/required/workspace_mode). A plain run has neither and
-- leaves it NULL.
--
-- Only ALTER TABLE ADD COLUMN IF NOT EXISTS on a table 000001 created, so no new grants are
-- needed and the whole chain stays safe to re-run (Migrate is idempotent).
ALTER TABLE runs ADD COLUMN IF NOT EXISTS parent_run_id TEXT REFERENCES runs (id);
ALTER TABLE runs ADD COLUMN IF NOT EXISTS depth INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS delegation JSONB;

-- One-active-root is per session and counts ROOT runs only (spec §22.3, §22.8): a child run
-- shares its parent's session but must not consume the session's single root slot, so the
-- 000006 partial unique index gains "AND parent_run_id IS NULL" (the handoff 000006 named).
-- 000006 already created the index (root-only predicate absent), so this DROPs and re-CREATEs
-- it with the child-excluding predicate. DROP-then-CREATE (not CREATE IF NOT EXISTS) is what
-- makes the predicate change actually take on a chain where 000006 created the old shape; it
-- stays idempotent because a re-run drops and recreates the same new-predicate index.
DROP INDEX IF EXISTS runs_one_active_root_per_session;
CREATE UNIQUE INDEX runs_one_active_root_per_session
    ON runs (session_id)
    WHERE state NOT IN ('completed', 'failed', 'canceled', 'timed_out', 'budget_exceeded')
      AND parent_run_id IS NULL;

INSERT INTO schema_migrations (version) VALUES (7) ON CONFLICT DO NOTHING;
