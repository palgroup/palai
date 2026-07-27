-- 000042 makes a Slack EDIT and a Slack DELETE act on the turn they name (E20, SLK-005). It adds two things,
-- and they are two halves of one sentence: `slack_message_turns` says WHICH turn a Slack message became, and
-- `responses.retracted_at` says a turn has been WITHDRAWN from the conversation.
--
-- WHY EITHER IS NEEDED, and it is a defect found against the owner's real workspace: a `message_deleted`
-- arriving inside a thread the app held satisfied the run-birth rule ("a follow-up in a thread we hold"), so
-- the app answered the deletion — "it seems you deleted your message, would you like help with something
-- else?". A deletion is not a turn. But refusing to answer it is only half: since history carries the USER's
-- prior turns, a message the human deleted goes on being shown to the model forever unless something retracts
-- it, and a user who deletes a message reasonably expects it to stop influencing the agent.
--
-- Acting on "the turn a message became" needs a handle from the Slack message to the response, and nothing in
-- the tree had one. The idempotency reservation is keyed by Slack's EVENT id, and a deletion arrives under a
-- NEW event id naming an OLD message ts — the two identities never meet. slack_thread_sessions is per THREAD
-- (a thread holds many turns). Hence one row per admitted message, written next to the thread claim.
CREATE TABLE IF NOT EXISTS slack_message_turns (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL REFERENCES organizations (id),
    project_id TEXT NOT NULL REFERENCES projects (id),
    connection_id TEXT NOT NULL REFERENCES slack_connections (id) ON DELETE CASCADE,
    team_id TEXT NOT NULL,
    channel_id TEXT NOT NULL,
    -- The HUMAN's message ts, which is the id Slack reuses: an edit does not change it, and a deletion names
    -- it in `previous_message.ts`. It is therefore the only stable handle a later event can present.
    message_ts TEXT NOT NULL,
    -- The turn that message became. ON DELETE CASCADE, so a retention purge that reaps the response takes the
    -- handle with it rather than leaving a row pointing at nothing.
    response_id TEXT NOT NULL REFERENCES responses (id) ON DELETE CASCADE,
    session_id TEXT NOT NULL REFERENCES sessions (id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id),
    -- One turn per message. The write is ON CONFLICT DO NOTHING, which is what makes a redelivery (SLK-002
    -- replays onto the SAME response) and the message.channels TWIN of a top-level mention — same ts, second
    -- delivery — leave the FIRST response as the turn rather than re-pointing the handle.
    UNIQUE (organization_id, project_id, team_id, channel_id, message_ts)
);

CALL palai_apply_tenant_policy('slack_message_turns', 'organization_id', true);
-- Created after 000029's blanket GRANT ... ON ALL TABLES, so this table needs its own grant or the runtime
-- role fails closed with "permission denied" rather than with the row-scoped policy.
GRANT SELECT, INSERT, UPDATE, DELETE ON slack_message_turns TO palai_app;

-- retracted_at is the WITHDRAWN fact, and it lives on responses rather than on the Slack table above because
-- the thing being withdrawn is a canonical turn: SessionHistory is what assembles run.start's conversation
-- (execution.historyMessages), it knows nothing of Slack, and a filter it cannot apply is not a filter. It is
-- the purged_at column's shape (000002) for a different reason — purged means retention reaped the content,
-- retracted means the human took the words back.
--
-- IT HIDES, IT DOES NOT ERASE: the row keeps what was said and now also records that it was withdrawn, so an
-- operator can still answer "what happened in this session". Only the history shown to the MODEL drops it.
-- Scrubbing the words from the database as well is a data-retention decision (§22.2 owns purge) and is
-- deliberately not made here.
ALTER TABLE responses
    ADD COLUMN IF NOT EXISTS retracted_at TIMESTAMPTZ;

INSERT INTO schema_migrations (version) VALUES (42) ON CONFLICT DO NOTHING;
