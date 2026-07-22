package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// TestAdmitConcurrencyLimitedRenders429 proves a ConcurrencyLimited admission surfaces as 429 +
// Retry-After + the §20.10 stable concurrency_exceeded RFC 9457 problem (§20.12 concurrent-run cap).
func TestAdmitConcurrencyLimitedRenders429(t *testing.T) {
	srv := admissionTestServer(t, AdmitResult{ConcurrencyLimited: true})
	resp := postResponse(t, srv.URL)
	defer resp.Body.Close()
	assertCapacity429(t, resp)
}

// TestAdmitQueueFullRenders429 proves a QueueDepthExceeded admission surfaces as 429 + Retry-After
// + the same stable concurrency_exceeded code (the queued bound folds into the one registered
// admission-capacity 429 code; §20.10 declares no separate queued code).
func TestAdmitQueueFullRenders429(t *testing.T) {
	srv := admissionTestServer(t, AdmitResult{QueueDepthExceeded: true})
	resp := postResponse(t, srv.URL)
	defer resp.Body.Close()
	assertCapacity429(t, resp)
}

func assertCapacity429(t *testing.T, resp *http.Response) {
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
	if p.Status != http.StatusTooManyRequests || !p.Retryable {
		t.Fatalf("problem = %+v, want status 429 retryable", p)
	}
	// The emitted code MUST be a published §20.10 stable code — a client coded against the spec must
	// be able to match it. Fails loudly if a future edit ships an unregistered public code.
	if !knownProblemCodes(t)[p.Code] {
		t.Fatalf("429 code %q is not in problem.json known_codes", p.Code)
	}
}

// knownProblemCodes loads the §20.10 stable-code enum from the canonical schema, so a test can assert
// an emitted code is actually published. It walks up to the repo root to find the schema regardless of
// the test's working directory.
func knownProblemCodes(t *testing.T) map[string]bool {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	var raw []byte
	for {
		candidate := filepath.Join(dir, "protocols", "schemas", "common", "problem.json")
		if raw, err = os.ReadFile(candidate); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate protocols/schemas/common/problem.json from the test dir")
		}
		dir = parent
	}
	var schema struct {
		Defs struct {
			KnownCodes struct {
				Enum []string `json:"enum"`
			} `json:"known_codes"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("decode problem.json: %v", err)
	}
	set := map[string]bool{}
	for _, c := range schema.Defs.KnownCodes.Enum {
		set[c] = true
	}
	return set
}
