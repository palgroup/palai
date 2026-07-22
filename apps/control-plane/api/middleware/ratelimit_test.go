package middleware

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/palgroup/palai/packages/contracts"
)

// TestKeyedLimiterBurstThenRefill proves the per-key token bucket allows up to `burst`
// requests immediately, denies the next with a positive Retry-After, and refills at
// `ratePerSec` so a request is admitted again once a whole token has accrued.
func TestKeyedLimiterBurstThenRefill(t *testing.T) {
	now := time.Unix(0, 0)
	lim := newKeyedLimiter(2 /* rate/sec */, 3 /* burst */, 1000, func() time.Time { return now })

	// The full burst drains without denial.
	for i := 0; i < 3; i++ {
		if ok, _ := lim.allow("k"); !ok {
			t.Fatalf("burst request %d denied, want allowed", i)
		}
	}
	// The bucket is empty: the next request is denied and told how long to wait.
	ok, retry := lim.allow("k")
	if ok {
		t.Fatal("4th request allowed, want denied (burst exhausted)")
	}
	if retry <= 0 {
		t.Fatalf("Retry-After = %v, want positive", retry)
	}
	// At 2 tokens/sec one token accrues in 500ms; before that it stays denied.
	now = now.Add(499 * time.Millisecond)
	if ok, _ := lim.allow("k"); ok {
		t.Fatal("request allowed before a token refilled, want denied")
	}
	now = now.Add(2 * time.Millisecond) // now 501ms elapsed → ≥1 token
	if ok, _ := lim.allow("k"); !ok {
		t.Fatal("request denied after a full token refilled, want allowed")
	}
}

// TestKeyedLimiterIsolatesKeys proves one key exhausting its bucket does not throttle another.
func TestKeyedLimiterIsolatesKeys(t *testing.T) {
	now := time.Unix(0, 0)
	lim := newKeyedLimiter(1, 1, 1000, func() time.Time { return now })
	if ok, _ := lim.allow("a"); !ok {
		t.Fatal("first request for a denied")
	}
	if ok, _ := lim.allow("a"); ok {
		t.Fatal("second request for a allowed, want denied")
	}
	if ok, _ := lim.allow("b"); !ok {
		t.Fatal("first request for b denied — buckets are not isolated per key")
	}
}

// TestKeyedLimiterBoundsMapAgainstDistinctKeys proves the bucket map cannot grow without bound when
// presented an attacker-controlled stream of distinct keys (the pre-Auth junk-bearer DoS): after
// maxBuckets distinct keys the map stays at or below the ceiling instead of one entry per key.
func TestKeyedLimiterBoundsMapAgainstDistinctKeys(t *testing.T) {
	now := time.Unix(0, 0)
	const ceiling = 16
	lim := newKeyedLimiter(1, 1, ceiling, func() time.Time { return now })
	for i := 0; i < 100*ceiling; i++ {
		lim.allow(fmt.Sprintf("junk-%d", i)) // each key distinct — the flood
	}
	lim.mu.Lock()
	n := len(lim.buckets)
	lim.mu.Unlock()
	if n > ceiling {
		t.Fatalf("bucket map grew to %d entries, want <= ceiling %d (unbounded = memory-exhaustion DoS)", n, ceiling)
	}
}

// TestRateLimitMiddlewareEmitsProblem proves the middleware returns 429 + Retry-After + an
// RFC 9457 problem once a key's burst is spent, and that an unauthenticated request passes
// through (Auth answers it, not the limiter).
func TestRateLimitMiddlewareEmitsProblem(t *testing.T) {
	var reached int
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { reached++; w.WriteHeader(http.StatusOK) })
	// rate 0.001/sec so no token refills during the test; burst 1.
	handler := RequestContext(RateLimit(0.001, 1)(next))

	do := func(auth string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/responses/x", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	if rec := do("Bearer key-a"); rec.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", rec.Code)
	}
	rec := do("Bearer key-a")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("429 response missing Retry-After header")
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", ct)
	}
	var p contracts.Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if p.Code != "rate_limited" || p.Status != http.StatusTooManyRequests || !p.Retryable {
		t.Fatalf("problem = %+v, want code rate_limited status 429 retryable", p)
	}

	// A different key is unaffected; a request with no bearer is passed through to next.
	if rec := do("Bearer key-b"); rec.Code != http.StatusOK {
		t.Fatalf("distinct key status = %d, want 200", rec.Code)
	}
	before := reached
	if rec := do(""); rec.Code != http.StatusOK {
		t.Fatalf("no-bearer request status = %d, want 200 (passed to next)", rec.Code)
	}
	if reached != before+1 {
		t.Fatal("no-bearer request was not passed through to next")
	}
}
