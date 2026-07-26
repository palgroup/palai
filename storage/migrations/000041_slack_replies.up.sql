-- E19: the Slack RETURN LEG. A mention births a run (000035 + E19 T1) and the run reaches a terminal state,
-- but nothing wrote the answer back — the loop was open and a human who asked a question in Slack was left
-- staring at silence.
--
-- This table is the durable ORDER TO POST, and it exists for one reason: the enqueue runs INSIDE the run's
-- terminal transaction (coordinator.applyRunTransitionTx), so "the run is terminal" and "its answer is
-- recorded for delivery" commit together. There is no window in which a run finished and the intent to reply
-- was lost, however long the poster is down or absent. That is the 000037 queue_deliveries pattern verbatim,
-- and it is deliberately the same shape so an operator reads one delivery model, not two.
--
-- WHY A TABLE AT ALL, since the earlier E19 tasks took no migration: exactly-once needs durable per-RUN
-- state, and nothing existing carries it. slack_thread_sessions.last_bot_message_ts is per THREAD (a thread
-- holds many runs), the §24.5 outbox is documented as one row per aggregate transition, and queue_deliveries
-- is keyed by a queue_connection_id this path has none of. A retried terminal transaction, a reconciled
-- cancel, or a restarted poster each have to collapse onto ONE visible message; UNIQUE (run_id) is what
-- makes that a database fact rather than a hope.
CREATE TABLE IF NOT EXISTS slack_reply_deliveries (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL REFERENCES organizations (id),
    project_id TEXT NOT NULL REFERENCES projects (id),
    connection_id TEXT NOT NULL REFERENCES slack_connections (id) ON DELETE CASCADE,
    -- The run whose answer this delivers. UNIQUE, and it is the whole exactly-once claim: the enqueue is
    -- ON CONFLICT DO NOTHING, so however many times a run's terminal transaction runs, one row exists and
    -- one message is posted.
    run_id TEXT NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    response_id TEXT NOT NULL DEFAULT '',
    -- The destination is FROZEN at enqueue time rather than re-derived at post time. A thread correlation
    -- can legitimately be deleted afterwards (repairDeadCorrelation clears a dead session's row), and an
    -- answer that lost its address because the NEXT message repaired the thread would be an answer nobody
    -- ever sees.
    channel_id TEXT NOT NULL,
    thread_ts TEXT NOT NULL,
    -- The run's terminal state, so the poster renders "here is the answer" or "this did not finish" without
    -- re-reading the run (and without racing a retention purge).
    run_state TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending', 'delivered', 'dead')),
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    max_attempts INTEGER NOT NULL DEFAULT 8 CHECK (max_attempts > 0),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    -- The ts Slack assigned the posted message. Recorded for the same reason SLK-006 records one on the
    -- thread: it is the handle any later repair of THIS message edits.
    message_ts TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id),
    UNIQUE (run_id)
);

CREATE INDEX IF NOT EXISTS slack_reply_deliveries_due_idx
    ON slack_reply_deliveries (next_attempt_at) WHERE state = 'pending';

-- The enqueue is a lookup by the terminating run's SESSION, on every terminal transition in the deployment —
-- including the overwhelming majority that have nothing to do with Slack. Unindexed that is a sequential
-- scan of slack_thread_sessions inside the hot terminal transaction.
CREATE INDEX IF NOT EXISTS slack_thread_sessions_session_idx
    ON slack_thread_sessions (session_id);

CALL palai_apply_tenant_policy('slack_reply_deliveries', 'organization_id', true);
-- Created after 000029's blanket GRANT ... ON ALL TABLES, so this table needs its own grant or the runtime
-- role fails closed with "permission denied" rather than with the row-scoped policy. UPDATE is the poster's
-- (delivered / reschedule / dead); DELETE is retention's when the run it hangs off is reaped.
GRANT SELECT, INSERT, UPDATE, DELETE ON slack_reply_deliveries TO palai_app;

INSERT INTO schema_migrations (version) VALUES (41) ON CONFLICT DO NOTHING;
