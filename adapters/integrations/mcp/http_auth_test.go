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
)

// authCapture is a minimal MCP server that records the Authorization header of every request it serves. It
// answers just enough of the protocol for Initialize to complete.
type authCapture struct {
	mu   sync.Mutex
	seen []string
}

func (a *authCapture) header(i int) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if i >= len(a.seen) {
		return ""
	}
	return a.seen[i]
}

func (a *authCapture) serve() *httptest.Server {
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.seen = append(a.seen, r.Header.Get("Authorization"))
		a.mu.Unlock()
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method == "initialize" {
			writeJSON(w, req.ID, map[string]any{"protocolVersion": ProtocolVersion})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
}

// dialCapture builds a transport pointed at srv with the given secret, trusting only srv's own certificate.
func dialCapture(t *testing.T, srv *httptest.Server, secret string) (Transport, error) {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return NewHTTPTransport(HTTPOptions{
		URL:          srv.URL,
		Bearer:       secret,
		AllowPrivate: true,
		TLSConfig:    &tls.Config{RootCAs: pool},
	})
}

// TestHTTPAuthorizationSchemeComesFromTheSecret proves the transport sends the scheme the UPSTREAM asked for,
// asserted on the bytes the SERVER received rather than on the helper's return value.
//
// This is what makes the Atlassian Rovo MCP server reachable: a personal API token authenticates as
// "Basic base64(email:api_token)" while a service-account key is "Bearer <key>"
// (support.atlassian.com/atlassian-rovo-mcp-server/docs/configuring-authentication-via-api-token/, fetched
// 2026-07-27). Before this, the transport hardcoded "Bearer "+secret, so a Basic credential went out as
// "Bearer Basic ..." and every request 401'd.
func TestHTTPAuthorizationSchemeComesFromTheSecret(t *testing.T) {
	// A secret that names Basic is sent VERBATIM — not re-wrapped in Bearer.
	basic := "Basic ZXhhbXBsZUBleGFtcGxlLmNvbTpub3QtYS1yZWFsLXRva2Vu"
	t.Run("basic is sent verbatim", func(t *testing.T) {
		cap := &authCapture{}
		srv := cap.serve()
		defer srv.Close()
		tr, err := dialCapture(t, srv, basic)
		if err != nil {
			t.Fatalf("construct transport: %v", err)
		}
		if err := NewClient(tr).Initialize(context.Background()); err != nil {
			t.Fatalf("initialize: %v", err)
		}
		if got := cap.header(0); got != basic {
			t.Fatalf("Authorization header = %q, want the Basic credential verbatim %q", got, basic)
		}
	})

	// A BARE secret (no scheme) keeps the historical Bearer default, so connections registered before the
	// scheme was honoured are bit-unchanged.
	t.Run("a bare secret stays Bearer", func(t *testing.T) {
		cap := &authCapture{}
		srv := cap.serve()
		defer srv.Close()
		tr, err := dialCapture(t, srv, "not-a-real-token")
		if err != nil {
			t.Fatalf("construct transport: %v", err)
		}
		if err := NewClient(tr).Initialize(context.Background()); err != nil {
			t.Fatalf("initialize: %v", err)
		}
		if got := cap.header(0); got != "Bearer not-a-real-token" {
			t.Fatalf("Authorization header = %q, want the bare secret defaulted to Bearer", got)
		}
	})

	// No secret ⇒ no Authorization header at all (never an empty "Bearer ").
	t.Run("no secret sends no header", func(t *testing.T) {
		cap := &authCapture{}
		srv := cap.serve()
		defer srv.Close()
		tr, err := dialCapture(t, srv, "")
		if err != nil {
			t.Fatalf("construct transport: %v", err)
		}
		if err := NewClient(tr).Initialize(context.Background()); err != nil {
			t.Fatalf("initialize: %v", err)
		}
		if got := cap.header(0); got != "" {
			t.Fatalf("Authorization header = %q, want none for a credential-less connection", got)
		}
	})
}

// TestHTTPAuthorizationRejectsHeaderInjection proves a secret carrying CR/LF/NUL is a TERMINAL construction
// error, not a spliced request. This is the one boundary that ships the credential off-box, so a malformed
// secret must fail closed — and the error must not echo the secret.
func TestHTTPAuthorizationRejectsHeaderInjection(t *testing.T) {
	needle := "s3cr3t-needle"
	for name, secret := range map[string]string{
		"CRLF header splice": "Bearer " + needle + "\r\nX-Injected: yes",
		"bare LF":            needle + "\n",
		"NUL":                "Basic " + needle + "\x00",
	} {
		_, err := NewHTTPTransport(HTTPOptions{URL: "https://mcp.example.test/mcp", Bearer: secret})
		if !errors.Is(err, ErrProtocol) {
			t.Fatalf("%s: NewHTTPTransport err = %v, want ErrProtocol (fail closed)", name, err)
		}
		if strings.Contains(err.Error(), needle) {
			t.Fatalf("%s: the error echoes the credential — a secret must never reach an error string", name)
		}
	}
}
