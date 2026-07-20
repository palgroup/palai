package execution

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/packages/coordinator"
)

// TestPreviewRouteDeniesExpiredAndWrongTenant proves the §29.16 inbound-sandbox authorization
// (SAN-010): a preview route authorizes the caller on every connection, denies an expired route
// and a wrong-tenant caller, and never exposes the direct sandbox address on any path. It drives a
// real HTTP round-trip through the proxy handler with a real backend standing in for the sandbox.
func TestPreviewRouteDeniesExpiredAndWrongTenant(t *testing.T) {
	// A real backend stands in for the sandbox preview server behind the proxy.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "backend-ok")
	}))
	defer backend.Close()
	sandboxAddress := strings.TrimPrefix(backend.URL, "http://") // host:port the caller must never see

	tenantA := coordinator.Tenant{Organization: "org_a", Project: "proj_a"}
	tenantB := coordinator.Tenant{Organization: "org_b", Project: "proj_b"}

	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	clock := now
	authz := NewPreviewProxy(func() time.Time { return clock })

	// The caller identity is asserted per connection from a header the gateway sets after auth.
	caller := func(tenant coordinator.Tenant) http.Header {
		return http.Header{"X-Palai-Org": []string{tenant.Organization}, "X-Palai-Project": []string{tenant.Project}}
	}

	grant := authz.Grant(PreviewGrant{
		Tenant:    tenantA,
		SessionID: "sess_1",
		RunID:     "run_1",
		Target:    sandboxAddress,
		Protocols: []string{"http"},
		ExpiresAt: now.Add(time.Minute),
	})
	if strings.Contains(grant.Route, sandboxAddress) || grant.Route == "" {
		t.Fatalf("grant route must be a random token that does not embed the sandbox address, got %q", grant.Route)
	}

	srv := httptest.NewServer(authz)
	defer srv.Close()

	do := func(route string, hdr http.Header) (int, string) {
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/preview/"+route, nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.Header = hdr
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do request: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	// Authorized caller within expiry reaches the sandbox preview.
	if status, body := do(grant.Route, caller(tenantA)); status != http.StatusOK || body != "backend-ok" {
		t.Fatalf("authorized preview = %d %q, want 200 backend-ok", status, body)
	}

	// Wrong tenant is denied and learns nothing about the sandbox.
	if status, body := do(grant.Route, caller(tenantB)); status < 400 {
		t.Fatalf("wrong-tenant preview status = %d, want a denial", status)
	} else if strings.Contains(body, sandboxAddress) || strings.Contains(body, "backend-ok") {
		t.Fatalf("wrong-tenant denial leaked the sandbox: %q", body)
	}

	// An unknown/guessed route is denied without disclosure.
	if status, _ := do("frt_guessed", caller(tenantA)); status < 400 {
		t.Fatalf("unknown-route status = %d, want a denial", status)
	}

	// After expiry the same caller and route are denied, and the sandbox address is never exposed.
	clock = now.Add(2 * time.Minute)
	status, body := do(grant.Route, caller(tenantA))
	if status < 400 {
		t.Fatalf("expired preview status = %d, want a denial", status)
	}
	if strings.Contains(body, sandboxAddress) || strings.Contains(body, "backend-ok") {
		t.Fatalf("expired denial leaked the sandbox address or content: %q", body)
	}
}
