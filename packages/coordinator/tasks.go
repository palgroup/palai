package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/palgroup/palai/storage"
)

// Task registry event types (spec §11, master plan line 410). Kept in sync with the canonical
// registry protocols/schemas/execution/event-types.json and the emitter list in
// apps/control-plane/internal/execution/events.go.
const (
	eventTaskCreated = "task.created.v1"
	eventTaskUpdated = "task.updated.v1"
)

// Task is one durable, session-scoped task/todo the model manages through the registry tools (spec
// §11). Key is the model's stable handle within the session; Detail is arbitrary model metadata.
type Task struct {
	Key    string          `json:"key"`
	Kind   string          `json:"kind"`
	Title  string          `json:"title"`
	Status string          `json:"status"`
	Detail json.RawMessage `json:"detail,omitempty"`
}

// TaskUpsert is one durable task/todo mutation (spec §11). Key + SessionID identify the row. The
// Set* flags mark which fields the caller actually provided, so a partial update (a status-only
// change, say) keeps every unset field's stored value instead of clobbering it.
type TaskUpsert struct {
	SessionID  string
	RunID      string // the active run, for the run-active guard
	ResponseID string // the response the ordered task event is journaled on
	Key        string
	NewRowID   string // app-minted row id, used only on first insert
	Kind       string
	Title      string
	Status     string
	Detail     json.RawMessage
	SetKind    bool
	SetTitle   bool
	SetStatus  bool
	SetDetail  bool
}

// rowQuerier is the read surface shared by *pgxpool.Pool and pgx.Tx, so the task list read serves
// both a plain read (ListTasks) and the read-after-write inside UpsertTask's transaction.
type rowQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// UpsertTask creates or updates a durable task/todo and journals its change on the session's ordered
// event stream, all in one transaction so the row and its journal event are atomic (REG-002). It
// read-modify-writes: a field the caller did not set keeps its stored value, so a status-only update
// does not erase the title. It returns the session's full task list after the change, so the model
// reads the current durable state (REG-001). It guards the run active — a canceled run's stale
// attempt records nothing, the same discipline as every other commit-before-deliver write.
func (s *Store) UpsertTask(ctx context.Context, tenant Tenant, in TaskUpsert) ([]Task, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("begin task upsert: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if err := guardRunActive(ctx, tx, tenant, in.RunID); err != nil {
		return nil, err
	}

	// Read the existing row so an unset field keeps its stored value.
	kind, title, status, detail := "task", "", "open", json.RawMessage("{}")
	var existingID string
	err = tx.QueryRow(ctx, storage.Query("GetTaskByKey"), in.SessionID, in.Key, tenant.Organization, tenant.Project).
		Scan(&existingID, &kind, &title, &status, &detail)
	exists := true
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		exists = false
		kind, title, status, detail = "task", "", "open", json.RawMessage("{}")
	case err != nil:
		return nil, fmt.Errorf("read task %q: %w", in.Key, err)
	}

	if in.SetKind && in.Kind != "" {
		kind = in.Kind
	}
	if in.SetTitle {
		title = in.Title
	}
	if in.SetStatus && in.Status != "" {
		status = in.Status
	}
	if in.SetDetail && len(in.Detail) > 0 {
		detail = in.Detail
	}

	eventType := eventTaskUpdated
	if exists {
		if _, err := tx.Exec(ctx, storage.Query("UpdateTaskByKey"), in.SessionID, in.Key, kind, title, status, detail); err != nil {
			return nil, fmt.Errorf("update task %q: %w", in.Key, err)
		}
	} else {
		eventType = eventTaskCreated
		if _, err := tx.Exec(ctx, storage.Query("InsertTask"),
			in.NewRowID, tenant.Organization, tenant.Project, in.SessionID, in.Key, kind, title, status, detail); err != nil {
			return nil, fmt.Errorf("insert task %q: %w", in.Key, err)
		}
	}

	payload, _ := json.Marshal(map[string]any{"key": in.Key, "kind": kind, "title": title, "status": status})
	if _, err := appendEvent(ctx, tx, tenant, in.SessionID, in.ResponseID, eventType, payload); err != nil {
		return nil, err
	}

	tasks, err := listTasks(ctx, tx, tenant, in.SessionID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit task upsert: %w", err)
	}
	return tasks, nil
}

// ListTasks returns a session's durable task/todo list in creation order. Tenant-scoped: a foreign
// session yields no rows (no cross-tenant existence disclosure). This is the context-recovery read —
// a fresh attempt reads exactly what is done and what is not (REG-001).
func (s *Store) ListTasks(ctx context.Context, tenant Tenant, sessionID string) ([]Task, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	return listTasks(ctx, s.pool, tenant, sessionID)
}

func listTasks(ctx context.Context, q rowQuerier, tenant Tenant, sessionID string) ([]Task, error) {
	rows, err := q.Query(ctx, storage.Query("ListTasksBySession"), sessionID, tenant.Organization, tenant.Project)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()
	tasks := []Task{}
	for rows.Next() {
		var t Task
		var detail []byte
		if err := rows.Scan(&t.Key, &t.Kind, &t.Title, &t.Status, &detail); err != nil {
			return nil, fmt.Errorf("scan task: %w", err)
		}
		t.Detail = json.RawMessage(detail)
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tasks: %w", err)
	}
	return tasks, nil
}
