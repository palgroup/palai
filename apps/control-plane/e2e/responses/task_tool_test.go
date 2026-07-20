//go:build e2e

package responses

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// taskWritingProvider drives one durable task write: the first step calls the palai.task tool, then
// the next step (seeing the tool result) finishes.
type taskWritingProvider struct{ key, title string }

func (p taskWritingProvider) Execute(_ context.Context, req modelbroker.Request, _ string, _ func(modelbroker.Delta)) (modelbroker.Result, error) {
	res := modelbroker.Result{
		ModelRequestID: req.ModelRequestID, Model: "fake",
		Usage: contracts.Usage{InputTokens: 5, OutputTokens: 3, TotalTokens: 8}, Attempts: 1,
	}
	for _, m := range req.Messages {
		if m.Role == "tool" {
			res.ProviderRequestID = "prov_final"
			res.Output = "recorded"
			res.FinishReason = "stop"
			return res, nil
		}
	}
	res.ProviderRequestID = "prov_task"
	res.ToolCalls = []modelbroker.ToolCall{{
		ID: "call_task", Name: "palai.task",
		Arguments: fmt.Sprintf(`{"key":%q,"title":%q,"status":"open"}`, p.key, p.title),
	}}
	res.FinishReason = "tool_calls"
	return res, nil
}

// TestDurableTaskToolWritesThroughModelCall proves the durable registry end to end through the REAL
// model tool surface (REG-001/002 integration): the model calls the palai.task tool, the engine
// dispatches it, the fenced tool-call runs the registry, and the durable row + its ordered
// task.created.v1 journal event land — the same path a live coding run would take, minus the provider.
func TestDurableTaskToolWritesThroughModelCall(t *testing.T) {
	h := newHarness(t)
	stop := h.runWorker(h.newOrchestratorWithTools(subprocessDialer{engineDir: h.engineDir},
		taskWritingProvider{key: "impl-x", title: "implement X"}, tools.TaskTool()))
	defer stop()

	respID, sessionID, _ := h.admitWith(`{"input":"track this"}`, newID("idem"))
	h.awaitResponseState(respID, "completed", 90*time.Second)

	// The durable task was written by the model's tool call, session-scoped.
	var kind, title, status string
	if err := h.spine.Pool().QueryRow(context.Background(),
		`SELECT kind, title, status FROM tasks WHERE session_id=$1 AND task_key='impl-x' AND organization_id=$2 AND project_id=$3`,
		sessionID, h.tenant.Organization, h.tenant.Project).Scan(&kind, &title, &status); err != nil {
		t.Fatalf("read durable task: %v", err)
	}
	if kind != "task" || title != "implement X" || status != "open" {
		t.Fatalf("durable task = kind:%s title:%q status:%s, want task / implement X / open", kind, title, status)
	}
	// The ordered journal carries task.created.v1 on the response, so attached clients see the update.
	if n := h.count(`SELECT count(*) FROM events WHERE response_id=$1 AND type='task.created.v1'`, respID); n != 1 {
		t.Fatalf("task.created.v1 on the response journal = %d, want 1", n)
	}
}
