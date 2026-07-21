-- Inbound-trigger auth rider (spec §20.2.2/§21.7, E11 Task 5). 000021 pre-provisioned the whole inbound
-- DATA path (trigger_deliveries.source/source_tenant/source_event_id/raw_payload, the source-dedupe
-- UNIQUE partial index, principal_id), so there is NO delivery-side migration. But three lineage facts
-- on the trigger itself have no home yet, so this ALTER rider adds them (the 000018-rider idiom; ADD
-- COLUMN IF NOT EXISTS with a benign DEFAULT, so the whole chain stays re-runnable; 000001's blanket
-- GRANT already covers triggers).
--
--   created_by               the principal an inbound run admits AS. AdmissionInput.Principal is
--                            load-bearing (§20.9 idempotency is principal-scoped) and an unauthenticated
--                            source POST carries none — the schedules.created_by (000022) precedent for
--                            cron. Stamped at CreateTrigger from the API caller's scope; pre-023 rows
--                            default '' and inbound simply requires it set.
--   inbound_secret_ref       the source-secret HANDLE (never bytes) the receiver verifies under — the
--   inbound_secret_ref_next  webhook_endpoints.SigningSecretRef/Next precedent verbatim. Two refs, not
--                            one: rotation overlap at a trust boundary is a §21.4 requirement, and T4
--                            Verify already speaks multi-secret. Refs live on the trigger (a mutable
--                            endpoint column), NOT on trigger_revisions — rotation must not mint pipeline
--                            revisions, and a droppable credential handle at a trust boundary is unsafe.

ALTER TABLE triggers
    ADD COLUMN IF NOT EXISTS created_by TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS inbound_secret_ref TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS inbound_secret_ref_next TEXT NOT NULL DEFAULT '';

INSERT INTO schema_migrations (version) VALUES (23) ON CONFLICT DO NOTHING;
