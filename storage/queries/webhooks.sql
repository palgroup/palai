-- Outbound webhook queries (spec §21.4-21.6, E11 Task 4). Endpoint registration, the journal fan-out
-- cursor, the delivery pump's due-scan + state transitions, and the sanitized attempt view. Every
-- read/write is tenant-scoped by the verified identity (§39.2), never a request-body field.

-- CreateWebhookEndpoint initializes the fan-out cursor to the CURRENT journal high-water mark plus the
-- pump's re-scan lag ($15), so a brand-new endpoint only receives events journaled AFTER it was created
-- — never the tenant's entire historical journal (F5). The + lag keeps the pump's read-back window
-- (cursor - lag) at or above the current max, so no pre-creation event is re-scanned into a delivery.
-- name: CreateWebhookEndpoint
INSERT INTO webhook_endpoints (
    id, project_id, url, enabled, event_filter, api_revision,
    signing_secret_ref, signing_secret_ref_next, fixed_headers,
    timeout_ms, max_attempts, retry_window_seconds, allow_private_destination, cursor_journal_id
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13,
    (SELECT COALESCE(max(journal_id), 0) FROM events) + $14)
RETURNING id;

-- ListWebhookEndpoints and GetWebhookEndpoint share ONE projection, and they are written next to each other
-- so they stay that way: two reads of a resource that disagree about its shape are two resources as far as a
-- caller is concerned.
--
-- The projection carries the DELIVERY POLICY (timeout_ms, max_attempts, retry_window_seconds) and the secret
-- HANDLES, because an operator who set a timeout has to be able to read it back — otherwise the only record
-- of what a deployment does is what somebody remembers typing. A signing_secret_ref is a handle and not a
-- value; what is behind it stays unreadable through every route.
--
-- fixed_headers is DELIBERATELY ABSENT and that is the one exclusion. It is a free map the operator writes,
-- an Authorization header for the receiver is a normal thing to put in it, and returning it would turn a
-- read route into a credential read.
--
-- `, id DESC` IS THE TIEBREAKER AND IT IS NOT DECORATION. created_at is a microsecond timestamptz and nothing
-- forbids two rows from sharing one, so `ORDER BY created_at DESC` alone is a PARTIAL order: both orders of a
-- tied pair are correct answers and which one comes back is decided by where the rows physically sit, which
-- the caller cannot see. Measured on 2026-08-01: the same query returned [high low] for one tied pair and
-- [low high] for an identically-shaped one whose rows had been written in the other order. id is the PRIMARY
-- KEY, so adding it makes the order TOTAL.
-- name: ListWebhookEndpoints
SELECT id, url, enabled, event_filter, api_revision,
       signing_secret_ref, signing_secret_ref_next,
       timeout_ms, max_attempts, retry_window_seconds,
       allow_private_destination, created_at
FROM webhook_endpoints
WHERE project_id = $1
ORDER BY created_at DESC, id DESC;

-- GetWebhookEndpoint is the singular read, and its column list is character-for-character the list's above.
-- name: GetWebhookEndpoint
SELECT id, url, enabled, event_filter, api_revision,
       signing_secret_ref, signing_secret_ref_next,
       timeout_ms, max_attempts, retry_window_seconds,
       allow_private_destination, created_at
FROM webhook_endpoints
WHERE id = $1 AND project_id = $2;

-- DeleteWebhookEndpoint unregisters an endpoint (spec §21.4). Tenant-scoped by the verified identity, so an
-- id belonging to another project matches nothing and is reported as absent rather than as forbidden — one
-- tenant learns nothing about another tenant's ids.
--
-- IT TAKES EXACTLY ONE ROW AND THAT IS ENFORCED BY THE SCHEMA, NOT BY THIS STATEMENT. Migration 000052
-- dropped the ON DELETE CASCADE from webhook_deliveries.endpoint_id precisely so this DELETE cannot reach
-- the delivery trail: a delivery records a payload this deployment already sent, and unregistering an
-- address does not un-send it. The surviving deliveries keep the deleted endpoint's id, unresolvable and
-- shown as such. A component test re-reads pg_constraint and fails if any referrer of this table ever
-- acquires a destructive delete action again.
--
-- trigger_revisions.callback_endpoint_id still references this table with no delete action, so an endpoint a
-- trigger revision pins CANNOT be deleted: Postgres raises 23503 and the store renders it as a typed refusal.
-- That one is kept on purpose — a trigger revision is live configuration that will send to this endpoint the
-- next time it fires, not a record of the past.
-- name: DeleteWebhookEndpoint
DELETE FROM webhook_endpoints
WHERE id = $1 AND project_id = $2
RETURNING id;

-- FanOutEndpoints returns the enabled endpoints and their current durable cursor, so the pump can
-- scan the journal past each endpoint's high-water mark. Not tenant-scoped: the pump is a system loop
-- that serves every project (each endpoint carries its own scope forward onto the delivery rows).
-- name: FanOutEndpoints
SELECT id, project_id, event_filter, api_revision, cursor_journal_id
FROM webhook_endpoints
WHERE enabled;

-- ReadJournalForEndpoint reads the matching journal slice past an endpoint's cursor, ordered by the
-- global journal_id (the 000020 IDENTITY cursor). It is tenant-scoped to the endpoint's own
-- organization + project (§39.2): an endpoint only ever fans out its OWN project's events — a delivery
-- must never carry another tenant's journal. Self-generated webhook.* events are excluded so a
-- delivery-outcome event can never fan out into another delivery (loop guard, §50 webhook loop
-- detection). An empty filter matches every (non-webhook) event.
-- name: ReadJournalForEndpoint
SELECT journal_id, id, session_id, type, payload
FROM events
WHERE project_id = $1
  AND journal_id > $2
  AND type NOT LIKE 'webhook.%'
  AND (cardinality($3::text[]) = 0 OR type = ANY ($3::text[]))
ORDER BY journal_id
LIMIT $4;

-- name: AdvanceEndpointCursor
UPDATE webhook_endpoints SET cursor_journal_id = $2 WHERE id = $1 AND cursor_journal_id < $2;

-- InsertDelivery materializes one delivery for a (endpoint, event). ON CONFLICT DO NOTHING makes
-- fan-out idempotent: a pump crash between insert and cursor-advance, or a catch-up re-scan, never
-- double-emits (spec §21.6 dedupe).
-- name: InsertDelivery
INSERT INTO webhook_deliveries (
    id, project_id, endpoint_id, session_id, event_id, event_type, payload
) VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (endpoint_id, event_id) DO NOTHING;

-- DueDeliveries returns pending deliveries whose backoff clock has elapsed, joined to their endpoint's
-- delivery policy — one row is everything an attempt needs. Ordered by next_attempt_at so the oldest
-- due delivery is served first; per-row independence means one dead endpoint never blocks another
-- (AUT — no head-of-line). ponytail: no FOR UPDATE — a single supervised pump owns the loop; the
-- attempt UNIQUE(delivery_id, attempt_number) is the backstop if two ever race.
-- name: DueDeliveries
SELECT d.id, d.project_id, d.session_id, d.endpoint_id, d.event_id, d.event_type,
       d.payload, d.attempt_count, d.first_attempt_at,
       e.url, e.allow_private_destination, e.timeout_ms, e.max_attempts, e.retry_window_seconds,
       e.signing_secret_ref, e.signing_secret_ref_next, e.fixed_headers, e.api_revision
FROM webhook_deliveries d
JOIN webhook_endpoints e ON e.id = d.endpoint_id
WHERE d.state = 'pending' AND d.next_attempt_at <= clock_timestamp() AND e.enabled
ORDER BY d.next_attempt_at
LIMIT $1;

-- RecordDeliveryAttempt appends the next attempt row with a MONOTONIC attempt_number (max+1), not the
-- delivery's retry-budget count — so an operator redelivery (which resets attempt_count to 0 for a
-- fresh budget) keeps appending 4,5,6… instead of colliding on 1 and being silently dropped by the
-- UNIQUE(delivery_id,attempt_number) constraint (F6). One writer per delivery per tick, so max+1 has no
-- race.
-- name: RecordDeliveryAttempt
INSERT INTO delivery_attempts (delivery_id, attempt_number, status_code, duration_ms, response_excerpt, error)
VALUES ($1,
    (SELECT COALESCE(max(attempt_number), 0) + 1 FROM delivery_attempts WHERE delivery_id = $1),
    $2, $3, $4, $5);

-- name: MarkDeliveryDelivered
UPDATE webhook_deliveries
SET state = 'delivered', attempt_count = $2, first_attempt_at = COALESCE(first_attempt_at, clock_timestamp()), updated_at = clock_timestamp()
WHERE id = $1;

-- name: RescheduleDelivery
UPDATE webhook_deliveries
SET attempt_count = $2, next_attempt_at = $3, first_attempt_at = COALESCE(first_attempt_at, clock_timestamp()), updated_at = clock_timestamp()
WHERE id = $1;

-- name: MarkDeliveryDead
UPDATE webhook_deliveries
SET state = 'dead', attempt_count = $2, first_attempt_at = COALESCE(first_attempt_at, clock_timestamp()), updated_at = clock_timestamp()
WHERE id = $1;

-- RedeliverDelivery revives a delivery on operator request with the SAME id and payload (spec §21.6):
-- it re-queues the row and resets the retry budget/window so a dead delivery can actually re-attempt.
-- Tenant-scoped and idempotent — re-calling on an already-pending row is a no-op.
--
-- THE EXISTS CLAUSE CLOSES A HOLE E29 T3 OPENED. Before endpoints were deletable, every delivery had a live
-- endpoint and this UPDATE could not lie. Now a delivery can outlive its endpoint by design, and re-queuing
-- one of those would move the row to `pending` and answer 202 for work that can never happen: DueDeliveries
-- INNER JOINs webhook_endpoints, so the pump would never pick it up. Matching nothing here is what lets the
-- store tell "no such delivery" apart from "its endpoint is gone" and answer the second one honestly.
-- name: RedeliverDelivery
UPDATE webhook_deliveries d
SET state = 'pending', attempt_count = 0, first_attempt_at = NULL, next_attempt_at = clock_timestamp(), updated_at = clock_timestamp()
WHERE d.id = $1 AND d.project_id = $2
  AND EXISTS (SELECT 1 FROM webhook_endpoints e WHERE e.id = d.endpoint_id)
RETURNING d.id;

-- DeliveryExists distinguishes the two ways RedeliverDelivery can match nothing: a delivery that is not
-- there at all (404) and one whose endpoint has been deleted (a refusal that names the reason). Without it
-- the second would be reported as the first, which would tell an operator their delivery record had
-- vanished when it is still readable.
-- name: DeliveryExists
SELECT 1 FROM webhook_deliveries WHERE id = $1 AND project_id = $2;

-- ListWebhookDeliveries. `, id DESC` is the tiebreaker, and the LIMIT below is why it matters MORE here than
-- on the endpoint list: under a partial order the page BOUNDARY moves. Two rows sharing a created_at can
-- both land on page one, or one can be served on page one and again on page two, or fall between the two and
-- never be served at all — and a reader who loses a delivery record is told nothing. Measured on 2026-08-01
-- with three rows tied on created_at and LIMIT 2: one fixture's first page was [high mid] and an
-- identically-shaped one's was [low high], so a row that appeared in one page was absent from the other.
--
-- This tree has already had an ORDER BY that could not distinguish two rows decide a security outcome twice.
-- id is the PRIMARY KEY, so the order is now TOTAL and the page boundary is a property of the query.
-- name: ListWebhookDeliveries
SELECT id, endpoint_id, event_id, event_type, state, attempt_count, next_attempt_at, created_at, updated_at
FROM webhook_deliveries
WHERE project_id = $1
  AND ($2 = '' OR state = $2)
ORDER BY created_at DESC, id DESC
LIMIT $3;

-- name: GetWebhookDelivery
SELECT id, endpoint_id, event_id, event_type, state, attempt_count, next_attempt_at, created_at, updated_at
FROM webhook_deliveries
WHERE id = $1 AND project_id = $2;

-- ListDeliveryAttempts is the sanitized attempt view (spec §21.6): status, duration, and the bounded
-- excerpt — the signing secret and secret-ref header values are structurally absent (they are never
-- written to this table).
-- name: ListDeliveryAttempts
SELECT a.attempt_number, a.status_code, a.duration_ms, a.response_excerpt, a.error, a.created_at
FROM delivery_attempts a
JOIN webhook_deliveries d ON d.id = a.delivery_id
WHERE a.delivery_id = $1 AND d.project_id = $2
ORDER BY a.attempt_number;
