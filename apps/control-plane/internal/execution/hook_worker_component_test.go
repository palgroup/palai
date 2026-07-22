//go:build component

package execution

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	remotehttp "github.com/palgroup/palai/adapters/tools/http"
	"github.com/palgroup/palai/apps/control-plane/internal/extensions"
	"github.com/palgroup/palai/packages/contracts"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// The E12 Task 8 component proof (spec §28.17, TOL-012): a REAL local HTTP hook worker gives a real before_tool
// deny/allow over the REAL T4 signed transport, driven through dispatchTool against a real spine. It pins the
// critical seam: a policy DENY leaves NO tool-ledger row and delivers a structured deny the model sees; an
// ALLOW lets the tool execute + commit normally.

// hookWorker is a local tool-http.v1 hook worker that denies a named tool and allows the rest.
func hookWorker(t *testing.T, denyTool string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var env struct {
			Arguments map[string]any `json:"arguments"`
		}
		_ = json.NewDecoder(r.Body).Decode(&env)
		name, _ := env.Arguments["tool_name"].(string)
		w.Header().Set("Content-Type", "application/json")
		if name == denyTool {
			_, _ = w.Write([]byte(`{"result":{"decision":"deny","reason":"blocked by the project hook worker"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"result":{"decision":"allow"}}`))
	}))
}

// TestHookWorkerBeforeToolDenyAndAllow drives a real remote before_tool policy hook through dispatchTool: a
// denied tool leaves NO ledger row and the model gets a structured deny; an allowed tool executes + commits.
func TestHookWorkerBeforeToolDenyAndAllow(t *testing.T) {
	ctx := context.Background()
	cs, tenant, sessionID, runID := openLedgerSpine(t)
	pool := cs.Pool()

	worker := hookWorker(t, "push")
	defer worker.Close()
	// A remote before_tool policy hook pointing at the local worker (allow_private for the loopback fixture).
	execSQL(t, pool,
		`INSERT INTO hooks (id, organization_id, project_id, name, hook_point, category, executor, config, secret_ref)
		 VALUES ($1,$2,$3,'guard','before_tool','policy','remote_http', jsonb_build_object('url',$4::text,'allow_private',true), 'sref_hook')`,
		redeliveryID("hook"), tenant.Organization, tenant.Project, worker.URL)

	ext := extensions.New(pool)
	ext.SetRemoteInvoker(
		remotehttp.NewExecutor(remotehttp.NewOperations(pool)),
		func(_, _ string) ([]byte, error) { return []byte("component-hook-secret"), nil },
	)

	var pushRuns, fileRuns int32
	broker := toolbroker.New(
		toolbroker.Tool{Name: "push", InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"}, ReplayClass: toolbroker.ClassPure,
			Invoke: func(map[string]any) (map[string]any, error) {
				atomic.AddInt32(&pushRuns, 1)
				return map[string]any{"pushed": true}, nil
			}},
		toolbroker.Tool{Name: "file", InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"}, ReplayClass: toolbroker.ClassPure,
			Invoke: func(map[string]any) (map[string]any, error) {
				atomic.AddInt32(&fileRuns, 1)
				return map[string]any{"read": true}, nil
			}},
	)

	orch := &Orchestrator{spine: cs, tools: broker, hooks: ext}
	newAttempt := func() (*attemptState, *recordingChannel) {
		ch := &recordingChannel{}
		st := &attemptState{
			attempt:   AttemptDescriptor{RunID: contracts.RunID(runID), AttemptID: contracts.AttemptID(redeliveryID("att")), Fence: 1},
			tenant:    tenant,
			sessionID: sessionID,
			ch:        ch,
		}
		return st, ch
	}

	// DENY: the worker blocks push. The seam delivers a structured deny result, NEVER executes the tool, and
	// leaves NO tool-ledger row (the pre-write is skipped on a deny).
	denyCall := redeliveryID("tc")
	st, ch := newAttempt()
	if err := orch.dispatchTool(ctx, st, toolRequestFrame(denyCall, "push", map[string]any{"branch": "main"})); err != nil {
		t.Fatalf("dispatchTool(push) error = %v", err)
	}
	if atomic.LoadInt32(&pushRuns) != 0 {
		t.Fatal("a denied tool was executed — the deny did not short-circuit before Execute")
	}
	if !deliveredDeny(ch, denyCall) {
		t.Fatalf("no structured deny delivered to the model for %s: %+v", denyCall, ch.sent)
	}
	if rows := ledgerRowCount(t, pool, denyCall); rows != 0 {
		t.Fatalf("a denied call left %d tool-ledger rows, want 0 (no pre-write on a deny)", rows)
	}
	assertEventJournaled(t, cs, sessionID, "policy.denied.v1", 1)

	// ALLOW: the worker permits file. The tool executes and commits a ledger row normally.
	allowCall := redeliveryID("tc")
	st2, _ := newAttempt()
	if err := orch.dispatchTool(ctx, st2, toolRequestFrame(allowCall, "file", map[string]any{"path": "/x"})); err != nil {
		t.Fatalf("dispatchTool(file) error = %v", err)
	}
	if atomic.LoadInt32(&fileRuns) != 1 {
		t.Fatalf("an allowed tool ran %d times, want 1", fileRuns)
	}
	if rows := ledgerRowCount(t, pool, allowCall); rows != 1 {
		t.Fatalf("an allowed call committed %d tool-ledger rows, want 1", rows)
	}
}

// deliveredDeny reports whether the recording channel received a tool.result for callID carrying a structured
// denial (status=denied) the model reads.
func deliveredDeny(ch *recordingChannel, callID string) bool {
	for _, f := range ch.sent {
		if f.Type != "tool.result" {
			continue
		}
		if id, _ := f.Data["tool_call_id"].(string); id != callID {
			continue
		}
		content, _ := f.Data["content"].(string)
		if strings.Contains(content, "\"status\":\"denied\"") || strings.Contains(content, "denied") {
			return true
		}
	}
	return false
}

// ledgerRowCount counts tool_calls rows for a call id (the durable pre-write/commit half). The tool_call_id
// is the row's primary key `id`.
func ledgerRowCount(t *testing.T, pool *pgxpool.Pool, callID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM tool_calls WHERE id = $1`, callID).Scan(&n); err != nil {
		t.Fatalf("count tool_calls: %v", err)
	}
	return n
}
