package mcp

import (
	"errors"
	"sync"
	"time"
)

// ErrToolUnavailable is the visible failure a tripped circuit breaker returns: an MCP server that has failed
// N times in a row is shed FAST (no per-call container start, no dial) so a broken/hostile connection cannot
// stall every run that names it. The control plane stays UP — this is a connection-level breaker, NOT the
// engine-host quarantine (§28.21, EXT-005). It is honest naming: the model sees tool_unavailable, not a
// silent hang.
var ErrToolUnavailable = errors.New("mcp: tool unavailable (connection circuit breaker open)")

// breaker is a per-connection, in-memory circuit breaker. Ceiling (named): in-memory only — a control-plane
// restart resets it, and it re-trips on the next failures; a durable breaker is the upgrade path if
// cross-restart shedding ever matters. It trips after threshold consecutive failures, stays open for
// cooldown, then admits ONE half-open trial whose outcome re-opens or closes it.
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

// newBreaker builds a breaker. A non-positive threshold defaults to 5; a non-positive cooldown to 30s.
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

// allow reports whether a call to connID may proceed. An open breaker denies until the cooldown elapses,
// then admits half-open trials until one records an outcome (recordSuccess closes it, recordFailure
// re-opens). ponytail: per-call containers are serial per connection, so in practice this is one trial at a
// time; under hypothetical concurrent calls it admits each until the first outcome — acceptable, not a wedge.
func (b *breaker) allow(connID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.states[connID]
	if s == nil || !s.open {
		return true
	}
	if b.now().Sub(s.openedAt) >= b.cooldown {
		s.halfOpen = true // admit one trial; recordSuccess/Failure resolves it
		return true
	}
	return false
}

// recordSuccess closes the breaker for connID (a healthy call clears the failure streak).
func (b *breaker) recordSuccess(connID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.states[connID] = &breakerState{}
}

// recordFailure counts a failure. A half-open trial that fails re-opens immediately; otherwise the streak
// trips the breaker once it reaches the threshold.
func (b *breaker) recordFailure(connID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.states[connID]
	if s == nil {
		s = &breakerState{}
		b.states[connID] = s
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
