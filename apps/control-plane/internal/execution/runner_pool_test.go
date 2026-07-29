package execution_test

// The E24 T2 placement proofs, both driven over the REAL gateway (enrollment + mTLS WebSocket + the
// real lease offer) rather than against a method call, because the claim is about what crosses the
// WIRE: which machine is offered which run.
//
// Two claims, and they pull in opposite directions on purpose. The first is that a pool is a REFUSAL:
// a run that needs an unsandboxed host is not "nearly" satisfied by a sandboxed container, so a
// cross-pool offer must not happen at all. The second is that a deployment which has configured no
// pool must not be able to tell any of this was built.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/apps/control-plane/internal/fleet"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/runner"
)

// hostPoolID is a pool whose posture is 'unsandboxed-host' — a rented Mac. The gateway never reads a
// posture (a pool IS one, and the runner plane carries only the pool id), so what the id stands for
// lives in the registry; here it names the pool the sandboxed runner is NOT in.
const hostPoolID = "pool_mac_host"

// parkedRunner is one machine enrolled and parked on the gateway, with the lease it was offered — if
// it was offered one at all. lease is buffered so a runner that is never leased leaks no goroutine.
type parkedRunner struct {
	identity runner.Identity
	lease    chan runner.Lease
}

// park enrolls a machine against the fixture and holds it open waiting for a lease, the way a real
// runner's park loop does. The returned channel receives at most one lease.
func park(t *testing.T, f *gatewayFixture, token string) *parkedRunner {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	identity, err := runner.Enroll(ctx, f.bootstrap(token))
	if err != nil {
		t.Fatalf("enroll %s: %v", token, err)
	}
	pr := &parkedRunner{identity: identity, lease: make(chan runner.Lease, 1)}
	go func() {
		lease, err := f.session(identity).ReceiveLease(ctx)
		if err == nil {
			pr.lease <- lease
		}
	}()
	return pr
}

// TestAnUnsandboxedHostAttemptIsNeverOfferedToASandboxedLinuxRunner is §2's second non-negotiable
// stated as a measurement: "YERLEŞTİRME BİR REDDİR, BİR TERCİH DEĞİLDİR". An attempt that names the
// unsandboxed-host pool must not be offered to a machine enrolled in the sandboxed-linux one — not
// queued onto it, not given it as the nearest available thing, refused.
//
// RED before the fix, and the reason is one line of the gateway: `available` was a SINGLE unbuffered
// channel that every parked runner sent itself down and every Dial received from, with no pool on
// either side. Any parked machine satisfied any attempt. A Mac pool and a container pool were two
// names for one queue.
//
// The assertion is deliberately two-sided. That the Dial fails proves little on its own — a Dial that
// always failed would pass it — so the runner that must NOT be used is asserted to still be parked
// and still to hold no lease, and then the SAME runner is shown taking the run that does belong to
// its pool.
func TestAnUnsandboxedHostAttemptIsNeverOfferedToASandboxedLinuxRunner(t *testing.T) {
	f := newGatewayFixture(t, newOneUseTokens("pool-token-1"))
	f.gateway.SetRegistry(newFakeRegistry(hostPoolID))

	sandboxed := park(t, f, "pool-token-1")
	waitConnected(t, f.gateway, 1)

	// The run needs a host: it names the unsandboxed-host pool, which has no machine in it.
	wanted := f.attempt("run_pool_host", "att_pool_host", 1)
	wanted.PoolID = hostPoolID
	dialCtx, cancelDial := context.WithTimeout(context.Background(), time.Second)
	defer cancelDial()
	if ch, err := f.gateway.Dial(dialCtx, wanted); err == nil {
		_ = ch.Close()
		t.Fatalf("an attempt for pool %q was offered a runner enrolled in %q: placement picked the nearest machine instead of refusing",
			hostPoolID, fleet.DefaultPoolID)
	}
	select {
	case lease := <-sandboxed.lease:
		t.Fatalf("the sandboxed runner received lease %s for a run that named the unsandboxed-host pool", lease.LeaseID)
	default:
	}
	// The refusal must not have consumed the machine: it is still parked, still available to its own
	// pool. A "refusal" that drops the connection would be an outage wearing a policy's name.
	if got := f.gateway.Connected(); got != 1 {
		t.Fatalf("gateway Connected() = %d after the refusal, want 1 — the refused Dial took the runner off the gateway", got)
	}

	// And the same machine takes the run that IS its pool's, so the refusal above is a placement
	// decision and not a broken gateway.
	own := f.attempt("run_pool_own", "att_pool_own", 2)
	own.PoolID = fleet.DefaultPoolID
	ownCtx, cancelOwn := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelOwn()
	ch, err := f.gateway.Dial(ownCtx, own)
	if err != nil {
		t.Fatalf("Dial for the runner's OWN pool: %v", err)
	}
	defer ch.Close()
	select {
	case lease := <-sandboxed.lease:
		if lease.RunID != own.RunID {
			t.Fatalf("runner was leased run %s, want %s", lease.RunID, own.RunID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the runner was never offered the run that belongs to its own pool")
	}
}

// TestASingleRunnerDeploymentWithNoPoolConfigurationIsBitUnchanged is §2's FIRST non-negotiable, and
// its name states the claim rather than describing the steps: today's compose stack, which configures
// no pool anywhere, must not be able to observe that pools were built.
//
// "Bit-unchanged" is taken literally rather than behaviourally. The lease offer the runner receives is
// compared BYTE FOR BYTE against the message the gateway produced before this task — rebuilt here from
// the pre-E24-T2 field set, with the received message's own timestamp so the only thing not compared
// is the clock. A behavioural assertion ("the runner still got a lease") would pass with a pool_id
// riding along in the offer, and a field the runner does not expect is exactly the kind of addition
// that breaks an older machine in the field.
//
// The enrolment side is the same claim from the other end: a runner that declares no posture sends the
// same request body it always sent (proved in packages/runner, where the encoder lives).
func TestASingleRunnerDeploymentWithNoPoolConfigurationIsBitUnchanged(t *testing.T) {
	f := newGatewayFixture(t, newOneUseTokens("unchanged-token"))
	// No registry: a stack with no database still enrols runners and serves leases, and that is the
	// posture the Docker-free tiers and every wire proof run in. The pool machinery must survive it.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	identity, err := runner.Enroll(ctx, f.bootstrap("unchanged-token"))
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}

	// The raw wire, so the OFFER'S BYTES are what is compared — a decoded runner.Lease would have
	// discarded any extra field before the assertion could see it.
	conn, _, err := websocket.Dial(ctx, f.sessionURL, &websocket.DialOptions{
		Subprotocols: []string{runner.RunnerProtocolV1},
		HTTPClient:   f.runnerHTTPClient(identity),
	})
	if err != nil {
		t.Fatalf("dial session: %v", err)
	}
	defer conn.CloseNow()
	hello, err := json.Marshal(contracts.RunnerMessage{
		Protocol: runner.RunnerProtocolV1, Type: "runner.hello",
		Time: time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("encode hello: %v", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, hello); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	waitConnected(t, f.gateway, 1)

	// An attempt with NO pool configuration: the field is left zero exactly as every caller in the
	// tree leaves it today.
	attempt := f.attempt("run_unchanged", "att_unchanged", 4)
	if attempt.PoolID != "" || !attempt.QueuedAt.IsZero() {
		t.Fatalf("the fixture attempt is pool-configured (%q / %s); this proof is about the UNCONFIGURED one", attempt.PoolID, attempt.QueuedAt)
	}
	ch, err := f.gateway.Dial(ctx, attempt)
	if err != nil {
		t.Fatalf("a deployment with no pool configuration could not dial: %v", err)
	}
	defer ch.Close()

	_, offered, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read lease offer: %v", err)
	}
	var received contracts.RunnerMessage
	if err := json.Unmarshal(offered, &received); err != nil {
		t.Fatalf("decode lease offer: %v", err)
	}
	// The pre-E24-T2 offer, verbatim: protocol/type/time/lease/run/attempt/fence and a data map of
	// exactly image_digest + limits (runner_gateway.go leaseOffer, before this task).
	want, err := json.Marshal(contracts.RunnerMessage{
		Protocol:  runner.RunnerProtocolV1,
		Type:      "lease.offer",
		Time:      received.Time,
		LeaseID:   "lease_" + string(attempt.AttemptID),
		RunID:     attempt.RunID,
		AttemptID: attempt.AttemptID,
		Fence:     int(attempt.Fence),
		Data: map[string]any{
			"image_digest": attempt.ImageDigest,
			"limits":       attempt.Limits,
		},
	})
	if err != nil {
		t.Fatalf("encode the pre-pool offer: %v", err)
	}
	if string(offered) != string(want) {
		t.Fatalf("the lease offer a pool-less deployment receives is no longer byte-identical.\n got: %s\nwant: %s", offered, want)
	}
}

// TestAPoolServesItsWaitingRunsOldestFirst is the ORDER, chosen explicitly and proved to be the chosen
// one. §3.6 D2 measured that the capability plane's queue is `ORDER BY job_id` over a crypto/rand hex
// id — not FIFO, not any order a person would recognise, and re-decided on every poll. So FIFO is not
// a behaviour this task preserves; it is a decision this task takes, and the reason is that a run a
// human is waiting on must not be overtaken because another run's identifier sorted lower.
//
// The proof turns the two plausible WRONG orders into the test's own inputs: the three attempts arrive
// at the gateway in one order, carry ids that sort in a second, and carry run timestamps in a third.
// Only the timestamps decide. An implementation that leaned on Go's channel receive order (arrival) or
// on an id comparison fails it.
func TestAPoolServesItsWaitingRunsOldestFirst(t *testing.T) {
	f := newGatewayFixture(t, newOneUseTokens("fifo-token-a", "fifo-token-b", "fifo-token-c"))
	base := time.Now().Add(-time.Hour)

	type waiting struct {
		name     string
		queuedAt time.Time
	}
	// Arrival order: c, a, b. Id order: att_fifo_a, att_fifo_b, att_fifo_c. Age order: b, c, a.
	arrivals := []waiting{
		{name: "c", queuedAt: base.Add(2 * time.Minute)},
		{name: "a", queuedAt: base.Add(5 * time.Minute)},
		{name: "b", queuedAt: base.Add(1 * time.Minute)},
	}
	wantOrder := []string{"b", "c", "a"}

	served := make(chan string, len(arrivals))
	dialErr := make(chan error, len(arrivals))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for i, w := range arrivals {
		attempt := f.attempt("run_fifo_"+w.name, "att_fifo_"+w.name, 1)
		attempt.QueuedAt = w.queuedAt
		go func(name string, attempt execution.AttemptDescriptor) {
			ch, err := f.gateway.Dial(ctx, attempt)
			if err != nil {
				dialErr <- err
				return
			}
			served <- name
			_ = ch.Close()
		}(w.name, attempt)
		// Each Dial is fully registered as waiting before the next one arrives, so "arrival order" is
		// a fact of this test and not a race it happens to win.
		f.waitWaiting(t, fleet.DefaultPoolID, i+1)
	}

	// One machine, serving one run at a time: each lease is closed before the next runner parks, so the
	// pool hands its waiting runs out strictly in sequence.
	for i := range arrivals {
		p := park(t, f, "fifo-token-"+arrivals[i].name)
		select {
		case <-p.lease:
		case err := <-dialErr:
			t.Fatalf("a waiting Dial failed: %v", err)
		case <-time.After(10 * time.Second):
			t.Fatalf("no waiting run was served after runner %d parked", i+1)
		}
	}

	got := make([]string, 0, len(arrivals))
	for range arrivals {
		select {
		case name := <-served:
			got = append(got, name)
		case err := <-dialErr:
			t.Fatalf("a waiting Dial failed: %v", err)
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d waiting runs were served: %v", len(got), len(arrivals), got)
		}
	}
	for i := range wantOrder {
		if got[i] != wantOrder[i] {
			t.Fatalf("the pool served its waiting runs in order %v, want oldest-first %v — the order is neither arrival nor id, it is the run's queued-at",
				got, wantOrder)
		}
	}
}

// TestDialRefusesAPoolWhoseRunnerNeverComesRatherThanTakingAnother pins the negative half of the
// per-pool queue: waiting on an EMPTY pool is waiting, not borrowing. It is separate from the posture
// proof above because it holds for two pools of the SAME posture — the pool is the queue, so a machine
// in one is not a machine in the other whatever the two have in common.
func TestDialRefusesAPoolWhoseRunnerNeverComesRatherThanTakingAnother(t *testing.T) {
	f := newGatewayFixture(t, newOneUseTokens("empty-token"))
	f.gateway.SetRegistry(newFakeRegistry("pool_second"))
	occupied := park(t, f, "empty-token")
	waitConnected(t, f.gateway, 1)

	attempt := f.attempt("run_empty", "att_empty", 1)
	attempt.PoolID = "pool_second"
	dialCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ch, err := f.gateway.Dial(dialCtx, attempt)
	if err == nil {
		_ = ch.Close()
		t.Fatal("a Dial for an empty pool was served by another pool's runner")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Dial on an empty pool = %v, want the wait to expire (T4 turns this wait into a park)", err)
	}
	select {
	case lease := <-occupied.lease:
		t.Fatalf("the other pool's runner was leased %s", lease.LeaseID)
	default:
	}
}
