-- Reverse 000022: drop the schedule tables (children before parents — schedule_occurrences references
-- schedules). DROP ... IF EXISTS keeps the rollback idempotent even after an earlier migration has already
-- removed a dependency (the 000019/000020/000021 pattern).

DROP TABLE IF EXISTS schedule_occurrences;
DROP TABLE IF EXISTS schedules;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 22;
    END IF;
END
$$;
