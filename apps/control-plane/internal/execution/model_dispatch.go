package execution

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// modelProvider and modelSecret are the deterministic broker coordinates this kernel
// routes to. ponytail: hardcoded until model routing (provider + credential from
// model_routes) is wired; the runner-gateway task (11c) revisits this with real
// routing. The e2e broker registers its adapter and resolver under these names.
const (
	modelProvider = "fake"
	modelSecret   = modelbroker.SecretRef("model")
)

// dispatchModel handles a model.request: it persists the model result and only then
// delivers model.result to the engine (commit-before-deliver, spec §24.7). The tool
// calls the provider returns cross the engine boundary as objects, not the provider's
// raw JSON string (spec §25.9).
func (o *Orchestrator) dispatchModel(ctx context.Context, st *attemptState, frame contracts.EngineFrame) error {
	requestID, _ := frame.Data["model_request_id"].(string)

	messages, err := decodeMessages(frame.Data["messages"])
	if err != nil {
		return fmt.Errorf("model request %s: %w", requestID, err)
	}

	result, err := o.models.Route(ctx, modelProvider, modelbroker.Request{
		ModelRequestID: contracts.ModelRequestID(requestID),
		Model:          modelProvider,
		Messages:       messages,
		Reservation:    modelbroker.Reservation{},
		Secret:         modelSecret,
	}, nil)
	if err != nil {
		return fmt.Errorf("route model request %s: %w", requestID, err)
	}
	st.usage = addUsage(st.usage, result.Usage)

	toolCalls, err := toEngineToolCalls(result.ToolCalls)
	if err != nil {
		// A tool-call arguments string that is not a JSON object is a provider fault,
		// sanitized here; the raw string never reaches the engine (spec §25.9).
		return fmt.Errorf("model request %s: provider_error: %w", requestID, err)
	}

	data := map[string]any{"model_request_id": requestID}
	if result.Output != "" {
		data["output"] = result.Output
	}
	if len(toolCalls) > 0 {
		data["tool_calls"] = toolCalls
	}

	payload, _ := json.Marshal(map[string]any{"run_id": st.attempt.RunID, "model_request_id": requestID})
	if _, err := o.spine.CommitModelResult(ctx, st.tenant, st.sessionID, "run.model_result.v1", payload); err != nil {
		return err
	}
	return st.ch.Send(ctx, o.frame(st, "model.result", data, string(frame.ID)))
}

// decodeMessages converts the engine's assembled conversation into canonical model
// messages. The engine carries tool-call arguments and content as JSON objects, while
// the canonical message shape carries both as strings, so this is the inbound half of
// the same string/object boundary toEngineToolCalls owns outbound (spec §25.9).
func decodeMessages(raw any) ([]modelbroker.Message, error) {
	items, ok := raw.([]any)
	if !ok {
		if raw == nil {
			return nil, nil
		}
		return nil, fmt.Errorf("messages is not an array")
	}
	out := make([]modelbroker.Message, 0, len(items))
	for _, item := range items {
		fields, ok := item.(map[string]any)
		if !ok {
			continue
		}
		msg := modelbroker.Message{Content: asJSONString(fields["content"])}
		msg.Role, _ = fields["role"].(string)
		msg.ToolCallID, _ = fields["tool_call_id"].(string)
		if calls, ok := fields["tool_calls"].([]any); ok {
			for _, raw := range calls {
				call, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				id, _ := call["id"].(string)
				name, _ := call["name"].(string)
				msg.ToolCalls = append(msg.ToolCalls, modelbroker.ToolCall{ID: id, Name: name, Arguments: asJSONString(call["arguments"])})
			}
		}
		out = append(out, msg)
	}
	return out, nil
}

// asJSONString keeps a string value as-is and serializes any other JSON value (object,
// number, null) to its compact JSON, so a canonical string field never rejects the
// object shapes the engine uses for content and arguments.
func asJSONString(v any) string {
	switch value := v.(type) {
	case nil:
		return ""
	case string:
		return value
	default:
		encoded, _ := json.Marshal(value)
		return string(encoded)
	}
}

// toEngineToolCalls resolves each provider tool call's arguments — a JSON string — to
// an object exactly once. This is the single boundary where the string becomes the
// object the engine wire (and engine.schema.json $defs/tool_call) requires.
func toEngineToolCalls(calls []modelbroker.ToolCall) ([]contracts.ToolCall, error) {
	if len(calls) == 0 {
		return nil, nil
	}
	out := make([]contracts.ToolCall, 0, len(calls))
	for _, c := range calls {
		args := map[string]any{}
		if c.Arguments != "" {
			if err := json.Unmarshal([]byte(c.Arguments), &args); err != nil {
				return nil, fmt.Errorf("tool call %q arguments are not a JSON object", c.Name)
			}
		}
		out = append(out, contracts.ToolCall{Name: c.Name, Arguments: args})
	}
	return out, nil
}

func addUsage(a, b contracts.Usage) contracts.Usage {
	return contracts.Usage{
		InputTokens:  a.InputTokens + b.InputTokens,
		OutputTokens: a.OutputTokens + b.OutputTokens,
		TotalTokens:  a.TotalTokens + b.TotalTokens,
		ToolCalls:    a.ToolCalls + b.ToolCalls,
	}
}
