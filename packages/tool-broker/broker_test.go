package toolbroker

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
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

// TestIdempotentToolSameKeySingleExternalObject proves TOL-002 (spec §26.6-26.7): an idempotent tool
// retried under a DIFFERENT tool_call_id (the model re-proposed the same operation after a kill) but the
// SAME stable external idempotency key settles ONE object at a FAITHFUL destination double that dedups by
// that key — the broker's per-call_id cache cannot help across distinct ids, the destination key does. A
// genuinely different key is a different object.
func TestIdempotentToolSameKeySingleExternalObject(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]bool{}
	var objects int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		mu.Lock()
		if !seen[key] { // a faithful destination: one object per idempotency key, retries fold in
			seen[key] = true
			atomic.AddInt32(&objects, 1)
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	put := Tool{
		Name: "http.put", InputSchema: openObject, OutputSchema: openObject, ReplayClass: ClassIdempotent,
		Invoke: func(args map[string]any) (map[string]any, error) {
			key, _ := args["key"].(string)
			req, _ := http.NewRequest(http.MethodPut, server.URL, http.NoBody)
			req.Header.Set("Idempotency-Key", key) // the STABLE destination key (spec §35.3), not the tool_call_id
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return nil, err
			}
			_ = resp.Body.Close()
			return map[string]any{"ok": true}, nil
		},
	}
	broker := New(put)
	ctx := context.Background()

	// Two DISTINCT tool_call_ids (a retry after a kill re-proposed the op) carrying the SAME key.
	if _, err := broker.Execute(ctx, contracts.ToolCallID("tc_a"), "http.put", map[string]any{"key": "obj-1"}, 1, ExecEnv{}); err != nil {
		t.Fatalf("first Execute error = %v", err)
	}
	if _, err := broker.Execute(ctx, contracts.ToolCallID("tc_b"), "http.put", map[string]any{"key": "obj-1"}, 2, ExecEnv{}); err != nil {
		t.Fatalf("resend Execute error = %v", err)
	}
	if got := atomic.LoadInt32(&objects); got != 1 {
		t.Fatalf("destination holds %d objects for one idempotency key across two tool_call_ids, want 1 (TOL-002)", got)
	}
	// A different key is a genuinely different object.
	if _, err := broker.Execute(ctx, contracts.ToolCallID("tc_c"), "http.put", map[string]any{"key": "obj-2"}, 3, ExecEnv{}); err != nil {
		t.Fatalf("distinct-key Execute error = %v", err)
	}
	if got := atomic.LoadInt32(&objects); got != 2 {
		t.Fatalf("destination holds %d objects for two distinct keys, want 2", got)
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

// TestExecReceivesCallIDAndFence proves the broker hands a workspace/registry tool the per-call identity
// its Exec surface needs (E12 T4): the remote_http binder keys the invoke Idempotency-Key on the
// tool_call_id and stamps the operation row with the live attempt fence, so both must reach Exec through
// ExecEnv. Execute sets them per-call on a copy, so a caller's shared ExecEnv template is never mutated.
func TestExecReceivesCallIDAndFence(t *testing.T) {
	var gotCall contracts.ToolCallID
	var gotFence uint64
	tool := Tool{
		Name: "remote.echo", InputSchema: openObject, OutputSchema: openObject, ReplayClass: ClassIdempotent,
		Exec: func(_ context.Context, env ExecEnv, args map[string]any) (map[string]any, error) {
			gotCall, gotFence = env.CallID, env.Fence
			return args, nil
		},
	}
	broker := New(tool)
	// A template ExecEnv the caller reuses across calls — Execute must not mutate it.
	template := ExecEnv{}
	if _, err := broker.Execute(context.Background(), contracts.ToolCallID("tc_remote_1"), "remote.echo", map[string]any{}, 7, template); err != nil {
		t.Fatalf("execute error = %v", err)
	}
	if gotCall != contracts.ToolCallID("tc_remote_1") {
		t.Fatalf("Exec saw CallID %q, want tc_remote_1", gotCall)
	}
	if gotFence != 7 {
		t.Fatalf("Exec saw Fence %d, want 7", gotFence)
	}
	if template.CallID != "" || template.Fence != 0 {
		t.Fatalf("Execute mutated the caller's ExecEnv template (CallID=%q Fence=%d)", template.CallID, template.Fence)
	}
}

// TestRegistryLookupFallback proves the E12 SetLookup fallback: a tool absent from the static conformance
// set is resolved by the injected per-tenant lookup and then runs through the SAME fence/ledger/replay
// machinery — a completed row replays cached. A lookup miss (or no lookup) is ErrUnknownTool, and the
// resolved tool never enters the static set (Discoverable stays false, tenant isolation).
func TestRegistryLookupFallback(t *testing.T) {
	echo := Tool{
		Name: "fetch", InputSchema: openObject,
		Invoke: func(args map[string]any) (map[string]any, error) { return args, nil },
	}
	var calls int32
	broker := New() // empty static set
	broker.SetLookup(func(_ context.Context, _ ExecEnv, name string) (Tool, bool, error) {
		atomic.AddInt32(&calls, 1)
		if name == "fetch" {
			return echo, true, nil
		}
		return Tool{}, false, nil
	})
	ctx := context.Background()

	// A static-set miss resolves through the lookup and executes.
	out, err := broker.Execute(ctx, contracts.ToolCallID("tc_r1"), "fetch", map[string]any{"q": "x"}, 1, ExecEnv{})
	if err != nil {
		t.Fatalf("registry tool execute error = %v, want a resolved run", err)
	}
	if out.Result["q"] != "x" {
		t.Fatalf("echo result = %v, want the input args back", out.Result)
	}
	// The resolved tool never entered the static set.
	if broker.Discoverable("fetch") {
		t.Fatal("a lookup-resolved tool must not enter the static conformance set")
	}
	// A completed row replays cached without re-invoking the lookup's tool.
	replay, err := broker.Execute(ctx, contracts.ToolCallID("tc_r1"), "fetch", map[string]any{"q": "x"}, 2, ExecEnv{})
	if err != nil || !replay.Cached {
		t.Fatalf("replay cached = %v err = %v, want a cached replay", replay.Cached, err)
	}

	// An unknown name is ErrUnknownTool even with a lookup wired.
	if _, err := broker.Execute(ctx, contracts.ToolCallID("tc_r2"), "nope", map[string]any{}, 3, ExecEnv{}); !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("unknown registry tool err = %v, want ErrUnknownTool", err)
	}
	// With no lookup wired at all, a miss is still ErrUnknownTool (static-only behaviour unchanged).
	if _, err := New().Execute(ctx, contracts.ToolCallID("tc_r3"), "fetch", map[string]any{}, 1, ExecEnv{}); !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("no-lookup miss err = %v, want ErrUnknownTool", err)
	}
}

// TestReplayClassResolvedFallback proves M2: a registry tool's DECLARED replay class is read through the
// same lookup the executor uses, so the pre-write kill-recovery marker decision sees the true class — not
// the ClassPure static-miss default. A static tool still reads its own class; a lookup miss is ClassPure.
func TestReplayClassResolvedFallback(t *testing.T) {
	static := Tool{Name: "static.pure", InputSchema: openObject, ReplayClass: ClassPure,
		Invoke: func(a map[string]any) (map[string]any, error) { return a, nil }}
	broker := New(static)
	broker.SetLookup(func(_ context.Context, _ ExecEnv, name string) (Tool, bool, error) {
		if name == "reg.irreversible" {
			return Tool{Name: name, ReplayClass: ClassIrreversible}, true, nil
		}
		return Tool{}, false, nil
	})
	ctx := context.Background()

	// The static ReplayClassOf can't see the registry tool — it defaults to pure (the latent hole).
	if got := broker.ReplayClassOf("reg.irreversible"); got != ClassPure {
		t.Fatalf("static ReplayClassOf(registry) = %q, want the ClassPure static-miss default", got)
	}
	// The resolved variant reads the DECLARED class through the lookup → needs a pre-write marker.
	got, err := broker.ReplayClassResolved(ctx, ExecEnv{}, "reg.irreversible")
	if err != nil {
		t.Fatalf("ReplayClassResolved error = %v", err)
	}
	if got != ClassIrreversible || !NeedsPreWrite(got) {
		t.Fatalf("resolved class = %q needsPreWrite=%v, want irreversible + pre-write", got, NeedsPreWrite(got))
	}
	// A static tool still resolves its own class; an unknown name is ClassPure.
	if got, _ := broker.ReplayClassResolved(ctx, ExecEnv{}, "static.pure"); got != ClassPure {
		t.Fatalf("resolved static class = %q, want pure", got)
	}
	if got, _ := broker.ReplayClassResolved(ctx, ExecEnv{}, "nope"); got != ClassPure {
		t.Fatalf("resolved unknown class = %q, want pure", got)
	}
}
