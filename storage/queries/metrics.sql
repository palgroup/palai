-- Metrics queries (E14 Task 6) back the unauthenticated internal /metrics exposition. Each is an
-- installation-wide aggregate GROUPED BY a lifecycle enum, never by tenant, and each runs under the
-- WithSystemScope escape hatch (a cross-tenant count cannot be scoped to one tenant). They add no
-- column and read only tables 000001/000020 already declared.

-- MetricRunStates counts runs by lifecycle state across the whole installation (run-state gauge).
-- name: MetricRunStates
SELECT state, count(*)
FROM runs
GROUP BY state;

-- MetricJobStatuses counts durable jobs by status; 'queued' is the backlog, 'failed'/'dead' the
-- dispatch failures. The durable_jobs_claimable_idx covers the status leading column.
-- name: MetricJobStatuses
SELECT status, count(*)
FROM durable_jobs
GROUP BY status;

-- MetricQueueReady is the claimable-backlog depth and the age of its oldest member: queued jobs whose
-- ready_at has arrived. COALESCE keeps the age 0 (not NULL) on an empty queue. Drives the queue alert.
-- name: MetricQueueReady
SELECT count(*),
       COALESCE(EXTRACT(EPOCH FROM (clock_timestamp() - min(ready_at))), 0)
FROM durable_jobs
WHERE status = 'queued' AND ready_at <= clock_timestamp();

-- MetricJobInflightOldest is the age of the oldest running job — a dispatch-progress proxy. A stuck
-- dispatcher shows here as a growing age even before the queue backs up.
-- name: MetricJobInflightOldest
SELECT COALESCE(EXTRACT(EPOCH FROM (clock_timestamp() - min(updated_at))), 0)
FROM durable_jobs
WHERE status = 'running';

-- MetricWebhookDeliveryStates counts outbound-webhook (callback) deliveries by state: 'pending' is
-- backlog, 'dead' is exhausted-retry failure. Drives the callback alert.
-- name: MetricWebhookDeliveryStates
SELECT state, count(*)
FROM webhook_deliveries
GROUP BY state;

-- MetricDBClock returns the database wall clock; the collector subtracts its own clock to publish the
-- skew the clock alert reads.
-- name: MetricDBClock
SELECT clock_timestamp();
