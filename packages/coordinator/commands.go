package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	statemachines "github.com/palgroup/palai/packages/state-machines"
	"github.com/palgroup/palai/storage"
)

// command.accepted.v1 is the command's birth event, not a CommandTable transition (see
// packages/state-machines/command.go). The applied/rejected event names ARE table
// transitions, sourced from the table so it stays the single definition of those names.
const commandAcceptedEvent = "command.accepted.v1"

var (
	commandAppliedEvent  = mustCommandEvent(statemachines.CommandApplying, statemachines.CommandCmdFinishApply)
	commandRejectedEvent = mustCommandEvent(statemachines.CommandApplying, statemachines.CommandCmdReject)
	commandExpiredEvent  = mustCommandEvent(statemachines.CommandApplying, statemachines.CommandCmdExpire)
)

// ErrCommandNotPending reports an apply/reject on a command that is not (or no longer)
// queued — an already-applied command, or one another pump already claimed. It makes the
// pump idempotent: a redelivered boundary re-reads the pending set and skips it.
var ErrCommandNotPending = errors.New("command_not_pending")

// mustCommandEvent reads the event a CommandTable transition emits, panicking at init if the
// table has no such row — a broken single source of truth is a build-time defect.
func mustCommandEvent(from statemachines.CommandState, cmd statemachines.CommandCommand) string {
	_, event, err := statemachines.Apply(from, cmd, statemachines.CommandTable)
	if err != nil {
		panic(fmt.Sprintf("command table has no %v->%v transition: %v", from, cmd, err))
	}
	return event
}

// CommandInput is an accepted command's caller-supplied fields (spec §22.4). Payload carries
// the command's own content (e.g. the send_message text) as customer content.
type CommandInput struct {
	CommandID string
	Kind      string // send_message | change_config | approve | deny | pause | resume | fork_session | close_session
	Delivery  string // queue | steer | interrupt (send_message only)
	Payload   []byte
	// ForkSessionID is the freshly minted child session id a fork_session opens; the store adapter
	// mints it (one place mints session ids) and the coordinator creates it under the fork.
	ForkSessionID string
}

// Command is the durable command projection. Replayed marks a duplicate command_id that
// returned the original row rather than re-applying (spec §22.4 idempotency).
type Command struct {
	ID              string
	SessionID       string
	Kind            string
	Delivery        string
	State           string
	AppliedSequence *int64
	Result          []byte
	CreatedAt       time.Time
	Replayed        bool
	// SessionNotFound marks a command posted to an unknown or foreign session (404, no
	// existence disclosure) — decided before any row is created.
	SessionNotFound bool
}

// PendingCommand is one queued command the pump settles at a boundary — a send_message it
// delivers or a change_config it applies. Kind lets the pump branch.
type PendingCommand struct {
	ID       string
	Kind     string
	Delivery string
	Payload  []byte
}

// AcceptCommand durably records a command and resolves it as far as it can synchronously
// (spec §22.4, §9.2). A duplicate command_id returns the original row (idempotent, via the
// command table's own unique — not idempotency_records). A fresh command is journaled
// (command.accepted.v1), then: approve/deny are rejected here (no approval source until E09),
// a send_message with no live root run is rejected (no loop to steer), and a send_message on
// a live run is left queued for the command pump. Everything commits in one transaction, so a
// rejection leaves the command durably rejected and an unknown session leaves nothing behind.
func (s *Store) AcceptCommand(ctx context.Context, tenant Tenant, sessionID string, in CommandInput) (Command, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Command{}, fmt.Errorf("begin accept command: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	// The session must be visible in scope; a foreign/unknown id is a 404, never an FK error.
	// Its lifecycle state gates new work: a closed session rejects everything but its own exit.
	var sessionState string
	if err := tx.QueryRow(ctx, storage.Query("GetSessionInScope"), sessionID, tenant.Organization, tenant.Project).
		Scan(new(string), &sessionState, new(time.Time)); errors.Is(err, pgx.ErrNoRows) {
		return Command{SessionNotFound: true}, nil
	} else if err != nil {
		return Command{}, fmt.Errorf("resolve command session: %w", err)
	}

	// Resolve the session's live root run: the loop a steer/queue delivers to and the response
	// its journal events belong to. Absent (all terminal) means no loop to steer.
	var runID, responseID string
	switch err := tx.QueryRow(ctx, storage.Query("ActiveRootRun"), sessionID, tenant.Organization, tenant.Project).
		Scan(&runID, &responseID); {
	case errors.Is(err, pgx.ErrNoRows):
		// no live run — runID stays ""
	case err != nil:
		return Command{}, fmt.Errorf("resolve active root run: %w", err)
	}

	// Reserve the command. ON CONFLICT DO NOTHING makes a duplicate command_id return no row,
	// so we read and replay the original resource unchanged.
	err = tx.QueryRow(ctx, storage.Query("InsertCommand"),
		in.CommandID, tenant.Organization, tenant.Project, sessionID, nullableText(runID), in.Kind, nullableText(in.Delivery), in.Payload).
		Scan(new(string))
	if errors.Is(err, pgx.ErrNoRows) {
		cmd, err := readCommand(ctx, tx, tenant, in.CommandID)
		if err != nil {
			return Command{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return Command{}, fmt.Errorf("commit command replay: %w", err)
		}
		cmd.Replayed = true
		return cmd, nil
	}
	if err != nil {
		return Command{}, fmt.Errorf("insert command: %w", err)
	}

	acceptedPayload, _ := json.Marshal(map[string]any{"command_id": in.CommandID, "kind": in.Kind, "delivery": in.Delivery})
	if _, err := appendEvent(ctx, tx, tenant, sessionID, responseID, commandAcceptedEvent, acceptedPayload); err != nil {
		return Command{}, err
	}

	// Synchronous resolution. The session-lifecycle gate comes first: a non-active session rejects
	// new work (spec §22.1), except close_session which is that exit. Then the per-kind rules:
	// approve/deny have no approval source (E09); a send_message with no live run has no loop to
	// steer; a change_config outside the allowlist is denied before any revision (no silent
	// fallback — SES-008). close_session transitions the session and sweeps its queued commands
	// (F1). A permitted send_message on a live run stays queued for the pump; a permitted
	// change_config is queued regardless of run state (it can carry to the next run).
	if reason := sessionStateRejection(in.Kind, sessionState); reason != nil {
		if err := rejectCommandTx(ctx, tx, tenant, sessionID, responseID, in.CommandID, reason); err != nil {
			return Command{}, err
		}
	} else if in.Kind == "close_session" {
		if err := applyCloseSessionTx(ctx, tx, tenant, sessionID, runID, in.CommandID); err != nil {
			return Command{}, err
		}
	} else if in.Kind == "fork_session" {
		if err := applyForkSessionTx(ctx, tx, tenant, sessionID, in.ForkSessionID, in.CommandID); err != nil {
			return Command{}, err
		}
	} else if in.Kind == "resume" {
		// resume acts on a paused (waiting) run now — it re-enters running and enqueues a fresh
		// attempt — rather than queuing for a boundary (there is no live loop to deliver to).
		if err := applyResumeTx(ctx, tx, tenant, sessionID, responseID, runID, in.CommandID); err != nil {
			return Command{}, err
		}
	} else {
		reason := rejectionReason(in.Kind, runID)
		switch in.Kind {
		case "change_config":
			reason, err = changeConfigRejection(ctx, tx, tenant, in.Payload)
			if err != nil {
				return Command{}, err
			}
		case "approve", "deny":
			// The E08 devir closes here: approve/deny consult the pending-approval source (the first
			// source is the push/PR publication). A pending approval keeps the command queued for the
			// boundary pump; none preserves the E08 no_pending_approval rejection.
			reason, err = approvalRejection(ctx, tx, tenant, sessionID)
			if err != nil {
				return Command{}, err
			}
		}
		if reason != nil {
			if err := rejectCommandTx(ctx, tx, tenant, sessionID, responseID, in.CommandID, reason); err != nil {
				return Command{}, err
			}
		}
	}

	cmd, err := readCommand(ctx, tx, tenant, in.CommandID)
	if err != nil {
		return Command{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Command{}, fmt.Errorf("commit accept command: %w", err)
	}
	return cmd, nil
}

// rejectionReason returns the typed rejection a command earns at accept time, or nil to keep
// it queued. approve/deny carry the no_pending_approval default here but are refined by approvalRejection
// (it needs the pending-approval source). change_config is resolved separately (changeConfigRejection)
// because its verdict needs the project policy, so neither is decided by this pure switch alone.
func rejectionReason(kind, runID string) map[string]any {
	switch kind {
	case "approve", "deny":
		return map[string]any{"code": "no_pending_approval", "detail": "no approval is pending for this session"}
	case "send_message":
		if runID == "" {
			// ponytail: spec §9.2 defers a QUEUED message on a terminal run into the NEXT
			// response's history; T2 rejects it (no live loop). That next-response-history carry
			// is T4 work — it needs the run.start history assembly to fold a pending message.
			return map[string]any{"code": "no_active_run", "detail": "the session has no active run to deliver to"}
		}
		return nil
	case "pause":
		// A pause needs a live run to cooperatively stop; with none it is a no-op rejection. It
		// stays queued for the boundary pump otherwise (the pump applies it at the next safe
		// boundary). ponytail: a pause on an already-waiting run (double-pause) queues and applies
		// on the next resume's first boundary; guarding on running specifically needs the run state
		// here — add it if double-pause ever becomes a real case.
		if runID == "" {
			return map[string]any{"code": "no_active_run", "detail": "the session has no active run to pause"}
		}
		return nil
	case "change_config":
		return nil // resolved by changeConfigRejection (needs the project policy)
	default:
		return map[string]any{"code": "unsupported_command", "detail": "the command kind is not supported"}
	}
}

// changeConfigRejection resolves a change_config command's typed rejection at accept, or nil to
// keep it queued (spec §9.3). Only policy denies a config change: a model or tool outside the
// project allowlist is denied outright, never silently narrowed to an allowed value (SES-008).
// Run state does NOT reject it — a permitted change is queued regardless. A change submitted
// mid-run applies at that run's next step boundary; one with no boundary in its own run (an idle
// session, or a single-step run) carries to the next run's start (the cross-run config carry).
func changeConfigRejection(ctx context.Context, tx pgx.Tx, tenant Tenant, payload []byte) (map[string]any, error) {
	var req struct {
		Model string   `json:"model"`
		Tools []string `json:"tools"`
	}
	_ = json.Unmarshal(payload, &req)
	policy, err := projectConfigTx(ctx, tx, tenant)
	if err != nil {
		return nil, err
	}
	if !policy.AllowModel(req.Model) {
		return map[string]any{"code": "model_not_allowed", "detail": "the requested model is outside the project allowlist"}, nil
	}
	if bad := policy.DeniedTool(req.Tools); bad != "" {
		return map[string]any{"code": "tool_not_allowed", "detail": "tool " + bad + " is outside the project allowlist"}, nil
	}
	return nil, nil
}

// approvalRejection resolves an approve/deny command's typed rejection at accept, or nil to keep it
// queued for the boundary pump (spec §22.4-22.5, APV-001). A side-effect tool (push/PR) records a
// pending publication (RequestPublication); an approve/deny with a pending approval in the session is
// queued and applied at the next boundary (ApplyApprovalDecision). With NO pending approval it is the
// E08 no_pending_approval rejection — the closure of the E08 devir, preserving
// TestApproveWithoutPendingApprovalRejected.
func approvalRejection(ctx context.Context, tx pgx.Tx, tenant Tenant, sessionID string) (map[string]any, error) {
	var pending bool
	if err := tx.QueryRow(ctx, storage.Query("SessionHasPendingApproval"), sessionID, tenant.Organization, tenant.Project).Scan(&pending); err != nil {
		return nil, fmt.Errorf("check pending approval: %w", err)
	}
	if !pending {
		return map[string]any{"code": "no_pending_approval", "detail": "no approval is pending for this session"}, nil
	}
	return nil, nil
}

// sessionStateRejection gates a command by the session's lifecycle state (spec §22.1): a
// non-active session rejects new work (send_message/change_config/fork/pause/resume), so a closed
// session accepts nothing new. close_session is the lifecycle exit itself and is always allowed
// (idempotent on an already-closed session), so it is never gated here.
func sessionStateRejection(kind, state string) map[string]any {
	if kind == "close_session" || state == string(statemachines.SessionActive) {
		return nil
	}
	return map[string]any{"code": "session_not_active", "detail": "the session is not active and accepts no new commands"}
}

// applyCloseSessionTx applies a close_session command (spec §22.1, §22.4): it transitions the
// session active/paused -> closing, sweeps its still-queued commands to expired (F1, closing the
// change_config orphan T3 left), finishes the close to closed when no root run is live, and marks
// the close command applied. The session row is locked so concurrent closes serialize, and an
// already-closed session is an idempotent no-op that still applies the command.
func applyCloseSessionTx(ctx context.Context, tx pgx.Tx, tenant Tenant, sessionID, activeRunID, commandID string) error {
	var state string
	if err := tx.QueryRow(ctx, storage.Query("LockSession"), sessionID, tenant.Organization, tenant.Project).Scan(&state); err != nil {
		return fmt.Errorf("lock session %s: %w", sessionID, err)
	}
	if s := statemachines.SessionState(state); s == statemachines.SessionActive || s == statemachines.SessionPaused {
		if err := applySessionTransitionTx(ctx, tx, tenant, sessionID, s, statemachines.SessionCmdClose); err != nil {
			return err
		}
		if err := sweepQueuedSessionCommands(ctx, tx, tenant, sessionID, commandID); err != nil {
			return err
		}
		// No live root run -> finish the close now; a live run keeps the session closing until it
		// terminalizes (new work is already rejected at closing).
		if activeRunID == "" {
			if err := applySessionTransitionTx(ctx, tx, tenant, sessionID, statemachines.SessionClosing, statemachines.SessionCmdFinishClose); err != nil {
				return err
			}
		}
	}
	// The close command is session-scoped (no response); mark it applied last so its
	// applied_sequence is the final boundary of the close.
	if _, err := applyCommandInTx(ctx, tx, tenant, sessionID, "", commandID); err != nil {
		return err
	}
	return nil
}

// applyForkSessionTx applies a fork_session command (spec §22.8): it opens a NEW active child
// session (a fresh journal — its own session_sequences allocate from zero), reference-copies the
// parent's immutable response history up to the fork boundary into the child, and marks the fork
// command applied with the child session id as its result. No workspace, and no pending-approval
// or lease is inherited (there is nothing to inherit — the child is a fresh session with only the
// copied history). A response written to the parent after the fork is never copied, so the fork's
// future is isolated.
func applyForkSessionTx(ctx context.Context, tx pgx.Tx, tenant Tenant, parentSessionID, childSessionID, commandID string) error {
	if childSessionID == "" {
		return fmt.Errorf("fork_session requires a child session id")
	}
	if _, err := tx.Exec(ctx, storage.Query("InsertSession"), childSessionID, tenant.Organization, tenant.Project); err != nil {
		return fmt.Errorf("insert fork child session: %w", err)
	}
	if _, err := tx.Exec(ctx, storage.Query("ForkCopyResponses"), parentSessionID, tenant.Organization, tenant.Project, childSessionID); err != nil {
		return fmt.Errorf("copy fork history: %w", err)
	}
	if _, err := applyCommandInTx(ctx, tx, tenant, parentSessionID, "", commandID); err != nil {
		return err
	}
	// The result carries the child session id so the caller can address the fork.
	if _, err := tx.Exec(ctx, storage.Query("SetCommandResult"),
		commandID, tenant.Organization, tenant.Project, mustMarshal(map[string]any{"session_id": childSessionID})); err != nil {
		return fmt.Errorf("set fork command result: %w", err)
	}
	return nil
}

// applyResumeTx applies a resume command (spec §22.3, SES-009): it re-enters a paused (waiting)
// root run into running and enqueues a fresh response.run job, so a worker opens a NEW attempt on
// the SAME run — one that replays the committed transcript from the journal and re-delivers any
// message the pause left queued. Resume needs a paused run: with no live run, or a run that is not
// waiting, it is a typed rejection (nothing to resume). The run transition, the job enqueue, and
// the command-applied commit together, so the job becomes claimable only once the run is durably
// running again (nothing dispatches before commit).
func applyResumeTx(ctx context.Context, tx pgx.Tx, tenant Tenant, sessionID, responseID, runID, commandID string) error {
	if runID == "" {
		return rejectCommandTx(ctx, tx, tenant, sessionID, responseID, commandID,
			map[string]any{"code": "no_paused_run", "detail": "the session has no paused run to resume"})
	}
	var lockedSession, lockedState string
	var lockedResponse *string
	if err := tx.QueryRow(ctx, storage.Query("LockRun"), runID, tenant.Organization, tenant.Project).
		Scan(&lockedSession, &lockedResponse, &lockedState); err != nil {
		return fmt.Errorf("lock run for resume: %w", err)
	}
	if statemachines.RunState(lockedState) != statemachines.RunWaiting {
		return rejectCommandTx(ctx, tx, tenant, sessionID, responseID, commandID,
			map[string]any{"code": "not_paused", "detail": "the run is not paused"})
	}
	if _, err := applyRunTransitionTx(ctx, tx, tenant, runID, statemachines.RunCmdResume); err != nil {
		return err
	}
	jobID, err := newJobID()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, storage.Query("EnqueueJob"),
		jobID, tenant.Organization, tenant.Project, "response.run", []byte(fmt.Sprintf(`{"run_id":%q}`, runID))); err != nil {
		return fmt.Errorf("enqueue resume job: %w", err)
	}
	resumeResponse := ""
	if lockedResponse != nil {
		resumeResponse = *lockedResponse
	}
	if _, err := applyCommandInTx(ctx, tx, tenant, lockedSession, resumeResponse, commandID); err != nil {
		return err
	}
	return nil
}

// applySessionTransitionTx applies one SessionTable transition within tx: it advances the session
// state and journals the session-scoped lifecycle event (response_id NULL — the event carries no
// customer content). Mirrors ApplyRunTransition for runs (spec §22.1).
func applySessionTransitionTx(ctx context.Context, tx pgx.Tx, tenant Tenant, sessionID string, current statemachines.SessionState, cmd statemachines.SessionCommand) error {
	next, event, err := statemachines.Apply(current, cmd, statemachines.SessionTable)
	if err != nil {
		return fmt.Errorf("session %s transition %s: %w", sessionID, cmd, err)
	}
	if _, err := tx.Exec(ctx, storage.Query("UpdateSessionState"), sessionID, tenant.Organization, tenant.Project, string(next)); err != nil {
		return fmt.Errorf("update session state: %w", err)
	}
	payload := mustMarshal(map[string]any{"session_id": sessionID, "state": next})
	if _, err := appendEvent(ctx, tx, tenant, sessionID, "", event, payload); err != nil {
		return err
	}
	return nil
}

// sweepQueuedSessionCommands expires the session's still-queued commands (except exceptID, the
// close command being applied) and journals command.expired.v1 per swept command (spec §22.4
// lifecycle — the F1 close-sweep). Mirrors sweepQueuedCommands for runs.
func sweepQueuedSessionCommands(ctx context.Context, tx pgx.Tx, tenant Tenant, sessionID, exceptID string) error {
	rows, err := tx.Query(ctx, storage.Query("ExpireQueuedSessionCommands"), sessionID, tenant.Organization, tenant.Project, exceptID)
	if err != nil {
		return fmt.Errorf("expire queued session commands: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("scan expired session command: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := appendEvent(ctx, tx, tenant, sessionID, "", commandExpiredEvent, mustMarshal(map[string]any{"command_id": id})); err != nil {
			return err
		}
	}
	return nil
}

// rejectCommandTx drives the command queued->rejected within tx and journals
// command.rejected.v1. The reason is the caller-facing result. The state UPDATE is
// unconditional here because the caller holds the row (fresh insert or FOR UPDATE lock).
func rejectCommandTx(ctx context.Context, tx pgx.Tx, tenant Tenant, sessionID, responseID, commandID string, reason map[string]any) error {
	result, _ := json.Marshal(reason)
	if _, err := tx.Exec(ctx, storage.Query("CompleteCommandRejected"),
		commandID, tenant.Organization, tenant.Project, result); err != nil {
		return fmt.Errorf("reject command: %w", err)
	}
	payload, _ := json.Marshal(map[string]any{"command_id": commandID, "reason": reason})
	if _, err := appendEvent(ctx, tx, tenant, sessionID, responseID, commandRejectedEvent, payload); err != nil {
		return err
	}
	return nil
}

// PendingBoundaryCommands returns a run's queued boundary commands in creation order — the
// pump's read. It carries send_message (delivered) and change_config (applied); Kind lets the
// pump branch. A command that has left 'queued' never reappears — the deliver-once guarantee
// (spec §9.2, §9.3).
func (s *Store) PendingBoundaryCommands(ctx context.Context, tenant Tenant, runID string) ([]PendingCommand, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	rows, err := s.pool.Query(ctx, storage.Query("PendingBoundaryCommands"), runID, tenant.Organization, tenant.Project)
	if err != nil {
		return nil, fmt.Errorf("read pending commands: %w", err)
	}
	defer rows.Close()
	var out []PendingCommand
	for rows.Next() {
		var c PendingCommand
		var delivery *string
		if err := rows.Scan(&c.ID, &c.Kind, &delivery, &c.Payload); err != nil {
			return nil, fmt.Errorf("scan pending command: %w", err)
		}
		if delivery != nil {
			c.Delivery = *delivery
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// PendingSessionConfigCommands returns a session's still-queued change_config commands in
// creation order — the run-start drain's read, applied before the first model step so a switch
// with no boundary in its own run (an idle-session change, or a single-step run) takes effect on
// the next run (spec §9.3). Single-winner apply skips any already settled at a boundary or by the
// interrupt watcher, so re-draining on a reclaimed attempt is a no-op.
func (s *Store) PendingSessionConfigCommands(ctx context.Context, tenant Tenant, sessionID string) ([]PendingCommand, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	rows, err := s.pool.Query(ctx, storage.Query("PendingSessionConfigCommands"), sessionID, tenant.Organization, tenant.Project)
	if err != nil {
		return nil, fmt.Errorf("read pending session config commands: %w", err)
	}
	defer rows.Close()
	var out []PendingCommand
	for rows.Next() {
		var c PendingCommand
		var delivery *string
		if err := rows.Scan(&c.ID, &c.Kind, &delivery, &c.Payload); err != nil {
			return nil, fmt.Errorf("scan pending session config command: %w", err)
		}
		if delivery != nil {
			c.Delivery = *delivery
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ApplyCommand advances a queued command to applied and journals command.applied.v1, whose
// sequence is the applied_sequence — the journal boundary where the command took effect
// (spec §22.4). It runs under guardRunActive so a terminal run rejects the write (the pump's
// fence discipline via the same run-terminal guard the model/tool commits use), and the
// state UPDATE is single-winner (WHERE state='queued'), so a redelivered boundary applies a
// command exactly once. The caller sends the message.deliver frame only after this commits
// (commit-before-deliver). A not-queued command returns ErrCommandNotPending.
//
// It is the boundary send_message apply (the only ApplyCommand caller), so it also journals the
// durable delivered-message row in the SAME transaction (spec §26.9, E10 Task 2): boundaryRequestID
// is the model_request_id of the step at whose boundary the message is delivered, the key a fresh
// attempt redelivers it under. command.applied.v1 therefore now means the delivered message is
// durable, not merely in the engine's memory — the crash-before-fold and pause/resume losses close.
// The interrupt-path fold (InterruptModelStep) writes the SAME durable row keyed by the aborted step's
// boundary (E10 Task 7, ENG-012), so both delivery paths redeliver at the input boundary.
func (s *Store) ApplyCommand(ctx context.Context, tenant Tenant, sessionID, responseID, runID, commandID, boundaryRequestID string) (int64, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, fmt.Errorf("begin apply command: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if err := guardRunActive(ctx, tx, tenant, runID); err != nil {
		return 0, err
	}
	seq, err := applyCommandInTx(ctx, tx, tenant, sessionID, responseID, commandID)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, storage.Query("InsertDeliveredMessage"),
		commandID, tenant.Organization, tenant.Project, runID, nullableText(boundaryRequestID), seq); err != nil {
		return 0, fmt.Errorf("record delivered message: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit apply command: %w", err)
	}
	return seq, nil
}

// RedeliveredMessage is a delivered-message row read back for redelivery on a fresh attempt (spec
// §26.9, E10 Task 2): the command it references, that command's delivery mode and content payload,
// and the fold state at record time. The orchestrator folds it at the input boundary by re-sending
// the message.deliver frame.
type RedeliveredMessage struct {
	CommandID       string
	Delivery        string
	Payload         []byte
	AppliedSequence int64
	FoldState       string
}

// RedeliverBoundaryMessages returns the messages a run recorded at one input boundary, in canonical
// (applied_sequence) order, so a reconstructing attempt refolds them at that same boundary (spec
// §26.9). Both delivered and folded rows are returned — reconstruction folds the turn at its
// original boundary either way. The read is tenant-scoped; a boundary with no recorded message
// returns no rows.
func (s *Store) RedeliverBoundaryMessages(ctx context.Context, tenant Tenant, runID, boundaryRequestID string) ([]RedeliveredMessage, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	rows, err := s.pool.Query(ctx, storage.Query("RedeliverBoundaryMessages"),
		runID, tenant.Organization, tenant.Project, boundaryRequestID)
	if err != nil {
		return nil, fmt.Errorf("read boundary messages: %w", err)
	}
	defer rows.Close()
	var out []RedeliveredMessage
	for rows.Next() {
		var m RedeliveredMessage
		if err := rows.Scan(&m.CommandID, &m.Delivery, &m.Payload, &m.AppliedSequence, &m.FoldState); err != nil {
			return nil, fmt.Errorf("scan boundary message: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// applyCommandInTx is the single-winner apply shared by the boundary pump (ApplyCommand) and
// the in-flight-abort path (InterruptModelStep): it claims a queued command and journals
// command.applied.v1, whose own sequence is the applied_sequence it carries (spec §22.4). The
// CommandTable path is queued->applying->applied; applying is the in-transaction apply state,
// so only the accept and applied boundaries are journaled. A not-queued command (already
// applied, or claimed by a racing path) returns ErrCommandNotPending.
func applyCommandInTx(ctx context.Context, tx pgx.Tx, tenant Tenant, sessionID, responseID, commandID string) (int64, error) {
	var runIDCol *string
	var kind, state string
	if err := tx.QueryRow(ctx, storage.Query("LockCommand"), commandID, tenant.Organization, tenant.Project).
		Scan(&runIDCol, &kind, &state); errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrCommandNotPending
	} else if err != nil {
		return 0, fmt.Errorf("lock command: %w", err)
	}
	if statemachines.CommandState(state) != statemachines.CommandQueued {
		return 0, ErrCommandNotPending
	}

	// Allocate the sequence first so the applied event can name the boundary it marks.
	var seq int64
	if err := tx.QueryRow(ctx, storage.Query("AllocateSequence"), sessionID).Scan(&seq); err != nil {
		return 0, fmt.Errorf("allocate command sequence: %w", err)
	}
	eventID, err := newEventID()
	if err != nil {
		return 0, err
	}
	appliedPayload := mustMarshal(map[string]any{"command_id": commandID, "applied_sequence": seq})
	if _, err := tx.Exec(ctx, storage.Query("AppendEvent"),
		eventID, tenant.Organization, tenant.Project, sessionID, nullableText(responseID), seq, commandAppliedEvent, appliedPayload); err != nil {
		return 0, fmt.Errorf("append command applied event: %w", err)
	}
	if _, err := tx.Exec(ctx, storage.Query("CompleteCommandApplied"),
		commandID, tenant.Organization, tenant.Project, seq); err != nil {
		return 0, fmt.Errorf("apply command: %w", err)
	}
	return seq, nil
}

// PendingPauseCommand returns the oldest queued pause command for a run — the boundary pump's
// pause read (spec §22.3, SES-009). A pause pre-empts the boundary: the pump applies it and stops
// driving the loop, so it is read before the boundary delivery set, not mixed into it. found is
// false when none is pending.
func (s *Store) PendingPauseCommand(ctx context.Context, tenant Tenant, runID string) (commandID string, found bool, err error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	switch err := s.pool.QueryRow(ctx, storage.Query("PendingPauseCommand"), runID, tenant.Organization, tenant.Project).
		Scan(&commandID); {
	case errors.Is(err, pgx.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("read pending pause: %w", err)
	}
	return commandID, true, nil
}

// PauseRun applies a pause command at a safe loop boundary (spec §22.3, SES-009): in one
// transaction it drives the run running -> waiting (its compute is released once the attempt ends)
// and marks the pause command applied, so the paused run and the applied pause commit together. It
// is the cooperative half of pause — the orchestrator stops driving the loop once this returns.
// Returns the pause's applied_sequence. A run already terminal (it finished on this step) rejects
// with ErrRunTerminal, which the caller treats as "nothing to pause".
func (s *Store) PauseRun(ctx context.Context, tenant Tenant, sessionID, responseID, runID, commandID string) (int64, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, fmt.Errorf("begin pause: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if _, err := applyRunTransitionTx(ctx, tx, tenant, runID, statemachines.RunCmdWait); err != nil {
		return 0, err
	}
	seq, err := applyCommandInTx(ctx, tx, tenant, sessionID, responseID, commandID)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit pause: %w", err)
	}
	return seq, nil
}

// PendingInterrupt is the in-flight-abort watcher's verdict: the queued command that demands
// aborting the current model step. Kind branches the handler (send_message vs change_config);
// Payload carries the command content (the message text, or the config change fields).
type PendingInterrupt struct {
	CommandID string
	Kind      string
	Payload   []byte
}

// PendingInterruptCommand returns the oldest queued command that demands aborting the current
// model step, for the in-flight-abort watcher (spec §9.2, §9.3, §25.11) — a send_message
// interrupt or a change_config immediate switch. found is false when none is pending.
func (s *Store) PendingInterruptCommand(ctx context.Context, tenant Tenant, runID string) (hit PendingInterrupt, found bool, err error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	switch err := s.pool.QueryRow(ctx, storage.Query("PendingInterruptCommand"), runID, tenant.Organization, tenant.Project).
		Scan(&hit.CommandID, &hit.Kind, &hit.Payload); {
	case errors.Is(err, pgx.ErrNoRows):
		return PendingInterrupt{}, false, nil
	case err != nil:
		return PendingInterrupt{}, false, fmt.Errorf("read pending interrupt: %w", err)
	}
	return hit, true, nil
}

// InterruptModelStep records an aborted model step and applies the interrupt command in one
// transaction (spec §9.2, §25.11): it journals the partial step event (the controller aborted
// the in-flight provider call), applies the command (command.applied.v1), and — for a send_message
// interrupt — journals the durable delivered_messages row so the interrupt-delivered turn survives a
// reclaim (spec §26.9, ENG-012). It runs under guardRunActive, so a run canceled during the abort
// rejects the write. Returns the command's applied_sequence. A command already applied by a racing
// boundary returns ErrCommandNotPending.
//
// The durable row closes the ENG-012 outage half the boundary path (ApplyCommand) already closed for
// its own folds: an interrupt-delivered message lived ONLY in the engine subprocess's memory, so a
// crash between the fold and the resumed step's commit dropped it (the command is drained single-winner,
// nothing redelivered it, run.start carries prior responses only). boundaryRequestID is the aborted
// step's model_request_id — the deterministic boundary key a reconstructing attempt redelivers under,
// so interrupt-delivered and boundary-delivered messages at the same step interleave by applied_sequence
// (§26.9). Empty boundaryRequestID (a change_config interrupt has no message) writes no row.
func (s *Store) InterruptModelStep(ctx context.Context, tenant Tenant, sessionID, responseID, runID, commandID, boundaryRequestID, partialEventType string, partialPayload []byte) (int64, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, fmt.Errorf("begin interrupt: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if err := guardRunActive(ctx, tx, tenant, runID); err != nil {
		return 0, err
	}
	if _, err := appendEvent(ctx, tx, tenant, sessionID, responseID, partialEventType, partialPayload); err != nil {
		return 0, err
	}
	// The aborted step burned real provider spend that CommitModelResult will never settle, so record it
	// here in the same transaction (E13 T6). Not tokens — those never arrive on a canceled stream — but
	// the step itself, so an interrupt cannot spend invisibly past a budget (see meterInterruptedStep).
	if err := settleUsage(ctx, tx, tenant, interruptedStepEntry(sessionID, runID, boundaryRequestID)); err != nil {
		return 0, err
	}
	seq, err := applyCommandInTx(ctx, tx, tenant, sessionID, responseID, commandID)
	if err != nil {
		return 0, err
	}
	// The interrupt-delivered message is now durable, keyed by the aborted step's boundary, so a
	// reconstructing attempt refolds it exactly once at that same input boundary (spec §26.9). ON
	// CONFLICT DO NOTHING (InsertDeliveredMessage) keeps a redelivered interrupt idempotent.
	if boundaryRequestID != "" {
		if _, err := tx.Exec(ctx, storage.Query("InsertDeliveredMessage"),
			commandID, tenant.Organization, tenant.Project, runID, nullableText(boundaryRequestID), seq); err != nil {
			return 0, fmt.Errorf("record interrupt delivered message: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit interrupt: %w", err)
	}
	return seq, nil
}

// sweepQueuedCommands expires a run's still-queued commands, closing the §22.4 lifecycle: a
// command accepted mid-run that never reached a delivery boundary (the run was canceled, or it
// was queued on the final step) must not sit queued forever. ApplyRunTransition calls it inside
// the terminal transition's tx, so the run's terminality and its commands' expiry commit
// together. Journals command.expired.v1 per swept command so attached clients see the lifecycle.
//
// send_message and change_config are NOT expired — they carry to the next response (E10 T7 ENG-012 fork
// 3). A surviving send_message that never folded into THIS response gets a warning.raised.v1 so the user
// SEES it will carry rather than silently vanish; the actual carry re-scopes it at the next run.start
// (CarrySessionSendMessages).
func sweepQueuedCommands(ctx context.Context, tx pgx.Tx, tenant Tenant, sessionID string, responseID *string, runID string, terminal statemachines.RunState) error {
	// send_message survives (carries) ONLY on a clean completion; a canceled/failed terminal expires it
	// like the rest (an aborted run has no clean next response to carry into, E10 T7 fork 3).
	expireSendMessages := terminal != statemachines.RunCompleted
	rows, err := tx.Query(ctx, storage.Query("ExpireQueuedCommandsForRun"), runID, tenant.Organization, tenant.Project, expireSendMessages)
	if err != nil {
		return fmt.Errorf("expire queued commands: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("scan expired command: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	resp := ""
	if responseID != nil {
		resp = *responseID
	}
	for _, id := range ids {
		if _, err := appendEvent(ctx, tx, tenant, sessionID, resp, commandExpiredEvent, mustMarshal(map[string]any{"command_id": id})); err != nil {
			return err
		}
	}
	// Only a clean completion leaves send_messages queued to carry — warn each so the user sees it.
	if !expireSendMessages {
		return warnSurvivingSendMessages(ctx, tx, tenant, sessionID, resp, runID)
	}
	return nil
}

// warnSurvivingSendMessages journals warning.raised.v1 for each send_message still queued on a terminal
// run (E10 T7 fork 3): the message did not fold into this response and will carry to the next — the user
// sees it, not a silent drop. The commands stay queued; CarrySessionSendMessages re-scopes them at the
// next run.start so the ordinary boundary pump delivers them there.
func warnSurvivingSendMessages(ctx context.Context, tx pgx.Tx, tenant Tenant, sessionID, responseID, runID string) error {
	rows, err := tx.Query(ctx, storage.Query("SurvivingQueuedSendMessagesForRun"), runID, tenant.Organization, tenant.Project)
	if err != nil {
		return fmt.Errorf("read surviving send messages: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("scan surviving send message: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		payload := mustMarshal(map[string]any{"command_id": id, "code": "message_carried_to_next_response",
			"detail": "the message did not fold into this response before it terminated; it stays queued and carries to the next response's input boundary"})
		if _, err := appendEvent(ctx, tx, tenant, sessionID, responseID, warningRaisedEvent, payload); err != nil {
			return err
		}
	}
	return nil
}

// CarrySessionSendMessages re-scopes a session's still-queued send_message commands to the run starting
// now (E10 T7 ENG-012 fork 3, the cross-run carry): a message queued on a prior terminal run — one that
// never folded into that response — becomes a normal queued command on this run, so this run's ordinary
// boundary pump delivers it at its first input boundary. It is the send_message analogue of the
// change_config carry (PendingSessionConfigCommands) and reuses the entire delivery path — no new frame.
// Run at run.start (never on a restore, which resumes past the boundary). Returns how many carried.
func (s *Store) CarrySessionSendMessages(ctx context.Context, tenant Tenant, sessionID, runID string) (int64, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	tag, err := s.pool.Exec(ctx, storage.Query("CarrySessionSendMessages"), sessionID, tenant.Organization, tenant.Project, runID)
	if err != nil {
		return 0, fmt.Errorf("carry session send messages: %w", err)
	}
	return tag.RowsAffected(), nil
}

// readCommand reads a command's projection within tx.
func readCommand(ctx context.Context, tx pgx.Tx, tenant Tenant, commandID string) (Command, error) {
	var (
		cmd      Command
		delivery *string
		result   []byte
		appliedS *int64
	)
	cmd.ID = commandID
	if err := tx.QueryRow(ctx, storage.Query("GetCommand"), commandID, tenant.Organization, tenant.Project).
		Scan(&cmd.SessionID, &cmd.Kind, &delivery, &cmd.State, &appliedS, &result, &cmd.CreatedAt); err != nil {
		return Command{}, fmt.Errorf("read command: %w", err)
	}
	if delivery != nil {
		cmd.Delivery = *delivery
	}
	cmd.AppliedSequence = appliedS
	cmd.Result = result
	return cmd, nil
}

func mustMarshal(v any) []byte {
	out, _ := json.Marshal(v)
	return out
}
