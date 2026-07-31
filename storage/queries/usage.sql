-- Usage metering: the append-only settlement ledger and the two durable admission limits read against
-- it (spec §43, E13 Task 6, migration 000032). Metering ONLY — no price, no invoice, no adjustment
-- (E13-H/SaaS). Every statement here filters project_id explicitly: 000032 secures these three tables
-- at the ORGANIZATION level on purpose (so an org-wide limit can be summed from a project-narrowed
-- admission connection), which means the intra-organization narrowing is this file's job, not RLS's.

-- SettleUsage records one settled meter fact. Both $1 (the row id) and $8 (the dedupe key) are derived
-- by the caller from the settling operation's own identity, so a redelivered model step re-derives the
-- same row and settles exactly once: the conflict is the whole point (BIL-001, replay without double
-- settlement).
--
-- The conflict target is NAMED, and that is deliberate: an untargeted ON CONFLICT DO NOTHING would also
-- swallow a PRIMARY KEY collision, and the id is a 96-bit hash — two DIFFERENT (tenant, dedupe_key)
-- triples that happened to hash equal would silently lose a settlement, which in a money path is the one
-- failure mode worse than an error. Naming the dedupe unique keeps the replay a no-op while a genuine id
-- collision (or any unique constraint added later) raises 23505 loudly instead of dropping revenue.
-- name: SettleUsage
INSERT INTO usage_ledger (id, organization_id, project_id, session_id, run_id, meter, quantity, unit, dedupe_key)
VALUES ($1, $2, $3, nullif($4, ''), nullif($5, ''), $6, $7, $9, $8)
ON CONFLICT (organization_id, project_id, dedupe_key) DO NOTHING;

-- ExhaustedBudget returns the caller's first budget whose cumulative settled usage since period_start
-- has reached its limit, or no row when every budget still has headroom.
--
-- meter_prefix is matched with LIKE, so a `%` or `_` a tenant puts in its OWN prefix behaves as a SQL
-- wildcard rather than a literal (documented, not escaped: it can only widen or narrow that tenant's own
-- limit, never reach another tenant's rows, and `_` only ever makes a limit match MORE meters — it fails
-- closed). Escape it if meter prefixes ever become anything but the fixed vocabulary this phase writes. A budget row with project_id=''
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
-- silently forgive the spend already recorded in the current period, and a budget created TODAY must not
-- retroactively charge yesterday's. Nothing advances it afterwards, so a budget is a LIFETIME cap in this
-- phase whose only remediation is a higher limit_quantity (billing-period rollover is E13-H).
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

-- UsageSeries is the BUCKETED read behind GET /v1/usage/series: quantity per meter per time bucket for
-- the caller's scope, so a dashboard draws a chart from one aggregate instead of paging the ledger.
-- UsageTotals answers "how much, ever"; this answers "how much, when", and neither is derivable from
-- the other without a page loop the client should not be running.
--
-- $3 is the bucket unit and Go has ALREADY restricted it to 'hour' or 'day' before it arrives — it is
-- the only parameter interpolated into an interval literal below, and a bound parameter is a VALUE at
-- execution time, never re-parsed as SQL, so the concatenation cannot inject. The allow-list is what
-- keeps it a bucket the index and the row ceiling were sized for, not what keeps it safe.
--
-- date_trunc's THREE-argument form is used deliberately. The two-argument form truncates a timestamptz
-- in the SESSION's TimeZone, so the same window would bucket differently against a server whose
-- timezone drifted from the container default — a silent, invisible re-slicing of every chart. Pinning
-- 'UTC' makes a bucket mean the same instant everywhere. It requires PostgreSQL 16; the deployed image
-- is pinned by digest and IS 16:
--   docker inspect palai-<id>-postgres-1 --format '{{.Config.Image}}'
--     -> postgres@sha256:17e67d7b9890c99b055ba1e0d5c5be4ec27c9d3a72bda32db24a5e5d8a85af0c
--   psql -tAc 'SHOW server_version;' -> 16.14 (Debian 16.14-1.pgdg13+1)   (2026-07-31)
-- and scripts/test/component:25 pins that SAME digest, so the tier that proves this cannot drift from
-- the tier that runs it.
--
-- ZERO-FILL. `present` collects the (meter, unit) pairs that actually occur in the window and the final
-- CROSS JOIN gives each of them EVERY bucket, so a meter that stopped spending renders as zeros rather
-- than as absent points a chart would silently close over. The fill is deliberately scoped to the
-- window: a window with no rows at all returns NO points, because nothing here knows which meters the
-- caller expected, and inventing a vocabulary would be a claim this table cannot support.
--
-- `present` is derived FROM `agg` rather than from a second pass over usage_ledger, and that is not a
-- tidiness point — it halves the work and removes a duplicated predicate. Measured on 500k seeded rows
-- (2026-07-31, see the migration 000049 header for the harness):
--   two ledger scans: Buffers: shared hit=10081, Execution Time: 103.040 ms
--   one ledger scan:  Buffers: shared hit=5042,  Execution Time:  56.024 ms
-- The second copy of the WHERE clause was also the kind this tree keeps paying for: two predicates that
-- must agree forever, where a filter added to one and not the other silently changes which meters the
-- chart admits without changing any number on it.
--
-- ORDERING. (bucket_start, meter, unit) is the CROSS JOIN's own key — generate_series emits each bucket
-- once, `present` holds each (meter, unit) once (DISTINCT), so the triple is unique per output row by
-- construction and the ORDER BY is TOTAL. `ORDER BY bucket_start` alone would NOT be: grouping by meter
-- puts several rows in every bucket, and their order between runs would be whatever the join happened
-- to emit — a chart drawn from an arbitrary interleaving of meters.
-- name: UsageSeries
WITH bounds AS (
    SELECT date_trunc($3::text, $4::timestamptz, 'UTC') AS series_start,
           $5::timestamptz                              AS series_end
),
agg AS (
    SELECT date_trunc($3::text, l.occurred_at, 'UTC') AS bucket_start,
           l.meter, l.unit, sum(l.quantity) AS quantity, count(*) AS entries
    FROM usage_ledger l, bounds b
    WHERE l.organization_id = $1 AND ($2 = '' OR l.project_id = $2)
      AND l.occurred_at >= b.series_start AND l.occurred_at <= b.series_end
      AND ($6 = '' OR l.meter = $6)
    GROUP BY 1, 2, 3
),
present AS (
    SELECT DISTINCT meter, unit FROM agg
)
SELECT g.bucket_start, p.meter, p.unit,
       coalesce(a.quantity, 0), coalesce(a.entries, 0)
FROM bounds b
CROSS JOIN generate_series(b.series_start, b.series_end, ('1 ' || $3::text)::interval) AS g(bucket_start)
CROSS JOIN present p
LEFT JOIN agg a ON a.bucket_start = g.bucket_start AND a.meter = p.meter AND a.unit = p.unit
ORDER BY g.bucket_start, p.meter, p.unit;

-- ListUsageLedger is the shared keyset page over the ledger (the same (created_at, id) cursor every
-- E13 T4 list uses; occurred_at is this table's created_at). The ledger carries no lifecycle state, so
-- there is no status filter — the handler rejects ?status= for this kind.
-- $8 (session) and $9 (meter) are the two narrowings a dashboard asks for: "what did this session cost"
-- and "input tokens only". '' means NO predicate rather than "match the empty value" — the same idiom $2
-- already uses for project — so an unfiltered page is unaffected and cannot silently return nothing.
-- ORDER BY stays (occurred_at DESC, id DESC): total, which a keyset cursor requires. Narrowing the WHERE
-- does not change the ordering, so a cursor minted on an unfiltered page still resolves.
-- name: ListUsageLedger
SELECT id, schema_version, project_id, session_id, run_id, meter, quantity, unit, occurred_at
FROM usage_ledger
WHERE organization_id = $1 AND ($2 = '' OR project_id = $2)
  AND ($3::timestamptz IS NULL OR occurred_at >= $3)
  AND ($4::timestamptz IS NULL OR occurred_at <= $4)
  AND ($5::timestamptz IS NULL OR (occurred_at, id) < ($5, $6))
  AND ($8 = '' OR session_id = $8)
  AND ($9 = '' OR meter = $9)
ORDER BY occurred_at DESC, id DESC
LIMIT $7;
