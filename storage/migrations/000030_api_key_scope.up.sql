-- 000030 makes the tenancy provisioning API's key store enforceable (E13 Task 2, TEN-003/MCI-001) and
-- folds in three least-privilege hardening steps the T1 RLS review deferred here (M1, M2, M5-adjacent).
--
-- The api_keys table (000001) gains two columns the provisioning API writes and VerifyAPIKey enforces:
--   scopes      a coarse capability set (empty = unrestricted, the ConfigPolicy §9.3 idiom). The
--               provisioning surface requires the `provision` capability; a key minted with a narrower
--               set (e.g. {run}) authenticates but cannot open a tenant. HONEST CEILING: this is basic
--               scopes only — named roles, relationships, and OIDC are E13-H/E17.
--   expires_at  a hard expiry enforced at verify time: past expires_at resolves to invalid_token, so a
--               provisioned key can be handed out with a lifetime instead of living until revoked.
--
-- api_keys carries organization_id (+ project_id), so migration 000029's catalogue loop already secured
-- it with the tenant_isolation policy on every boot. This migration RE-ASSERTS that policy explicitly
-- after the ALTER — belt-and-suspenders, and the living demonstration of the rule stated in storage/embed.go:
--
--   M3 RULE FOR EVERY LATER MIGRATION (T3+): a new tenant-scoped table (one carrying organization_id) MUST
--   call palai_apply_tenant_policy in its OWN migration. 000029's loop covers it on the next boot, but
--   tests/security/tenancy fails a table that ships without ENABLE+FORCE, so make the policy explicit where
--   the table is born rather than relying on the sweep. api_keys below is the pattern to copy.

ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS scopes TEXT[] NOT NULL DEFAULT '{}';
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ;

-- has_project=true: api_keys has project_id, so a project-narrowed connection sees only its own project's
-- keys. The provisioning API deliberately widens to the whole organization (palai.project_id empty) so an
-- org admin manages every project's keys — see internal/identity. VerifyAPIKey reads under the system scope,
-- which every tenant policy admits. This CALL is idempotent (the procedure DROPs+CREATEs the policy).
CALL palai_apply_tenant_policy('api_keys', 'organization_id', true);

-- M1 (least-privilege): the migration ledger is written ONLY by the RESET-ROLE owner path (coordinator
-- asOwner / storage.MigrationUp). 000029's blanket `GRANT ... ON ALL TABLES` handed the non-owner runtime
-- role write access it never needs, so revoke the writes here. SELECT is retained: it is not a tenant
-- table (nonTenantTables), operators and the component migration assertions read the ledger, and only the
-- writes are the concern.
REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON schema_migrations FROM palai_app;

-- M2 (deployment portability): every pool acquisition runs `SET ROLE palai_app` (storage.OpenPool), which
-- requires the connecting login role to be a MEMBER of palai_app. On the local/compose stack the URL is a
-- superuser, for which SET ROLE always works; on a typical managed Postgres the URL is a NON-superuser owner
-- that is not a member, so without this grant the app cannot switch roles and every query would run as the
-- owner with RLS inert. Grant membership when the current role is neither palai_app itself nor already a
-- member (superusers report membership via pg_has_role, so compose is a no-op). This cannot escalate: the
-- current role is the migration owner, which already holds every privilege palai_app does. The fail-safe
-- alternative — point PALAI_DATABASE_URL at a palai_app LOGIN role directly — needs no grant at all.
DO $$
BEGIN
    IF current_user <> 'palai_app' AND NOT pg_has_role(current_user, 'palai_app', 'MEMBER') THEN
        EXECUTE format('GRANT palai_app TO %I', current_user);
    END IF;
END
$$;

INSERT INTO schema_migrations (version) VALUES (30) ON CONFLICT DO NOTHING;
