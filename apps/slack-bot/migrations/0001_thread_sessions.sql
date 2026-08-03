-- 0001 creates thread_sessions: the (bot, team, channel, thread) -> Palai session correlation this
-- bot's Socket Mode handler (Task 9) and relay (Task 7) key every event on. It is the whole of this
-- schema so far, and it deliberately carries NONE of storage/migrations/000035_slack.up.sql's four
-- foreign keys (organizations, projects, slack_connections, sessions) even though it plays the same
-- role slack_thread_sessions does there — see apps/slack-bot/internal/store/threads.go for why: this
-- bot lives in its own database and does not own, and cannot reference, any Palai table.
--
-- The composite PRIMARY KEY stands in for that table's separate `id TEXT PRIMARY KEY` +
-- `UNIQUE (org, project, team, channel, thread)` pair: nothing outside this schema ever addresses a
-- row by a surrogate id (no API exposes one, nothing else references one), so the natural key IS the
-- primary key. The one property kept unchanged from 000035's
-- UNIQUE (organization_id, project_id, team_id, channel_id, thread_ts) is that a second event in the
-- SAME thread resolves the SAME session (see threads.go BindThread), and a concurrent race collapses
-- at this constraint (23505) rather than minting two sessions. bot_id is NEW here — the old table had
-- no such column — because two different bots (an iOS bot and an Android bot) may sit in the same
-- Slack channel and thread, and must not share a session.
CREATE TABLE IF NOT EXISTS thread_sessions (
    bot_id     TEXT NOT NULL,
    team_id    TEXT NOT NULL,
    channel_id TEXT NOT NULL,
    thread_ts  TEXT NOT NULL,
    session_id TEXT NOT NULL,               -- Palai's own session id string, never a REFERENCES; see threads.go.
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (bot_id, team_id, channel_id, thread_ts)
);
