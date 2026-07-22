package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/automation"
)

// fakeVerifier accepts any bearer token as one fixed tenant, so the handler tests exercise the routed
// path without a real credential store.
type fakeVerifier struct{}

func (fakeVerifier) VerifyAPIKey(context.Context, string) (middleware.Scope, error) {
	return middleware.Scope{Organization: "org_1", Project: "prj_1", Principal: "prin_1"}, nil
}

// fakeWebhookAPI records what reached the store seam and scripts a redeliver miss.
type fakeWebhookAPI struct {
	created     *automation.EndpointCreate
	redeliverOK bool
}

func (f *fakeWebhookAPI) CreateEndpoint(_ context.Context, _, _ string, c automation.EndpointCreate) (string, error) {
	f.created = &c
	return "whe_created", nil
}
func (f *fakeWebhookAPI) ListEndpoints(context.Context, string, string) ([]automation.EndpointView, error) {
	return nil, nil
}
func (f *fakeWebhookAPI) ListDeliveries(context.Context, string, string, string, int) ([]automation.DeliveryView, error) {
	return nil, nil
}
func (f *fakeWebhookAPI) GetDelivery(context.Context, string, string, string) (*automation.DeliveryView, bool, error) {
	return nil, false, nil
}
func (f *fakeWebhookAPI) ListAttempts(context.Context, string, string, string) ([]automation.AttemptView, error) {
	return nil, nil
}
func (f *fakeWebhookAPI) Redeliver(context.Context, string, string, string) (bool, error) {
	return f.redeliverOK, nil
}

func webhookTestServer(t *testing.T, api *fakeWebhookAPI) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, api, nil, nil, nil, nil, SSEConfig{}, nil, nil))
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, url, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s error = %v", url, err)
	}
	return resp
}

// TestCreateEndpointRejectsPrivateDestinationAtTheAPI proves the create-time egress gate is wired into
// the router (AUT-012 fail-fast half): the named SSRF vectors — the cloud metadata IP, a loopback name
// (localhost), a literal private IP, and http to a private host — are each a 400 that never reaches the
// store, while a literal public https destination is created 201.
func TestCreateEndpointRejectsPrivateDestinationAtTheAPI(t *testing.T) {
	fake := &fakeWebhookAPI{}
	srv := webhookTestServer(t, fake)

	// Each of these is a distinct SSRF vector the endpoint must refuse without dialing.
	for _, attack := range []string{
		`{"url":"http://169.254.169.254/latest/meta-data"}`, // cloud metadata (literal link-local)
		`{"url":"http://localhost/hook"}`,                   // hostname resolving to loopback
		`{"url":"http://10.0.0.5/hook"}`,                    // literal RFC1918
		`{"url":"http://[::1]/hook"}`,                       // literal IPv6 loopback
		`{"url":"https://169.254.169.254/x"}`,               // metadata even over https
	} {
		resp := post(t, srv.URL+"/v1/webhook-endpoints", attack)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("SSRF create %s status = %d, want 400", attack, resp.StatusCode)
		}
	}
	if fake.created != nil {
		t.Fatal("a rejected SSRF create still reached the store")
	}

	// A literal public https destination is created (no DNS needed, so the happy path is offline-clean).
	resp := post(t, srv.URL+"/v1/webhook-endpoints", `{"url":"https://93.184.216.34/x","event_filter":["run.completed.v1"]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("public destination create status = %d, want 201", resp.StatusCode)
	}
	if fake.created == nil || fake.created.URL != "https://93.184.216.34/x" {
		t.Fatalf("store did not receive the created endpoint: %+v", fake.created)
	}

	// A loopback URL WITH the explicit allowlist flag is accepted (a self-host receiver).
	fake.created = nil
	resp = post(t, srv.URL+"/v1/webhook-endpoints", `{"url":"http://127.0.0.1:9000/hook","allow_private_destination":true}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("loopback+flag create status = %d, want 201", resp.StatusCode)
	}
}

// TestCreateEndpointBoundsDeliveryPolicy pins F4/F9: an out-of-range timeout/attempts is a typed 400
// (never a DB-CHECK 500) and never reaches the store; an in-range value is accepted.
func TestCreateEndpointBoundsDeliveryPolicy(t *testing.T) {
	fake := &fakeWebhookAPI{}
	srv := webhookTestServer(t, fake)

	for _, bad := range []string{
		`{"url":"https://93.184.216.34/x","timeout_ms":-5}`,
		`{"url":"https://93.184.216.34/x","timeout_ms":600000}`,
		`{"url":"https://93.184.216.34/x","max_attempts":-1}`,
		`{"url":"https://93.184.216.34/x","max_attempts":9999}`,
	} {
		resp := post(t, srv.URL+"/v1/webhook-endpoints", bad)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("out-of-range create %s status = %d, want 400", bad, resp.StatusCode)
		}
	}
	if fake.created != nil {
		t.Fatal("an out-of-range create reached the store")
	}

	// In-range values are accepted and the defaults fill for omitted fields.
	resp := post(t, srv.URL+"/v1/webhook-endpoints", `{"url":"https://93.184.216.34/x","timeout_ms":5000}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("in-range create status = %d, want 201", resp.StatusCode)
	}
	if fake.created == nil || fake.created.TimeoutMS != 5000 || fake.created.MaxAttempts != 20 {
		t.Fatalf("store got %+v, want timeout 5000 + default 20 attempts", fake.created)
	}
}

// TestRedeliverMissingIsNotFound proves the redeliver route maps a missing delivery to 404.
func TestRedeliverMissingIsNotFound(t *testing.T) {
	srv := webhookTestServer(t, &fakeWebhookAPI{redeliverOK: false})
	resp := post(t, srv.URL+"/v1/webhook-deliveries/whd_missing/redeliver", ``)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("redeliver missing status = %d, want 404", resp.StatusCode)
	}

	srv2 := webhookTestServer(t, &fakeWebhookAPI{redeliverOK: true})
	resp = post(t, srv2.URL+"/v1/webhook-deliveries/whd_live/redeliver", ``)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("redeliver hit status = %d, want 202", resp.StatusCode)
	}
}
