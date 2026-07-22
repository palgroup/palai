-- Usage metering: the append-only settlement ledger and the two durable admission limits read against
-- it (spec §43, E13 Task 6, migration 000032). Metering ONLY — no price, no invoice, no adjustment
-- (E13-H/SaaS). Every statement here filters project_id explicitly: 000032 secures these three tables
-- at the ORGANIZATION level on purpose (so an org-wide limit can be summed from a project-narrowed
-- admission connection), which means the intra-organization narrowing is this file's job, not RLS's.

-- SettleUsage records one settled meter fact. Both $1 (the row id) and $8 (the dedupe key) are derived
-- by the caller from the settling operation's own identity, so a redelivered model step re-derives the
-- same row and settles exactly once: the conflict is the whole point (BIL-001, replay without double
-- settlement). ON CONFLICT DO NOTHING covers both the primary key and the tenant-scoped dedupe unique.
-- name: SettleUsage
INSERT INTO usage_ledger (id, organization_id, project_id, session_id, run_id, meter, quantity, unit, dedupe_key)
VALUES ($1, $2, $3, nullif($4, ''), nullif($5, ''), $6, $7, $9, $8)
ON CONFLICT DO NOTHING;

-- ExhaustedBudget returns the caller's first budget whose cumulative settled usage since period_start
-- has reached its limit, or no row when every budget still has headroom. A budget row with project_id=''
-- covers the whole organization (it sums every project's rows); a concrete project narrows both the
-- budget and the sum to that project. meter_prefix matches by prefix, so 'model.' caps every model
-- meter and '' caps everything.
--
-- The join is LEFT so a budget with zero usage still evaluates (and trivially fails the HAVING). Ordered
-- by meter_prefix so the reported limit is stable when several are exhausted at once.
-- name: ExhaustedBudget
SELECT b.meter_prefix, b.limit_quantity, coalesce(sum(l.quantity), 0), b.period_start
FROM budgets b
LEFT JOIN usage_ledger l
       ON l.organization_id = b.organization_id
      AND (b.project_id = '' OR l.project_id = b.project_id)
      AND l.meter LIKE b.meter_prefix || '%'
      AND l.occurred_at >= b.period_start
WHERE b.organization_id = $1 AND b.project_id IN ('', $2)
GROUP BY b.id, b.meter_prefix, b.limit_quantity, b.period_start
HAVING coalesce(sum(l.quantity), 0) >= b.limit_quantity
ORDER BY b.meter_prefix
LIMIT 1;

-- ExhaustedQuota is ExhaustedBudget over a ROLLING window instead of a period. It also returns the
-- OLDEST in-window row's timestamp: that row is the first to age out, so oldest + window is the honest
-- moment capacity next releases — the stable reset information the 429 remediation body carries.
-- name: ExhaustedQuota
SELECT q.meter_prefix, q.limit_quantity, coalesce(sum(l.quantity), 0), q.window_seconds, min(l.occurred_at)
FROM quotas q
LEFT JOIN usage_ledger l
       ON l.organization_id = q.organization_id
      AND (q.project_id = '' OR l.project_id = q.project_id)
      AND l.meter LIKE q.meter_prefix || '%'
      AND l.occurred_at >= now() - make_interval(secs => q.window_seconds)
WHERE q.organization_id = $1 AND q.project_id IN ('', $2)
GROUP BY q.id, q.meter_prefix, q.limit_quantity, q.window_seconds
HAVING coalesce(sum(l.quantity), 0) >= q.limit_quantity
ORDER BY q.meter_prefix
LIMIT 1;

-- UpsertBudget sets the caller's limit for one (scope, meter prefix). A budget is mutable config, so a
-- re-POST of the same prefix RESTATES the limit rather than minting a second row that would race the
-- first — the unique key is the identity. period_start is only set on creation: raising a limit must not
-- silently forgive the spend already recorded in the current period.
-- name: UpsertBudget
INSERT INTO budgets (id, organization_id, project_id, meter_prefix, limit_quantity)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (organization_id, project_id, meter_prefix)
DO UPDATE SET limit_quantity = EXCLUDED.limit_quantity, updated_at = clock_timestamp()
RETURNING id, project_id, meter_prefix, limit_quantity, period_start, updated_at;

-- ListBudgets shows the caller everything that binds it: the organization-wide limits plus its own
-- project's. An org-scoped caller ($2 = '') sees every project's limits, since that is its whole scope.
-- name: ListBudgets
SELECT id, project_id, meter_prefix, limit_quantity, period_start, updated_at
FROM budgets
WHERE organization_id = $1 AND ($2 = '' OR project_id IN ('', $2))
ORDER BY project_id, meter_prefix;

-- UpsertQuota is UpsertBudget for the rolling-window limit; the window itself is restated too, since a
-- quota's window is part of the limit a caller is setting.
-- name: UpsertQuota
INSERT INTO quotas (id, organization_id, project_id, meter_prefix, limit_quantity, window_seconds)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (organization_id, project_id, meter_prefix)
DO UPDATE SET limit_quantity = EXCLUDED.limit_quantity, window_seconds = EXCLUDED.window_seconds, updated_at = clock_timestamp()
RETURNING id, project_id, meter_prefix, limit_quantity, window_seconds, updated_at;

-- name: ListQuotas
SELECT id, project_id, meter_prefix, limit_quantity, window_seconds, updated_at
FROM quotas
WHERE organization_id = $1 AND ($2 = '' OR project_id IN ('', $2))
ORDER BY project_id, meter_prefix;

-- UsageTotals is the metering-visibility summary: one line per meter for the caller's scope. $2 is the
-- caller's project ('' widens to the whole organization, which is what an org-scoped key sees).
-- name: UsageTotals
SELECT meter, unit, sum(quantity), count(*)
FROM usage_ledger
WHERE organization_id = $1 AND ($2 = '' OR project_id = $2)
GROUP BY meter, unit
ORDER BY meter;

-- ListUsageLedger is the shared keyset page over the ledger (the same (created_at, id) cursor every
-- E13 T4 list uses; occurred_at is this table's created_at). The ledger carries no lifecycle state, so
-- there is no status filter — the handler rejects ?status= for this kind.
-- name: ListUsageLedger
SELECT id, schema_version, project_id, session_id, run_id, meter, quantity, unit, occurred_at
FROM usage_ledger
WHERE organization_id = $1 AND ($2 = '' OR project_id = $2)
  AND ($3::timestamptz IS NULL OR occurred_at >= $3)
  AND ($4::timestamptz IS NULL OR occurred_at <= $4)
  AND ($5::timestamptz IS NULL OR (occurred_at, id) < ($5, $6))
ORDER BY occurred_at DESC, id DESC
LIMIT $7;
