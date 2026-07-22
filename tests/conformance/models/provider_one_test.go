package models_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// cannedStream is a recorded OpenAI chat.completion SSE transcript: a role delta,
// two text deltas, a tool call whose name arrives on the first fragment and whose
// arguments span two more, a finish-reason chunk, and a usage-only chunk under
// stream_options, terminated by [DONE]. It drives provider_one's real conversion
// code (consume/fragment assembly/usage folding) over real HTTP with zero spend.
const cannedStream = `data: {"id":"chatcmpl-CANNED123","object":"chat.completion.chunk","model":"gpt-4o-mini-canned","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}

data: {"id":"chatcmpl-CANNED123","object":"chat.completion.chunk","model":"gpt-4o-mini-canned","choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}]}

data: {"id":"chatcmpl-CANNED123","object":"chat.completion.chunk","model":"gpt-4o-mini-canned","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}

data: {"id":"chatcmpl-CANNED123","object":"chat.completion.chunk","model":"gpt-4o-mini-canned","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"palai_conformance_math_add","arguments":""}}]},"finish_reason":null}]}

data: {"id":"chatcmpl-CANNED123","object":"chat.completion.chunk","model":"gpt-4o-mini-canned","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":7,"}}]},"finish_reason":null}]}

data: {"id":"chatcmpl-CANNED123","object":"chat.completion.chunk","model":"gpt-4o-mini-canned","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"b\":5}"}}]},"finish_reason":null}]}

data: {"id":"chatcmpl-CANNED123","object":"chat.completion.chunk","model":"gpt-4o-mini-canned","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: {"id":"chatcmpl-CANNED123","object":"chat.completion.chunk","model":"gpt-4o-mini-canned","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}

data: [DONE]

`

// TestProviderOneConvertsStreamedCompletion drives the real provider_one adapter
// over real HTTP against a canned OpenAI SSE transcript and asserts the canonical
// conversion: text deltas assembled in order, tool-call fragments reassembled with
// the canonical (dotted) name restored, usage folded, provider request id and
// finish reason surfaced, exactly one attempt.
func TestProviderOneConvertsStreamedCompletion(t *testing.T) {
	var sentBody []byte
	var sentAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sentBody, _ = io.ReadAll(r.Body)
		sentAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, cannedStream)
	}))
	defer srv.Close()

	req := baseRequest()
	req.Deadline = time.Now().Add(30 * time.Second) // a real HTTP call needs a live deadline
	req.Tools = []modelbroker.ToolSchema{{
		Name:       "palai.conformance.math.add",
		Parameters: map[string]any{"type": "object", "required": []any{"a", "b"}, "additionalProperties": false},
		Strict:     true,
	}}
	req.ForceToolCall = true

	var texts []string
	res, err := newBroker(t, providerone.Adapter{BaseURL: srv.URL}).
		Route(context.Background(), "provider-one", req, func(d modelbroker.Delta) {
			if d.Text != "" {
				texts = append(texts, d.Text)
			}
		})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if err := res.Validate(); err != nil {
		t.Fatalf("result is not canonical: %v", err)
	}

	// buildBody: the wire request carried the stream flags and the wire-encoded tool.
	assertRequestBody(t, sentBody)
	if sentAuth != "Bearer "+sentinelSecret {
		t.Errorf("adapter did not send the redeemed credential in the Authorization header")
	}

	// consume: text deltas assembled in stream order.
	if res.Output != "Hello" {
		t.Errorf("Output = %q, want the assembled %q", res.Output, "Hello")
	}
	if strings.Join(texts, "") != "Hello" {
		t.Errorf("streamed text = %q, want the increments in order", strings.Join(texts, ""))
	}
	if res.ProviderRequestID != "chatcmpl-CANNED123" || res.Model != "gpt-4o-mini-canned" {
		t.Errorf("provider id/model = %q/%q, want the streamed values", res.ProviderRequestID, res.Model)
	}
	if res.FinishReason != "tool_calls" {
		t.Errorf("finish reason = %q, want tool_calls", res.FinishReason)
	}
	if res.Usage.InputTokens != 11 || res.Usage.OutputTokens != 7 || res.Usage.TotalTokens != 18 {
		t.Errorf("usage = %+v, want input=11 output=7 total=18", res.Usage)
	}
	if res.Attempts != 1 {
		t.Errorf("attempts = %d, want exactly 1", res.Attempts)
	}

	// consume + wireToolName: fragments reassembled and the canonical name restored.
	if len(res.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v, want exactly one reassembled call", res.ToolCalls)
	}
	call := res.ToolCalls[0]
	if call.Name != "palai.conformance.math.add" {
		t.Errorf("tool name = %q, want the canonical dotted name restored from the wire", call.Name)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
		t.Fatalf("reassembled arguments %q are not valid JSON: %v", call.Arguments, err)
	}
	if args["a"] != float64(7) || args["b"] != float64(5) {
		t.Errorf("reassembled arguments = %v, want {a:7,b:5}", args)
	}
}

// finalStream is a minimal canned SSE completion (a single text chunk + a usage chunk), enough to
// drive one non-forced continuation call so a test can inspect the request body the adapter sent.
const finalStream = `data: {"id":"chatcmpl-FINAL","object":"chat.completion.chunk","model":"gpt-4o-mini-canned","choices":[{"index":0,"delta":{"role":"assistant","content":"12"},"finish_reason":"stop"}]}

data: {"id":"chatcmpl-FINAL","object":"chat.completion.chunk","model":"gpt-4o-mini-canned","choices":[],"usage":{"prompt_tokens":20,"completion_tokens":1,"total_tokens":21}}

data: [DONE]

`

// TestProviderOneWireEncodesAssistantHistoryToolCallName pins the multi-step continuation contract
// (E12 T1b sibling): an assistant turn in the conversation HISTORY carries a tool_call whose name is
// the CANONICAL dotted name. wireMessages must wire-encode it to the provider charset [A-Za-z0-9_-]
// — symmetric with buildBody's tools array — or the provider 400s the continuation. Proven against
// the real API: OpenAI rejects messages[i].tool_calls[].function.name that does not match
// ^[A-Za-z0-9_-]+$. The provider tool_call id (T1b) rides through verbatim on both the assistant turn
// and the tool message, so assistant.tool_calls[].id == the tool message's tool_call_id.
func TestProviderOneWireEncodesAssistantHistoryToolCallName(t *testing.T) {
	var sentBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sentBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, finalStream)
	}))
	defer srv.Close()

	req := baseRequest()
	req.Deadline = time.Now().Add(30 * time.Second)
	// The continuation conversation: an assistant turn that called the tool (canonical dotted name +
	// the provider id) followed by the tool result answering that same id.
	req.Messages = []modelbroker.Message{
		{Role: "user", Content: "add 7 and 5"},
		{Role: "assistant", ToolCalls: []modelbroker.ToolCall{{ID: "call_abc", Name: "palai.conformance.math.add", Arguments: `{"a":7,"b":5}`}}},
		{Role: "tool", ToolCallID: "call_abc", Content: `{"sum":12}`},
	}
	req.Tools = []modelbroker.ToolSchema{{Name: "palai.conformance.math.add", Parameters: map[string]any{"type": "object"}}}

	if _, err := newBroker(t, providerone.Adapter{BaseURL: srv.URL}).
		Route(context.Background(), "provider-one", req, nil); err != nil {
		t.Fatalf("route: %v", err)
	}

	var sent struct {
		Messages []struct {
			Role      string `json:"role"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tool_calls"`
			ToolCallID string `json:"tool_call_id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(sentBody, &sent); err != nil {
		t.Fatalf("adapter sent a body that is not JSON: %v", err)
	}
	var assistant, toolMsg int = -1, -1
	for i, m := range sent.Messages {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			assistant = i
		} else if m.Role == "tool" {
			toolMsg = i
		}
	}
	if assistant < 0 || toolMsg < 0 {
		t.Fatalf("continuation body is missing the assistant tool-call turn or the tool message: %s", sentBody)
	}
	if got := sent.Messages[assistant].ToolCalls[0].Function.Name; got != "palai_conformance_math_add" {
		t.Errorf("assistant-history tool_call name = %q, want the wire-encoded name (OpenAI rejects the dotted name)", got)
	}
	// The provider id threads verbatim (T1b), matching on both turns so the conversation is well-formed.
	if got := sent.Messages[assistant].ToolCalls[0].ID; got != "call_abc" {
		t.Errorf("assistant-history tool_call id = %q, want call_abc (threaded verbatim)", got)
	}
	if got := sent.Messages[toolMsg].ToolCallID; got != "call_abc" {
		t.Errorf("tool message tool_call_id = %q, want call_abc (matches the assistant tool_call id)", got)
	}
}

// TestProviderOneSanitizesHTTPErrorBody drives the adapter against a 429 whose body
// carries provider free-text, and asserts sanitizeError surfaces the stable code
// and HTTP status but drops the free-text (which can echo a key prefix).
func TestProviderOneSanitizesHTTPErrorBody(t *testing.T) {
	const providerText = "Rate limit reached for key sk-REDACTED-abcd on requests"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"`+providerText+`","type":"requests","code":"rate_limit_exceeded"}}`)
	}))
	defer srv.Close()

	req := baseRequest()
	req.Deadline = time.Now().Add(30 * time.Second)
	res, err := newBroker(t, providerone.Adapter{BaseURL: srv.URL}).
		Route(context.Background(), "provider-one", req, nil)
	if err != nil {
		t.Fatalf("a sanitized provider error must ride on the Result, not the Go error: %v", err)
	}
	if res.Error == nil || res.Error.Status != http.StatusTooManyRequests {
		t.Fatalf("Result.Error = %+v, want a sanitized 429", res.Error)
	}
	if res.Error.Code != "rate_limit_exceeded" {
		t.Errorf("error code = %q, want the stable provider code surfaced", res.Error.Code)
	}
	if strings.Contains(res.Error.Message, "Rate limit") || strings.Contains(res.Error.Message, "sk-") {
		t.Errorf("sanitized error leaked provider free-text: %q", res.Error.Message)
	}
}

func assertRequestBody(t *testing.T, body []byte) {
	t.Helper()
	var sent struct {
		Stream        bool `json:"stream"`
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
		ToolChoice string `json:"tool_choice"`
		Tools      []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("adapter sent a body that is not JSON: %v", err)
	}
	if !sent.Stream || !sent.StreamOptions.IncludeUsage {
		t.Errorf("request did not ask for a usage-bearing stream: %s", body)
	}
	if sent.ToolChoice != "required" {
		t.Errorf("forced tool call did not set tool_choice=required, got %q", sent.ToolChoice)
	}
	if len(sent.Tools) != 1 || sent.Tools[0].Function.Name != "palai_conformance_math_add" {
		t.Errorf("tool was not wire-encoded to the provider charset: %s", body)
	}
}
