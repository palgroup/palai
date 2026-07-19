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

-- PendingBoundaryCommands is the pump's boundary read: a run's queued commands the pump
-- applies at a safe boundary — send_message (queue/steer, and an interrupt that was not caught
-- in-flight, which degrades to a boundary delivery) and change_config (a normal switch, or an
-- immediate switch whose interrupt missed the in-flight window). A command that has left
-- 'queued' never reappears — the deliver-once guarantee (spec §9.2, §9.3). kind lets the pump
-- branch: deliver a message vs. apply a config revision.
-- name: PendingBoundaryCommands
SELECT id, kind, delivery, payload
FROM commands
WHERE run_id = $1 AND organization_id = $2 AND project_id = $3
  AND state = 'queued' AND kind IN ('send_message', 'change_config')
ORDER BY created_at;

-- ExpireQueuedCommandsForRun expires a run's still-queued commands when the run terminalizes
-- (spec §22.4 lifecycle): a command accepted mid-run that never reached a delivery boundary
-- must not sit queued forever. change_config is EXCLUDED — a config switch with no boundary in
-- its run carries to the next run's start (the cross-run config carry, spec §9.3), so it stays
-- queued for the session-config drain, not expired here. RETURNs the swept ids for command.expired.v1.
-- name: ExpireQueuedCommandsForRun
UPDATE commands
SET state = 'expired', updated_at = clock_timestamp()
WHERE run_id = $1 AND organization_id = $2 AND project_id = $3 AND state = 'queued'
  AND kind <> 'change_config'
RETURNING id;

-- PendingSessionConfigCommands is the run-start drain's read: the session's still-queued
-- change_config commands, in creation order, applied before the first model step so the run
-- routes under any switch that had no boundary in its own run — an idle-session change, or a
-- single-step run (spec §9.3). Session-scoped (not run-scoped): it carries a change accepted
-- against a now-terminal run OR with no run at all. Single-winner apply skips any a boundary or
-- the interrupt watcher already settled.
-- name: PendingSessionConfigCommands
SELECT id, kind, delivery, payload
FROM commands
WHERE session_id = $1 AND organization_id = $2 AND project_id = $3
  AND state = 'queued' AND kind = 'change_config'
ORDER BY created_at;

-- PendingInterruptCommand is the in-flight-abort watcher's read: the oldest queued command that
-- demands aborting the current model step (spec §9.2, §9.3, §25.11) — a send_message interrupt,
-- or a change_config immediate switch (both carry delivery = 'interrupt'). kind lets the handler
-- branch: fold a delivered message vs. apply a config revision + warn. The single-winner apply
-- then decides whether the watcher or a boundary settles it.
-- name: PendingInterruptCommand
SELECT id, kind, payload
FROM commands
WHERE run_id = $1 AND organization_id = $2 AND project_id = $3
  AND state = 'queued' AND delivery = 'interrupt' AND kind IN ('send_message', 'change_config')
ORDER BY created_at
LIMIT 1;
