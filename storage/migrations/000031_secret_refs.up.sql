-- 000031 adds the durable secret-ref store behind the restart-less secret write-path (E13 Task 3,
-- SEC-002/MCI-002). Until now every tenant secret lived only in the env-file bridge
-- (PALAI_*_SECRET_FILE_<ORG>__<REF>, main.go), so rotating one meant editing a file and restarting the
-- process. This table lets a tenant POST a secret value over the API; the resolver reads the latest
-- version fresh on the next request, so a rotation takes effect with NO restart. A ref absent from this
-- store still falls back to the env-file bridge (the E09 credential-broker seam is preserved).
--
-- The value is stored envelope-encrypted at rest: `ciphertext` is a single master-key AES-256-GCM sealed
-- blob (nonce || sealed(value)), never the plaintext. HONEST CEILING: one master key held by the process
-- (PALAI_SECRET_MASTER_KEY_FILE) — a KMS backend + one-operation audience/fence lease ceremony is E13-H
-- (SEC-001/003). The value has NO read-back path: the API returns only metadata (name/version/updated_at).
--
-- A rotation is a NEW version row — (organization_id, name, version) is UNIQUE and the resolver reads the
-- MAX(version) — so the write-path is append-only per name and the version history is retained.
--
-- secret_refs carries organization_id (NO project_id: it fronts the org-scoped env bridge), so per the M3
-- rule (storage/migrations/000030_api_key_scope.up.sql) it MUST assert its own policy in THIS migration:
-- ENABLE + FORCE row level security + the org policy, born secured rather than leaning on 000029's boot
-- sweep. The org always comes from the verified key (never a body field), so a ref names only its own org.
CREATE TABLE IF NOT EXISTS secret_refs (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL REFERENCES organizations (id),
    name TEXT NOT NULL,
    version INTEGER NOT NULL,
    ciphertext BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (organization_id, name, version)
);

-- has_project=false: the store is org-scoped, matching the org-only env-file bridge it fronts.
CALL palai_apply_tenant_policy('secret_refs', 'organization_id', false);

-- secret_refs is the FIRST table created after 000029's blanket `GRANT ... ON ALL TABLES`, so that sweep
-- never saw it — a new table needs its own grant or the runtime role fails closed with "permission denied
-- for table secret_refs" instead of the row-scoped policy. Append-only (INSERT on create/rotate, SELECT on
-- resolve/list/get; never UPDATE/DELETE), so grant only those two — the 000015/000017 append-only precedent.
GRANT SELECT, INSERT ON secret_refs TO palai_app;

INSERT INTO schema_migrations (version) VALUES (31) ON CONFLICT DO NOTHING;
