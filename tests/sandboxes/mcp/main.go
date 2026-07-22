// Command mcp-fixture is a minimal, newline-delimited JSON-RPC MCP server (2025-11-25 stdio framing)
// used ONLY by the E12 Task 5 test tiers. It is the untrusted-code stand-in the OCI sandbox confines: it
// runs with no network, no mounts, and no credentials — everything it needs arrives on stdin.
//
// It speaks the subset T5 exercises: initialize / notifications/initialized, tools/list (two tools: a pure
// `echo`, and a long-running `slow` that emits notifications/progress when a progressToken is supplied and
// stops on notifications/cancelled), and tools/call. Any other method returns a JSON-RPC error (resources/
// prompts are the "unsupported surface" the client surfaces rather than silently skipping — E17 deferral).
//
// The same binary backs the component HTTP tier: `go run` behind a local harness that frames each POST body
// as one JSON-RPC message. The wire logic is transport-agnostic (handle() takes bytes, returns bytes).
package main

import (
	"bufio"
	"encoding/json"
	"os"
	"sync"
	"time"
)

// protocolVersion is the MCP revision this fixture advertises; the client asserts it matches.
const protocolVersion = "2025-11-25"

// request is the inbound JSON-RPC message shape (request or notification — a notification has no id).
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// server holds the single cancellation flag the `slow` tool polls. A per-call container only ever runs one
// request of interest, so one flag is enough (no per-request registry).
type server struct {
	mu        sync.Mutex
	cancelled map[string]bool // requestId -> cancelled
	out       *bufio.Writer
}

func main() {
	s := &server{cancelled: map[string]bool{}, out: bufio.NewWriter(os.Stdout)}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // MCP messages may be large; bound at 4MiB
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			continue // a non-JSON line is ignored (the client never sends one)
		}
		s.dispatch(req)
	}
}

// dispatch routes one inbound message. A notification (no id) is handled for its side effect only; a request
// gets exactly one response. notifications/cancelled flips the cancel flag the slow tool polls.
func (s *server) dispatch(req request) {
	switch req.Method {
	case "initialize":
		s.reply(req.ID, map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "palai-mcp-fixture", "version": "0.1.0"},
		})
	case "notifications/initialized":
		// no response
	case "notifications/cancelled":
		var p struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		_ = json.Unmarshal(req.Params, &p)
		s.mu.Lock()
		s.cancelled[string(p.RequestID)] = true
		s.mu.Unlock()
	case "tools/list":
		s.reply(req.ID, map[string]any{"tools": toolCatalog()})
	case "tools/call":
		s.callTool(req)
	default:
		// resources/prompts and everything else: a JSON-RPC method-not-found error, never a silent skip.
		s.replyError(req.ID, -32601, "method not found: "+req.Method)
	}
}

// toolCatalog is the fixture's two-tool advertisement. echo is pure; slow is the progress/cancel tool.
func toolCatalog() []map[string]any {
	objSchema := func(props map[string]any) map[string]any {
		return map[string]any{"type": "object", "properties": props}
	}
	return []map[string]any{
		{
			"name":        "echo",
			"description": "Echoes its message argument back.",
			"inputSchema": objSchema(map[string]any{"message": map[string]any{"type": "string"}}),
		},
		{
			"name":        "slow",
			"description": "Emits progress then returns; honours cancellation.",
			"inputSchema": objSchema(map[string]any{"steps": map[string]any{"type": "number"}}),
		},
	}
}

// callTool executes a tools/call. echo returns immediately; slow emits progress notifications (when the
// caller supplied a progressToken) and polls the cancel flag between steps.
func (s *server) callTool(req request) {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
		Meta      struct {
			ProgressToken json.RawMessage `json:"progressToken"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		s.replyError(req.ID, -32602, "invalid params")
		return
	}
	switch p.Name {
	case "echo":
		msg, _ := p.Arguments["message"].(string)
		s.reply(req.ID, toolResult(map[string]any{"echo": msg}))
	case "slow":
		steps := 3
		if n, ok := p.Arguments["steps"].(float64); ok && n > 0 {
			steps = int(n)
		}
		for i := 1; i <= steps; i++ {
			s.mu.Lock()
			cancelled := s.cancelled[string(req.ID)]
			s.mu.Unlock()
			if cancelled {
				s.replyError(req.ID, -32800, "request cancelled")
				return
			}
			if len(p.Meta.ProgressToken) > 0 {
				s.notify("notifications/progress", map[string]any{
					"progressToken": json.RawMessage(p.Meta.ProgressToken),
					"progress":      i,
					"total":         steps,
				})
			}
			time.Sleep(20 * time.Millisecond)
		}
		s.reply(req.ID, toolResult(map[string]any{"done": true, "steps": steps}))
	default:
		s.replyError(req.ID, -32602, "unknown tool: "+p.Name)
	}
}

// toolResult wraps a structured payload in the MCP tools/call result envelope: a text content block plus
// structuredContent, so a client that validates against an output schema has the object form.
func toolResult(structured map[string]any) map[string]any {
	text, _ := json.Marshal(structured)
	return map[string]any{
		"content":           []map[string]any{{"type": "text", "text": string(text)}},
		"structuredContent": structured,
		"isError":           false,
	}
}

func (s *server) reply(id json.RawMessage, result any) {
	s.write(map[string]any{"jsonrpc": "2.0", "id": rawOrNull(id), "result": result})
}

func (s *server) replyError(id json.RawMessage, code int, message string) {
	s.write(map[string]any{"jsonrpc": "2.0", "id": rawOrNull(id), "error": map[string]any{"code": code, "message": message}})
}

func (s *server) notify(method string, params any) {
	s.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// write serialises one message as a single newline-delimited line (MCP stdio framing: no embedded newline).
func (s *server) write(msg map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	encoded, err := json.Marshal(msg)
	if err != nil {
		return
	}
	_, _ = s.out.Write(encoded)
	_ = s.out.WriteByte('\n')
	_ = s.out.Flush()
}

// rawOrNull renders a missing id as JSON null (a notification reply should not happen, but never panic).
func rawOrNull(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}
