package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/palgroup/palai/adapters/integrations/a2a"
	"github.com/palgroup/palai/adapters/integrations/webhook"
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

// stubOrgResolver is the minimal Admitter this wiring test needs: only ResolveOrganization is ever called
// (through newA2AScopeFunc, when an authed A2A route resolves its scope) — the test proves ROUTING/auth, not
// admission, so every other Admitter method is unreachable and left panicking.
type stubOrgResolver struct{ Admitter }

func (stubOrgResolver) ResolveOrganization(context.Context, string) (string, error) {
	return "org_1", nil
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
		ScopeFunc:  newA2AScopeFunc(stubOrgResolver{}),
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
	srv := &a2a.Server{Interfaces: stubIfaceStore{iface: iface}, ScopeFunc: newA2AScopeFunc(stubOrgResolver{}), BaseURL: "https://cp.test"}

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

// TestA2APushSurfaceStaysUnmountedWithoutAConfiguredPusher is the typed-nil guard on the D13 gate. The whole
// gate is one `s.Pusher != nil` comparison, and in Go a nil *WebhookPusher assigned into the Pusher
// interface field makes that comparison TRUE — which would re-mount the CRUD and re-advertise push on a
// deployment that configured no pusher, i.e. exactly the silent-drop surface D13 exists to abolish. This is
// the SUP-2 rule applied to an interface comparison.
func TestA2APushSurfaceStaysUnmountedWithoutAConfiguredPusher(t *testing.T) {
	iface := a2a.PublishedInterface{
		ID: "a2aif_wire", Organization: "org_1", Project: "prj_1",
		Name: "Wired", Version: "1", PushNotifications: true, AuthScheme: "bearer",
	}
	srv := NewA2AServer(nil, stubIfaceStore{iface: iface}, nil, AdmissionLimits{}, "https://cp.test", nil)

	ts := httptest.NewServer(NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil,
		WithA2A(srv, srv.PublicCardHandler())))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/a2a/interfaces/a2aif_wire/agent-card.json")
	if err != nil {
		t.Fatalf("get card: %v", err)
	}
	defer resp.Body.Close()
	var card struct {
		Capabilities struct {
			PushNotifications bool `json:"pushNotifications"`
		} `json:"capabilities"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&card); err != nil {
		t.Fatalf("decode card: %v", err)
	}
	if card.Capabilities.PushNotifications {
		t.Fatalf("card advertises push with no configured pusher — a typed-nil *WebhookPusher defeated the `Pusher != nil` gate")
	}
	if code := get(t, ts.URL+"/v1/a2a/interfaces/a2aif_wire/tasks/t1/pushNotificationConfigs", "Bearer any"); code != http.StatusNotFound {
		t.Fatalf("pushNotificationConfigs with no configured pusher = %d, want 404", code)
	}
}

// TestA2APushMountsOnlyWithAConfiguredPusher pins the other half of the D13 gate: once a pusher IS
// configured, the card advertises push AND the CRUD is reachable. Card-says-true / CRUD-404s is just as
// dishonest as the reverse, and both directions now derive from the same condition.
func TestA2APushMountsOnlyWithAConfiguredPusher(t *testing.T) {
	iface := a2a.PublishedInterface{
		ID: "a2aif_wire", Organization: "org_1", Project: "prj_1",
		Name: "Wired", Version: "1", PushNotifications: true, AuthScheme: "bearer",
	}
	pusher := a2a.NewWebhookPusher(webhook.NewSender(), a2a.PushPolicy{AllowedHosts: []string{"sink.example.test"}})
	srv := NewA2AServer(nil, stubIfaceStore{iface: iface}, stubTasks{}, AdmissionLimits{}, "https://cp.test", pusher)

	ts := httptest.NewServer(NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil,
		WithA2A(srv, srv.PublicCardHandler())))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/a2a/interfaces/a2aif_wire/agent-card.json")
	if err != nil {
		t.Fatalf("get card: %v", err)
	}
	defer resp.Body.Close()
	var card struct {
		Capabilities struct {
			PushNotifications bool `json:"pushNotifications"`
		} `json:"capabilities"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&card); err != nil {
		t.Fatalf("decode card: %v", err)
	}
	if !card.Capabilities.PushNotifications {
		t.Fatalf("card advertises no push with a pusher configured — the card and the mounted surface disagree")
	}
	// Reachable (the task itself does not exist, so 404-for-that-task is fine; what must NOT happen is the
	// route being absent). An unknown A2A OPERATION and an unknown TASK are distinguished by the body.
	code := get(t, ts.URL+"/v1/a2a/interfaces/a2aif_wire/tasks/t1/pushNotificationConfigs", "Bearer any")
	if code == http.StatusNotFound {
		r2, err := http.NewRequest(http.MethodGet, ts.URL+"/v1/a2a/interfaces/a2aif_wire/tasks/t1/pushNotificationConfigs", nil)
		if err != nil {
			t.Fatal(err)
		}
		r2.Header.Set("Authorization", "Bearer any")
		got, err := http.DefaultClient.Do(r2)
		if err != nil {
			t.Fatal(err)
		}
		defer got.Body.Close()
		var body map[string]any
		_ = json.NewDecoder(got.Body).Decode(&body)
		if e, ok := body["error"].(map[string]any); ok && e["message"] == "unknown A2A operation" {
			t.Fatalf("pushNotificationConfigs route is UNMOUNTED with a pusher configured: %v", body)
		}
	}
}

// stubTasks is an empty Tasks store: every lookup misses, which is enough to prove the ROUTE is mounted.
type stubTasks struct{}

func (stubTasks) Put(context.Context, string, string, a2a.TaskRef) error { return nil }
func (stubTasks) GetRef(context.Context, string, string, string, string) (a2a.TaskRef, bool, error) {
	return a2a.TaskRef{}, false, nil
}
func (stubTasks) GetRefByRun(context.Context, string, string, string, string) (a2a.TaskRef, bool, error) {
	return a2a.TaskRef{}, false, nil
}
func (stubTasks) List(context.Context, string, string, string, int) ([]a2a.TaskRef, error) {
	return nil, nil
}
func (stubTasks) SetPushConfigs(context.Context, string, string, string, string, []a2a.PushNotificationConfig) error {
	return nil
}
