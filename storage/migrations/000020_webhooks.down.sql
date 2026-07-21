-- Reverse 000020: drop the webhook tables (children before parents) and the events journal_id rider,
-- before 000001 drops events itself. DROP ... IF EXISTS keeps the rollback idempotent even after an
-- earlier migration has already removed a dependency (the 000016/000018 pattern).

DROP TABLE IF EXISTS delivery_attempts;
DROP TABLE IF EXISTS webhook_deliveries;
DROP TABLE IF EXISTS webhook_endpoints;

DROP INDEX IF EXISTS events_journal_id_idx;
ALTER TABLE events DROP COLUMN IF EXISTS journal_id;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 20;
    END IF;
END
$$;
