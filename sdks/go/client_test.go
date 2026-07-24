package palai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// roundTripFunc adapts a function to http.RoundTripper for injected transports.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

const queuedBody = `{"id":"resp_1","object":"response","status":"queued","session_id":"ses_1","run_id":"run_1"}`

func problemBody(status int, code string) string {
	return `{"type":"t","title":"` + code + `","status":` + strconv.Itoa(status) + `,"code":"` + code + `","request_id":"req_1"}`
}

func testClient(t *testing.T, rt http.RoundTripper) *Client {
	t.Helper()
	c, err := New(
		WithAPIKey("sk-test"),
		WithBaseURL("http://palai.test"),
		WithHTTPClient(&http.Client{Transport: rt}),
		WithMaxRetries(3),
		WithBackoff(1, 2),
	)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return c
}

// --- API-013: idempotent retry with a stable Idempotency-Key ---------------------------------

func TestRetryReusesSameIdempotencyKey(t *testing.T) {
	var mu sync.Mutex
	var keys []string
	attempt := 0
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		attempt++
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		n := attempt
		mu.Unlock()
		if n <= 2 {
			return jsonResp(503, problemBody(503, "capacity_unavailable")), nil
		}
		return jsonResp(202, queuedBody), nil
	})
	client := testClient(t, rt)

	resp, err := client.Responses.Create(context.Background(), ResponseCreateRequest{Input: "hi"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if resp.ID != "resp_1" {
		t.Fatalf("id = %q", resp.ID)
	}
	if len(keys) != 3 {
		t.Fatalf("two 503s then success is three attempts, got %d", len(keys))
	}
	for _, k := range keys {
		if k == "" || k != keys[0] {
			t.Fatalf("idempotency key must be identical and non-empty across retries: %v", keys)
		}
	}
}

func TestNonRetryableStatusThrowsImmediately(t *testing.T) {
	attempts := 0
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		attempts++
		return jsonResp(400, problemBody(400, "invalid_request")), nil
	})
	client := testClient(t, rt)

	_, err := client.Responses.Retrieve(context.Background(), "x")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 400 {
		t.Fatalf("want *APIError 400, got %v", err)
	}
	if apiErr.Class() != "InvalidRequestError" {
		t.Fatalf("class = %q", apiErr.Class())
	}
	if attempts != 1 {
		t.Fatalf("a 400 must not be retried, attempts = %d", attempts)
	}
}

func TestPersistentNetworkErrorRetriesThenConnectionError(t *testing.T) {
	attempts := 0
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		attempts++
		return nil, errors.New("network unreachable")
	})
	c, _ := New(WithAPIKey("sk-test"), WithBaseURL("http://palai.test"),
		WithHTTPClient(&http.Client{Transport: rt}), WithMaxRetries(2), WithBackoff(1, 2))

	_, err := c.Responses.Retrieve(context.Background(), "x") // GET is idempotent → retries
	var connErr *ConnectionError
	if !errors.As(err, &connErr) {
		t.Fatalf("want *ConnectionError, got %v", err)
	}
	if attempts != 3 {
		t.Fatalf("initial attempt plus two retries = 3, got %d", attempts)
	}
	if !connErr.Retryable() {
		t.Fatal("a connection error is retryable")
	}
}

// TestNonIdempotentPostFailsClosed: a mutation with NO idempotency key is not re-sent on a network
// failure — a connection torn after the server committed must not double-apply (fail closed).
func TestNonIdempotentPostFailsClosed(t *testing.T) {
	attempts := 0
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		attempts++
		return nil, errors.New("network unreachable")
	})
	client := testClient(t, rt)

	err := client.doJSON(context.Background(), http.MethodPost, "/v1/x", requestOptions{}, nil)
	var connErr *ConnectionError
	if !errors.As(err, &connErr) {
		t.Fatalf("want *ConnectionError, got %v", err)
	}
	if attempts != 1 {
		t.Fatalf("a non-idempotent POST must fail closed with a single attempt, got %d", attempts)
	}
}

func TestContextCancelIsTerminal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		cancel() // cancel mid-flight
		return nil, context.Canceled
	})
	client := testClient(t, rt)
	_, err := client.Responses.Retrieve(ctx, "x")
	var connErr *ConnectionError
	if !errors.As(err, &connErr) {
		t.Fatalf("want *ConnectionError on cancel, got %v", err)
	}
}

func TestMissingAPIKeyIsAnError(t *testing.T) {
	t.Setenv("PALAI_API_KEY", "")
	if _, err := New(WithBaseURL("http://palai.test")); err == nil {
		t.Fatal("New must require an API key")
	}
}

func TestRetryAfterHeaderHonored(t *testing.T) {
	attempts := 0
	start := time.Now()
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			resp := jsonResp(429, problemBody(429, "rate_limited"))
			resp.Header.Set("Retry-After", "0") // 0s: honored, no long sleep
			return resp, nil
		}
		return jsonResp(202, queuedBody), nil
	})
	client := testClient(t, rt)
	if _, err := client.Responses.Create(context.Background(), ResponseCreateRequest{Input: "hi"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("a 429 then success is two attempts, got %d", attempts)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("Retry-After: 0 should not introduce a long delay")
	}
}
