package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// interruptPollInterval is how often the in-flight-abort watcher checks for a pending interrupt
// while a provider call is outstanding (spec §9.2, §25.11). ponytail: a DB poll during the
// model call; a LISTEN/NOTIFY signal would drop the poll if model-call latency ever makes it
// matter. The watcher only runs for the duration of one provider call.
const interruptPollInterval = 25 * time.Millisecond

// interruptHit is the watcher's verdict for one model call: found reports whether a pending
// interrupt was seen (and canceled the call). kind branches the handler — a send_message folds
// its message, a change_config applies its revision + warns (spec §9.2, §9.3). payload carries
// the raw command content the handler decodes.
type interruptHit struct {
	found     bool
	commandID string
	kind      string
	message   string
	payload   []byte
}

// ModelRoute is the broker coordinates this kernel routes a model.request to: the
// adapter name, the model id put on the provider wire, and the SecretRef the executor
// redeems at call time. ponytail: env-selected in main.go
// (PALAI_MODEL_PROVIDER/PALAI_MODEL) until DB-backed model_routes routing is wired —
// that lookup is the deferred E-series carve-out. The default is the deterministic
// fake provider every existing suite registers its adapter and resolver under.
type ModelRoute struct {
	Provider string
	Model    string
	Secret   modelbroker.SecretRef
}

var defaultModelRoute = ModelRoute{Provider: "fake", Model: "fake", Secret: modelbroker.SecretRef("model")}

// dispatchModel handles a model.request. A committed result for the stable
// model_request_id is replayed without re-calling the provider (the DB half of
// cross-attempt dedup, spec §53.4). Otherwise the request is persisted (row + event)
// BEFORE the provider is called (spec §24.7 order), the result is committed, and only
// then is model.result delivered (commit-before-deliver). Provider tool calls cross
// the engine boundary as objects, never the raw JSON string (spec §25.9).
//
// It reports whether the result carries tool calls: a result with tool calls means the run
// continues to another model step, so the command pump has a next input boundary to deliver a
// queued/steered message into. A final result (no tool calls) is the run's last step.
func (o *Orchestrator) dispatchModel(ctx context.Context, st *attemptState, frame contracts.EngineFrame) (bool, error) {
	requestID, _ := frame.Data["model_request_id"].(string)

	if stored, found, err := o.spine.LookupModelResult(ctx, st.tenant, requestID); err != nil {
		return false, err
	} else if found {
		var data map[string]any
		if err := json.Unmarshal(stored, &data); err != nil {
			return false, fmt.Errorf("replay model result %s: %w", requestID, err)
		}
		// The committed result carries the used model, so the replay branch fills the
		// terminal projection's model without re-routing (spec §53.4).
		if model, ok := data["model"].(string); ok {
			st.model = model
		}
		toolCalls, _ := data["tool_calls"].([]any)
		// ponytail: replayed usage is not re-accounted; the crash-recovery path
		// undercounts. Store usage on the row if accurate recovered metering matters.
		return len(toolCalls) > 0, st.ch.Send(ctx, o.frame(st, "model.result", data, string(frame.ID)))
	}

	messages, err := decodeMessages(frame.Data["messages"])
	if err != nil {
		return false, fmt.Errorf("model request %s: %w", requestID, err)
	}

	// Resolve the per-step effective model from the session's config revisions (spec §9.3,
	// §25.16). A normal switch's revision applied at the previous boundary, and an immediate
	// switch's applied at the interrupt boundary, so this step routes under the current config;
	// the provider stays env-selected (E06 §7.3 carve-out) — only the model id moves.
	effectiveModel, err := o.effectiveModel(ctx, st)
	if err != nil {
		return false, err
	}

	requestEvent, _ := json.Marshal(map[string]any{"run_id": st.attempt.RunID, "model_request_id": requestID})
	if err := o.spine.CommitModelRequest(ctx, st.tenant, st.sessionID, st.responseID, string(st.attempt.RunID), requestID, eventModelStepCreated, requestEvent); err != nil {
		return false, err
	}

	// Watch for an interrupt while the provider call is outstanding: an interrupt command
	// cancels this call's context (the §25.11 in-flight-abort controller half). The watcher
	// runs only for this call and always reports exactly once on modelCtx being canceled.
	modelCtx, cancelModel := context.WithCancel(ctx)
	defer cancelModel()
	hitCh := make(chan interruptHit, 1)
	go o.watchInterrupt(modelCtx, st, cancelModel, hitCh)

	// Accumulate the streamed-so-far text so an interrupt can record it as the explicit partial
	// item (spec §25.16), not None. onDelta runs synchronously inside Route on this goroutine,
	// so a plain builder needs no lock.
	var partial strings.Builder
	onDelta := func(d modelbroker.Delta) {
		if d.Text != "" {
			partial.WriteString(d.Text)
		}
	}

	result, err := o.models.Route(modelCtx, o.route.Provider, modelbroker.Request{
		ModelRequestID: contracts.ModelRequestID(requestID),
		// Stable across attempts: the same run and model-request identity re-derive the
		// same key, so a reclaimed attempt that re-routes carries it and the provider
		// settles one effect (spec §53.4, §35.3).
		IdempotencyKey: string(st.attempt.RunID) + "/" + requestID,
		Model:          effectiveModel,
		Messages:       messages,
		// A ChildRun runs under its parent-intersected budget (spec §25.18); a root run stays
		// unbounded here (0). Enforcement is the broker's Reservation.Admit at settle.
		Reservation: modelbroker.Reservation{MaxTotalTokens: st.childBudget},
		Secret:      o.route.Secret,
	}, onDelta)
	cancelModel()
	hit := <-hitCh
	// An interrupt that actually aborted the in-flight call (canceled ctx) ends this step
	// partial and resumes in a new one, folding a message or applying the new config (spec §9.2,
	// §9.3). An interrupt that raced a normal return leaves the command queued for a boundary.
	if hit.found && errors.Is(err, context.Canceled) {
		return false, o.handleInterrupt(ctx, st, frame, requestID, hit, partial.String())
	}
	if err != nil {
		return false, fmt.Errorf("route model request %s: %w", requestID, err)
	}
	st.usage = addUsage(st.usage, result.Usage)
	st.model = result.Model

	toolCalls, err := toEngineToolCalls(result.ToolCalls)
	if err != nil {
		// A tool-call arguments string that is not a JSON object is a provider fault,
		// sanitized here; the raw string never reaches the engine (spec §25.9).
		return false, fmt.Errorf("model request %s: provider_error: %w", requestID, err)
	}

	data := map[string]any{"model_request_id": requestID, "model": result.Model}
	// Persist the provider's own request id (a chatcmpl-... for provider-one, the fake id
	// for the deterministic adapter). It is safe, non-secret correlation evidence — the UAT
	// reads it back from the committed result for the live-round-trip receipt.
	if result.ProviderRequestID != "" {
		data["provider_request_id"] = result.ProviderRequestID
	}
	if result.Output != "" {
		data["output"] = result.Output
	}
	if len(toolCalls) > 0 {
		data["tool_calls"] = toolCalls
	}

	stored, _ := json.Marshal(data)
	resultEvent, _ := json.Marshal(map[string]any{"run_id": st.attempt.RunID, "model_request_id": requestID})
	if _, err := o.spine.CommitModelResult(ctx, st.tenant, st.sessionID, st.responseID, string(st.attempt.RunID), requestID, stored, eventModelStepCompleted, resultEvent); err != nil {
		return false, err
	}
	return len(toolCalls) > 0, st.ch.Send(ctx, o.frame(st, "model.result", data, string(frame.ID)))
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

// effectiveModel resolves the model id this step routes under (spec §9.3, §25.16). A session
// config revision's model wins; absent one, the deployment default. Only the model id moves —
// the provider and its credential ref stay env-selected (E06 §7.3 carve-out).
func (o *Orchestrator) effectiveModel(ctx context.Context, st *attemptState) (string, error) {
	// A ChildRun routes its own model id (spec §25.18): the cheaper/alias model the delegation
	// asked for, within the same env-selected provider (E06 §7.3 carve-out). It wins over the
	// session/deployment default so a child can run a different model than its parent.
	if st.childModel != "" {
		return st.childModel, nil
	}
	override, found, err := o.spine.LatestSessionConfig(ctx, st.tenant, st.sessionID)
	if err != nil {
		return "", err
	}
	if found && override.Model != "" {
		return override.Model, nil
	}
	return o.route.Model, nil
}

// watchInterrupt polls for a pending interrupt while a model call is outstanding and cancels
// the call if one arrives (the §25.11 in-flight-abort controller half). It reports exactly one
// interruptHit when modelCtx ends: found when it aborted the call, empty when the caller
// canceled it because the call returned first. The poll uses modelCtx, so a normal return that
// cancels it makes any in-flight poll fall through to the done branch.
func (o *Orchestrator) watchInterrupt(ctx context.Context, st *attemptState, cancel context.CancelFunc, out chan<- interruptHit) {
	ticker := time.NewTicker(interruptPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			out <- interruptHit{}
			return
		case <-ticker.C:
			pending, found, err := o.spine.PendingInterruptCommand(ctx, st.tenant, string(st.attempt.RunID))
			if err == nil && found {
				message := ""
				if pending.Kind == "send_message" {
					message = decodeMessage(pending.Payload)
				}
				out <- interruptHit{found: true, commandID: pending.CommandID, kind: pending.Kind, message: message, payload: pending.Payload}
				cancel()
				return
			}
		}
	}
}

// handleInterrupt ends an interrupt-aborted model step and resumes the run in a new one. It
// records the partial step — carrying the streamed-so-far output as the explicit partial item
// (spec §25.16), not None — applies the interrupt command atomically, then tells the engine the
// step was interrupted so it re-requests the model. A send_message folds its delivered message
// into the new step; a change_config applies its revision so the new step routes under the new
// config, plus a warning (spec §9.2, §9.3). The synthetic model.result is ALWAYS sent once the
// call was aborted — otherwise the engine, still awaiting a result, would hang — and carries the
// partial output so the engine records it as the partial assistant turn. A command a boundary
// already applied (the raced degraded path) skips the settle but still resumes the engine.
func (o *Orchestrator) handleInterrupt(ctx context.Context, st *attemptState, frame contracts.EngineFrame, requestID string, hit interruptHit, partialOutput string) error {
	partialData := map[string]any{"run_id": st.attempt.RunID, "model_request_id": requestID}
	if partialOutput != "" {
		partialData["output"] = partialOutput
	}
	partial, _ := json.Marshal(partialData)

	if hit.kind == "change_config" {
		if err := o.applyImmediateConfigChange(ctx, st, hit, partial); err != nil {
			return err
		}
	} else {
		// The aborted step's model_request_id keys the durable interrupt-delivered row (spec §26.9,
		// ENG-012): a reconstructing attempt refolds the message at THIS same boundary, interleaved with
		// any boundary-delivered message by applied_sequence.
		switch _, err := o.spine.InterruptModelStep(ctx, st.tenant, st.sessionID, st.responseID, string(st.attempt.RunID), hit.commandID, requestID, eventModelStepInterrupted, partial); {
		case errors.Is(err, coordinator.ErrCommandNotPending):
			// Already applied by a boundary; the message is delivered — just resume the engine.
		case err != nil:
			return err
		default:
			deliver := o.frame(st, "message.deliver", map[string]any{"command_id": hit.commandID, "delivery": "interrupt", "message": hit.message}, "")
			if err := st.ch.Send(ctx, deliver); err != nil {
				return err
			}
		}
	}

	resultData := map[string]any{"model_request_id": requestID, "interrupted": true}
	if partialOutput != "" {
		resultData["output"] = partialOutput
	}
	interrupted := o.frame(st, "model.result", resultData, string(frame.ID))
	return st.ch.Send(ctx, interrupted)
}

// applyImmediateConfigChange settles an immediate config switch that aborted the in-flight step
// (spec §9.3, §25.16): it resolves the new ConfigSnapshot, records the partial step, applies the
// change (the config revision + config.revised.v1), and raises a warning — atomically. The
// resumed model step then re-resolves and routes under the new config. A change a boundary
// already applied returns ErrCommandNotPending and is a no-op.
func (o *Orchestrator) applyImmediateConfigChange(ctx context.Context, st *attemptState, hit interruptHit, partial []byte) error {
	plan, err := o.planConfigChange(ctx, st, hit.commandID, hit.payload)
	if err != nil {
		return err
	}
	warning, _ := json.Marshal(map[string]any{"command_id": hit.commandID, "code": "config_switch_interrupted", "detail": "the in-flight model step was interrupted for an immediate config switch"})
	switch _, err := o.spine.InterruptForConfigChange(ctx, st.tenant, st.sessionID, st.responseID, string(st.attempt.RunID), plan, hit.commandID, eventModelStepInterrupted, partial, eventWarningRaised, warning); {
	case errors.Is(err, coordinator.ErrCommandNotPending):
		return nil // already applied by a boundary; the resumed step still picks up the config
	default:
		return err
	}
}
