-- Remote HTTP tool async-operation ledger (spec §28.24-28.25, E12 Task 4). The broker's remote_http
-- executor opens ONE pending operation before an invoke, polls it after a 202, and the signed callback
-- endpoint consumes its one-use token atomically. A late callback (after the executor timed out) writes
-- late_result here and the RemoteToolProber carries the uncertain tool_call to reconciled_completed —
-- this table never commits to the ledger itself. Every statement is tenant-scoped by the caller.

-- SweepExpiredRemoteOperation flips a stuck pending operation (deadline already passed — a prior invoke
-- that failed transiently or a crashed process) to timed_out, so the next drive of the SAME tool_call can
-- open a fresh operation and re-POST rather than block on a callback that will never come. Run before Open.
-- name: SweepExpiredRemoteOperation
UPDATE remote_tool_operations
SET state = 'timed_out', completed_at = clock_timestamp()
WHERE tool_call_id = $1 AND state = 'pending' AND deadline < clock_timestamp();

-- OpenRemoteOperation opens the pending operation before the invoke POST. ON CONFLICT DO NOTHING makes
-- the partial-unique(tool_call_id WHERE pending) a 0-row no-op: a duplicate LIVE (non-expired) invoke
-- cannot open a second pending row, and the executor polls the existing one instead of re-POSTing.
-- name: OpenRemoteOperation
INSERT INTO remote_tool_operations
    (id, organization_id, project_id, tool_call_id, secret_ref, callback_token_hash, deadline, fence)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT DO NOTHING;

-- FailRemoteOperation closes a pending operation the invoke got a DEFINITE negative answer for (a signed
-- 401/403, a 409, an RFC 9457 problem body, an unexpected status, or a connect-time egress deny — the
-- request did not land a side effect), so a fresh attempt's Open opens a NEW pending row and re-POSTs. A
-- transient network error BEFORE the deadline does NOT fail the row (the POST may have landed) — the
-- deadline sweep handles that.
-- name: FailRemoteOperation
UPDATE remote_tool_operations
SET state = 'failed', completed_at = clock_timestamp()
WHERE id = $1 AND state = 'pending';

-- PollRemoteOperation reads an operation's state + result by its id (the executor polls the row it
-- opened). completed carries the result to return; timed_out/late_result tells the executor its invoke
-- resolved too late (it returns a timeout, never the late result — that reconciles via the prober).
-- name: PollRemoteOperation
SELECT state, result FROM remote_tool_operations WHERE id = $1;

-- CompleteSyncRemoteOperation closes the pending row a 200 (synchronous result) answered: the result is
-- already in the executor's hand, so this only records it + closes the row. A row a callback already
-- resolved (not pending) is left untouched (0 rows).
-- name: CompleteSyncRemoteOperation
UPDATE remote_tool_operations
SET state = 'completed', result = $2, result_hash = $3, completed_at = clock_timestamp()
WHERE id = $1 AND state = 'pending';

-- TimeoutRemoteOperation flips a still-pending operation to timed_out when the executor's deadline fires.
-- A callback that already completed it (not pending) is left untouched (0 rows) — the executor re-polls
-- and returns that result instead of a timeout.
-- name: TimeoutRemoteOperation
UPDATE remote_tool_operations
SET state = 'timed_out', completed_at = clock_timestamp()
WHERE id = $1 AND state = 'pending';

-- RemoteOperationForCallback reads the verify-before-persist inputs for a callback: the org + secret_ref
-- (to resolve the signing secret), the token hash (constant-time compared in Go), and the current
-- state + result_hash (to decide idempotent-200 vs 409 on a second callback). No row = generic 404.
-- name: RemoteOperationForCallback
SELECT organization_id, secret_ref, callback_token_hash, state, result_hash
FROM remote_tool_operations WHERE id = $1;

-- ConsumeRemoteCallback is the atomic one-use token consume: it flips a pending/timed_out row to
-- completed (a callback within the deadline) or late_result (after it), records the result + hash, and
-- RETURNs the new state. The token is verified constant-time in Go BEFORE this runs; the state gate makes
-- the consume one-use (a second callback matches 0 rows -> the idempotent/conflict path). A pending row
-- past its deadline resolves late_result, so the waiting executor (which accepts only 'completed') never
-- commits a late result — it reconciles through the prober instead.
-- name: ConsumeRemoteCallback
UPDATE remote_tool_operations
SET state = CASE WHEN state = 'pending' AND clock_timestamp() <= deadline THEN 'completed' ELSE 'late_result' END,
    result = $2, result_hash = $3, completed_at = clock_timestamp()
WHERE id = $1 AND state IN ('pending', 'timed_out')
RETURNING state;

-- ProberReadRemoteOperation reads the resolved result for an uncertain tool_call (the RemoteToolProber's
-- destination read, spec §26.7): the newest row that carries a result (completed or late_result). No such
-- row -> the operation never resolved, so the prober escalates to manual_resolution.
-- name: ProberReadRemoteOperation
SELECT state, result
FROM remote_tool_operations
WHERE tool_call_id = $1 AND result IS NOT NULL
ORDER BY completed_at DESC NULLS LAST
LIMIT 1;
