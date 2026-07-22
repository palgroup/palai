package mcp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/palgroup/palai/packages/egress"
)

// captureServer is a TLS MCP double that records every request's Authorization + Origin headers, so a test
// can prove exactly what the transport sent (and, crucially, what it did NOT).
type captureServer struct {
	mu     sync.Mutex
	auth   []string
	origin []string
}

func (c *captureServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.auth = append(c.auth, r.Header.Get("Authorization"))
		c.origin = append(c.origin, r.Header.Get("Origin"))
		c.mu.Unlock()
		var req struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method == "initialize" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"` + ProtocolVersion + `"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

// connBearer is a distinctive sentinel for the CONNECTION's own upstream credential (the only Authorization
// the transport may send). platformToken is the platform-session sentinel that must appear on NO MCP request
// — the confused-deputy invariant (TOL-009): the transport structurally has no platform token to forward.
const (
	connBearer    = "conn-OWN-upstream-bearer-a1b2c3"
	platformToken = "platform-SESSION-token-must-never-leak-9f8e7d"
)

// TestUpstreamTokenNeverForwardedToMCPServer proves TOL-009: the ONLY Authorization an MCP HTTP transport
// sends is the connection's OWN resolved bearer. The platform session token appears in no header on any
// MCP-bound request — the transport has no field that could carry it, and this pins that it stays so.
func TestUpstreamTokenNeverForwardedToMCPServer(t *testing.T) {
	cap := &captureServer{}
	srv := httptest.NewTLSServer(cap.handler())
	defer srv.Close()

	tr, err := NewHTTPTransport(HTTPOptions{URL: srv.URL, Bearer: connBearer, AllowPrivate: true, TLSConfig: trust(srv)})
	if err != nil {
		t.Fatalf("construct transport: %v", err)
	}
	if err := NewClient(tr).Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.auth) == 0 {
		t.Fatal("server captured no request")
	}
	for i, a := range cap.auth {
		if a != "Bearer "+connBearer {
			t.Fatalf("request %d Authorization = %q, want the connection's own bearer only", i, a)
		}
		if strings.Contains(a, platformToken) {
			t.Fatalf("request %d leaked the platform token in Authorization", i)
		}
	}
}

// TestHTTPOriginValidated proves the POST carries an Origin header derived from the registered URL (a
// server's DNS-rebinding defence has something to pin), and that it is the URL's own origin — never migrated.
func TestHTTPOriginValidated(t *testing.T) {
	cap := &captureServer{}
	srv := httptest.NewTLSServer(cap.handler())
	defer srv.Close()

	wantOrigin, err := OriginOf(srv.URL)
	if err != nil {
		t.Fatalf("origin of test url: %v", err)
	}
	tr, err := NewHTTPTransport(HTTPOptions{URL: srv.URL, AllowPrivate: true, TLSConfig: trust(srv)})
	if err != nil {
		t.Fatalf("construct transport: %v", err)
	}
	if err := NewClient(tr).Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	for i, o := range cap.origin {
		if o != wantOrigin {
			t.Fatalf("request %d Origin = %q, want %q (the registered URL's origin)", i, o, wantOrigin)
		}
	}
}

// TestAudienceMismatchDenied proves TOL-009's replay defence: a connection whose bearer is bound to audience
// A but whose URL origin is B is DENIED at construction — connection A's token can never be dialed at B. A
// matching audience (or none) is allowed.
func TestAudienceMismatchDenied(t *testing.T) {
	// Bearer bound to a DIFFERENT origin than the URL → denied (a replay to another upstream).
	_, err := NewHTTPTransport(HTTPOptions{URL: "https://server-b.example.test/mcp", Audience: "https://server-a.example.test", Bearer: connBearer})
	if !errors.Is(err, ErrAudienceMismatch) || !errors.Is(err, egress.ErrDenied) {
		t.Fatalf("audience mismatch: err = %v, want ErrAudienceMismatch (⊂ egress.ErrDenied)", err)
	}
	// Audience matching the URL origin is fine.
	if _, err := NewHTTPTransport(HTTPOptions{URL: "https://server-a.example.test/mcp", Audience: "https://server-a.example.test", Bearer: connBearer}); err != nil {
		t.Fatalf("matching audience: err = %v, want nil", err)
	}
	// No audience declared opts out of the extra binding (the URL origin IS the audience).
	if _, err := NewHTTPTransport(HTTPOptions{URL: "https://server-a.example.test/mcp", Bearer: connBearer}); err != nil {
		t.Fatalf("no audience: err = %v, want nil", err)
	}
}

// TestMetadataFetchSSRFVetted proves the create/discover fail-fast gate (Manager.VetConnection): an http
// connection whose URL RESOLVES to a private address is rejected at registration — before it is ever dialed.
// A stdio connection has no egress, so it is not vetted. Every reject is egress.ErrDenied.
func TestMetadataFetchSSRFVetted(t *testing.T) {
	m := NewManager(Config{Resolver: staticResolver{ip: "10.0.0.5"}})
	ctx := context.Background()

	if err := m.VetConnection(ctx, ConnConfig{Transport: "http", URL: "https://mcp.example.test/mcp"}); !errors.Is(err, egress.ErrDenied) {
		t.Fatalf("http url resolving private: VetConnection err = %v, want egress.ErrDenied", err)
	}
	// A literal internal IP is denied by the static half even without resolution.
	if err := m.VetConnection(ctx, ConnConfig{Transport: "http", URL: "https://169.254.169.254/mcp"}); !errors.Is(err, egress.ErrDenied) {
		t.Fatalf("http url to metadata IP: VetConnection err = %v, want egress.ErrDenied", err)
	}
	// A stdio connection has no network egress — nothing to vet.
	if err := m.VetConnection(ctx, ConnConfig{Transport: "stdio", ImageDigest: "sha256:" + zeros64(), Cmd: []string{"/mcp"}}); err != nil {
		t.Fatalf("stdio VetConnection err = %v, want nil (no egress)", err)
	}
}

// TestPassiveOAuthValidator proves the passive PKCE/exact-redirect gate (E12 T6 step 8): a declared OAuth
// block MUST use PKCE S256 and an exact https redirect; an absent block is a no-op; a client_secret (or any
// non-allowlisted key) is a reject — the block never opens an inline-credential door. HONEST CEILING: there
// is NO interactive flow (E17) — this validates shape only.
func TestPassiveOAuthValidator(t *testing.T) {
	if err := ValidateOAuthMetadata(nil); err != nil {
		t.Fatalf("absent oauth block: err = %v, want nil", err)
	}
	ok := map[string]any{"code_challenge_method": "S256", "redirect_uri": "https://app.example.test/callback"}
	if err := ValidateOAuthMetadata(ok); err != nil {
		t.Fatalf("valid oauth block: err = %v, want nil", err)
	}
	// Present-and-valid endpoints stay accepted (they are https URLs, not a hiding place for a secret).
	okEndpoints := map[string]any{"authorization_endpoint": "https://idp.example.test/authorize", "token_endpoint": "https://idp.example.test/token", "code_challenge_method": "S256", "redirect_uri": "https://app.example.test/cb"}
	if err := ValidateOAuthMetadata(okEndpoints); err != nil {
		t.Fatalf("valid oauth block with endpoints: err = %v, want nil", err)
	}
	for name, bad := range map[string]map[string]any{
		"plain PKCE":        {"code_challenge_method": "plain", "redirect_uri": "https://app.example.test/cb"},
		"wildcard redirect": {"code_challenge_method": "S256", "redirect_uri": "https://*.example.test/cb"},
		"http redirect":     {"code_challenge_method": "S256", "redirect_uri": "http://app.example.test/cb"},
		"inline secret":     {"code_challenge_method": "S256", "redirect_uri": "https://app.example.test/cb", "client_secret": "sk-oops"},
		// A secret hiding in an endpoint VALUE must be rejected — an endpoint that is not an https URL.
		"authz endpoint secret": {"authorization_endpoint": "sk-live-SECRET", "token_endpoint": "https://idp.example.test/token", "code_challenge_method": "S256", "redirect_uri": "https://app.example.test/cb"},
		"token endpoint secret": {"authorization_endpoint": "https://idp.example.test/authorize", "token_endpoint": "not-a-url", "code_challenge_method": "S256", "redirect_uri": "https://app.example.test/cb"},
	} {
		if err := ValidateOAuthMetadata(bad); !errors.Is(err, ErrOAuthMetadata) {
			t.Fatalf("%s: err = %v, want ErrOAuthMetadata", name, err)
		}
	}
}

// trust pins the httptest server's self-signed cert (no verification bypass).
func trust(srv *httptest.Server) *tls.Config {
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return &tls.Config{RootCAs: pool}
}
