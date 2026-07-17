package coordinator

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/palgroup/palai/storage"
)

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

// appendEvent journals one event under a freshly allocated, gap-free session sequence
// inside tx. It is the shared body of the commit-before-deliver primitives below.
func appendEvent(ctx context.Context, tx pgx.Tx, tenant Tenant, sessionID, eventType string, payload []byte) (int64, error) {
	var seq int64
	if err := tx.QueryRow(ctx, storage.Query("AllocateSequence"), sessionID).Scan(&seq); err != nil {
		return 0, fmt.Errorf("allocate session sequence: %w", err)
	}
	eventID, err := newEventID()
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, storage.Query("AppendEvent"),
		eventID, tenant.Organization, tenant.Project, sessionID, seq, eventType, payload); err != nil {
		return 0, fmt.Errorf("append event: %w", err)
	}
	return seq, nil
}

// CommitModelResult persists a model result's journal event in one transaction. The
// orchestrator calls it before delivering model.result to the engine, so no provider
// result reaches the engine until its state is durable (spec §24.7).
func (s *Store) CommitModelResult(ctx context.Context, tenant Tenant, sessionID, eventType string, payload []byte) (int64, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, fmt.Errorf("begin model commit: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	seq, err := appendEvent(ctx, tx, tenant, sessionID, eventType, payload)
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
func (s *Store) CommitToolResult(ctx context.Context, tenant Tenant, sessionID, runID string, fence uint64, callID, name string, arguments, result []byte, eventType string, payload []byte) (int64, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, fmt.Errorf("begin tool commit: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if _, err := tx.Exec(ctx, storage.Query("UpsertToolCall"),
		callID, tenant.Organization, tenant.Project, runID, int64(fence), name, arguments, result); err != nil {
		return 0, fmt.Errorf("upsert tool call: %w", err)
	}
	seq, err := appendEvent(ctx, tx, tenant, sessionID, eventType, payload)
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
