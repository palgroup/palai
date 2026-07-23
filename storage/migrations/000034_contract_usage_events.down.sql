-- Reverse 000034. A CONTRACT is one-way by design: this migration does NOT re-create usage_events. The
-- table had zero readers and no data, so there is nothing to restore; a real downgrade that somehow
-- needed it would restore from a pre-upgrade backup (the rollback-window discipline in
-- docs/operations/upgrade.md), not from this down file. Rolling the full chain down then back up is still
-- clean: 000001 re-creates usage_events at the head and this migration drops it again at the tail, so
-- "up from scratch" and "down then up" reach the same end state — usage_events absent.
--
-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 34;
    END IF;
END
$$;
