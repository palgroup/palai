-- Reverse 000043: drop the durable requester. The columns' DEFAULT and NOT NULL go with them; both tables
-- keep their tenant policy and grants, which were never this migration's to give.
--
-- A rollback makes every pending reply MENTION NOBODY — that is the fail-closed path the forward migration
-- documents, reached by removing the id rather than by never having had one. The answers still land in their
-- threads; the person who asked simply is not pinged. Nothing else in the tree reads these columns.
ALTER TABLE IF EXISTS slack_reply_deliveries
    DROP COLUMN IF EXISTS requester_user_id;

ALTER TABLE IF EXISTS slack_message_turns
    DROP COLUMN IF EXISTS requester_user_id;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 43;
    END IF;
END
$$;
