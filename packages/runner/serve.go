package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	"github.com/palgroup/palai/packages/contracts"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
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
	// PersistIdentity is called with every identity this loop obtains after the first — each renewal and
	// each recovery — so a device can write the new certificate beside its durable key.
	//
	// ‼️ IT EXISTS BECAUSE AN UNPERSISTED RENEWAL IS INVISIBLE UNTIL A RESTART, AND THEN EXPENSIVE. The
	// runner rolls its certificate forward in memory; without this hook the disk keeps the certificate
	// the machine enrolled with, so a restart after that one expires takes the RECOVERY path and spends a
	// pool key to be issued something the machine had already been issued. On a machine whose key file
	// was removed after installation — a provisioner that deleted it, which is a reasonable thing to do —
	// there is no recovery path at all and the machine is simply out of the fleet.
	//
	// nil is every runner built before this field, and it costs them nothing: they had no disk identity
	// to keep in step.
	PersistIdentity func(Identity) error
	Now             func() time.Time
	Log             func(format string, args ...any)
	Backoff         time.Duration // between a failed dial/renewal and the next attempt; zero = 1s
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
	// Shell runs an exec.request on THIS machine (toolserver.go). It is what makes where a command
	// runs a property of the machine that took the lease rather than of the control-plane process, and
	// it is the reason this package now depends on the tool-broker seam at all.
	//
	// nil is a runner that serves engines but runs no commands, which is every runner built before
	// this field existed. It is not silent: an exec.request still gets an exec.result carrying a
	// refusal, because a control plane blocking a tool call on this machine's answer must be told it
	// will not get one.
	Shell toolbroker.ShellRunner
	// Settings polls the control plane for this machine's current configuration, reporting in the same
	// round trip what the machine did with the previous document. nil disables the poll entirely — a
	// machine then receives its configuration once, at enrolment, which is the behaviour of every runner
	// built before this field existed and the posture of every Docker-free wire proof.
	//
	// It takes the report and returns the document, so the two can never be wired to different endpoints.
	Settings func(ctx context.Context, current Identity, report Settings) (Settings, error)
	// Update moves this machine to a named version and reports whether anything changed. Nil = this
	// build has no updater wired, and PALAI_AGENT_TARGET_VERSION then reports NOT READ.
	//
	// IT IS A SEAM AND NOT A DIRECT CALL because it downloads, verifies and replaces binaries — the one
	// operation in this package that must be drivable in a test without a network or the right to write
	// into /usr/local.
	Update func(ctx context.Context, targetVersion string) (bool, error)
	// ExitForUpdate ends the process after a successful update, so the service manager starts the new
	// binary. Nil is a no-op, which is what a test wants and what a build with no updater has anyway.
	ExitForUpdate func()
	// SettingsInterval is how long the runner waits between polls, and it is therefore the WORST-CASE
	// LATENCY from an operator pressing save to this machine acting on it. Zero uses
	// defaultSettingsInterval.
	//
	// It is a configuration-freshness knob and NOT a security parameter, which is the reason it is its own
	// field rather than derived from the renewal cadence: renewal fires at 80% of certificate lifetime, so
	// tying the two would mean an operator who wanted faster configuration had to shorten certificate
	// lifetimes to get it.
	SettingsInterval time.Duration
}

// defaultSettingsInterval is the poll period when SettingsInterval is unset: fast enough that an operator
// pressing save in the panel sees the machine act within a coffee-sip, slow enough that a thousand-machine
// fleet is ~33 requests a second against an endpoint that does two indexed reads and one keyed update.
const defaultSettingsInterval = 30 * time.Second

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
	//
	// N IS NO LONGER FIXED FOR THE PROCESS'S LIFETIME. It used to be read once here, which meant a machine
	// could only learn a new concurrency by being restarted — so the one genuinely per-machine setting this
	// product has was also the one an operator could not change from the panel without walking to the box.
	// The pool below grows and shrinks on the settings poll's instruction instead.
	leases := &leasePool{}
	leases.resize(ctx, &wg, cfg, state, logf, backoff, cfg.Concurrency)

	if cfg.Settings != nil {
		wg.Go(func() { cfg.settingsLoop(ctx, state, leases, &wg, logf, backoff) })
	}
	wg.Wait()
}

// leasePool is the LIVE size of the park-loop set: how many leases this machine wants to hold at once, and
// how many loops are actually running.
//
// GROWING IS IMMEDIATE AND SHRINKING IS COOPERATIVE, and the asymmetry is deliberate rather than a
// simplification. A new loop can be started at any moment because it starts by parking, which is idle. A
// loop cannot be stopped at any moment because it may be SUPERVISING AN ENGINE — killing it there would
// abandon somebody's run to make a configuration number true faster. So a shrink marks the intent and each
// loop retires itself at the only safe point it has: after finishing a lease, before parking for the next.
//
// The observable consequence is worth stating because an operator will see it: lowering concurrency on a
// busy machine takes effect as its in-flight leases finish, not at once. Raising it takes effect
// immediately. Both are reported as `applied` — the machine HAS accepted the number and is acting on it;
// what remains is work it had already taken on, which is not a pending restart.
type leasePool struct {
	mu      sync.Mutex
	want    int
	running int
}

// resize sets the wanted loop count and starts any loops needed to reach it. It is safe to call from the
// settings loop while park loops are running.
func (p *leasePool) resize(ctx context.Context, wg *sync.WaitGroup, cfg ServeConfig, state *serveState,
	logf func(string, ...any), backoff time.Duration, want int,
) {
	if want < 1 {
		// Zero is not "park nothing", it is the unset default — the same clamp Serve applied before this
		// pool existed. A machine that genuinely should take no work is CORDONED, which is a fleet decision
		// with its own durable state, not a concurrency of zero that would look identical to a misconfigured
		// one.
		want = 1
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.want = want
	for p.running < p.want {
		p.running++
		wg.Go(func() { cfg.parkLoop(ctx, state, p, logf, backoff) })
	}
}

// current reports the wanted size, for the log line that tells an operator what the machine decided.
func (p *leasePool) current() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.want
}

// retire reports whether the calling loop should exit because the pool has been shrunk below the number of
// loops running. It decrements the running count as it says yes, so exactly as many loops retire as were
// asked to.
func (p *leasePool) retire() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running > p.want {
		p.running--
		return true
	}
	return false
}

// serveState is the identity the lease loops and the renewer share, plus the say-it-once gates
// for the operator-facing notices. A 1/s retry loop that repeats the same sentence forever buries
// the one thing the operator needs, so each notice is printed once for the runner's lifetime.
type serveState struct {
	mu       sync.Mutex
	identity Identity
	stale    sync.Once // a dial rejected the client certificate
	settings sync.Once // the settings poll failed
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
func (cfg ServeConfig) parkLoop(ctx context.Context, state *serveState, leases *leasePool, logf func(string, ...any), backoff time.Duration) {
	for {
		if ctx.Err() != nil {
			return
		}
		// THE ONLY SAFE RETIREMENT POINT, and it is here rather than anywhere else in the loop for one
		// reason: below this line the loop may be holding a lease and supervising somebody's engine. A
		// concurrency reduction that took effect there would abandon a run to make a number true sooner.
		if leases.retire() {
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
		serveLease(ctx, cfg.Supervisor, leaseSession, NewToolServer(cfg.Shell), cfg.WorkspaceRoot, cfg.AllowUnsafeBind, logf)
	}
}

// renewLoop keeps the runner's identity usable, on its own connection — never touching a parked
// or in-flight lease. It waits until each certificate's renewal point and rolls the certificate
// forward over the current identity; when that window was MISSED and the identity is already
// past NotAfter, it recovers with Reenroll instead, because renewal authenticates with the very
// certificate that expired and so cannot serve the one case that needs it most.
// settingsLoop keeps this machine's configuration current: every interval it reports what it did with the
// document it holds and receives the one it should hold now, applying the difference.
//
// IT REPORTS BEFORE IT APPLIES, ON THE NEXT POLL RATHER THAN THIS ONE, and that ordering is what makes the
// report honest. The verdicts sent up describe the document this machine has ALREADY acted on, so a panel
// showing "applied" is reading a machine's account of something it did, not its intention to do it. The
// first poll of a process's life therefore reports revision 0 and nothing applied, which is true: it has
// been sent nothing yet.
//
// A FAILED POLL CHANGES NOTHING. The machine keeps running what it has and asks again after the backoff —
// the same position every other loop in this file takes, and the only safe one: a control plane that
// becomes unreachable must not be able to reconfigure a fleet by silence.
func (cfg ServeConfig) settingsLoop(ctx context.Context, state *serveState, leases *leasePool, wg *sync.WaitGroup,
	logf func(string, ...any), backoff time.Duration,
) {
	interval := cfg.SettingsInterval
	if interval <= 0 {
		interval = defaultSettingsInterval
	}
	// What this machine will report next time: the revision it acted on and its verdict per setting. It
	// starts empty, which is the true statement that nothing has been applied yet.
	report := Settings{}
	for {
		if ctx.Err() != nil {
			return
		}
		document, err := cfg.Settings(ctx, state.current(), report)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// Said once per runner lifetime, like the other operator-facing notices in this file: a 30s loop
			// that repeats the same sentence forever buries the one thing an operator needs to read.
			state.settings.Do(func() {
				logf("settings poll: %v; the machine keeps the configuration it holds and will retry each interval", err)
			})
			if sleep(ctx, backoff) != nil {
				return
			}
			continue
		}
		if document.Revision != report.Revision {
			report = Settings{
				Revision: document.Revision,
				Settings: cfg.applySettings(ctx, leases, wg, state, logf, backoff, document.Settings),
			}
		}
		if sleep(ctx, interval) != nil {
			return
		}
	}
}

// applySettings acts on one document and returns this machine's verdict for every setting in it.
//
// THE SWITCH IS THE SINGLE SOURCE OF TRUTH FOR WHAT THIS BINARY CAN BE TOLD, and that is the whole design.
// A setting with a case here is one cmd/runner acts on; everything else reports VerdictNotRead. There is
// no second list to keep in step, so a panel can never show a machine "holding" a value this build has no
// code to read — which is the same defect as a form that saves and does nothing, one hop further from the
// operator and correspondingly harder to see.
//
// IT IS DELIBERATELY NOT DRIVEN BY THE CONTROL PLANE'S CATALOGUE. The catalogue says which settings are
// WRITABLE; this says which are READ by this binary at this version, and a fleet is heterogeneous — an
// older machine that cannot act on a newly-catalogued setting must say so about itself rather than inherit
// a claim from the server that is describing a different build.
func (cfg ServeConfig) applySettings(ctx context.Context, leases *leasePool, wg *sync.WaitGroup, state *serveState,
	logf func(string, ...any), backoff time.Duration, settings map[string]string,
) map[string]string {
	verdict := make(map[string]string, len(settings))
	for name, value := range settings {
		switch name {
		case "PALAI_RUNNER_CONCURRENCY":
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 {
				// A value this binary's own reader would not parse is REFUSED rather than coerced, and the
				// machine says so by name. The control plane's write path applies the same grammar before
				// storing, so reaching this arm means the two disagree — which is exactly the state an
				// operator needs to see rather than have silently rounded to a default.
				verdict[name] = RefusedVerdict("not a positive integer")
				continue
			}
			if was := leases.current(); n != was {
				// `was` is read BEFORE the resize. Reading it after would print the number just set as the
				// previous one, which is the kind of log line that makes an operator believe nothing changed.
				leases.resize(ctx, wg, cfg, state, logf, backoff, n)
				logf("settings: serving %d leases at once (was %d)", n, was)
			}
			verdict[name] = VerdictApplied
		case "PALAI_AGENT_TARGET_VERSION":
			// THE MACHINE MOVES ITSELF, and the plane only names where to. cfg.Update is nil on a build
			// with no updater wired and on every deployment that has not configured a release base, and
			// then this reports NOT READ rather than pretending: a panel that showed "applied" for a
			// machine that cannot update is the defect this verdict map exists to prevent.
			if cfg.Update == nil {
				verdict[name] = VerdictNotRead
				continue
			}
			changed, err := cfg.Update(ctx, value)
			switch {
			case err != nil:
				// The running binary is untouched — the updater proves a download before installing it —
				// so the machine keeps working and says why it did not move.
				verdict[name] = RefusedVerdict(err.Error())
			case changed:
				// The process ends here so the service manager starts the new binary. Reported first: a
				// verdict written after the exit is a verdict nobody reads.
				verdict[name] = VerdictApplied
				logf("settings: updated to %s — exiting so the service manager starts it", value)
				if cfg.ExitForUpdate != nil {
					defer cfg.ExitForUpdate()
				}
			default:
				verdict[name] = VerdictApplied
			}
		default:
			verdict[name] = VerdictNotRead
		}
	}
	return verdict
}

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
		// ‼️ PERSISTED BEFORE IT IS ANNOUNCED, and the ordering is the whole value of the hook. A machine
		// that renewed and then restarted before the new certificate reached disk would come back holding
		// the OLD one, and if the restart outlasted its expiry it would take the recovery path — spending a
		// pool key for a certificate it had already been issued. The persist is best-effort by design: a
		// disk that cannot be written must not stop a runner that is currently serving leases, and the
		// machine still has a valid certificate in memory. It is logged so the failure is not silent.
		if cfg.PersistIdentity != nil {
			if err := cfg.PersistIdentity(renewed); err != nil {
				logf("could not persist the renewed identity, so a restart will fall back to the previous one: %v", err)
			}
		}
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
func serveLease(ctx context.Context, supervisor *StreamSupervisor, leaseSession *LeaseSession, tools *ToolServer, allocationRoot string, allowUnsafeBind bool, logf func(string, ...any)) {
	defer leaseSession.Close()
	lease := leaseSession.Lease()
	logf("received lease %s for run %s (fence %d)", lease.LeaseID, lease.RunID, lease.Fence)

	// THE MACHINE OPENS THE ALLOCATION (A.3 T5). It used to have to already exist, because the control
	// plane created it — on the control plane's own disk, which is the same disk only when the two share
	// a filesystem. Now the machine lays out the §29.9 directory for any allocation the offer names
	// under its managed root, and every refusal that governed the MOUNT still governs the creation:
	// resolveNewAllocationDir walks the root down to the leaf and refuses any component that is not a
	// real directory, so a symlink cannot make this write outside the root.
	//
	// It happens HERE, before the relay and before the engine, because both need it: the supervisor
	// bind-mounts this path into the engine, and a ws.request can arrive the instant the relay starts.
	if dir, err := openLeaseWorkspace(lease, allocationRoot, allowUnsafeBind); err != nil {
		logf("reject lease %s: %v", lease.LeaseID, err)
		if cerr := leaseSession.Complete(ctx, "failed", err.Error(), ""); cerr != nil {
			logf("report rejected lease completion for run %s: %v", lease.RunID, cerr)
		}
		return
	} else {
		// The MACHINE's spelling of the path — symlink-resolved against its own root, so on macOS a
		// /var allocation becomes /private/var. The mount and every later under-root check use it.
		lease.WorkspaceHostPath = dir
	}

	// A normal allocation must sit under the runner's managed root before it is bind-mounted; an
	// unsafe local bind requires the runner's own opt-in (spec §30.13, §24 boundary). Reject, don't mount.
	//
	// IT RUNS AFTER THE CREATION ABOVE AND IS NOT REDUNDANT WITH IT, and the difference is the kind of
	// comparison: this one symlink-RESOLVES both sides and compares real paths, which is only possible
	// once the directory exists; the creation walks the path lexically and refuses non-directory
	// components. Two independent tests of the same property is what this tree means by defence in
	// depth, and it has already recorded the case where a perturbation stayed green because the second
	// guard held.
	if err := admitWorkspaceMount(lease, allocationRoot, allowUnsafeBind); err != nil {
		logf("reject lease %s: %v", lease.LeaseID, err)
		if cerr := leaseSession.Complete(ctx, "failed", err.Error(), ""); cerr != nil {
			logf("report rejected lease completion for run %s: %v", lease.RunID, cerr)
		}
		return
	}

	leaseCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Controller frames relayed over the lease feed the supervisor's stdin injection; the
	// supervisor's forwarded engine frames are relayed back to the controller by the sink. Exec
	// requests on the same connection are not relayed at all — they run on THIS machine's executor
	// (toolserver.go), which is the whole of what a lease can now be asked to do beyond an engine.
	inbound := make(chan contracts.EngineFrame)
	go RelayInboundServing(leaseCtx, leaseSession, MachineServers{
		Tools: tools,
		// THE DISK BEHIND ws.* (A.3 T5), bound to the SAME managed root admitWorkspaceMount just
		// checked this lease's path against. One value, one meaning: a machine that will not mount a
		// path outside its root will not read or write one either.
		Workspace: NewWorkspaceServer(allocationRoot),
	}, inbound, logf)

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

	// THE REASON GOES WITH THE OUTCOME, not only into this machine's log. `streamErr` is what the two log
	// lines below have always printed; until 2026-08-04 the control plane got the outcome CLASS and nothing
	// else, so a stale engine-image pin reached the operator as "runner reported lease outcome \"failed\""
	// with advice to check their repository binding. errorReason renders nil as "" — a succeeded lease
	// carries no reason.
	if err := leaseSession.Complete(ctx, OutcomeClass(streamErr), errorReason(streamErr), stderrDigest(result.Stderr)); err != nil {
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
		// ‼️ THE REFUSAL NAMES BOTH ROOTS, because the two are configured in different places and the
		// message without them is unactionable. The control plane allocates under its own
		// PALAI_WORKSPACE_ROOT and sends an ABSOLUTE path; an installed agent uses its own
		// device.Paths.WorkspaceRoot and reads no environment at all (that is deliberate — the machine
		// owns its disk). So on any deployment where the plane and the machine are different hosts the
		// two roots differ BY CONSTRUCTION, and every run fails here.
		//
		// HONEST CEILING, and it is a wire-contract one rather than a bug in this function: the lease
		// offer carries WorkspaceHostPath and no allocation IDENTITY, so this machine cannot derive
		// `<its own root>/<allocation id>` and check that instead. Until the offer carries the id, a
		// remote Mac needs the plane's root and the device's root to name the same directory.
		return fmt.Errorf("workspace path %q is outside this runner's allocation root %q: the control plane "+
			"allocated under ITS workspace root and this machine manages a different one, so the two must "+
			"name the same directory until the lease offer carries the allocation id", path, root)
	}
	return nil
}

// resolveNewAllocationDir answers where an allocation that DOES NOT EXIST YET may be created, and it
// is the one place in this package that has to reason about a path it cannot symlink-resolve (A.3
// T5). workspaceUnderRoot resolves both sides and is therefore useless before the directory is there;
// this walks down instead, creating each missing component and REFUSING any existing component that
// is not a real directory.
//
// THE LSTAT IS THE CHECK AND MkdirAll WOULD NOT HAVE IT. MkdirAll follows an existing symlink
// component without complaint, so a `<root>/a` pointing at /etc would make `<root>/a/b` a directory
// in /etc — created by a path the control plane named, which is precisely the trust boundary §30.13
// draws. Every component is therefore Lstat'ed: a symlink, a regular file, a device, anything that is
// not a directory, ends the walk.
//
// The root itself is resolved first and the result is built from the RESOLVED root, so a runner whose
// allocation root is legitimately behind a symlink — /var on macOS is exactly this — still works, and
// the path returned is the one every later workspaceUnderRoot will compare against.
// openLeaseWorkspace decides where THIS lease's workspace is on THIS machine, creating it when the
// offer names an allocation that is not there yet. An empty path is a workspace-less lease and stays
// empty.
//
// AN UNSAFE LOCAL BIND IS NOT CREATED, and that asymmetry is the whole of what the flag means: a
// §30.13 direct host bind (REP-012) names a directory the OPERATOR already has and deliberately
// placed outside the managed root, so there is nothing to mint and nothing to place. Creating it
// would turn a typo into a new empty directory somewhere on the operator's disk and mount it as if it
// were their project.
func openLeaseWorkspace(lease Lease, allocationRoot string, allowUnsafeBind bool) (string, error) {
	if lease.WorkspaceHostPath == "" {
		return "", nil
	}
	if lease.WorkspaceUnsafe {
		if !allowUnsafeBind {
			return "", errors.New("unsafe local bind requested but runner has not opted in (PALAI_WORKSPACE_UNSAFE_BIND=1)")
		}
		return lease.WorkspaceHostPath, nil
	}
	dir, err := resolveNewAllocationDir(lease.WorkspaceHostPath, allocationRoot)
	if err != nil {
		return "", err
	}
	if err := workspace.Prepare(dir); err != nil {
		return "", fmt.Errorf("lay out allocation: %w", err)
	}
	return dir, nil
}

func resolveNewAllocationDir(path, root string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("workspace open names no allocation")
	}
	if strings.TrimSpace(root) == "" {
		return "", errors.New("this runner has no managed allocation root, so it can open no allocation")
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve runner allocation root: %w", err)
	}
	// The request's path is compared LEXICALLY against the resolved root, which is sound only because
	// every component below is then verified to be a real directory: a `..` that climbs out is caught
	// here, and a symlink that would climb out is caught by the walk.
	abs := filepath.Clean(path)
	if !filepath.IsAbs(abs) {
		return "", errors.New("workspace allocation path must be absolute")
	}
	rel, err := filepath.Rel(realRoot, abs)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("workspace path is outside the runner allocation root")
	}
	current := realRoot
	for _, component := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		switch {
		case errors.Is(err, os.ErrNotExist):
			// 0o755 mirrors workspace.Prepare's mode. The sentence that used to finish this comment —
			// "the allocation is handed to the run's own uid by the session-account layer" — named a
			// hand-over nothing performed; docs/measurements/faz-a5-residue.md §2 measured every tenant
			// running as uid 501. It now happens in exactly one place and this is not it:
			// execution.HandTreeTo, called after the clone and after a restore, on the control plane.
			// When no session-account layer is wired, nothing is handed to anybody and this directory
			// stays this process's — which is the declared unsandboxed-host posture, not a boundary.
			if err := os.Mkdir(current, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
				return "", fmt.Errorf("create allocation directory: %w", err)
			}
		case err != nil:
			return "", fmt.Errorf("inspect allocation directory: %w", err)
		case !info.IsDir():
			// A symlink lands here too — Lstat does not follow it, so its mode is ModeSymlink and never
			// ModeDir. That is the case this loop exists for.
			return "", fmt.Errorf("allocation path component %s is not a directory", component)
		}
	}
	return current, nil
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

// errorReason is the sentence that goes on the wire beside the outcome class. A nil error renders empty
// rather than "<nil>": a succeeded lease has no reason, and a reader must be able to tell "it worked"
// from "it failed and said nothing".
func errorReason(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
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
