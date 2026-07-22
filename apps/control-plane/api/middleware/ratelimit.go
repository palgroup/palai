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
// ponytail: an in-process bucket is correct for the single-replica compose deployment — every
// request for a key hits the same process, so one bucket bounds it. A multi-replica distributed
// limiter (shared counter) + weighted per-tenant fairness (QUO-002) is SaaS scope, not basic tier.
// The bucket map grows one entry per distinct key and is never evicted; the basic tier serves a
// handful of keys, so that is fine — add TTL/LRU eviction if key cardinality ever grows unbounded.
func RateLimit(ratePerSec float64, burst int) func(http.Handler) http.Handler {
	if ratePerSec <= 0 {
		return func(next http.Handler) http.Handler { return next }
	}
	lim := newKeyedLimiter(ratePerSec, burst, time.Now)
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

// keyedLimiter is a set of per-key token buckets sharing one refill rate and depth. One mutex
// guards the whole map: the limiter itself caps throughput, so contention on it is self-bounded.
type keyedLimiter struct {
	ratePerSec float64
	burst      float64
	now        func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newKeyedLimiter(ratePerSec float64, burst int, now func() time.Time) *keyedLimiter {
	return &keyedLimiter{
		ratePerSec: ratePerSec,
		burst:      float64(burst),
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
