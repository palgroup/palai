package providerone

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// THE ENDPOINT IS A PROPERTY OF THE CONNECTION, NOT OF THE PROCESS (E29 provider wiring).
//
// Until now the only way to aim this adapter somewhere other than api.openai.com was Adapter.BaseURL — a
// field the composition root sets ONCE, at boot, from PALAI_OPENAI_COMPATIBLE_BASE_URL. That makes a custom
// endpoint a deployment-wide constant: two projects on one stack cannot reach two endpoints, and an operator
// cannot name one from the console at all, because by the time their row exists the adapter value is built.
//
// A per-REQUEST base URL is the seam that makes `model_connections.base_url` reachable. It is resolved the
// same way IdempotencyKey was added: an optional field on the canonical request that the adapter forwards,
// empty meaning exactly what it meant before.
//
// PRECEDENCE IS DELIBERATE: the request wins over the adapter's field, which wins over DefaultBaseURL. The
// request's value came from a tenant-scoped row the operator wrote; the adapter's came from the deployment's
// env. A deployment default must never silently override a project's own connection — that is §27.7's "a
// route cannot silently select something else" applied to the endpoint rather than the model.
func TestExecuteHonoursThePerRequestBaseURL(t *testing.T) {
	var got string
	// A deliberately NON-streaming 500 so the call ends immediately: this test measures WHERE the request
	// landed, and nothing about the response body.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Path
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	// The adapter carries the DEPLOYMENT's endpoint; the request carries the CONNECTION's.
	adapter := Adapter{BaseURL: srv.URL + "/deployment-default"}
	if _, err := adapter.Execute(context.Background(), modelbroker.Request{
		Model:    "any",
		BaseURL:  srv.URL + "/this-projects-own-endpoint",
		Messages: []modelbroker.Message{{Role: "user", Content: "ping"}},
	}, "sk-test", nil); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got != "/this-projects-own-endpoint" {
		t.Fatalf("request landed on %q, want the connection's own endpoint — a per-project base URL cannot "+
			"reach the wire, so a custom OpenAI-compatible connection is a row nothing dials", got)
	}
}

// With no per-request endpoint the adapter is bit-unchanged: the deployment field, else the OpenAI default.
// This is the compatibility half — every existing deployment must keep dialling exactly where it did.
func TestExecuteFallsBackToTheAdapterEndpoint(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Path
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	adapter := Adapter{BaseURL: srv.URL + "/deployment-default"}
	if _, err := adapter.Execute(context.Background(), modelbroker.Request{
		Model:    "any",
		Messages: []modelbroker.Message{{Role: "user", Content: "ping"}},
	}, "sk-test", nil); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got != "/deployment-default" {
		t.Fatalf("request landed on %q, want the deployment's endpoint", got)
	}
	if (Adapter{}).baseURL() != DefaultBaseURL {
		t.Fatalf("a bare adapter no longer defaults to OpenAI")
	}
}
