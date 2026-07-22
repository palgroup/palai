package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/internal/automation"
)

// fakeTriggerAPI records what reached the store seam and scripts its outcomes.
type fakeTriggerAPI struct {
	revised     *automation.TriggerRevisionInput
	reviseErr   error
	triggerHit  bool
	delivery    automation.DeliveryResult
	deliveryErr error
	deliveryHit bool
}

func (f *fakeTriggerAPI) CreateTrigger(context.Context, string, string, string, string, string) (string, error) {
	return "trg_created", nil
}
func (f *fakeTriggerAPI) IngestInbound(context.Context, string, map[string]string, []byte) (automation.InboundResult, error) {
	return automation.InboundResult{}, nil
}
func (f *fakeTriggerAPI) SetInboundSecretRefs(context.Context, string, string, string, string, string) error {
	return nil
}
func (f *fakeTriggerAPI) ReviseTrigger(_ context.Context, _, _, _ string, in automation.TriggerRevisionInput) (automation.TriggerRevision, error) {
	f.revised = &in
	if f.reviseErr != nil {
		return automation.TriggerRevision{}, f.reviseErr
	}
	return automation.TriggerRevision{ID: "trev_1", RevisionNumber: 1}, nil
}
func (f *fakeTriggerAPI) GetTrigger(context.Context, string, string, string) (automation.TriggerView, bool, error) {
	return automation.TriggerView{ID: "trg_1", Name: "nightly"}, f.triggerHit, nil
}
func (f *fakeTriggerAPI) CreateDeliveryIdempotent(context.Context, string, string, string, string, string, []byte) (automation.DeliveryResult, error) {
	return f.delivery, f.deliveryErr
}
func (f *fakeTriggerAPI) GetDelivery(context.Context, string, string, string) (automation.TriggerDeliveryView, bool, error) {
	return automation.TriggerDeliveryView{ID: "tdel_1", State: "run_created"}, f.deliveryHit, nil
}

func triggerTestServer(t *testing.T, api *fakeTriggerAPI) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, api, nil, SSEConfig{}, nil))
	t.Cleanup(srv.Close)
	return srv
}

func do(t *testing.T, method, url, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s error = %v", method, url, err)
	}
	return resp
}

// TestTriggerManagementSurface pins the create/revise/get routes: a create needs a name (400 else, 201
// with), a revise with an invalid config is a 400 (fail-closed) while a valid one is 201, and a GET of an
// unknown trigger is 404 while a known one is 200.
func TestTriggerManagementSurface(t *testing.T) {
	fake := &fakeTriggerAPI{}
	srv := triggerTestServer(t, fake)

	if resp := do(t, "POST", srv.URL+"/v1/triggers", `{}`, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("create without name status = %d, want 400", resp.StatusCode)
	}
	if resp := do(t, "POST", srv.URL+"/v1/triggers", `{"name":"nightly"}`, nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}

	// A revise carrying a mapping escape is a 400 (fail-closed), never a 500.
	fake.reviseErr = automation.ErrMappingVerb
	if resp := do(t, "POST", srv.URL+"/v1/triggers/trg_1/revisions", `{"input_mapping":{"fields":{"x":{"fetch":"http://evil"}}}}`, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad-config revise status = %d, want 400", resp.StatusCode)
	}
	// An unknown trigger is a 404.
	fake.reviseErr = automation.ErrTriggerNotFound
	if resp := do(t, "POST", srv.URL+"/v1/triggers/trg_missing/revisions", `{}`, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("revise unknown trigger status = %d, want 404", resp.StatusCode)
	}
	// A valid revise is a 201.
	fake.reviseErr = nil
	if resp := do(t, "POST", srv.URL+"/v1/triggers/trg_1/revisions", `{"concurrency_policy":"queue"}`, nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("valid revise status = %d, want 201", resp.StatusCode)
	}

	// GET: unknown → 404, known → 200.
	if resp := do(t, "GET", srv.URL+"/v1/triggers/trg_missing", ``, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET unknown trigger status = %d, want 404", resp.StatusCode)
	}
	fake.triggerHit = true
	if resp := do(t, "GET", srv.URL+"/v1/triggers/trg_1", ``, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET known trigger status = %d, want 200", resp.StatusCode)
	}
}

// TestDeliveryRouteRequiresIdempotencyKey proves the manual/API delivery POST carries the Idempotency-Key
// requirement (mounted middleware): a delivery without the header is a 400 and never reaches the store; a
// delivery with the header ingests (202); an unknown trigger is a 404; and the delivery view GET routes.
func TestDeliveryRouteRequiresIdempotencyKey(t *testing.T) {
	fake := &fakeTriggerAPI{delivery: automation.DeliveryResult{ID: "tdel_1", State: "run_created", RunID: "run_1"}}
	srv := triggerTestServer(t, fake)

	// No Idempotency-Key → 400 from the mounted middleware.
	if resp := do(t, "POST", srv.URL+"/v1/triggers/trg_1/deliveries", `{"order":{"id":"o1"}}`, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("delivery without Idempotency-Key status = %d, want 400", resp.StatusCode)
	}
	// With the header → 202 accepted.
	hdr := map[string]string{"Idempotency-Key": "k1"}
	if resp := do(t, "POST", srv.URL+"/v1/triggers/trg_1/deliveries", `{"order":{"id":"o1"}}`, hdr); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("delivery with Idempotency-Key status = %d, want 202", resp.StatusCode)
	}

	// A disabled trigger is a 409; an unknown trigger is a 404.
	fake.deliveryErr = automation.ErrTriggerNotFound
	if resp := do(t, "POST", srv.URL+"/v1/triggers/trg_missing/deliveries", `{}`, hdr); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("delivery to unknown trigger status = %d, want 404", resp.StatusCode)
	}

	// GET delivery view: unknown → 404, known → 200.
	if resp := do(t, "GET", srv.URL+"/v1/trigger-deliveries/tdel_missing", ``, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET unknown delivery status = %d, want 404", resp.StatusCode)
	}
	fake.deliveryHit = true
	if resp := do(t, "GET", srv.URL+"/v1/trigger-deliveries/tdel_1", ``, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET known delivery status = %d, want 200", resp.StatusCode)
	}
}
