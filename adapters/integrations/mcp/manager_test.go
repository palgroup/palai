package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/oci"
	"github.com/palgroup/palai/packages/coordinator"
)

// failDriver is an InteractiveDriver whose Start always fails, simulating an MCP server that crashes on
// launch. It counts starts so a test can prove the breaker sheds without dialing once open.
type failDriver struct{ starts int }

func (d *failDriver) Start(context.Context, oci.ContainerSpec) (oci.Process, error) {
	d.starts++
	return nil, errors.New("mcp server crashed on launch")
}

// TestMCPServerCrashTripsBreakerToolUnavailable proves EXT-005's connection-level defence: repeated launch
// failures trip the in-memory breaker; once open, a further call returns ErrToolUnavailable WITHOUT dialing
// (the container start count stops advancing) — a visible, fast failure, and the control plane stays up.
func TestMCPServerCrashTripsBreakerToolUnavailable(t *testing.T) {
	driver := &failDriver{}
	m := NewManager(Config{Driver: driver, BreakerThreshold: 3, BreakerCooldown: time.Hour})
	conn := ConnConfig{ID: "mcpc_x", Transport: "stdio", ImageDigest: "sha256:" + zeros64(), Cmd: []string{"/mcp"}}
	ctx := context.Background()

	// The first three calls each attempt a start and fail (a real dial error, not shed).
	for i := 0; i < 3; i++ {
		if _, err := m.Call(ctx, CallScope{}, conn, "echo", nil); err == nil {
			t.Fatalf("call %d: expected a launch failure", i)
		}
	}
	if driver.starts != 3 {
		t.Fatalf("start attempts = %d, want 3 before the breaker trips", driver.starts)
	}
	// The breaker is now open: the next call is shed FAST as ErrToolUnavailable, with no new dial.
	if _, err := m.Call(ctx, CallScope{}, conn, "echo", nil); !errors.Is(err, ErrToolUnavailable) {
		t.Fatalf("post-trip call err = %v, want ErrToolUnavailable", err)
	}
	if driver.starts != 3 {
		t.Fatalf("start attempts = %d after the breaker opened, want it unchanged at 3 (shed without dialing)", driver.starts)
	}
}

// TestMCPStdioRequiresDriver proves the stdio path fails cleanly (never escapes) when no OCI driver is
// wired — a call returns an error rather than running the server on the host.
func TestMCPStdioRequiresDriver(t *testing.T) {
	m := NewManager(Config{}) // no driver
	_, err := m.Call(context.Background(), CallScope{}, ConnConfig{ID: "c", Transport: "stdio", ImageDigest: "sha256:" + zeros64(), Cmd: []string{"/mcp"}}, "echo", nil)
	if err == nil {
		t.Fatal("stdio call with no driver returned nil, want a clean failure (no host escape)")
	}
}

func zeros64() string { return "0000000000000000000000000000000000000000000000000000000000000000" }

// mcpProbeServer is a real MCP server over Streamable HTTP, speaking exactly the subset client.go uses:
// initialize, notifications/initialized, tools/list, tools/call. It REQUIRES the bearer, so a test that
// gets a result has proven the credential travelled — not merely that a request was made.
func mcpProbeServer(t *testing.T, wantBearer string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
			Params struct {
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&msg)

		reply := func(v map[string]any) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Mcp-Session-Id", "probe-session")
			_ = json.NewEncoder(w).Encode(v)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+wantBearer {
			reply(map[string]any{"jsonrpc": "2.0", "id": msg.ID,
				"error": map[string]any{"code": -32001, "message": "unauthorized"}})
			return
		}
		switch msg.Method {
		case "initialize":
			reply(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{
				"protocolVersion": ProtocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "palai-probe", "version": "0.0.1"}}})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			reply(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{
				"tools": []map[string]any{{
					"name":        "reverse",
					"description": "Reverses the text it is given.",
					"inputSchema": map[string]any{"type": "object"},
				}}}})
		case "tools/call":
			text, _ := msg.Params.Arguments["text"].(string)
			runes := []rune(text)
			for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
				runes[i], runes[j] = runes[j], runes[i]
			}
			reply(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{
				"content": []map[string]any{{"type": "text", "text": string(runes)}}, "isError": false}})
		default:
			reply(map[string]any{"jsonrpc": "2.0", "id": msg.ID,
				"error": map[string]any{"code": -32601, "message": "no method " + msg.Method}})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestManagerDiscoversAndCallsARealHTTPServerWithItsBearer is the link that had NO test: manager_test.go
// used httptest zero times, so nothing proved the Manager could reach an HTTP MCP server at all — only
// that the transport parsed bytes (http_test.go) and that the client framed messages (client_test.go).
//
// It is the runtime half of the chain the console now opens: an operator defines a connection, names it
// on an agent revision, and a run on some machine reaches THAT server with THAT credential. The server
// here refuses any other bearer, so a passing Call proves the secret_ref was redeemed and sent — a server
// that ignored authentication would let this pass with the resolver returning nothing.
func TestManagerDiscoversAndCallsARealHTTPServerWithItsBearer(t *testing.T) {
	const bearer = "probe-bearer-value-not-real"
	srv := mcpProbeServer(t, bearer)

	m := NewManager(Config{
		// AllowPrivate, because httptest binds loopback — the same flag a self-hosted deployment sets when
		// its MCP server is on its own network (PALAI_MCP_ALLOW_PRIVATE).
		AllowPrivate: true,
		Secrets: func(_ coordinator.Tenant, ref string) ([]byte, error) {
			if ref != "probe-ref" {
				return nil, errors.New("unknown ref " + ref)
			}
			return []byte(bearer), nil
		},
		DefaultTimeout: 10 * time.Second,
	})
	conn := ConnConfig{ID: "mcpc_probe", Project: "prj_probe", Name: "probe", Transport: "http", URL: srv.URL, SecretRef: "probe-ref"}

	tools, err := m.Discover(context.Background(), conn)
	if err != nil {
		t.Fatalf("Discover against a real HTTP MCP server: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "reverse" {
		t.Fatalf("discovered %+v, want the server's one tool", tools)
	}

	out, err := m.Call(context.Background(), CallScope{Project: "prj_probe", RunID: "run_probe"}, conn, "reverse",
		map[string]any{"text": "palai"})
	if err != nil {
		t.Fatalf("Call against a real HTTP MCP server: %v", err)
	}
	// The result must be the SERVER's answer, which is the only thing that distinguishes a working chain
	// from a transport that returned an empty envelope.
	if !strings.Contains(fmt.Sprint(out), "ialap") {
		t.Fatalf("the server's answer did not come back: %v", out)
	}
}

// TestAWrongBearerIsRefusedRatherThanSilentlySucceeding — the negative that makes the test above mean
// something. If the resolver hands back the wrong value the call must FAIL: a chain that "works" with any
// credential is a chain that was never carrying one.
func TestAWrongBearerIsRefusedRatherThanSilentlySucceeding(t *testing.T) {
	srv := mcpProbeServer(t, "the-right-one")
	m := NewManager(Config{
		AllowPrivate:   true,
		Secrets:        func(_ coordinator.Tenant, _ string) ([]byte, error) { return []byte("the-wrong-one"), nil },
		DefaultTimeout: 10 * time.Second,
	})
	conn := ConnConfig{ID: "mcpc_probe", Project: "prj_probe", Transport: "http", URL: srv.URL, SecretRef: "probe-ref"}

	if _, err := m.Discover(context.Background(), conn); err == nil {
		t.Fatal("a wrong bearer discovered tools: the credential is not reaching the server, so every other " +
			"assertion about it proves nothing")
	}
}
