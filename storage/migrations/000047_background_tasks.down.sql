-- Reverse 000047. Guarded on the target still existing, because MigrationDown() is one concatenated
-- chain and re-running it must stay a clean no-op — 000046's, 000045's and 000044's reversals are
-- written the same way.
--
-- THE HONEST SHAPE OF THIS ROLLBACK: dropping this table does not stop a single process. The rows are
-- the only record that a container labelled io.palai.bg or a host process group belongs to this
-- program, so a rollback across this line converts every live background task into an ORPHAN — a
-- process nothing remembers, with a log file in an allocation nobody will read. An operator rolling
-- back past 47 with tasks running should kill them first; docs/operations/background-execution.md
-- (E26 T7) says so in the same words.
--
-- The index goes with the table; it is named here only so the reversal reads as the exact inverse of
-- the forward file rather than relying on the implicit drop.
DROP INDEX IF EXISTS background_tasks_running_idx;
DROP TABLE IF EXISTS background_tasks;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 47;
    END IF;
END
$$;
