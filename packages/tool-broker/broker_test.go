package toolbroker

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/palgroup/palai/packages/contracts"
	statemachines "github.com/palgroup/palai/packages/state-machines"
)

// openObject is the permissive schema every test tool uses (any object passes).
var openObject = map[string]any{"type": "object"}

// TestDuplicateToolCallIdSingleExecution proves TOL-016's ledger half against a REAL local HTTP
// destination with a request counter (spec §26.7): a duplicate/retry carrying the SAME tool_call_id runs
// the external effect exactly ONCE — the completed row is authoritative and the replay is served cached,
// never re-hitting the server. A DIFFERENT tool_call_id is a distinct operation and does hit it. The
// counter is the external-effect witness the in-memory Cached flag alone cannot prove.
func TestDuplicateToolCallIdSingleExecution(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// A side-effecting tool: every real execution POSTs to the destination (the forced external effect).
	poster := Tool{
		Name: "http.post", InputSchema: openObject, OutputSchema: openObject, ReplayClass: ClassIdempotent,
		Invoke: func(map[string]any) (map[string]any, error) {
			resp, err := http.Post(server.URL, "application/json", http.NoBody)
			if err != nil {
				return nil, err
			}
			_ = resp.Body.Close()
			return map[string]any{"status": float64(resp.StatusCode)}, nil
		},
	}
	broker := New(poster)
	ctx := context.Background()

	// First execution of tool_call_id "tc_1": the external effect fires once.
	first, err := broker.Execute(ctx, contracts.ToolCallID("tc_1"), "http.post", map[string]any{}, 1, ExecEnv{})
	if err != nil {
		t.Fatalf("first Execute error = %v", err)
	}
	if first.Cached {
		t.Fatal("a first execution must not be cached")
	}
	if first.ReplayClass != ClassIdempotent {
		t.Fatalf("first outcome replay class = %q, want idempotent (declared at registration, copied to the ledger)", first.ReplayClass)
	}

	// The SAME tool_call_id redelivered (a retry / reclaim) replays the completed row cached — no second POST.
	second, err := broker.Execute(ctx, contracts.ToolCallID("tc_1"), "http.post", map[string]any{}, 2, ExecEnv{})
	if err != nil {
		t.Fatalf("duplicate Execute error = %v", err)
	}
	if !second.Cached {
		t.Fatal("a duplicate tool_call_id must replay cached, not re-execute")
	}
	if second.ReplayClass != ClassIdempotent {
		t.Fatalf("cached outcome replay class = %q, want idempotent (label survives the replay)", second.ReplayClass)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("external effect fired %d times for one tool_call_id, want exactly 1 (TOL-016)", got)
	}

	// A DIFFERENT tool_call_id is a genuinely new operation: it hits the destination.
	if _, err := broker.Execute(ctx, contracts.ToolCallID("tc_2"), "http.post", map[string]any{}, 3, ExecEnv{}); err != nil {
		t.Fatalf("distinct Execute error = %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("external effect fired %d times for two distinct tool_call_ids, want 2", got)
	}
}

// TestPureToolReplayLabeledNoDuplication proves TOL-001 (spec §26.7): a pure tool may re-run freely, but a
// redelivered tool_call_id replays the completed row cached and LABELS the outcome as a replay
// (Cached=true) carrying the declared pure class — semantically ONE result, never a second effect.
func TestPureToolReplayLabeledNoDuplication(t *testing.T) {
	var runs int32
	pure := Tool{
		Name: "double", InputSchema: openObject, OutputSchema: openObject, // ReplayClass unset -> defaults pure
		Invoke: func(args map[string]any) (map[string]any, error) {
			atomic.AddInt32(&runs, 1)
			n, _ := args["n"].(float64)
			return map[string]any{"out": n * 2}, nil
		},
	}
	broker := New(pure)
	ctx := context.Background()

	first, err := broker.Execute(ctx, contracts.ToolCallID("tc_pure"), "double", map[string]any{"n": float64(21)}, 1, ExecEnv{})
	if err != nil {
		t.Fatalf("first Execute error = %v", err)
	}
	if first.Cached || first.State != statemachines.ToolCallCompleted {
		t.Fatalf("first outcome = {cached:%v state:%v}, want {false completed}", first.Cached, first.State)
	}
	if first.ReplayClass != ClassPure {
		t.Fatalf("unset replay class = %q, want the pure default", first.ReplayClass)
	}

	replay, err := broker.Execute(ctx, contracts.ToolCallID("tc_pure"), "double", map[string]any{"n": float64(21)}, 2, ExecEnv{})
	if err != nil {
		t.Fatalf("replay Execute error = %v", err)
	}
	if !replay.Cached {
		t.Fatal("a redelivered pure tool_call_id must be labelled a replay (Cached=true)")
	}
	if replay.Result["out"] != first.Result["out"] {
		t.Fatalf("replay result = %v, want the same single result %v", replay.Result["out"], first.Result["out"])
	}
	if got := atomic.LoadInt32(&runs); got != 1 {
		t.Fatalf("pure tool ran %d times, want 1 (the replay is served cached, semantically single)", got)
	}
}
