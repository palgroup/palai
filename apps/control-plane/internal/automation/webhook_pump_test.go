package automation

import (
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/integrations/webhook"
)

// TestRetryMatrixAndDeadLetterBounds pins the §21.6 retry classification, the bounded jittered
// backoff, and the 72h/20-attempt dead-letter cutoff — all as pure, clock-injected functions (no
// sleep, no network, no DB): the proof-class distinction the plan draws (a sleep-simulated timeline
// does not count; a computed schedule does).
func TestRetryMatrixAndDeadLetterBounds(t *testing.T) {
	// The status/transport classification matrix (§21.6).
	cases := []struct {
		name string
		res  webhook.Result
		want outcome
	}{
		{"2xx completes", webhook.Result{StatusCode: 200}, outcomeComplete},
		{"201 completes", webhook.Result{StatusCode: 201}, outcomeComplete},
		{"204 completes", webhook.Result{StatusCode: 204}, outcomeComplete},
		{"404 terminal", webhook.Result{StatusCode: 404}, outcomeDead},
		{"400 terminal", webhook.Result{StatusCode: 400}, outcomeDead},
		{"410 terminal", webhook.Result{StatusCode: 410}, outcomeDead},
		{"408 retries", webhook.Result{StatusCode: 408}, outcomeRetry},
		{"409 retries (documented)", webhook.Result{StatusCode: 409}, outcomeRetry},
		{"425 retries", webhook.Result{StatusCode: 425}, outcomeRetry},
		{"429 retries", webhook.Result{StatusCode: 429}, outcomeRetry},
		{"500 retries", webhook.Result{StatusCode: 500}, outcomeRetry},
		{"503 retries", webhook.Result{StatusCode: 503}, outcomeRetry},
		{"transport error retries", webhook.Result{StatusCode: 0}, outcomeRetry},
		{"egress/redirect deny is terminal", webhook.Result{Terminal: true}, outcomeDead},
	}
	for _, c := range cases {
		if got := classify(c.res); got != c.want {
			t.Errorf("%s: classify = %v, want %v", c.name, got, c.want)
		}
	}

	// Jittered exponential backoff is bounded: the ceiling grows, never decreases, and is capped at
	// MaxBackoff; every jittered sample lies within [0, ceiling].
	cfg := PumpConfig{BaseBackoff: time.Second, MaxBackoff: time.Hour}
	var prev time.Duration
	for attempt := 1; attempt <= 25; attempt++ {
		ceil := backoffCeiling(attempt, cfg.BaseBackoff, cfg.MaxBackoff)
		if ceil < prev {
			t.Fatalf("attempt %d: ceiling %v decreased below %v", attempt, ceil, prev)
		}
		if ceil > cfg.MaxBackoff {
			t.Fatalf("attempt %d: ceiling %v exceeds the MaxBackoff cap %v", attempt, ceil, cfg.MaxBackoff)
		}
		prev = ceil
		for i := 0; i < 32; i++ {
			d := nextBackoff(attempt, cfg.BaseBackoff, cfg.MaxBackoff)
			if d < 0 || d > ceil {
				t.Fatalf("attempt %d: jittered backoff %v outside [0, %v]", attempt, d, ceil)
			}
		}
	}
	// The last attempt's ceiling is pinned to the cap (exponential saturates well before attempt 25).
	if backoffCeiling(25, cfg.BaseBackoff, cfg.MaxBackoff) != cfg.MaxBackoff {
		t.Fatal("a late attempt's ceiling must saturate at MaxBackoff")
	}

	// Dead-letter cutoff: exhausted by attempt count OR by the elapsed retry window (§21.6: 72h / 20).
	start := time.Unix(1784203200, 0)
	policy := deliveryPolicy{MaxAttempts: 20, RetryWindow: 72 * time.Hour}
	if retryExhausted(5, start, start.Add(time.Hour), policy) {
		t.Fatal("5 attempts, 1h in: not exhausted")
	}
	if !retryExhausted(20, start, start.Add(time.Hour), policy) {
		t.Fatal("20 attempts: exhausted by count")
	}
	if !retryExhausted(3, start, start.Add(73*time.Hour), policy) {
		t.Fatal("73h elapsed: exhausted by window")
	}
}
