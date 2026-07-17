// Package modelbroker routes a canonical model request to a provider adapter and
// returns a canonical result. It owns the request/result contract every adapter
// converts to (spec §25.9 stable request IDs, §53.4 single retry owner). The
// credential never rides on the request: the broker resolves a SecretRef name and
// only the executor redeems it (broker.go). Provider-specific wire handling lives
// in the adapters (adapters/models/*).
package modelbroker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/palgroup/palai/packages/contracts"
)

// SecretRef names the credential the executor redeems for a call. It is an opaque
// name only; the credential value is never carried on the request.
type SecretRef string

// PrivacyFlags carry the caller's data-handling constraints to the provider.
type PrivacyFlags struct {
	NoRetain bool `json:"no_retain,omitempty"`
	NoTrain  bool `json:"no_train,omitempty"`
}

// Message is one conversation turn sent to the provider.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolSchema is a function tool the model may call; Parameters is a JSON Schema.
type ToolSchema struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
	Strict      bool           `json:"strict,omitempty"`
}

// OutputSchema constrains the final output to a JSON Schema (structured output).
type OutputSchema struct {
	Name   string         `json:"name"`
	Schema map[string]any `json:"schema"`
	Strict bool           `json:"strict,omitempty"`
}

// Request is the canonical model call the broker routes to a provider. It carries
// the logical request id, route revision, model step id, deadline, privacy flags
// and budget reservation — never the credential value.
type Request struct {
	ModelRequestID contracts.ModelRequestID `json:"model_request_id"`
	// IdempotencyKey is the provider-facing dedup key, stable across attempts of one
	// logical request (spec §35.3 at-least-once + idempotent effect, §53.4). The executor
	// derives it from the run and model-request identity and the adapter forwards it, so a
	// reclaimed attempt that re-routes the same request produces exactly one provider
	// effect even when the crash window between routing and commit re-opens the call.
	IdempotencyKey string        `json:"idempotency_key,omitempty"`
	RouteRevision  int           `json:"route_revision"`
	ModelStepID    string        `json:"model_step_id"`
	Model          string        `json:"model"`
	Messages       []Message     `json:"messages"`
	Tools          []ToolSchema  `json:"tools,omitempty"`
	ForceToolCall  bool          `json:"force_tool_call,omitempty"`
	OutputSchema   *OutputSchema `json:"output_schema,omitempty"`
	Deadline       time.Time     `json:"deadline"`
	Privacy        PrivacyFlags  `json:"privacy,omitempty"`
	Reservation    Reservation   `json:"reservation"`
	Secret         SecretRef     `json:"secret"`
}

// ToolCall is a tool the model asked to run. Arguments is the provider-generated
// JSON string, kept verbatim so downstream validation sees exactly what the model
// produced.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolCallDelta is a streamed fragment of a tool call. Providers deliver the
// arguments incrementally, keyed by Index; ID and Name arrive on the first
// fragment (spec §25.9).
type ToolCallDelta struct {
	Index             int    `json:"index"`
	ID                string `json:"id,omitempty"`
	Name              string `json:"name,omitempty"`
	ArgumentsFragment string `json:"arguments_fragment,omitempty"`
}

// Delta is one streamed increment: either a text fragment or a tool-call fragment.
type Delta struct {
	Text     string         `json:"text,omitempty"`
	ToolCall *ToolCallDelta `json:"tool_call,omitempty"`
}

// SanitizedError is a provider error stripped of any credential or raw upstream
// body. Status is the HTTP status class the provider returned.
type SanitizedError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Status  int    `json:"status,omitempty"`
}

// Result is the canonical outcome of a model call: the actual model and provider
// request id, the streamed deltas, tool requests, usage, and — on failure — a
// sanitized error. Attempts reports how many provider attempts were made, so a
// provider that retried surfaces it rather than hiding it (spec §53.4).
type Result struct {
	ModelRequestID    contracts.ModelRequestID `json:"model_request_id"`
	ProviderRequestID string                   `json:"provider_request_id"`
	Model             string                   `json:"model"`
	Output            string                   `json:"output,omitempty"`
	ToolCalls         []ToolCall               `json:"tool_calls,omitempty"`
	Deltas            []Delta                  `json:"deltas,omitempty"`
	Usage             contracts.Usage          `json:"usage"`
	FinishReason      string                   `json:"finish_reason,omitempty"`
	Attempts          int                      `json:"attempts"`
	Error             *SanitizedError          `json:"error,omitempty"`
}

// Validate reports whether the result honors the canonical contract every adapter
// must satisfy. It is the shared conformance assertion for the fake adapter and
// the live adapter alike: usage is non-negative and consistent, streamed deltas
// carry content, and a successful result identifies the provider request and
// carries output or tool requests.
func (r Result) Validate() error {
	if r.ModelRequestID == "" {
		return errors.New("result: model_request_id is required for correlation")
	}
	if r.Usage.InputTokens < 0 || r.Usage.OutputTokens < 0 || r.Usage.TotalTokens < 0 {
		return errors.New("result: usage token counts must be non-negative")
	}
	if r.Usage.TotalTokens > 0 && r.Usage.TotalTokens < r.Usage.InputTokens+r.Usage.OutputTokens {
		return fmt.Errorf("result: total_tokens %d is below input+output", r.Usage.TotalTokens)
	}
	for i, d := range r.Deltas {
		if d.Text == "" && d.ToolCall == nil {
			return fmt.Errorf("result: delta %d carries neither text nor a tool-call fragment", i)
		}
	}
	if r.Error != nil {
		if r.Error.Code == "" {
			return errors.New("result: sanitized error has no code")
		}
		return nil
	}
	if r.ProviderRequestID == "" {
		return errors.New("result: a successful result must carry the provider request id")
	}
	if r.Output == "" && len(r.ToolCalls) == 0 {
		return errors.New("result: a successful result must carry output or tool requests")
	}
	return nil
}

// ModelAdapter converts a canonical Request to a canonical Result against one
// provider. The redeemed credential is passed separately, so it never lives on the
// Request; streamed increments are delivered to onDelta (which may be nil) as they
// arrive.
type ModelAdapter interface {
	Execute(ctx context.Context, req Request, secret string, onDelta func(Delta)) (Result, error)
}
