-- Hooks registry management + dispatch-load resolution (spec §28.17, E12 Task 8, TOL-012). Create + disable
-- are the admin management surface; HooksForPoint is the per-run dispatch load that walks a project's enabled
-- hooks for one point in deterministic (created_at, id) registration order. Every statement is tenant-scoped
-- by project_id (000062 rekeyed the policy). Rows still CARRY organization_id: hooks has a UNIQUE
-- index over it, and a unique index treats NULL as distinct from NULL.

-- name: InsertHook
INSERT INTO hooks (id, organization_id, project_id, name, hook_point, category, executor, config, secret_ref, timeout_ms)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);

-- GetHook reads a hook's committed shape (admin read-back + the CRUD roundtrip), disabled or not. It backs
-- GET /v1/hooks/{id} (E29 T1) and the store's own roundtrip.
--
-- It returns disabled_at as an INSTANT rather than the boolean it used to derive, and created_at beside it.
-- Both are what ListHooks needs to page and to answer the question `disable` leaves open: the boolean says
-- a hook is off, the instant says since when — and created_at is the keyset column the list orders by, so
-- the two reads project the same nineteen bytes of truth rather than two different subsets.
-- name: GetHook
SELECT id, name, hook_point, category, executor, config, secret_ref, timeout_ms, disabled_at, created_at
FROM hooks
WHERE id = $1 AND project_id = $2;

-- ListHooks pages a project's hooks newest-first (GET /v1/hooks, E29 T1). Tenant-scoped by
-- project_id AND by RLS; cursor + created_at bounds.
--
-- IT RETURNS DISABLED HOOKS TOO, and that is the whole reason it exists rather than reusing HooksForPoint
-- below. HooksForPoint takes a POINT and filters `disabled_at IS NULL` — it is the dispatch loop's read,
-- the set of hooks that will fire. A management list built on it would make POST /v1/hooks/{id}/disable a
-- write with NO read-back: the hook would vanish from the only enumeration there is, which is
-- indistinguishable from a hook that was deleted, and hooks cannot be deleted.
--
-- The projection is GetHook's, field for field, so a list row and a singular read are one shape.
-- ORDER BY is total: (created_at, id), both descending, for the reason ListSchedules states.
-- name: ListHooks
SELECT id, name, hook_point, category, executor, config, secret_ref, timeout_ms, disabled_at, created_at
FROM hooks
WHERE project_id = $1
  AND ($2::timestamptz IS NULL OR created_at >= $2)
  AND ($3::timestamptz IS NULL OR created_at <= $3)
  AND ($4::timestamptz IS NULL OR (created_at, id) < ($4, $5))
ORDER BY created_at DESC, id DESC
LIMIT $6;

-- HooksForPoint loads a project's ENABLED hooks for one point in registration order (created_at, id) — the
-- documented deterministic firing sequence. A disabled hook (disabled_at set) is skipped. This is the ONLY
-- read the run dispatch loop issues per fire point.
-- name: HooksForPoint
SELECT id, hook_point, category, executor, config, secret_ref, timeout_ms
FROM hooks
WHERE project_id = $1 AND hook_point = $2 AND disabled_at IS NULL
ORDER BY created_at, id;

-- DisableHook flips the admin kill-switch once (a re-disable is a zero-row no-op).
-- name: DisableHook
UPDATE hooks
SET disabled_at = clock_timestamp()
WHERE id = $1 AND project_id = $2 AND disabled_at IS NULL
RETURNING id;

-- HookExists verifies a hook id is in scope (disambiguates an unknown hook from an already-disabled one).
-- name: HookExists
SELECT 1 FROM hooks WHERE id = $1 AND project_id = $2;
