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
     attempt_fence, posture, handle, machine_id, state, output_path, env_keys, deadline_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 'running', $12, $13, $14);

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
--
-- deadline_at RIDES THIS READ because it is the reaper's only clock (E26 T5, §0.2). It is a column and
-- not a context for the reason the table exists: a context dies with the process that made it, and the
-- whole point of a background task is to outlive that process. NULL means the operator asked for no
-- ceiling and wrote a `0` to say so.
-- name: RunningBackgroundTasks
SELECT b.id, b.organization_id, b.project_id, b.run_id, b.session_id, b.response_id,
       b.posture, b.handle, b.machine_id, b.output_path, b.env_keys, b.deadline_at,
       COALESCE((
           SELECT a.host_path
           FROM workspace_allocations a
           WHERE a.workspace_id = (
                   SELECT w.id FROM workspaces w
                   WHERE w.session_id = b.session_id
                     AND w.project_id = b.project_id
                   ORDER BY w.created_at, w.id
                   LIMIT 1
               )
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
SET state = CASE WHEN state = 'running' THEN $3 ELSE state END,
    exit_code = COALESCE(exit_code, $4::INTEGER),
    finished_at = COALESCE(finished_at, clock_timestamp()),
    notified_at = clock_timestamp()
WHERE id = $1 AND project_id = $2 AND notified_at IS NULL
RETURNING run_id, session_id, response_id, state, exit_code, output_path;

-- CountRunningBackgroundTasks IS THE CEILING'S ONLY SOURCE OF TRUTH (E26 T5, §0.3), and it counts ROWS
-- rather than anything this process remembers — E24 opened `runners.capacity` and no Go expression ever
-- read it, which is the mistake §3.6 D12 names and this query exists not to repeat.
--
-- THE HOST COUNT IS DELIBERATELY CROSS-TENANT and the run count deliberately is not. A machine ceiling
-- that saw only the caller's tenant would let every tenant have twenty and the Mac have as many as there
-- are tenants; the caller therefore runs this under WithSystemScope, exactly as the reconciler's sweeps
-- do, and scopes the per-run half in the predicate instead of in the session.
-- name: CountRunningBackgroundTasks
SELECT count(*) FILTER (WHERE run_id = $1 AND project_id = $2) AS per_run,
       count(*) AS per_host
FROM background_tasks
WHERE state = 'running';

-- BackgroundTaskForRun resolves a task id AND enforces that it belongs to the asking run in ONE
-- statement, so no caller can do the lookup without doing the check. It is what makes a restart
-- survivable: before it, the id -> handle mapping lived in a map that died with the process that made
-- it, and a control plane that restarted could no longer stop a build it had started.
--
-- It is NOT filtered by state. A kill of a task that already finished must be idempotent (§3.5 P7's
-- "cancelling twice is idempotent"), and a row filtered out by state would report "no such task" for a
-- task the model can see in its own transcript.
-- name: BackgroundTaskForRun
SELECT id, posture, handle, output_path, state
FROM background_tasks
WHERE id = $1 AND run_id = $2 AND project_id = $3;

-- RunningBackgroundTasksOfRun is the park gate's read (E26 T3) and the cancellation's kill list (T5).
-- Both used to ask an in-memory map; a map answers wrongly after a restart in two opposite directions —
-- a run that should park does not, and a cancellation kills nothing.
-- name: RunningBackgroundTasksOfRun
SELECT id, posture, handle, output_path
FROM background_tasks
WHERE run_id = $1 AND project_id = $2 AND state = 'running'
ORDER BY started_at, id;

-- SettleEndedBackgroundTask records a task THIS CONTROL PLANE ENDED — a model's explicit kill or a run
-- cancellation — as opposed to one it merely observed finishing.
--
-- IT STAMPS notified_at AND THAT IS THE POINT. The sweep's exactly-once claim is `notified_at IS NULL`,
-- so a row left unstamped would be picked up on the next tick and turned into an exit notification for a
-- task somebody deliberately ended; on a cancelled run that notification has no turn to land in and
-- would become an orphaned-notice warning an operator has to explain. A kill we performed needs no
-- notification: the caller that performed it already knows.
--
-- `state = 'running'` in the predicate keeps it monotonic: a task the sweep already settled keeps the
-- answer the probe gave.
-- name: SettleEndedBackgroundTask
UPDATE background_tasks
SET state = $3,
    finished_at = COALESCE(finished_at, clock_timestamp()),
    notified_at = COALESCE(notified_at, clock_timestamp())
WHERE id = $1 AND project_id = $2 AND state = 'running';

-- BackgroundLogRoots and RunningBackgroundTaskIDs are the log retention sweep's two inputs (E26 T5).
--
-- THE ROOTS COME FROM THE ALLOCATIONS AND NOT FROM THE TASK HISTORY, and that is what keeps the sweep
-- proportional to what is on disk rather than to what has ever run. A DISTINCT over background_tasks
-- would grow with the table forever; the allocation set is bounded by the workspaces this machine
-- currently holds, and an allocation that is gone took its logs with it.
-- name: BackgroundLogRoots
SELECT DISTINCT host_path
FROM workspace_allocations
WHERE state = 'active' AND host_path <> ''
ORDER BY host_path;

-- The live ids are the sweep's only exclusion, and it is by TASK ID rather than by age: a build that
-- prints nothing for a day is not a finished build, and truncating its output mid-flight would be worse
-- than keeping it. The log file is named for the task, so the id is the join.
-- name: RunningBackgroundTaskIDs
SELECT id FROM background_tasks WHERE state = 'running' ORDER BY id;
