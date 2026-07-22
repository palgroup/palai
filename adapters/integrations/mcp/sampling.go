package mcp

import (
	"context"
	"encoding/json"
)

// This file is the transport-agnostic MCP sampling hook (spec §28.14, E12 Task 6, TOL-010). An MCP server can
// send a server→client REQUEST — sampling/createMessage — asking the platform to run an LLM completion on its
// behalf. Before T6 both read loops silently DROPPED such a message (a request that is neither a response nor
// a progress notification), so sampling was invisible. This hook makes it visible and, by default, DENIED:
//
//   - default-deny: a nil handler (sampling off for the connection) returns a JSON-RPC error, never a silent
//     drop and never a model call;
//   - flood cap: at most maxSamplingPerCall requests are served per tools/call, so a hostile server cannot
//     drive unbounded budgeted model steps (or unbounded events) from one call;
//   - breaker-safe: a denial is a JSON-RPC RESPONSE the transport writes back, NOT a transport error, so a
//     server that floods denials can never trip the per-connection breaker (poisoning it against a tenant);
//   - transport-agnostic: stdio (the network-less container) and HTTP route through the SAME gate, so a
//     stdio server's sampling request is denied/enabled by exactly the same path an HTTP server's is.
//
// The gate NEVER touches a bearer: it routes through the platform's OWN model credential, control-plane-side.

// maxSamplingPerCall bounds server sampling/createMessage requests one tools/call may make (flood defence).
const maxSamplingPerCall = 4

// samplingMethod is the ONLY server→client request method Palai serves; any other is method-not-found (Palai
// requests no roots/elicitation surface — those are E17 deferrals, denied here rather than silently skipped).
const samplingMethod = "sampling/createMessage"

// JSON-RPC 2.0 error codes for a denied server→client request.
const (
	rpcMethodNotFound = -32601
	rpcInternalError  = -32603
)

// SamplingHandler routes a server sampling/createMessage to a budgeted, brokered model step and returns the
// raw JSON-RPC result on success, or an error to DENY. A nil handler denies every request (default-deny) —
// sampling is off unless a connection enables it AND a router is wired. The handler routes through the
// platform's own model credential control-plane-side; it never receives, returns, or logs the MCP bearer.
type SamplingHandler func(ctx context.Context, params json.RawMessage) (json.RawMessage, error)

// SamplingRouter is the control-plane seam the manager binds a per-call SamplingHandler over (Config.Sampling).
// execution.MCPSamplingRouter implements it: a Route through packages/model-broker under a SEPARATE
// Reservation, emitting model_step.created/completed.v1 events tagged source:"mcp_sampling" — no new engine
// frame or event kind (§61). Nil ⇒ default-deny.
type SamplingRouter interface {
	RouteSampling(ctx context.Context, scope CallScope, conn ConnConfig, params json.RawMessage) (json.RawMessage, error)
}

// samplingGate serves a transport's inbound server→client requests during ONE tools/call. It is created per
// call and drained on the single Call goroutine (exactly one client request is outstanding at a time), so its
// counter needs no lock. A nil handler default-denies.
type samplingGate struct {
	handler SamplingHandler
	count   int
}

// serve turns one inbound server request into the (result, rpcError) the transport writes back as the
// JSON-RPC response. A non-sampling method is method-not-found; a sampling request past the flood cap, or
// with no handler (default-deny), or that the handler denies (e.g. budget exceeded), is a JSON-RPC error with
// a STABLE, non-leaky message — never internal detail, never a bearer.
func (g *samplingGate) serve(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, *rpcError) {
	if method != samplingMethod {
		return nil, &rpcError{Code: rpcMethodNotFound, Message: "method not supported"}
	}
	g.count++
	if g.count > maxSamplingPerCall {
		return nil, &rpcError{Code: rpcInternalError, Message: "sampling request rate exceeded"}
	}
	if g.handler == nil {
		return nil, &rpcError{Code: rpcMethodNotFound, Message: "sampling is not enabled for this connection"}
	}
	result, err := g.handler(ctx, params)
	if err != nil {
		return nil, &rpcError{Code: rpcInternalError, Message: "sampling denied"}
	}
	return result, nil
}

// samplingReceiver is implemented by a transport that can serve server→client sampling requests. The manager
// binds the per-call handler after dialing (transport.(samplingReceiver).setSampling); a transport without
// one — or a call with no handler — default-denies.
type samplingReceiver interface {
	setSampling(SamplingHandler)
}

// isServerRequest reports whether an inbound frame is a server→client REQUEST (a method AND an id), as opposed
// to a notification (method, no id) or a response to our own call (id, no method). Only a request is served —
// a response with a non-matching id is an earlier/foreign response the caller still skips.
func isServerRequest(msg rpcMessage) bool {
	return msg.Method != "" && len(msg.ID) != 0
}

// serverResponseFrame builds the JSON-RPC response frame a transport writes back for a served server request:
// the same id, with either result or error. rpcErr wins when set.
func serverResponseFrame(id json.RawMessage, result json.RawMessage, rpcErr *rpcError) map[string]any {
	frame := map[string]any{"jsonrpc": "2.0", "id": id}
	if rpcErr != nil {
		frame["error"] = map[string]any{"code": rpcErr.Code, "message": rpcErr.Message}
	} else {
		frame["result"] = result
	}
	return frame
}
