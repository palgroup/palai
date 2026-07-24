package modelbroker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// A route's fallback chain plus a per-target circuit breaker (spec §27.9 single retry owner;
// E16 T6, MOD-005/006/008/012). The broker itself routes ONE provider (Route). A Chain wraps an
// ordered list of candidate Targets and, when a target fails UPSTREAM, tries the next — recording
// EVERY attempt, so a fallover is a NEW visible attempt with the correct target/usage, never a
// hidden retry multiplier. A target that fails upstream repeatedly is shed by its circuit; a
// caller-invalid error (the caller's own 4xx) trips nothing and fails over nowhere, because the
// same malformed request fails identically on every target.
//
// ponytail: NOT wired into the live dispatch path. effectiveRoute (E13 T8) resolves ONE target and
// explicitly defers ranking/failover; a multi-candidate route needs route config the DB does not
// carry yet. This proves the runtime behavior deterministically across the adapter families; the
// live wiring is a routing-config follow-up (§5 out-of-scope for T6's conformance mandate).
//
// ponytail: the breaker below MIRRORS adapters/integrations/mcp.Breaker (the proven per-connection
// breaker) rather than importing it — model-broker must not depend on an integration package. Same
// in-memory ceiling: it resets on restart and re-trips on the next failures; a durable breaker is
// the upgrade path if cross-restart shedding ever matters.

var (
	// ErrCircuitOpen is the shed signal for a target whose circuit is open. It never surfaces to a
	// caller when a permitted route serves the request; it is the last error only if every target
	// is open or failed.
	ErrCircuitOpen = errors.New("circuit_open")
	// ErrAllTargetsFailed is returned when no target in the chain produced a served result.
	ErrAllTargetsFailed = errors.New("all_targets_failed")
)

// Target is one candidate in a fallback chain: the provider adapter to route to and the SecretRef
// the executor redeems for it. Model, when set, overrides the request's model for this target (a
// fallback family may run a different model id); empty leaves the request's model unchanged.
type Target struct {
	Provider string
	Secret   SecretRef
	Model    string
}

// Chain routes a request through an ordered fallback chain, gated by a per-target circuit breaker.
type Chain struct {
	broker  *Broker
	breaker *breaker
}

// NewChain builds a Chain over a broker. A non-positive threshold defaults to 5 consecutive upstream
// failures before a target's circuit opens; a non-positive cooldown defaults to 30s open before one
// half-open trial.
func NewChain(b *Broker, threshold int, cooldown time.Duration) *Chain {
	return &Chain{broker: b, breaker: newBreaker(threshold, cooldown, time.Now)}
}

// Route executes req against the first target that a) is not shed by its circuit and b) does not
// fail upstream. It records every attempt it makes: a served result reports the cumulative Attempts
// (so a fallover surfaces as 2, an honest count). A caller-invalid outcome is returned immediately
// without failing over; a cancel/deadline/budget outcome is returned immediately (caller intent or
// platform cutoff). If every target is shed or fails upstream, the last failure is returned.
func (c *Chain) Route(ctx context.Context, targets []Target, req Request, onDelta func(Delta)) (Result, error) {
	if len(targets) == 0 {
		return Result{}, errors.New("route chain: no targets")
	}
	var totalAttempts int
	var lastResult Result
	var lastErr error = ErrAllTargetsFailed
	for _, tgt := range targets {
		key := tgt.Provider + "|" + string(tgt.Secret)
		if !c.breaker.Allow(key) {
			lastErr = fmt.Errorf("%w: %s", ErrCircuitOpen, tgt.Provider)
			continue // shed this target fast — no call — and try the next
		}
		call := req
		call.Secret = tgt.Secret
		if tgt.Model != "" {
			call.Model = tgt.Model
		}
		res, err := c.broker.Route(ctx, tgt.Provider, call, onDelta)
		oc := classifyOutcome(err, res)
		// Count an attempt only when a real provider call was made. A misconfigured target (unknown
		// provider/secret) is rejected before the adapter runs, so it is NOT an attempt.
		if oc != outcomeMisconfigured {
			totalAttempts += attemptsOf(res)
		}

		switch oc {
		case outcomeSuccess:
			c.breaker.RecordSuccess(key)
			res.Attempts = totalAttempts
			return res, nil
		case outcomeStop:
			// Cancel / deadline / budget cutoff — a fallover cannot help and must not mask it. Report
			// the cumulative attempts made before the stop (this target's call plus any prior fallovers).
			res.Attempts = totalAttempts
			return res, err
		case outcomeCallerInvalid:
			// The caller's own request is bad; it fails identically on every target. Do not trip the
			// circuit and do not fail over — surface it (the provider-side error rides on the Result).
			res.Attempts = totalAttempts
			return res, err
		case outcomeMisconfigured:
			// A misconfigured target (unknown provider/secret) is routed AROUND without tripping the
			// upstream-health circuit — the next target may be configured.
			lastResult, lastErr = res, err
		case outcomeUpstream:
			c.breaker.RecordFailure(key)
			lastResult, lastErr = res, upstreamErr(err, res)
		}
	}
	return lastResult, lastErr
}

// outcome classifies one target's result for the chain.
type outcome int

const (
	outcomeSuccess       outcome = iota
	outcomeStop                  // cancel / deadline / budget — return as-is, never fail over
	outcomeCallerInvalid         // the caller's 4xx — return, no trip, no fallover
	outcomeMisconfigured         // unknown provider/secret — fail over, do not trip the circuit
	outcomeUpstream              // transport error or provider 5xx/429/401/403 — trip + fail over
)

// classifyOutcome decides how the chain treats one target's (err, res). A provider-side failure
// rides on res.Error (Go err nil); a transport/config failure is the Go err.
func classifyOutcome(err error, res Result) outcome {
	switch {
	case err == nil && res.Error == nil:
		return outcomeSuccess
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded), errors.Is(err, ErrBudgetExceeded):
		return outcomeStop
	case errors.Is(err, ErrUnknownProvider), errors.Is(err, ErrUnknownSecret):
		return outcomeMisconfigured
	case err != nil:
		return outcomeUpstream // an adapter transport/upstream error
	case callerInvalidStatus(res.Error.Status):
		return outcomeCallerInvalid
	default:
		return outcomeUpstream // a provider-side 5xx/429/401/403 — the target is unhealthy
	}
}

// callerInvalidStatus reports whether an HTTP status is the caller's own fault (a malformed request)
// rather than a target-health failure. Only a 400/422 is caller-invalid: it fails identically on
// every target, so failing over is pointless and the circuit must not count it. A 401/403 (bad
// credential) IS worth failing over — a different target carries a different credential.
func callerInvalidStatus(status int) bool {
	return status == 400 || status == 422
}

// attemptsOf reports how many provider attempts one Route made: a served/error Result carries its
// own Attempts; a transport Go error left a zero Result, which counts as one attempt.
func attemptsOf(res Result) int {
	if res.Attempts == 0 {
		return 1
	}
	return res.Attempts
}

// upstreamErr yields a non-nil error for an upstream failure so the chain's last error is meaningful
// even when the failure rode on res.Error (Go err nil).
func upstreamErr(err error, res Result) error {
	if err != nil {
		return err
	}
	if res.Error != nil {
		return fmt.Errorf("%w: provider %s (status %d)", ErrAllTargetsFailed, res.Error.Code, res.Error.Status)
	}
	return ErrAllTargetsFailed
}

// breaker is a per-target, in-memory circuit breaker (the mcp.Breaker mirror; see the file header).
// It trips after threshold consecutive failures, stays open for cooldown, then admits a half-open
// trial whose outcome re-opens or closes it. ponytail (the caveat the mcp original names and this
// context sharpens): during the half-open window Allow admits EVERY caller until the first outcome
// lands — mcp's per-connection calls are serial so it is one trial in practice, but Chain.Route can
// be driven concurrently, so several fallover attempts may slip through one half-open window. That
// is acceptable (they all resolve the trial), not a wedge; a strict single-trial gate is the upgrade
// path if a thundering half-open ever matters.
type breaker struct {
	mu        sync.Mutex
	states    map[string]*breakerState
	threshold int
	cooldown  time.Duration
	now       func() time.Time
}

type breakerState struct {
	failures int
	openedAt time.Time
	open     bool
	halfOpen bool
}

func newBreaker(threshold int, cooldown time.Duration, now func() time.Time) *breaker {
	if threshold <= 0 {
		threshold = 5
	}
	if cooldown <= 0 {
		cooldown = 30 * time.Second
	}
	if now == nil {
		now = time.Now
	}
	return &breaker{states: map[string]*breakerState{}, threshold: threshold, cooldown: cooldown, now: now}
}

// Allow reports whether a call to key may proceed. An open breaker denies until the cooldown elapses,
// then admits half-open trials that RecordSuccess/RecordFailure resolves — every caller during the
// half-open window is admitted until an outcome lands (see the breaker type's ponytail caveat).
func (b *breaker) Allow(key string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.states[key]
	if s == nil || !s.open {
		return true
	}
	if b.now().Sub(s.openedAt) >= b.cooldown {
		s.halfOpen = true
		return true
	}
	return false
}

// RecordSuccess closes the breaker for key (a healthy call clears the failure streak).
func (b *breaker) RecordSuccess(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.states[key] = &breakerState{}
}

// RecordFailure counts a failure. A failed half-open trial re-opens immediately; otherwise the streak
// trips the breaker once it reaches the threshold.
func (b *breaker) RecordFailure(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.states[key]
	if s == nil {
		s = &breakerState{}
		b.states[key] = s
	}
	if s.halfOpen {
		s.open = true
		s.halfOpen = false
		s.openedAt = b.now()
		return
	}
	s.failures++
	if s.failures >= b.threshold {
		s.open = true
		s.openedAt = b.now()
	}
}
