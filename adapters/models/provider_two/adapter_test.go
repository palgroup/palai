package providertwo_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	providertwo "github.com/palgroup/palai/adapters/models/provider_two"
	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// sentinelSecret is the credential the adapter must send in x-api-key and never
// surface in a canonical conversion.
const sentinelSecret = "sk-ant-SENTINEL-must-never-leak-000"

// cannedStream is a recorded Anthropic Messages SSE transcript: message_start with
// the real message id/model/usage, two text deltas, a tool_use block whose name
// arrives on content_block_start and whose JSON input spans two input_json_delta
// fragments, a message_delta carrying the stop reason and final output tokens, and
// message_stop. It drives provider-two's real conversion code (event fold / fragment
// assembly / usage derivation) over real HTTP with zero spend.
const cannedStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_CANNED123","model":"claude-canned","usage":{"input_tokens":11,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_abc","name":"palai_conformance_math_add"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"a\":7,"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"b\":5}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}

event: message_stop
data: {"type":"message_stop"}

`

func toolSchemaRequest() modelbroker.Request {
	req := modelbroker.Request{
		ModelRequestID: contracts.ModelRequestID("mreq_provider_two_1"),
		RouteRevision:  1,
		ModelStepID:    "step-1",
		Model:          "claude-canned",
		Messages:       []modelbroker.Message{{Role: "user", Content: "add 7 and 5"}},
		Deadline:       time.Now().Add(30 * time.Second),
		Reservation:    modelbroker.Reservation{MaxTotalTokens: 100},
	}
	req.Tools = []modelbroker.ToolSchema{{
		Name:       "palai.conformance.math.add",
		Parameters: map[string]any{"type": "object", "required": []any{"a", "b"}, "additionalProperties": false},
		Strict:     true,
	}}
	req.ForceToolCall = true
	return req
}

// TestConvertsStreamedMessage drives the real provider-two adapter over real HTTP
// against a canned Anthropic SSE transcript and asserts the canonical conversion:
// text deltas assembled in order, tool_use reassembled with the canonical (dotted)
// name restored, usage derived (input + output), the message id/model/finish reason
// surfaced, exactly one attempt — and the request body carries the Anthropic shape.
func TestConvertsStreamedMessage(t *testing.T) {
	var sentBody []byte
	var sentKey, sentVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sentBody, _ = io.ReadAll(r.Body)
		sentKey = r.Header.Get("x-api-key")
		sentVersion = r.Header.Get("anthropic-version")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, cannedStream)
	}))
	defer srv.Close()

	var texts []string
	res, err := providertwo.Adapter{BaseURL: srv.URL}.Execute(context.Background(), toolSchemaRequest(), sentinelSecret,
		func(d modelbroker.Delta) {
			if d.Text != "" {
				texts = append(texts, d.Text)
			}
		})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	res.ModelRequestID = toolSchemaRequest().ModelRequestID // the broker normally re-stamps this
	if err := res.Validate(); err != nil {
		t.Fatalf("result is not canonical: %v", err)
	}

	// buildBody: the wire request carried the Anthropic shape and the credential header.
	assertRequestBody(t, sentBody)
	if sentKey != sentinelSecret {
		t.Errorf("adapter did not send the credential in x-api-key")
	}
	if sentVersion == "" {
		t.Errorf("adapter did not send the anthropic-version header")
	}

	// consume: text deltas assembled in stream order.
	if res.Output != "Hello" {
		t.Errorf("Output = %q, want assembled %q", res.Output, "Hello")
	}
	if strings.Join(texts, "") != "Hello" {
		t.Errorf("streamed text = %q, want the increments in order", strings.Join(texts, ""))
	}
	if res.ProviderRequestID != "msg_CANNED123" || res.Model != "claude-canned" {
		t.Errorf("provider id/model = %q/%q, want the streamed values", res.ProviderRequestID, res.Model)
	}
	if res.FinishReason != "tool_calls" {
		t.Errorf("finish reason = %q, want the canonical tool_calls (Anthropic tool_use)", res.FinishReason)
	}
	if res.Usage.InputTokens != 11 || res.Usage.OutputTokens != 7 || res.Usage.TotalTokens != 18 {
		t.Errorf("usage = %+v, want input=11 output=7 total=18 (derived)", res.Usage)
	}
	if res.Attempts != 1 {
		t.Errorf("attempts = %d, want exactly 1 (no hidden provider retry)", res.Attempts)
	}

	// consume + wireToolName: fragments reassembled and the canonical name restored.
	if len(res.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v, want exactly one reassembled call", res.ToolCalls)
	}
	call := res.ToolCalls[0]
	if call.Name != "palai.conformance.math.add" {
		t.Errorf("tool name = %q, want the canonical dotted name restored from the wire", call.Name)
	}
	if call.ID != "toolu_abc" {
		t.Errorf("tool id = %q, want the provider tool_use id", call.ID)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
		t.Fatalf("reassembled arguments %q are not valid JSON: %v", call.Arguments, err)
	}
	if args["a"] != float64(7) || args["b"] != float64(5) {
		t.Errorf("reassembled arguments = %v, want {a:7,b:5}", args)
	}
}

func assertRequestBody(t *testing.T, body []byte) {
	t.Helper()
	var sent struct {
		Model      string `json:"model"`
		MaxTokens  int    `json:"max_tokens"`
		Stream     bool   `json:"stream"`
		ToolChoice struct {
			Type string `json:"type"`
		} `json:"tool_choice"`
		Tools []struct {
			Name        string         `json:"name"`
			InputSchema map[string]any `json:"input_schema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("adapter sent a body that is not JSON: %v", err)
	}
	if !sent.Stream {
		t.Errorf("request did not set stream=true: %s", body)
	}
	if sent.MaxTokens <= 0 {
		t.Errorf("request did not carry the required max_tokens: %s", body)
	}
	if sent.ToolChoice.Type != "any" {
		t.Errorf("forced tool call did not set tool_choice type=any, got %q", sent.ToolChoice.Type)
	}
	if len(sent.Tools) != 1 || sent.Tools[0].Name != "palai_conformance_math_add" {
		t.Errorf("tool was not wire-encoded to the provider charset with input_schema: %s", body)
	}
	if sent.Tools[0].InputSchema == nil {
		t.Errorf("tool schema was sent under the wrong key (Anthropic uses input_schema): %s", body)
	}
}

// TestSanitizesHTTPError drives the adapter against a 429 whose body carries an
// Anthropic error envelope and asserts the sanitized error surfaces the stable type
// and HTTP status but drops the free text (which can echo a key prefix).
func TestSanitizesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"rate limit reached for key sk-ant-REDACTED"}}`)
	}))
	defer srv.Close()

	req := toolSchemaRequest()
	res, err := providertwo.Adapter{BaseURL: srv.URL}.Execute(context.Background(), req, sentinelSecret, nil)
	if err != nil {
		t.Fatalf("a sanitized provider error must ride on the Result, not the Go error: %v", err)
	}
	if res.Error == nil || res.Error.Status != http.StatusTooManyRequests {
		t.Fatalf("Result.Error = %+v, want a sanitized 429", res.Error)
	}
	if res.Error.Code != "rate_limit_error" {
		t.Errorf("error code = %q, want the stable provider type surfaced", res.Error.Code)
	}
	if strings.Contains(res.Error.Message, "rate limit reached") || strings.Contains(res.Error.Message, "sk-ant") {
		t.Errorf("sanitized error leaked provider free-text: %q", res.Error.Message)
	}
}

// TestHonorsCancellation proves a canceled context aborts the call rather than
// returning a completed result.
func TestHonorsCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, cannedStream)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := toolSchemaRequest()
	req.Deadline = time.Time{} // let the canceled ctx, not a deadline, stop the call
	if _, err := (providertwo.Adapter{BaseURL: srv.URL}).Execute(ctx, req, sentinelSecret, nil); err == nil {
		t.Fatal("execute on a canceled context returned no error")
	}
}
