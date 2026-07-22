// Package mcp is the Palai MCP client (spec §28.13, MCP 2025-11-25). It speaks the tools subset — initialize,
// tools/list, tools/call, progress, cancel — over two transports: stdio (an MCP server run as untrusted code
// inside a hardened, network-less OCI container, one container per call) and Streamable HTTP (vetted through
// packages/egress). An MCP server is UNTRUSTED: the stdio container gets no mounts, no network, and no
// credentials; the HTTP transport sends the connection's own bearer only, never the platform's.
//
// This file is the transport-agnostic protocol layer: the Client drives a Transport with typed requests and
// parses typed results. It speaks ONLY the tools subset (initialize/tools-list/tools-call) — it never
// requests resources/prompts (E17 deferral), so there is no non-tools surface to skip.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// ProtocolVersion is the MCP revision this client negotiates. A server that answers initialize with a
// different version is rejected rather than best-effort adapted (honest capability claim).
const ProtocolVersion = "2025-11-25"

var (
	// ErrProtocol marks a malformed or protocol-violating server message (bad JSON-RPC, wrong version, a
	// JSON-RPC error result). It classifies a connection failure the breaker counts.
	ErrProtocol = errors.New("mcp: protocol error")
)

// Progress is one advisory progress notification (spec §basic/utilities/progress). It never advances the
// tool-call state machine — it rides the tool_call.progress.v1 advisory event.
type Progress struct {
	Progress float64
	Total    float64
	Message  string
}

// RemoteTool is one entry from a server's tools/list: its remote name, untrusted description, and input
// schema. Discovery turns each into a connection-namespaced draft tool revision (never auto-published).
type RemoteTool struct {
	Name        string
	Description string
	InputSchema map[string]any
}

// Transport is a single-request-at-a-time JSON-RPC channel. A per-call container (stdio) or one POST (http)
// only ever has one outstanding request, so Call may route any progress notification it observes to
// onProgress without a token registry (the per-call-container ceiling). Notify sends a fire-and-forget
// notification (no id, no response). Close tears the transport (and its container/connection) down.
type Transport interface {
	Call(ctx context.Context, method string, params any, onProgress func(Progress)) (json.RawMessage, error)
	Notify(ctx context.Context, method string, params any) error
	Close(ctx context.Context) error
}

// Client is the protocol driver over a Transport. advertiseSampling controls whether Initialize advertises
// the `sampling` capability — the manager sets it ONLY when the connection enables sampling AND a router is
// wired, so a server never sees a sampling capability it cannot exercise (default-deny is invisible).
type Client struct {
	t                 Transport
	advertiseSampling bool
}

// NewClient wraps a transport as an MCP client.
func NewClient(t Transport) *Client { return &Client{t: t} }

// Initialize performs the MCP handshake: initialize (asserting the negotiated protocol version) then the
// notifications/initialized acknowledgement. A version mismatch or a JSON-RPC error is ErrProtocol.
func (c *Client) Initialize(ctx context.Context) error {
	capabilities := map[string]any{}
	if c.advertiseSampling {
		// Advertised ONLY when sampling is enabled + a router is wired: a server that sees this may send
		// sampling/createMessage, which the gate routes to a budgeted model step; a server that does not
		// see it should not send one (and if it does anyway, the gate default-denies).
		capabilities["sampling"] = map[string]any{}
	}
	raw, err := c.t.Call(ctx, "initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    capabilities,
		"clientInfo":      map[string]any{"name": "palai", "version": "0.1.0"},
	}, nil)
	if err != nil {
		return err
	}
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("%w: initialize result: %v", ErrProtocol, err)
	}
	if result.ProtocolVersion != ProtocolVersion {
		return fmt.Errorf("%w: server protocol %q != %q", ErrProtocol, result.ProtocolVersion, ProtocolVersion)
	}
	return c.t.Notify(ctx, "notifications/initialized", map[string]any{})
}

// ListTools returns the server's advertised tools (tools/list). The descriptions are UNTRUSTED text the
// caller must treat as draft (discovery.go pins them behind approval).
func (c *Client) ListTools(ctx context.Context) ([]RemoteTool, error) {
	raw, err := c.t.Call(ctx, "tools/list", map[string]any{}, nil)
	if err != nil {
		return nil, err
	}
	var result struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("%w: tools/list result: %v", ErrProtocol, err)
	}
	out := make([]RemoteTool, 0, len(result.Tools))
	for _, t := range result.Tools {
		out = append(out, RemoteTool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
	}
	return out, nil
}

// CallTool invokes a remote tool (tools/call), routing any progress notification to onProgress. It returns
// the result's structuredContent when present (the object form an output schema validates), else the raw
// MCP result object — always data-only, never trusted for capability. A ctx cancel drives the transport's
// notifications/cancelled and returns the ctx error.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any, onProgress func(Progress)) (map[string]any, error) {
	if args == nil {
		args = map[string]any{}
	}
	// A progressToken opts this call into progress notifications (spec progress utility). The transport
	// generates the token domain; we pass a stable per-call marker the server echoes.
	params := map[string]any{
		"name":      name,
		"arguments": args,
		"_meta":     map[string]any{"progressToken": "palai-progress"},
	}
	raw, err := c.t.Call(ctx, "tools/call", params, onProgress)
	if err != nil {
		return nil, err
	}
	var result struct {
		StructuredContent map[string]any `json:"structuredContent"`
		IsError           bool           `json:"isError"`
		Content           []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("%w: tools/call result: %v", ErrProtocol, err)
	}
	if result.IsError {
		return nil, fmt.Errorf("%w: tool %q returned isError", ErrProtocol, name)
	}
	if result.StructuredContent != nil {
		return result.StructuredContent, nil
	}
	// No structured content: return the text blocks as a data-only object (no schema constraint applies).
	text := ""
	if len(result.Content) > 0 {
		text = result.Content[0].Text
	}
	return map[string]any{"content": text}, nil
}

// Close tears down the transport.
func (c *Client) Close(ctx context.Context) error { return c.t.Close(ctx) }
