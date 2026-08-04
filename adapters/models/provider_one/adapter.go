// Package providerone is the provider-one (OpenAI) model adapter. It speaks the
// Chat Completions streaming API as plain HTTPS + SSE over the standard library —
// no provider SDK — and converts the streamed chunks into a canonical
// modelbroker.Result: text deltas, tool requests, the real provider request id and
// model, usage, and a sanitized error. The credential is used only for the
// Authorization header of a single request; it is never retried, logged, or placed
// in the result. Hidden retry is off by construction (net/http does not retry), so
// every call is exactly one attempt (spec §53.4).
package providerone

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// DefaultBaseURL is the OpenAI Chat Completions endpoint.
const DefaultBaseURL = "https://api.openai.com/v1/chat/completions"

const maxSSELineBytes = 1 << 20 // one MiB, matching the engine frame ceiling

// Adapter converts a canonical request into an OpenAI streaming chat completion.
type Adapter struct {
	BaseURL string       // defaults to DefaultBaseURL
	Client  *http.Client // defaults to a no-retry client
}

// Execute performs one streaming chat completion and returns the canonical result.
func (a Adapter) Execute(ctx context.Context, req modelbroker.Request, secret string, onDelta func(modelbroker.Delta)) (modelbroker.Result, error) {
	if !req.Deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, req.Deadline)
		defer cancel()
	}

	body, names, err := buildBody(req)
	if err != nil {
		return modelbroker.Result{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint(req), bytes.NewReader(body))
	if err != nil {
		return modelbroker.Result{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Authorization", "Bearer "+secret) // the sole use of the credential
	if req.IdempotencyKey != "" {
		// Stable across attempts, so a reclaimed retry that re-routes the same request
		// settles one provider effect rather than double-charging (spec §53.4, §35.3).
		httpReq.Header.Set("Idempotency-Key", req.IdempotencyKey)
	}

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

// endpoint resolves where THIS request goes, and the precedence is the point (E29 provider wiring):
// the request's own base URL (the tenant's connection row) beats the adapter's field (the deployment's
// env), which beats the vendor default. A deployment-wide setting must never silently override a
// project's own connection — that is spec §27.7's "a route cannot silently select something else",
// applied to the endpoint rather than to the model.
func (a Adapter) endpoint(req modelbroker.Request) string {
	if req.BaseURL != "" {
		return req.BaseURL
	}
	return a.baseURL()
}

func (a Adapter) client() *http.Client {
	if a.Client != nil {
		return a.Client
	}
	// A fresh client with no custom transport does not retry; ctx bounds the call.
	return &http.Client{}
}

// consume reads the SSE stream and folds every chunk into one canonical result.
// names maps each provider wire tool name back to the canonical tool name.
func (a Adapter) consume(req modelbroker.Request, r io.Reader, names map[string]string, onDelta func(modelbroker.Delta)) (modelbroker.Result, error) {
	res := modelbroker.Result{ModelRequestID: req.ModelRequestID, Attempts: 1}
	var output strings.Builder
	tools := newToolAccumulator()

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxSSELineBytes)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		payload, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue // SSE comments, event: lines, and blanks carry no chunk
		}
		payload = strings.TrimSpace(payload)
		if payload == "[DONE]" {
			break
		}
		var c chunk
		if err := json.Unmarshal([]byte(payload), &c); err != nil {
			return modelbroker.Result{}, fmt.Errorf("decode stream chunk: %w", err)
		}
		if c.ID != "" {
			res.ProviderRequestID = c.ID
		}
		if c.Model != "" {
			res.Model = c.Model
		}
		if c.Usage != nil {
			res.Usage = contracts.Usage{
				InputTokens:  c.Usage.PromptTokens,
				OutputTokens: c.Usage.CompletionTokens,
				TotalTokens:  c.Usage.TotalTokens,
				// OpenAI reports cache-READ under prompt_tokens_details.cached_tokens and no
				// explicit cache-WRITE count (it auto-caches), so the canonical write counter
				// stays 0 for this family — an honest normalization (MOD-010). NOTE the
				// cross-family asymmetry the schema documents: OpenAI FOLDS cached tokens INTO
				// prompt_tokens, so here CacheReadTokens is a SUBSET of InputTokens/TotalTokens
				// (a biller must not add it again). Provider-two reports them DISJOINT.
				CacheReadTokens: c.Usage.PromptTokensDetails.CachedTokens,
			}
		}
		for _, choice := range c.Choices {
			if text := choice.Delta.Content; text != "" {
				output.WriteString(text)
				delta := modelbroker.Delta{Text: text}
				res.Deltas = append(res.Deltas, delta)
				if onDelta != nil {
					onDelta(delta)
				}
			}
			for _, tc := range choice.Delta.ToolCalls {
				tc.Function.Name = canonicalName(names, tc.Function.Name)
				tools.add(tc)
				delta := modelbroker.Delta{ToolCall: &modelbroker.ToolCallDelta{
					Index:             tc.Index,
					ID:                tc.ID,
					Name:              tc.Function.Name,
					ArgumentsFragment: tc.Function.Arguments,
				}}
				res.Deltas = append(res.Deltas, delta)
				if onDelta != nil {
					onDelta(delta)
				}
			}
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				res.FinishReason = *choice.FinishReason
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return modelbroker.Result{}, fmt.Errorf("read stream: %w", err)
	}

	res.Output = output.String()
	res.ToolCalls = tools.result()
	if res.Usage.ToolCalls == 0 {
		res.Usage.ToolCalls = len(res.ToolCalls)
	}
	return res, nil
}

// chunk is one streamed chat.completion.chunk.
type chunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content   string             `json:"content"`
			ToolCalls []toolCallFragment `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		TotalTokens         int `json:"total_tokens"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

type toolCallFragment struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// toolAccumulator folds streamed tool-call fragments (keyed by index) into whole
// tool requests: the id and name arrive on the first fragment, the arguments in
// pieces across the rest.
type toolAccumulator struct {
	order []int
	byIdx map[int]*modelbroker.ToolCall
}

func newToolAccumulator() *toolAccumulator {
	return &toolAccumulator{byIdx: map[int]*modelbroker.ToolCall{}}
}

func (t *toolAccumulator) add(frag toolCallFragment) {
	call, ok := t.byIdx[frag.Index]
	if !ok {
		call = &modelbroker.ToolCall{}
		t.byIdx[frag.Index] = call
		t.order = append(t.order, frag.Index)
	}
	if frag.ID != "" {
		call.ID = frag.ID
	}
	if frag.Function.Name != "" {
		call.Name = frag.Function.Name
	}
	call.Arguments += frag.Function.Arguments
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

// buildBody renders the canonical request as an OpenAI chat completion body with
// usage requested in the stream. It returns a wire→canonical tool-name map: the
// provider restricts function names to [A-Za-z0-9_-], so canonical dotted names
// are encoded on the wire and restored on the way back.
func buildBody(req modelbroker.Request) ([]byte, map[string]string, error) {
	names := map[string]string{}
	body := map[string]any{
		"model":          req.Model,
		"messages":       wireMessages(req.Messages),
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
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
			// A TYPED TOOL CANNOT CROSS THIS ADAPTER, AND IT MUST NOT BE DROPPED QUIETLY. This provider
			// speaks OpenAI's chat/completions shape, where an Anthropic-defined tool type has no
			// meaning. Skipping it would leave the model with no editor and no sign anything was
			// missing — the run would simply stop being able to change a line while looking healthy.
			// Failing here names the route as the thing to fix. Also covers openai_compatible, which
			// embeds this Adapter and shares this builder.
			if t.Type != "" {
				return nil, nil, fmt.Errorf("tool %q declares the Anthropic-defined type %q, which this OpenAI-compatible provider cannot carry; route this agent to an Anthropic model or drop the tool from its set", t.Name, t.Type)
			}
			fn := map[string]any{"name": wire, "parameters": t.Parameters}
			if t.Description != "" {
				fn["description"] = t.Description
			}
			if t.Strict {
				fn["strict"] = true
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
		body["tools"] = tools
		if req.ForceToolCall {
			body["tool_choice"] = "required"
		}
	}
	if req.OutputSchema != nil {
		body["response_format"] = map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   req.OutputSchema.Name,
				"schema": req.OutputSchema.Schema,
				"strict": req.OutputSchema.Strict,
			},
		}
	}
	data, err := json.Marshal(body)
	return data, names, err
}

// wireToolName encodes a canonical tool name into the provider's allowed function
// name charset [A-Za-z0-9_-], bounded to 64 characters.
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

func wireMessages(messages []modelbroker.Message) []map[string]any {
	out := make([]map[string]any, 0, len(messages))
	for _, m := range messages {
		wire := map[string]any{"role": m.Role}
		switch {
		case len(m.Images) > 0:
			// A turn with images renders as the multi-part content array. Only reached when the turn
			// actually carries one, so a text-only run's body stays byte-identical to the pre-vision one.
			wire["content"] = contentParts(m)
		case m.Content != "":
			wire["content"] = m.Content
		}
		if m.ToolCallID != "" {
			wire["tool_call_id"] = m.ToolCallID
		}
		if len(m.ToolCalls) > 0 {
			calls := make([]map[string]any, 0, len(m.ToolCalls))
			for _, c := range m.ToolCalls {
				calls = append(calls, map[string]any{
					"id":   c.ID,
					"type": "function",
					// Wire-encode the history tool name to the provider charset [A-Za-z0-9_-], symmetric
					// with buildBody's tools array — OpenAI 400s an assistant tool_call whose function
					// name (the canonical dotted name) does not match ^[A-Za-z0-9_-]+$, which would
					// break every multi-step continuation (E12 T1b sibling; the deferred symmetry the
					// real provider forced once the id threaded).
					"function": map[string]any{"name": wireToolName(c.Name), "arguments": c.Arguments},
				})
			}
			wire["tool_calls"] = calls
		}
		out = append(out, wire)
	}
	return out
}

// contentParts renders one turn's text + images as the provider's multi-part content array.
//
// CONTRACT: https://developers.openai.com/api/docs/api-reference/chat/create (checked 2026-07-27) — a user
// message's content may be an array whose parts are {"type":"text","text":…} and
// {"type":"image_url","image_url":{"url":…}}, and the url may be a `data:<media-type>;base64,<bytes>` URI.
//
// THE IMAGE GOES AS BYTES, NEVER AS A URL, and this is the security half rather than a style choice: an
// `image_url` pointing at a remote address makes the PROVIDER dereference something chosen upstream of us
// — for a Slack file that means handing OpenAI a files.slack.com address, and any credential the address
// carries with it. The bytes are already control-plane-side (spec §24) by the time a Message exists, so
// there is nothing to gain by re-addressing them.
//
// An empty-worded turn (a file shared with no comment) contributes NO text part: a zero-length text part is
// rejected by the provider, and "" says nothing a missing part does not.
//
// `detail` is deliberately left unset so the endpoint applies its own default. Choosing low/high here would
// be choosing a cost/accuracy tradeoff on the operator's behalf with no way to override it.
func contentParts(m modelbroker.Message) []map[string]any {
	parts := make([]map[string]any, 0, len(m.Images)+1)
	if m.Content != "" {
		parts = append(parts, map[string]any{"type": "text", "text": m.Content})
	}
	for _, img := range m.Images {
		parts = append(parts, map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": dataURL(img)},
		})
	}
	return parts
}

// dataURL encodes one image as a base64 data URI. The media type is the SNIFFED one the caller resolved
// (never a filename or a sender-declared mimetype); an empty one falls back to the octet-stream default
// rather than emitting a malformed `data:;base64,` the endpoint would reject.
func dataURL(img modelbroker.Image) string {
	mediaType := img.MediaType
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(img.Data)
}

// sanitizeError converts a non-200 response into a canonical error that carries the
// HTTP status and the provider's stable error code, but never the provider's free
// text — which can echo a credential prefix — and never the credential itself.
func sanitizeError(resp *http.Response) *modelbroker.SanitizedError {
	code := "provider_error"
	var parsed struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if raw, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024)); err == nil {
		if json.Unmarshal(raw, &parsed) == nil {
			switch {
			case parsed.Error.Code != "":
				code = parsed.Error.Code
			case parsed.Error.Type != "":
				code = parsed.Error.Type
			}
		}
	}
	return &modelbroker.SanitizedError{
		Code:    code,
		Message: fmt.Sprintf("provider returned HTTP %d", resp.StatusCode),
		Status:  resp.StatusCode,
	}
}
