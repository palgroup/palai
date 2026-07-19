-- Durable command queries (spec §22.4, §9.2). Every read/write is tenant-scoped: a
-- command_id or session from another tenant matches no row, so a cross-tenant id leaks
-- nothing (§39.2). The command's own (org, project, id) uniqueness carries idempotency —
-- a duplicate command_id returns the original row rather than re-applying.

-- InsertCommand reserves a command atomically. ON CONFLICT DO NOTHING makes a duplicate
-- command_id a no-op that RETURNs no row, so the caller reads and replays the original.
-- name: InsertCommand
INSERT INTO commands (id, organization_id, project_id, session_id, run_id, kind, delivery, payload, state)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'queued')
ON CONFLICT (organization_id, project_id, id) DO NOTHING
RETURNING id;

-- GetCommand reads a command's projection fields for the accept/replay response.
-- name: GetCommand
SELECT session_id, kind, delivery, state, applied_sequence, result, created_at
FROM commands
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- LockCommand reads and locks a command so an apply/reject transition sees a stable state
-- (the single-winner gate: only the tx that finds it 'queued' advances it).
-- name: LockCommand
SELECT run_id, kind, state
FROM commands
WHERE id = $1 AND organization_id = $2 AND project_id = $3
FOR UPDATE;

-- SetCommandState advances a command to an intermediate state (queued -> applying).
-- name: SetCommandState
UPDATE commands
SET state = $4, updated_at = clock_timestamp()
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- CompleteCommandApplied records the terminal applied state and the journal sequence where
-- the command took effect (spec §22.4 applied_sequence).
-- name: CompleteCommandApplied
UPDATE commands
SET state = 'applied', applied_sequence = $4, updated_at = clock_timestamp()
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- CompleteCommandRejected records the terminal rejected state and the caller-facing result.
-- name: CompleteCommandRejected
UPDATE commands
SET state = 'rejected', result = $4, updated_at = clock_timestamp()
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- ActiveRootRun resolves a session's live (non-terminal) root run and its response, so an
-- accepted command targets the loop the pump will steer and the response its journal events
-- belong to. No live run (all terminal) yields no row, which the accept path renders as a
-- typed rejection (no active loop). Terminal states are the RunTable destinations; the latest
-- live run wins if several exist (one-active-root is a T4 constraint).
-- name: ActiveRootRun
SELECT id, response_id
FROM runs
WHERE session_id = $1 AND organization_id = $2 AND project_id = $3
  AND state NOT IN ('completed', 'failed', 'canceled', 'timed_out', 'budget_exceeded')
ORDER BY created_at DESC
LIMIT 1;

-- PendingSendMessageCommands is the pump's boundary read: a run's queued send_message
-- commands in creation order. A command that has left 'queued' is already delivered, so it
-- never reappears — that is the deliver-once guarantee (spec §9.2). It includes interrupt
-- deliveries: an interrupt not caught in-flight degrades to a boundary delivery here.
-- name: PendingSendMessageCommands
SELECT id, delivery, payload
FROM commands
WHERE run_id = $1 AND organization_id = $2 AND project_id = $3
  AND state = 'queued' AND kind = 'send_message'
ORDER BY created_at;

-- ExpireQueuedCommandsForRun expires a run's still-queued commands when the run terminalizes
-- (spec §22.4 lifecycle): a command accepted mid-run that never reached a delivery boundary
-- must not sit queued forever. RETURNs the swept ids so each gets a command.expired.v1 event.
-- name: ExpireQueuedCommandsForRun
UPDATE commands
SET state = 'expired', updated_at = clock_timestamp()
WHERE run_id = $1 AND organization_id = $2 AND project_id = $3 AND state = 'queued'
RETURNING id;

-- PendingInterruptCommand is the in-flight-abort watcher's read: the oldest queued interrupt
-- for a run (spec §9.2, §25.11). A found row means the current model step should be aborted;
-- the single-winner apply then decides whether the watcher or a boundary delivers it.
-- name: PendingInterruptCommand
SELECT id, payload
FROM commands
WHERE run_id = $1 AND organization_id = $2 AND project_id = $3
  AND state = 'queued' AND kind = 'send_message' AND delivery = 'interrupt'
ORDER BY created_at
LIMIT 1;
