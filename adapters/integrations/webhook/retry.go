package webhook

import (
	"math/rand/v2"
	"time"
)

// The outbound-delivery retry / dead-letter discipline (spec §21.6). It lives here, beside the Sender whose
// Result it classifies, so every outbound caller inherits ONE copy: the control-plane webhook pump
// (apps/control-plane/internal/automation) and the A2A push pusher (adapters/integrations/a2a, E19 T4).
// Before E19 T4 these were unexported in the pump; a second delivery surface would have grown a second,
// silently-diverging copy of the terminal rules and the backoff curve.

// Outcome is one attempt's disposition.
type Outcome int

const (
	OutcomeComplete Outcome = iota
	OutcomeRetry
	OutcomeDead
)

// Classify maps one attempt's Result to the delivery outcome (spec §21.6): 2xx completes; a terminal
// egress/redirect deny is dead; network errors, 408/409/425/429, and 5xx retry; every other 4xx is
// terminal. Retrying a receiver that already said "no, permanently" is retry multiplication, not resilience.
func Classify(res Result) Outcome {
	if res.Terminal {
		return OutcomeDead
	}
	switch {
	case res.StatusCode >= 200 && res.StatusCode < 300:
		return OutcomeComplete
	case res.StatusCode == 0: // no HTTP response — a transport error
		return OutcomeRetry
	case res.StatusCode == 408, res.StatusCode == 409, res.StatusCode == 425, res.StatusCode == 429:
		return OutcomeRetry
	case res.StatusCode >= 500:
		return OutcomeRetry
	default: // other 4xx
		return OutcomeDead
	}
}

// BackoffCeiling is the deterministic upper bound for a given attempt: base * 2^(attempt-1), capped at max.
// Exposed so the schedule is testable without observing jitter.
func BackoffCeiling(attempt int, base, max time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	ceil := base
	for i := 1; i < attempt; i++ {
		ceil *= 2
		if ceil >= max || ceil <= 0 { // cap, and guard the doubling overflow
			return max
		}
	}
	if ceil > max {
		return max
	}
	return ceil
}

// NextBackoff is the jittered delay before the next attempt: a full-jitter sample in [0, ceiling], which
// decorrelates a thundering herd of retries against one recovering receiver.
func NextBackoff(attempt int, base, max time.Duration) time.Duration {
	ceil := BackoffCeiling(attempt, base, max)
	if ceil <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(ceil) + 1))
}

// DeliveryPolicy is a delivery's dead-letter bound (spec §21.6: 72h / 20 attempts by default for the
// journal pump; the A2A pusher sets a tighter one — a task notification is time-sensitive).
type DeliveryPolicy struct {
	MaxAttempts int
	RetryWindow time.Duration
}

// RetryExhausted reports whether a delivery has hit its dead-letter cutoff: the attempt count reached the
// cap, or the elapsed time since the first attempt exceeded the retry window.
func RetryExhausted(attemptCount int, firstAt, now time.Time, policy DeliveryPolicy) bool {
	if policy.MaxAttempts > 0 && attemptCount >= policy.MaxAttempts {
		return true
	}
	if policy.RetryWindow > 0 && now.Sub(firstAt) >= policy.RetryWindow {
		return true
	}
	return false
}
