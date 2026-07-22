-- Reverse 000027: drop the runs rider first, then the revision table (its FK references skills), then
-- the skill lineage table. IF EXISTS keeps the down idempotent.
ALTER TABLE runs DROP COLUMN IF EXISTS skill_pins;
DROP TABLE IF EXISTS skill_revisions;
DROP TABLE IF EXISTS skills;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 27;
    END IF;
END
$$;
