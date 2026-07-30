-- Background task queries (E26, migration 000047). A background task is a PROCESS that outlives the
-- tool call that started it, so everything here is written for a reader that may be a different control
-- plane from the one that spawned it: no query trusts memory, and the two that decide anything are
-- single-winner UPDATEs rather than read-then-write pairs.

-- InsertBackgroundTask records one started process. It is written AFTER the operating system has the
-- process and the handle is known, because handle is NOT NULL and a placeholder would be a row that
-- names nothing — and the caller kills the process if this insert fails, since a task with no row is an
-- orphan no reaper will ever look at.
--
-- env_keys carries KEY NAMES ONLY (migration 000047's own comment); nothing in this file ever sees a
-- value, and the read path re-resolves them to redact the log (T6).
-- name: InsertBackgroundTask
INSERT INTO background_tasks
    (id, organization_id, project_id, run_id, session_id, response_id, tool_call_id,
     attempt_fence, posture, handle, state, output_path, env_keys, deadline_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'running', $11, $12, $13);

-- RunningBackgroundTasks is the reaper's read: every task the DATABASE still believes is running,
-- across every tenant, so it runs under WithSystemScope like the other sweeps. The partial index
-- background_tasks_running_idx serves exactly this predicate, so the read stays proportional to the
-- live set rather than to the accumulating history.
--
-- allocation_root IS JOINED HERE AND NOT STORED ON THE ROW, and that is migration 000047's decision
-- rather than this query's: output_path is allocation-RELATIVE so that the row stays true when the
-- allocation moves and so that no row discloses a host path (spec §29.9). The absolute path is
-- therefore reconstructed the same way provisioning reconstructs it — the session's workspace, then
-- that workspace's current (max-fence) allocation — which is `WorkspaceForSession` followed by
-- `CurrentAllocation`, expressed as one join so the sweep makes one round trip per tick instead of two
-- per task.
--
-- EVERY ORDERING HERE IS EXPLICIT, INCLUDING THE TWO INSIDE THE SUBQUERIES. A `LIMIT 1` with no
-- ORDER BY has decided an outcome in this tree more than once; picking "some workspace" or "some
-- allocation" would make the tail a model reads depend on physical row order. The workspace tie-break
-- is (created_at, id) — the same order WorkspaceForSession takes — and the allocation is max fence,
-- which is what CurrentAllocation means.
--
-- An empty root is a legitimate answer (a run with no workspace attached): the sweep then notifies
-- WITHOUT a tail rather than skipping the notification, because losing an exit is worse than losing an
-- excerpt.
-- name: RunningBackgroundTasks
SELECT b.id, b.organization_id, b.project_id, b.run_id, b.session_id, b.response_id,
       b.posture, b.handle, b.output_path, b.env_keys,
       COALESCE((
           SELECT a.host_path
           FROM workspace_allocations a
           WHERE a.workspace_id = (
                   SELECT w.id FROM workspaces w
                   WHERE w.session_id = b.session_id
                     AND w.organization_id = b.organization_id
                     AND w.project_id = b.project_id
                   ORDER BY w.created_at, w.id
                   LIMIT 1
               )
             AND a.organization_id = b.organization_id
             AND a.project_id = b.project_id
           ORDER BY a.fence DESC
           LIMIT 1
       ), '') AS allocation_root
FROM background_tasks b
WHERE b.state = 'running'
ORDER BY b.started_at, b.id;

-- ClaimBackgroundNotice IS THE EXACTLY-ONCE KEY, and it is one statement because two would be a race.
-- Two reconciler ticks, two control planes and a crash-restart all reach this line; the one whose
-- UPDATE finds notified_at still NULL takes the row and every other reads zero rows. There is no
-- in-memory flag anywhere in the path, deliberately: a flag is erased by the restart this is meant to
-- survive.
--
-- THE STATE IS ONLY WRITTEN IF THE ROW IS STILL `running`. A reaper that already classified this task
-- (killed on a cancellation, expired on a deadline — T5) knows something a probe cannot re-derive, and
-- overwriting its answer with `exited` would erase why the task ended. COALESCE does the same for
-- exit_code and finished_at: first writer wins, and NULL stays NULL when nobody knows, which is the
-- column's whole point.
--
-- It RETURNS what actually landed rather than what the caller proposed, so the notification quotes the
-- ROW. A model must not be told "exit code 0" about a row that says killed.
-- name: ClaimBackgroundNotice
UPDATE background_tasks
SET state = CASE WHEN state = 'running' THEN $4 ELSE state END,
    exit_code = COALESCE(exit_code, $5::INTEGER),
    finished_at = COALESCE(finished_at, clock_timestamp()),
    notified_at = clock_timestamp()
WHERE id = $1 AND organization_id = $2 AND project_id = $3 AND notified_at IS NULL
RETURNING run_id, session_id, response_id, state, exit_code, output_path;
