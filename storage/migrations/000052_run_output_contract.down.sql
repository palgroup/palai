-- Reverse 000052. Guarded on the target still existing, because MigrationDown() is one concatenated
-- chain and re-running it must stay a clean no-op — 000051's, 000050's and 000049's reversals are
-- written the same way.
--
-- THE HONEST SHAPE OF THIS ROLLBACK: dropping output_contract discards the schema every in-flight
-- run was admitted under. A run that crosses this line mid-flight loses the contract its caller
-- stated, so its answer is no longer checked against anything and it terminates `completed` on
-- whatever the model produced — which is precisely the pre-000052 behaviour, and precisely the
-- defect. It does not corrupt anything and no credential is involved, but a caller who demanded a
-- schema and got prose across a rollback was told the truth by neither side. Drain structured-output
-- runs before crossing it.
ALTER TABLE IF EXISTS runs DROP COLUMN IF EXISTS output_contract;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 52;
    END IF;
END
$$;
