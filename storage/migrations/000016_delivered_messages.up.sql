-- Durable delivered-message rows (spec §26.9, §22.4; E10 Task 2 — the reclaim-crash-mid-fold
-- closure of the E08 Task 4 M1/R1 debt, command_pump.go). A send_message delivered at a run's
-- safe boundary lives only in the engine subprocess's memory until a model step folds it. A crash
-- between apply and that fold (variant-1) or a pause/resume after it (R1) drops the turn: the
-- command is already drained (single-winner WHERE state='queued') so nothing redelivers it, and
-- run.start carries prior responses only. This row makes each boundary delivery durable so a fresh
-- attempt redelivers it at its ORIGINAL boundary during reconstruction — command.applied.v1 now
-- means the delivered message is durable, not just in memory.
--
-- The row REFERENCES the command; the customer content stays in commands.payload (no second copy),
-- so the existing commands.payload secret discipline covers it (no new content surface to scan).
--
-- CREATE TABLE / INDEX ... IF NOT EXISTS keep Migrate idempotent per-boot, matching 000004.

CREATE TABLE IF NOT EXISTS delivered_messages (
    -- The send_message command whose apply delivered this message. A command applies exactly once
    -- (single-winner), so it delivers at most one message: (org, project, command_id) is the key,
    -- and the write is ON CONFLICT DO NOTHING idempotent.
    command_id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    -- The run the message was delivered into — the redelivery + fold-mark scope.
    run_id TEXT NOT NULL REFERENCES runs (id),
    -- The model_request_id of the step at whose boundary the message was delivered. The engine
    -- derives it deterministically from (run, step), so it re-identifies the SAME input boundary on
    -- every attempt (§26.9: a redelivered message folds at an input boundary, never inside a step).
    -- Nullable only in principle — the boundary pump runs after a continuing model step, so a real
    -- delivery always carries the step it followed.
    boundary_request_id TEXT,
    -- The journal sequence of this delivery's command.applied.v1 (spec §22.4) — the canonical order
    -- key when several messages share one boundary.
    applied_sequence BIGINT NOT NULL,
    -- delivered -> folded: 'delivered' at apply, 'folded' once the following model step commits (so
    -- variant-1, the crash before that commit, is distinguishable from R1, the crash after it).
    fold_state TEXT NOT NULL DEFAULT 'delivered',
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (organization_id, project_id, command_id),
    -- The command carries the content and delivery mode this row references (spec §24.3 system of
    -- record); its own FKs guarantee the tenant, so no separate projects FK is needed here.
    FOREIGN KEY (organization_id, project_id, command_id) REFERENCES commands (organization_id, project_id, id)
);

-- The redelivery read and the fold-mark both scope by run; the redelivery additionally keys on the
-- boundary, so one composite index serves both.
CREATE INDEX IF NOT EXISTS delivered_messages_run_boundary_idx
    ON delivered_messages (run_id, boundary_request_id);

INSERT INTO schema_migrations (version) VALUES (16) ON CONFLICT DO NOTHING;
