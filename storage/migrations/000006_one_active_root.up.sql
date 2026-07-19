-- One active root run per session (spec §22.3, §22.8). A session chains many responses
-- over its life, but at most one root run may be non-terminal at a time: a second
-- concurrent root run is the one-active-root violation, rejected by the database
-- (unique_violation, SQLSTATE 23505) rather than an app-code check-then-insert race.
-- 000003 added the sessions.active_root_run_id pointer as prep; the invariant itself is a
-- partial unique index over the non-terminal run states, mirroring attempts_one_active_per_run
-- (000001) — the same shape the codebase already uses for "one live row per parent".
--
-- The non-terminal predicate is the complement of the RunTable's terminal destinations
-- (packages/state-machines/run.go), so a run counts against its session's slot exactly while it
-- can still take a command, and frees the slot the instant it terminalizes.
--
-- Only a CREATE INDEX IF NOT EXISTS on a table 000001 created, so no new grants are needed and
-- the whole chain stays safe to re-run (Migrate is idempotent).
--
-- ponytail: keyed on session_id over the non-terminal states. Every run is a root run today;
-- when T5 adds runs.parent_run_id for child runs this predicate gains "AND parent_run_id IS
-- NULL" so a child run does not consume its parent session's single root slot.
CREATE UNIQUE INDEX IF NOT EXISTS runs_one_active_root_per_session
    ON runs (session_id)
    WHERE state NOT IN ('completed', 'failed', 'canceled', 'timed_out', 'budget_exceeded');

INSERT INTO schema_migrations (version) VALUES (6) ON CONFLICT DO NOTHING;
