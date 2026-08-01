package store

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/packages/coordinator"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// TestModelRouteWritesRejectAnOrgGranularKey pins the E13 T8 review NIT 2: an ORG-granular provision key
// (the T2 shape — Scope.Project == "") is a LEGITIMATE key, but every model-routing row is keyed by
// (organization, project). Without a guard such a key inserts project_id=” and the composite FK to
// projects rejects it, surfacing as a 500 for a well-formed request. It must be a 400 naming what is
// missing. The guard short-circuits before any query, so this needs no database.
func TestModelRouteWritesRejectAnOrgGranularKey(t *testing.T) {
	s := &Store{}
	ctx := context.Background()
	orgOnly := middleware.Scope{Organization: "org_1"}

	conn, err := s.CreateModelConnection(ctx, orgOnly, []byte(`{"provider":"provider-one","secret_ref":"openai"}`))
	if err != nil || conn.MissingField == "" {
		t.Fatalf("CreateModelConnection(org-granular key) = (%+v, %v), want a MissingField reject (400), not a DB error", conn, err)
	}
	route, err := s.CreateModelRoute(ctx, orgOnly, []byte(`{"name":"default"}`))
	if err != nil || route.MissingField == "" {
		t.Fatalf("CreateModelRoute(org-granular key) = (%+v, %v), want a MissingField reject (400)", route, err)
	}
	rev, err := s.CreateModelRouteRevision(ctx, orgOnly, "mroute_1", []byte(`{"model":"m","connection_id":"mconn_1"}`))
	if err != nil || rev.MissingField == "" {
		t.Fatalf("CreateModelRouteRevision(org-granular key) = (%+v, %v), want a MissingField reject (400)", rev, err)
	}
	pub, err := s.PublishModelRouteRevision(ctx, orgOnly, "mroute_1", "mrev_1")
	if err != nil || pub.MissingField == "" {
		t.Fatalf("PublishModelRouteRevision(org-granular key) = (%+v, %v), want a MissingField reject (400)", pub, err)
	}
}

// TestModelRouteReadsRejectAnOrgGranularKey is the read-back partner (E16 T1, review SF-2) of the write
// guard above: the six read-back methods must short-circuit on an ORG-granular key (Scope.Project == "")
// with the same MissingField reject (400) BEFORE any store query — model routing is per project. Without
// requireProjectScope on a read, an org-granular key would reach the (here nil) spine, panicking or 500ing
// a well-formed request. This is the fast glue-layer regression guard partnering the real-Postgres
// tenant-scoping proof (tests/component/postgres TestModelRouteReadsAreTenantScoped); it needs no database.
func TestModelRouteReadsRejectAnOrgGranularKey(t *testing.T) {
	s := &Store{}
	ctx := context.Background()
	orgOnly := middleware.Scope{Organization: "org_1"}

	reads := []struct {
		name string
		call func() (api.ProvisionResult, error)
	}{
		{"ListModelConnections", func() (api.ProvisionResult, error) { return s.ListModelConnections(ctx, orgOnly) }},
		{"GetModelConnection", func() (api.ProvisionResult, error) { return s.GetModelConnection(ctx, orgOnly, "mconn_1") }},
		{"ListModelRoutes", func() (api.ProvisionResult, error) { return s.ListModelRoutes(ctx, orgOnly) }},
		{"GetModelRoute", func() (api.ProvisionResult, error) { return s.GetModelRoute(ctx, orgOnly, "mroute_1") }},
		{"ListModelRouteRevisions", func() (api.ProvisionResult, error) { return s.ListModelRouteRevisions(ctx, orgOnly, "mroute_1") }},
		{"GetModelRouteRevision", func() (api.ProvisionResult, error) {
			return s.GetModelRouteRevision(ctx, orgOnly, "mroute_1", "mrev_1")
		}},
	}
	for _, r := range reads {
		out, err := r.call()
		if err != nil || out.MissingField == "" {
			t.Fatalf("%s(org-granular key) = (%+v, %v), want a MissingField reject (400), not a store call", r.name, out, err)
		}
	}
}

// AN UNKNOWN PROVIDER FAMILY IS REFUSED AT THE FORM, NOT AT 3AM (E29 provider wiring).
//
// The create used to accept ANY string for `provider`. So `{"provider":"openai"}` — the name the product's
// own prose uses, and the first thing anybody types — got a 201, was stored, was published onto a route,
// and then killed the first model step of the first run with `unknown_provider: openai`. Nothing between
// the form and that run had a reason to look at the value, because nothing knew what the accepted values
// were: the adapter map was a literal in the composition root.
//
// The families now live in modelbroker.Families(), which is also what the adapter map is BUILT from
// (adapters/models/registry), so this validation cannot drift away from what the broker can dial.
//
// The guard runs before any query, so this needs no database.
func TestCreateModelConnectionRefusesAnUnknownProviderFamily(t *testing.T) {
	s := &Store{}
	scope := middleware.Scope{Organization: "org_1", Project: "prj_1"}

	for _, provider := range []string{"openai", "anthropic", "gpt-4o-mini", "Provider-One", ""} {
		out, err := s.CreateModelConnection(context.Background(), scope,
			[]byte(`{"provider":`+quote(provider)+`,"secret_ref":"k"}`))
		if err != nil {
			t.Fatalf("provider %q returned an error, want a 400 reject: %v", provider, err)
		}
		if out.MissingField == "" && !out.BadField {
			t.Fatalf("provider %q was ACCEPTED: it is not a family the broker can dial, so this connection "+
				"is a row whose first run dies with unknown_provider", provider)
		}
	}
}

// The three shapes the product promises, and the endpoint rule that separates them.
//
// A base URL is accepted ONLY by the custom family, and REQUIRED by it. Both halves matter and the second
// is the dangerous one: a custom connection with no endpoint silently falls back to the family default,
// which is api.openai.com — so an operator who meant to keep their prompts on their own vLLM would have
// been shipping them to OpenAI, with a key they minted for something else, and nothing would have said so.
func TestCreateModelConnectionEndpointRule(t *testing.T) {
	s := &Store{}
	scope := middleware.Scope{Organization: "org_1", Project: "prj_1"}
	call := func(body string) (api.ProvisionResult, error) {
		return s.CreateModelConnection(context.Background(), scope, []byte(body))
	}

	// A custom connection with no endpoint is refused rather than silently pointed at OpenAI.
	if out, err := call(`{"provider":"openai-compatible","secret_ref":"k"}`); err != nil || out.MissingField == "" {
		t.Fatalf("custom family with no base_url = (%+v, %v), want a MissingField reject — an empty endpoint "+
			"means this connection dials api.openai.com with the operator's private-endpoint key", out, err)
	}
	// A base URL on a family that HAS an endpoint is a custom connection wearing a disguise.
	if out, err := call(`{"provider":"provider-one","secret_ref":"k","base_url":"https://example.test/v1/chat/completions"}`); err != nil || (out.MissingField == "" && !out.BadField) {
		t.Fatalf("provider-one with a base_url = (%+v, %v), want a reject", out, err)
	}
	// A scheme the control plane must never dial.
	if out, err := call(`{"provider":"openai-compatible","secret_ref":"k","base_url":"file:///etc/passwd"}`); err != nil || (out.MissingField == "" && !out.BadField) {
		t.Fatalf("file:// base_url = (%+v, %v), want a reject", out, err)
	}
	// The cloud metadata address stays denied even though a self-host endpoint may be private.
	if out, err := call(`{"provider":"openai-compatible","secret_ref":"k","base_url":"http://169.254.169.254/v1/chat/completions"}`); err != nil || (out.MissingField == "" && !out.BadField) {
		t.Fatalf("metadata-address base_url = (%+v, %v), want a reject", out, err)
	}
	// A CREDENTIAL VALUE STILL CANNOT BE INLINED. base_url is a new accepted field, and the risk a new
	// field brings to a strictly-decoded body is that the decode stops being strict.
	if out, err := call(`{"provider":"provider-one","secret_ref":"k","secret_value":"sk-live"}`); err != nil || !out.BadField {
		t.Fatalf("inlined secret_value = (%+v, %v), want a BadField reject", out, err)
	}
}

// quote renders a JSON string literal, so a table can carry the empty string without hand-written escaping.
func quote(s string) string { return `"` + s + `"` }

// THE PROBE LEAVES THE PROCESS, AND WHAT IT CANNOT DO IS ANSWER GREEN WITHOUT ASKING (E29).
//
// The brief this was built against named the failure precisely: "a test connection that only checks the
// string is non-empty is theatre. If a real probe is out of scope, say so and ship nothing rather than a
// fake green." So the two things worth pinning are (a) a real HTTP request carrying the real credential
// reaches the real endpoint, and (b) every path that reached NO verdict says `not_probed` rather than
// borrowing one that reads as a pass.
//
// The prober here is the PRODUCTION one — adapters/models/provider_one — against an httptest server, so
// what is measured is the shipped classification and not a double's idea of it.
func TestVerifyModelConnectionMakesARealRequest(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		w.WriteHeader(http.StatusUnauthorized) // the endpoint rejects this key
	}))
	defer srv.Close()

	rec := coordinator.ModelConnectionRecord{
		ID: "mconn_1", Provider: "openai-compatible", SecretRef: "vllm",
		BaseURL: srv.URL + "/v1/chat/completions",
	}
	s := (&Store{}).WithModelConnectionInspectors(
		map[string]ConnectionInspector{"openai-compatible": providerone.Adapter{}},
		func(org, ref string) ([]byte, bool, error) { return []byte("sk-the-operators-key"), true, nil },
	)

	probe := s.probeConnection(context.Background(), coordinator.Tenant{Organization: "org_1", Project: "prj_1"}, rec)

	// (a) A real request, carrying the real credential, at the endpoint's models list.
	if gotAuth != "Bearer sk-the-operators-key" {
		t.Fatalf("the endpoint received Authorization %q — the probe did not send the connection's own credential", gotAuth)
	}
	if gotPath != "/v1/models" {
		t.Fatalf("the probe hit %q, want the endpoint's models list", gotPath)
	}
	// (b) A 401 is a VERDICT, and it is the rejection one.
	if probe.Outcome != modelbroker.ProbeRejected {
		t.Fatalf("outcome = %q, want %q", probe.Outcome, modelbroker.ProbeRejected)
	}
	// THE CREDENTIAL IS IN NOTHING THE PROBE RETURNS. A boolean, never a matcher that prints its operands.
	rendered := probe.Endpoint + "\x00" + probe.Detail
	if strings.Contains(rendered, "sk-the-operators-key") {
		t.Fatal("the probe's own result carries the credential")
	}
}

// Every refusal that reached NO verdict answers `not_probed`, never a pass. Three of them, and the third —
// an endpoint whose shape yields no models list — is the one most likely to be quietly rendered as OK.
func TestVerifyModelConnectionNeverInventsAGreen(t *testing.T) {
	tenant := coordinator.Tenant{Organization: "org_1", Project: "prj_1"}
	rec := coordinator.ModelConnectionRecord{ID: "mconn_1", Provider: "provider-one", SecretRef: "k"}
	resolves := func(org, ref string) ([]byte, bool, error) { return []byte("sk-x"), true, nil }

	cases := map[string]*Store{
		"no prober wired for the family": (&Store{}).WithModelConnectionInspectors(map[string]ConnectionInspector{}, resolves),
		"no secret store wired":          (&Store{}).WithModelConnectionInspectors(map[string]ConnectionInspector{"provider-one": providerone.Adapter{}}, nil),
		"the secret ref does not resolve": (&Store{}).WithModelConnectionInspectors(
			map[string]ConnectionInspector{"provider-one": providerone.Adapter{}},
			func(org, ref string) ([]byte, bool, error) { return nil, false, nil }),
	}
	for name, s := range cases {
		probe := s.probeConnection(context.Background(), tenant, rec)
		if probe.Outcome != modelbroker.ProbeUnsupported {
			t.Errorf("%s: outcome = %q, want %q — nothing was checked, and any other value reads as a measurement",
				name, probe.Outcome, modelbroker.ProbeUnsupported)
		}
		if probe.Detail == "" {
			t.Errorf("%s: the refusal names no reason, so an operator cannot tell what to fix", name)
		}
	}

	// An endpoint whose shape yields no models list: the probe declines rather than guessing at a URL.
	weird := coordinator.ModelConnectionRecord{ID: "mconn_2", Provider: "openai-compatible", SecretRef: "k",
		BaseURL: "https://gateway.example.test/completions"}
	s := (&Store{}).WithModelConnectionInspectors(map[string]ConnectionInspector{"openai-compatible": providerone.Adapter{}}, resolves)
	if probe := s.probeConnection(context.Background(), tenant, weird); probe.Outcome != modelbroker.ProbeUnsupported {
		t.Fatalf("unusual endpoint shape: outcome = %q, want %q", probe.Outcome, modelbroker.ProbeUnsupported)
	}
}
