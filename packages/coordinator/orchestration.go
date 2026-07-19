package coordinator

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	statemachines "github.com/palgroup/palai/packages/state-machines"
	"github.com/palgroup/palai/storage"
)

// RunIDForResponse resolves a response's root run within the tenant scope. found is false
// for an unknown or foreign response, so the caller renders the same 404 as retrieval and
// never leaks a cross-tenant resource's existence (spec §39.2). LP's response:run is 1:1.
func (s *Store) RunIDForResponse(ctx context.Context, tenant Tenant, responseID string) (string, bool, error) {
	var runID string
	err := s.pool.QueryRow(ctx, storage.Query("RunIDForResponse"), responseID, tenant.Organization, tenant.Project).Scan(&runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("resolve run for response %s: %w", responseID, err)
	}
	return runID, true, nil
}

// guardRunActive locks the run and rejects a commit whose run has already reached a terminal
// state, returning ErrRunTerminal. It is the shared precondition of the commit-before-deliver
// primitives below: once a run is terminal (e.g. canceled mid-attempt), no result event may
// be appended after its terminal event, so the "terminal is the journal's end" contract holds
// even when a cancel races an in-flight attempt (spec §22.3 monotonic terminality). The FOR
// UPDATE lock serializes against ApplyRunTransition, so a cancel and a commit cannot both win.
func guardRunActive(ctx context.Context, tx pgx.Tx, tenant Tenant, runID string) error {
	var sessionID, state string
	var responseID *string
	err := tx.QueryRow(ctx, storage.Query("LockRun"), runID, tenant.Organization, tenant.Project).Scan(&sessionID, &responseID, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("run %s not found in tenant scope", runID)
	}
	if err != nil {
		return fmt.Errorf("lock run: %w", err)
	}
	if runTerminalStates[statemachines.RunState(state)] {
		return fmt.Errorf("%w: run %s is %s", ErrRunTerminal, runID, state)
	}
	return nil
}

// RunContext resolves a run's durable execution context for the orchestrator: its
// tenant scope, session, response, and admitted input. The run id comes from the
// claimed durable job, so this by-primary-key read is the same cross-tenant
// infrastructure read the job claim performs; it establishes the scope every later
// orchestrator write is gated by.
func (s *Store) RunContext(ctx context.Context, runID string) (Tenant, string, string, []byte, error) {
	var (
		tenant     Tenant
		sessionID  string
		responseID string
		input      []byte
	)
	err := s.pool.QueryRow(ctx, storage.Query("RunContext"), runID).
		Scan(&tenant.Organization, &tenant.Project, &sessionID, &responseID, &input)
	if err != nil {
		return Tenant{}, "", "", nil, fmt.Errorf("read run context for %s: %w", runID, err)
	}
	return tenant, sessionID, responseID, input, nil
}

// PriorResponse is one earlier response in a session, as run.start history needs it:
// Output is the stored terminal projection (nil once purged or not yet terminal), and
// Purged marks a reaped response whose content is a redacted_content marker (spec §22.2).
type PriorResponse struct {
	Output []byte
	Purged bool
}

// SessionHistory returns a session's responses created before responseID, in creation
// order, so run.start can carry them as conversation history (spec §9, §22.2). It is
// tenant-scoped; a foreign session or response yields no rows.
func (s *Store) SessionHistory(ctx context.Context, tenant Tenant, sessionID, responseID string) ([]PriorResponse, error) {
	rows, err := s.pool.Query(ctx, storage.Query("SessionHistory"), sessionID, tenant.Organization, tenant.Project, responseID)
	if err != nil {
		return nil, fmt.Errorf("read session history: %w", err)
	}
	defer rows.Close()
	var out []PriorResponse
	for rows.Next() {
		var p PriorResponse
		if err := rows.Scan(&p.Output, &p.Purged); err != nil {
			return nil, fmt.Errorf("scan session history: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// appendEvent journals one event under a freshly allocated, gap-free session sequence
// inside tx. It is the shared body of the commit-before-deliver primitives below.
// responseID keys the event to its owning response so the retention purge is per-response
// (spec §22.2); an empty string journals a session-scoped event with a NULL response_id.
func appendEvent(ctx context.Context, tx pgx.Tx, tenant Tenant, sessionID, responseID, eventType string, payload []byte) (int64, error) {
	var seq int64
	if err := tx.QueryRow(ctx, storage.Query("AllocateSequence"), sessionID).Scan(&seq); err != nil {
		return 0, fmt.Errorf("allocate session sequence: %w", err)
	}
	eventID, err := newEventID()
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, storage.Query("AppendEvent"),
		eventID, tenant.Organization, tenant.Project, sessionID, nullableText(responseID), seq, eventType, payload); err != nil {
		return 0, fmt.Errorf("append event: %w", err)
	}
	return seq, nil
}

// CommitModelRequest records a model request and its journal event before the provider
// is called, so the request has a durable trace and a reclaimed attempt can dedup
// against a committed result (spec §24.7 order, §53.4). The row is idempotent; the
// event is journaled only on the fresh insert, so a re-derived request adds nothing.
func (s *Store) CommitModelRequest(ctx context.Context, tenant Tenant, sessionID, responseID, runID, requestID, eventType string, payload []byte) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin model request: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if err := guardRunActive(ctx, tx, tenant, runID); err != nil {
		return err
	}
	err = tx.QueryRow(ctx, storage.Query("InsertModelRequest"),
		requestID, tenant.Organization, tenant.Project, runID).Scan(new(string))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // already recorded by an earlier attempt; nothing new to journal
	}
	if err != nil {
		return fmt.Errorf("insert model request: %w", err)
	}
	if _, err := appendEvent(ctx, tx, tenant, sessionID, responseID, eventType, payload); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit model request: %w", err)
	}
	return nil
}

// LookupModelResult returns a committed model result for replay, if one exists. A
// reclaimed attempt re-derives the same stable model_request_id and finds the result
// here, so the provider is never dispatched twice for one logical request (spec §53.4).
func (s *Store) LookupModelResult(ctx context.Context, tenant Tenant, requestID string) ([]byte, bool, error) {
	var state string
	var result []byte
	err := s.pool.QueryRow(ctx, storage.Query("GetModelResult"), requestID, tenant.Organization, tenant.Project).
		Scan(&state, &result)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read model result: %w", err)
	}
	if state != "completed" {
		return nil, false, nil
	}
	return result, true, nil
}

// CommitModelResult completes the model request row with its result and journals the
// result event in one transaction. The orchestrator calls it before delivering
// model.result to the engine, so no provider result reaches the engine until its state
// is durable (spec §24.7).
func (s *Store) CommitModelResult(ctx context.Context, tenant Tenant, sessionID, responseID, runID, requestID string, result []byte, eventType string, payload []byte) (int64, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, fmt.Errorf("begin model commit: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if err := guardRunActive(ctx, tx, tenant, runID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, storage.Query("CompleteModelRequest"),
		requestID, tenant.Organization, tenant.Project, result); err != nil {
		return 0, fmt.Errorf("complete model request: %w", err)
	}
	seq, err := appendEvent(ctx, tx, tenant, sessionID, responseID, eventType, payload)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit model result: %w", err)
	}
	return seq, nil
}

// CommitToolResult persists a completed tool_call row and its journal event in one
// transaction. The orchestrator calls it before delivering tool.result to the engine
// (commit-before-deliver). The tool_call row is idempotent (UpsertToolCall keeps the
// authoritative completed row); duplicate frames are already deduped by the caller's
// frame ledger, so this runs once per tool call.
func (s *Store) CommitToolResult(ctx context.Context, tenant Tenant, sessionID, responseID, runID string, fence uint64, callID, name string, arguments, result []byte, eventType string, payload []byte) (int64, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, fmt.Errorf("begin tool commit: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if err := guardRunActive(ctx, tx, tenant, runID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, storage.Query("UpsertToolCall"),
		callID, tenant.Organization, tenant.Project, runID, int64(fence), name, arguments, result); err != nil {
		return 0, fmt.Errorf("upsert tool call: %w", err)
	}
	seq, err := appendEvent(ctx, tx, tenant, sessionID, responseID, eventType, payload)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit tool result: %w", err)
	}
	return seq, nil
}

// FinalizeResponse writes the terminal Response projection built from committed run,
// output, and usage. It is the last durable write of a run, so a restart reads the
// same terminal status and body (spec §24.7, LP-008).
func (s *Store) FinalizeResponse(ctx context.Context, tenant Tenant, responseID, state string, projection []byte) error {
	if _, err := s.pool.Exec(ctx, storage.Query("UpdateResponse"),
		responseID, tenant.Organization, tenant.Project, state, projection); err != nil {
		return fmt.Errorf("finalize response %s: %w", responseID, err)
	}
	return nil
}
