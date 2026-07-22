package middleware

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// RateLimit is the §20.12 basic-tier request-rate limiter: an in-process token bucket keyed by
// the presented bearer API key. It sits AHEAD of Auth so a flood is shed before the credential
// DB read, and it keys on a hash of the token (no raw key retained in the bucket map). On exceed
// it renders 429 + Retry-After + an RFC 9457 problem; a request with no bearer token is passed
// through untouched (Auth answers it 401). ratePerSec is the sustained refill and burst the bucket
// depth; ratePerSec <= 0 disables the limiter entirely (the middleware becomes a pass-through).
//
// The limiter sits BEFORE Auth, so the key is an ATTACKER-controlled bearer string: the bucket map is
// hard-capped at maxLimiterBuckets and evicts fully-refilled (idle) buckets — resetting outright if a
// live flood of distinct keys still fills it — so a `Bearer junk$i` storm cannot exhaust memory. A
// reset costs at most one legitimate key its throttle state (a single burst slips through), never the
// process.
//
// ponytail: an in-process bucket is correct for the single-replica compose deployment — every
// request for a key hits the same process, so one bucket bounds it. A multi-replica distributed
// limiter (shared counter) + weighted per-tenant fairness (QUO-002) is SaaS scope, not basic tier.
func RateLimit(ratePerSec float64, burst int) func(http.Handler) http.Handler {
	if ratePerSec <= 0 {
		return func(next http.Handler) http.Handler { return next }
	}
	// Clamp burst to at least the per-second rate: a rate set with burst forgotten (<=0) would start
	// every bucket at 0 and 429 EVERY authenticated request forever (total lockout). A configured
	// burst is honoured as-is.
	if burst < 1 {
		burst = int(math.Ceil(ratePerSec))
		if burst < 1 {
			burst = 1
		}
	}
	lim := newKeyedLimiter(ratePerSec, burst, maxLimiterBuckets, time.Now)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r.Header.Get("Authorization"))
			if !ok {
				next.ServeHTTP(w, r) // no key to meter — Auth rejects it
				return
			}
			sum := sha256.Sum256([]byte(token))
			allowed, retry := lim.allow(hex.EncodeToString(sum[:]))
			if !allowed {
				w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retry.Seconds()))))
				WriteProblem(w, r, http.StatusTooManyRequests, "rate_limited",
					"the request rate for this API key has been exceeded")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// maxLimiterBuckets hard-caps the live bucket map. Pre-Auth the key is an attacker-controlled bearer
// string, so this bound (≈ a few MB at ~200B/entry) is the memory-exhaustion backstop, not a tuning knob.
const maxLimiterBuckets = 50_000

// keyedLimiter is a set of per-key token buckets sharing one refill rate and depth. One mutex
// guards the whole map: the limiter itself caps throughput, so contention on it is self-bounded.
type keyedLimiter struct {
	ratePerSec float64
	burst      float64
	maxBuckets int
	now        func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newKeyedLimiter(ratePerSec float64, burst, maxBuckets int, now func() time.Time) *keyedLimiter {
	return &keyedLimiter{
		ratePerSec: ratePerSec,
		burst:      float64(burst),
		maxBuckets: maxBuckets,
		now:        now,
		buckets:    map[string]*bucket{},
	}
}

// allow consumes one token for key, refilling by the elapsed time first. It returns whether the
// request is admitted and, when denied, how long until a whole token accrues (the Retry-After).
func (l *keyedLimiter) allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	t := l.now()
	b, ok := l.buckets[key]
	if !ok {
		if l.maxBuckets > 0 && len(l.buckets) >= l.maxBuckets {
			l.evict(t)
		}
		b = &bucket{tokens: l.burst, last: t}
		l.buckets[key] = b
	}
	// Refill for the elapsed interval, capped at the bucket depth.
	if elapsed := t.Sub(b.last); elapsed > 0 {
		b.tokens = math.Min(l.burst, b.tokens+elapsed.Seconds()*l.ratePerSec)
		b.last = t
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	// Time for the fractional shortfall to refill to one whole token.
	wait := time.Duration((1 - b.tokens) / l.ratePerSec * float64(time.Second))
	return false, wait
}

// evict bounds the map (caller holds the lock). It first drops every bucket that has sat idle long
// enough to fully refill — those carry no state a fresh bucket wouldn't, so dropping them is free. If a
// live flood of distinct keys still fills the map (the DoS: buckets all just created, none refilled),
// it resets the map outright: bounded memory wins over one key's throttle state (a lone burst slips).
func (l *keyedLimiter) evict(now time.Time) {
	fullRefill := l.burst / l.ratePerSec // seconds for an empty bucket to reach full depth
	for k, b := range l.buckets {
		if now.Sub(b.last).Seconds() >= fullRefill {
			delete(l.buckets, k)
		}
	}
	if len(l.buckets) >= l.maxBuckets {
		l.buckets = make(map[string]*bucket, l.maxBuckets)
	}
}
