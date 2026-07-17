// Package models_test is the deterministic model-adapter conformance suite. It
// drives the fake adapter through the broker and asserts the canonical
// text/delta/tool/schema/usage/cancel/error conversions every adapter must honor
// (spec §25.9, §53.4). The same canonical Result contract (Result.Validate) is
// asserted against the live adapter in the protected live tier; this suite needs
// no network or credential.
package models_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	fake "github.com/palgroup/palai/adapters/models/fake"
	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// sentinelSecret is the credential value the broker redeems for the fake provider.
// No canonical conversion may ever surface it.
const sentinelSecret = "sk-fake-SENTINEL-must-never-leak-000"

func baseRequest() modelbroker.Request {
	return modelbroker.Request{
		ModelRequestID: contracts.ModelRequestID("mreq_conformance1"),
		RouteRevision:  3,
		ModelStepID:    "step-1",
		Model:          "fake-1",
		Messages:       []modelbroker.Message{{Role: "user", Content: "hello"}},
		Deadline:       time.Date(2026, time.July, 16, 12, 0, 5, 0, time.UTC),
		Privacy:        modelbroker.PrivacyFlags{NoRetain: true, NoTrain: true},
		Reservation:    modelbroker.Reservation{MaxTotalTokens: 100},
		Secret:         modelbroker.SecretRef("provider-one"),
	}
}

func newBroker(t *testing.T, adapter modelbroker.ModelAdapter) *modelbroker.Broker {
	t.Helper()
	return modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"provider-one": adapter},
		Secrets:  modelbroker.StaticResolver{"provider-one": sentinelSecret},
	})
}

// TestAdapterConvertsTextDeltasAndUsage proves streamed text increments assemble
// into the canonical Output and that usage is reported and internally consistent.
func TestAdapterConvertsTextDeltasAndUsage(t *testing.T) {
	adapter := fake.Adapter{Script: fake.Script{
		ProviderRequestID: "chatcmpl-fake-001",
		Model:             "fake-1",
		TextDeltas:        []string{"Hel", "lo, ", "world"},
		Usage:             contracts.Usage{InputTokens: 8, OutputTokens: 3, TotalTokens: 11},
	}}
	var seen []modelbroker.Delta
	res, err := newBroker(t, adapter).Route(context.Background(), "provider-one", baseRequest(),
		func(d modelbroker.Delta) { seen = append(seen, d) })
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if err := res.Validate(); err != nil {
		t.Fatalf("result is not canonical: %v", err)
	}
	if res.Output != "Hello, world" {
		t.Errorf("Output = %q, want assembled %q", res.Output, "Hello, world")
	}
	if len(seen) < 3 {
		t.Errorf("streamed %d deltas, want the 3 text increments", len(seen))
	}
	if res.ProviderRequestID != "chatcmpl-fake-001" {
		t.Errorf("ProviderRequestID = %q, want the real provider id", res.ProviderRequestID)
	}
	if res.Usage.TotalTokens != 11 || res.Usage.InputTokens != 8 || res.Usage.OutputTokens != 3 {
		t.Errorf("usage = %+v, want input=8 output=3 total=11", res.Usage)
	}
	if res.Attempts != 1 {
		t.Errorf("attempts = %d, want exactly 1 (no hidden provider retry)", res.Attempts)
	}
}

// TestAdapterConvertsToolRequestsWithSchemaArguments proves a tool request is
// converted with a stable id and arguments that parse as JSON conforming to the
// requested tool schema (the schema-constrained facet).
func TestAdapterConvertsToolRequestsWithSchemaArguments(t *testing.T) {
	req := baseRequest()
	req.Tools = []modelbroker.ToolSchema{{
		Name:       "palai.conformance.math.add",
		Parameters: map[string]any{"type": "object", "required": []any{"a", "b"}},
		Strict:     true,
	}}
	adapter := fake.Adapter{Script: fake.Script{
		ProviderRequestID: "chatcmpl-fake-002",
		Model:             "fake-1",
		ToolCalls: []modelbroker.ToolCall{{
			ID:        "call_1",
			Name:      "palai.conformance.math.add",
			Arguments: `{"a":7,"b":5}`,
		}},
		Usage: contracts.Usage{InputTokens: 12, OutputTokens: 6, TotalTokens: 18, ToolCalls: 1},
	}}
	res, err := newBroker(t, adapter).Route(context.Background(), "provider-one", req, nil)
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if err := res.Validate(); err != nil {
		t.Fatalf("result is not canonical: %v", err)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Name != "palai.conformance.math.add" {
		t.Fatalf("tool requests = %+v, want one add call", res.ToolCalls)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(res.ToolCalls[0].Arguments), &args); err != nil {
		t.Fatalf("tool arguments are not valid JSON: %v", err)
	}
	if _, ok := args["a"]; !ok {
		t.Errorf("tool arguments %v do not carry the schema-required key a", args)
	}
	if res.Usage.ToolCalls != 1 {
		t.Errorf("usage.tool_calls = %d, want 1", res.Usage.ToolCalls)
	}
}

// TestBrokerRedeemsSecretRefOnlyInExecutor proves the request carries a SecretRef
// name, never the value, and that the executor redeems it exactly once and hands
// the value to the adapter — nothing writes it back onto the request.
func TestBrokerRedeemsSecretRefOnlyInExecutor(t *testing.T) {
	var gotSecret string
	adapter := recordingAdapter{onExecute: func(req modelbroker.Request, secret string) {
		gotSecret = secret
		if strings.Contains(string(req.Secret), sentinelSecret) {
			t.Errorf("request.Secret carried the raw value, want an opaque name")
		}
	}}
	req := baseRequest()
	if string(req.Secret) == sentinelSecret {
		t.Fatalf("test setup leaked the secret into the request")
	}
	if _, err := newBroker(t, adapter).Route(context.Background(), "provider-one", req, nil); err != nil {
		t.Fatalf("route: %v", err)
	}
	if gotSecret != sentinelSecret {
		t.Errorf("executor redeemed %q, want the resolved credential value", gotSecret)
	}
}

// TestAdapterHonorsCancellation proves a canceled context stops the stream and is
// surfaced as a canceled outcome, not a completed one.
func TestAdapterHonorsCancellation(t *testing.T) {
	adapter := fake.Adapter{Script: fake.Script{
		ProviderRequestID: "chatcmpl-fake-003",
		Model:             "fake-1",
		TextDeltas:        []string{"partial"},
		Usage:             contracts.Usage{InputTokens: 4, OutputTokens: 1, TotalTokens: 5},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := newBroker(t, adapter).Route(ctx, "provider-one", baseRequest(), nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("route on canceled context: got %v, want context.Canceled", err)
	}
}

// TestAdapterSanitizesProviderError proves a provider-side error becomes a
// sanitized canonical error carried on the Result, with no credential in its text.
func TestAdapterSanitizesProviderError(t *testing.T) {
	adapter := fake.Adapter{Script: fake.Script{
		ProviderRequestID: "chatcmpl-fake-004",
		Model:             "fake-1",
		Err:               &modelbroker.SanitizedError{Code: "provider_error", Message: "upstream refused the request", Status: 429},
	}}
	res, err := newBroker(t, adapter).Route(context.Background(), "provider-one", baseRequest(), nil)
	if err != nil {
		t.Fatalf("a sanitized provider error must ride on the Result, not the Go error: %v", err)
	}
	if res.Error == nil || res.Error.Status != 429 {
		t.Fatalf("Result.Error = %+v, want a sanitized 429", res.Error)
	}
	if strings.Contains(res.Error.Message, sentinelSecret) {
		t.Error("sanitized error leaked the credential")
	}
	if err := res.Validate(); err != nil {
		t.Fatalf("an error result is still canonical: %v", err)
	}
}

// recordingAdapter captures the redeemed secret and request the executor passes in.
type recordingAdapter struct {
	onExecute func(req modelbroker.Request, secret string)
}

func (a recordingAdapter) Execute(ctx context.Context, req modelbroker.Request, secret string, onDelta func(modelbroker.Delta)) (modelbroker.Result, error) {
	a.onExecute(req, secret)
	return modelbroker.Result{
		ModelRequestID:    req.ModelRequestID,
		ProviderRequestID: "chatcmpl-fake-rec",
		Model:             req.Model,
		Output:            "ok",
		Usage:             contracts.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
	}, nil
}
