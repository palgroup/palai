package coordinator

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/palgroup/palai/storage"
)

// AppendToolProgress journals an advisory tool_call.progress.v1 event (E12 T5, MCP progress). Progress is
// ADVISORY — it is NOT a ToolCallTable transition (the model_step.delta class), so this appends the event
// WITHOUT touching the tool-call state machine, fence, or ledger row. It is best-effort: the MCP manager's
// progress sink calls it and ignores its error, so a progress append that fails never stalls or fails the
// tools/call itself. The payload carries the tool_call_id it correlates to plus the progress numbers.
// maxProgressMessage caps the advisory message stored per progress event, so a hostile server cannot bloat
// the events journal with a giant message per notification (the per-call COUNT is capped in the MCP manager).
const maxProgressMessage = 4 * 1024

// AppendModelStep journals ONE model_step.{created,completed}.v1 event for an MCP sampling step (E12 T6,
// TOL-010). Sampling is a SEPARATE brokered model step that runs DURING an active tools/call; it REUSES the
// existing model_step event kinds with source:"mcp_sampling" in the payload — no new event kind (§61). Unlike
// CommitModelRequest/CommitModelResult it touches NO model_requests row and does NOT fold delivered messages
// (those belong to the engine's OWN model steps, not a nested sampling call) — it is a pure durable event
// append, run-active-guarded so a sampling step on a dead run journals nothing.
func (s *Store) AppendModelStep(ctx context.Context, tenant Tenant, sessionID, responseID, runID, eventType string, payload []byte) error {
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin model step: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := guardRunActive(ctx, tx, tenant, runID); err != nil {
		return err
	}
	if _, err := appendEvent(ctx, tx, tenant, sessionID, responseID, eventType, payload); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit model step: %w", err)
	}
	return nil
}

// maxDeltaText caps one journalled delta's text. The dispatch-side sink coalesces a provider's token
// stream into windows before it ever reaches here, so this is the second bound rather than the first:
// it stops a single pathological window from bloating the journal, the way maxProgressMessage stops a
// hostile MCP server doing it per notification.
const maxDeltaText = 16 * 1024

// AppendModelStepDelta journals ONE advisory model_step.delta.v1 event: the text a provider streamed
// during an OPEN model step, coalesced into a window by the caller.
//
// WHY THIS EXISTS AT ALL. model_step.delta.v1 has been in the canonical registry and in the AsyncAPI
// x-event-types list since the event catalogue was written, and until now NOTHING IN PRODUCTION WROTE
// IT — the dispatch loop's onDelta accumulated into a local strings.Builder so an interrupt could
// record a partial (spec §25.16) and did nothing else. A client therefore saw model_step.created.v1
// and then model_step.completed.v1 with the whole answer arriving at once. The declared event existed;
// the writer did not.
//
// IT IS ADVISORY, exactly like AppendToolProgress and for the same reason: a delta advances no state
// machine, touches no model_requests row, and folds no delivered message. Callers treat a failure as
// nothing — a delta that cannot be journalled must never fail the model step whose text it describes.
//
// It is run-active-guarded like AppendModelStep: a delta belonging to a run that is already gone
// journals nothing rather than appending to a dead run's stream.
func (s *Store) AppendModelStepDelta(ctx context.Context, tenant Tenant, sessionID, responseID, runID, requestID, text string) error {
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	if sessionID == "" {
		return fmt.Errorf("model step delta needs a session id")
	}
	if text == "" {
		return nil // an empty window is not an event
	}
	if len(text) > maxDeltaText {
		text = text[:maxDeltaText]
	}
	payload := mustMarshal(map[string]any{
		"run_id":           runID,
		"model_request_id": requestID,
		"text":             text,
	})
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin model step delta: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := guardRunActive(ctx, tx, tenant, runID); err != nil {
		return err
	}
	if _, err := appendEvent(ctx, tx, tenant, sessionID, responseID, "model_step.delta.v1", payload); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit model step delta: %w", err)
	}
	return nil
}

func (s *Store) AppendToolProgress(ctx context.Context, tenant Tenant, sessionID, responseID, callID string, progress, total float64, message string) error {
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	if sessionID == "" {
		return fmt.Errorf("tool progress needs a session id")
	}
	if len(message) > maxProgressMessage {
		message = message[:maxProgressMessage]
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
