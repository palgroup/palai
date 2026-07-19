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
	Kind      string // send_message | approve | deny
	Delivery  string // queue | steer | interrupt (send_message only)
	Payload   []byte
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

// PendingCommand is one queued send_message command the pump delivers at a boundary.
type PendingCommand struct {
	ID       string
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
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Command{}, fmt.Errorf("begin accept command: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	// The session must be visible in scope; a foreign/unknown id is a 404, never an FK error.
	if err := tx.QueryRow(ctx, storage.Query("GetSessionInScope"), sessionID, tenant.Organization, tenant.Project).
		Scan(new(string), new(string), new(time.Time)); errors.Is(err, pgx.ErrNoRows) {
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

	// Synchronous resolution. approve/deny have no pending-approval source (E09 deferral);
	// a send_message with no live run has no loop to steer. Both are typed rejections. A
	// send_message on a live run stays queued for the pump.
	if reason := rejectionReason(in.Kind, runID); reason != nil {
		if err := rejectCommandTx(ctx, tx, tenant, sessionID, responseID, in.CommandID, reason); err != nil {
			return Command{}, err
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
// it queued. approve/deny are unsupported until E09; a send_message needs a live root run.
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
	default:
		return map[string]any{"code": "unsupported_command", "detail": "the command kind is not supported"}
	}
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

// PendingSendMessageCommands returns a run's queued send_message commands in creation order
// — the pump's boundary read. A command that has left 'queued' never reappears, which is the
// deliver-once guarantee (spec §9.2).
func (s *Store) PendingSendMessageCommands(ctx context.Context, tenant Tenant, runID string) ([]PendingCommand, error) {
	rows, err := s.pool.Query(ctx, storage.Query("PendingSendMessageCommands"), runID, tenant.Organization, tenant.Project)
	if err != nil {
		return nil, fmt.Errorf("read pending commands: %w", err)
	}
	defer rows.Close()
	var out []PendingCommand
	for rows.Next() {
		var c PendingCommand
		var delivery *string
		if err := rows.Scan(&c.ID, &delivery, &c.Payload); err != nil {
			return nil, fmt.Errorf("scan pending command: %w", err)
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
func (s *Store) ApplyCommand(ctx context.Context, tenant Tenant, sessionID, responseID, runID, commandID string) (int64, error) {
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
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit apply command: %w", err)
	}
	return seq, nil
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

// PendingInterruptCommand returns the oldest queued interrupt for a run and its message, for
// the in-flight-abort watcher (spec §9.2, §25.11). found is false when none is pending.
func (s *Store) PendingInterruptCommand(ctx context.Context, tenant Tenant, runID string) (commandID, message string, found bool, err error) {
	var payload []byte
	switch err := s.pool.QueryRow(ctx, storage.Query("PendingInterruptCommand"), runID, tenant.Organization, tenant.Project).
		Scan(&commandID, &payload); {
	case errors.Is(err, pgx.ErrNoRows):
		return "", "", false, nil
	case err != nil:
		return "", "", false, fmt.Errorf("read pending interrupt: %w", err)
	}
	var body struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(payload, &body)
	return commandID, body.Message, true, nil
}

// InterruptModelStep records an aborted model step and applies the interrupt command in one
// transaction (spec §9.2, §25.11): it journals the partial step event (the controller aborted
// the in-flight provider call), then applies the command (command.applied.v1). It runs under
// guardRunActive, so a run canceled during the abort rejects the write. Returns the command's
// applied_sequence. A command already applied by a racing boundary returns ErrCommandNotPending.
func (s *Store) InterruptModelStep(ctx context.Context, tenant Tenant, sessionID, responseID, runID, commandID, partialEventType string, partialPayload []byte) (int64, error) {
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
	seq, err := applyCommandInTx(ctx, tx, tenant, sessionID, responseID, commandID)
	if err != nil {
		return 0, err
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
func sweepQueuedCommands(ctx context.Context, tx pgx.Tx, tenant Tenant, sessionID string, responseID *string, runID string) error {
	rows, err := tx.Query(ctx, storage.Query("ExpireQueuedCommandsForRun"), runID, tenant.Organization, tenant.Project)
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
	return nil
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
