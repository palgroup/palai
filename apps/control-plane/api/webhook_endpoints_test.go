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
	srv := httptest.NewServer(NewRouter(fakeVerifier{}, nil, nil, nil, nil, api, SSEConfig{}, nil))
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
// the router (AUT-012 static half): a private/loopback URL without the allowlist flag is a 400 and
// never reaches the store, while a public URL is created 201.
func TestCreateEndpointRejectsPrivateDestinationAtTheAPI(t *testing.T) {
	fake := &fakeWebhookAPI{}
	srv := webhookTestServer(t, fake)

	resp := post(t, srv.URL+"/v1/webhook-endpoints", `{"url":"http://169.254.169.254/hook"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("private destination create status = %d, want 400", resp.StatusCode)
	}
	if fake.created != nil {
		t.Fatal("a rejected create still reached the store")
	}

	resp = post(t, srv.URL+"/v1/webhook-endpoints", `{"url":"https://hooks.example.com/x","event_filter":["run.completed.v1"]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("public destination create status = %d, want 201", resp.StatusCode)
	}
	if fake.created == nil || fake.created.URL != "https://hooks.example.com/x" {
		t.Fatalf("store did not receive the created endpoint: %+v", fake.created)
	}

	// A loopback URL WITH the explicit allowlist flag is accepted (a self-host receiver).
	fake.created = nil
	resp = post(t, srv.URL+"/v1/webhook-endpoints", `{"url":"http://127.0.0.1:9000/hook","allow_private_destination":true}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("loopback+flag create status = %d, want 201", resp.StatusCode)
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
