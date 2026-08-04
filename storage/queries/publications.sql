-- Publication + approval queries (spec §30.8-30.12, §22.4-22.5). The migration owns the constraints;
-- these are the read/write paths the coordinator issues. Every query is tenant-scoped — without a
-- project a read returns no row, so a cross-tenant id leaks nothing (§39.2). The
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
ON CONFLICT (project_id, idempotency_key) DO NOTHING
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
       coalesce(a.request_hash, ''), coalesce(p.args::text, '{}')
FROM publications p
LEFT JOIN approvals a ON a.publication_id = p.id
WHERE p.project_id = $1 AND p.idempotency_key = $2;

-- RunPublishedPullRequest is E23 T6's destination resolver: the pull request THIS RUN opened and the head
-- it was opened at, read back off its own published receipt. It is the whole reason the merge tool takes no
-- arguments — a PR number a model can name is a PR the approver did not approve, so the number comes from a
-- row the model never touched, written by the publisher from the provider's own answer.
--
-- Newest-first with LIMIT 1: a run can legitimately hold several PR publications (a re-proposed request
-- dedupes, but a rebased branch does not), and the latest published one is the one this run's work is in.
-- Tenant-scoped like everything here — a foreign run id resolves nothing.
-- name: RunPublishedPullRequest
SELECT coalesce((p.receipt->>'number')::int, 0), p.head_sha
FROM publications p
WHERE p.run_id = $1 AND p.project_id = $2
  AND p.operation = 'open_pull_request' AND p.state = 'published'
ORDER BY p.created_at DESC, p.id DESC
LIMIT 1;

-- SessionHasPendingApproval reports whether the session has a publication awaiting approval — the
-- command spine's accept-time gate: with a pending approval an approve/deny is queued for the boundary
-- pump, without one it is the E08 no_pending_approval rejection.
-- name: SessionHasPendingApproval
SELECT EXISTS (
    SELECT 1 FROM publications
    WHERE session_id = $1 AND project_id = $2 AND state = 'pending_approval'
);

-- PendingApprovalForSession returns the session's oldest publication still awaiting approval, so the
-- command spine can decide whether an approve/deny has a target (spec §22.4). No row → the E08
-- no_pending_approval rejection is preserved.
-- name: PendingApprovalForSession
SELECT p.id, p.session_id, p.run_id, coalesce(p.response_id, ''), p.operation, p.remote, p.branch,
       p.base, p.head_sha, p.idempotency_key, p.display, p.state, coalesce(p.receipt::text, ''),
       coalesce(a.request_hash, ''), coalesce(p.args::text, '{}')
FROM publications p
LEFT JOIN approvals a ON a.publication_id = p.id
WHERE p.session_id = $1 AND p.project_id = $2 AND p.state = 'pending_approval'
ORDER BY p.created_at, p.id
LIMIT 1;

-- PublicationState reads one publication's current state, tenant-scoped. It answers "did my decision
-- land?" for a caller whose approve/deny command was settled by someone else (spec §22.4): the boundary
-- pump applying the SAME command means the decision is durable, while an expiry sweep settling it means
-- nothing was decided — and only this state separates the two.
-- name: PublicationState
SELECT state FROM publications
WHERE id = $1 AND project_id = $2;

-- LockPendingApprovalForSession locks the session's oldest pending publication + its approval so an
-- approve/deny transition sees a stable state (the single-winner gate). It projects the approval's
-- expires_at so the consume-time guard (ApplyApprovalDecision) can reject an approve that arrives after
-- the minutes-scale expiry (spec §22.4, E10 T7): an expired approval authorizes nothing.
-- name: LockPendingApprovalForSession
SELECT p.id, coalesce(a.request_hash, ''), a.expires_at
FROM publications p
LEFT JOIN approvals a ON a.publication_id = p.id
WHERE p.session_id = $1 AND p.project_id = $2 AND p.state = 'pending_approval'
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
WHERE p.id = $1 AND p.project_id = $2
FOR UPDATE OF p;

-- SelectExpiredApprovals returns the still-open publications (pending_approval or approved) whose
-- one-shot approval has passed its minutes-scale expiry — the reconcile sweep's read (spec §22.4, E10
-- T7). Ordered so the sweep journals deterministically. The sweep expires each single-winner and emits
-- approval.expired.v1; this read only names the candidates.
-- name: SelectExpiredApprovals
-- run_id rides the projection since E23 T3: a run PARKS on its own pending approval, so the sweep that
-- closes the question is also the sweep that has to release the run waiting on it.
SELECT p.id, p.organization_id, p.project_id, p.session_id, coalesce(p.response_id, ''), p.run_id, p.state
FROM publications p
JOIN approvals a ON a.publication_id = p.id
WHERE p.state IN ('pending_approval', 'approved')
  AND a.expires_at IS NOT NULL AND a.expires_at < clock_timestamp()
ORDER BY p.created_at, p.id;

-- SetPublicationState transitions a publication to a new state single-winner: only the tx that finds it
-- in fromState advances it, so a redelivered boundary is a no-op.
-- name: SetPublicationState
UPDATE publications
SET state = $3, updated_at = clock_timestamp()
WHERE id = $1 AND project_id = $2 AND state = $4;

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
       coalesce(a.request_hash, ''), coalesce(p.args::text, '{}')
FROM publications p
LEFT JOIN approvals a ON a.publication_id = p.id
WHERE p.run_id = $1 AND p.project_id = $2 AND p.state = 'approved'
ORDER BY p.created_at, p.id;

-- MarkPublicationPublished records the external receipt and drives approved -> published single-winner
-- (spec §30.9-30.10). A second publish of an already-published row updates 0 rows — idempotent, so a
-- lost-ack re-drive that re-reconciled the remote does not double-journal.
-- name: MarkPublicationPublished
UPDATE publications
SET state = 'published', receipt = $3, updated_at = clock_timestamp()
WHERE id = $1 AND project_id = $2 AND state = 'approved';

-- ===================================================================================================
-- E23 T1 — THE GENERIC HALF. Everything above gates a PUBLICATION; everything below gates any TOOL
-- CALL, through the same approvals table (000044 R1) and the §26.7 tool-call machine that has carried
-- `approval_pending` since 000001 without a single caller.
--
-- The split in the projections is deliberate and is NOT duplication to be folded later: a publication's
-- lifecycle lives on the publications row, a tool call's on tool_calls.state. Forcing one shape would
-- mean giving one of them a second copy of its own state, and a second copy is a thing that can disagree.
-- ===================================================================================================

-- BeginToolCallApproval writes the durable PARKED marker: the call is recorded with the exact arguments
-- and request hash that were approved, at `approval_pending`, BEFORE any human is asked. Ordering matters
-- for the same reason the pre-write's does — a crash between asking and recording would leave a button
-- bound to a call that does not exist. ON CONFLICT DO NOTHING makes a re-driven attempt idempotent: the
-- FIRST park is authoritative, so a redelivered dispatch re-reads the row it already wrote rather than
-- re-asking a human a question they may have already answered.
-- name: BeginToolCallApproval
INSERT INTO tool_calls (id, organization_id, project_id, run_id, fence, state, name, arguments, replay_class, request_hash, commit_boundary)
VALUES ($1, $2, $3, $4, $5, 'approval_pending', $6, $7, $8, $9, $10)
ON CONFLICT (id) DO NOTHING;

-- InsertToolApproval opens the one-shot binding for a parked call (000044 R1). request_hash is
-- toolbroker.RequestHash(name, args): the button carries it, so an edited argument set is a DIFFERENT
-- call with no approval (REP-009, inherited for free). expires_at is finally set by somebody — 000013
-- forward-declared it and until this epic nothing ever wrote a value into it.
-- name: InsertToolApproval
INSERT INTO approvals (id, tool_call_id, organization_id, project_id, request_hash, allowed_approver, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (tool_call_id) DO NOTHING;

-- LockToolCallForDecision locks the gated call so a decision is single-winner: two clicks on the same
-- button, or a click racing the expiry reaper, settle exactly once.
-- name: LockToolCallForDecision
SELECT state, run_id, name, coalesce(arguments::text, '{}'), coalesce(request_hash, '')
FROM tool_calls
WHERE id = $1 AND project_id = $2
FOR UPDATE;

-- ToolApprovalForCall reads the whole parked-call projection: the ledger row's own facts joined to the
-- binding. It is the read every surface derives the approval SCREEN from — nothing stores a rendered
-- screen, so this is where one comes from.
-- name: ToolApprovalForCall
SELECT a.id, t.id, t.run_id, r.session_id, coalesce(r.response_id, ''), t.name,
       coalesce(t.arguments::text, '{}'), coalesce(a.request_hash, ''), t.state, a.expires_at,
       a.decided_by
FROM approvals a
JOIN tool_calls t ON t.id = a.tool_call_id
JOIN runs r ON r.id = t.run_id
WHERE a.tool_call_id = $1 AND a.project_id = $2;

-- ApproveToolCall advances approval_pending -> ready (§26.7's own transition, unused until now). Single
-- winner on the source state, so a doubled click approves once.
-- name: ApproveToolCall
UPDATE tool_calls
SET state = 'ready', updated_at = clock_timestamp()
WHERE id = $1 AND project_id = $2 AND state = 'approval_pending';

-- CancelToolCall drives a gated call to `canceled` and RECORDS THE ANSWER in the result column. It is
-- the shared exit for the three ways a call can fail to be authorized — a human denied it, its deadline
-- passed, or a before_tool hook denied it after the human said yes — so all three deliver the model the
-- same shape and all three survive a kill: the answer is durable before it is spoken.
-- name: CancelToolCall
UPDATE tool_calls
SET state = 'canceled', result = $3, updated_at = clock_timestamp()
WHERE id = $1 AND project_id = $2 AND state IN ('approval_pending', 'ready');

-- SetToolApprovalDecision records WHO decided (audit). The lifecycle stays on the tool_calls row.
-- name: SetToolApprovalDecision
UPDATE approvals
SET decided_by = $2, updated_at = clock_timestamp()
WHERE tool_call_id = $1;

-- SelectExpiredToolApprovals is the reaper's read: every still-parked call whose deadline passed. System
-- scoped by construction (a sweep spans tenants), and it carries the run's session/response so the
-- expiry can be journaled onto the right stream and the parked run woken.
-- name: SelectExpiredToolApprovals
SELECT a.tool_call_id, t.organization_id, t.project_id, t.run_id, r.session_id, coalesce(r.response_id, '')
FROM approvals a
JOIN tool_calls t ON t.id = a.tool_call_id
JOIN runs r ON r.id = t.run_id
WHERE t.state = 'approval_pending'
  AND a.expires_at IS NOT NULL AND a.expires_at < clock_timestamp()
ORDER BY a.created_at, a.id;

-- PendingToolApprovalForSession is the MODAL'S ONLY READ (E23 T4). A Show-arguments click carries the
-- one-shot request hash and nothing else, so the screen it opens is resolved from the hash plus the
-- session the click's thread correlated to — the same two bindings the decision path checks, one step
-- shallower.
--
-- ORDER BY + LIMIT 1 on created_at follows PendingApprovalForSession's inherited posture: a session's
-- OLDEST open approval is the decidable one, and the same one is the one shown. A second row can only
-- exist if a session has two open approvals whose arguments hash identically — literally the same call
-- twice — so showing the older is not an ambiguity, it is the same document.
--
-- It reads and only reads. The modal is opened INSIDE the interactivity ack budget, where a write would
-- be a write racing a trigger_id that dies in three seconds.
-- name: PendingToolApprovalForSession
SELECT a.id, t.id, t.run_id, r.session_id, coalesce(r.response_id, ''), t.name,
       coalesce(t.arguments::text, '{}'), coalesce(a.request_hash, ''), t.state, a.expires_at,
       a.decided_by
FROM approvals a
JOIN tool_calls t ON t.id = a.tool_call_id
JOIN runs r ON r.id = t.run_id
WHERE r.session_id = $1 AND a.request_hash = $2 AND t.state = 'approval_pending' AND a.project_id = $3
ORDER BY a.created_at
LIMIT 1;

-- PendingToolApprovalsForTenant is the SLACK-LESS READ (E23 T9): every gated call in the caller's project
-- that a human still owes an answer on. It is the same projection ToolApprovalForCall returns, widened
-- from one call to a page, because a human cannot decide what they cannot see and the only surface that
-- ever listed these was a Slack thread.
--
-- Keyset-ordered on (created_at, id) so the shared page envelope's cursor is total: created_at alone is a
-- clock_timestamp() default and two approvals opened in the same transaction can share it. It ORDERS
-- ASCENDING — unlike the other list surfaces, which are newest-first — because these are questions and the
-- oldest one is the one closest to expiring; the cursor ($4/$5) is therefore compared with `>` rather than
-- the `<` a DESC list uses. An ORDER BY that disagreed with its own cursor predicate is how a page-2
-- request comes back holding page 1 forever.
--
-- AN ELAPSED-BUT-UNSWEPT ROW IS DELIBERATELY INCLUDED. `t.state` is the authority on whether a question is
-- open; expires_at is a deadline the reaper (and DecideToolApproval's own consume-time guard) enforces.
-- Filtering on the clock here would make this read a SECOND opinion about which approvals exist, and the
-- two would disagree for the seconds between a deadline passing and the sweep — the expires_at field is
-- carried instead, so the surface can say the question is stale rather than pretend it never existed.
-- name: PendingToolApprovalsForTenant
SELECT a.id, t.id, t.run_id, r.session_id, coalesce(r.response_id, ''), t.name,
       coalesce(t.arguments::text, '{}'), coalesce(a.request_hash, ''), t.state, a.expires_at,
       a.decided_by, a.created_at
FROM approvals a
JOIN tool_calls t ON t.id = a.tool_call_id
JOIN runs r ON r.id = t.run_id
WHERE t.state = 'approval_pending' AND a.project_id = $1
  AND ($2::timestamptz IS NULL OR a.created_at >= $2)
  AND ($3::timestamptz IS NULL OR a.created_at <= $3)
  AND ($4::timestamptz IS NULL OR (a.created_at, a.id) > ($4, $5))
ORDER BY a.created_at, a.id
LIMIT $6;

-- ToolApprovalByID resolves ONE approval from the id the list surface handed out (E23 T9), so the HTTP
-- decision route can key on /v1/approvals/{approval_id} while the throat it calls keys on the tool call.
--
-- NO ORDER BY AND NO LIMIT, and that is safe here for a structural reason rather than an assumption:
-- approvals.id is the table's PRIMARY KEY (000013), and both joins are to primary keys too, so this
-- statement returns at most one row. (Contrast PendingToolApprovalForSession, whose predicate is a
-- (session, hash) pair that genuinely can match twice and therefore orders and limits.)
--
-- A row belonging to another tenant simply does not match, so a foreign id is indistinguishable from an
-- unknown one and the surface above answers 404 without learning that it exists.
-- name: ToolApprovalByID
SELECT a.id, t.id, t.run_id, r.session_id, coalesce(r.response_id, ''), t.name,
       coalesce(t.arguments::text, '{}'), coalesce(a.request_hash, ''), t.state, a.expires_at,
       a.decided_by, a.created_at
FROM approvals a
JOIN tool_calls t ON t.id = a.tool_call_id
JOIN runs r ON r.id = t.run_id
WHERE a.id = $1 AND a.project_id = $2;

-- PendingPublicationApprovalsForTenant is the PUBLICATION half of the Slack-less read, and it exists
-- because PendingToolApprovalsForTenant above JOINs tool_calls — so a publication approval, whose
-- approvals row carries publication_id and an EMPTY tool_call_id, could never match it. Measured
-- 2026-08-01: a push recorded approvals row apr_93e6ef29542cf765 while GET /v1/approvals answered
-- {"data":[]}, and the console's own /approvals page told the operator to go watch Live runs instead.
-- A human cannot decide what they cannot see, which is the sentence the tool half was written under;
-- this is the same sentence applied to the family it left out.
--
-- IT MIRRORS ITS SIBLING'S ORDER AND CURSOR EXACTLY — ascending (created_at, id), cursor compared with
-- `>` — because the two are merged into ONE page by the caller and a half that disagreed about direction
-- would interleave into a cursor that skips rows. These are questions and the oldest is closest to its
-- deadline.
--
-- AN ELAPSED-BUT-UNSWEPT ROW IS INCLUDED, for the same reason stated on the tool half: p.state is the
-- authority on whether the question is open, expires_at is a deadline the reaper enforces, and filtering
-- on the clock here would make this read a second opinion that disagrees with the sweep for the seconds
-- between them.
-- name: PendingPublicationApprovalsForTenant
SELECT a.id, p.id, p.run_id, p.session_id, coalesce(p.response_id, ''), p.operation,
       coalesce(p.args::text, '{}'), coalesce(a.request_hash, ''), p.state, a.expires_at,
       a.decided_by, a.created_at, p.remote, p.branch, p.base, p.head_sha, coalesce(p.display, ''),
       coalesce(bind.connection_ref, ''), coalesce(bind.repository_identity, '')
FROM approvals a
JOIN publications p ON p.id = a.publication_id
LEFT JOIN LATERAL (
    -- THE BINDING THIS RUN PREPARED FROM, and it is a LATERAL with its own ORDER BY + LIMIT 1 rather than a
    -- plain join for a reason this tree has paid for: preparation_receipts holds a row PER PREPARE, so a
    -- plain join multiplies the approval row by however many times the run prepared. The order is
    -- RunPublicationTarget's verbatim (prepared_at DESC, id DESC) so the credential shown here is the
    -- credential that read will resolve at publish time — two different orders would show one identity and
    -- write under another.
    SELECT rb.connection_ref, rb.repository_identity
    FROM preparation_receipts pr
    JOIN repository_bindings rb ON rb.id = pr.repository_binding_id
    WHERE pr.run_id = p.run_id AND pr.project_id = p.project_id
    ORDER BY pr.prepared_at DESC, pr.id DESC
    LIMIT 1
) bind ON TRUE
WHERE p.state = 'pending_approval' AND a.project_id = $1
  AND ($2::timestamptz IS NULL OR a.created_at >= $2)
  AND ($3::timestamptz IS NULL OR a.created_at <= $3)
  AND ($4::timestamptz IS NULL OR (a.created_at, a.id) > ($4, $5))
ORDER BY a.created_at, a.id
LIMIT $6;

-- PublicationApprovalByID resolves ONE pending publication approval from the id the list above handed
-- out, so POST /v1/approvals/{approval_id}/approve can reach the publication family. Its sibling
-- ToolApprovalByID could not: it JOINs tool_calls, so a publication id fell through to "no such
-- approval" — a 404 on a row that exists, measured with the correct request_hash on 2026-08-01.
--
-- NO ORDER BY AND NO LIMIT, and the reason is ToolApprovalByID's verbatim: approvals.id is the table's
-- PRIMARY KEY (000013) and publications.id is one too, so this statement returns at most one row. It is
-- named here rather than assumed because an unordered single-row read is exactly the shape that has
-- decided a security outcome twice in this tree.
--
-- A row belonging to another tenant does not match, so a foreign id is indistinguishable from an unknown
-- one and the surface answers 404 without confirming it exists.
-- name: PublicationApprovalByID
SELECT a.id, p.id, p.run_id, p.session_id, coalesce(p.response_id, ''), p.operation,
       coalesce(p.args::text, '{}'), coalesce(a.request_hash, ''), p.state, a.expires_at,
       a.decided_by, a.created_at, p.remote, p.branch, p.base, p.head_sha, coalesce(p.display, ''),
       coalesce(bind.connection_ref, ''), coalesce(bind.repository_identity, '')
FROM approvals a
JOIN publications p ON p.id = a.publication_id
LEFT JOIN LATERAL (
    -- THE BINDING THIS RUN PREPARED FROM, and it is a LATERAL with its own ORDER BY + LIMIT 1 rather than a
    -- plain join for a reason this tree has paid for: preparation_receipts holds a row PER PREPARE, so a
    -- plain join multiplies the approval row by however many times the run prepared. The order is
    -- RunPublicationTarget's verbatim (prepared_at DESC, id DESC) so the credential shown here is the
    -- credential that read will resolve at publish time — two different orders would show one identity and
    -- write under another.
    SELECT rb.connection_ref, rb.repository_identity
    FROM preparation_receipts pr
    JOIN repository_bindings rb ON rb.id = pr.repository_binding_id
    WHERE pr.run_id = p.run_id AND pr.project_id = p.project_id
    ORDER BY pr.prepared_at DESC, pr.id DESC
    LIMIT 1
) bind ON TRUE
WHERE a.id = $1 AND a.project_id = $2 AND p.state = 'pending_approval';
