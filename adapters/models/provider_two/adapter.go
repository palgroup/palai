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
	"encoding/base64"
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

// imageMediaTypes is the EXACT set the Messages API accepts inside an image block's base64 source, and it
// is a map rather than a comment because it is NARROWER than what reaches this adapter.
//
// CONTRACT: https://platform.claude.com/docs/en/build-with-claude/vision (checked 2026-08-04) — "Claude
// supports JPEG, PNG, GIF, and WebP images (image/jpeg, image/png, image/gif, image/webp)."
//
// WHY THE NARROWNESS MATTERS HERE. execution's resolver admits any media type with an `image/` prefix
// (model_dispatch.go's imageResolver), and every one of those types was produced by http.DetectContentType,
// whose sniff table ALSO emits image/bmp and image/vnd.microsoft.icon. Both would reach this adapter and
// both are outside the set above, so an unaccepted format is a real inbound case rather than a defensive
// one — see imageContent for what happens to it.
var imageMediaTypes = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/gif":  true,
	"image/webp": true,
}

// unreadableImageNote stands in for an image whose format this provider does not accept, and
// unplaceableImageNote for one attached to a turn whose Anthropic shape has nowhere to put an image.
//
// THEY EXIST BECAUSE THE ALTERNATIVE IS THE SILENT WRONG ANSWER. Until 2026-08-04 this adapter refused
// EVERY request carrying an image rather than send one, and the refusal's own reasoning was that dropping
// the picture and answering the text alone tells a user the model cannot see something it was never shown
// — the shape §27.5 exists to refuse. Sending images does not retire that reasoning, it narrows what it
// applies to: an image this family still cannot put on the wire is MARKED, never dropped, so the model
// reads that something was withheld and can say so. It is the same choice execution already made for the
// images IT cannot carry (missingImageNote, droppedImageNote, oversizeImageNote).
const (
	unreadableImageNote  = "(an image in this conversation is in a format this model cannot read)"
	unplaceableImageNote = "(an image attached to a non-user turn in this conversation is not shown here)"
)

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
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint(req), bytes.NewReader(body))
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
			Error:          sanitizeError(resp, secret),
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
			// AN ANTHROPIC-DEFINED TOOL IS DECLARED, NOT DESCRIBED. Its type names a schema the model
			// already carries, so the request sends {type, name} and NO input_schema — sending one would
			// be a different request shape than the one the tool's behaviour was trained against. The
			// description is omitted for the same reason: the platform does not get to restate what a
			// tool the model already knows does.
			if t.Type != "" {
				if len(t.Parameters) > 0 {
					return nil, nil, fmt.Errorf("tool %q declares both a type (%q) and an input schema; an Anthropic-defined tool carries neither schema nor description", t.Name, t.Type)
				}
				tools = append(tools, map[string]any{"type": t.Type, "name": wire})
				continue
			}
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
//
// IMAGES RIDE THE USER TURN AND ONLY THE USER TURN. That is not a shortcut around the other three
// branches, it is Anthropic's own shape: the top-level `system` field is text, an assistant turn's blocks
// are what the MODEL produced (writing an image into one would be putting words in its mouth), and a
// tool_result carrying an image would be a wire shape no round trip in this tree has ever exercised —
// which is exactly the "unproven shape" this family refused to guess at before it could see anything.
// Images on those turns are counted and reported once in the system text (unplaceableImageNote); the
// user turn — the one the operator's screenshot actually arrives on — carries the real blocks.
// isToolResultTurn reports whether an already-built wire message is a user turn carrying tool results,
// and is therefore one more result may be appended to.
//
// IT CHECKS THE FIRST BLOCK'S TYPE rather than only the role, because a user turn is also how an
// operator's own text and images reach the model. Appending a tool_result onto one of those would put
// a machine answer inside a human's message.
func isToolResultTurn(m map[string]any) bool {
	if m["role"] != "user" {
		return false
	}
	blocks, ok := m["content"].([]map[string]any)
	if !ok || len(blocks) == 0 {
		return false
	}
	return blocks[0]["type"] == "tool_result"
}

func (a Adapter) wireMessages(messages []modelbroker.Message) (string, []map[string]any) {
	var system strings.Builder
	out := make([]map[string]any, 0, len(messages))
	unplaceable := 0
	for _, m := range messages {
		switch m.Role {
		case "system":
			unplaceable += len(m.Images)
			if system.Len() > 0 {
				system.WriteByte('\n')
			}
			system.WriteString(m.Content)
		case "tool":
			unplaceable += len(m.Images)
			block := map[string]any{
				"type":        "tool_result",
				"tool_use_id": m.ToolCallID,
				"content":     m.Content,
			}
			// ADJACENT TOOL RESULTS SHARE ONE USER TURN, AND THAT IS A PROVIDER CONTRACT RATHER THAN A
			// TIDINESS PREFERENCE. Anthropic's parallel-tool-use documentation requires every result for a
			// turn to come back in a single user message, and names the price of splitting them: it
			// "silently trains Claude to stop making parallel calls". One message per result — which is
			// what this arm did — teaches the model, one turn at a time, to stop asking for several tools
			// at once, so the platform never sees the batching it would need in order to run them
			// concurrently.
			//
			// ONLY ADJACENT ONES MERGE. A user turn between two results means they answered different
			// model turns; folding those together would rewrite the conversation's shape rather than
			// repair its packaging.
			if n := len(out); n > 0 && isToolResultTurn(out[n-1]) {
				blocks, _ := out[n-1]["content"].([]map[string]any)
				out[n-1]["content"] = append(blocks, block)
				continue
			}
			out = append(out, map[string]any{"role": "user", "content": []map[string]any{block}})
		case "assistant":
			unplaceable += len(m.Images)
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
			if len(m.Images) == 0 {
				// THE REGRESSION GUARD: a turn carrying no image renders exactly as it did before this
				// family could see one, so every text-only run's request body is byte-identical to the
				// pre-vision one. It is also why the images ride a separate field rather than a re-typed
				// Content — this branch cannot tell the difference.
				out = append(out, map[string]any{
					"role":    "user",
					"content": []map[string]any{{"type": "text", "text": m.Content}},
				})
				continue
			}
			blocks, unreadable := imageContent(m.Images)
			// IMAGES FIRST, then the words. The vision guide's own advice (checked 2026-08-04): "Claude
			// works best when images come before text." Text after images still performs well, so this is
			// a preference rather than a requirement — but it is free to honor.
			content := blocks
			text := m.Content
			if unreadable > 0 {
				// One note per turn, not per image: repeating an identical sentence four times tells the
				// model nothing the first one did not.
				if text != "" {
					text += "\n"
				}
				text += unreadableImageNote
			}
			if text != "" {
				// An empty text block is REJECTED by the endpoint, and an image-only turn is the ordinary
				// case here — a file shared with no comment. "" says nothing a missing block does not.
				content = append(content, map[string]any{"type": "text", "text": text})
			}
			out = append(out, map[string]any{"role": "user", "content": content})
		}
	}
	if unplaceable > 0 {
		if system.Len() > 0 {
			system.WriteByte('\n')
		}
		system.WriteString(unplaceableImageNote)
	}
	return system.String(), out
}

// imageContent renders a turn's images as Anthropic image content blocks, returning the blocks plus a
// COUNT of the images it could not encode, so the caller can say so rather than drop them silently.
//
// CONTRACT: https://platform.claude.com/docs/en/build-with-claude/vision (checked 2026-08-04) — an image
// is {"type":"image","source":{"type":"base64","media_type":<one of imageMediaTypes>,"data":<base64>}}.
//
// THE BYTES, NEVER A URL, and that half is security rather than style. The same endpoint also accepts
// {"type":"url","url":…}, which would make ANTHROPIC dereference an address chosen upstream of us — for a
// Slack screenshot that means handing a third party a files.slack.com address and whatever credential the
// address carries. The bytes are already control-plane-side by the time a Message exists (spec §24), so a
// URL source buys nothing and leaks. Symmetric with provider-one's dataURL for the same reason, and kept
// local so the two families stay independent.
//
// PROVIDER CEILINGS THIS DOES NOT RE-IMPOSE, because execution's are already stricter (measured
// 2026-08-04 against the vision guide): Anthropic takes 10 MB of BASE64 per image on the first-party API
// and execution.maxImageBytes caps one image at 5 MiB DECODED, which encodes to ~7.0 MB; Anthropic takes
// 100 images per request on a 200k-context model (600 on wider ones) and execution.maxRunImages caps a
// whole conversation at 8, which is also far under the 20-image threshold above which a stricter
// per-image dimension limit applies. The one provider ceiling nothing here checks is the 8000x8000 px
// maximum — reading it means decoding a header per image and stdlib has no WebP decoder, and an image
// past it comes back as a sanitized 400 (invalid_request_error), which is loud rather than wrong.
func imageContent(images []modelbroker.Image) (blocks []map[string]any, unreadable int) {
	blocks = make([]map[string]any, 0, len(images))
	for _, img := range images {
		if !imageMediaTypes[img.MediaType] {
			// Counted, not named: MediaType is sniffed from third-party bytes, and echoing it into the
			// conversation would put attacker-influenced text in front of the model for no gain.
			unreadable++
			continue
		}
		blocks = append(blocks, map[string]any{
			"type": "image",
			"source": map[string]any{
				"type":       "base64",
				"media_type": img.MediaType,
				"data":       base64.StdEncoding.EncodeToString(img.Data),
			},
		})
	}
	return blocks, unreadable
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

// sanitizeError converts a non-200 response into a canonical error carrying the HTTP status, the
// provider's stable error type, AND — redacted — the provider's own explanation of what was wrong.
//
// THE EXPLANATION USED TO BE THROWN AWAY, and on 2026-08-04 that cost an hour of live debugging. A run
// carrying an image failed with `provider_error: provider returned HTTP 400 (code
// invalid_request_error, status 400)` once per second, forever. Anthropic had said exactly which field
// was invalid, in the response body, and this function dropped that sentence on the floor: the operator
// was left with a code that every malformed provider-two request in existence shares.
//
// WHY DROPPING IT WAS DEFENSIBLE, AND WHY CARRYING IT IS BETTER. The reason was real: a provider's free
// text can echo a credential prefix back at you, and this string reaches a log. The answer is to REDACT
// rather than to discard, because "no information" is not actually the safe choice — it just moves the
// cost from a hypothetical leak to a certain outage. redactCredential below is given the credential this
// very call used, so an echo is removed by construction rather than by hoping the provider is careful.
//
// The caller boundary is unchanged: orchestrator's terminalProblems still refuses to put this text in a
// caller-facing Problem, because it is the DEPLOYMENT's operator who needs it, not a tenant.
func sanitizeError(resp *http.Response, secret string) *modelbroker.SanitizedError {
	code := "provider_error"
	detail := ""
	var parsed struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if raw, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024)); err == nil {
		if json.Unmarshal(raw, &parsed) == nil {
			if parsed.Error.Type != "" {
				code = parsed.Error.Type
			}
			detail = redactCredential(parsed.Error.Message, secret)
		}
	}
	message := fmt.Sprintf("provider returned HTTP %d", resp.StatusCode)
	if detail != "" {
		message += ": " + detail
	}
	return &modelbroker.SanitizedError{Code: code, Message: message, Status: resp.StatusCode}
}

// maxProviderDetail bounds how much provider text is carried into a log line. Anthropic's validation
// messages are one sentence naming a JSON path; anything far longer is not an explanation.
const maxProviderDetail = 400

// redactCredential removes anything that could be the credential from provider text, and it is
// deliberately paranoid in two independent ways rather than trusting either one.
//
//   - ANY 8-character run of the SECRET ITSELF is removed. A provider that echoes a credential
//     typically echoes a PREFIX ("...api key sk-ant-api03-Xy7…"), which an exact-match comparison would
//     sail straight past. Scanning every 8-char window of the real credential catches a prefix, a
//     suffix, or any fragment, without ever needing to know which form the provider chose.
//   - ANY `sk-`-prefixed token is removed even if it matches nothing we hold, because the message may
//     quote a DIFFERENT key — an operator pasting the wrong secret is exactly when this fires.
//
// The window is 8 rather than 4 so that ordinary English and JSON paths cannot collide with it by
// accident; a key fragment shorter than 8 characters is not a usable credential.
func redactCredential(text, secret string) string {
	const window = 8
	if text == "" {
		return ""
	}
	for i := 0; i+window <= len(secret); i++ {
		if fragment := secret[i : i+window]; strings.Contains(text, fragment) {
			text = strings.ReplaceAll(text, fragment, "[redacted]")
		}
	}
	for {
		start := strings.Index(text, "sk-")
		if start < 0 {
			break
		}
		end := start + 3
		for end < len(text) && (text[end] == '-' || text[end] == '_' ||
			(text[end] >= 'a' && text[end] <= 'z') || (text[end] >= 'A' && text[end] <= 'Z') ||
			(text[end] >= '0' && text[end] <= '9')) {
			end++
		}
		text = text[:start] + "[redacted]" + text[end:]
	}
	text = strings.Join(strings.Fields(text), " ") // a log line is one line
	if len(text) > maxProviderDetail {
		text = text[:maxProviderDetail] + "…"
	}
	return text
}
