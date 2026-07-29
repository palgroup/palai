-- Reverse 000044. The ORDER matters and the first statement is the one that makes the rest legal: R1's
-- generic approvals have no publication, so restoring publication_id NOT NULL fails while they exist.
-- They are deleted rather than migrated, and that is the honest reversal — a rollback to a schema with no
-- concept of a gated tool call cannot keep the gates, and a run parked on one is woken by nothing
-- afterwards. A rollback across this line is an operator decision with that consequence attached.
--
-- Every statement is guarded on the table still existing, because MigrationDown() is one concatenated
-- chain and re-running it (or running it after 000013/000001 already dropped these tables) must stay a
-- clean no-op — 000013's own reversal is written the same way and for the same reason.

DO $$
BEGIN
    IF to_regclass('public.approvals') IS NOT NULL THEN
        DELETE FROM approvals WHERE tool_call_id IS NOT NULL;
    END IF;
END
$$;

DROP TABLE IF EXISTS slack_approval_deliveries;

DO $$
BEGIN
    IF to_regclass('public.tool_revisions') IS NOT NULL THEN
        ALTER TABLE tool_revisions DROP COLUMN IF EXISTS approval_label;
        ALTER TABLE tool_revisions DROP COLUMN IF EXISTS approval_required;
    END IF;
END
$$;

-- R2: back to the two decomposed operations 000013 declared. A merge_pull_request row would violate the
-- restored CHECK, so it goes with the constraint that permitted it.
DO $$
BEGIN
    IF to_regclass('public.publications') IS NOT NULL THEN
        DELETE FROM publications WHERE operation = 'merge_pull_request';
        ALTER TABLE publications DROP CONSTRAINT IF EXISTS publications_operation_check;
        ALTER TABLE publications ADD CONSTRAINT publications_operation_check
            CHECK (operation IN ('push_branch', 'open_pull_request'));
    END IF;
END
$$;

DROP INDEX IF EXISTS approvals_tool_call_expiry;
DROP INDEX IF EXISTS approvals_tool_call_key;

DO $$
BEGIN
    IF to_regclass('public.approvals') IS NOT NULL THEN
        ALTER TABLE approvals DROP CONSTRAINT IF EXISTS approvals_one_target;
        ALTER TABLE approvals DROP COLUMN IF EXISTS tool_call_id;
        ALTER TABLE approvals ALTER COLUMN publication_id SET NOT NULL;
    END IF;
END
$$;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 44;
    END IF;
END
$$;
