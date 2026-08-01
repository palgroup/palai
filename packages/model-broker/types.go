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

// Image is one image attached to a conversation turn, carried as RAW BYTES plus the media
// type the bytes were SNIFFED as. The adapter base64s it onto the provider wire; nothing
// here holds a URL, and that is deliberate — a provider-fetched URL would make the provider
// dereference an address chosen upstream of us, and the bytes are already control-plane-side
// by the time a Message is built (spec §24: the object store is control-plane only).
//
// MediaType is the SNIFFED type, never a filename extension or a sender-declared mimetype:
// an image is untrusted third-party input and its metadata is data, not authority.
type Image struct {
	MediaType string `json:"media_type"`
	Data      []byte `json:"data"`
}

// Message is one conversation turn sent to the provider. Content and Images are both
// content of the SAME turn: a turn with images renders as the provider's multi-part
// content shape (text part + image parts), a turn without them renders exactly as it
// did before images existed — so a text-only run's provider request is bit-unchanged.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	Images     []Image    `json:"images,omitempty"`
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
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	// BaseURL is the endpoint the CONNECTION named, empty meaning "wherever the adapter points" (E29
	// provider wiring). It is what makes a custom OpenAI-compatible endpoint a per-project property rather
	// than a per-deployment one: before this field the only way to move the endpoint was
	// PALAI_OPENAI_COMPATIBLE_BASE_URL, read once at boot and baked into a single adapter value, so two
	// projects on one stack could not reach two endpoints and the console could not name one at all.
	//
	// IT IS AN ADDRESS, NEVER A CREDENTIAL. The value is vetted through packages/egress before it is ever
	// stored, and any error that quotes it redacts userinfo and query first (redactBaseURL) — a gateway
	// URL can carry a token, and this string reaches logs and run outcomes.
	BaseURL       string        `json:"base_url,omitempty"`
	RouteRevision int           `json:"route_revision"`
	ModelStepID   string        `json:"model_step_id"`
	Model         string        `json:"model"`
	Messages      []Message     `json:"messages"`
	Tools         []ToolSchema  `json:"tools,omitempty"`
	ForceToolCall bool          `json:"force_tool_call,omitempty"`
	OutputSchema  *OutputSchema `json:"output_schema,omitempty"`
	Deadline      time.Time     `json:"deadline"`
	Privacy       PrivacyFlags  `json:"privacy,omitempty"`
	Reservation   Reservation   `json:"reservation"`
	Secret        SecretRef     `json:"secret"`
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
	if r.Usage.InputTokens < 0 || r.Usage.OutputTokens < 0 || r.Usage.TotalTokens < 0 ||
		r.Usage.CacheReadTokens < 0 || r.Usage.CacheWriteTokens < 0 {
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

// ProbeOutcome is what a credential probe OBSERVED. Each value states what was measured, not how it feels,
// because the difference decides what an operator does next: a rejected credential is a key to re-paste, an
// unreachable endpoint is a URL or a firewall, and a transient answer is a reason to try again rather than
// to go hunting.
type ProbeOutcome string

const (
	// ProbeAccepted: the endpoint answered and did NOT reject the credential. It is the strongest thing a
	// probe can honestly say — see CredentialProber for what it deliberately does not claim.
	ProbeAccepted ProbeOutcome = "credential_accepted"
	// ProbeRejected: the endpoint answered 401/403. The key is wrong, expired, or revoked.
	ProbeRejected ProbeOutcome = "credential_rejected"
	// ProbeUnreachable: no HTTP answer at all — DNS, connection refused, TLS, or a timeout.
	ProbeUnreachable ProbeOutcome = "unreachable"
	// ProbeTransient: an answer that is neither an auth verdict nor a failure to reach — 429, 5xx. Trying
	// again later is the right move, and reporting it as a failure would send the operator after a key
	// that is fine.
	ProbeTransient ProbeOutcome = "transient"
	// ProbeUnsupported: NO probe was performed, and this value exists so that fact can never be rendered
	// as a pass. A verify action that answers green when it measured nothing is worse than no button: it
	// converts an operator's uncertainty into false confidence, which is the failure this whole surface is
	// meant to prevent. Detail says exactly what could not be done.
	ProbeUnsupported ProbeOutcome = "not_probed"
)

// Probe is one credential probe's finding. Endpoint is REDACTED (no userinfo, no query) because it travels
// into an API response and a log; Detail is a sentence for a human and never carries the credential.
type Probe struct {
	Outcome  ProbeOutcome `json:"outcome"`
	Status   int          `json:"status,omitempty"`
	Endpoint string       `json:"endpoint"`
	Detail   string       `json:"detail"`
}

// CredentialProber is the optional half of an adapter: can this credential actually reach this endpoint?
// It backs the console's verify action, and the whole reason it is a real network call is that a check
// which only measures "the string is non-empty" is a green that means nothing — an operator who trusts it
// finds out at 3am instead.
//
// WHAT A PROBE DOES NOT CLAIM, and the verify surface repeats this in the operator's own words: that the
// MODEL id is valid, that a quota exists, or that a tool-calling run will succeed. It claims exactly that
// the endpoint answered and did not reject this credential. Anything more needs a real generation, which
// is a bill this button must not silently run up.
type CredentialProber interface {
	ProbeCredential(ctx context.Context, baseURL, secret string) Probe
}
