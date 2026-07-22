package coordinator

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// AppendToolProgress journals an advisory tool_call.progress.v1 event (E12 T5, MCP progress). Progress is
// ADVISORY — it is NOT a ToolCallTable transition (the model_step.delta class), so this appends the event
// WITHOUT touching the tool-call state machine, fence, or ledger row. It is best-effort: the MCP manager's
// progress sink calls it and ignores its error, so a progress append that fails never stalls or fails the
// tools/call itself. The payload carries the tool_call_id it correlates to plus the progress numbers.
func (s *Store) AppendToolProgress(ctx context.Context, tenant Tenant, sessionID, responseID, callID string, progress, total float64, message string) error {
	if sessionID == "" {
		return fmt.Errorf("tool progress needs a session id")
	}
	payload := mustMarshal(map[string]any{
		"tool_call_id": callID,
		"progress":     progress,
		"total":        total,
		"message":      message,
	})
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin tool progress: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := appendEvent(ctx, tx, tenant, sessionID, responseID, "tool_call.progress.v1", payload); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tool progress: %w", err)
	}
	return nil
}
