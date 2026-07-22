package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/palgroup/palai/adapters/integrations/mcp"
	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// MCPSamplingRouter is the control-plane sampling seam (E12 T6, TOL-010). When an MCP connection enables
// sampling, a server sampling/createMessage is routed as a SEPARATE brokered, budgeted model step: a Route
// through packages/model-broker under its OWN Reservation, journalled as model_step.created/completed.v1
// events tagged source:"mcp_sampling" (no new engine frame or event kind — §61; the sampling call is entirely
// control-plane-side, never crossing the engine wire). The provider credential is the platform's OWN model
// credential (route.Secret), redeemed control-plane-side by the broker — the MCP server never sees it. The
// call is METERED and CUT OFF at Admit when it runs past the connection's budget, so an enabled server can
// never drive an unbounded model spend from one tools/call.
type MCPSamplingRouter struct {
	broker *modelbroker.Broker
	route  ModelRoute
	emit   func(ctx context.Context, scope mcp.CallScope, eventType string, payload []byte) error
}

// NewMCPSamplingRouter wires the router. emit journals one model_step event (best-effort — a journal failure
// must never fail the sampling call; the budget is enforced by the broker regardless). A nil emit is inert.
func NewMCPSamplingRouter(broker *modelbroker.Broker, route ModelRoute, emit func(ctx context.Context, scope mcp.CallScope, eventType string, payload []byte) error) *MCPSamplingRouter {
	return &MCPSamplingRouter{broker: broker, route: route, emit: emit}
}

// defaultSamplingMaxTokens caps a sampling step when a connection declares no explicit budget, so an enabled
// connection can never drive an unbounded model spend from one tools/call.
const defaultSamplingMaxTokens = 2048

// RouteSampling implements mcp.SamplingRouter: decode → Route (separate Reservation) → journal + return the
// MCP sampling result, or a denial (the gate turns the returned error into a JSON-RPC error to the server).
func (r *MCPSamplingRouter) RouteSampling(ctx context.Context, scope mcp.CallScope, conn mcp.ConnConfig, params json.RawMessage) (json.RawMessage, error) {
	messages, err := decodeSamplingMessages(params)
	if err != nil {
		return nil, err
	}
	budget := conn.SamplingMaxTokens
	if budget <= 0 {
		budget = defaultSamplingMaxTokens
	}
	requestID := newExecID("mcpsmpl")

	// A visible, budgeted step: the created event is journalled BEFORE the provider call (spec §24.7 order).
	r.journal(ctx, scope, eventModelStepCreated, map[string]any{
		"run_id": scope.RunID, "model_request_id": requestID, "source": "mcp_sampling",
		"connection_id": conn.ID, "max_total_tokens": budget,
	})

	// The SEPARATE Reservation is the TOL-010 budget: the broker's Admit rejects usage past it at settle, so
	// the sampling result is cut off when it runs over — even though the provider call was made (the tokens
	// spent are recorded, the result denied). The route provider/model/secret are the platform's own.
	// ponytail: sampling stays on the DEPLOYMENT-DEFAULT route even for a project that publishes its own
	// model route (E13 T8) — an MCP server's sampling request is not the project's own model step. Routing
	// it through the project's connection is a deliberate decision, not an oversight; make it here if a
	// tenant should pay for its servers' sampling.
	result, routeErr := r.broker.Route(ctx, r.route.Provider, modelbroker.Request{
		ModelRequestID: contracts.ModelRequestID(requestID),
		IdempotencyKey: scope.RunID + "/" + requestID,
		Model:          r.route.Model,
		Messages:       messages,
		Reservation:    modelbroker.Reservation{MaxTotalTokens: budget},
		Secret:         r.route.Secret,
	}, nil)
	if routeErr != nil {
		// A budget cutoff still MADE the provider call (the tokens were spent, the result rejected at Admit):
		// carry the provider request id + usage so the denial event is honest evidence that a REAL provider
		// Route happened and the SEPARATE budget cut it off — the live smoke asserts exactly this.
		r.journal(ctx, scope, eventModelStepCompleted, map[string]any{
			"run_id": scope.RunID, "model_request_id": requestID, "source": "mcp_sampling",
			"connection_id": conn.ID, "denied": true, "reason": samplingDenyReason(routeErr),
			"provider_request_id": result.ProviderRequestID, "total_tokens": result.Usage.TotalTokens,
		})
		return nil, routeErr
	}

	r.journal(ctx, scope, eventModelStepCompleted, map[string]any{
		"run_id": scope.RunID, "model_request_id": requestID, "source": "mcp_sampling",
		"connection_id": conn.ID, "model": result.Model, "provider_request_id": result.ProviderRequestID,
		"total_tokens": result.Usage.TotalTokens,
	})

	out, _ := json.Marshal(map[string]any{
		"role":       "assistant",
		"content":    map[string]any{"type": "text", "text": result.Output},
		"model":      result.Model,
		"stopReason": "endTurn",
	})
	return out, nil
}

// journal emits one model_step event, best-effort — a journal failure must not fail the sampling call.
func (r *MCPSamplingRouter) journal(ctx context.Context, scope mcp.CallScope, eventType string, payload map[string]any) {
	if r.emit == nil {
		return
	}
	body, _ := json.Marshal(payload)
	_ = r.emit(ctx, scope, eventType, body)
}

// samplingDenyReason maps a Route failure to a STABLE, non-leaky reason for the completed event and the
// JSON-RPC error — never internal detail, never a credential (there is none on this path).
func samplingDenyReason(err error) string {
	if errors.Is(err, modelbroker.ErrBudgetExceeded) {
		return "budget_exceeded"
	}
	return "denied"
}

// decodeSamplingMessages converts MCP sampling/createMessage params into canonical broker messages. A
// systemPrompt becomes a leading system turn; each message's content ({type:"text",text} or a bare string)
// becomes the turn content. We route to the PLATFORM's configured model — the server's modelPreferences are
// deliberately ignored (a server cannot pick an arbitrary/expensive model; honest ceiling, and a defence).
func decodeSamplingMessages(params json.RawMessage) ([]modelbroker.Message, error) {
	var p struct {
		SystemPrompt string `json:"systemPrompt"`
		Messages     []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("mcp sampling: decode params: %w", err)
	}
	var out []modelbroker.Message
	if p.SystemPrompt != "" {
		out = append(out, modelbroker.Message{Role: "system", Content: p.SystemPrompt})
	}
	for _, m := range p.Messages {
		out = append(out, modelbroker.Message{Role: m.Role, Content: samplingText(m.Content)})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("mcp sampling: no messages to route")
	}
	return out, nil
}

// samplingText extracts the text from an MCP content block ({type:"text",text:"..."}) or a bare string.
func samplingText(raw json.RawMessage) string {
	var obj struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.Text != "" {
		return obj.Text
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}
