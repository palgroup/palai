package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/palgroup/palai/packages/coordinator"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// taskRegistry adapts the durable-spine store to the broker's TaskRegistry seam, so the model-facing
// task/todo tools persist through the same DB the control plane owns (spec §11, master plan line 410).
// The engine never sees the DB — the tool runs in the control plane behind the fenced tool-call row,
// like every other tool.
type taskRegistry struct {
	store *coordinator.Store
}

func newTaskRegistry(store *coordinator.Store) *taskRegistry { return &taskRegistry{store: store} }

// ApplyTask interprets one registry operation and returns the CURRENT durable task list as the tool
// result, so the model always reads back the full state (REG-001). action "list" reads; anything else
// upserts the task keyed by "key" within the session — idempotent, so a redelivered tool call is a
// no-op change. The tool injects "kind" (task vs todo); the model provides key/title/status/detail.
func (r *taskRegistry) ApplyTask(ctx context.Context, scope toolbroker.TaskScope, op map[string]any) (map[string]any, error) {
	tenant := coordinator.Tenant{Organization: scope.Org, Project: scope.Project}

	if action, _ := op["action"].(string); action == "list" {
		tasks, err := r.store.ListTasks(ctx, tenant, scope.SessionID)
		if err != nil {
			return nil, err
		}
		return tasksResult(tasks), nil
	}

	key, _ := op["key"].(string)
	if strings.TrimSpace(key) == "" {
		return nil, fmt.Errorf("task tool: a non-empty key identifies the task to add or update")
	}
	up := coordinator.TaskUpsert{
		SessionID: scope.SessionID, RunID: scope.RunID, ResponseID: scope.ResponseID,
		Key: key, NewRowID: newExecID("task"),
	}
	if kind, ok := op["kind"].(string); ok {
		up.Kind, up.SetKind = kind, true
	}
	if title, ok := op["title"].(string); ok {
		up.Title, up.SetTitle = title, true
	}
	if status, ok := op["status"].(string); ok {
		up.Status, up.SetStatus = status, true
	}
	if detail, ok := op["detail"]; ok {
		if raw, err := json.Marshal(detail); err == nil {
			up.Detail, up.SetDetail = raw, true
		}
	}
	tasks, err := r.store.UpsertTask(ctx, tenant, up)
	if err != nil {
		return nil, err
	}
	return tasksResult(tasks), nil
}

// tasksResult renders the task list as the tool's structured output. json round-trips each Task so
// the model sees key/kind/title/status/detail exactly as stored.
func tasksResult(tasks []coordinator.Task) map[string]any {
	out := make([]any, 0, len(tasks))
	for _, t := range tasks {
		raw, _ := json.Marshal(t)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		out = append(out, m)
	}
	return map[string]any{"tasks": out}
}
