// Package providertwo is the provider-two (Anthropic) model adapter — a second,
// INDEPENDENT direct provider family at the same broker contract as provider-one.
// It speaks the Anthropic Messages streaming API as plain HTTPS + SSE over the
// standard library (no provider SDK) and folds the streamed events into a canonical
// modelbroker.Result: text deltas, tool requests, the real provider message id and
// model, usage, and a sanitized error. The credential is used only for the x-api-key
// header of a single request; it is never retried, logged, or placed in the result.
// Hidden retry is off by construction (net/http does not retry), so every call is
// exactly one attempt (spec §53.4). Nothing here imports provider-one — the two
// families are independent by construction.
package providertwo

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// DefaultBaseURL is the Anthropic Messages endpoint.
const DefaultBaseURL = "https://api.anthropic.com/v1/messages"

// anthropicVersion is the required Messages API version header.
const anthropicVersion = "2023-06-01"

// defaultMaxTokens is the output ceiling sent when the adapter has none set.
// Anthropic REQUIRES max_tokens (OpenAI does not); the canonical Request carries
// no such field, so the adapter owns a default. ponytail: a fixed default; set
// Adapter.MaxTokens to override — a per-request ceiling belongs to routing (E16 T6),
// not the wire adapter.
const defaultMaxTokens = 4096

const maxSSELineBytes = 1 << 20 // one MiB, matching the engine frame ceiling

// Adapter converts a canonical request into an Anthropic streaming message.
type Adapter struct {
	BaseURL   string       // defaults to DefaultBaseURL
	Client    *http.Client // defaults to a no-retry client
	MaxTokens int          // defaults to defaultMaxTokens
}

// Execute performs one streaming message completion and returns the canonical result.
func (a Adapter) Execute(ctx context.Context, req modelbroker.Request, secret string, onDelta func(modelbroker.Delta)) (modelbroker.Result, error) {
	if !req.Deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, req.Deadline)
		defer cancel()
	}

	body, names, err := a.buildBody(req)
	if err != nil {
		return modelbroker.Result{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL(), bytes.NewReader(body))
	if err != nil {
		return modelbroker.Result{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("anthropic-version", anthropicVersion)
	httpReq.Header.Set("x-api-key", secret) // the sole use of the credential

	resp, err := a.client().Do(httpReq)
	if err != nil {
		return modelbroker.Result{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return modelbroker.Result{
			ModelRequestID: req.ModelRequestID,
			Model:          req.Model,
			Attempts:       1,
			Error:          sanitizeError(resp),
		}, nil
	}

	return a.consume(req, resp.Body, names, onDelta)
}

func (a Adapter) baseURL() string {
	if a.BaseURL != "" {
		return a.BaseURL
	}
	return DefaultBaseURL
}

func (a Adapter) client() *http.Client {
	if a.Client != nil {
		return a.Client
	}
	// A fresh client with no custom transport does not retry; ctx bounds the call.
	return &http.Client{}
}

func (a Adapter) maxTokens() int {
	if a.MaxTokens > 0 {
		return a.MaxTokens
	}
	return defaultMaxTokens
}

// consume reads the Anthropic SSE stream and folds every event into one canonical
// result. names maps each provider wire tool name back to the canonical tool name.
func (a Adapter) consume(req modelbroker.Request, r io.Reader, names map[string]string, onDelta func(modelbroker.Delta)) (modelbroker.Result, error) {
	res := modelbroker.Result{ModelRequestID: req.ModelRequestID, Attempts: 1}
	var output strings.Builder
	tools := newToolAccumulator()
	var inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens int

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxSSELineBytes)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		payload, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue // SSE comments and event: lines carry no JSON envelope
		}
		payload = strings.TrimSpace(payload)
		if payload == "" {
			continue
		}
		var ev event
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			return modelbroker.Result{}, fmt.Errorf("decode stream event: %w", err)
		}
		switch ev.Type {
		case "message_start":
			if ev.Message != nil {
				if ev.Message.ID != "" {
					res.ProviderRequestID = ev.Message.ID
				}
				if ev.Message.Model != "" {
					res.Model = ev.Message.Model
				}
				if ev.Message.Usage != nil {
					inputTokens = ev.Message.Usage.InputTokens
					outputTokens = ev.Message.Usage.OutputTokens
					// Anthropic reports cache-read and cache-creation (write) SEPARATELY from
					// input/output, so they fold into the canonical cache counters without
					// disturbing the base usage invariant (MOD-010).
					cacheReadTokens = ev.Message.Usage.CacheReadInputTokens
					cacheWriteTokens = ev.Message.Usage.CacheCreationInputTokens
				}
			}
		case "content_block_start":
			if ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use" {
				tools.start(ev.Index, ev.ContentBlock.ID, canonicalName(names, ev.ContentBlock.Name))
			}
		case "content_block_delta":
			if ev.Delta == nil {
				continue
			}
			if ev.Delta.Type == "text_delta" && ev.Delta.Text != "" {
				output.WriteString(ev.Delta.Text)
				delta := modelbroker.Delta{Text: ev.Delta.Text}
				res.Deltas = append(res.Deltas, delta)
				if onDelta != nil {
					onDelta(delta)
				}
			}
			if ev.Delta.Type == "input_json_delta" {
				tools.append(ev.Index, ev.Delta.PartialJSON)
				call := tools.at(ev.Index)
				delta := modelbroker.Delta{ToolCall: &modelbroker.ToolCallDelta{
					Index:             ev.Index,
					ID:                call.ID,
					Name:              call.Name,
					ArgumentsFragment: ev.Delta.PartialJSON,
				}}
				res.Deltas = append(res.Deltas, delta)
				if onDelta != nil {
					onDelta(delta)
				}
			}
		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				res.FinishReason = canonicalFinishReason(ev.Delta.StopReason)
			}
			if ev.Usage != nil && ev.Usage.OutputTokens > 0 {
				outputTokens = ev.Usage.OutputTokens // cumulative final output count
			}
		case "error":
			// A mid-stream provider error terminates the message with a sanitized error.
			code := "provider_error"
			if ev.Error != nil && ev.Error.Type != "" {
				code = ev.Error.Type
			}
			res.Error = &modelbroker.SanitizedError{Code: code, Message: "provider stream error"}
			res.FinishReason = "error"
		case "message_stop":
			// End of stream; remaining lines carry nothing.
		}
	}
	if err := scanner.Err(); err != nil {
		return modelbroker.Result{}, fmt.Errorf("read stream: %w", err)
	}

	res.Output = output.String()
	res.ToolCalls = tools.result()
	// Anthropic reports input/output separately and never a total; the canonical
	// contract carries a consistent total (Result.Validate), so derive it.
	// Cross-family asymmetry the schema documents: Anthropic reports cache read/creation DISJOINT
	// from input_tokens, and the derived total below EXCLUDES them, so for THIS family CacheReadTokens
	// and CacheWriteTokens are ADDITIVE to input/total (unlike provider-one, where cache-read is a
	// subset of input). A biller meters them per-provider; total_tokens (hence Reservation) does not
	// count this family's cache tokens.
	res.Usage = contracts.Usage{
		InputTokens: inputTokens, OutputTokens: outputTokens, TotalTokens: inputTokens + outputTokens,
		CacheReadTokens: cacheReadTokens, CacheWriteTokens: cacheWriteTokens,
	}
	if res.Usage.ToolCalls == 0 {
		res.Usage.ToolCalls = len(res.ToolCalls)
	}
	return res, nil
}

// event is one Anthropic Messages SSE event. Every event on the wire carries a
// discriminating "type"; only the fields this adapter reads are decoded.
type event struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	ContentBlock *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Type string `json:"type"`
	} `json:"error"`
}

// toolAccumulator folds streamed tool-use blocks (keyed by content-block index) into
// whole tool requests: the id and canonical name arrive on content_block_start, the
// JSON arguments in partial_json fragments across the input_json_delta events.
type toolAccumulator struct {
	order []int
	byIdx map[int]*modelbroker.ToolCall
}

func newToolAccumulator() *toolAccumulator {
	return &toolAccumulator{byIdx: map[int]*modelbroker.ToolCall{}}
}

func (t *toolAccumulator) start(idx int, id, name string) {
	call, ok := t.byIdx[idx]
	if !ok {
		call = &modelbroker.ToolCall{}
		t.byIdx[idx] = call
		t.order = append(t.order, idx)
	}
	if id != "" {
		call.ID = id
	}
	if name != "" {
		call.Name = name
	}
}

func (t *toolAccumulator) append(idx int, fragment string) {
	call, ok := t.byIdx[idx]
	if !ok {
		call = &modelbroker.ToolCall{}
		t.byIdx[idx] = call
		t.order = append(t.order, idx)
	}
	call.Arguments += fragment
}

func (t *toolAccumulator) at(idx int) modelbroker.ToolCall {
	if call, ok := t.byIdx[idx]; ok {
		return *call
	}
	return modelbroker.ToolCall{}
}

func (t *toolAccumulator) result() []modelbroker.ToolCall {
	if len(t.order) == 0 {
		return nil
	}
	out := make([]modelbroker.ToolCall, 0, len(t.order))
	for _, idx := range t.order {
		out = append(out, *t.byIdx[idx])
	}
	return out
}

// buildBody renders the canonical request as an Anthropic Messages body with
// streaming on. It returns a wire→canonical tool-name map: Anthropic, like OpenAI,
// restricts tool names to [A-Za-z0-9_-], so canonical dotted names are wire-encoded
// and restored on the way back.
func (a Adapter) buildBody(req modelbroker.Request) ([]byte, map[string]string, error) {
	names := map[string]string{}
	system, messages := a.wireMessages(req.Messages)
	body := map[string]any{
		"model":      req.Model,
		"max_tokens": a.maxTokens(),
		"messages":   messages,
		"stream":     true,
	}
	if system != "" {
		body["system"] = system
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			wire := wireToolName(t.Name)
			if existing, dup := names[wire]; dup {
				// Two canonical names encoding to one wire name would silently overwrite the
				// restore map (e.g. "a.b" and "a_b", or a 64-char truncation) — fail loudly.
				return nil, nil, fmt.Errorf("tool name %q collides with %q on the wire (both encode to %q)", t.Name, existing, wire)
			}
			names[wire] = t.Name
			tool := map[string]any{"name": wire, "input_schema": t.Parameters}
			if t.Description != "" {
				tool["description"] = t.Description
			}
			if t.Strict {
				tool["strict"] = true
			}
			tools = append(tools, tool)
		}
		body["tools"] = tools
		if req.ForceToolCall {
			// {"type":"any"} forces the model to use at least one tool (Anthropic's
			// equivalent of OpenAI tool_choice:"required").
			body["tool_choice"] = map[string]any{"type": "any"}
		}
	}
	if req.OutputSchema != nil {
		// Structured output is Anthropic's output_config.format (the deprecated
		// output_format is not used); the schema itself carries the strict constraint.
		body["output_config"] = map[string]any{
			"format": map[string]any{"type": "json_schema", "schema": req.OutputSchema.Schema},
		}
	}
	data, err := json.Marshal(body)
	return data, names, err
}

// wireMessages converts canonical messages into Anthropic's system + messages split.
// system-role turns collect into the top-level system string; tool-role turns become
// user messages carrying a tool_result block; assistant tool calls become tool_use
// content blocks with the wire-encoded name.
func (a Adapter) wireMessages(messages []modelbroker.Message) (string, []map[string]any) {
	var system strings.Builder
	out := make([]map[string]any, 0, len(messages))
	for _, m := range messages {
		switch m.Role {
		case "system":
			if system.Len() > 0 {
				system.WriteByte('\n')
			}
			system.WriteString(m.Content)
		case "tool":
			out = append(out, map[string]any{
				"role": "user",
				"content": []map[string]any{{
					"type":        "tool_result",
					"tool_use_id": m.ToolCallID,
					"content":     m.Content,
				}},
			})
		case "assistant":
			var content []map[string]any
			if m.Content != "" {
				content = append(content, map[string]any{"type": "text", "text": m.Content})
			}
			for _, c := range m.ToolCalls {
				input := json.RawMessage(c.Arguments)
				if len(input) == 0 {
					input = json.RawMessage("{}")
				}
				content = append(content, map[string]any{
					"type":  "tool_use",
					"id":    c.ID,
					"name":  wireToolName(c.Name),
					"input": input,
				})
			}
			out = append(out, map[string]any{"role": "assistant", "content": content})
		default: // "user" and anything else
			out = append(out, map[string]any{
				"role":    "user",
				"content": []map[string]any{{"type": "text", "text": m.Content}},
			})
		}
	}
	return system.String(), out
}

// wireToolName encodes a canonical tool name into the provider's allowed charset
// [A-Za-z0-9_-], bounded to 64 characters (Anthropic's tool-name rule, symmetric with
// provider-one's OpenAI rule — kept local so this family stays independent).
func wireToolName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == '_' || r == '-' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	wire := b.String()
	if len(wire) > 64 {
		wire = wire[:64]
	}
	return wire
}

// canonicalName restores the canonical tool name from its wire encoding, falling
// back to the wire name for anything the request did not declare.
func canonicalName(names map[string]string, wire string) string {
	if wire == "" {
		return ""
	}
	if canonical, ok := names[wire]; ok {
		return canonical
	}
	return wire
}

// canonicalFinishReason maps Anthropic stop reasons onto the broker's canonical
// vocabulary (the one provider-one passes through from OpenAI and the fake uses), so a
// completion reports the SAME finish reason across families: "stop" for a natural end,
// "tool_calls" for a tool request, "length" for a truncation. Anything else (e.g.
// "refusal") passes through unchanged.
func canonicalFinishReason(stop string) string {
	switch stop {
	case "end_turn":
		return "stop"
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length" // Anthropic's truncation reason == OpenAI's "length"
	default:
		return stop
	}
}

// sanitizeError converts a non-200 response into a canonical error that carries the
// HTTP status and the provider's stable error type, but never the provider's free
// text (which can echo a credential prefix) and never the credential itself.
func sanitizeError(resp *http.Response) *modelbroker.SanitizedError {
	code := "provider_error"
	var parsed struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if raw, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024)); err == nil {
		if json.Unmarshal(raw, &parsed) == nil && parsed.Error.Type != "" {
			code = parsed.Error.Type
		}
	}
	return &modelbroker.SanitizedError{
		Code:    code,
		Message: fmt.Sprintf("provider returned HTTP %d", resp.StatusCode),
		Status:  resp.StatusCode,
	}
}
