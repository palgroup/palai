//go:build e2e

package responses

import (
	"context"
	"sync"
	"testing"

	fake "github.com/palgroup/palai/adapters/models/fake"
	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// crashAfterFirstRoute wraps a model adapter and, exactly once, cancels the running
// attempt after the inner adapter has recorded its provider effect but before the
// orchestrator commits the result — the "Route given, result not committed, process
// died" window a reclaim re-opens. It makes the crash deterministic without racing a
// wall clock.
type crashAfterFirstRoute struct {
	inner modelbroker.ModelAdapter
	mu    sync.Mutex
	crash func()
}

func (c *crashAfterFirstRoute) Execute(ctx context.Context, req modelbroker.Request, secret string, onDelta func(modelbroker.Delta)) (modelbroker.Result, error) {
	res, err := c.inner.Execute(ctx, req, secret, onDelta)
	c.mu.Lock()
	crash := c.crash
	c.crash = nil
	c.mu.Unlock()
	if crash != nil {
		crash() // the effect is already recorded; the commit that follows now fails
	}
	return res, err
}

// TestReclaimAfterCrashBetweenRouteAndCommitReusesProviderIdempotencyKey proves the
// cross-attempt provider idempotency key closes the crash window Task 11's DB-side replay
// leaves open. Attempt 1 persists the model request, routes it (one provider effect), then
// crashes before committing the result. The reclaimed attempt re-derives the same stable
// model_request_id — so LookupModelResult misses (the result was never committed) and the
// request is re-routed — but carries the same idempotency key, so the provider replays its
// stored result: the provider saw the same key twice and settled exactly one effect.
func TestReclaimAfterCrashBetweenRouteAndCommitReusesProviderIdempotencyKey(t *testing.T) {
	h := newHarness(t)
	responseID, _, runID := h.admit()

	ledger := fake.NewIdempotencyLedger()
	idempotent := fake.Adapter{
		Script: fake.Script{
			ProviderRequestID: "prov_reclaim",
			Model:             "fake",
			Output:            "reclaimed",
			Usage:             contracts.Usage{InputTokens: 5, OutputTokens: 3, TotalTokens: 8},
		},
		Idempotency: ledger,
	}

	// A single stable model request id both attempts re-derive.
	mreq := newID("mreq")
	engineReady := scriptFrame("engine.ready", runID, 1, map[string]any{
		"selected_protocol": "engine.v1",
		"engine":            map[string]any{"name": "fake", "version": "0"},
		"max_frame_bytes":   1024, "nonce": "n",
	})
	modelRequest := scriptFrame("model.request", runID, 2, map[string]any{"model_request_id": mreq})

	// Attempt 1: route the request, then crash before the result is committed.
	crashCtx, crashCancel := context.WithCancel(context.Background())
	defer crashCancel()
	crashing := &crashAfterFirstRoute{inner: idempotent, crash: crashCancel}
	attempt1 := h.newOrchestratorWithAdapter(scriptedDialer{&scriptedChannel{frames: []contracts.EngineFrame{engineReady, modelRequest}}}, crashing)
	if err := attempt1.ExecuteAttempt(crashCtx, h.descriptor(runID, 1)); err == nil {
		t.Fatal("attempt 1 completed; it must crash between route and commit")
	}

	// The result was not committed, so the model request row is still 'requested'.
	if got := h.count(`SELECT count(*) FROM model_requests WHERE id=$1 AND state='completed'`, mreq); got != 0 {
		t.Fatalf("model request committed a result despite the crash (completed rows = %d)", got)
	}
	if ledger.Effects() != 1 {
		t.Fatalf("provider effects after attempt 1 = %d, want 1", ledger.Effects())
	}

	// Attempt 2 (reclaimed, higher fence): the same stable request id re-routes because
	// no committed result exists, and runs to a terminal completion.
	terminal := []contracts.EngineFrame{
		engineReady, modelRequest,
		scriptFrame("output.item", runID, 3, map[string]any{"type": "message", "content": "reclaimed"}),
		scriptFrame("run.terminal", runID, 4, map[string]any{"outcome": "completed", "output": "reclaimed"}),
	}
	attempt2 := h.newOrchestratorWithAdapter(scriptedDialer{&scriptedChannel{frames: terminal}}, idempotent)
	if err := attempt2.ExecuteAttempt(context.Background(), h.descriptor(runID, 2)); err != nil {
		t.Fatalf("reclaimed attempt error = %v", err)
	}

	// The provider saw the same idempotency key on both attempts and settled one effect.
	keys := ledger.Keys()
	wantKey := runID + "/" + mreq
	if len(keys) != 2 || keys[0] != wantKey || keys[1] != wantKey {
		t.Fatalf("provider idempotency keys = %v, want %q twice", keys, wantKey)
	}
	if ledger.Effects() != 1 {
		t.Fatalf("provider effects = %d, want exactly 1 (one external effect across attempts)", ledger.Effects())
	}
	if state, _ := h.response(responseID); state != "completed" {
		t.Fatalf("response state = %q, want completed", state)
	}
}
