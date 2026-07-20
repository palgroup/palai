package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/palgroup/palai/packages/contracts"
)

// renewalFraction is the point in a certificate's lifetime at which the runner renews it —
// 80% (8/10) through the validity window — leaving the final 20% as margin to roll the
// certificate forward before it can expire.
const renewalFraction = 8

// ServeConfig drives the runner's park -> lease -> supervise loop with certificate renewal.
type ServeConfig struct {
	Session    Session
	Supervisor *StreamSupervisor
	// Renew rolls the client certificate forward over the runner's existing identity; nil
	// disables renewal (a one-shot, single-lifetime runner). Renewal runs on its OWN mTLS
	// connection and never touches a parked or in-flight lease connection, so a rollover is
	// always lease-safe. It authenticates with the current certificate — the one-use
	// bootstrap token is never presented again.
	Renew   func(ctx context.Context, current Identity) (Identity, error)
	Now     func() time.Time
	Log     func(format string, args ...any)
	Backoff time.Duration // between a failed dial/renewal and the next attempt; zero = 1s
	// Concurrency is how many leases the runner parks at once on its shared enrolled identity.
	// Zero or one is the sequential one-lease-at-a-time default (LP-0 unchanged); >1 lets a
	// delegating run's parent hold its engine while an inline child dials its own on the same
	// runner (spec §25.18), instead of deadlocking on a single lease slot.
	Concurrency int
}

// Serve runs the runner's lease loop until ctx is cancelled: it parks for a lease, supervises
// the leased engine, and repeats, while a background renewer rolls the client certificate
// forward as it nears expiry. The renewer runs on a separate connection, so a rollover never
// interrupts a parked or in-flight lease; each fresh dial picks up the renewed identity, so a
// re-dial after the original certificate would have expired still authenticates — closing the
// review's "open lease...retrying" 1/s-forever loop on expiry.
func (cfg ServeConfig) Serve(ctx context.Context) {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	logf := cfg.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	backoff := cfg.Backoff
	if backoff <= 0 {
		backoff = time.Second
	}

	// The identity is shared between the lease loop (which reads it for each dial) and the
	// renewer (which replaces it on rollover). The renewer never touches the live connection,
	// only the identity the NEXT dial will use — that is what makes the rollover lease-safe.
	var mu sync.Mutex
	identity := cfg.Session.Identity

	var wg sync.WaitGroup
	if cfg.Renew != nil {
		wg.Go(func() {
			cfg.renewLoop(ctx, &mu, &identity, now, logf, backoff)
		})
	}

	// Park N leases concurrently on the shared identity (default 1 = the sequential LP-0
	// behaviour). >1 lets a delegating run hold its parent engine while an inline child dials
	// its own on the same runner (spec §25.18), rather than deadlocking on one lease slot.
	loops := cfg.Concurrency
	if loops < 1 {
		loops = 1
	}
	for range loops {
		wg.Go(func() { cfg.parkLoop(ctx, &mu, &identity, logf, backoff) })
	}
	wg.Wait()
}

// parkLoop parks for one lease at a time and supervises the leased engine until ctx is
// cancelled, re-reading the shared (renewable) identity for each dial. N of these run
// concurrently per Serve's Concurrency, each an independent lease slot on one runner identity.
func (cfg ServeConfig) parkLoop(ctx context.Context, mu *sync.Mutex, identity *Identity, logf func(string, ...any), backoff time.Duration) {
	for {
		if ctx.Err() != nil {
			return
		}
		mu.Lock()
		session := cfg.Session
		session.Identity = *identity
		mu.Unlock()

		leaseSession, err := session.OpenLease(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return // signalled to stop between leases — clean exit, no spin
			}
			// A transient dial error must not end the runner. A stale-identity error (the
			// certificate was rejected) is not fixed by re-dialing the same cert — the renewer
			// refreshes it concurrently — so both cases back off and retry; the log names which.
			logf("open lease: %v; retrying%s", err, staleIdentityHint(err))
			if sleep(ctx, backoff) != nil {
				return
			}
			continue
		}
		serveLease(ctx, cfg.Supervisor, leaseSession, logf)
	}
}

// renewLoop rolls the certificate forward as it nears expiry, on its own mTLS connection. It
// waits until each certificate's renewal point, renews over the current identity, and swaps
// in the result — never touching a parked or in-flight lease connection.
func (cfg ServeConfig) renewLoop(ctx context.Context, mu *sync.Mutex, identity *Identity, now func() time.Time, logf func(string, ...any), backoff time.Duration) {
	for {
		mu.Lock()
		current := *identity
		mu.Unlock()

		deadline, ok := renewalDeadline(current)
		if !ok {
			return // no renewable certificate
		}
		if wait := deadline.Sub(now()); wait > 0 {
			if sleep(ctx, wait) != nil {
				return
			}
		}

		renewed, err := cfg.Renew(ctx, current)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			logf("renew runner certificate: %v; retrying", err)
			if sleep(ctx, backoff) != nil {
				return
			}
			continue
		}
		mu.Lock()
		*identity = renewed
		mu.Unlock()
		logf("renewed runner certificate; valid until %s", renewed.NotAfter.UTC().Format(time.RFC3339))
	}
}

// renewalDeadline is the instant a certificate reaches its renewal point (renewalFraction of
// the way through its validity window) — when the renewer rolls it forward.
func renewalDeadline(identity Identity) (time.Time, bool) {
	leaf := identity.Certificate.Leaf
	if leaf == nil {
		return time.Time{}, false
	}
	total := leaf.NotAfter.Sub(leaf.NotBefore)
	if total <= 0 {
		return time.Time{}, false
	}
	return leaf.NotBefore.Add(total * renewalFraction / 10), true
}

// staleIdentityHint names a dial failure whose cause is a rejected client certificate — one
// that re-dialing the same identity cannot clear — so the log distinguishes it from a
// transient network error. The background renewer refreshes the identity concurrently, so
// both cases simply retry; the hint is for the operator reading the log.
func staleIdentityHint(err error) string {
	msg := err.Error()
	if strings.Contains(msg, "tls:") || strings.Contains(msg, "certificate") || strings.Contains(msg, "expired") {
		return " (client identity may be stale; renewal is refreshing it)"
	}
	return ""
}

// sleep blocks for d or until ctx is cancelled, returning the context error on cancellation
// so the caller can distinguish a backoff that elapsed from a shutdown.
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// serveLease supervises one leased engine to a terminal outcome: it relays controller frames
// into the engine and engine frames back to the control plane, then reports lease completion.
// A lease-scoped context stops the inbound relay goroutine so it never outlives the lease. A
// failed lease is logged, not fatal, so one bad engine does not end the runner's service.
func serveLease(ctx context.Context, supervisor *StreamSupervisor, leaseSession *LeaseSession, logf func(string, ...any)) {
	defer leaseSession.Close()
	lease := leaseSession.Lease()
	logf("received lease %s for run %s (fence %d)", lease.LeaseID, lease.RunID, lease.Fence)

	leaseCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Controller frames relayed over the lease feed the supervisor's stdin injection; the
	// supervisor's forwarded engine frames are relayed back to the controller by the sink.
	inbound := make(chan contracts.EngineFrame)
	go func() {
		defer close(inbound)
		for {
			frame, err := leaseSession.ReceiveControllerFrame(leaseCtx)
			if err != nil {
				return
			}
			select {
			case inbound <- frame:
			case <-leaseCtx.Done():
				return
			}
		}
	}()

	sink := func(ctx context.Context, frame contracts.EngineFrame) error {
		return leaseSession.SendEngineFrame(ctx, frame)
	}

	result, streamErr := supervisor.Stream(leaseCtx, EngineRequest{
		ImageDigest: lease.ImageDigest,
		RunID:       lease.RunID,
		AttemptID:   lease.AttemptID,
		Fence:       lease.Fence,
		Limits:      lease.Limits,
	}, inbound, sink)

	if err := leaseSession.Complete(ctx, OutcomeClass(streamErr), stderrDigest(result.Stderr)); err != nil {
		logf("report lease completion for run %s: %v", lease.RunID, err)
		return
	}
	if streamErr != nil {
		logf("supervise engine for run %s (exit %d, %d stderr bytes): %v", lease.RunID, result.ExitCode, result.StderrBytes, streamErr)
		return
	}
	logf("engine completed for run %s: %d stdout bytes", lease.RunID, result.StdoutBytes)
}

// OutcomeClass maps a supervised streaming outcome to the lease.complete outcome class the
// control plane records: a wall-time kill is lost, any other failure is failed, and a clean
// run is succeeded.
func OutcomeClass(err error) string {
	switch {
	case err == nil:
		return "succeeded"
	case errors.Is(err, ErrEngineTimeout):
		return "lost"
	default:
		return "failed"
	}
}

// stderrDigest is the content digest of the already-redacted stderr the runner reports on
// completion, so the controller can correlate logs without the runner shipping raw stderr.
func stderrDigest(redacted []byte) string {
	sum := sha256.Sum256(redacted)
	return "sha256:" + hex.EncodeToString(sum[:])
}
