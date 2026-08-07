-- The E19/E20/E23 Slack tables, removed because the cutover took their last reader and their last writer
-- and left the tables behind.
--
-- WHAT REPLACED THEM: apps/slack-bot, a separate application with its OWN connection pool and its OWN
-- migrations (apps/slack-bot/migrations/0001_thread_sessions.sql). The correlation these tables existed
-- for still happens; it happens over there. store/threads.go says so in its own words — "What IS kept from
-- 000035_slack.up.sql's slack_thread_sessions" — which is the sentence that makes these five the OLD
-- implementation rather than a second half of the new one.
--
-- MEASURED 2026-08-07 BEFORE DROPPING ANYTHING, because a table with no reader looks exactly like a table
-- whose reader you failed to find:
--
--   storage/queries/slack.sql                      -> deleted in the cutover; no query file names them
--   grep -rn '<table>' --include='*.go' apps/ adapters/ packages/ cmd/ | grep -v _test | grep -vE ':\s*//'
--                                                  -> 0 for all five (only past-tense comments remain,
--                                                     "was written here and is GONE (cutover, 2026-08-05)")
--   information_schema FKs pointing INTO them from a non-slack table
--                                                  -> 0
--   SELECT count(*) on all five on the live plane  -> 0, 0, 0, 0, 0
--
-- THE ORDER IS THE FOREIGN KEYS' AND NOT PREFERENCE, and there is no CASCADE on purpose: slack_reply_
-- deliveries and slack_thread_sessions both reference slack_connections, so the children go first and the
-- parent last. CASCADE would have worked and would also have dropped anything I had failed to measure,
-- which is the difference between a drop that is verified and a drop that is merely successful.
DROP TABLE IF EXISTS slack_approval_deliveries;
DROP TABLE IF EXISTS slack_message_turns;
DROP TABLE IF EXISTS slack_reply_deliveries;
DROP TABLE IF EXISTS slack_thread_sessions;
DROP TABLE IF EXISTS slack_connections;

-- Their RLS policies go with them: a policy is owned by its table, so 000002's five
-- `CALL palai_apply_tenant_policy('slack_…', 'project_id')` lines leave nothing behind here. That is worth
-- stating because the reverse is the failure this tree has recorded — a rule whose selector went empty
-- still reads as live — and the check is `SELECT count(*) FROM pg_policies WHERE tablename LIKE 'slack\_%'`
-- -> 0 after this applies.

INSERT INTO schema_migrations (version) VALUES (9) ON CONFLICT DO NOTHING;
