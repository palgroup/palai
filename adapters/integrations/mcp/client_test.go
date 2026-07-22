package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"
)

// fakeServer is an in-test newline-delimited JSON-RPC MCP double wired to a StdioTransport over two pipes.
// It proves the CLIENT's framing/id-matching/progress/cancel logic without a container; the real fixture in
// a hardened OCI container is the component tier's isolation proof (stdio_component_test.go).
type fakeServer struct {
	mu        sync.Mutex
	cancelled bool // set when notifications/cancelled arrives
}

// wire builds a StdioTransport talking to a fresh fakeServer over two io.Pipes, and starts the server loop.
func wire(t *testing.T) (Transport, *fakeServer) {
	t.Helper()
	c2sR, c2sW := io.Pipe() // client -> server
	s2cR, s2cW := io.Pipe() // server -> client
	srv := &fakeServer{}
	go srv.serve(c2sR, s2cW)
	tr := NewStdioTransport(c2sW, s2cR, func(context.Context) error { _ = c2sW.Close(); return nil })
	t.Cleanup(func() { _ = tr.Close(context.Background()) })
	return tr, srv
}

func (s *fakeServer) serve(r io.Reader, w io.Writer) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxStdioMessage)
	enc := func(v any) { b, _ := json.Marshal(v); _, _ = w.Write(append(b, '\n')) }
	for sc.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &req) != nil {
			continue
		}
		switch req.Method {
		case "initialize":
			enc(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": ProtocolVersion}})
		case "notifications/initialized":
		case "notifications/cancelled":
			s.mu.Lock()
			s.cancelled = true
			s.mu.Unlock()
		case "tools/list":
			enc(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"tools": []map[string]any{
				{"name": "echo", "description": "echo", "inputSchema": map[string]any{"type": "object"}},
				{"name": "slow", "description": "slow", "inputSchema": map[string]any{"type": "object"}},
			}}})
		case "tools/call":
			s.handleCall(req.ID, req.Params, enc)
		}
	}
}

func (s *fakeServer) handleCall(id, params json.RawMessage, enc func(any)) {
	var p struct {
		Name string `json:"name"`
		Meta struct {
			ProgressToken json.RawMessage `json:"progressToken"`
		} `json:"_meta"`
	}
	_ = json.Unmarshal(params, &p)
	switch p.Name {
	case "echo":
		enc(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"structuredContent": map[string]any{"echo": "hi"}}})
	case "slow":
		for i := 1; i <= 3; i++ {
			s.mu.Lock()
			cancelled := s.cancelled
			s.mu.Unlock()
			if cancelled {
				enc(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32800, "message": "cancelled"}})
				return
			}
			enc(map[string]any{"jsonrpc": "2.0", "method": "notifications/progress", "params": map[string]any{
				"progressToken": json.RawMessage(p.Meta.ProgressToken), "progress": i, "total": 3,
			}})
			time.Sleep(30 * time.Millisecond)
		}
		enc(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"structuredContent": map[string]any{"done": true}}})
	}
}

// TestMCPClientStdioFramingDiscoveryCall proves initialize → tools/list → tools/call echo round-trips over
// the newline-delimited framing with correct id matching and structuredContent extraction.
func TestMCPClientStdioFramingDiscoveryCall(t *testing.T) {
	tr, _ := wire(t)
	c := NewClient(tr)
	ctx := context.Background()
	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	tools, err := c.ListTools(ctx)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(tools) != 2 || tools[0].Name != "echo" {
		t.Fatalf("tools = %+v, want [echo slow]", tools)
	}
	out, err := c.CallTool(ctx, "echo", map[string]any{"message": "hi"}, nil)
	if err != nil {
		t.Fatalf("call echo: %v", err)
	}
	if out["echo"] != "hi" {
		t.Fatalf("echo result = %v, want structuredContent {echo:hi}", out)
	}
}

// TestMCPClientProgressNotificationsRouted proves a long-running tools/call's notifications/progress reach
// the onProgress callback while the client waits for the terminal result.
func TestMCPClientProgressNotificationsRouted(t *testing.T) {
	tr, _ := wire(t)
	c := NewClient(tr)
	ctx := context.Background()
	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	var mu sync.Mutex
	var seen []Progress
	out, err := c.CallTool(ctx, "slow", nil, func(p Progress) {
		mu.Lock()
		seen = append(seen, p)
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("call slow: %v", err)
	}
	if out["done"] != true {
		t.Fatalf("slow result = %v, want done:true", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 3 {
		t.Fatalf("progress notifications = %d, want >= 3", len(seen))
	}
}

// TestMCPClientCancelSendsCancelledAndReturnsCtxErr proves a cancelled ctx during a tools/call sends
// notifications/cancelled to the server and surfaces the ctx error (the manager then tears the container
// down). The server records the cancellation.
func TestMCPClientCancelSendsCancelledAndReturnsCtxErr(t *testing.T) {
	tr, srv := wire(t)
	c := NewClient(tr)
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(40 * time.Millisecond); cancel() }()
	_, err := c.CallTool(ctx, "slow", nil, nil)
	if err == nil {
		t.Fatal("call slow with a cancelled ctx returned nil error, want ctx cancellation")
	}
	// The server observed notifications/cancelled (give the in-flight write a beat to land).
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		srv.mu.Lock()
		cancelled := srv.cancelled
		srv.mu.Unlock()
		if cancelled {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("server never received notifications/cancelled after ctx cancel")
}
