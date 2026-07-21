-- Schedule management + the ticker's due-scan / claim / handoff-sweep (spec §33, E11 Task 3). Writes are
-- the management surface (create / firing-relevant revise / pause-resume / soft-delete) and the ticker's
-- durable occurrence claim; reads drive the due-scan and the pending-occurrence handoff sweep. Every
-- management statement is tenant-scoped by (organization_id, project_id); the two system-loop scans
-- (DueSchedules, PendingOccurrences) run cross-tenant like the delivery-reconciler's sweeps.

-- name: InsertSchedule
INSERT INTO schedules (
    id, organization_id, project_id, name, trigger_id, created_by, kind, cron_expr, timezone,
    one_time_at, misfire_policy, misfire_grace_seconds, max_catch_up, jitter_seconds,
    starts_at, ends_at, next_fire_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17);

-- GetSchedule reads a schedule's management projection (GET /v1/schedules/{id}), tenant-scoped.
-- name: GetSchedule
SELECT id, name, trigger_id, kind, cron_expr, timezone, misfire_policy, misfire_grace_seconds,
       max_catch_up, jitter_seconds, status, status_reason, revision, next_fire_at, one_time_at,
       starts_at, ends_at, created_at, updated_at
FROM schedules
WHERE id = $1 AND organization_id = $2 AND project_id = $3 AND deleted_at IS NULL;

-- ReviseSchedule applies a firing-relevant edit in place, bumping revision and recomputing next_fire_at
-- (the no-schedule_revisions-table decision — occurrences pin the revision they fired under). Tenant-scoped.
-- name: ReviseSchedule
UPDATE schedules
SET cron_expr = $4, timezone = $5, one_time_at = $6, misfire_policy = $7, misfire_grace_seconds = $8,
    max_catch_up = $9, jitter_seconds = $10, starts_at = $11, ends_at = $12,
    revision = revision + 1, next_fire_at = $13, status = 'active', status_reason = '',
    updated_at = clock_timestamp()
WHERE id = $1 AND organization_id = $2 AND project_id = $3 AND deleted_at IS NULL
RETURNING revision;

-- SetScheduleStatus pauses or resumes a schedule (status='paused'|'active'). A pause stops the due-scan
-- from admitting new occurrences; an in-flight run is untouched. Tenant-scoped.
-- name: SetScheduleStatus
UPDATE schedules
SET status = $4, status_reason = $5, updated_at = clock_timestamp()
WHERE id = $1 AND organization_id = $2 AND project_id = $3 AND deleted_at IS NULL
RETURNING id;

-- SoftDeleteSchedule tombstones a schedule (deleted_at set) so the due-scan skips it while its occurrence
-- rows + linked deliveries stay queryable under retention. Tenant-scoped.
-- name: SoftDeleteSchedule
UPDATE schedules
SET deleted_at = clock_timestamp(), updated_at = clock_timestamp()
WHERE id = $1 AND organization_id = $2 AND project_id = $3 AND deleted_at IS NULL
RETURNING id;

-- ListScheduleOccurrences reads a schedule's occurrences newest-first (GET
-- /v1/schedules/{id}/occurrences). Tenant-scoped through the parent schedule.
-- name: ListScheduleOccurrences
SELECT o.occurrence_id, o.schedule_revision, o.planned_at, o.admitted_at, o.state, o.delivery_id, o.reason
FROM schedule_occurrences o
JOIN schedules s ON s.id = o.schedule_id
WHERE o.schedule_id = $1 AND s.organization_id = $2 AND s.project_id = $3
ORDER BY o.planned_at DESC, o.occurrence_id
LIMIT $4;

-- DueSchedules lists active, non-deleted schedules whose next_fire_at is due (<= now) — the ticker's
-- due-scan sweep unit. System-wide (not tenant-scoped: the ticker is a system loop, like the webhook
-- pump's fan-out). No FOR UPDATE SKIP LOCKED — correctness is the occurrence unique index, not a row
-- lock; see the ponytail note in scheduler.go.
-- name: DueSchedules
SELECT id, organization_id, project_id, trigger_id, created_by, kind, cron_expr, timezone,
       misfire_policy, misfire_grace_seconds, max_catch_up, jitter_seconds, ends_at, revision, next_fire_at
FROM schedules
WHERE status = 'active' AND deleted_at IS NULL AND next_fire_at IS NOT NULL
  AND next_fire_at <= $1
ORDER BY next_fire_at
LIMIT $2;

-- ClaimOccurrence is the exactly-once claim: INSERT ... ON CONFLICT DO NOTHING on the
-- UNIQUE(schedule_id, schedule_revision, planned_at) index. The caller reads RowsAffected() — 1 means this
-- replica won the (schedule, revision, instant), 0 means another replica (or a prior tick) already owns it.
-- Born 'pending'; the handoff sweep admits it.
-- name: ClaimOccurrence
INSERT INTO schedule_occurrences (occurrence_id, schedule_id, schedule_revision, planned_at, state)
VALUES ($1, $2, $3, $4, 'pending')
ON CONFLICT (schedule_id, schedule_revision, planned_at) DO NOTHING;

-- RecordSkipWindow materializes the ONE windowed-skip occurrence a misfire records (from/to/count in
-- reason), keyed to the most-recent skipped instant so its deterministic occurrence_id never collides with
-- a fired occurrence. ON CONFLICT DO NOTHING so two replicas planning the same window collapse to one row.
-- name: RecordSkipWindow
INSERT INTO schedule_occurrences (occurrence_id, schedule_id, schedule_revision, planned_at, state, reason)
VALUES ($1, $2, $3, $4, 'skipped', $5)
ON CONFLICT (schedule_id, schedule_revision, planned_at) DO NOTHING;

-- AdvanceNextFireAt moves a schedule's next_fire_at forward after a due tick, guarded on the revision +
-- the value the tick read ($5) so only the first replica to advance from that instant wins — a losing
-- replica's UPDATE affects 0 rows and it advances nothing. $6 NULL exhausts the schedule (a one_time that
-- fired, or a cron past ends_at).
-- name: AdvanceNextFireAt
UPDATE schedules
SET next_fire_at = $6, updated_at = clock_timestamp()
WHERE id = $1 AND organization_id = $2 AND project_id = $3 AND revision = $4 AND next_fire_at = $5;

-- FailSchedule freezes a schedule on the `fail` misfire policy: status='failed' + a reason, nothing fires,
-- admission stops until an operator resumes it. Guarded on the read next_fire_at so only one replica fails
-- it once.
-- name: FailSchedule
UPDATE schedules
SET status = 'failed', status_reason = $4, updated_at = clock_timestamp()
WHERE id = $1 AND organization_id = $2 AND project_id = $3 AND next_fire_at = $5;

-- PendingOccurrences lists occurrences durably committed 'pending' but not yet handed to the delivery
-- pipeline, joined to their schedule for the fire coordinates (trigger, tenant, principal, jitter, ends).
-- System-wide handoff sweep. A schedule soft-deleted after an occurrence was claimed still hands off (the
-- occurrence rows + deliveries are preserved under retention), so this does NOT filter deleted_at.
-- name: PendingOccurrences
SELECT o.occurrence_id, o.schedule_id, o.schedule_revision, o.planned_at,
       s.organization_id, s.project_id, s.trigger_id, s.created_by, s.jitter_seconds, s.ends_at
FROM schedule_occurrences o
JOIN schedules s ON s.id = o.schedule_id
WHERE o.state = 'pending'
ORDER BY o.planned_at
LIMIT $1;

-- MarkOccurrenceAdmitted records the delivery a pending occurrence handed off to and stamps admitted_at
-- (the real admission time — planned_at vs admitted_at makes lateness + jitter visible). Idempotent on
-- re-sweep: it only moves a still-'pending' row, so a double handoff (crash/replica) writes once.
-- name: MarkOccurrenceAdmitted
UPDATE schedule_occurrences
SET state = 'admitted', admitted_at = clock_timestamp(), delivery_id = $2
WHERE occurrence_id = $1 AND state = 'pending';

-- MarkOccurrenceFailed terminalizes a pending occurrence 'failed' with a reason (the delivery pipeline
-- rejected/failed the firing — no billable run was born). Only moves a still-'pending' row.
-- name: MarkOccurrenceFailed
UPDATE schedule_occurrences
SET state = 'failed', reason = $2
WHERE occurrence_id = $1 AND state = 'pending';
