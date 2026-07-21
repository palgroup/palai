-- Publication + approval queries (spec §30.8-30.12, §22.4-22.5). The migration owns the constraints;
-- these are the read/write paths the coordinator issues. Every query is tenant-scoped — without
-- organization and project a read returns no row, so a cross-tenant id leaks nothing (§39.2). The
-- publication's own (org, project, idempotency_key) uniqueness carries the operation-level idempotency
-- (decision (b)): a duplicate request returns the original row rather than a second pending approval.

-- InsertPublication reserves a pending publication idempotently. ON CONFLICT on the idempotency key
-- RETURNs no row for a duplicate, so the caller reads and replays the original (no double approval /
-- push / PR). state defaults to pending_approval.
-- name: InsertPublication
INSERT INTO publications
    (id, organization_id, project_id, session_id, run_id, response_id, operation, remote, branch, base,
     head_sha, idempotency_key, display, args)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
ON CONFLICT (organization_id, project_id, idempotency_key) DO NOTHING
RETURNING id;

-- InsertApproval records the one-shot approval binding for a publication (spec §22.4). It rides the
-- publication's first insert only (the caller skips it on a replay).
-- name: InsertApproval
INSERT INTO approvals (id, publication_id, organization_id, project_id, request_hash, allowed_approver, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (publication_id) DO NOTHING;

-- GetPublicationByKey reads a publication by its idempotency key — the replay read after an ON CONFLICT
-- insert returns no row.
-- name: GetPublicationByKey
SELECT p.id, p.session_id, p.run_id, coalesce(p.response_id, ''), p.operation, p.remote, p.branch,
       p.base, p.head_sha, p.idempotency_key, p.display, p.state, coalesce(p.receipt::text, ''),
       coalesce(a.request_hash, '')
FROM publications p
LEFT JOIN approvals a ON a.publication_id = p.id
WHERE p.organization_id = $1 AND p.project_id = $2 AND p.idempotency_key = $3;

-- SessionHasPendingApproval reports whether the session has a publication awaiting approval — the
-- command spine's accept-time gate: with a pending approval an approve/deny is queued for the boundary
-- pump, without one it is the E08 no_pending_approval rejection.
-- name: SessionHasPendingApproval
SELECT EXISTS (
    SELECT 1 FROM publications
    WHERE session_id = $1 AND organization_id = $2 AND project_id = $3 AND state = 'pending_approval'
);

-- PendingApprovalForSession returns the session's oldest publication still awaiting approval, so the
-- command spine can decide whether an approve/deny has a target (spec §22.4). No row → the E08
-- no_pending_approval rejection is preserved.
-- name: PendingApprovalForSession
SELECT p.id, p.session_id, p.run_id, coalesce(p.response_id, ''), p.operation, p.remote, p.branch,
       p.base, p.head_sha, p.idempotency_key, p.display, p.state, coalesce(p.receipt::text, ''),
       coalesce(a.request_hash, '')
FROM publications p
LEFT JOIN approvals a ON a.publication_id = p.id
WHERE p.session_id = $1 AND p.organization_id = $2 AND p.project_id = $3 AND p.state = 'pending_approval'
ORDER BY p.created_at, p.id
LIMIT 1;

-- LockPendingApprovalForSession locks the session's oldest pending publication + its approval so an
-- approve/deny transition sees a stable state (the single-winner gate). It projects the approval's
-- expires_at so the consume-time guard (ApplyApprovalDecision) can reject an approve that arrives after
-- the minutes-scale expiry (spec §22.4, E10 T7): an expired approval authorizes nothing.
-- name: LockPendingApprovalForSession
SELECT p.id, coalesce(a.request_hash, ''), a.expires_at
FROM publications p
LEFT JOIN approvals a ON a.publication_id = p.id
WHERE p.session_id = $1 AND p.organization_id = $2 AND p.project_id = $3 AND p.state = 'pending_approval'
ORDER BY p.created_at, p.id
LIMIT 1
FOR UPDATE OF p;

-- LockPublicationApprovalExpiry locks one publication + its approval expiry for the pump's consume-time
-- expiry guard (spec §22.4, §30.9-30.10, E10 T7): before publishing an APPROVED row the pump checks
-- whether its approval elapsed between approval and publish. FOR UPDATE OF p serializes it against a
-- concurrent publish/deny so the approved->expired transition is single-winner.
-- name: LockPublicationApprovalExpiry
SELECT p.state, a.expires_at
FROM publications p
LEFT JOIN approvals a ON a.publication_id = p.id
WHERE p.id = $1 AND p.organization_id = $2 AND p.project_id = $3
FOR UPDATE OF p;

-- SelectExpiredApprovals returns the still-open publications (pending_approval or approved) whose
-- one-shot approval has passed its minutes-scale expiry — the reconcile sweep's read (spec §22.4, E10
-- T7). Ordered so the sweep journals deterministically. The sweep expires each single-winner and emits
-- approval.expired.v1; this read only names the candidates.
-- name: SelectExpiredApprovals
SELECT p.id, p.organization_id, p.project_id, p.session_id, coalesce(p.response_id, ''), p.state
FROM publications p
JOIN approvals a ON a.publication_id = p.id
WHERE p.state IN ('pending_approval', 'approved')
  AND a.expires_at IS NOT NULL AND a.expires_at < clock_timestamp()
ORDER BY p.created_at, p.id;

-- SetPublicationState transitions a publication to a new state single-winner: only the tx that finds it
-- in fromState advances it, so a redelivered boundary is a no-op.
-- name: SetPublicationState
UPDATE publications
SET state = $4, updated_at = clock_timestamp()
WHERE id = $1 AND organization_id = $2 AND project_id = $3 AND state = $5;

-- SetApprovalDecision records who decided an approval (audit), leaving the lifecycle state on the
-- publication.
-- name: SetApprovalDecision
UPDATE approvals
SET decided_by = $2, updated_at = clock_timestamp()
WHERE publication_id = $1;

-- ApprovedPublicationsForRun returns a run's approved-but-unpublished publications in creation order —
-- the approval pump's drain (spec §30.9-30.10). A published/denied/expired row never reappears.
-- name: ApprovedPublicationsForRun
SELECT p.id, p.session_id, p.run_id, coalesce(p.response_id, ''), p.operation, p.remote, p.branch,
       p.base, p.head_sha, p.idempotency_key, p.display, p.state, coalesce(p.receipt::text, ''),
       coalesce(a.request_hash, '')
FROM publications p
LEFT JOIN approvals a ON a.publication_id = p.id
WHERE p.run_id = $1 AND p.organization_id = $2 AND p.project_id = $3 AND p.state = 'approved'
ORDER BY p.created_at, p.id;

-- MarkPublicationPublished records the external receipt and drives approved -> published single-winner
-- (spec §30.9-30.10). A second publish of an already-published row updates 0 rows — idempotent, so a
-- lost-ack re-drive that re-reconciled the remote does not double-journal.
-- name: MarkPublicationPublished
UPDATE publications
SET state = 'published', receipt = $4, updated_at = clock_timestamp()
WHERE id = $1 AND organization_id = $2 AND project_id = $3 AND state = 'approved';
