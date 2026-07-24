package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/palgroup/palai/adapters/integrations/a2a"
)

// The PRODUCTION ScopeFunc the A2A server reads its authenticated tenant from lives in a2a.go
// (a2aScopeFunc): the scope the auth middleware published from the verified bearer, the ONLY identity
// authority — never anything the A2A client supplies (§38.6). This test drives that SAME production func
// through the real router, so the wiring below is live, not a test stand-in.

// stubIfaceStore is a minimal InterfaceStore for the wiring test: it serves one published interface. The
// card/task Runs seams are not needed to prove the router MOUNT + auth boundary.
type stubIfaceStore struct{ iface a2a.PublishedInterface }

func (s stubIfaceStore) ResolvePublic(_ context.Context, id string) (a2a.PublishedInterface, bool, error) {
	if id == s.iface.ID {
		return s.iface, true, nil
	}
	return a2a.PublishedInterface{}, false, nil
}

func (s stubIfaceStore) Get(_ context.Context, org, project, id string) (a2a.PublishedInterface, bool, error) {
	if id == s.iface.ID && org == s.iface.Organization && project == s.iface.Project {
		return s.iface, true, nil
	}
	return a2a.PublishedInterface{}, false, nil
}

// TestA2ARouterWiringEnforcesAuthBoundary proves WithA2A mounts the surface correctly: the public Agent Card
// is reachable WITHOUT a bearer (it bypasses auth on the top mux — a safe published projection, A2A-001),
// while an authed A2A route is rejected by the router's auth middleware when no bearer is presented and
// resolves under the verified scope when one is. This exercises the production ScopeFunc plumbing
// (middleware.Auth -> ScopeFrom -> a2a.Server) that NewA2AServer + main.go activate — the same wiring, so
// this is a live-wiring proof, not dead code.
func TestA2ARouterWiringEnforcesAuthBoundary(t *testing.T) {
	iface := a2a.PublishedInterface{
		ID: "a2aif_wire", Organization: "org_1", Project: "prj_1",
		Name: "Wired", Version: "1", Streaming: true, ExtendedCard: true, AuthScheme: "bearer",
	}
	srv := &a2a.Server{
		Interfaces: stubIfaceStore{iface: iface},
		ScopeFunc:  a2aScopeFunc,
		BaseURL:    "https://cp.test",
		NewID:      func(p string) string { return p + "_x" },
	}
	router := NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil,
		WithA2A(srv, srv.PublicCardHandler()))
	ts := httptest.NewServer(router)
	defer ts.Close()

	// Public card: no bearer -> 200 (bypasses auth).
	if code := get(t, ts.URL+"/v1/a2a/interfaces/a2aif_wire/agent-card.json", ""); code != http.StatusOK {
		t.Fatalf("public card without bearer = %d, want 200 (must bypass auth)", code)
	}
	// Authed route: no bearer -> 401 (router auth middleware rejects before the handler).
	if code := get(t, ts.URL+"/v1/a2a/interfaces/a2aif_wire/extendedAgentCard", ""); code != http.StatusUnauthorized {
		t.Fatalf("extended card without bearer = %d, want 401 (auth must be enforced)", code)
	}
	// Authed route: with bearer -> 200, resolved under the verified scope (org_1/prj_1).
	if code := get(t, ts.URL+"/v1/a2a/interfaces/a2aif_wire/extendedAgentCard", "Bearer any"); code != http.StatusOK {
		t.Fatalf("extended card with bearer = %d, want 200", code)
	}
}

// TestA2ACapabilityAdvertisedOnlyWhenMounted proves discovery never claims what the deployment cannot serve
// (§2, the workspacesCapability posture): `a2a` appears in GET /v1/capabilities ONLY when WithA2A actually
// mounted the backing surface. A binary that wires no A2A store advertises no `a2a` capability, so the
// discovery lie (advertise a2a while every A2A route 404s) cannot recur.
func TestA2ACapabilityAdvertisedOnlyWhenMounted(t *testing.T) {
	iface := a2a.PublishedInterface{ID: "a2aif_wire", Organization: "org_1", Project: "prj_1", Name: "Wired", Version: "1"}
	srv := &a2a.Server{Interfaces: stubIfaceStore{iface: iface}, ScopeFunc: a2aScopeFunc, BaseURL: "https://cp.test"}

	mounted := NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil,
		WithA2A(srv, srv.PublicCardHandler()))
	if got := capabilityValue(t, mounted, "a2a"); got != "preview" {
		t.Fatalf("with WithA2A mounted, a2a capability = %q, want preview", got)
	}

	unmounted := NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil)
	if got := capabilityValue(t, unmounted, "a2a"); got != "" {
		t.Fatalf("without WithA2A, a2a capability = %q, want absent (must not advertise an unmounted surface)", got)
	}
}

// capabilityValue fetches GET /v1/capabilities under a verified bearer and returns the named capability's
// tier ("" when the key is absent).
func capabilityValue(t *testing.T, router http.Handler, name string) string {
	t.Helper()
	ts := httptest.NewServer(router)
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/capabilities", nil)
	req.Header.Set("Authorization", "Bearer any")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("capabilities = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Capabilities map[string]string `json:"capabilities"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body.Capabilities[name]
}

func get(t *testing.T, url, auth string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	return resp.StatusCode
}
