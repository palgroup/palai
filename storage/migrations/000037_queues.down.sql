-- Reverse 000037: drop the queue-adapter tables. Each table's tenant_isolation policy, indexes, UNIQUE
-- constraints, and palai_app grants are dropped with it (none can outlive its table), so no explicit
-- DROP POLICY / REVOKE is needed. queue_messages/queue_effect_receipts/queue_deliveries reference
-- queue_connections, so drop them first. A rollback loses queued/in-flight messages and undelivered
-- outbound results; the runs themselves are untouched.
DROP TABLE IF EXISTS queue_deliveries;
DROP TABLE IF EXISTS queue_effect_receipts;
DROP TABLE IF EXISTS queue_messages;
DROP TABLE IF EXISTS queue_connections;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 37;
    END IF;
END
$$;
