-- 000062 (tenant policy by project, A.2 Task 2): the isolation boundary moves from ORGANIZATION to
-- PROJECT. Palai is becoming single-tenant per installation (Palai Cloud design: one installation is one
-- customer's engine, and multi-tenancy belongs to the SaaS layer above it now, not the core). This
-- migration does not touch 000029's machinery — the catalogue sweep, ENABLE/FORCE ROW LEVEL SECURITY,
-- and palai_apply_tenant_policy's DROP+CREATE idempotency all stay exactly as they are — it only changes
-- which COLUMN the policy expression reads for the tables listed below.
--
-- Consumes A.2 Task 1 (c766c2ec): every WithTenant connection now carries a mandatory, non-empty
-- project, so keying isolation on project_id no longer starves any caller that used to lean on an
-- empty-project fallback — except the handful of surfaces named below, which this migration deliberately
-- leaves untouched. organization_id stays on every row and palai.org_id keeps being set on every
-- acquisition; Task 4 removes both. Only the policy's key changes here.
--
-- THREE NAMED EXCLUSIONS, and each is a measured decision, not an oversight — a table that carries
-- project_id is swept into the new policy UNLESS it is one of these three:
--
--   api_keys, principals (000001/000030) — both carry organization_id AND project_id, and 000030's own
--   comment states the reason plainly: "The provisioning API deliberately widens to the whole
--   organization (palai.project_id empty) so an org admin manages every project's keys"
--   (apps/control-plane/internal/identity/store.go, orgScope -> storage.WithOrgScope). That widening
--   works ONLY because 000029's has_project=true expression treats an EMPTY CALLER project GUC as "no
--   narrowing, show the whole organization" — the new project-keyed expression below has no such
--   fallback (a caller with no project GUC set matches only rows whose OWN project_id is also '').
--   Sweeping these two tables would silently confine identity/store.go's org-wide listing of api_keys
--   and principals to whichever project happens to carry project_id = '', breaking every other
--   project's key/principal management — a regression this task's tests would not have caught, because
--   the tenancy fixture never seeds a second project's api_keys for one organization. Left exactly as
--   000030 wrote them.
--
--   usage_ledger (000032) — carries organization_id AND project_id but is secured at has_project=false
--   ON PURPOSE, and the reason is a live query, not a hypothetical: ExhaustedBudget/ExhaustedQuota
--   (storage/queries/usage.sql) LEFT JOIN usage_ledger against an organization-wide budget
--   (budgets.project_id = '') with `(b.project_id = '' OR l.project_id = b.project_id)`, to sum EVERY
--   sibling project's spend from the ONE project-scoped connection the admission request opened.
--   usage_ledger.project_id is NEVER '' (every settled row belongs to a real project, enforced by its
--   FK to projects), so the new policy's `OR project_id = ''` fallback would never fire for it — a
--   project-scoped connection would see only its OWN project's usage rows, and an organization-wide
--   (soon: installation-wide) budget would silently sum only the caller's own spend, undercounting every
--   sibling project. That is exactly the fail-OPEN, fail-SILENT failure mode this task's own brief warns
--   about for budgets/quotas directly — just one join away. usage_ledger stays organization_id-keyed so
--   that query keeps seeing what it needs; nothing else reads usage_ledger under a narrower assumption.
--
-- budgets, quotas (000032) DO move, and are the reason this migration ships its own test. Both carry
-- project_id NOT NULL DEFAULT '', and project_id = '' meant "covers the whole organization" there. Once
-- organizations are gone that becomes "covers the whole installation", and an installation-wide limit
-- must stay visible to every project or it silently applies to nothing — see the OR branch below and
-- TestInstallationWideRowsAreVisibleToEveryProject.
--
-- The policy body, rebuilt for project_id. has_project stays in the signature only so
-- palai_apply_tenant_policy's call shape (target, tenant_key, has_project) does not have to change for
-- any caller; the new expression ignores it, because project_id is now the WHOLE key rather than an
-- optional narrowing layered atop organization_id the way 000029's has_project split modeled it.
--
-- `current_setting(''palai.project_id'', true) IS NOT NULL` GATES the `= ''''` fallback, and it is load-
-- bearing, not decoration: the first cut of this expression was the un-gated `%1$s = current_setting(...)
-- OR %1$s = ''''`, and it failed tests/security/tenancy's own
-- TestConnectionWithoutTenantContextSeesNoTenantRows — a connection whose project GUC was never set at
-- all (NULL, the "forgotten scope" case the test exists to catch) could still see any installation-wide
-- row, because `project_id = ''''` does not reference the caller at all. storage.OpenPool's PrepareConn
-- already refuses to acquire a connection with no scope (A.2 Task 1, ErrProjectRequired), so in
-- production this path is unreachable through the Go wrapper — but this corpus drives raw pgx on purpose
-- to prove the DATABASE denies it too, independent of that Go-level refusal (the package's own stated
-- reason for bypassing the wrapper). The IS NOT NULL guard requires the caller to have declared SOME
-- project scope — even the empty string, which WithOrgScope-shaped callers would set explicitly — before
-- the installation-wide fallback can apply; a GUC that was simply never set sees nothing, on every table.
CREATE OR REPLACE FUNCTION palai_tenant_policy_expression(tenant_key TEXT, has_project BOOLEAN)
RETURNS TEXT
LANGUAGE sql IMMUTABLE
AS $$
    SELECT format(
        'coalesce(current_setting(''palai.system'', true), '''') = ''on'' '
        'OR (current_setting(''palai.project_id'', true) IS NOT NULL '
        'AND (%1$s = current_setting(''palai.project_id'', true) OR %1$s = ''''))',
        tenant_key);
$$;

-- The catalogue sweep, 000029's shape, driven off project_id instead of organization_id, with the three
-- named exclusions above. A LATER table that gains a project_id column and is not on this exclusion list
-- picks up the new policy the moment the chain re-runs, the same guarantee 000029 gave for
-- organization_id — and tests/security/tenancy's TestEveryTenantTableIsRowLevelSecured still fails loudly
-- if a genuinely tenant-scoped table is ever left off both.
DO $$
DECLARE entry RECORD;
BEGIN
    FOR entry IN
        SELECT c.relname AS table_name
          FROM pg_class c
          JOIN pg_namespace n ON n.oid = c.relnamespace
         WHERE n.nspname = 'public' AND c.relkind = 'r'
           AND c.relname NOT IN ('api_keys', 'principals', 'usage_ledger')
           AND EXISTS (SELECT 1 FROM information_schema.columns col
                        WHERE col.table_schema = 'public' AND col.table_name = c.relname
                          AND col.column_name = 'project_id')
    LOOP
        CALL palai_apply_tenant_policy(entry.table_name, 'project_id', false);
    END LOOP;
END
$$;

INSERT INTO schema_migrations (version) VALUES (62) ON CONFLICT DO NOTHING;
