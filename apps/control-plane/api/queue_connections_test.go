package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/internal/automation"
)

// fakeQueueAPI records what reached the store seam.
type fakeQueueAPI struct {
	createdProject    string
	created           *automation.QueueConnectionInput
	items             []automation.QueueConnectionItem
	lastWindow        automation.ListWindow
	listedProject     string
	gotProject, gotID string
}

func (f *fakeQueueAPI) CreateQueueConnection(_ context.Context, project string, in automation.QueueConnectionInput) (string, error) {
	f.createdProject, f.created = project, &in
	return "qconn_1", nil
}

func (f *fakeQueueAPI) ListQueueConnections(_ context.Context, project string, w automation.ListWindow) ([]automation.QueueConnectionItem, error) {
	f.listedProject, f.lastWindow = project, w
	return f.items, nil
}

func (f *fakeQueueAPI) GetQueueConnectionItem(_ context.Context, project, id string) (automation.QueueConnectionItem, bool, error) {
	f.gotProject, f.gotID = project, id
	for _, it := range f.items {
		if it.ID == id {
			return it, true, nil
		}
	}
	return automation.QueueConnectionItem{}, false, nil
}

// publicResolver resolves every name to a routable public address, so the create-time egress vet exercises
// the POLICY rather than the test host's DNS.
type publicResolver struct{}

func (publicResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	return []net.IPAddr{{IP: net.ParseIP("203.0.113.10")}}, nil
}

func queueTestServer(t *testing.T, q QueueConnectionAPI) string {
	t.Helper()
	srv := httptest.NewServer(NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil,
		WithQueueConnections(q, publicResolver{})))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestQueueConnectionCreateTakesTenantFromTheVerifiedScope is the §39.2 pin: the binding lands in the
// BEARER's org/project, and a body that names another tenant changes nothing. The registration side of the
// same rule the bridge enforces on the message side.
func TestQueueConnectionCreateTakesTenantFromTheVerifiedScope(t *testing.T) {
	fake := &fakeQueueAPI{}
	base := queueTestServer(t, fake)

	resp := do(t, "POST", base+"/v1/queue-connections",
		`{"name":"orders","direction":"inbound","organization_id":"org_victim","project_id":"prj_victim",
		  "config":{"agent_revision_id":"arev_1","principal_id":"prin_1"}}`, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if loc != "/v1/queue-connections/qconn_1" {
		t.Fatalf("Location = %q, want /v1/queue-connections/qconn_1", loc)
	}
	// The header is FOLLOWED, not merely compared. Asserting the string alone is what let this address name
	// a route nobody mounted from the family's first day until E29 T2: the test was green the whole time,
	// because it proved the header was WRITTEN and never that it resolved.
	fake.items = []automation.QueueConnectionItem{{ID: "qconn_1", Name: "orders", Kind: "local", Direction: "inbound"}}
	followed := do(t, "GET", base+loc, "", nil)
	if followed.StatusCode != http.StatusOK {
		t.Fatalf("following the create's Location %q = %d, want 200; a 201 must not name an address the router does not serve", loc, followed.StatusCode)
	}
	var got map[string]any
	if err := json.NewDecoder(followed.Body).Decode(&got); err != nil {
		t.Fatalf("decode the followed body: %v", err)
	}
	if got["id"] != "qconn_1" || got["object"] != "queue_connection" {
		t.Fatalf("followed body = %v, want the created connection in the list's projection", got)
	}
	if fake.gotProject != "prj_1" {
		t.Fatalf("read ran in project %s, want the verified scope — a singular read takes its tenant from the bearer like the create does", fake.gotProject)
	}
	if fake.createdProject != "prj_1" {
		t.Fatalf("connection created in project %s, want the verified scope prj_1", fake.createdProject)
	}
	if fake.created.Kind != "local" {
		t.Fatalf("kind = %q, want local (the only adapter this binary has)", fake.created.Kind)
	}
	if fake.created.Visibility != 30*time.Second || fake.created.Capacity != 1024 || fake.created.MaxDeliveries != 20 {
		t.Fatalf("defaults not applied: %+v", *fake.created)
	}
}

// TestQueueConnectionCreateRejectsAtTheBoundary pins every refusal the create surface owes: an unwritten
// broker class, a bad direction, a config missing its run target or destination, an out-of-range knob, and
// — the one that is a security property rather than tidiness — an UNKNOWN config key. config is
// tenant-supplied JSONB the bridge reads a run target out of, so an open shape is where a credential or a
// second "tenant" field would get parked.
func TestQueueConnectionCreateRejectsAtTheBoundary(t *testing.T) {
	base := queueTestServer(t, &fakeQueueAPI{})
	for _, tc := range []struct{ name, body string }{
		{"no name", `{"direction":"inbound","config":{"agent_revision_id":"a","principal_id":"p"}}`},
		{"unwritten broker product", `{"name":"q","kind":"sqs","direction":"inbound","config":{"agent_revision_id":"a","principal_id":"p"}}`},
		{"bad direction", `{"name":"q","direction":"sideways","config":{}}`},
		{"no config", `{"name":"q","direction":"inbound"}`},
		{"inbound without a revision", `{"name":"q","direction":"inbound","config":{"principal_id":"p"}}`},
		{"inbound without a principal", `{"name":"q","direction":"inbound","config":{"agent_revision_id":"a"}}`},
		{"unknown inbound config key", `{"name":"q","direction":"inbound","config":{"agent_revision_id":"a","principal_id":"p","bearer_token":"secret"}}`},
		{"inbound config naming a tenant", `{"name":"q","direction":"inbound","config":{"agent_revision_id":"a","principal_id":"p","organization_id":"org_victim"}}`},
		{"outbound without a destination", `{"name":"q","direction":"outbound","config":{}}`},
		{"unknown outbound config key", `{"name":"q","direction":"outbound","config":{"destination_url":"https://sink.example/x","signing_secret":"shh"}}`},
		{"outbound to a loopback destination", `{"name":"q","direction":"outbound","config":{"destination_url":"http://127.0.0.1:9/x"}}`},
		{"outbound over plain http", `{"name":"q","direction":"outbound","config":{"destination_url":"http://sink.example/x"}}`},
		{"capacity out of range", `{"name":"q","direction":"inbound","capacity":999999,"config":{"agent_revision_id":"a","principal_id":"p"}}`},
		{"visibility out of range", `{"name":"q","direction":"inbound","visibility_seconds":99999,"config":{"agent_revision_id":"a","principal_id":"p"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if resp := do(t, "POST", base+"/v1/queue-connections", tc.body, nil); resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
		})
	}
}

// TestQueueConnectionCreateAllowsPrivateDestinationOnlyWithTheFlag mirrors the webhook-endpoint posture: a
// self-host deployment can opt a private destination in explicitly, and only explicitly.
func TestQueueConnectionCreateAllowsPrivateDestinationOnlyWithTheFlag(t *testing.T) {
	fake := &fakeQueueAPI{}
	base := queueTestServer(t, fake)
	resp := do(t, "POST", base+"/v1/queue-connections",
		`{"name":"results","direction":"outbound","config":{"destination_url":"http://127.0.0.1:8080/ingest","allow_private":true}}`, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 with the self-host flag set", resp.StatusCode)
	}
	var cfg map[string]any
	if err := json.Unmarshal(fake.created.Config, &cfg); err != nil {
		t.Fatalf("stored config is not JSON: %v", err)
	}
	if cfg["allow_private"] != true || cfg["destination_url"] != "http://127.0.0.1:8080/ingest" {
		t.Fatalf("stored config = %v, want the vetted destination + flag", cfg)
	}
}

// TestQueueConnectionListRendersThePageEnvelope pins the E16 T1 list shape: the store is asked for
// Limit+1 rows in the caller's own scope, and the response is the shared keyset envelope.
func TestQueueConnectionListRendersThePageEnvelope(t *testing.T) {
	fake := &fakeQueueAPI{items: []automation.QueueConnectionItem{
		{ID: "qconn_1", Name: "orders", Kind: "local", Direction: "inbound", Enabled: true,
			Config: json.RawMessage(`{"agent_revision_id":"arev_1","principal_id":"prin_1"}`), CreatedAt: time.Now()},
		{ID: "qconn_2", Name: "results", Kind: "local", Direction: "outbound", Enabled: true,
			Config: json.RawMessage(`{"destination_url":"https://sink.example/x"}`), CreatedAt: time.Now()},
	}}
	base := queueTestServer(t, fake)

	resp := do(t, "GET", base+"/v1/queue-connections?limit=2", ``, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200", resp.StatusCode)
	}
	var page struct {
		Data    []map[string]any `json:"data"`
		HasMore bool             `json:"has_more"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatalf("decode page: %v", err)
	}
	if len(page.Data) != 2 || page.HasMore {
		t.Fatalf("page = %d rows has_more=%v, want 2 rows and no further page", len(page.Data), page.HasMore)
	}
	if page.Data[0]["object"] != "queue_connection" || page.Data[0]["direction"] != "inbound" {
		t.Fatalf("row projection = %v", page.Data[0])
	}
	if fake.listedProject != "prj_1" {
		t.Fatalf("listed under project %s, want the verified scope", fake.listedProject)
	}
	if fake.lastWindow.Limit != 3 {
		t.Fatalf("store asked for Limit=%d, want limit+1 (the has_more over-fetch)", fake.lastWindow.Limit)
	}
}

// TestQueueConnectionRoutesUnmountedWithoutTheOption is the §2 discovery-honesty half at the routing level:
// a binary that wires no queue store serves no queue routes, which is what makes deriving `queues` from the
// mount meaningful rather than decorative.
func TestQueueConnectionRoutesUnmountedWithoutTheOption(t *testing.T) {
	srv := httptest.NewServer(NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil))
	t.Cleanup(srv.Close)
	for _, m := range []string{"GET", "POST"} {
		if resp := do(t, m, srv.URL+"/v1/queue-connections", `{}`, nil); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404 with no queue store wired", m, resp.StatusCode)
		}
	}
}
