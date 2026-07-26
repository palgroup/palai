-- Reverse 000041: drop the Slack return-leg outbox. Its tenant_isolation policy, indexes, UNIQUE constraint
-- and palai_app grants are dropped with the table (none can outlive it), so no explicit DROP POLICY /
-- REVOKE is needed. The session index on slack_thread_sessions is dropped separately — that table is
-- 000035's and survives this rollback. A rollback loses undelivered replies; the runs and the answers they
-- produced are untouched (the reply is a rendering of /v1/responses, never the record of it).
DROP TABLE IF EXISTS slack_reply_deliveries;
DROP INDEX IF EXISTS slack_thread_sessions_session_idx;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 41;
    END IF;
END
$$;
