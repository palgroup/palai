package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/packages/contracts"
)

// scriptedAdmitter returns a fixed AdmitResult so the handler's rendering of each admission
// outcome can be exercised without a database.
type scriptedAdmitter struct{ result AdmitResult }

func (s scriptedAdmitter) AdmitResponse(context.Context, AdmitRequest) (AdmitResult, error) {
	return s.result, nil
}
func (s scriptedAdmitter) GetResponse(context.Context, middleware.Scope, string) (RetrieveResult, error) {
	return RetrieveResult{}, nil
}
func (s scriptedAdmitter) CancelResponse(context.Context, middleware.Scope, string) (RetrieveResult, error) {
	return RetrieveResult{}, nil
}

func admissionTestServer(t *testing.T, result AdmitResult) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(NewRouter(fakeVerifier{}, scriptedAdmitter{result: result},
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil))
	t.Cleanup(srv.Close)
	return srv
}

func postResponse(t *testing.T, url string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url+"/v1/responses", strings.NewReader(`{"model":"m","input":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer test")
	req.Header.Set("Idempotency-Key", "k-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/responses error = %v", err)
	}
	return resp
}

// TestAdmitConcurrencyLimitedRenders429 proves a ConcurrencyLimited admission surfaces as
// 429 + Retry-After + the concurrency_limit RFC 9457 problem (§20.12 concurrent-run cap).
func TestAdmitConcurrencyLimitedRenders429(t *testing.T) {
	srv := admissionTestServer(t, AdmitResult{ConcurrencyLimited: true})
	resp := postResponse(t, srv.URL)
	defer resp.Body.Close()
	assertCapacity429(t, resp, "concurrency_limit")
}

// TestAdmitQueueFullRenders429 proves a QueueDepthExceeded admission surfaces as 429 + Retry-After
// + the queue_full RFC 9457 problem (§20.12 queued-run bound).
func TestAdmitQueueFullRenders429(t *testing.T) {
	srv := admissionTestServer(t, AdmitResult{QueueDepthExceeded: true})
	resp := postResponse(t, srv.URL)
	defer resp.Body.Close()
	assertCapacity429(t, resp, "queue_full")
}

func assertCapacity429(t *testing.T, resp *http.Response, wantCode string) {
	t.Helper()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("429 response missing Retry-After header")
	}
	var p contracts.Problem
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if p.Code != wantCode || p.Status != http.StatusTooManyRequests || !p.Retryable {
		t.Fatalf("problem = %+v, want code %s status 429 retryable", p, wantCode)
	}
}
