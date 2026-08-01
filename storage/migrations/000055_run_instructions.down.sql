-- Reverse 000055. Guarded on the target still existing, because MigrationDown() is one concatenated
-- chain and re-running it must stay a clean no-op — 000054's, 000053's and 000051's reversals are
-- written the same way.
--
-- THE HONEST SHAPE OF THIS ROLLBACK: dropping `instructions` discards the run-specific instruction
-- layer every in-flight run was admitted under. A run that crosses this line mid-flight has its next
-- model step assembled WITHOUT the caller's instructions, so a run told "answer in one word" starts
-- answering in paragraphs halfway through — a silent behaviour change inside one run, not an error.
-- That is the pre-000055 behaviour and precisely the defect. It corrupts nothing and involves no
-- credential. The pinned revision's own instructions are unaffected: they live on
-- agent_revisions.instructions and this migration never touched them.
ALTER TABLE IF EXISTS runs DROP COLUMN IF EXISTS instructions;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 55;
    END IF;
END
$$;
