package admin

import (
	"bytes"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeServerCAPEM writes an httptest TLS server's self-signed certificate to a PEM file — used as the CA
// trust anchor a client must pin to reach that server over https (the self-signed edge shape).
func writeServerCAPEM(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	der := srv.Certificate().Raw
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	path := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("write CA pem: %v", err)
	}
	return path
}

// TestCATrustReachesHTTPSEdge is the edge-trust enabling change (E14 T7): with --ca naming the self-signed
// server's certificate the admin CLI reaches the https edge; WITHOUT it the untrusted cert is rejected. This
// is the exact mechanism the self-host journey uses to provision THROUGH the production TLS edge.
func TestCATrustReachesHTTPSEdge(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer srv.Close()
	caFile := writeServerCAPEM(t, srv)
	t.Setenv("PALAI_BASE_URL", srv.URL)
	t.Setenv("PALAI_API_KEY", "admin-key-xyz")

	// With --ca the self-signed edge is trusted and the call succeeds.
	var out bytes.Buffer
	if err := Run("org", []string{"list", "--ca", caFile}, &out, strings.NewReader("")); err != nil {
		t.Fatalf("org list --ca should reach the https edge, got: %v", err)
	}

	// PALAI_CA_FILE is honored the same as the flag.
	out.Reset()
	t.Setenv("PALAI_CA_FILE", caFile)
	if err := Run("org", []string{"list"}, &out, strings.NewReader("")); err != nil {
		t.Fatalf("org list with PALAI_CA_FILE should reach the https edge, got: %v", err)
	}
	os.Unsetenv("PALAI_CA_FILE")

	// WITHOUT a CA the self-signed cert is untrusted — the call must fail (not silently connect).
	out.Reset()
	if err := Run("org", []string{"list"}, &out, strings.NewReader("")); err == nil {
		t.Fatal("org list with no CA must reject the untrusted self-signed edge cert")
	}

	// A named-but-unreadable CA is a hard error, never a silent fall-through to the system trust store.
	out.Reset()
	if err := Run("org", []string{"list", "--ca", filepath.Join(t.TempDir(), "missing.crt")}, &out, strings.NewReader("")); err == nil || !strings.Contains(err.Error(), "read --ca file") {
		t.Fatalf("a missing --ca file must be a hard error, got: %v", err)
	}

	// A CA file with no PEM certificates is a hard error too.
	empty := filepath.Join(t.TempDir(), "empty.crt")
	if err := os.WriteFile(empty, []byte("not a pem"), 0o600); err != nil {
		t.Fatalf("write empty ca: %v", err)
	}
	if err := Run("org", []string{"list", "--ca", empty}, &out, strings.NewReader("")); err == nil || !strings.Contains(err.Error(), "no PEM certificates") {
		t.Fatalf("a CA file with no certificates must be a hard error, got: %v", err)
	}
}
