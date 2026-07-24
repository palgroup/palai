package models_test

// Runtime conformance — MOD-005 fallback, MOD-006 partial-then-fallback, MOD-008 attempt count,
// MOD-012 circuit (E16 T6). A route's fallback chain and a per-target circuit breaker are broker
// primitives (packages/model-broker/chain.go). This suite drives them deterministically with
// scripted adapters: a fallover is a NEW visible attempt (honest Attempts count, never a hidden
// retry multiplier), a partial streamed before a failure stays visible, an UPSTREAM failure trips
// the target's circuit and sheds it, and a caller-invalid error trips nothing and fails over
// nowhere.

import (
	"context"
	"errors"
	"testing"

	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// canceledAdapter reports a canceled call, the way a wire adapter surfaces a mid-call cancel.
type canceledAdapter struct{ calls *int }

func (a canceledAdapter) Execute(_ context.Context, _ modelbroker.Request, _ string, _ func(modelbroker.Delta)) (modelbroker.Result, error) {
	*a.calls++
	return modelbroker.Result{}, context.Canceled
}

// partialFailAdapter streams one partial delta, then returns a provider-side error at the given
// HTTP status — a primary that cuts off after a partial. okAdapter always succeeds; failingAdapter
// always returns a provider error at the given status. Each counts its Execute calls so a test can
// prove a target was shed (never called) or a route never failed over.
type partialFailAdapter struct {
	status int
	calls  *int
}

func (a partialFailAdapter) Execute(_ context.Context, req modelbroker.Request, _ string, onDelta func(modelbroker.Delta)) (modelbroker.Result, error) {
	*a.calls++
	d := modelbroker.Delta{Text: "Par"}
	if onDelta != nil {
		onDelta(d)
	}
	return modelbroker.Result{
		ModelRequestID: req.ModelRequestID, ProviderRequestID: "prov-partial-fail", Model: req.Model,
		Output: "Par", Deltas: []modelbroker.Delta{d}, Attempts: 1,
		Error: &modelbroker.SanitizedError{Code: "provider_error", Message: "cut off after partial", Status: a.status},
	}, nil
}

type failingAdapter struct {
	status int
	calls  *int
}

func (a failingAdapter) Execute(_ context.Context, req modelbroker.Request, _ string, _ func(modelbroker.Delta)) (modelbroker.Result, error) {
	*a.calls++
	return modelbroker.Result{
		ModelRequestID: req.ModelRequestID, ProviderRequestID: "prov-fail", Model: req.Model, Attempts: 1,
		Error: &modelbroker.SanitizedError{Code: "provider_error", Message: "upstream", Status: a.status},
	}, nil
}

type okAdapter struct {
	id    string
	calls *int
}

func (a okAdapter) Execute(_ context.Context, req modelbroker.Request, _ string, onDelta func(modelbroker.Delta)) (modelbroker.Result, error) {
	*a.calls++
	d := modelbroker.Delta{Text: "ok"}
	if onDelta != nil {
		onDelta(d)
	}
	return modelbroker.Result{
		ModelRequestID: req.ModelRequestID, ProviderRequestID: a.id, Model: req.Model,
		Output: "ok", Deltas: []modelbroker.Delta{d},
		Usage: contracts.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}, FinishReason: "stop", Attempts: 1,
	}, nil
}

// chainBroker builds a broker over the named adapters, all redeeming the sentinel secret.
func chainBroker(adapters map[string]modelbroker.ModelAdapter) *modelbroker.Broker {
	secrets := modelbroker.StaticResolver{}
	for name := range adapters {
		secrets[modelbroker.SecretRef(name)] = sentinelSecret
	}
	return modelbroker.New(modelbroker.Config{Adapters: adapters, Secrets: secrets})
}

// TestChainFailsOverAfterPartialWithHonestAttemptCount is MOD-005 + MOD-006 + MOD-008: the primary
// streams a partial then fails UPSTREAM; the chain fails over to the secondary, the partial the
// caller already saw stays visible, the secondary's output is returned, and Attempts reports BOTH
// real attempts — an honest count, not a hidden retry multiplier.
func TestChainFailsOverAfterPartialWithHonestAttemptCount(t *testing.T) {
	var primaryCalls, secondaryCalls int
	broker := chainBroker(map[string]modelbroker.ModelAdapter{
		"primary":   partialFailAdapter{status: 503, calls: &primaryCalls},
		"secondary": okAdapter{id: "prov-secondary", calls: &secondaryCalls},
	})
	chain := modelbroker.NewChain(broker, 5, 0)

	req := baseRequest()
	targets := []modelbroker.Target{
		{Provider: "primary", Secret: "primary"},
		{Provider: "secondary", Secret: "secondary"},
	}
	var streamed []string
	res, err := chain.Route(context.Background(), targets, req, func(d modelbroker.Delta) {
		streamed = append(streamed, d.Text)
	})
	if err != nil {
		t.Fatalf("chain route: %v", err)
	}
	if res.Output != "ok" || res.ProviderRequestID != "prov-secondary" {
		t.Errorf("result = %q/%q, want the secondary's served result", res.Output, res.ProviderRequestID)
	}
	if res.Attempts != 2 {
		t.Errorf("attempts = %d, want 2 (primary failed + secondary served — an honest count, not a hidden multiplier)", res.Attempts)
	}
	if primaryCalls != 1 || secondaryCalls != 1 {
		t.Errorf("calls primary=%d secondary=%d, want each called once", primaryCalls, secondaryCalls)
	}
	// The partial streamed before the primary failed stays VISIBLE — no seamless/hidden retry erased it.
	if len(streamed) < 2 || streamed[0] != "Par" {
		t.Errorf("streamed = %v, want the primary partial %q kept before the secondary's delta", streamed, "Par")
	}
}

// TestChainCallerInvalidTripsNothingAndFailsOverNowhere is MOD-012 (the caller-invalid half): a
// caller-invalid (400) error fails identically on every target, so the chain neither trips the
// circuit nor fails over — it surfaces the typed error on the Result. Repeated past the breaker
// threshold, the primary is STILL called every time (the circuit never opened) and the secondary
// is NEVER called (no failover).
func TestChainCallerInvalidTripsNothingAndFailsOverNowhere(t *testing.T) {
	var primaryCalls, secondaryCalls int
	broker := chainBroker(map[string]modelbroker.ModelAdapter{
		"primary":   failingAdapter{status: 400, calls: &primaryCalls},
		"secondary": okAdapter{id: "prov-secondary", calls: &secondaryCalls},
	})
	const threshold = 3
	chain := modelbroker.NewChain(broker, threshold, 0)
	req := baseRequest()
	targets := []modelbroker.Target{
		{Provider: "primary", Secret: "primary"},
		{Provider: "secondary", Secret: "secondary"},
	}

	const routes = threshold + 2 // well past the threshold
	for i := 0; i < routes; i++ {
		res, err := chain.Route(context.Background(), targets, req, nil)
		if err != nil {
			t.Fatalf("route %d: unexpected go error %v", i, err)
		}
		if res.Error == nil || res.Error.Status != 400 {
			t.Fatalf("route %d: result error = %+v, want the surfaced 400", i, res.Error)
		}
		if res.Attempts != 1 {
			t.Errorf("route %d: attempts = %d, want 1 (a caller-invalid error is a single real attempt)", i, res.Attempts)
		}
	}
	if primaryCalls != routes {
		t.Errorf("primary calls = %d, want %d — a caller-invalid error must NOT open the circuit", primaryCalls, routes)
	}
	if secondaryCalls != 0 {
		t.Errorf("secondary calls = %d, want 0 — a caller-invalid error must NOT fail over", secondaryCalls)
	}
}

// TestChainUpstreamFailuresTripCircuitThenShed is MOD-012 (the upstream half): repeated UPSTREAM
// failures on a target trip its circuit; once open, the chain sheds that target FAST — no further
// call to it — and fails over to a permitted route. Every route still returns a served result.
func TestChainUpstreamFailuresTripCircuitThenShed(t *testing.T) {
	var primaryCalls, secondaryCalls int
	broker := chainBroker(map[string]modelbroker.ModelAdapter{
		"primary":   failingAdapter{status: 503, calls: &primaryCalls},
		"secondary": okAdapter{id: "prov-secondary", calls: &secondaryCalls},
	})
	const threshold = 3
	chain := modelbroker.NewChain(broker, threshold, 0) // 0 cooldown -> default (long); stays open for the test
	req := baseRequest()
	targets := []modelbroker.Target{
		{Provider: "primary", Secret: "primary"},
		{Provider: "secondary", Secret: "secondary"},
	}

	const routes = threshold + 2
	for i := 0; i < routes; i++ {
		res, err := chain.Route(context.Background(), targets, req, nil)
		if err != nil {
			t.Fatalf("route %d: %v", i, err)
		}
		if res.Output != "ok" {
			t.Fatalf("route %d: output = %q, want the secondary's served result", i, res.Output)
		}
	}
	// The primary's circuit opens after `threshold` consecutive upstream failures; the routes after
	// that shed it without a call, so it is invoked exactly `threshold` times, not `routes` times.
	if primaryCalls != threshold {
		t.Errorf("primary calls = %d, want %d — the circuit must shed the failed target after it opens", primaryCalls, threshold)
	}
	if secondaryCalls != routes {
		t.Errorf("secondary calls = %d, want %d — every route must fail over to the permitted target", secondaryCalls, routes)
	}
}

// TestChainCancelStopsTheChainWithoutFailingOver is MOD-009 ∩ chain: a cancel is caller intent, so
// the chain returns it immediately and NEVER fails over to another target (a fallover would burn a
// second provider call and budget against a run the caller already stopped).
func TestChainCancelStopsTheChainWithoutFailingOver(t *testing.T) {
	var primaryCalls, secondaryCalls int
	broker := chainBroker(map[string]modelbroker.ModelAdapter{
		"primary":   canceledAdapter{calls: &primaryCalls},
		"secondary": okAdapter{id: "prov-secondary", calls: &secondaryCalls},
	})
	chain := modelbroker.NewChain(broker, 5, 0)
	targets := []modelbroker.Target{{Provider: "primary", Secret: "primary"}, {Provider: "secondary", Secret: "secondary"}}
	_, err := chain.Route(context.Background(), targets, baseRequest(), nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: got %v, want context.Canceled surfaced", err)
	}
	if secondaryCalls != 0 {
		t.Errorf("secondary calls = %d, want 0 — a cancel must not fail over", secondaryCalls)
	}
}

// TestChainRoutesAroundMisconfiguredTarget is the chain's misconfigured-target guard: an unknown
// provider (config error, not upstream) is routed AROUND to the next target without tripping the
// upstream-health circuit.
func TestChainRoutesAroundMisconfiguredTarget(t *testing.T) {
	var secondaryCalls int
	broker := chainBroker(map[string]modelbroker.ModelAdapter{
		"secondary": okAdapter{id: "prov-secondary", calls: &secondaryCalls},
	})
	chain := modelbroker.NewChain(broker, 5, 0)
	targets := []modelbroker.Target{
		{Provider: "missing", Secret: "missing"}, // never registered
		{Provider: "secondary", Secret: "secondary"},
	}
	res, err := chain.Route(context.Background(), targets, baseRequest(), nil)
	if err != nil {
		t.Fatalf("route around misconfigured target: %v", err)
	}
	if res.Output != "ok" || secondaryCalls != 1 {
		t.Errorf("result=%q secondaryCalls=%d, want the chain to route around the missing target to the served secondary", res.Output, secondaryCalls)
	}
}
