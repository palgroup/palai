-- Usage metering: the append-only settlement ledger and the two durable admission limits read against
-- it (spec §43, E13 Task 6, migration 000032). Metering ONLY — no price, no invoice, no adjustment
-- (E13-H/SaaS). Every statement here filters project_id explicitly, and that has outlived its original
-- reason: 000032 secured these three tables at the ORGANIZATION level on purpose (so an org-wide limit
-- could be summed from a project-narrowed admission connection), and 000066 rekeyed them to the
-- INSTALLATION, which is wider still. Either way RLS does not narrow to a project, so the narrowing is
-- this file's job — a statement here that forgot its project_id predicate would read every project's
-- rows, not be denied.

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
INSERT INTO usage_ledger (id, project_id, session_id, run_id, meter, quantity, unit, dedupe_key, model_request_id)
VALUES ($1, $2, nullif($3, ''), nullif($4, ''), $5, $6, $8, $7, nullif($9, ''))
ON CONFLICT (project_id, dedupe_key) DO NOTHING;

-- ExhaustedBudget returns the caller's first budget whose cumulative settled usage since period_start
-- has reached its limit, or no row when every budget still has headroom.
--
-- meter_prefix is matched with LIKE, so a `%` or `_` a tenant puts in its OWN prefix behaves as a SQL
-- wildcard rather than a literal (documented, not escaped: it can only widen or narrow that tenant's own
-- limit, never reach another tenant's rows, and `_` only ever makes a limit match MORE meters — it fails
-- closed). Escape it if meter prefixes ever become anything but the fixed vocabulary this phase writes. A budget row with project_id=''
-- covers the whole installation (it sums every project's rows); a concrete project narrows both the
-- budget and the sum to that project. meter_prefix matches by prefix, so 'model.' caps every model
-- meter and '' caps everything.
--
-- The join is LEFT so a budget with zero usage still evaluates (and trivially fails the HAVING).
--
-- WHICH LIMIT IS REPORTED WHEN SEVERAL ARE EXHAUSTED AT ONCE — a semantic decision, not a tidiness one,
-- and the reason the first ORDER BY key is a boolean.
--
-- `ORDER BY meter_prefix` alone was NOT a total ordering, and the tie was not an accident: the schema's
-- own scope model guarantees it. budgets is UNIQUE on (project_id, meter_prefix)
-- (000032, rebuilt by 000065), so an installation-wide row (project_id = '') and a project row on the SAME meter_prefix are
-- both legal and both live; the WHERE above takes BOTH; the HAVING can exhaust BOTH; and meter_prefix is
-- EQUAL for the two. `LIMIT 1` then picked one, and nothing here said which. Measured 2026-08-01 on the
-- pinned postgres 16.14: the plan is Limit -> Sort(meter_prefix) -> GroupAggregate(Group Key: b.id), so
-- the bounded top-1 sort kept the row with the smaller b.id — and budget ids are crypto/rand
-- (middleware.NewID). Which limit an operator's 429 named was a coin flip fixed when they POSTed it.
--
-- THE NARROWEST LIMIT WINS. `(b.project_id = '') ASC` sorts false before true, so the PROJECT row comes
-- first and the installation-wide row is reported only when it is the only one exhausted. The reason is
-- the 429 itself: its remediation body must name a limit the CALLER CAN ACT ON. A project operator
-- cannot raise the installation's budget, so naming it answers "you are blocked" with something the
-- reader cannot turn into an action — and it is the wrong number twice over, since the installation
-- row's `used` sums every sibling project's spend as well.
--
-- b.id is the LAST key and it is the PRIMARY KEY (000032), so the ordering is now TOTAL: two budgets
-- that tie on scope and prefix cannot exist (the UNIQUE key forbids it), but the tiebreaker is written
-- anyway, because "this can never tie" is exactly the belief that put this query here.
--
-- ENFORCEMENT IS UNCHANGED, and the distinction matters: both rows were enforced before this ordering
-- and both are enforced after. The HAVING catches both and either produces the 429. What changed is
-- which one is NAMED.
-- name: ExhaustedBudget
SELECT b.meter_prefix, b.limit_quantity, coalesce(sum(l.quantity), 0), b.period_start
FROM budgets b
LEFT JOIN usage_ledger l
       ON (b.project_id = '' OR l.project_id = b.project_id)
      AND l.meter LIKE b.meter_prefix || '%'
      AND l.occurred_at >= b.period_start
WHERE b.project_id IN ('', $1)
GROUP BY b.id, b.meter_prefix, b.limit_quantity, b.period_start
HAVING coalesce(sum(l.quantity), 0) >= b.limit_quantity
ORDER BY (b.project_id = '') ASC, b.meter_prefix, b.id
LIMIT 1;

-- ExhaustedQuota is ExhaustedBudget over a ROLLING window instead of a period. It also returns the
-- OLDEST in-window row's timestamp: that row is the first to age out, so oldest + window is the honest
-- moment capacity next releases — the stable reset information the 429 remediation body carries.
--
-- The ordering is ExhaustedBudget's, for ExhaustedBudget's reasons, and it is restated here rather than
-- cross-referenced because the two queries had the same defect and a fix applied to one of them leaves
-- half of it shipped: quotas carries the SAME UNIQUE (project_id, meter_prefix), the
-- same WHERE takes both scopes, and `ORDER BY q.meter_prefix` was equally non-total. Narrowest first
-- (`q.project_id = ''` sorts last), q.id last so the ordering is TOTAL.
-- name: ExhaustedQuota
SELECT q.meter_prefix, q.limit_quantity, coalesce(sum(l.quantity), 0), q.window_seconds, min(l.occurred_at)
FROM quotas q
LEFT JOIN usage_ledger l
       ON (q.project_id = '' OR l.project_id = q.project_id)
      AND l.meter LIKE q.meter_prefix || '%'
      AND l.occurred_at >= now() - make_interval(secs => q.window_seconds)
WHERE q.project_id IN ('', $1)
GROUP BY q.id, q.meter_prefix, q.limit_quantity, q.window_seconds
HAVING coalesce(sum(l.quantity), 0) >= q.limit_quantity
ORDER BY (q.project_id = '') ASC, q.meter_prefix, q.id
LIMIT 1;

-- UpsertBudget sets the caller's limit for one (scope, meter prefix). A budget is mutable config, so a
-- re-POST of the same prefix RESTATES the limit rather than minting a second row that would race the
-- first — the unique key is the identity. period_start is only set on creation: raising a limit must not
-- silently forgive the spend already recorded in the current period, and a budget created TODAY must not
-- retroactively charge yesterday's. Nothing advances it afterwards, so a budget is a LIFETIME cap in this
-- phase whose only remediation is a higher limit_quantity (billing-period rollover is E13-H).
-- name: UpsertBudget
INSERT INTO budgets (id, project_id, meter_prefix, limit_quantity)
VALUES ($1, $2, $3, $4)
ON CONFLICT (project_id, meter_prefix)
DO UPDATE SET limit_quantity = EXCLUDED.limit_quantity, updated_at = clock_timestamp()
RETURNING id, project_id, meter_prefix, limit_quantity, period_start, updated_at;

-- ListBudgets shows the caller everything that binds it: the installation-wide limits plus its own
-- project's. A caller passing $1 = '' sees every project's limits, which since A.2 Task 6 is the
-- `system` key rather than an org-scoped one — there is no org-granular key left to be.
-- name: ListBudgets
SELECT id, project_id, meter_prefix, limit_quantity, period_start, updated_at
FROM budgets
WHERE ($1 = '' OR project_id IN ('', $1))
ORDER BY project_id, meter_prefix;

-- UpsertQuota is UpsertBudget for the rolling-window limit; the window itself is restated too, since a
-- quota's window is part of the limit a caller is setting.
-- name: UpsertQuota
INSERT INTO quotas (id, project_id, meter_prefix, limit_quantity, window_seconds)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (project_id, meter_prefix)
DO UPDATE SET limit_quantity = EXCLUDED.limit_quantity, window_seconds = EXCLUDED.window_seconds, updated_at = clock_timestamp()
RETURNING id, project_id, meter_prefix, limit_quantity, window_seconds, updated_at;

-- name: ListQuotas
SELECT id, project_id, meter_prefix, limit_quantity, window_seconds, updated_at
FROM quotas
WHERE ($1 = '' OR project_id IN ('', $1))
ORDER BY project_id, meter_prefix;

-- UsageTotals is the metering-visibility summary: one line per meter for the caller's scope. $2 is the
-- caller's project ('' widens to the whole installation, which is what a `system` key sees).
-- name: UsageTotals
SELECT meter, unit, sum(quantity), count(*)
FROM usage_ledger
WHERE ($1 = '' OR project_id = $1)
GROUP BY meter, unit
-- ORDER BY (meter, unit), which is the GROUP BY's own key and therefore TOTAL. `ORDER BY meter` alone
-- was not: nothing constrains a meter to a single unit, so two rows sharing a meter would come back in
-- whatever order the aggregate happened to produce. No meter carries two units TODAY, so this was
-- latent rather than live — but it is the same defect UsageSeries's own header (twelve lines below)
-- warns about for `ORDER BY bucket_start`, and the fix costs one word.
ORDER BY meter, unit;

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
    SELECT date_trunc($2::text, $3::timestamptz, 'UTC') AS series_start,
           $4::timestamptz                              AS series_end
),
agg AS (
    SELECT date_trunc($2::text, l.occurred_at, 'UTC') AS bucket_start,
           l.meter, l.unit, sum(l.quantity) AS quantity, count(*) AS entries
    FROM usage_ledger l, bounds b
    WHERE ($1 = '' OR l.project_id = $1)
      AND l.occurred_at >= b.series_start AND l.occurred_at <= b.series_end
      AND ($5 = '' OR l.meter = $5)
    GROUP BY 1, 2, 3
),
present AS (
    SELECT DISTINCT meter, unit FROM agg
)
SELECT g.bucket_start, p.meter, p.unit,
       coalesce(a.quantity, 0), coalesce(a.entries, 0)
FROM bounds b
CROSS JOIN generate_series(b.series_start, b.series_end, ('1 ' || $2::text)::interval) AS g(bucket_start)
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
SELECT id, schema_version, project_id, session_id, run_id, meter, quantity, unit, occurred_at, model_request_id
FROM usage_ledger
WHERE ($1 = '' OR project_id = $1)
  AND ($2::timestamptz IS NULL OR occurred_at >= $2)
  AND ($3::timestamptz IS NULL OR occurred_at <= $3)
  AND ($4::timestamptz IS NULL OR (occurred_at, id) < ($4, $5))
  AND ($7 = '' OR session_id = $7)
  AND ($8 = '' OR meter = $8)
ORDER BY occurred_at DESC, id DESC
LIMIT $6;
