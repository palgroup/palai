package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/palgroup/palai/adapters/integrations/a2a"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// a2aScopeFunc is the PRODUCTION ScopeFunc the A2A server reads its authenticated tenant from: the scope the
// auth middleware published from the verified bearer. It is the ONLY identity authority — never anything the
// A2A client supplies (§38.6). main.go wires exactly this.
func a2aScopeFunc(r *http.Request) (a2a.Scope, bool) {
	s, ok := middleware.ScopeFrom(r.Context())
	return a2a.Scope{Organization: s.Organization, Project: s.Project}, ok
}

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
// (middleware.Auth -> ScopeFrom -> a2a.Server), so the WithA2A option is live wiring, not dead code.
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
