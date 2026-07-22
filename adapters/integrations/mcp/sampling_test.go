package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// samplingFixture is a stdio MCP double that, on a tools/call, sends `toSend` server→client
// sampling/createMessage REQUESTS before answering the tool. It records how each request was answered
// (denied = a JSON-RPC error, satisfied = a result) so a test can prove default-deny, the flood cap, and the
// enabled path — all WITHOUT a container.
type samplingFixture struct {
	toSend    int
	mu        sync.Mutex
	sent      int
	denied    int
	satisfied int
	initCaps  json.RawMessage
}

func (f *samplingFixture) serve(r io.Reader, w io.Writer) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxStdioMessage)
	enc := func(v any) { b, _ := json.Marshal(v); _, _ = w.Write(append(b, '\n')) }
	toolResult := func(id json.RawMessage) map[string]any {
		return map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"structuredContent": map[string]any{"done": true}}}
	}
	pending := json.RawMessage(nil)
	remaining := 0
	for sc.Scan() {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &msg) != nil {
			continue
		}
		switch {
		case msg.Method == "initialize":
			var p struct {
				Capabilities json.RawMessage `json:"capabilities"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			f.mu.Lock()
			f.initCaps = p.Capabilities
			f.mu.Unlock()
			enc(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{"protocolVersion": ProtocolVersion}})
		case msg.Method == "notifications/initialized":
		case msg.Method == "tools/call":
			pending, remaining = msg.ID, f.toSend
			for i := 0; i < f.toSend; i++ {
				f.mu.Lock()
				f.sent++
				f.mu.Unlock()
				enc(map[string]any{"jsonrpc": "2.0", "id": fmt.Sprintf("srv-%d", i), "method": samplingMethod,
					"params": map[string]any{"messages": []any{map[string]any{"role": "user", "content": map[string]any{"type": "text", "text": "hi"}}}}})
			}
			if remaining == 0 {
				enc(toolResult(pending))
				pending = nil
			}
		case msg.Method == "" && len(msg.ID) != 0:
			// The client's response to one of our sampling requests: denied (error) or satisfied (result).
			f.mu.Lock()
			if len(msg.Error) != 0 {
				f.denied++
			} else {
				f.satisfied++
			}
			f.mu.Unlock()
			if remaining--; remaining == 0 && pending != nil {
				enc(toolResult(pending))
				pending = nil
			}
		}
	}
}

// wireSampling builds a StdioTransport talking to a samplingFixture and binds the given handler (nil ⇒
// default-deny). It returns the transport + fixture; the caller drives it through a Client.
func wireSampling(t *testing.T, fx *samplingFixture, handler SamplingHandler) Transport {
	t.Helper()
	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	go fx.serve(c2sR, s2cW)
	tr := NewStdioTransport(c2sW, s2cR, func(context.Context) error { _ = c2sW.Close(); return nil })
	tr.(samplingReceiver).setSampling(handler)
	t.Cleanup(func() { _ = tr.Close(context.Background()) })
	return tr
}

// TestSamplingDeniedByDefault proves TOL-010 half 1: with no handler wired (the default), a server's
// sampling/createMessage during a tools/call gets a JSON-RPC error — NOT a silent drop, NOT a model call —
// and the tools/call still completes normally. Because the denial is a written response and NOT a returned
// transport error, it can never trip the breaker (a hostile server cannot poison it by flooding denials).
func TestSamplingDeniedByDefault(t *testing.T) {
	fx := &samplingFixture{toSend: 1}
	c := NewClient(wireSampling(t, fx, nil)) // default-deny
	ctx := context.Background()
	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	// A client with no sampling handler does NOT advertise the capability.
	fx.mu.Lock()
	caps := string(fx.initCaps)
	fx.mu.Unlock()
	if strings.Contains(caps, "sampling") {
		t.Fatalf("default client advertised sampling capability: %s", caps)
	}
	out, err := c.CallTool(ctx, "echo", nil, nil)
	if err != nil {
		t.Fatalf("tools/call returned an error (a sampling denial must NOT fail the call / trip the breaker): %v", err)
	}
	if out["done"] != true {
		t.Fatalf("tools/call result = %v, want done:true", out)
	}
	fx.mu.Lock()
	defer fx.mu.Unlock()
	if fx.sent != 1 || fx.denied != 1 || fx.satisfied != 0 {
		t.Fatalf("sampling: sent=%d denied=%d satisfied=%d, want 1/1/0 (default-deny)", fx.sent, fx.denied, fx.satisfied)
	}
}

// TestSamplingFloodCapped proves the per-call flood cap: even with an ENABLED handler, at most
// maxSamplingPerCall requests reach it in one tools/call; the rest are denied WITHOUT a model call, so a
// hostile server cannot drive unbounded budgeted steps from one call. The enabled handler that IS reached
// returns a result (proving the enabled write-back path), so those requests are satisfied.
func TestSamplingFloodCapped(t *testing.T) {
	var invoked int
	handler := func(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
		invoked++
		return json.RawMessage(`{"role":"assistant","content":{"type":"text","text":"ok"}}`), nil
	}
	fx := &samplingFixture{toSend: maxSamplingPerCall + 2}
	c := NewClient(wireSampling(t, fx, handler))
	ctx := context.Background()
	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if _, err := c.CallTool(ctx, "echo", nil, nil); err != nil {
		t.Fatalf("tools/call: %v", err)
	}
	if invoked != maxSamplingPerCall {
		t.Fatalf("handler invocations = %d, want the flood cap %d", invoked, maxSamplingPerCall)
	}
	fx.mu.Lock()
	defer fx.mu.Unlock()
	if fx.satisfied != maxSamplingPerCall || fx.denied != 2 {
		t.Fatalf("sampling: satisfied=%d denied=%d, want %d/2 (cap then deny)", fx.satisfied, fx.denied, maxSamplingPerCall)
	}
}

// TestInitializeAdvertisesSamplingOnlyWhenEnabled proves the capability is advertised ONLY when a router is
// bound — a server never sees a sampling capability it cannot exercise (default-deny is invisible).
func TestInitializeAdvertisesSamplingOnlyWhenEnabled(t *testing.T) {
	fx := &samplingFixture{toSend: 0}
	handler := func(context.Context, json.RawMessage) (json.RawMessage, error) { return nil, nil }
	c := NewClient(wireSampling(t, fx, handler))
	c.advertiseSampling = true // the manager sets this when a router is wired
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	fx.mu.Lock()
	defer fx.mu.Unlock()
	if !strings.Contains(string(fx.initCaps), "sampling") {
		t.Fatalf("enabled client did not advertise sampling: %s", fx.initCaps)
	}
}

// TestSamplingDeniedByDefaultHTTP proves the SAME default-deny over the HTTP transport: a server request in
// the SSE stream is NOT misread as the tools/call result (it carries an id), is denied (a JSON-RPC error
// POSTed back), and the real result still arrives. Before T6 the readSSE loop would have returned the sampling
// request's params AS the tool result.
func TestSamplingDeniedByDefaultHTTP(t *testing.T) {
	var mu sync.Mutex
	var deniedResponses int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Error  json.RawMessage `json:"error"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch {
		case req.Method == "initialize":
			writeJSON(w, req.ID, map[string]any{"protocolVersion": ProtocolVersion})
		case req.Method == "tools/call":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flush := w.(http.Flusher)
			// A server sampling request FOLLOWED by the real result, both on the stream (a cooperative server
			// answers on the original stream). The client must deny the first and return the second.
			fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":\"srv-1\",\"method\":\"%s\",\"params\":{\"messages\":[]}}\n\n", samplingMethod)
			flush.Flush()
			fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"structuredContent\":{\"done\":true}}}\n\n", req.ID)
			flush.Flush()
		case req.Method == "" && len(req.ID) != 0:
			// The client's POSTed response to our sampling request.
			mu.Lock()
			if len(req.Error) != 0 {
				deniedResponses++
			}
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	tr, err := NewHTTPTransport(HTTPOptions{URL: srv.URL, AllowPrivate: true, TLSConfig: trust(srv)})
	if err != nil {
		t.Fatalf("construct transport: %v", err)
	}
	c := NewClient(tr) // default-deny (no handler bound)
	ctx := context.Background()
	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	out, err := c.CallTool(ctx, "echo", nil, nil)
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}
	if out["done"] != true {
		t.Fatalf("tools/call result = %v, want done:true (the sampling request must not be misread as the result)", out)
	}
	// Give the best-effort response POST a moment to land, then assert it was a denial.
	mu.Lock()
	defer mu.Unlock()
	if deniedResponses != 1 {
		t.Fatalf("denied sampling responses = %d, want 1 (HTTP default-deny)", deniedResponses)
	}
}
