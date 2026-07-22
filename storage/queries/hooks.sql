-- Hooks registry management + dispatch-load resolution (spec §28.17, E12 Task 8, TOL-012). Create + disable
-- are the admin management surface; HooksForPoint is the per-run dispatch load that walks a project's enabled
-- hooks for one point in deterministic (created_at, id) registration order. Every statement is tenant-scoped
-- by (organization_id, project_id).

-- name: InsertHook
INSERT INTO hooks (id, organization_id, project_id, name, hook_point, category, executor, config, secret_ref, timeout_ms)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);

-- GetHook reads a hook's committed shape (admin read-back + the CRUD roundtrip), disabled or not.
-- name: GetHook
SELECT id, name, hook_point, category, executor, config, secret_ref, timeout_ms, disabled_at IS NOT NULL
FROM hooks
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- HooksForPoint loads a project's ENABLED hooks for one point in registration order (created_at, id) — the
-- documented deterministic firing sequence. A disabled hook (disabled_at set) is skipped. This is the ONLY
-- read the run dispatch loop issues per fire point.
-- name: HooksForPoint
SELECT id, hook_point, category, executor, config, secret_ref, timeout_ms
FROM hooks
WHERE organization_id = $1 AND project_id = $2 AND hook_point = $3 AND disabled_at IS NULL
ORDER BY created_at, id;

-- DisableHook flips the admin kill-switch once (a re-disable is a zero-row no-op).
-- name: DisableHook
UPDATE hooks
SET disabled_at = clock_timestamp()
WHERE id = $1 AND organization_id = $2 AND project_id = $3 AND disabled_at IS NULL
RETURNING id;

-- HookExists verifies a hook id is in scope (disambiguates an unknown hook from an already-disabled one).
-- name: HookExists
SELECT 1 FROM hooks WHERE id = $1 AND organization_id = $2 AND project_id = $3;
