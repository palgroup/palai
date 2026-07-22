package api_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/packages/contracts"
)

// testToken is the bearer credential the fake backend accepts. The full key is
// never stored: the fake compares the presented token to a known value, mirroring
// the production hash comparison without a database.
const testToken = "test-token"

// testScope is the tenant the fake resolves testToken to. Handlers must derive
// scope from this identity, never from a request-body project_id.
var testScope = middleware.Scope{
	Organization: "org_test",
	Project:      "prj_test",
	Principal:    "prin_test",
}

// fakeBackend is the in-process store seam: it implements auth verification and
// idempotent admission without Postgres so the HTTP contract runs Docker-free.
// The durable admission invariants themselves are proven against real Postgres in
// tests/component/postgres.
type fakeBackend struct {
	mu   sync.Mutex
	seen map[string]stored
	byID map[string][]byte
}

type stored struct {
	hash   string
	respID string
	body   []byte
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{seen: map[string]stored{}, byID: map[string][]byte{}}
}

func (f *fakeBackend) VerifyAPIKey(_ context.Context, token string) (middleware.Scope, error) {
	if token != testToken {
		return middleware.Scope{}, middleware.ErrInvalidToken
	}
	return testScope, nil
}

func (f *fakeBackend) AdmitResponse(_ context.Context, req api.AdmitRequest) (api.AdmitResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if prev, ok := f.seen[req.IdempotencyKey]; ok {
		if prev.hash != req.RequestHash {
			return api.AdmitResult{Conflict: true}, nil
		}
		return api.AdmitResult{ResponseID: prev.respID, Body: prev.body, Replayed: true}, nil
	}
	f.seen[req.IdempotencyKey] = stored{hash: req.RequestHash, respID: req.ResponseID, body: req.Body}
	f.byID[req.ResponseID] = req.Body
	return api.AdmitResult{ResponseID: req.ResponseID, Body: req.Body}, nil
}

// GetResponse serves the retrieval seam from the by-id index: an unknown id is a
// miss (404) and a stored resource returns its committed body (200). The 410 purge
// path is proven against real Postgres in the e2e tier, not here.
func (f *fakeBackend) GetResponse(_ context.Context, _ middleware.Scope, id string) (api.RetrieveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, ok := f.byID[id]
	if !ok {
		return api.RetrieveResult{}, nil
	}
	return api.RetrieveResult{Body: body, Found: true}, nil
}

// CancelResponse mirrors GetResponse in the fake: an unknown id is a miss (404) and a stored
// resource returns its body. The durable cancel transaction — the run transition, the
// canceled projection, and the commit-after-terminal guard — is proven against real Postgres
// in the e2e tier, not here; this tier only asserts the HTTP 401/404 contract of the route.
func (f *fakeBackend) CancelResponse(_ context.Context, _ middleware.Scope, id string) (api.RetrieveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, ok := f.byID[id]
	if !ok {
		return api.RetrieveResult{}, nil
	}
	return api.RetrieveResult{Body: body, Found: true}, nil
}

// EventReader: the response-admission conformance tier never streams, so these
// satisfy the interface without a journal. The SSE contract is proven end-to-end
// against real Postgres in tests/e2e/sse.
func (f *fakeBackend) SessionExists(context.Context, string, string, string) (bool, error) {
	return false, nil
}

func (f *fakeBackend) ResolveCursor(context.Context, string, string, string, string) (int64, bool, error) {
	return 0, false, nil
}

func (f *fakeBackend) After(context.Context, string, string, string, int64, int) ([]contracts.Event, error) {
	return nil, nil
}

func (f *fakeBackend) RecordAttachDenied(context.Context, string, string, string, string) error {
	return nil
}

// newTestServer starts the real router + middleware stack over a fresh fake
// backend, so every test exercises genuine HTTP round-trips.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	backend := newFakeBackend()
	// Sessions + bindings seams are nil: this Docker-free tier exercises only the response surface, so
	// the standalone session/command + repository-binding routes stay unmounted here (proven in the e2e
	// responses tier).
	srv := httptest.NewServer(api.NewRouter(backend, backend, backend, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, api.SSEConfig{}, nil, nil))
	t.Cleanup(srv.Close)
	return srv
}

// postResponses issues POST /v1/responses with the given headers and body.
func postResponses(t *testing.T, srv *httptest.Server, headers map[string]string, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/responses", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// getResponse issues GET /v1/responses/{id} with a valid bearer token. Retrieval is
// a plain read, so it carries no idempotency key.
func getResponse(t *testing.T, srv *httptest.Server, id string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/responses/"+id, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// cancelResponse issues POST /v1/responses/{id}/cancel with a valid bearer token.
// Cancel is naturally idempotent, so it carries no idempotency key.
func cancelResponse(t *testing.T, srv *httptest.Server, id string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/responses/"+id+"/cancel", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// authedHeaders carries a valid bearer token and idempotency key.
func authedHeaders(key string) map[string]string {
	return map[string]string{
		"Authorization":   "Bearer " + testToken,
		"Idempotency-Key": key,
	}
}

func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return data
}
