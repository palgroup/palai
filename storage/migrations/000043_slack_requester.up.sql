-- 000043 gives the Slack return leg a DURABLE REQUESTER (E21 T3, plan §3.6 D4). The agent can now ask the
-- person who asked it — "which branch should I publish?" — and asking someone needs their id at the moment
-- the answer is posted.
--
-- WHY A MIGRATION AT ALL, since the id already arrives with every event: it arrives IN MEMORY and nothing
-- else. slack.Event.UserID lives for the length of one Admit call; none of the three Slack tables had a user
-- column, the idempotency reservation is keyed team+event_id, and the prompt deliberately excludes it. The
-- thing that SENDS the mention is the reply pump — a durable, retrying worker that reads slack_reply_deliveries
-- across process restarts, minutes or hours after Admit returned. A field in memory cannot reach it. (The
-- same measurement corrected a comment in slack_admit.go that claimed an operator who needs to know who wrote
-- "still has it": for the user id, before this migration, nobody had it.)
--
-- TWO COLUMNS, ONE FACT, and the pair is what makes the freeze possible. EnqueueTerminalSlackReply runs inside
-- the run's terminal transaction (coordinator.applyRunTransitionTx) and is handed the run's own identifiers —
-- org, project, run, response, session, state. It cannot be handed a Slack user id without teaching the
-- coordinator about Slack, so the id has to be READABLE from a row that transaction can already see:
--
--   * slack_message_turns.requester_user_id — written at ADMISSION, next to the turn handle, by the one
--     function that holds the event. That row already answers "which turn did this message become"; it now
--     also answers "and who wrote it".
--   * slack_reply_deliveries.requester_user_id — COPIED at enqueue from the turn that opened the run, exactly
--     as channel_id and thread_ts are copied there today, and for the same reason that table's own comment
--     gives: the destination is frozen because the source can legitimately disappear afterwards (a retention
--     purge reaps the response and cascades the turn handle away). A mention that vanished because retention
--     ran between the terminal transaction and the eighth delivery attempt would be a notification lost to
--     an unrelated schedule.
--
-- DEFAULT '' IS THE FAIL-CLOSED VALUE, not a filler. Every row written before today carries it, and the
-- renderer's rule for an empty id is to send the words WITHOUT a mention — never an invented id, never a
-- fallback to <!here>. So the backfill is the empty string on purpose: there is no honest way to reconstruct
-- who asked a question that was answered last week, and guessing at a person to notify is worse than not
-- notifying one.
--
-- An ALTER on tables that are already under tenant policy, so no palai_apply_tenant_policy call and no new
-- grant: both tables keep the policies and privileges 000041/000042 gave them. Guarded and idempotent (a
-- second boot re-runs the whole chain), following 000042's ALTER on responses.

ALTER TABLE slack_message_turns
    ADD COLUMN IF NOT EXISTS requester_user_id TEXT NOT NULL DEFAULT '';

ALTER TABLE slack_reply_deliveries
    ADD COLUMN IF NOT EXISTS requester_user_id TEXT NOT NULL DEFAULT '';

INSERT INTO schema_migrations (version) VALUES (43) ON CONFLICT DO NOTHING;
