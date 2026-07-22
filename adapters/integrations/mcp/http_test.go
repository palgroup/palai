package mcp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/palgroup/palai/packages/egress"
)

// staticResolver flips a name to a fixed IP, proving DNS-rebinding is closed (the sender_test idiom).
type staticResolver struct{ ip string }

func (s staticResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	return []net.IPAddr{{IP: net.ParseIP(s.ip)}}, nil
}

// TestMCPHTTPTransportVettedEgress proves an HTTP MCP connection cannot reach a private/loopback/metadata
// target: the static gate rejects a literal internal IP, and a name that RESOLVES internal is rejected by
// the fail-fast VetHTTPURL and by the pinned dialer at connect (rebinding closed). Every reject is
// egress.ErrDenied.
func TestMCPHTTPTransportVettedEgress(t *testing.T) {
	for name, url := range map[string]string{
		"cloud metadata (link-local)": "https://169.254.169.254/latest/meta-data",
		"loopback":                    "https://127.0.0.1/mcp",
		"cgnat metadata":              "https://100.100.100.200/mcp",
		"http downgrade":              "http://mcp.example.test/mcp",
		"embedded credentials":        "https://user:pass@mcp.example.test/mcp",
	} {
		if _, err := NewHTTPTransport(HTTPOptions{URL: url}); !errors.Is(err, egress.ErrDenied) {
			t.Errorf("%s (%q): NewHTTPTransport err = %v, want egress.ErrDenied", name, url, err)
		}
	}

	// A name that resolves to an internal IP is rejected by the fail-fast discovery gate.
	if err := VetHTTPURL(context.Background(), staticResolver{ip: "10.0.0.5"}, "https://mcp.example.test/mcp", false); !errors.Is(err, egress.ErrDenied) {
		t.Fatalf("rebinding to private IP: VetHTTPURL err = %v, want egress.ErrDenied", err)
	}

	// And even if discovery is bypassed, the pinned dialer denies the rebinding at connect time.
	tr, err := NewHTTPTransport(HTTPOptions{URL: "https://mcp.example.test/mcp", Resolver: staticResolver{ip: "10.0.0.5"}})
	if err != nil {
		t.Fatalf("construct transport for a name (static gate is permissive on an unresolved name): %v", err)
	}
	if _, err := tr.Call(context.Background(), "initialize", map[string]any{}, nil); !errors.Is(err, egress.ErrDenied) {
		t.Fatalf("connect to a rebinding name: Call err = %v, want egress.ErrDenied at dial", err)
	}
}

// TestMCPHTTPProgressOverSSE proves the Streamable HTTP transport parses an SSE response: a tools/call whose
// server streams progress notifications then a terminal result reaches onProgress + returns the result. The
// local harness runs over loopback (AllowPrivate — the test-only egress flag) and TLS.
func TestMCPHTTPProgressOverSSE(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "initialize":
			writeJSON(w, req.ID, map[string]any{"protocolVersion": ProtocolVersion})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flush := w.(http.Flusher)
			for i := 1; i <= 2; i++ {
				fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{\"progressToken\":\"palai-progress\",\"progress\":%d,\"total\":2}}\n\n", i)
				flush.Flush()
			}
			fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"structuredContent\":{\"done\":true}}}\n\n", req.ID)
			flush.Flush()
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	// Trust the httptest server's self-signed cert by pinning it (no verification bypass).
	certPool := x509.NewCertPool()
	certPool.AddCert(srv.Certificate())
	tr, err := NewHTTPTransport(HTTPOptions{
		URL:          srv.URL,
		AllowPrivate: true,
		TLSConfig:    &tls.Config{RootCAs: certPool},
	})
	if err != nil {
		t.Fatalf("construct http transport: %v", err)
	}
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
		t.Fatalf("call over SSE: %v", err)
	}
	if out["done"] != true {
		t.Fatalf("SSE result = %v, want done:true", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("SSE progress notifications = %d, want 2", len(seen))
	}
}

// writeJSON writes a single JSON-RPC response (the non-SSE application/json path).
func writeJSON(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}
