package execution

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/fleet"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/packages/runner"
	"github.com/palgroup/palai/packages/version"
)

// ErrRunnerCordoned is returned by Dial when the gateway is cordoned: no NEW lease is offered so an
// attempt requeues rather than dispatching onto a runner that is draining for an upgrade (§48.4). An
// in-flight lease is untouched — cordon stops new work, drain waits for the current work to finish.
var ErrRunnerCordoned = errors.New("runner gateway cordoned: no new leases")

// ErrRunnerRevoked is returned by Dial (and closes an incoming connect) when the gateway is revoked: a
// decommissioned/compromised runner's new leases AND stale session frames are refused (SAN-011). Revoke
// is the hard stop cordon is not — a cordoned runner still completes its lease, a revoked one does not.
var ErrRunnerRevoked = errors.New("runner gateway revoked: leases and session frames refused")

// ErrPoolHasNoRunner is returned by Dial when the attempt's pool held NO machine of the attempt's
// tenant for the whole dial budget. It is the difference between "everything is busy" and "there is
// nothing here", and that difference is why it exists as its own error: the orchestrator PARKS the run
// on it (§3.6 D12) instead of failing the attempt, so a Mac that takes six to twenty minutes to boot
// still finds its run waiting when it arrives. A pool whose machines are merely all leased returns the
// context error exactly as before and rides the existing retry ladder.
var ErrPoolHasNoRunner = errors.New("runner pool has no machine for this tenant")

// CertIssuer signs an enrolling runner's public key into a short-lived client
// certificate with the local control-plane CA. The CA the gateway binds is injected
// (an in-test CA in the conformance proof); binding it to the .palai layout is Task 12.
type CertIssuer interface {
	SignRunnerCertificate(publicKeyDER []byte, runnerDNS string) (certificateDER []byte, err error)
}

// EnrollmentTokens redeems the bootstrap credential a machine presents to enrol. Consume returns an
// error for a credential this control plane does not recognise, or recognises and refuses.
//
// IT IS NOT ONE-USE, AND THIS COMMENT USED TO SAY IT WAS (§3.6 D4, corrected in E24 T3). The claim was
// written when the only implementation spent a token permanently, and it outlived that by a long way:
// the shipped FileEnrollmentTokens re-reads its file on every call and admits a redemption once per
// issued-certificate lifetime, deliberately, because renewal runs over the certificate that is expiring
// and a machine that missed its window would otherwise have NO way back — see the threat model in
// local_credentials.go, which explains at length why one-use was replaced. Presenting a credential
// twice is therefore an ordinary event on this path; what a refusal means is "not live", not
// "already used".
type EnrollmentTokens interface {
	Consume(token string) error
}

// PoolEnrollment is the pool-scoped enrolment credential (E24 T3): a credential that names the ONE pool
// it admits into, can expire, can be revoked, and is RECORDED against the certificate it minted.
//
// It EMBEDS EnrollmentTokens rather than replacing it, and that is the substitutability requirement
// enforced by the compiler instead of promised in prose: a pool-key implementation has to be usable
// anywhere the file token is. The gateway nonetheless calls RedeemPoolKey and not Consume, because the
// pool binding is the entire point and Consume discards it.
type PoolEnrollment interface {
	EnrollmentTokens
	// RedeemPoolKey resolves a presented value into the grant it authorises. runnerID is the
	// server-minted id a refusal is journalled against; declaredPool is what the MACHINE said it
	// belongs to and never widens the grant. fleet.ErrUnknownPoolKey — and only that error — means "not
	// mine", so the gateway may fall through to the file token; every other error is a recognised
	// credential that was refused.
	RedeemPoolKey(ctx context.Context, presented, runnerID, declaredPool string) (fleet.PoolGrant, error)
}

// RunnerGateway is the control-plane counterpart of the runner's outbound-only model:
// it serves the enrollment endpoint and the mutually-authenticated session endpoint the
// runner dials out to, and it is the production EngineDialer — Dial offers a connected
// runner the waiting attempt's lease and bridges its session frames as an EngineChannel.
// The orchestrator is written once against that seam and never learns it drives a runner
// over a WebSocket rather than a subprocess.
type RunnerGateway struct {
	issuer CertIssuer
	tokens EnrollmentTokens
	// poolKeys is the FIRST link of the credential chain (E24 T3): a pool key names the pool it admits
	// into, so a machine presenting one lands in that pool and is recorded against that key. Nil leaves
	// the chain exactly as it was — file token only, default pool — which is every deployment built
	// before this task and is what makes `SetPoolKeys` a setter rather than a constructor parameter.
	poolKeys PoolEnrollment
	// poolSettings answers an enrolling machine its pool's desired configuration. Nil sends none, which
	// leaves the runner on the configuration it was started with.
	poolSettings PoolSettings
	now          func() time.Time
	// pools is the rendezvous, one queue per pool (E24 T2). It replaced a SINGLE unbuffered channel
	// every parked runner sent itself down and every Dial received from, which made a Mac pool and a
	// container pool two names for one queue: any parked machine satisfied any attempt.
	//
	// A map guarded by an RWMutex rather than a fixed set, because pools are rows an operator creates
	// and the gateway learns of one when a machine parks in it or an attempt asks for it.
	// ponytail: the map grows by pool and is never pruned — a pool id is a durable row, not caller
	// input, so there is nothing here for a stranger to grow. Prune on pool deletion if that lands.
	poolsMu sync.RWMutex
	pools   map[string]*poolQueue
	// connected counts runner sessions currently held open on this gateway (handshaked and either
	// parked for a lease or serving one). An alive runner keeps its Concurrency park-loops dialed in,
	// so this is >0 while a runner is up and drops to 0 when it stops — the real signal behind the
	// palai_runner_sessions gauge and the runner-down alert (E14 Task 6).
	connected atomic.Int64
	// machines is the PER-RUNNER lifecycle: one cordon flag, one revoke flag and one in-flight-lease
	// counter per enrolled machine, keyed by the SERVER-minted runner id (E24 T5). It replaced the single
	// `active atomic.Int64` the whole-gateway drain used to wait on, and the whole-gateway drain now waits
	// on the SUM — a control-plane swap still drains everything (§48.4), it just knows whose leases those
	// were. See runnerLifecycle for why a cordon takes a machine OUT of the rendezvous rather than being
	// compared against inside Dial.
	machinesMu sync.RWMutex
	machines   map[string]*runnerLifecycle
	// sessions is every live runner session on this gateway, which is what the heartbeat pings and what
	// the reaper cuts. A machine may hold several (PALAI_RUNNER_CONCURRENCY parks one loop per concurrent
	// lease), so this is a set of connections and not a map keyed by runner id.
	sessions map[*pendingRunner]struct{}
	// cpVersion is this control-plane's version stamp, checked against the runner's advertised version in
	// the connect handshake for the §48.2 support window (OPS-008). Defaulted to version.Resolve; a test
	// or a deploy override sets it with SetControlPlaneVersion.
	cpVersion string
	// cordoned stops NEW leases (Dial refuses) while an in-flight lease finishes — the upgrade-drain
	// signal. revoked is the hard stop: new connects rejected AND session frames dropped (SAN-011).
	//
	// THESE TWO ARE WHOLE-GATEWAY AND THEY STAY, which is E24 T5's instruction and the right answer: a
	// control-plane swap must drain EVERYTHING, so `Drain` (SIGTERM) still cordons the gateway itself. What
	// E24 T5 added is the per-machine layer above (`machines`), because "take that Mac out of service"
	// asked these bools a question they could not answer — the upgrade path this comment used to name.
	cordoned atomic.Bool
	revoked  atomic.Bool
	// identity is the client certificate the gateway last saw a runner present, on connect or on
	// renew. It is the only place the runner's certificate lifetime is observable from OUTSIDE the
	// runner process, and it is what `palai local doctor` reads: a healthy runner refreshes this
	// every renewal, so a NotAfter in the past means the runner stopped rolling its identity
	// forward — the expired-identity condition, named where the operator meets it. Nil until the
	// first runner presents a certificate.
	identity atomic.Pointer[RunnerIdentity]
	// registry is the durable multi-runner inventory (E24 T1). `identity` above stays exactly what it
	// was — the LAST certificate seen, which is what /healthz/runner and `palai local doctor` read —
	// and the registry is the thing that can answer for MORE THAN ONE machine. Nil leaves the gateway
	// behaving as it did: a stack that wires no database still enrols runners and serves leases, which
	// is what the Docker-free conformance tier and every wire proof depend on.
	registry fleet.Registry
	// newID mints the SERVER-side runner id. Injected so a proof can make an id deterministic;
	// production passes middleware.NewID.
	newID func(prefix string) string
	// wake re-enters a run that parked for want of a machine when one joins its pool (E24 T4). Nil
	// leaves handleConnect exactly as it was: a gateway with no durable spine — the conformance tier and
	// every wire proof — parks machines and serves leases and wakes nothing, because in that posture
	// there are no durable runs to wake.
	wake CapacityWaker
}

// CapacityWaker re-enters the oldest run parked on a pool for want of a machine. *coordinator.Store is
// the only implementation; the interface exists so the gateway does not depend on the database, exactly
// as it does not depend on it for the registry.
type CapacityWaker interface {
	// WakeRunAwaitingCapacity drives that run waiting->running and enqueues its response.run job in ONE
	// transaction, and reports the run it woke (empty when there was none). A run parked for any OTHER
	// reason — a human's pause, an approval, a detached child — is NOT a candidate: those have their own
	// wakers and waking them here would override a decision somebody else owns.
	WakeRunAwaitingCapacity(ctx context.Context, tenant coordinator.Tenant, poolID string) (string, error)
}

// RunnerIdentity is the client certificate a runner last presented to the gateway: who it
// claimed to be, when that certificate stops being valid, and when the gateway saw it.
type RunnerIdentity struct {
	RunnerDNS string    `json:"runner_dns"`
	NotAfter  time.Time `json:"not_after"`
	SeenAt    time.Time `json:"seen_at"`
}

// LastRunnerIdentity reports the client certificate the gateway last saw, or false when no
// runner has presented one yet.
func (g *RunnerGateway) LastRunnerIdentity() (RunnerIdentity, bool) {
	seen := g.identity.Load()
	if seen == nil {
		return RunnerIdentity{}, false
	}
	return *seen, true
}

// recordIdentity remembers the certificate the runner now HOLDS, so the record advances on every
// renewal and goes stale exactly when the runner stops renewing. Connect passes the presented
// leaf; enroll and renew pass the certificate they just ISSUED, which is the one the runner will
// hold from here — recording the presented (outgoing) certificate at renew would leave the record
// one generation behind, reading "expired" for the tail of every renewal cycle on a runner that is
// perfectly healthy.
func (g *RunnerGateway) recordIdentity(leaf *x509.Certificate) {
	g.identity.Store(&RunnerIdentity{RunnerDNS: renewDNS(leaf), NotAfter: leaf.NotAfter, SeenAt: g.now()})
}

// recordIssuedIdentity records a certificate the gateway just signed, from its DER. A DER the CA
// produced a moment ago that will not parse is not worth failing the request over — the runner has
// its certificate either way — so a parse failure only leaves the record where it was.
func (g *RunnerGateway) recordIssuedIdentity(certDER []byte) {
	if leaf, err := x509.ParseCertificate(certDER); err == nil {
		g.recordIdentity(leaf)
	}
}

// NewRunnerGateway binds the CA issuer and the one-use token store into a gateway.
func NewRunnerGateway(issuer CertIssuer, tokens EnrollmentTokens) *RunnerGateway {
	return &RunnerGateway{
		issuer:    issuer,
		tokens:    tokens,
		now:       time.Now,
		pools:     map[string]*poolQueue{},
		machines:  map[string]*runnerLifecycle{},
		sessions:  map[*pendingRunner]struct{}{},
		cpVersion: version.Resolve(),
		newID:     middleware.NewID,
	}
}

// SetRegistry wires the durable runner inventory (E24 T1). Unset, the gateway records nothing and
// behaves exactly as it did before the registry existed — see the field comment for why that is a
// supported posture rather than a fallback. A setter rather than a constructor parameter for the
// reason SetControlPlaneVersion is one: every existing caller compiles unchanged.
func (g *RunnerGateway) SetRegistry(r fleet.Registry) { g.registry = r }

// SetPoolKeys wires the pool enrolment keys (E24 T3) ahead of the file bootstrap token in the
// credential chain. Unset, the gateway admits only the file token and every machine lands in the
// default pool — the pre-E24 posture, unchanged to the byte.
func (g *RunnerGateway) SetPoolKeys(k PoolEnrollment) { g.poolKeys = k }

// PoolSettings answers one pool's desired configuration — what the admin plane decided this machine
// should be, which is the half a machine cannot know about itself.
//
// The poolID it is asked about comes from the RESOLVED GRANT and never from the enrolment request, so a
// machine cannot read another pool's document by declaring that pool.
// THREE METHODS, TWO MOMENTS. DesiredSettingsForPool serves ENROLMENT, where the machine has no identity
// yet and the pool comes from the resolved grant. The other two serve the SETTINGS POLL, where the machine
// has a certificate and is therefore addressable individually — so the poll can overlay a machine document
// on the pool's, and can record what the machine says it did with the result.
type PoolSettings interface {
	DesiredSettingsForPool(ctx context.Context, poolID string) (map[string]string, error)
	// DesiredSettingsForMachine is the pool's document with this machine's own laid over it, plus the
	// revision that produced the pair — a CHANGE DETECTOR the machine compares against what it is running.
	DesiredSettingsForMachine(ctx context.Context, poolID, runnerID string) (map[string]string, int64, error)
	// RecordRunnerConfigReport stores the machine's verdict per setting. matched=false with a nil error is a
	// real state rather than a fault — a machine that enrolled before the registry existed has no row and
	// never will — so it is a return value rather than an error the caller has to pick apart.
	RecordRunnerConfigReport(ctx context.Context, dns string, revision int64, applied map[string]string, at time.Time) (matched bool, err error)
}

// SetPoolSettings wires the desired-configuration read into enrolment. Unset, every enrolment answers with
// no settings and every runner falls back to the configuration it was started with — the posture of every
// deployment built before the runner plane had a reader.
func (g *RunnerGateway) SetPoolSettings(s PoolSettings) { g.poolSettings = s }

// SetCapacityWaker wires the durable wake a machine's arrival performs (E24 T4). Unset, handleConnect
// wakes nothing — the posture every Docker-free wire proof runs in, where there are no durable runs.
func (g *RunnerGateway) SetCapacityWaker(w CapacityWaker) { g.wake = w }

// SetControlPlaneVersion overrides the control-plane version stamp the connect handshake checks the
// runner's advertised version against (§48.2 window). Defaulted to version.Resolve; a test injects a
// concrete version to exercise the OPS-008 skew rejection deterministically.
func (g *RunnerGateway) SetControlPlaneVersion(v string) { g.cpVersion = v }

// Connected reports the number of runner sessions currently held open on the gateway — the value
// behind the palai_runner_sessions gauge. Safe to call from the metrics scrape goroutine.
func (g *RunnerGateway) Connected() int64 { return g.connected.Load() }

// Waiting reports how many attempts are currently queued for a pool with no machine free to take
// them, across every tenant that has a machine or an attempt in it. It is the number behind the one
// question an operator of a fleet actually asks — "why is nothing running in my Mac pool" — which
// before E24 had no answer at all: a blocked Dial was a goroutine parked on a channel and nothing
// counted it.
func (g *RunnerGateway) Waiting(poolID string) int {
	suffix := "\x00" + poolKey(poolID)
	total := 0
	g.poolsMu.RLock()
	for key, q := range g.pools {
		if strings.HasSuffix(key, suffix) {
			total += q.depth()
		}
	}
	g.poolsMu.RUnlock()
	return total
}

// queueKey is the rendezvous identity: a TENANT and a pool, never a pool alone.
//
// THE TENANT IS IN THE KEY BECAUSE THAT IS WHAT MAKES A CROSS-TENANT OFFER UNREACHABLE RATHER THAN
// REFUSED (§3.6 D8). T2 put the pool here for the same reason and wrote down why: a machine parked in
// one queue cannot be handed to a waiter on another, so there is no code path left that could later
// relax the rule into a preference. A comparison inside Dial would have been one `if` away from being
// softened into "close enough" by somebody debugging an idle Mac at 3am.
//
// It is also not merely the pool's tenant restated. A pool id is a durable row and DOES imply a
// tenant — but `fleet.ResolvePool` falls back to a CONSTANT default pool id, so a tenant that has
// configured nothing resolves to a string that belongs to the bootstrap tenant. Two tenants therefore
// collide on one pool id by default, and the pool alone cannot tell them apart.
//
// An empty tenant is the pre-E24 attempt and the machine no registry knows: they meet each other and
// nothing else, which is §2's bit-unchanged rule and not a wildcard.
func queueKey(tenant coordinator.Tenant, poolID string) string {
	return tenant.Organization + "\x00" + tenant.Project + "\x00" + poolKey(poolID)
}

// queueFor resolves a (tenant, pool) rendezvous, creating it on first use. Double-checked under the
// write lock so two concurrent first-uses cannot end up with two queues for one key — which would be
// exactly the bug T2 removed, reintroduced by a map.
func (g *RunnerGateway) queueFor(tenant coordinator.Tenant, poolID string) *poolQueue {
	key := queueKey(tenant, poolID)
	g.poolsMu.RLock()
	q, ok := g.pools[key]
	g.poolsMu.RUnlock()
	if ok {
		return q
	}
	g.poolsMu.Lock()
	defer g.poolsMu.Unlock()
	if q, ok = g.pools[key]; ok {
		return q
	}
	q = &poolQueue{}
	g.pools[key] = q
	return q
}

// poolKey normalises an unset pool to the default one. It is the single place "no pool configured"
// becomes "the default pool", which is the whole of §2's bit-unchanged rule for a deployment that
// has never heard of a pool.
func poolKey(poolID string) string {
	if poolID == "" {
		return fleet.DefaultPoolID
	}
	return poolID
}

// Cordon stops the gateway offering NEW leases: Dial returns ErrRunnerCordoned so a waiting attempt
// requeues instead of dispatching onto a runner that is about to be replaced (§48.4 drain). An in-flight
// lease is untouched. Resume clears it. Idempotent.
func (g *RunnerGateway) Cordon() { g.cordoned.Store(true) }

// Resume clears a cordon so the gateway offers leases again — the rollback/abort counterpart to Cordon.
func (g *RunnerGateway) Resume() { g.cordoned.Store(false) }

// Revoke is the hard stop (SAN-011): new connects are rejected and session frames from any live runner
// connection are dropped (stale events refused), on top of the cordon's new-lease refusal. A revoked
// gateway never un-revokes in-process — a revoked runner identity is decommissioned, not paused.
func (g *RunnerGateway) Revoke() {
	g.cordoned.Store(true)
	g.revoked.Store(true)
}

// Cordoned reports whether new leases are currently refused (cordoned or revoked). Revoked reports the
// hard-stop state. Both back the drain/revoke drills and a doctor surface.
func (g *RunnerGateway) Cordoned() bool { return g.cordoned.Load() }
func (g *RunnerGateway) Revoked() bool  { return g.revoked.Load() }

// runnerLifecycle is ONE machine's cordon/revoke state and its in-flight lease count (E24 T5).
//
// A CORDONED MACHINE LEAVES THE RENDEZVOUS RATHER THAN BEING COMPARED AGAINST INSIDE Dial, and that is
// the same structural choice T2 made for the pool and T4 made for the tenant, for the same reason: a
// machine that is not in the queue is UNREACHABLE from a Dial, so there is no code path left that could
// later be softened into "close enough" by somebody debugging an idle Mac at 3am. It is also the cheaper
// one — a comparison inside the handover would need the queue to consult per-runner state under its own
// lock, and a resume would then need a way to re-run the match.
//
// `changed` is the broadcast: it is CLOSED and REPLACED on every transition, so any number of watchers
// (a machine parks one session per concurrent lease) wake on one write without a condition variable and
// without a subscriber list to leak.
type runnerLifecycle struct {
	mu sync.Mutex
	// pending is E24 T6's waiting room: the machine enrolled into a pool with `strict_enrollment` and no
	// human has admitted it yet. It is a THIRD flag rather than a reuse of `cordoned` because the two differ
	// in the one place it matters — a cordoned machine is a MEMBER of its pool (its pool has capacity, all of
	// it busy, so a run rides the retry ladder) while a pending machine is ABSENT capacity (the run parks and
	// waits for the human). Collapsing them would dead-letter every run placed in a pool whose machines are
	// all waiting for an approval, in ~2.5 minutes, while the approver is at lunch.
	pending  bool
	cordoned bool
	revoked  bool
	changed  chan struct{}
	// active counts THIS machine's in-flight leases. The whole-gateway Drain sums these; the read surface
	// publishes one machine's as `active_leases`, which is the question a cordon exists to let an operator
	// ask ("can I unplug it yet?").
	active atomic.Int64
}

// state reports the machine's current posture and the channel that closes on its next transition. Both
// are read together under one lock, so a watcher cannot miss a change between reading the flags and
// selecting on the channel.
func (l *runnerLifecycle) state() (cordoned, revoked bool, changed <-chan struct{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.changed == nil {
		l.changed = make(chan struct{})
	}
	return l.cordoned, l.revoked, l.changed
}

// set applies a transition and broadcasts it. A revoke is ONE-WAY: once revoked, neither flag comes
// back, which is today's in-memory semantics unchanged (a revoked runner identity is decommissioned, not
// paused) rather than a new rule. A no-op transition broadcasts nothing, so a repeated cordon does not
// churn every parked session of that machine.
//
// IT DOES NOT TOUCH `pending` (E24 T6). A cordon of a machine still in the waiting room would otherwise
// erase the fact that nobody admitted it — the durable side refuses that write for the same reason
// (SetRunnerState) — and a resume would then look legitimate.
func (l *runnerLifecycle) set(cordoned, revoked bool) {
	l.mu.Lock()
	if l.revoked || (l.cordoned == cordoned && l.revoked == revoked) {
		l.mu.Unlock()
		return
	}
	l.cordoned, l.revoked = cordoned, revoked
	l.broadcast()
	l.mu.Unlock()
}

// awaiting reports whether this machine is still in the waiting room, whether it has been revoked, and the
// channel that closes on its next transition (E24 T6). All three are read under ONE lock for the reason
// state() reads its two that way: a watcher that read the flag and then took the channel could miss the
// transition in between and wait for a broadcast that has already happened.
func (l *runnerLifecycle) awaiting() (pending, revoked bool, changed <-chan struct{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.changed == nil {
		l.changed = make(chan struct{})
	}
	return l.pending, l.revoked, l.changed
}

// setPending puts a machine into the waiting room or admits it out of one, and broadcasts the change.
//
// A REVOKED MACHINE IS NOT ADMITTED BY IT, which is set()'s one-way rule applied to the same field: an
// approval must not be a way to bring a decommissioned identity back, and the durable statement takes the
// same position (`state IN ('pending','active')`).
func (l *runnerLifecycle) setPending(pending bool) {
	l.mu.Lock()
	if l.revoked || l.pending == pending {
		l.mu.Unlock()
		return
	}
	l.pending = pending
	l.broadcast()
	l.mu.Unlock()
}

// broadcast closes the current transition channel and installs the next. Called with l.mu held.
func (l *runnerLifecycle) broadcast() {
	if l.changed != nil {
		close(l.changed)
	}
	l.changed = make(chan struct{})
}

// lifecycle resolves a machine's lifecycle record, creating it on first use. Double-checked under the
// write lock so two concurrent first-uses cannot end up with two records for one machine — which would
// be a cordon written to one and read from the other.
//
// ponytail: the map grows by enrolled machine and is never pruned. A runner id is server-minted and
// durable, so there is nothing here a stranger can grow; prune on runner deletion if that ever lands.
func (g *RunnerGateway) lifecycle(runnerID string) *runnerLifecycle {
	g.machinesMu.RLock()
	life, ok := g.machines[runnerID]
	g.machinesMu.RUnlock()
	if ok {
		return life
	}
	g.machinesMu.Lock()
	defer g.machinesMu.Unlock()
	if life, ok = g.machines[runnerID]; ok {
		return life
	}
	life = &runnerLifecycle{}
	g.machines[runnerID] = life
	return life
}

// CordonRunner stops offering NEW leases to ONE machine: it leaves its pool's rendezvous and stays
// connected, so an in-flight lease finishes and nothing new arrives. ResumeRunner puts it back. Both are
// idempotent, and both are safe for a machine that has never connected — the record is created and the
// machine adopts it when it does, which is what makes a cordon written while a Mac is rebooting stick.
func (g *RunnerGateway) CordonRunner(runnerID string) {
	g.lifecycle(runnerID).set(true, false)
	g.evict(runnerID)
}

// ResumeRunner clears a cordon. It does NOT un-revoke: see runnerLifecycle.set.
func (g *RunnerGateway) ResumeRunner(runnerID string) { g.lifecycle(runnerID).set(false, false) }

// ApproveRunner admits ONE machine out of a strict pool's waiting room (E24 T6): its held-open session
// leaves awaitApproval, joins its pool's rendezvous, and — because the join is followed by the same wake
// every connect performs — re-enters a run that parked on that pool for want of a machine.
//
// IT IS CALLED FOR EVERY *found* APPROVAL AND NOT ONLY FOR THE ONE THAT MOVED THE ROW, which is what makes
// the operator's retry the fix for one narrow race: the API writes the row and then tells this gateway, so
// an approval landing between a connecting machine's row read and its adoption of that row leaves the row
// admitted and the session still waiting. A second approve is a 200 (the statement is idempotent) and
// reaches the session. Safe for a machine that has never connected: the record is created and adopted at
// its next connect, the same as a cordon written while a Mac is rebooting.
func (g *RunnerGateway) ApproveRunner(runnerID string) { g.lifecycle(runnerID).setPending(false) }

// RevokeRunner is the hard stop for ONE machine (SAN-011, per-runner): its live sessions are CUT, its
// in-flight lease dies with them and is reclaimed by the existing E10 recovery layer, its session frames
// are refused, and it cannot reconnect. Irreversible in this process; the durable half is
// `runners.state = 'revoked'`, which is what makes it survive a restart.
func (g *RunnerGateway) RevokeRunner(runnerID string) {
	g.lifecycle(runnerID).set(true, true)
	g.evict(runnerID)
}

// evict takes every live session of ONE machine out of its pool's rendezvous, synchronously, so that a
// cordon which has RETURNED is a cordon no Dial can get past. Without it the flag alone is only
// eventually effective — the machine's own connect handler would wake and unpark itself, and a Dial
// arriving in that window would be handed a machine an operator has just taken out of service.
//
// It is not a substitute for the connect handler's own reaction: that is what keeps the session
// CONNECTED and re-parks it on resume. This is only the removal, and unpark is idempotent, so the two
// racing each other is a no-op rather than a bug.
//
// THE ONE CASE IT CANNOT WIN, named where a reader meets it: a Dial that has ALREADY popped this machine
// off the queue holds it, and unpark reports so. Dial's own post-receive re-check refuses that lease and
// hands the connection back, which costs the machine its session (it re-dials at once) — the honest trade
// against offering a lease to a cordoned machine.
func (g *RunnerGateway) evict(runnerID string) {
	g.machinesMu.RLock()
	evicting := make([]*pendingRunner, 0, len(g.sessions))
	for pr := range g.sessions {
		if pr.runnerID == runnerID {
			evicting = append(evicting, pr)
		}
	}
	g.machinesMu.RUnlock()
	for _, pr := range evicting {
		if pr.queue != nil {
			pr.queue.unpark(pr)
		}
	}
}

// RunnerActiveLeases reports how many leases ONE machine is currently serving — the answer to the
// question a cordon exists to let an operator ask, which is "can I take this Mac away yet?". It is the
// per-runner half of the counter Drain sums.
func (g *RunnerGateway) RunnerActiveLeases(runnerID string) int64 {
	g.machinesMu.RLock()
	life, ok := g.machines[runnerID]
	g.machinesMu.RUnlock()
	if !ok {
		return 0
	}
	return life.active.Load()
}

// activeLeases is every machine's in-flight leases summed — what the WHOLE-GATEWAY drain waits on. It is
// a sum rather than one counter because the counter became per-machine; a drain that read one machine's
// would return nil while another was mid-lease, which is a control plane exiting during a run.
func (g *RunnerGateway) activeLeases() int64 {
	total := int64(0)
	g.machinesMu.RLock()
	for _, life := range g.machines {
		total += life.active.Load()
	}
	g.machinesMu.RUnlock()
	return total
}

// Drain cordons the gateway and blocks until every in-flight lease has quiesced (active == 0) or ctx is
// done. It stops new leases and waits for the in-flight lease to finish; if it cannot finish within ctx,
// the caller (a control-plane shutting down for a swap) exits anyway and the interrupted run is reclaimed
// and completed by the EXISTING E10 recovery layer (coordinator reconcile + WorkspaceRecovery, §26.3) —
// drain REUSES that layer, it does not re-implement run migration here. Returns nil on quiesce, ctx.Err()
// on timeout.
// THE BODY IS E15 T2'S, UNCHANGED, AND ONLY THE COUNTER MOVED (E24 T5): `g.active.Load()` became the SUM
// of the per-machine counters. The cordon is still whole-gateway, the tick is still 25ms, the handover to
// the E10 recovery layer is still by exiting anyway, and the return is still nil-on-quiesce /
// ctx.Err()-on-timeout. A per-runner drain is deliberately NOT a surface: nothing in production would
// call one, and this task's own subject is surfaces with no caller.
func (g *RunnerGateway) Drain(ctx context.Context) error {
	g.Cordon()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if g.activeLeases() == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// pendingRunner is a runner that completed the handshake and is parked waiting for a
// lease. The connect handler holds the HTTP goroutine open on release so the hijacked
// WebSocket stays alive for the whole lease; the EngineChannel closes release when the
// attempt ends, letting the handler return and tear the connection down.
//
// A single readLoop goroutine owns the connection's read side for its whole life. gc is set by Dial
// (before it writes the lease offer) so that readLoop, which is the sole reader, relays the runner's
// engine frames once a lease is assigned and otherwise just detects a park-time disconnect. That
// disconnect detection is what keeps palai_runner_sessions honest: a runner that dies while parked-
// and-idle (nothing else reads the connection then) is noticed at once, not only at the next Dial.
type pendingRunner struct {
	conn *websocket.Conn
	// runnerID and dns identify WHICH machine this session belongs to (E24 T5). Both come from the
	// certificate the connect presented and never from anything the connecting party said: dns is the SAN,
	// runnerID is derived from it, and they are what a cordon, a revoke and a heartbeat write name.
	runnerID string
	dns      string
	// life is this machine's cordon/revoke state and lease counter, shared by every session it holds.
	life *runnerLifecycle
	// queue is the (tenant, pool) rendezvous this session parks on, so a cordon can take the machine OUT
	// of it synchronously — see RunnerGateway.evict for why "synchronously" is the whole property.
	queue *poolQueue
	// beatAt bounds how often a heartbeat frame may advance the durable liveness stamp, so a runner that
	// sends them in a loop cannot turn one UPDATE per frame into a write amplifier.
	beatAt  atomic.Int64
	release chan struct{}
	// taken closes when a Dial has claimed this machine off its pool's queue. It exists because the
	// queue replaced the rendezvous a channel send used to be: the connect handler no longer learns it
	// was picked up from the send completing, so it is told.
	taken        chan struct{}
	takenOnce    sync.Once
	disconnected chan struct{}
	discOnce     sync.Once
	gc           atomic.Pointer[gatewayChannel]
}

// markDisconnected closes disconnected exactly once (readLoop may reach it from several paths).
func (pr *pendingRunner) markDisconnected() { pr.discOnce.Do(func() { close(pr.disconnected) }) }

// markTaken closes taken exactly once. A machine is handed to exactly one waiter, so once is all this
// should ever need — and a double close would panic the whole control plane, which is not a risk worth
// carrying to save a sync.Once.
func (pr *pendingRunner) markTaken() { pr.takenOnce.Do(func() { close(pr.taken) }) }

// Routes returns the gateway HTTP surface: the certless enrollment endpoint and the
// mutually-authenticated session endpoint. It carries no public API auth middleware —
// the endpoints assert their own token and mTLS identity.
func (g *RunnerGateway) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/runner/enroll", g.handleEnroll)
	mux.HandleFunc("/v1/runner/renew", g.handleRenew)
	mux.HandleFunc("/v1/runner/settings", g.handleSettings)
	mux.HandleFunc("/v1/runner/connect", g.handleConnect)
	return mux
}

type enrollRequest struct {
	// RunnerID is what the enrolling machine calls ITSELF. Since E24 T1 it is a LABEL and decides
	// nothing: the gateway mints the identity. It is still accepted (and still required non-empty) so
	// a pre-E24 runner enrolls unchanged, and it is recorded so an operator can recognise the machine.
	RunnerID  string `json:"runner_id"`
	PublicKey string `json:"public_key"`
	// The machine's reported shape. Absent on every runner built before E24, which is why neither is
	// required and neither decides anything here — they are inventory (T4 may compare them).
	OS   string `json:"os,omitempty"`
	Arch string `json:"arch,omitempty"`
	// Posture is what the machine SAYS it is (E24 T2). It is the one declared field that DECIDES
	// something: the registry compares it with the pool's and refuses the enrolment on a disagreement.
	// Absent declares nothing and enrols exactly as before — see fleet.ErrPostureMismatch for the line
	// between comparing a claim and verifying one, which this does not cross.
	Posture string `json:"posture,omitempty"`
	// PoolID is the pool the machine was CONFIGURED to join (E24 T3). It authorises nothing: the pool a
	// certificate is minted into comes from the presented KEY, and a declaration that disagrees with the
	// key REFUSES the enrolment rather than being silently widened or silently overridden. Absent
	// declares nothing and inherits the key's pool.
	PoolID string `json:"pool_id,omitempty"`
}

type enrollResponse struct {
	Certificate string `json:"certificate"`
	// RunnerID is the id the SERVER minted, returned so the runner carries it from here. It is the
	// backward-compatibility half of "the server mints the id": an old runner that sent its own name
	// still learns the real one, and a runner too old to read this field still works, because its
	// certificate's SAN carries the same id and that is what renew matches on.
	RunnerID string `json:"runner_id,omitempty"`
	// Settings is the machine's configuration as the ADMIN PLANE decided it — this pool's desired document.
	//
	// IT RIDES THIS ANSWER RATHER THAN A NEW ENDPOINT, for a credential reason rather than a convenience:
	// the operator surface for the same document is gated on the `provision` capability, and a runner holds
	// no API key and must not be given one. It already authenticates for THIS call, so this is the one place
	// a machine can be told what it is without widening what it holds.
	//
	// `omitempty` is the same contract RunnerID documents above: a control plane with no document for the
	// pool sends nothing, and a runner receiving nothing runs on the configuration it was started with.
	Settings map[string]string `json:"settings,omitempty"`
}

// handleEnroll exchanges a bearer bootstrap token for a short-lived client certificate: it spends the
// token, MINTS the runner's identity, signs the runner's public key with the local CA under that
// identity, records the machine in the registry, and returns both.
//
// THE IDENTITY IS THE SERVER'S (E24 T1, §2). Before, the gateway signed whatever name the enrolling
// party asked for — runnerDNS(request.RunnerID) with no verification — while
// deploy/compose/runner-entrypoint.sh:10 hardcoded PALAI_RUNNER_ID="runner-local", so
// `docker compose up --scale runner=3` was three machines holding one name and one certificate
// identity. "Revoke runner X" cannot mean anything while X is a name the runner chose.
//
// The token is authenticated before anything is minted, so an invalid token mints nothing. The
// REGISTRY is written BEFORE the certificate is signed: a recorded runner that failed to get a
// certificate is a harmless stale row an operator can see, while a certificate no row records is an
// identity in the field that no revoke can reach.
func (g *RunnerGateway) handleEnroll(w http.ResponseWriter, r *http.Request) {
	presented, ok := bearer(r.Header.Get("Authorization"))
	if !ok {
		http.Error(w, "invalid enrollment token", http.StatusUnauthorized)
		return
	}
	// THE BODY IS DECODED BEFORE THE CREDENTIAL IS RESOLVED, and the order is deliberate rather than
	// incidental: the pool the machine DECLARES is part of what the credential is checked against, so a
	// chain that resolved first would have to resolve twice. Nothing is minted or written until the
	// credential has been resolved below.
	var request enrollRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&request); err != nil || request.RunnerID == "" {
		http.Error(w, "invalid enrollment request", http.StatusBadRequest)
		return
	}
	publicDER, err := base64.StdEncoding.DecodeString(request.PublicKey)
	if err != nil {
		http.Error(w, "invalid public key", http.StatusBadRequest)
		return
	}

	// ONE MINTER, and this line is the whole of it. The id minted here is the id the row carries, the
	// id the certificate's SAN is built from, and the id returned to the runner — three views of one
	// machine. Letting the store mint a second one shipped once and made every later lookup miss: the
	// row recorded the name derived here while the CA signed the store's, and a renew resolves the row
	// BY THE SAN. It is minted BEFORE the credential is resolved because a refused enrolment is
	// journalled against a machine, and a record with no subject is not a record.
	runnerID := g.mintRunnerID()
	grant, status, err := g.resolveEnrollment(r.Context(), presented, runnerID, request.PoolID)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	if reg := g.registry; reg != nil {
		if _, err := reg.Register(r.Context(), fleet.Registration{
			ID: runnerID, PoolID: grant.PoolID, Label: request.RunnerID, DNS: runnerDNS(runnerID),
			PublicKeySHA256: publicKeyFingerprint(publicDER), OS: request.OS, Arch: request.Arch,
			Posture: request.Posture, KeyID: grant.KeyID,
		}); err != nil {
			// A posture the pool does not have is the machine's mistake and it is told so — an operator
			// who has handed a Mac the Linux pool's credential needs to read that, not a 503. Every other
			// failure is the control plane's and stays deliberately unspecific to the caller.
			if errors.Is(err, fleet.ErrPostureMismatch) {
				http.Error(w, "declared posture is not this pool's", http.StatusConflict)
				return
			}
			// A registry that cannot record the machine must not issue it an identity — that is the
			// whole point of writing first. The refusal is deliberately unspecific to the caller.
			http.Error(w, "enrollment could not be recorded", http.StatusServiceUnavailable)
			return
		}
	}

	certDER, err := g.issuer.SignRunnerCertificate(publicDER, runnerDNS(runnerID))
	if err != nil {
		http.Error(w, "sign runner certificate", http.StatusBadRequest)
		return
	}
	g.recordIssuedIdentity(certDER)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(enrollResponse{
		Certificate: base64.StdEncoding.EncodeToString(certDER),
		RunnerID:    runnerID,
		// grant.PoolID, never request.PoolID: the grant is the credential chain's verdict about which pool
		// this machine's key belongs to, and reading the request instead would let a machine ask for another
		// pool's configuration by naming it.
		Settings: g.settingsFor(r.Context(), grant.PoolID),
	})
}

// settingsFor reads the pool's desired configuration, and answers nothing rather than failing.
//
// A LOOKUP FAILURE MUST NOT REFUSE THE ENROLMENT. A machine's identity does not depend on its
// configuration: a runner that enrols with no settings runs on what it was started with, which is what
// every runner did before this document existed. Refusing here would turn a database blip into a fleet
// that cannot come back, and what is lost by answering anyway is a configuration the machine picks up on
// its next enrolment.
//
// The two answers are deliberately identical to the caller and different in the log: "no document" is an
// ordinary state for a pool nobody has configured, and an error is not.
func (g *RunnerGateway) settingsFor(ctx context.Context, poolID string) map[string]string {
	if g.poolSettings == nil || poolID == "" {
		return nil
	}
	settings, err := g.poolSettings.DesiredSettingsForPool(ctx, poolID)
	if err != nil {
		log.Printf("runner enrolment: pool %s desired configuration unreadable, enrolling without it: %v", poolID, err)
		return nil
	}
	return settings
}

// resolveEnrollment is THE CREDENTIAL CHAIN, tried in one order and in one place: a pool key first, the
// file bootstrap token second, refusal third.
//
// THE FILE TOKEN IS NOT DELETED AND MUST NOT BE. It is the only path that can mint an identity for a
// machine whose certificate has ALREADY EXPIRED — renewal authenticates with the certificate that is
// expiring, so a machine that missed its window (a sleeping laptop, a stalled Docker Desktop) has
// nothing else to present. local_credentials.go explains that at length; deleting it here would brick
// exactly the machine an operator was least able to reach.
//
// ONLY "NOT MINE" FALLS THROUGH. fleet.ErrUnknownPoolKey means the presented value matched no key row,
// and that is the one case where the file token is asked. A REVOKED or EXPIRED or wrong-pool key is a
// recognised credential that was refused, and re-asking a different credential about it would turn the
// chain into a way to shop for an issuer.
//
// On failure it returns the status the caller writes: 401 for a credential that is not live, 409 for a
// key that IS live but does not admit into the pool the machine declared — a Mac pointed at the wrong
// pool's key needs to read that, and a 401 would send its operator hunting for a typo in the key.
func (g *RunnerGateway) resolveEnrollment(ctx context.Context, presented, runnerID, declaredPool string) (fleet.PoolGrant, int, error) {
	if g.poolKeys != nil {
		grant, err := g.poolKeys.RedeemPoolKey(ctx, presented, runnerID, declaredPool)
		switch {
		case err == nil:
			return grant, http.StatusOK, nil
		case errors.Is(err, fleet.ErrPoolScopeMismatch):
			return fleet.PoolGrant{}, http.StatusConflict,
				errors.New("the presented enrollment key does not admit into the declared pool")
		case errors.Is(err, fleet.ErrPoolKeyRevoked), errors.Is(err, fleet.ErrPoolKeyExpired):
			// Deliberately the same words as an unknown credential: which of the three it was is in the
			// journal, where an operator can read it, and not on the wire, where an attacker could probe
			// with it. The operator-facing distinction is the 409 above, which is a MISTAKE and not a
			// credential state.
			return fleet.PoolGrant{}, http.StatusUnauthorized, errors.New("invalid enrollment token")
		case !errors.Is(err, fleet.ErrUnknownPoolKey):
			// A database that cannot answer must not be a fall-through: a store outage would otherwise
			// silently demote every machine to the file token and the default pool.
			return fleet.PoolGrant{}, http.StatusServiceUnavailable, errors.New("enrollment could not be resolved")
		}
	}
	if err := g.tokens.Consume(presented); err != nil {
		return fleet.PoolGrant{}, http.StatusUnauthorized, errors.New("invalid enrollment token")
	}
	// The file token admits into the DEFAULT pool and nowhere else, so a machine that declares another
	// one is refused rather than quietly redirected — the same rule the pool key follows, for the same
	// reason. It carries no key id: the file token is not a row and cannot be revoked on its own.
	if declaredPool != "" && declaredPool != fleet.DefaultPoolID {
		return fleet.PoolGrant{}, http.StatusConflict,
			errors.New("the presented enrollment key does not admit into the declared pool")
	}
	return fleet.PoolGrant{PoolID: fleet.DefaultPoolID}, http.StatusOK, nil
}

// mintRunnerID mints the server-side runner identity. The `rnr_` prefix follows middleware.NewID's
// convention, so a runner id is recognisable in a log line and cannot be confused with any other
// resource's opaque id.
func (g *RunnerGateway) mintRunnerID() string {
	if g.newID == nil {
		return middleware.NewID("rnr")
	}
	return g.newID("rnr")
}

// publicKeyFingerprint is the sha256 of the enrolling machine's PUBLIC key DER, recorded so an
// operator can tell a re-enrolled machine from a different one. A public key is not a credential and
// its digest is not a secret; the private half never leaves the runner.
func publicKeyFingerprint(publicDER []byte) string {
	sum := sha256.Sum256(publicDER)
	return hex.EncodeToString(sum[:])
}

// handleRenew re-issues a runner's client certificate over its EXISTING mutually-authenticated
// identity — no enrollment token. It asserts the verified client chain (the server TLS accepts
// a certless handshake for enrollment, so a tokenless, certless caller is rejected here), then
// re-signs the public key the presented certificate carries with a fresh validity window. An
// expired certificate cannot complete the mTLS handshake, so renewal is only possible while
// the current identity is still valid — the proactive ~80%-TTL renewal keeps it so. The
// bootstrap credential is never presented again on this path — it is not one-use, but renewal does
// not use it — so a long-lived runner rolls its certificate forward without re-enrolling.
func (g *RunnerGateway) handleRenew(w http.ResponseWriter, r *http.Request) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		http.Error(w, "runner client certificate required", http.StatusUnauthorized)
		return
	}
	leaf := r.TLS.PeerCertificates[0]
	publicDER, err := x509.MarshalPKIXPublicKey(leaf.PublicKey)
	if err != nil {
		http.Error(w, "marshal runner public key", http.StatusBadRequest)
		return
	}
	dns := renewDNS(leaf)
	certDER, err := g.issuer.SignRunnerCertificate(publicDER, dns)
	if err != nil {
		http.Error(w, "sign runner certificate", http.StatusBadRequest)
		return
	}
	g.recordIssuedIdentity(certDER)
	// SECOND HONEST CEILING (§T1): a renew presents a CERTIFICATE, not an id — the protocol has no
	// field for one — so the registry row is found by the certificate's DNS. That is a NAME match, and
	// it is deliberately not treated as re-proving anything the enrolment proved: it advances
	// last_seen_at and the recorded expiry, and it does not touch which key issued the identity (T3's
	// binding). A runner the registry does not know keeps renewing exactly as before.
	g.recordSeen(r.Context(), dns, certNotAfter(certDER))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(enrollResponse{
		Certificate: base64.StdEncoding.EncodeToString(certDER),
		RunnerID:    runnerIDFromDNS(dns),
	})
}

// settingsRequest is what a machine SAYS about the configuration it is currently holding. It is the report
// half of the poll, sent on the same round trip that asks for the current document.
type settingsRequest struct {
	// Revision is the document revision this machine resolved and acted on, 0 when it has never been sent
	// one. It is the machine's claim about itself and it decides nothing on this side — the answer below is
	// computed from the journal regardless — so a machine that lies about it only misreports its own row.
	Revision int64 `json:"revision"`
	// Applied is the machine's verdict per setting: `applied` (this process changed behaviour) or
	// `pending_restart` (it holds the value and is still running the old one). The control plane does not
	// and cannot derive these — whether a setting takes effect without a restart is a fact about the
	// RUNNER's code — which is exactly why the machine is asked instead of guessed at.
	Applied map[string]string `json:"applied,omitempty"`
}

// settingsResponse is the machine's effective configuration: its pool's document with its own overlaid.
type settingsResponse struct {
	Revision int64             `json:"revision"`
	Settings map[string]string `json:"settings,omitempty"`
}

// maxSettingsReport bounds the report body. A document is capped at 64 settings on the way in
// (api.maxDesiredSettings), so a report far larger than that is not a machine describing one.
const maxSettingsReport = 64 * 1024

// handleSettings is THE CHANGE-PROPAGATION SEAM: a machine that is already running asks what its
// configuration is now, and says what it did with the last one.
//
// WHY A POLL AND NOT A PUSH, since "push it from the panel" is what was asked for. cmd/runner's own header
// states the property that decides it: "It opens no inbound port and writes no credential to disk." A push
// needs a listener on every machine — a rented Mac behind NAT, a laptop, a box in somebody's office — and
// adding one would be a far larger change to the trust boundary than anything this file otherwise does.
// The runner-initiated poll gives the operator the same thing: an edit in the panel reaches the machine
// within one interval, with no inbound port and no new credential. What is traded is latency, and it is
// bounded and stated rather than hidden.
//
// THE ALTERNATIVE THAT LOOKS FREE AND IS NOT: riding the existing lease connection. The runner does hold a
// long-lived session to this gateway, so a control frame could be pushed down it — but that connection
// exists only while the machine is PARKED FOR WORK, and it is torn down and re-dialled around every lease.
// A configuration that arrived only between leases would reach a busy machine last, which is the machine an
// operator is most likely to be reconfiguring. Renewal was the third candidate and is worse still: it fires
// at ~80% of certificate lifetime, so its period is a security parameter and tying configuration latency to
// it would mean one cannot be tuned without moving the other.
//
// IT IS AUTHENTICATED AS THE MACHINE IT NAMES, which is the property that makes a machine-scoped document
// safe to serve. The identity comes from the CERTIFICATE's DNS and never from a body field, so a machine
// cannot ask for another machine's configuration by naming it — the same rule handleEnroll follows when it
// takes the pool from the resolved grant rather than from the request.
//
// A REVOKED MACHINE IS REFUSED. handleConnect already refuses one a session; a decommissioned machine that
// could still pull configuration would be a fleet member in every sense that matters. Cordon does NOT
// refuse: a cordoned machine finishes its in-flight work and is still a machine an operator may be fixing.
func (g *RunnerGateway) handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		http.Error(w, "runner client certificate required", http.StatusUnauthorized)
		return
	}
	dns := renewDNS(r.TLS.PeerCertificates[0])
	if dns == "" {
		http.Error(w, "runner certificate carries no identity", http.StatusUnauthorized)
		return
	}
	var request settingsRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxSettingsReport)).Decode(&request); err != nil {
		http.Error(w, "invalid settings report", http.StatusBadRequest)
		return
	}

	// The poll is also a LIVENESS BEAT, and deliberately so: it is the only thing a machine with no work
	// does on a schedule, so a fleet that is idle but healthy stops looking like a fleet that is gone.
	poolID, _, durable := g.recordSeen(r.Context(), dns, time.Time{})
	runnerID := runnerIDFromDNS(dns)
	life := g.lifecycle(runnerID)
	if durable == "revoked" {
		life.set(true, true)
	}
	if _, revoked, _ := life.state(); g.revoked.Load() || revoked {
		http.Error(w, ErrRunnerRevoked.Error(), http.StatusForbidden)
		return
	}

	if g.poolSettings == nil {
		// No desired-configuration store wired. The machine is answered with an empty document rather than an
		// error, because "nobody has configured you" is the posture every deployment built before this had and
		// a runner receiving it keeps running on what it was started with.
		writeSettings(w, settingsResponse{})
		return
	}
	// THE REPORT IS RECORDED BEFORE THE ANSWER IS COMPUTED, so a machine's verdict about revision N is
	// durable even if resolving N+1 fails. The other order loses the report on exactly the request that was
	// about to hand out a new document.
	// Both branches serve the machine anyway — this is bookkeeping on the side of a request whose real work
	// is answering the configuration, and failing that for a row that could not be written trades a working
	// fleet for a tidy table. They are distinguished in the LOG because one is expected and one is not.
	switch matched, err := g.poolSettings.RecordRunnerConfigReport(r.Context(), dns, request.Revision, request.Applied, g.now()); {
	case err != nil:
		log.Printf("runner settings: recording %s's report failed, answering it anyway: %v", runnerID, err)
	case !matched:
		log.Printf("runner settings: %s reported revision %d and the registry has no row for it (a machine enrolled before the registry existed)", runnerID, request.Revision)
	}
	settings, revision, err := g.poolSettings.DesiredSettingsForMachine(r.Context(), poolID, runnerID)
	if err != nil {
		// The same position settingsFor takes at enrolment, for the same reason: a machine's configuration
		// must not be able to turn a database blip into a machine that stops working. It keeps running what it
		// has and asks again next interval.
		log.Printf("runner settings: %s's desired configuration unreadable, answering with none: %v", runnerID, err)
		writeSettings(w, settingsResponse{})
		return
	}
	writeSettings(w, settingsResponse{Revision: revision, Settings: settings})
}

func writeSettings(w http.ResponseWriter, response settingsResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

// recordSeen advances the registry's liveness stamp and reports the POOL and the TENANT the machine
// belongs to, and is a no-op returning zero values when no registry is wired. It deliberately swallows
// both the not-found case and any store error: this is an INVENTORY write on the side of a request
// whose real work (issuing a certificate, accepting a session) has already succeeded or is about to,
// and failing that request because a bookkeeping row could not be updated would trade a working fleet
// for a tidy table.
//
// The pool it returns is why the swallowing is safe rather than merely convenient: "" is not a guess,
// it is the DEFAULT pool by way of poolKey — where a deployment with no pool configuration places
// every machine and every run, so nothing about it is a fallback with different behaviour.
//
// THE TENANT FALLS BACK TO THE DEFAULT POOL'S OWNER, and that clause is load-bearing rather than tidy.
// A machine the registry has no row for is a runner that enrolled BEFORE E24 and has not restarted
// since: renewal rolls its certificate forward over its own mTLS identity and never re-enrols, so it
// stays unknown for as long as it lives. Leaving its tenant empty would have parked every one of that
// deployment's runs forever the moment the control plane was upgraded — a queue keyed by tenant is
// exactly as good at excluding your own runner as somebody else's. Reading the tenant off the default
// pool row gives that machine the tenant a single-runner install has always had.
//
// IT ALSO REPORTS THE DURABLE LIFECYCLE STATE (E24 T5), and that is what makes a revocation survive a
// restart: the row is read on the same write that records the liveness, so a new process learns that a
// machine is decommissioned from the database rather than from the memory it no longer has. A machine the
// registry has no row for reports "" — the pre-E24 runner, whose lifecycle is the whole gateway's exactly
// as it always was.
func (g *RunnerGateway) recordSeen(ctx context.Context, dns string, notAfter time.Time) (string, coordinator.Tenant, string) {
	reg := g.registry
	if reg == nil || dns == "" {
		return "", coordinator.Tenant{}, ""
	}
	row, found, err := reg.RecordSeen(ctx, dns, notAfter, g.now())
	if err != nil {
		return "", coordinator.Tenant{}, ""
	}
	if found {
		return row.PoolID, coordinator.Tenant{Organization: row.Organization, Project: row.Project}, row.State
	}
	pool, found, err := reg.Pool(ctx, fleet.DefaultPoolID)
	if err != nil || !found {
		return "", coordinator.Tenant{}, ""
	}
	return pool.ID, coordinator.Tenant{Organization: pool.Organization, Project: pool.Project}, ""
}

// certNotAfter reads the expiry out of a DER the CA just produced. A DER that will not parse leaves
// the recorded expiry where it was — recordIssuedIdentity already takes that position for the same
// reason: the runner has its certificate either way.
func certNotAfter(certDER []byte) time.Time {
	leaf, err := x509.ParseCertificate(certDER)
	if err != nil {
		return time.Time{}
	}
	return leaf.NotAfter
}

// runnerIDFromDNS is the inverse of runnerDNS. A DNS name that does not carry the suffix belongs to a
// runner enrolled before E24 (its SAN is the name it chose), and returning it unchanged is correct:
// that string IS the only identity that runner has.
func runnerIDFromDNS(dns string) string {
	return strings.TrimSuffix(dns, runnerDNSSuffix)
}

// renewDNS is the runner DNS identity the presented certificate carries — its SAN, or the
// common name when the SAN is absent — so a renewal preserves the enrolled identity.
func renewDNS(leaf *x509.Certificate) string {
	if len(leaf.DNSNames) > 0 {
		return leaf.DNSNames[0]
	}
	return leaf.Subject.CommonName
}

// handleConnect accepts the runner's mutually-authenticated WebSocket, completes the
// runner.v1 handshake, and parks the connection as available. It asserts the verified
// client chain itself — the server TLS accepts a certless handshake for enrollment, so a
// session presenting no runner certificate is rejected here — then holds the HTTP
// goroutine open until the lease's channel releases it.
func (g *RunnerGateway) handleConnect(w http.ResponseWriter, r *http.Request) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		http.Error(w, "runner client certificate required", http.StatusUnauthorized)
		return
	}
	g.recordIdentity(r.TLS.PeerCertificates[0])
	// Connect is a liveness moment: the machine just authenticated with its certificate. Recorded
	// BEFORE the revoked check so that a runner turned away still leaves a trace of having tried — an
	// operator debugging "why is this Mac idle" needs the attempt, not only the successes.
	leaf := r.TLS.PeerCertificates[0]
	// The same write reports which POOL and which TENANT this machine belongs to, which together decide
	// the queue it parks on. Both come from the REGISTRY and never from the connecting party: the
	// session wire carries neither field and inventing one would let a machine choose its own placement
	// — or, worse, its own tenant. A stack with no registry yields the zero pair, and poolKey turns the
	// pool half into the default pool: §2's bit-unchanged rule.
	dns := renewDNS(leaf)
	poolID, tenant, durable := g.recordSeen(r.Context(), dns, leaf.NotAfter)
	// THE MACHINE'S OWN LIFECYCLE, ADOPTED FROM THE ROW (E24 T5). The row is the authority a restart has
	// and the memory is not, so a state the database records is applied to this process before the session
	// is admitted — which is the whole of "a revocation survives a restart". It only ever UPGRADES the
	// in-process state (an `active` row never clears a cordon somebody just wrote through the API), because
	// the API writes the row first and then this gateway, so the two can only disagree in the direction
	// where memory is ahead.
	life := g.lifecycle(runnerIDFromDNS(dns))
	switch durable {
	case "revoked":
		life.set(true, true)
	case "cordoned":
		life.set(true, false)
	case "pending":
		// The waiting room, adopted from the row (E24 T6). A machine that enrolled into a strict pool and has
		// not been admitted keeps arriving here after every reboot, and the row is the only thing that knows.
		life.setPending(true)
	}
	// A revoked gateway refuses the session before the upgrade, so a decommissioned runner never even
	// parks (SAN-011). Cordon does NOT reject the connect — a cordoned runner still parks and finishes an
	// in-flight lease; it is only Dial that stops offering it NEW work.
	if _, revoked, _ := life.state(); g.revoked.Load() || revoked {
		http.Error(w, ErrRunnerRevoked.Error(), http.StatusForbidden)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{runner.RunnerProtocolV1}})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(64 * 1024)
	if conn.Subprotocol() != runner.RunnerProtocolV1 {
		_ = conn.Close(websocket.StatusPolicyViolation, "subprotocol")
		return
	}
	_, helloPayload, err := conn.Read(r.Context()) // consume runner.hello
	if err != nil {
		return
	}
	// §48.2 support window (OPS-008): the runner advertises its build stamp in the hello's data.version.
	// A skew outside the current+previous-two-minors window is rejected here — at CONNECT, not enroll — so
	// an ALREADY-ENROLLED runner that is now too old after a control-plane upgrade is caught every session
	// (an enroll-time check would miss it: the runner never re-enrolls). The close reason carries the
	// required intermediate-hop message the runner logs. Two unstamped dev builds compare equal (skip).
	if ok, message := version.Supported(g.cpVersion, helloRunnerVersion(helloPayload)); !ok {
		_ = conn.Close(websocket.StatusPolicyViolation, truncateCloseReason(message))
		return
	}

	// The handshake succeeded: this connection is a live runner session for as long as the handler
	// runs (parked, then leased). Count it here and release the count on any return path below.
	g.connected.Add(1)
	defer g.connected.Add(-1)

	pr := &pendingRunner{
		conn: conn, runnerID: runnerIDFromDNS(dns), dns: dns, life: life,
		release: make(chan struct{}), disconnected: make(chan struct{}), taken: make(chan struct{}),
	}
	queue := g.queueFor(tenant, poolID)
	pr.queue = queue
	// The session joins the set the heartbeat pings, the reaper cuts and a cordon evicts, for exactly as
	// long as it is held open. Registered AFTER its queue is set, so an eviction can never see a session
	// with nowhere to be removed from.
	g.addSession(pr)
	defer g.removeSession(pr)
	// One goroutine owns the read side for the connection's whole life: while parked it turns a
	// dropped connection into a disconnected signal (nothing else reads then), and once a lease is
	// assigned it relays the runner's engine frames. Without it a runner that died while parked-and-
	// idle would keep the connected count — and so palai_runner_sessions — falsely at its old value.
	//
	// It starts BEFORE the waiting room below (E24 T6) rather than after the join, because a machine that
	// sits waiting for a human for an hour and dies in the middle of it must drop the connected count when
	// it dies, not when somebody finally approves it.
	go g.readLoop(pr)

	// THE WAITING ROOM (E24 T6), and it is AHEAD of the join and the wake deliberately. A machine nobody has
	// admitted is not capacity: it is not a member of its pool, so a run placed there gets
	// ErrPoolHasNoRunner and PARKS (T4) instead of spending the retry ladder while the approver is asleep —
	// and when the human does admit it, this returns into the ordinary join/wake/park sequence, so the
	// approval re-enters that parked run through the SAME wake every connect performs. No second waking
	// path, which is the rule T4 set for the same reason.
	if !g.awaitApproval(r.Context(), pr) {
		return
	}
	// A machine is a MEMBER of its queue for the whole session — parked or leased — and membership is
	// what answers "is there anything here at all". Counted before the park and released on every return
	// path, because the answer decides whether a run PARKS (nothing here) or rides the retry ladder
	// (something here, all of it busy), and getting that backwards either dead-letters runs a Mac would
	// have served or parks runs a busy machine will free in seconds.
	queue.join()
	defer queue.leave()
	// THE WAKE, and it is here rather than anywhere else because this is the only moment the control
	// plane learns a pool gained capacity. It runs BEFORE the park so a machine that connects into a
	// pool with a run waiting on it re-enters that run at once. It does NOT reserve this machine for the
	// run it woke: the woken run dials like any other and may lose the machine to another run, which
	// parks again — a benign race, proved by a test, and deliberately cheaper than a second durable
	// notion of "assigned to" alongside the one the job queue already is.
	g.wakeParkedRun(r.Context(), tenant, poolID)

	if !g.parkUntilLeased(r.Context(), pr, queue) {
		return
	}
	// Hold the hijacked connection open for the lease. release closes when the attempt ends;
	// disconnected closes if the runner drops mid-lease; the request context covers server shutdown; and a
	// REVOKE of this machine returns, which tears the connection down and kills the lease with it — the
	// hard stop cordon is not. The interrupted run is reclaimed by the existing E10 recovery layer, the
	// same way a control-plane exit mid-lease already is.
	holdLease(r.Context(), pr)
}

// awaitApproval holds a machine that enrolled into a STRICT pool outside its pool's rendezvous until a
// human admits it (E24 T6). It reports whether the session should go on to park; false means the machine
// was revoked, dropped its connection, or the server is shutting down.
//
// THE MACHINE NEVER ENTERS THE QUEUE WHILE IT WAITS, which is the whole enforcement and the reason this is
// a hold rather than a comparison inside Dial. T5 took the same position for a cordon and T2 for the pool:
// a machine that is not in the rendezvous is UNREACHABLE from a Dial, so there is no code path left that a
// later change could soften into a preference. A check inside the handover would be one `if` away from
// "close enough" for whoever is debugging an idle Mac at 3am.
//
// IT HOLDS THE SESSION OPEN rather than refusing the connect, and that pairing is what makes the approval
// cheap: the machine keeps its certificate and its connection, so an admission is a broadcast on a channel
// and not a re-enrolment. Refusing the connect instead would have made the human's approval useless until
// the machine's own retry loop came back.
func (g *RunnerGateway) awaitApproval(ctx context.Context, pr *pendingRunner) bool {
	for {
		pending, revoked, changed := pr.life.awaiting()
		if revoked {
			return false
		}
		if !pending {
			return true
		}
		select {
		case <-changed:
			// Admitted, revoked, or cordoned — the next pass reads which.
		case <-pr.disconnected:
			return false
		case <-ctx.Done():
			return false
		}
	}
}

// parkUntilLeased parks pr and reports whether a Dial claimed it. A CORDON takes the machine out of the
// rendezvous and holds it here — connected, leaseless, resumable — which is what makes a cordon neither
// an outage nor a decommission. A revoke, a disconnect or a finished request returns false.
//
// The unpark's return value is load-bearing: false means a Dial took the machine in the race between the
// transition and the unpark, and the caller must fall through to the lease hold rather than tear a
// connection down under an attempt that is already using it.
func (g *RunnerGateway) parkUntilLeased(ctx context.Context, pr *pendingRunner, queue *poolQueue) bool {
	for {
		cordoned, revoked, changed := pr.life.state()
		if revoked {
			return false
		}
		if cordoned {
			select {
			case <-changed:
				continue // resumed (or revoked — the next pass decides which)
			case <-pr.disconnected:
				return false
			case <-ctx.Done():
				return false
			}
		}
		queue.park(pr)
		select {
		case <-pr.taken:
			return true
		case <-changed:
			// Leave the rendezvous (idempotent — a cordon may have evicted this session already) and then ask
			// whether anybody claimed the machine on the way past. `taken` is closed under the queue's own lock
			// at the moment of a handover, so this answer is not a race: unclaimed means re-read the posture,
			// claimed means the lease owns this connection now.
			queue.unpark(pr)
			select {
			case <-pr.taken:
				return true
			default:
				continue
			}
		case <-pr.disconnected:
			queue.unpark(pr)
			return false // the runner dropped before any lease
		case <-ctx.Done():
			queue.unpark(pr)
			return false
		}
	}
}

// holdLease blocks for the life of an offered lease. A revoke of THIS machine returns, so the connection
// is torn down and a decommissioned runner's in-flight work stops rather than completing.
func holdLease(ctx context.Context, pr *pendingRunner) {
	for {
		_, revoked, changed := pr.life.state()
		if revoked {
			return
		}
		select {
		case <-pr.release:
			return
		case <-pr.disconnected:
			return
		case <-ctx.Done():
			return
		case <-changed:
		}
	}
}

// addSession and removeSession bracket one live session's membership of the heartbeat set.
func (g *RunnerGateway) addSession(pr *pendingRunner) {
	g.machinesMu.Lock()
	g.sessions[pr] = struct{}{}
	g.machinesMu.Unlock()
}

func (g *RunnerGateway) removeSession(pr *pendingRunner) {
	g.machinesMu.Lock()
	delete(g.sessions, pr)
	g.machinesMu.Unlock()
}

// Dial offers a machine IN THE ATTEMPT'S POOL the attempt's lease and returns the bridged
// EngineChannel. It blocks until such a machine is free or ctx is done, then publishes the channel to
// the connection's readLoop and writes the lease.offer. It is the production EngineDialer the
// orchestrator drives unchanged.
//
// THE POOL IS A REFUSAL, NOT A PREFERENCE (§2). A machine enrolled in another pool is not offered this
// lease — not as a fallback, not as the nearest thing — because a pool IS a posture: an attempt that
// needs an unsandboxed host is not nearly satisfied by a sandboxed container. Structurally the queue is
// the enforcement: a machine parked in pool A is unreachable from a Dial on pool B, so there is no code
// path that could relax it later.
//
// An empty pool means the Dial WAITS, which today ends at ctx (~20s, then the retry ladder). T4 turns
// that wait into a durable park; nothing here needs to change for it.
func (g *RunnerGateway) Dial(ctx context.Context, attempt AttemptDescriptor) (EngineChannel, error) {
	// A cordoned/revoked gateway offers no NEW lease: return before touching any pool queue so the
	// attempt requeues (drain) rather than dispatching onto a runner being replaced or decommissioned.
	if g.revoked.Load() {
		return nil, ErrRunnerRevoked
	}
	if g.cordoned.Load() {
		return nil, ErrRunnerCordoned
	}
	// An attempt with no timestamp of its own is ordered from the moment it queued. That is the honest
	// fallback rather than "first" or "last": it says only what is known.
	queuedAt := attempt.QueuedAt
	if queuedAt.IsZero() {
		queuedAt = g.now()
	}
	queue := g.queueFor(attempt.Tenant, attempt.PoolID)
	waiter := queue.wait(queuedAt)
	select {
	case pr := <-waiter.ch:
		pr.markTaken()
		// Re-check AFTER receiving the runner: a Dial already blocked in this select when Cordon/Revoke
		// fired would otherwise slip a lease past the pre-check and increment active after a Drain read
		// active==0 (a post-cordon lease). Refuse and hand the runner back (close release) instead.
		//
		// THE MACHINE'S OWN POSTURE IS RE-CHECKED HERE TOO (E24 T5), one layer down and for the same reason:
		// a cordoned machine has LEFT the rendezvous, so reaching this line at all means the transition
		// raced the handover. Handing it back costs that machine its connection — it re-dials at once — and
		// that is the honest trade against offering a lease to a machine an operator just took out of
		// service.
		cordonedMachine, revokedMachine, _ := pr.life.state()
		if g.revoked.Load() || g.cordoned.Load() || cordonedMachine || revokedMachine {
			close(pr.release)
			if g.revoked.Load() || revokedMachine {
				return nil, ErrRunnerRevoked
			}
			return nil, ErrRunnerCordoned
		}
		// A relayed engine frame can be as large as the lease's per-frame bound; raise the read limit
		// off the handshake cap before the runner's post-offer frames reach readLoop's blocked Read.
		pr.conn.SetReadLimit(attempt.Limits.MaxFrameBytes + 64*1024)
		offer, err := leaseOffer(attempt, g.now())
		if err != nil {
			close(pr.release)
			return nil, err
		}
		gc := newGatewayChannel(pr, attempt)
		// Close decrements THIS MACHINE's in-flight-lease counter (E24 T5). The whole-gateway Drain sums
		// every machine's, so a swap still waits for the whole fleet; the read surface publishes this one's.
		gc.active = &pr.life.active
		// Publish the channel BEFORE writing the offer, so the runner's first engine frame — which it
		// sends only after receiving the offer — always finds a relay target in readLoop.
		pr.gc.Store(gc)
		if err := pr.conn.Write(ctx, websocket.MessageText, offer); err != nil {
			// Do NOT close frames here: Write can flush the offer and still return a ctx-cancel error, so
			// the runner may already be sending a frame that readLoop is mid-emit on. close(release) alone
			// unblocks that emit (it returns false); the handler then returns → CloseNow → readLoop's Read
			// errors → readLoop closes frames itself. readLoop is the SOLE frames-closer, so there is no
			// send-on-closed-channel panic (which would crash the whole control plane).
			close(pr.release)
			return nil, fmt.Errorf("offer lease: %w", err)
		}
		pr.life.active.Add(1) // the lease is in flight; Close (always called on terminal) decrements it.
		return gc, nil
	case <-ctx.Done():
		// Leave the queue. abandon also covers the race where a machine was handed over between the
		// cancel and this line: it re-parks it rather than dropping a healthy connection.
		queue.abandon(waiter)
		// THE ONE DISTINCTION THIS WHOLE BUDGET EXISTS TO MAKE. The wait is spent first — so a runner
		// that is merely still starting up (compose brings the control plane up before its runner) is
		// picked up exactly as it always was, and this path is byte-compatible with the behaviour §2
		// protects. What changed is the ANSWER when it expires: a pool that held no machine of this
		// tenant for the whole budget is not a slow pool, it is an absent one, and the orchestrator
		// parks the run on it rather than spending five attempts and dead-lettering in ~2.5 minutes
		// while the Mac is still booting (§3.6 D12, P10). A pool whose machines are all leased returns
		// ctx.Err() and rides the existing ladder, unchanged.
		if queue.members() == 0 {
			return nil, ErrPoolHasNoRunner
		}
		return nil, ctx.Err()
	}
}

// wakeParkedRun re-enters the OLDEST run parked on this pool for want of a machine, in ONE transaction
// with the response.run job that opens its next attempt. It is called from handleConnect — the only
// moment the control plane learns a pool gained capacity.
//
// IT SWALLOWS ITS ERRORS AND SAYS SO. A wake is work done on the side of accepting a session, and a
// store that could not answer must not cost the machine its connection: the run stays waiting and the
// machine's NEXT connect (a runner re-dials after every lease) tries again. What it must never do is
// wake the WRONG run, and that is not a matter of error handling — the predicate is in the query.
func (g *RunnerGateway) wakeParkedRun(ctx context.Context, tenant coordinator.Tenant, poolID string) {
	if g.wake == nil || tenant.Organization == "" || tenant.Project == "" {
		return
	}
	_, _ = g.wake.WakeRunAwaitingCapacity(ctx, tenant, poolKey(poolID))
}

// heartbeatInterval and heartbeatTimeout are the reaper's cadence and its patience.
//
// ponytail: FIXED, not per-pool. §T5's own honest ceiling says so, and the reason it is tolerable is that
// the numbers are not a policy but a liveness probe: 30s between pings and 10s to answer one is generous
// for any machine that is running at all, and a machine that needs longer is a machine whose next lease
// would time out anyway. Per-pool tuning becomes worth building when a pool exists whose network makes
// 10s tight.
const (
	heartbeatInterval = 30 * time.Second
	heartbeatTimeout  = 10 * time.Second
	// heartbeatMinWrite bounds how often a runner-sent heartbeat FRAME may advance the durable stamp. The
	// gateway's own ping is already rate-limited by its interval; a frame arrives whenever the runner
	// chooses to send one, so this is what stops a chatty (or hostile) runner turning liveness into a write
	// amplifier.
	heartbeatMinWrite = 5 * time.Second
)

// Heartbeat pings every live runner session, advances the registry's liveness stamp for the ones that
// answer, and CUTS the ones that do not. It reports (alive, cut).
//
// A PING RATHER THAN A RUNNER-SENT FRAME, and this is the load-bearing design decision of the reaper.
// §T5 assumed the runner already sends heartbeats; it does not (see readLoop's default arm). A ping needs
// NO runner change at all — coder/websocket answers one from inside the peer's own read loop, and every
// runner is in that loop for its whole session — and it proves strictly more than a timer-driven frame
// would: that the connection is alive in BOTH directions right now. That is what catches the failure this
// reaper exists for, which is a session alive to the kernel and dead to the process — a suspended laptop,
// an unplugged Mac, a wedged runner — where `readLoop` learns nothing because a peer that stopped
// answering produces no read error.
//
// CUTTING IS ALL IT DOES, AND IT DELIBERATELY DOES NOT WAKE THAT POOL'S PARKED RUNS: capacity is still
// absent, so waking a run would hand it straight back to a pool with nothing in it. What the cut DOES do
// is decrement the count that decides whether the next run parks or rides the retry ladder — the pool's
// membership — so a run placed there parks (honest: nothing here) instead of being handed to a corpse.
//
// IT WRITES NO `unhealthy` STATE, WHICH IS A CORRECTION TO §T5. `runners.state` is CHECK-constrained to
// ('pending','active','cordoned','revoked') by migration 000045 and E24 owns exactly one migration, which
// is T1's — so `unhealthy` is not a value this task can write. It is also not a value this task needs:
// health is DERIVED from `last_seen_at`, which is already durable and already advancing, and a stamped
// flag would additionally have to be CLEARED when the machine came back. A reaper re-derives after a
// restart either way.
func (g *RunnerGateway) Heartbeat(ctx context.Context, timeout time.Duration) (alive, cut int) {
	if timeout <= 0 {
		timeout = heartbeatTimeout
	}
	g.machinesMu.RLock()
	live := make([]*pendingRunner, 0, len(g.sessions))
	for pr := range g.sessions {
		live = append(live, pr)
	}
	g.machinesMu.RUnlock()

	for _, pr := range live {
		pingCtx, cancel := context.WithTimeout(ctx, timeout)
		err := pr.conn.Ping(pingCtx)
		cancel()
		if err != nil {
			// It stopped answering. Cutting is what the connect handler is waiting for: it unparks, drops the
			// connected count and the pool's membership, and any in-flight lease ends as a disconnect — which
			// the EXISTING E10 recovery layer reclaims, exactly as it reclaims a control plane that exited.
			pr.markDisconnected()
			cut++
			continue
		}
		g.recordHeartbeat(pr)
		alive++
	}
	return alive, cut
}

// HeartbeatLoop runs Heartbeat every heartbeatInterval until ctx is done. It is the shape every other
// supervised loop in this tree has, so a transient error is a logged restart rather than a dead reaper —
// and it returns ctx.Err() so the supervisor knows the difference between cancelled and crashed.
func (g *RunnerGateway) HeartbeatLoop(ctx context.Context) error {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			g.Heartbeat(ctx, heartbeatTimeout)
		}
	}
}

// recordHeartbeat advances the durable liveness stamp for one session's machine, at most once every
// heartbeatMinWrite. It passes a ZERO certificate expiry, so `cert_not_after` is left where the last
// enrol/renew put it (the statement coalesces): a heartbeat proves the machine is there and says nothing
// new about its certificate.
func (g *RunnerGateway) recordHeartbeat(pr *pendingRunner) {
	if g.registry == nil || pr.dns == "" {
		return
	}
	now := g.now()
	last := pr.beatAt.Load()
	if last != 0 && now.Sub(time.Unix(0, last)) < heartbeatMinWrite {
		return
	}
	if !pr.beatAt.CompareAndSwap(last, now.UnixNano()) {
		return // another frame won the window; one write is the point
	}
	// Not the request context: a heartbeat outlives the frame that triggered it and a ping has no request
	// at all. Bounded so a stalled database cannot hold the reaper's pass open.
	ctx, cancel := context.WithTimeout(context.Background(), heartbeatTimeout)
	defer cancel()
	g.recordSeen(ctx, pr.dns, time.Time{})
}

// poolQueue is ONE pool's rendezvous: the machines parked in it with nothing to do, and the attempts
// waiting on it with nothing to run on. Exactly one of the two lists is non-empty at any moment —
// a parked machine and a waiting attempt meet immediately.
//
// THE ORDER IS A DECISION, AND THIS IS WHERE IT IS WRITTEN DOWN. §3.6 D2 measured the capability
// plane's queue: `ORDER BY latest.job_id LIMIT 1` over a crypto/rand hex id, which is not FIFO, is not
// any order a person would recognise, and is re-decided on every poll — the smallest hex wins, and
// wins again. So FIFO is NOT a behaviour E24 preserves; it is a decision E24 takes, and the reason is
// that a run a human is waiting on must not be overtaken because another run's identifier sorted lower.
//
// The coordinate is the RUN's queued-at, not the moment the attempt reached this gateway, and that
// distinction is the point rather than a detail. A run that gets bounced — a cordon during an upgrade,
// a retry, a resume after a park — re-dials, and with arrival order it would go to the back of the
// line every single time: the same starvation D2 found, transplanted into the runner plane. Carrying
// the run's own timestamp keeps its place.
//
// seq breaks a tie so the order is TOTAL. Two runs created in the same nanosecond (or an attempt with
// no timestamp, which takes the clock at the moment it queued) must still have a defined winner, or
// "FIFO" is only mostly an order.
//
// ponytail: linear insert and a slice, O(n) per waiting attempt — n here is the number of runs blocked
// on ONE pool with nothing free, which is bounded by the dispatch workers. A heap when a pool's queue
// is long enough for anyone to measure.
type poolQueue struct {
	mu      sync.Mutex
	parked  []*pendingRunner
	waiting []*poolWaiter
	seq     uint64
	// present counts the machines holding a session on this queue, parked OR leased. `parked` cannot
	// answer that question — a machine serving a lease is not in it — and the difference between "no
	// machine here" and "every machine here is busy" is what decides whether a run parks durably or
	// rides the retry ladder.
	present int
}

// join and leave bracket one machine's session on this queue.
func (q *poolQueue) join() {
	q.mu.Lock()
	q.present++
	q.mu.Unlock()
}

func (q *poolQueue) leave() {
	q.mu.Lock()
	q.present--
	q.mu.Unlock()
}

// members is how many machines currently hold a session on this queue.
func (q *poolQueue) members() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.present
}

// poolWaiter is one attempt's place in a pool's queue. ch is buffered so a handover never blocks the
// machine doing the handing — a waiter that has given up in the meantime must not wedge a park.
type poolWaiter struct {
	queuedAt time.Time
	seq      uint64
	ch       chan *pendingRunner
}

// olderFirst is the ordering, in one place: earlier queued-at first, then earlier arrival.
func (w *poolWaiter) olderFirst(other *poolWaiter) bool {
	if !w.queuedAt.Equal(other.queuedAt) {
		return w.queuedAt.Before(other.queuedAt)
	}
	return w.seq < other.seq
}

// depth is the number of attempts queued on this pool with nothing free to take them.
func (q *poolQueue) depth() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.waiting)
}

// park hands pr to the OLDEST waiting attempt, or holds it until one arrives.
func (q *poolQueue) park(pr *pendingRunner) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.waiting) > 0 {
		w := q.waiting[0]
		q.waiting = q.waiting[1:]
		// MARKED UNDER THE QUEUE LOCK (E24 T5), which is what makes `taken` the authority on "somebody
		// claimed this machine". A cordon evicts a machine by unparking it, so unpark's own return value can
		// no longer tell a Dial's claim from an eviction — and getting that wrong strands a resumed machine
		// in the lease hold with no lease. unpark takes this same lock, so after it returns the answer is
		// already decided.
		pr.markTaken()
		w.ch <- pr // buffered: never blocks
		return
	}
	q.parked = append(q.parked, pr)
}

// unpark removes a machine that dropped (or whose request ended) while parked. It reports false when
// the machine had already been handed to a waiter — the caller cannot take it back then, and does not
// need to: the Dial holding it discovers the dead connection when it writes the offer.
func (q *poolQueue) unpark(pr *pendingRunner) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, parked := range q.parked {
		if parked == pr {
			q.parked = append(q.parked[:i], q.parked[i+1:]...)
			return true
		}
	}
	return false
}

// wait registers an attempt on this pool. A machine already parked is handed over at once; otherwise
// the waiter takes its place in the queue by age.
func (q *poolQueue) wait(queuedAt time.Time) *poolWaiter {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.seq++
	w := &poolWaiter{queuedAt: queuedAt, seq: q.seq, ch: make(chan *pendingRunner, 1)}
	if len(q.parked) > 0 {
		pr := q.parked[0]
		q.parked = q.parked[1:]
		pr.markTaken() // under the queue lock — see park() for why this is where it happens
		w.ch <- pr
		return w
	}
	at := len(q.waiting)
	for i, queued := range q.waiting {
		if w.olderFirst(queued) {
			at = i
			break
		}
	}
	q.waiting = append(q.waiting, nil)
	copy(q.waiting[at+1:], q.waiting[at:])
	q.waiting[at] = w
	return w
}

// abandon drops a waiter whose attempt gave up. If a machine had already been handed to it in the race
// between the two, the machine goes back on the queue — dropping it would tear down a healthy
// connection because an unrelated attempt timed out.
func (q *poolQueue) abandon(w *poolWaiter) {
	q.mu.Lock()
	for i, queued := range q.waiting {
		if queued == w {
			q.waiting = append(q.waiting[:i], q.waiting[i+1:]...)
			q.mu.Unlock()
			return
		}
	}
	q.mu.Unlock()
	select {
	case pr := <-w.ch:
		q.park(pr)
	default:
	}
}

// readLoop is the connection's sole reader for its whole life. Before a lease (pr.gc unset) it exists
// only to notice a disconnect: a read error there closes disconnected, so the parked connect handler
// returns and the connected count — and palai_runner_sessions — drops at once rather than lingering
// until the next Dial. After Dial publishes the channel it relays the runner's session frames: an
// engine.frame surfaces on Receive; a lease.complete ends the stream (clean io.EOF on a succeeded
// outcome, an error otherwise so the attempt retries); a read error ends it as EOF.
func (g *RunnerGateway) readLoop(pr *pendingRunner) {
	for {
		messageType, payload, err := pr.conn.Read(context.Background())
		if err != nil {
			if gc := pr.gc.Load(); gc != nil {
				gc.closeFrames() // Receive sees io.EOF
			}
			pr.markDisconnected()
			return
		}
		// A gateway revoked mid-session refuses this runner's stale frames (SAN-011): tear the relay down
		// as if the runner disconnected, so a decommissioned runner's in-flight events reach no attempt.
		// Since E24 T5 a revoke of THIS MACHINE does the same, which is what makes the hard stop targeted.
		if _, revokedMachine, _ := pr.life.state(); g.revoked.Load() || revokedMachine {
			if gc := pr.gc.Load(); gc != nil {
				gc.failRelay(ErrRunnerRevoked)
			}
			pr.markDisconnected()
			return
		}
		gc := pr.gc.Load()
		if gc == nil {
			continue // parked: a runner sends nothing before a lease; ignore any stray frame
		}
		if messageType != websocket.MessageText {
			gc.failRelay(errors.New("runner session frame must be a text message"))
			return
		}
		var message contracts.RunnerMessage
		if err := json.Unmarshal(payload, &message); err != nil {
			gc.failRelay(fmt.Errorf("decode runner session frame: %w", err))
			return
		}
		switch message.Type {
		case "engine.frame":
			frame, err := decodeRelayFrame(message.Data)
			if err != nil {
				gc.failRelay(err)
				return
			}
			if !gc.emit(relayRead{frame: frame}) {
				gc.closeFrames()
				return
			}
		case "lease.complete":
			if outcome, _ := message.Data["outcome"].(string); outcome != "succeeded" {
				gc.failRelay(fmt.Errorf("runner reported lease outcome %q", outcome))
				return
			}
			gc.closeFrames() // succeeded → close frames → Receive sees io.EOF
			return
		case runner.ExecResultType:
			// The machine's answer to a command this control plane asked it to run (A.3). Without this
			// arm the message would fall through to the heartbeat default below and be counted as
			// liveness, and every RemoteShell would wait for an answer that had already arrived.
			//
			// The hand-off cannot block — see execPending.deliver — because this goroutine is the
			// connection's sole reader and a reader parked here strands every later message on it.
			gc.deliverExecResult(message.Data)
		default:
			// A heartbeat carries nothing to RELAY and it does carry one fact: this machine is alive. Since
			// E24 T5 that fact advances `runners.last_seen_at`, which is the stamp an operator reads.
			//
			// NOTHING IN THIS TREE SENDS ONE TODAY, AND THAT IS A CORRECTION TO §T5. The plan reasoned that
			// binding this arm was nearly free "because the frame already arrives and is thrown away" — it does
			// not arrive: packages/runner writes exactly `runner.hello`, `engine.frame` and `lease.complete`
			// (session.go), and while PARKED it is blocked in Read and writes nothing at all. So this arm is
			// forward compatibility for a runner that grows one (T7's relay is the obvious candidate), and the
			// liveness that actually keeps the stamp fresh today is the gateway's own ping — see Heartbeat.
			g.recordHeartbeat(pr)
		}
	}
}

// gatewayChannel bridges the runner's lease session to the orchestrator's EngineChannel: Send relays a
// controller frame to the runner, the gateway's readLoop surfaces the runner's engine frames on
// Receive, and the runner's lease.complete closes the stream — clean (io.EOF) on a succeeded outcome,
// an error otherwise so the attempt is retried.
type gatewayChannel struct {
	pr          *pendingRunner
	attempt     AttemptDescriptor
	leaseID     string
	frames      chan relayRead
	releaseOnce sync.Once
	framesOnce  sync.Once
	// active is the gateway's in-flight-lease counter (nil in the white-box channel tests). Close
	// decrements it exactly once so a drain sees the lease finish.
	active *atomic.Int64
	// execs is the set of commands this lease asked the MACHINE to run and has not yet heard back
	// about (A.3). It lives here because readLoop below is the connection's sole reader: the answer
	// arrives on the goroutine that reads every other message, so this is where that goroutine hands
	// it over. Every method that touches it is in remote_shell.go, beside the client that waits on it.
	execs execPending
}

type relayRead struct {
	frame contracts.EngineFrame
	err   error
}

func newGatewayChannel(pr *pendingRunner, attempt AttemptDescriptor) *gatewayChannel {
	return &gatewayChannel{pr: pr, attempt: attempt, leaseID: leaseID(attempt), frames: make(chan relayRead)}
}

// closeFrames closes the frames channel exactly once (readLoop reaches it from several paths), so
// Receive sees io.EOF and a repeated close never panics.
//
// It also answers every command still waiting on this machine (A.3), which is what a tool call gets
// instead of silence when the connection that would have carried its exec.result is the one that just
// ended. This covers the path a dropped connection actually takes — readLoop's read error, which
// reaches here directly. Every OTHER teardown answers earlier still, in failRelay, and that ordering
// is load-bearing rather than incidental; the comment there says why.
func (c *gatewayChannel) closeFrames() {
	c.framesOnce.Do(func() {
		close(c.frames)
		c.execs.closeAll(errLeaseConnectionEnded)
	})
}

// Send relays one controller->engine frame to the runner inside a controller.frame.
func (c *gatewayChannel) Send(ctx context.Context, frame contracts.EngineFrame) error {
	message := contracts.RunnerMessage{
		Protocol:  runner.RunnerProtocolV1,
		Type:      "controller.frame",
		Time:      time.Now().UTC().Format(time.RFC3339),
		LeaseID:   c.leaseID,
		RunID:     c.attempt.RunID,
		AttemptID: c.attempt.AttemptID,
		Fence:     int(c.attempt.Fence),
		Data:      map[string]any{"frame": frame},
	}
	payload, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("encode controller frame: %w", err)
	}
	return c.pr.conn.Write(ctx, websocket.MessageText, payload)
}

// Receive yields the next engine frame the runner streamed, or io.EOF once the lease
// completes cleanly.
func (c *gatewayChannel) Receive(ctx context.Context) (contracts.EngineFrame, error) {
	select {
	case read, ok := <-c.frames:
		if !ok {
			return contracts.EngineFrame{}, io.EOF
		}
		return read.frame, read.err
	case <-ctx.Done():
		return contracts.EngineFrame{}, ctx.Err()
	}
}

// Close releases the connect handler, which tears the WebSocket down, and decrements the gateway's
// in-flight-lease counter exactly once so a concurrent Drain sees this lease finish.
func (c *gatewayChannel) Close() error {
	c.releaseOnce.Do(func() {
		close(c.pr.release)
		if c.active != nil {
			c.active.Add(-1)
		}
		// The other teardown door (A.3). An attempt that releases its lease while a command is still
		// out gets its answer here rather than waiting for readLoop to notice the torn-down socket.
		c.execs.closeAll(errLeaseConnectionEnded)
	})
	return nil
}

// failRelay ends the relay with a reason: every command still waiting on this machine is answered,
// the reason is reported to the attempt, and the stream closes.
//
// THE ORDER IS THE POINT AND IT IS NOT COSMETIC (A.3). emit blocks until the orchestrator receives,
// and an orchestrator waiting inside a tool call is not receiving — so emitting first would park this
// goroutine against a caller that is itself parked on an answer only this goroutine can deliver, and
// neither would ever move. Answering the command first releases the orchestrator, which returns to
// Receive, which is what lets the emit below complete and the attempt learn why its lease ended.
func (c *gatewayChannel) failRelay(err error) {
	c.execs.closeAll(err)
	c.emit(relayRead{err: err})
	c.closeFrames()
}

// emit delivers one read to Receive, or stops if the channel was closed.
func (c *gatewayChannel) emit(read relayRead) bool {
	select {
	case c.frames <- read:
		return true
	case <-c.pr.release:
		return false
	}
}

// leaseOffer builds the runner.v1 lease.offer for an attempt: the fenced identity, the
// immutable engine image digest, and the execution bounds the runner enforces.
func leaseOffer(attempt AttemptDescriptor, now time.Time) ([]byte, error) {
	message := contracts.RunnerMessage{
		Protocol:  runner.RunnerProtocolV1,
		Type:      "lease.offer",
		Time:      now.UTC().Format(time.RFC3339),
		LeaseID:   leaseID(attempt),
		RunID:     attempt.RunID,
		AttemptID: attempt.AttemptID,
		Fence:     int(attempt.Fence),
		Data: map[string]any{
			"image_digest": attempt.ImageDigest,
			"limits":       attempt.Limits,
		},
	}
	// Carry the workspace allocation the runner bind-mounts to /workspace (spec §29.9, FLAG A). Only
	// when the attempt holds one, so a workspace-less lease is byte-for-byte the pre-E09 offer.
	if attempt.WorkspaceHostPath != "" {
		message.Data["workspace_host_path"] = attempt.WorkspaceHostPath
		message.Data["workspace_read_only"] = attempt.WorkspaceReadOnly
		message.Data["workspace_unsafe"] = attempt.WorkspaceUnsafe
	}
	return json.Marshal(message)
}

// decodeRelayFrame extracts the single engine.v1 frame a relay message carries in its
// data.frame field — the inbound mirror of Send's controller.frame wrapping.
func decodeRelayFrame(data map[string]any) (contracts.EngineFrame, error) {
	raw, ok := data["frame"]
	if !ok {
		return contracts.EngineFrame{}, errors.New("relay message carries no frame")
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return contracts.EngineFrame{}, fmt.Errorf("encode relay frame: %w", err)
	}
	var frame contracts.EngineFrame
	if err := json.Unmarshal(encoded, &frame); err != nil {
		return contracts.EngineFrame{}, fmt.Errorf("decode relay frame: %w", err)
	}
	return frame, nil
}

// leaseID derives a stable lease id for an attempt so every offer for the same attempt
// carries the same lease identity.
func leaseID(attempt AttemptDescriptor) string {
	return "lease_" + string(attempt.AttemptID)
}

// runnerDNSSuffix is the internal zone every runner certificate's SAN sits in.
const runnerDNSSuffix = ".runners.palai.internal"

// runnerDNS derives the client-certificate DNS identity for an enrolling runner. The
// session verifies the controller's identity, not its own, so this names the runner in
// its certificate without being hostname-checked.
//
// Since E24 T1 the id it is given is the SERVER's, never the enrolling party's, which is what makes
// this name an identity rather than a claim.
func runnerDNS(runnerID string) string {
	return runnerID + runnerDNSSuffix
}

// helloRunnerVersion extracts the runner's advertised build stamp from a runner.hello payload's
// data.version field. A hello that carries none (a pre-E15-T2 runner, or a malformed frame) yields the
// empty string, which version.Supported treats as an unstamped build and does not enforce — so an older
// runner that predates the advertised-version handshake is not spuriously rejected by the window check.
func helloRunnerVersion(payload []byte) string {
	var message contracts.RunnerMessage
	if err := json.Unmarshal(payload, &message); err != nil {
		return ""
	}
	v, _ := message.Data["version"].(string)
	return v
}

// truncateCloseReason bounds a WebSocket close reason to the 123-byte control-frame limit (RFC 6455), so
// a long OPS-008 hop message still closes cleanly with as much of the reason as fits.
func truncateCloseReason(reason string) string {
	const maxCloseReason = 123
	if len(reason) <= maxCloseReason {
		return reason
	}
	return reason[:maxCloseReason]
}

// bearer extracts the token from an "Authorization: Bearer <token>" header.
func bearer(header string) (string, bool) {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || header[:len(prefix)] != prefix {
		return "", false
	}
	return header[len(prefix):], true
}
