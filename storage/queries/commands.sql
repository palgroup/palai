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

-- SetCommandResult stores a command's caller-facing result without changing its state — the
-- fork_session apply uses it to carry the new child session id back to the caller.
-- name: SetCommandResult
UPDATE commands
SET result = $4, updated_at = clock_timestamp()
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- ForkCopyResponses reference-copies a parent session's immutable (terminal, unpurged) response
-- history into the fork child up to the fork boundary — every response that exists at fork time
-- (spec §22.8). Fresh ids (gen_random_uuid, core in PG13+); the child owns the copies, so its
-- SessionHistory reads them unchanged and a response written to the PARENT after the fork is never
-- copied — the fork's future is isolated. ponytail: purged tombstones are skipped (they carry no
-- content); add a redacted_content copy if fork fidelity to §22.2 markers ever matters.
-- name: ForkCopyResponses
INSERT INTO responses (id, organization_id, project_id, session_id, state, input, output, store, created_at)
SELECT 'resp_' || replace(gen_random_uuid()::text, '-', ''), organization_id, project_id, $4, state, input, output, store, created_at
FROM responses
WHERE session_id = $1 AND organization_id = $2 AND project_id = $3
  AND state IN ('completed', 'failed', 'canceled', 'timed_out', 'budget_exceeded')
  AND purged_at IS NULL;

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
-- ExpireQueuedCommandsForRun expires a terminal run's still-queued commands (spec §22.4 lifecycle).
-- change_config is NEVER expired (it carries cross-run regardless of the terminal kind). send_message
-- carries ONLY on a CLEAN completion ($4 = false): a message that never folded into a completed response
-- stays queued, is warned, and carries to the next response (E10 T7 ENG-012 fork 3). On a canceled/failed
-- terminal ($4 = true) it IS expired — an aborted run has no clean next response to carry into.
-- name: ExpireQueuedCommandsForRun
UPDATE commands
SET state = 'expired', updated_at = clock_timestamp()
WHERE run_id = $1 AND organization_id = $2 AND project_id = $3 AND state = 'queued'
  AND kind <> 'change_config'
  AND (kind <> 'send_message' OR $4)
RETURNING id;

-- SurvivingQueuedSendMessagesForRun returns the send_message commands that survive a run's terminal
-- (they carry to the next response, E10 T7 fork 3) so the terminal sweep can journal warning.raised.v1
-- for each — the user SEES that a mid-run message did not fold into this response and will carry.
-- name: SurvivingQueuedSendMessagesForRun
SELECT id FROM commands
WHERE run_id = $1 AND organization_id = $2 AND project_id = $3 AND state = 'queued' AND kind = 'send_message'
ORDER BY created_at, id;

-- CarrySessionSendMessages re-scopes a session's still-queued send_message commands to a fresh run at
-- its run.start (E10 T7 fork 3, the cross-run carry — the send_message analogue of the change_config
-- carry): a message queued on a prior terminal run becomes a normal queued command on the new run, so
-- the new run's ordinary boundary pump delivers it at its first input boundary. No new delivery path.
-- name: CarrySessionSendMessages
UPDATE commands
SET run_id = $4, updated_at = clock_timestamp()
WHERE session_id = $1 AND organization_id = $2 AND project_id = $3
  AND state = 'queued' AND kind = 'send_message' AND run_id <> $4;

-- ExpireQueuedSessionCommands expires a session's still-queued commands when the session closes
-- (spec §22.1, §22.4 lifecycle — the F1 close-sweep). close_session is a session's lifecycle
-- exit: a change_config queued for the cross-run carry (ExpireQueuedCommandsForRun excludes it)
-- would otherwise sit queued forever once the session never runs again, so close sweeps every
-- still-queued command. The close command itself ($4) is excluded — it is being applied, not
-- expired. RETURNs the swept ids for command.expired.v1.
-- name: ExpireQueuedSessionCommands
UPDATE commands
SET state = 'expired', updated_at = clock_timestamp()
WHERE session_id = $1 AND organization_id = $2 AND project_id = $3 AND state = 'queued' AND id <> $4
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

-- PendingPauseCommand is the boundary pump's pause read (spec §22.3, SES-009): the oldest queued
-- pause command for a run. A pause pre-empts the boundary — the pump applies it and stops driving
-- the loop, leaving every other queued command for resume to re-deliver — so it is read before the
-- boundary delivery set, not mixed into it.
-- name: PendingPauseCommand
SELECT id
FROM commands
WHERE run_id = $1 AND organization_id = $2 AND project_id = $3
  AND state = 'queued' AND kind = 'pause'
ORDER BY created_at
LIMIT 1;

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

-- InsertDeliveredMessage journals a boundary-delivered send_message durable at apply (spec §26.9,
-- E10 Task 2), in the SAME transaction as command.applied.v1 — so an applied command always has its
-- delivered-message row (variant-1's "applied lie" is closed). ON CONFLICT DO NOTHING makes it
-- idempotent: a command applies once (single-winner), so this writes at most one row per command.
-- name: InsertDeliveredMessage
INSERT INTO delivered_messages (command_id, organization_id, project_id, run_id, boundary_request_id, applied_sequence)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (organization_id, project_id, command_id) DO NOTHING;

-- MarkDeliveredMessagesFolded advances a run's still-'delivered' rows to 'folded' when a model step
-- commits (spec §26.9): a message delivered at a prior boundary was folded into the request the
-- committing step just answered. Runs in CommitModelResult's transaction, so the fold state and the
-- committed result move together. Idempotent (already-folded rows are untouched).
-- name: MarkDeliveredMessagesFolded
UPDATE delivered_messages
SET fold_state = 'folded', updated_at = clock_timestamp()
WHERE run_id = $1 AND organization_id = $2 AND project_id = $3 AND fold_state = 'delivered';

-- RedeliverBoundaryMessages reads the messages a run recorded at one input boundary, so a fresh
-- attempt redelivers them at that SAME boundary during reconstruction (spec §26.9). It joins the
-- command for the content/delivery it references (the row itself keeps no customer content), in
-- applied_sequence (canonical) order. Both 'delivered' (fold uncommitted — variant-1) and 'folded'
-- (fold committed — R1) rows are returned: reconstructing the conversation folds the turn at its
-- original boundary either way (uniform boundary-keyed redelivery).
-- name: RedeliverBoundaryMessages
SELECT d.command_id, c.delivery, c.payload, d.applied_sequence, d.fold_state
FROM delivered_messages d
JOIN commands c
  ON c.organization_id = d.organization_id AND c.project_id = d.project_id AND c.id = d.command_id
WHERE d.run_id = $1 AND d.organization_id = $2 AND d.project_id = $3 AND d.boundary_request_id = $4
ORDER BY d.applied_sequence;
