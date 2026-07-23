package models_test

// This file makes the deterministic model-adapter conformance suite adapter-PARAMETRIC:
// the fake, provider-one (OpenAI), provider-two (Anthropic), and the OpenAI-compatible
// adapter are each driven — through the broker, over their own wire fixture — to produce
// the SAME canonical tool-producing Result, and every adapter is run through the SAME
// canonical assert set. It is the mechanical proof that the broker's Result contract is
// provider-agnostic: a second independent provider family and a parametrized generic
// adapter converge on the identical canonical shape (E16 T5; MOD-001).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	fake "github.com/palgroup/palai/adapters/models/fake"
	openaicompatible "github.com/palgroup/palai/adapters/models/openai_compatible"
	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	providertwo "github.com/palgroup/palai/adapters/models/provider_two"
	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// anthropicCannedStream is the provider-two wire fixture: an Anthropic Messages SSE that
// produces the same math.add tool call the other adapters produce, so provider-two folds
// to the identical canonical shape.
const anthropicCannedStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_CANON2","model":"claude-canon","usage":{"input_tokens":11,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"palai_conformance_math_add"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"a\":7,"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"b\":5}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}

event: message_stop
data: {"type":"message_stop"}

`

// canonicalToolRequest is the one request every adapter answers: a forced call to the
// schema-constrained add tool. A live deadline is required for the wire adapters' HTTP.
func canonicalToolRequest() modelbroker.Request {
	req := baseRequest()
	req.Deadline = time.Now().Add(30 * time.Second)
	req.Tools = []modelbroker.ToolSchema{{
		Name:       "palai.conformance.math.add",
		Parameters: map[string]any{"type": "object", "required": []any{"a", "b"}, "additionalProperties": false},
		Strict:     true,
	}}
	req.ForceToolCall = true
	return req
}

// sseServer serves a canned SSE transcript over real HTTP.
func sseServer(t *testing.T, transcript string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(transcript))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// routeThrough runs one adapter through the broker under a fixed provider name, capturing
// the streamed deltas — the same seam every adapter is exercised behind.
func routeThrough(t *testing.T, adapter modelbroker.ModelAdapter) (modelbroker.Result, []modelbroker.Delta) {
	t.Helper()
	broker := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"p": adapter},
		Secrets:  modelbroker.StaticResolver{"p": sentinelSecret},
	})
	req := canonicalToolRequest()
	req.Secret = "p"
	var deltas []modelbroker.Delta
	res, err := broker.Route(context.Background(), "p", req, func(d modelbroker.Delta) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	return res, deltas
}

// TestAdaptersConvergeOnCanonicalToolResult runs all four adapters through the one shared
// canonical assert set. Each has its own family and its own wire fixture; the canonical
// Result they produce is identical in shape — the provider-agnostic contract.
func TestAdaptersConvergeOnCanonicalToolResult(t *testing.T) {
	cases := []struct {
		name    string
		adapter func(t *testing.T) modelbroker.ModelAdapter
	}{
		{"fake", func(t *testing.T) modelbroker.ModelAdapter {
			return fake.Adapter{Script: fake.Script{
				ProviderRequestID: "prov-fake-canon",
				Model:             "fake-1",
				ToolCalls: []modelbroker.ToolCall{{
					ID: "call_1", Name: "palai.conformance.math.add", Arguments: `{"a":7,"b":5}`,
				}},
				Usage: contracts.Usage{InputTokens: 9, OutputTokens: 5, TotalTokens: 14, ToolCalls: 1},
			}}
		}},
		{"provider-one", func(t *testing.T) modelbroker.ModelAdapter {
			return providerone.Adapter{BaseURL: sseServer(t, cannedStream).URL}
		}},
		{"provider-two", func(t *testing.T) modelbroker.ModelAdapter {
			return providertwo.Adapter{BaseURL: sseServer(t, anthropicCannedStream).URL}
		}},
		{"openai-compatible", func(t *testing.T) modelbroker.ModelAdapter {
			srv := sseServer(t, cannedStream)
			prober := openaicompatible.NewProber()
			prober.Preload(srv.URL, openaicompatible.CapabilityRecord{Streaming: true, ToolCalls: true, StructuredJSON: true})
			return openaicompatible.Adapter{Adapter: providerone.Adapter{BaseURL: srv.URL}, Prober: prober}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, deltas := routeThrough(t, c.adapter(t))
			assertCanonicalToolResult(t, res, deltas)
		})
	}
}

// assertCanonicalToolResult is the ONE canonical assert set every adapter passes.
func assertCanonicalToolResult(t *testing.T, res modelbroker.Result, deltas []modelbroker.Delta) {
	t.Helper()
	if err := res.Validate(); err != nil {
		t.Fatalf("result is not canonical: %v", err)
	}
	if res.Attempts != 1 {
		t.Errorf("attempts = %d, want exactly 1 (no hidden provider retry)", res.Attempts)
	}
	if res.FinishReason != "tool_calls" {
		t.Errorf("finish reason = %q, want the canonical tool_calls across every family", res.FinishReason)
	}
	if res.ProviderRequestID == "" {
		t.Error("a successful result must carry the provider request id")
	}
	// Usage is internally consistent and populated (each family reports its own numbers).
	if res.Usage.InputTokens <= 0 || res.Usage.OutputTokens <= 0 {
		t.Errorf("usage = %+v, want populated input/output", res.Usage)
	}
	if res.Usage.TotalTokens != res.Usage.InputTokens+res.Usage.OutputTokens {
		t.Errorf("usage total = %d, want input+output = %d", res.Usage.TotalTokens, res.Usage.InputTokens+res.Usage.OutputTokens)
	}
	// Exactly one tool call, canonical dotted name restored, schema-conforming arguments.
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Name != "palai.conformance.math.add" {
		t.Fatalf("tool calls = %+v, want one canonical add call", res.ToolCalls)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(res.ToolCalls[0].Arguments), &args); err != nil {
		t.Fatalf("tool arguments are not valid JSON: %v", err)
	}
	if args["a"] != float64(7) || args["b"] != float64(5) {
		t.Errorf("tool arguments = %v, want {a:7,b:5}", args)
	}
	// The tool call was streamed as at least one delta.
	var sawToolDelta bool
	for _, d := range deltas {
		if d.ToolCall != nil {
			sawToolDelta = true
		}
	}
	if !sawToolDelta {
		t.Error("no tool-call delta was streamed")
	}
}
