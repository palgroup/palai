-- `approvals.allowed_approver` leaves the schema. It never held anything and it never closed anything.
--
-- ‼️ THIS IS NOT A NEW MEASUREMENT. E23's plan recorded it on 2026-07-29 as D5, in these words: *"HİÇBİR
-- ŞEY TUTMUYOR … ağaçta onu OKUYAN tek bir satır yok"* — nothing holds it, and not one line in the tree
-- reads it. `docs/operations/approvals.md` has told operators the same thing in its own voice ever since:
-- "That column exists, is written with a value no caller ever sets, and is read by nothing. It is not a
-- control and never was; this list is." What was missing was the last step, and this is it.
--
-- WHAT THE CONTROL ACTUALLY IS, so the removal cannot be read as a weakening: the PROJECT's approver list,
-- checked in one place — `approverAuthorizedTx`, raising ErrApproverNotAuthorized — for every decision on
-- every path. That mechanism shipped with E23 T2, which is the task D5 gave rise to. Dropping this column
-- takes nothing away from it, because nothing ever joined the two.
--
-- MEASURED AGAIN BEFORE DROPPING, since a column with no reader looks exactly like one whose reader you
-- failed to find:
--
--   grep -rn 'AllowedApprover\b' --include='*.go' . | grep -v AllowedApprovers
--     -> 2 hits, both in packages/coordinator/publication.go: the struct field and the INSERT that spends
--        it. NOTHING SETS IT, in production or in a test, so its value was always "".
--   SELECT statements over `approvals` naming the column
--     -> 0 of 7. (apps/slack-bot's `AllowedApprovers` is a different thing entirely — that bot's own
--        per-bot list, in its own row, plural.)
--
-- The two INSERTs stop naming it in the same commit; this statement is what stops the schema promising it.
ALTER TABLE approvals DROP COLUMN IF EXISTS allowed_approver;

INSERT INTO schema_migrations (version) VALUES (10) ON CONFLICT DO NOTHING;
