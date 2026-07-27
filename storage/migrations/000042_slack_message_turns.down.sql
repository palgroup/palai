-- Reverse 000042: drop the Slack message -> turn handle and the withdrawn marker. The table's
-- tenant_isolation policy, UNIQUE constraint and palai_app grants are dropped with it (none can outlive the
-- table), so no explicit DROP POLICY / REVOKE is needed.
--
-- A rollback FORGETS EVERY RETRACTION: the responses come back into the history the model is shown, because
-- the column that hid them is gone. That is the honest consequence of rolling back and is stated rather than
-- worked around — the alternative (leaving the column behind) would be a schema the forward migration cannot
-- re-create idempotently on the next roll forward.
DROP TABLE IF EXISTS slack_message_turns;

ALTER TABLE IF EXISTS responses
    DROP COLUMN IF EXISTS retracted_at;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 42;
    END IF;
END
$$;
