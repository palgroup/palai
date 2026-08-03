package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/automation"
)

// The webhook-endpoint delete + singular read (E29 T3).
//
// This family shipped a create, a list, and nothing else. The create's own comment gave the ABSENCE of an
// Idempotency-Key a justification resting on a capability the tree did not have — "a duplicate endpoint is
// operator-visible + deletable" — and E29 T2 found the second half of that sentence load-bearing enough to
// write down while removing the dangling Location header it also explained. Nothing deleted an endpoint: no
// route, no store method, no SQL. An operator who mis-typed a URL had a permanent row.
//
// TestTheCreateCommentsDeletableClaimIsBackedByARoute is the RED that opened this task, and it is written
// against the COMMENT rather than against a remembered fact, so it fails again if the sentence returns
// without the route or the route leaves without the sentence.

// deletableClaim is the create comment's justification, verbatim. It is quoted rather than paraphrased
// because the guard below reads the shipped source for exactly this text: a paraphrase would drift
// silently, and drifting silently is what let this claim stand unbacked for eighteen epics.
const deletableClaim = "a duplicate endpoint is operator-visible + deletable"

// TestTheCreateCommentsDeletableClaimIsBackedByARoute reads the shipped source for the create's stated
// reason, then asks the shipped router whether that reason is true. Both halves matter: the claim without
// the route is the bug this task fixes, and the route without the claim would leave the create's missing
// Idempotency-Key unexplained.
func TestTheCreateCommentsDeletableClaimIsBackedByARoute(t *testing.T) {
	src, err := os.ReadFile("webhook_endpoints.go")
	if err != nil {
		t.Fatalf("read webhook_endpoints.go: %v", err)
	}
	if !strings.Contains(string(src), deletableClaim) {
		t.Fatalf("webhook_endpoints.go no longer carries the claim %q. If the create's reasoning changed, change "+
			"this guard with it — do not delete it: the claim is what makes the missing Idempotency-Key defensible.",
			deletableClaim)
	}

	fake := newFakeWebhookAPI("whe_1")
	srv := webhookTestServer(t, fake)

	resp := do(t, http.MethodDelete, srv.URL+"/v1/webhook-endpoints/whe_1", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		if body := readBody(t, resp); strings.Contains(body, "404 page not found") {
			t.Fatalf("webhook_endpoints.go says %q and the router serves no DELETE for the family: following the "+
				"claim gets Go's bare mux miss. The comment is the specification here, and it is false.", deletableClaim)
		}
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE /v1/webhook-endpoints/whe_1 = %d, want 204", resp.StatusCode)
	}
}

// TestDeletingAWebhookEndpointTwiceIs204Then404 pins the posture the two shipped precedents already take
// (DELETE /v1/schedules/{id} router.go:93, DELETE /v1/slack-connections/{id} router.go:353): the SECOND call
// produces no side effect, which is what DELETE being idempotent actually requires, and reports 404 because
// the resource is genuinely absent. A 204 on the second call is the other defensible answer and this tree
// does not take it — agreeing with the two routes an operator already knows is worth more than the argument
// between them.
func TestDeletingAWebhookEndpointTwiceIs204Then404(t *testing.T) {
	fake := newFakeWebhookAPI("whe_1")
	srv := webhookTestServer(t, fake)

	first := do(t, http.MethodDelete, srv.URL+"/v1/webhook-endpoints/whe_1", "", nil)
	first.Body.Close()
	if first.StatusCode != http.StatusNoContent {
		t.Fatalf("first DELETE = %d, want 204", first.StatusCode)
	}
	afterFirst := fake.deletes

	second := do(t, http.MethodDelete, srv.URL+"/v1/webhook-endpoints/whe_1", "", nil)
	defer second.Body.Close()
	if second.StatusCode != http.StatusNotFound {
		t.Fatalf("second DELETE = %d, want 404", second.StatusCode)
	}
	if fake.deletes != afterFirst+1 {
		t.Fatalf("the second DELETE reached the store %d time(s), want exactly 1 lookup that removes nothing",
			fake.deletes-afterFirst)
	}
	if len(fake.endpoints) != 0 {
		t.Fatalf("the second DELETE changed the store: %d endpoint(s) remain", len(fake.endpoints))
	}
}

// TestDeletingAWebhookEndpointRequiresTheProvisionCapability puts the destructive verb behind the gate every
// other org-admin surface uses. The create and the list are NOT gated, and that asymmetry is deliberate
// rather than overlooked: this task opens one route and gates it; it does not retro-gate two routes that
// have shipped ungated since E11, because narrowing a shipped route is a compatibility decision that earns
// its own review. The gap is real and it is reported rather than quietly closed or quietly ignored.
func TestDeletingAWebhookEndpointRequiresTheProvisionCapability(t *testing.T) {
	fake := newFakeWebhookAPI("whe_1")
	runOnly := scopedVerifier{middleware.Scope{Project: "prj_1", Scopes: []string{"responses"}}}
	srv := httptest.NewServer(NewRouter(runOnly, nil, nil, nil, nil, nil, fake, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil))
	t.Cleanup(srv.Close)

	resp := do(t, http.MethodDelete, srv.URL+"/v1/webhook-endpoints/whe_1", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("DELETE with a key holding only `responses` = %d, want 403", resp.StatusCode)
	}
	if fake.deletes != 0 {
		t.Fatalf("the refused DELETE still reached the store %d time(s); the gate must decide before the store does",
			fake.deletes)
	}
	if len(fake.endpoints) != 1 {
		t.Fatal("the refused DELETE removed the endpoint anyway")
	}
}

// TestTheSingularWebhookEndpointReadProjectsExactlyWhatTheListDoes is the RIDER's invariant. Two reads of
// one resource that disagree about its shape are two resources as far as a caller is concerned, and this
// tree has shipped that divergence before (the modelRoutes list-envelope split in the SDK parity matrix).
// The check is FIELD-WISE over the marshalled JSON rather than a spot-check of a few names, so a field added
// to one projection and not the other fails here rather than in a console six weeks later.
func TestTheSingularWebhookEndpointReadProjectsExactlyWhatTheListDoes(t *testing.T) {
	fake := newFakeWebhookAPI()
	fake.endpoints["whe_1"] = automation.EndpointView{
		ID: "whe_1", URL: "https://hooks.example/x", Enabled: true,
		EventFilter: []string{"run.completed.v1"}, APIRevision: "2026-01-01",
		SigningSecretRef: "secret_ref_live", SigningSecretRefNext: "secret_ref_next",
		TimeoutMS: 3000, MaxAttempts: 20, RetryWindowSeconds: 259200,
	}
	srv := webhookTestServer(t, fake)

	single := do(t, http.MethodGet, srv.URL+"/v1/webhook-endpoints/whe_1", "", nil)
	defer single.Body.Close()
	if single.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/webhook-endpoints/whe_1 = %d, want 200", single.StatusCode)
	}
	var singleBody map[string]json.RawMessage
	decodeInto(t, single, &singleBody)

	listed := do(t, http.MethodGet, srv.URL+"/v1/webhook-endpoints", "", nil)
	defer listed.Body.Close()
	var listBody struct {
		Data []map[string]json.RawMessage `json:"data"`
	}
	decodeInto(t, listed, &listBody)
	if len(listBody.Data) != 1 {
		t.Fatalf("the list returned %d endpoint(s), want 1", len(listBody.Data))
	}

	singleKeys, listKeys := sortedKeys(singleBody), sortedKeys(listBody.Data[0])
	if !reflect.DeepEqual(singleKeys, listKeys) {
		t.Fatalf("the singular read and the list project different fields.\n  singular: %v\n  list:     %v\n"+
			"They render the same EndpointView, so a difference here means one of the two routes reshapes it.",
			singleKeys, listKeys)
	}
	for k, want := range listBody.Data[0] {
		if string(singleBody[k]) != string(want) {
			t.Fatalf("field %q: singular read = %s, list = %s", k, singleBody[k], want)
		}
	}
}

// TestTheProjectionCarriesTheBehaviouralConfigurationAnOperatorMustReadBack closes four of the six fields
// written at create and readable nowhere. What admits them is that they are CONFIGURATION: an operator who
// set timeout_ms to 3000 has to be able to see 3000, or the only way to know what a deployment does is to
// remember what was typed. A signing_secret_ref is admitted on those grounds and one more — it is a HANDLE,
// and the value behind it stays unreadable, which is E25's environment-value rule applied here.
func TestTheProjectionCarriesTheBehaviouralConfigurationAnOperatorMustReadBack(t *testing.T) {
	fake := newFakeWebhookAPI()
	fake.endpoints["whe_1"] = automation.EndpointView{
		ID: "whe_1", URL: "https://hooks.example/x", Enabled: true,
		SigningSecretRef: "secret_ref_live", SigningSecretRefNext: "secret_ref_next",
		TimeoutMS: 3000, MaxAttempts: 7, RetryWindowSeconds: 259200,
	}
	srv := webhookTestServer(t, fake)

	resp := do(t, http.MethodGet, srv.URL+"/v1/webhook-endpoints/whe_1", "", nil)
	defer resp.Body.Close()
	var body map[string]any
	decodeInto(t, resp, &body)

	for field, want := range map[string]any{
		"signing_secret_ref":      "secret_ref_live",
		"signing_secret_ref_next": "secret_ref_next",
		"timeout_ms":              float64(3000),
		"max_attempts":            float64(7),
		"retry_window_seconds":    float64(259200),
	} {
		got, present := body[field]
		if !present {
			t.Fatalf("the projection has no %q; it is written at create and readable nowhere", field)
		}
		if got != want {
			t.Fatalf("%s = %v, want %v", field, got, want)
		}
	}
}

// TestTheEndpointProjectionStructurallyCannotCarryFixedHeaders is the one field of the six that stays OUT,
// and the reason is not that it is uninteresting — an operator writes that map freely and one of the things
// they write into it is an Authorization header for the receiver, so reflecting it back turns a read route
// into a credential read.
//
// The guard is REFLECTION over the projection type rather than a byte scan of a response, and the
// difference is the point: with no such field on EndpointView, a scan of these responses could never fail,
// and a scan that cannot fail proves nothing (the compressed-secret-scan lesson). What CAN fail is somebody
// adding the field, so that is what is watched — by shape, not by one spelling, so a rename does not slip
// past. The byte scan belongs where the value genuinely exists: against a real row, in the component tier
// (TestAWebhookEndpointsFixedHeadersReachNoReader).
func TestTheEndpointProjectionStructurallyCannotCarryFixedHeaders(t *testing.T) {
	viewType := reflect.TypeOf(automation.EndpointView{})
	for i := range viewType.NumField() {
		field := viewType.Field(i)
		hay := strings.ToLower(field.Name + " " + string(field.Tag))
		for _, needle := range []string{"fixedheader", "fixed_header", "header"} {
			if strings.Contains(hay, needle) {
				t.Fatalf("automation.EndpointView.%s (tag %q) puts request headers into the endpoint projection. "+
					"fixed_headers is a free map an operator writes and it can hold a credential for the "+
					"receiver; returning it from a read route is a credential read. If this field is genuinely "+
					"not fixed_headers, narrow this guard deliberately rather than deleting it.",
					field.Name, field.Tag)
			}
		}
	}
}

// TestCreateWritesALocationThatResolvesNowThatTheFamilyHasASingularRead restores what E29 T2 removed under
// protest. T2's comment in webhook_endpoints.go named the exact condition for the header's return — a route
// at the address it names — and this asserts the header is back AND that following it lands on the resource,
// rather than trusting either half on its own.
func TestCreateWritesALocationThatResolvesNowThatTheFamilyHasASingularRead(t *testing.T) {
	fake := newFakeWebhookAPI()
	srv := webhookTestServer(t, fake)

	created := do(t, http.MethodPost, srv.URL+"/v1/webhook-endpoints", `{"url":"https://hooks.example/x"}`, nil)
	defer created.Body.Close()
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("POST /v1/webhook-endpoints = %d, want 201", created.StatusCode)
	}
	location := created.Header.Get("Location")
	if location == "" {
		t.Fatal("the create writes no Location. The family now has a singular read, which is the exact condition " +
			"webhook_endpoints.go named for the header's return.")
	}

	followed := do(t, http.MethodGet, srv.URL+location, "", nil)
	defer followed.Body.Close()
	if followed.StatusCode != http.StatusOK {
		t.Fatalf("following Location %q = %d, want 200", location, followed.StatusCode)
	}
}

// TestTheTwoStoreRefusalsRenderAsConflictsRatherThanFiveHundreds drives the two typed refusals over the
// ROUTES. Both are proven at the store in the component tier, and that is not the same thing: proving a
// mechanism is not proving the surface a caller meets, and an error arm nothing drives is an arm that can
// render a 500 while every store-level test stays green.
//
// Both are 409 rather than 404 or 500 for the same reason. The resource IS there and the caller's request is
// well-formed; what refuses it is the state of something else — a trigger revision that pins the endpoint, an
// endpoint that no longer exists to receive a redelivery. A 404 would say "you asked about nothing", which is
// false and sends an operator looking for a resource that is sitting right there.
func TestTheTwoStoreRefusalsRenderAsConflictsRatherThanFiveHundreds(t *testing.T) {
	t.Run("deleting an endpoint a trigger revision pins", func(t *testing.T) {
		fake := newFakeWebhookAPI("whe_1")
		fake.pinned = "whe_1"
		srv := webhookTestServer(t, fake)

		resp := do(t, http.MethodDelete, srv.URL+"/v1/webhook-endpoints/whe_1", "", nil)
		defer resp.Body.Close()
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("DELETE of a pinned endpoint = %d, want 409.\n%s", resp.StatusCode, body)
		}
		// The answer has to name what is in the way, or it is a 409 an operator cannot act on.
		if !strings.Contains(body, "trigger") {
			t.Fatalf("the 409 does not say a trigger revision is what pins the endpoint:\n%s", body)
		}
		if len(fake.endpoints) != 1 {
			t.Fatal("the refused delete removed the endpoint anyway")
		}
	})

	t.Run("redelivering to a deleted endpoint", func(t *testing.T) {
		fake := newFakeWebhookAPI()
		fake.redeliverErr = automation.ErrDeliveryEndpointDeleted
		srv := webhookTestServer(t, fake)

		resp := do(t, http.MethodPost, srv.URL+"/v1/webhook-deliveries/whd_1/redeliver", "", nil)
		defer resp.Body.Close()
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("redelivering to a deleted endpoint = %d, want 409. A 202 would accept work the pump can "+
				"never do — its due-scan joins webhook_endpoints.\n%s", resp.StatusCode, body)
		}
		if !strings.Contains(body, "deleted") {
			t.Fatalf("the 409 does not say the endpoint was deleted:\n%s", body)
		}
	})
}

// ---- helpers ---------------------------------------------------------------------------------------

func newFakeWebhookAPI(ids ...string) *fakeWebhookAPI {
	f := &fakeWebhookAPI{endpoints: map[string]automation.EndpointView{}}
	for _, id := range ids {
		f.endpoints[id] = automation.EndpointView{ID: id, URL: "https://hooks.example/" + id}
	}
	return f
}

func decodeInto(t *testing.T, resp *http.Response, into any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		t.Fatalf("decode body: %v", err)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
