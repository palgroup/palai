package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
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
	// always lease-safe. It authenticates with the current certificate — the bootstrap token is
	// never presented on this path; Reenroll below is the only thing that presents it again.
	Renew func(ctx context.Context, current Identity) (Identity, error)
	// Reenroll is the RECOVERY path renewal cannot serve: it re-presents the file-mounted
	// bootstrap credential to obtain a wholly new identity. It runs when and only when the
	// runner holds no usable identity — the current certificate has already expired, so
	// renewal-over-mTLS is impossible for the same reason it was needed. nil disables it (the
	// pre-recovery behaviour: an expired identity is terminal until the process is restarted).
	Reenroll func(ctx context.Context) (Identity, error)
	Now      func() time.Time
	Log      func(format string, args ...any)
	Backoff  time.Duration // between a failed dial/renewal and the next attempt; zero = 1s
	// Concurrency is how many leases the runner parks at once on its shared enrolled identity.
	// Zero or one is the sequential one-lease-at-a-time default (LP-0 unchanged); >1 lets a
	// delegating run's parent hold its engine while an inline child dials its own on the same
	// runner (spec §25.18), instead of deadlocking on a single lease slot.
	Concurrency int
	// WorkspaceRoot is the runner's managed allocation root: a lease's workspace host path must sit
	// under it before the runner bind-mounts it, so a control plane cannot make the runner mount an
	// arbitrary host path (spec §30.13). A §30.13 unsafe local bind (REP-012) is the only exception,
	// and only when AllowUnsafeBind is set. Empty disables the under-root check — the pre-E09
	// behaviour for a runner with no configured workspace root.
	WorkspaceRoot string
	// AllowUnsafeBind lets this runner honour a lease's WorkspaceUnsafe flag (a §30.13 direct host
	// bind mount). Default false: a control plane alone cannot make the runner mount an arbitrary host
	// path — the runner's OWN operator must opt in (PALAI_WORKSPACE_UNSAFE_BIND=1), preserving the §24
	// trust boundary between control plane and runner.
	AllowUnsafeBind bool
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
	state := &serveState{identity: cfg.Session.Identity}

	var wg sync.WaitGroup
	if cfg.Renew != nil {
		wg.Go(func() {
			cfg.renewLoop(ctx, state, now, logf, backoff)
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
		wg.Go(func() { cfg.parkLoop(ctx, state, logf, backoff) })
	}
	wg.Wait()
}

// serveState is the identity the lease loops and the renewer share, plus the say-it-once gates
// for the operator-facing notices. A 1/s retry loop that repeats the same sentence forever buries
// the one thing the operator needs, so each notice is printed once for the runner's lifetime.
type serveState struct {
	mu       sync.Mutex
	identity Identity
	stale    sync.Once // a dial rejected the client certificate
	expired  sync.Once // the identity is past NotAfter, so recovery (not renewal) is running
	cure     sync.Once // recovery itself failed — the operator has to act
}

func (s *serveState) current() Identity {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.identity
}

func (s *serveState) replace(identity Identity) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.identity = identity
}

// parkLoop parks for one lease at a time and supervises the leased engine until ctx is
// cancelled, re-reading the shared (renewable) identity for each dial. N of these run
// concurrently per Serve's Concurrency, each an independent lease slot on one runner identity.
func (cfg ServeConfig) parkLoop(ctx context.Context, state *serveState, logf func(string, ...any), backoff time.Duration) {
	for {
		if ctx.Err() != nil {
			return
		}
		session := cfg.Session
		session.Identity = state.current()

		leaseSession, err := session.OpenLease(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return // signalled to stop between leases — clean exit, no spin
			}
			// A transient dial error must not end the runner. A stale-identity error (the
			// certificate was rejected) is not fixed by re-dialing the same cert — the renewer
			// refreshes or replaces it concurrently — so both cases back off and retry. The
			// stale case names what is happening ONCE; repeating it every second would bury it.
			logf("open lease: %v; retrying%s", err, cfg.staleIdentityHint(err, state))
			if sleep(ctx, backoff) != nil {
				return
			}
			continue
		}
		serveLease(ctx, cfg.Supervisor, leaseSession, cfg.WorkspaceRoot, cfg.AllowUnsafeBind, logf)
	}
}

// renewLoop keeps the runner's identity usable, on its own connection — never touching a parked
// or in-flight lease. It waits until each certificate's renewal point and rolls the certificate
// forward over the current identity; when that window was MISSED and the identity is already
// past NotAfter, it recovers with Reenroll instead, because renewal authenticates with the very
// certificate that expired and so cannot serve the one case that needs it most.
func (cfg ServeConfig) renewLoop(ctx context.Context, state *serveState, now func() time.Time, logf func(string, ...any), backoff time.Duration) {
	for {
		current := state.current()

		deadline, ok := renewalDeadline(current)
		if !ok {
			return // no renewable certificate
		}
		if wait := deadline.Sub(now()); wait > 0 {
			if sleep(ctx, wait) != nil {
				return
			}
		}

		renewed, err := cfg.refresh(ctx, current, now, logf, state)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			logf("%v; retrying", err)
			if sleep(ctx, backoff) != nil {
				return
			}
			continue
		}
		state.replace(renewed)
		logf("runner certificate valid until %s", renewed.NotAfter.UTC().Format(time.RFC3339))
	}
}

// refresh produces a usable identity from the current one: renewal over mTLS while the
// certificate is still valid (the normal path), or re-enrollment once it has expired (the
// recovery path). An expired certificate cannot complete the renew handshake, so spending one
// doomed handshake per backoff would only hide the real state — the expiry is checked FIRST.
func (cfg ServeConfig) refresh(ctx context.Context, current Identity, now func() time.Time, logf func(string, ...any), state *serveState) (Identity, error) {
	if current.NotAfter.After(now()) || cfg.Reenroll == nil {
		renewed, err := cfg.Renew(ctx, current)
		if err != nil {
			return Identity{}, fmt.Errorf("renew runner certificate: %w%s", err, cfg.expiredCure(current, now, state))
		}
		return renewed, nil
	}
	state.expired.Do(func() {
		logf("runner identity expired at %s: renewal needs the certificate that expired, so the runner is re-enrolling with its bootstrap credential — no restart needed",
			current.NotAfter.UTC().Format(time.RFC3339))
	})
	reenrolled, err := cfg.Reenroll(ctx)
	if err != nil {
		return Identity{}, fmt.Errorf("re-enroll expired runner identity: %w%s", err, cureHint(state))
	}
	return reenrolled, nil
}

// expiredCure names the operator's manual cure ONCE, and only for the runner that cannot help
// itself: no Reenroll is configured and its certificate has expired, so every retry from here is
// doomed. A runner with a recovery path says nothing — it fixes itself.
func (cfg ServeConfig) expiredCure(current Identity, now func() time.Time, state *serveState) string {
	if cfg.Reenroll != nil || current.NotAfter.After(now()) {
		return ""
	}
	return cureHint(state)
}

// cureHint is the operator-facing cure, printed at most once for the runner's lifetime.
func cureHint(state *serveState) string {
	hint := ""
	state.cure.Do(func() {
		hint = " — this runner's identity has expired and it cannot replace it; restart the stack to re-enroll it (`palai local down && palai local up`)"
	})
	return hint
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

// staleIdentityHint names a dial failure whose cause is a rejected client certificate — one that
// re-dialing the same identity cannot clear — so the log distinguishes it from a transient
// network error. The renewer refreshes or (once the certificate has expired) replaces the
// identity concurrently, so both cases simply retry. Said ONCE: this loop runs at 1/s, and a
// sentence repeated every second is a sentence nobody reads.
func (cfg ServeConfig) staleIdentityHint(err error, state *serveState) string {
	msg := err.Error()
	if !strings.Contains(msg, "tls:") && !strings.Contains(msg, "certificate") && !strings.Contains(msg, "expired") {
		return ""
	}
	hint := ""
	state.stale.Do(func() {
		if cfg.Reenroll != nil {
			hint = " (the client identity is stale; the runner is refreshing it — or re-enrolling if it has expired — and will pick it up on the next dial, no restart needed)"
			return
		}
		hint = " (the client identity is stale; renewal is refreshing it)"
	})
	return hint
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
func serveLease(ctx context.Context, supervisor *StreamSupervisor, leaseSession *LeaseSession, allocationRoot string, allowUnsafeBind bool, logf func(string, ...any)) {
	defer leaseSession.Close()
	lease := leaseSession.Lease()
	logf("received lease %s for run %s (fence %d)", lease.LeaseID, lease.RunID, lease.Fence)

	// A normal allocation must sit under the runner's managed root before it is bind-mounted; an
	// unsafe local bind requires the runner's own opt-in (spec §30.13, §24 boundary). Reject, don't mount.
	if err := admitWorkspaceMount(lease, allocationRoot, allowUnsafeBind); err != nil {
		logf("reject lease %s: %v", lease.LeaseID, err)
		if cerr := leaseSession.Complete(ctx, "failed", ""); cerr != nil {
			logf("report rejected lease completion for run %s: %v", lease.RunID, cerr)
		}
		return
	}

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
		ImageDigest:       lease.ImageDigest,
		RunID:             lease.RunID,
		AttemptID:         lease.AttemptID,
		Fence:             lease.Fence,
		Limits:            lease.Limits,
		WorkspaceHostPath: lease.WorkspaceHostPath,
		WorkspaceReadOnly: lease.WorkspaceReadOnly,
	}, inbound, sink)

	if err := leaseSession.Complete(ctx, OutcomeClass(streamErr), stderrDigest(result.Stderr)); err != nil {
		logf("report lease completion for run %s: %v", lease.RunID, err)
		return
	}
	if streamErr != nil {
		// THE STDERR ITSELF, NOT ITS LENGTH. The line above this one hashes result.Stderr into the lease
		// completion, so the bytes are in hand — and what reached the operator was the COUNT of them.
		// Measured 2026-08-02 driving the iOS live chain: a run oscillated waiting→running for four
		// minutes over five leases, and every attempt printed "(exit 0, 134 stderr bytes): engine wait did
		// not complete after stdout closed". The cause was 134 bytes away and the log reported its size.
		//
		// It is BOUNDED and one line: a runaway engine can produce megabytes, and the last few hundred
		// bytes are where a process says why it is stopping.
		logf("supervise engine for run %s (exit %d): %v — engine stderr: %s",
			lease.RunID, result.ExitCode, streamErr, tailStderr(result.Stderr))
		return
	}
	logf("engine completed for run %s: %d stdout bytes", lease.RunID, result.StdoutBytes)
}

// workspaceUnderRoot verifies a lease's workspace host path sits under the runner's managed
// allocation root before it is bind-mounted, so a compromised or buggy control plane cannot make
// the runner mount an arbitrary host path such as /etc (spec §30.13, carry (b)). Both the root and the
// path are symlink-resolved so a symlinked allocation cannot smuggle an out-of-root target past the
// prefix comparison.
//
// AN UNSET ROOT REFUSES A LEASE THAT CARRIES A PATH, AND THAT REVERSES A DECISION RATHER THAN FIXING AN
// OVERSIGHT. This function used to return nil for an empty root — "disables the check (no managed root
// configured)" — and packages/runner/workspace_lease_test.go asserted it. The reversal is measured, not
// preferred. On 2026-08-01 no shipped file gave a runner that root:
//
//	runner `environment:` block carries PALAI_WORKSPACE_ROOT?
//	  deploy/compose/compose.yaml               NO
//	  deploy/compose/production.yml             NO
//	  deploy/compose/native-control-plane.yml   NO   <- binds the PATH as a volume, sets no variable
//	  deploy/airgap/airgap.yml                  NO
//	docker inspect <live runner> | grep -c PALAI_WORKSPACE_ROOT   -> 0
//
// So "pre-E09 behaviour" was not a compatibility path some old deployment was on. It was the path EVERY
// deployment was on, which made the arm above it dead code and its own comment — "so a control plane
// cannot make the runner mount an arbitrary host path such as /etc" — false everywhere it shipped.
//
// IT WAS NOT EXPLOITABLE, AND THAT IS WHY IT SURVIVED. Workspaces are off on compose (the CONTROL PLANE's
// root is unset too, so `GET /v1/capabilities` reports `workspaces = unavailable` and no lease carries a
// path at all). The hole ARMS ITSELF the moment an operator turns the feature on: the control plane starts
// provisioning when ITS PALAI_WORKSPACE_ROOT is set (palai-control-plane/main.go:677) and nothing requires
// the runner's. One variable name, two planes, two meanings — set the one that makes the feature work and
// the one that guards it stays silent.
//
// A runner that cannot tell whether a path is inside its managed root has no basis on which to mount it,
// and failing open hands that decision to the control plane, which is precisely the party §24 draws the
// boundary against. An empty PATH is untouched: that is a workspace-less lease, which is what every
// non-coding run sends.
//
// WHAT THIS BREAKS, AND WHY THE SAME COMMIT FIXES IT: the NATIVE posture is the one shipped configuration
// where workspaces work, and its overlay bound ${PALAI_WORKSPACE_ROOT} into the runner as a VOLUME while
// setting no variable — so this refusal would have refused every coding run on it. deploy/compose/
// native-control-plane.yml now sets the variable in the runner'"'"'s environment block beside the bind, and
// TestTheNativeOverlayGivesTheRunnerTheRootItBindsIntoIt is the guard that keeps the two in step.
// admitWorkspaceMount decides whether the runner may bind-mount a lease's workspace. A normal
// allocation must sit under the runner's managed root, so a control plane cannot make the runner mount
// an arbitrary host path (spec §30.13). An unsafe local bind (REP-012) is exempt from that check — but
// ONLY when this runner's operator opted in (allowUnsafeBind), so a control plane alone cannot escalate
// to an arbitrary host mount across the §24 trust boundary.
func admitWorkspaceMount(lease Lease, allocationRoot string, allowUnsafeBind bool) error {
	if lease.WorkspaceUnsafe {
		if !allowUnsafeBind {
			return errors.New("unsafe local bind requested but runner has not opted in (PALAI_WORKSPACE_UNSAFE_BIND=1)")
		}
		return nil
	}
	return workspaceUnderRoot(lease.WorkspaceHostPath, allocationRoot)
}

func workspaceUnderRoot(path, root string) error {
	if path == "" {
		// A workspace-less lease. There is nothing to place, so there is nothing to refuse.
		return nil
	}
	if root == "" {
		return errors.New("this lease carries a workspace to bind-mount and this runner has no managed allocation " +
			"root, so it cannot tell whether the path is one it minted — set PALAI_WORKSPACE_ROOT on the runner to the " +
			"same host directory the control plane allocates under (spec §30.13, §24)")
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve runner allocation root: %w", err)
	}
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("resolve workspace path: %w", err)
	}
	rel, err := filepath.Rel(realRoot, realPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("workspace path is outside the runner allocation root")
	}
	return nil
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

// maxLoggedStderr bounds the engine stderr echoed into the runner log to the last 2 KiB.
//
// THE TAIL AND NOT THE HEAD, because a process that dies says why at the END. A head-bounded excerpt of
// a chatty engine is its banner, which is the one part of the output that is the same on every run and
// therefore says nothing about this one.
const maxLoggedStderr = 2 << 10

// tailStderr renders engine stderr for one log line: the last maxLoggedStderr bytes, newlines flattened
// so a multi-line panic does not break the line-per-event shape the rest of this log has.
func tailStderr(b []byte) string {
	if len(b) == 0 {
		// AN EMPTY STDERR IS A FINDING, not a blank. It says the engine died without explaining itself,
		// which sends a reader to the image and the limits rather than to a message that is not there.
		return "(empty — the engine wrote nothing to stderr)"
	}
	trimmed := ""
	if len(b) > maxLoggedStderr {
		b = b[len(b)-maxLoggedStderr:]
		trimmed = "…(tail) "
	}
	return trimmed + strings.Join(strings.Fields(string(b)), " ")
}
