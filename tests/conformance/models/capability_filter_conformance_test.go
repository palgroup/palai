package models_test

// Runtime conformance — MOD-004 capability hard-filter (E16 T6, completing the E13 T8 routing half).
// The OpenAI-compatible adapter's active capability probe (T5) is a HARD admission filter: a run that
// hard-requires a capability the endpoint lacks is REJECTED PRE-run — the completion is never sent —
// and the filter never loosens. The paired assertions prove it is specific (a text-only run on the
// SAME endpoint is admitted) yet hard (a tool-requiring run on that endpoint is always rejected).

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	openaicompatible "github.com/palgroup/palai/adapters/models/openai_compatible"
	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// TestCapabilityHardFilterRejectsUnsupportedBeforeAdmission is MOD-004: an endpoint whose probed
// record lacks tool-calling rejects a tool-requiring run PRE-run (no completion sent, the endpoint
// is never hit for it), while a text-only run on the SAME endpoint is admitted and served — the hard
// filter is applied per-requirement and never loosens.
func TestCapabilityHardFilterRejectsUnsupportedBeforeAdmission(t *testing.T) {
	var completions int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&completions, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(finalStream)) // a minimal text completion (provider_one_test.go)
	}))
	t.Cleanup(srv.Close)

	// A "private" endpoint that streams but does NOT support tool-calling (the honest missing-feature
	// record). Preloaded so the probe does not itself reach the endpoint.
	prober := openaicompatible.NewProber()
	prober.Preload(srv.URL, openaicompatible.CapabilityRecord{Streaming: true, ToolCalls: false, StructuredJSON: false})
	adapter := openaicompatible.Adapter{Adapter: providerone.Adapter{BaseURL: srv.URL}, Prober: prober}
	broker := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"p": adapter},
		Secrets:  modelbroker.StaticResolver{"p": sentinelSecret},
	})

	// A tool-requiring run hard-requires a capability the endpoint lacks: rejected PRE-run.
	toolReq := canonicalToolRequest()
	toolReq.Secret = "p"
	_, err := broker.Route(context.Background(), "p", toolReq, nil)
	if !errors.Is(err, openaicompatible.ErrCapabilityUnsupported) {
		t.Fatalf("tool-requiring run: got %v, want ErrCapabilityUnsupported (rejected before admission)", err)
	}
	if got := atomic.LoadInt64(&completions); got != 0 {
		t.Errorf("endpoint was hit %d times for a rejected run, want 0 — the completion must never be sent", got)
	}

	// A text-only run on the SAME endpoint requires no missing capability: admitted and served.
	textReq := baseRequest()
	textReq.Deadline = time.Now().Add(30 * time.Second)
	textReq.Secret = "p"
	res, err := broker.Route(context.Background(), "p", textReq, nil)
	if err != nil {
		t.Fatalf("text-only run on the same endpoint must be admitted: %v", err)
	}
	if err := res.Validate(); err != nil {
		t.Fatalf("admitted result is not canonical: %v", err)
	}
	if atomic.LoadInt64(&completions) != 1 {
		t.Errorf("endpoint completions = %d, want exactly the one admitted text run", completions)
	}
}
