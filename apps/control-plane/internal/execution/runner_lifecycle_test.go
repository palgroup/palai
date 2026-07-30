package execution_test

// THE MACHINE'S OWN LIFECYCLE (E24 T5), over the real mTLS wire, Docker-free.
//
// §3.6 D15 measured two things and the second is worse than the first. The first: cordon, drain and
// revoke were WHOLE-GATEWAY `atomic.Bool`s, so "take that Mac out of service" took every Mac out of
// service. The second: `Revoke()` and `Resume()` had NO PRODUCTION CALLER ANYWHERE IN THE TREE, and
// `Cordon()`'s only caller was `Drain`'s own first line, whose only caller is SIGTERM — so SAN-011's
// hard stop, written for a compromised runner, was proved by tests and reachable by nobody.
//
// These proofs are here rather than only in the component tier because they need no database: the
// gateway mints a distinct id per enrolment whether or not a registry is wired, so two file-token
// machines are two identities and the whole per-runner claim is stateable on the wire alone. The
// DURABILITY half cannot be — a restart is a database question — and lives in
// runner_lifecycle_component_test.go.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/runner"
)

// runnerIDOf reads the SERVER-minted id out of the identity the gateway issued. The certificate's SAN
// carries it (`rnr_….runners.palai.internal`), which is the only place a test can see it without the
// registry — and it is the same string every lifecycle surface names.
func runnerIDOf(t *testing.T, identity runner.Identity) string {
	t.Helper()
	leaf := identity.Certificate.Leaf
	if leaf == nil || len(leaf.DNSNames) == 0 {
		t.Fatal("the enrolled identity carries no certificate SAN, so nothing can name this machine")
	}
	return strings.TrimSuffix(leaf.DNSNames[0], ".runners.palai.internal")
}

// TestLifecycleCordonsOneMachineAndLeavesTheOtherServing is RED (1).
//
// Two machines are parked on ONE queue. Cordoning one must stop that one being offered new leases and
// must leave the other serving. Today `cordoned` is a process-global `atomic.Bool`, so the first
// assertion below — that the OTHER machine still takes a lease — fails: the gateway refuses every Dial.
//
// The proof is two-sided on purpose. A build that ignored the cordon entirely would satisfy "the other
// one still serves", so the cordoned machine is asserted to receive NO lease across a full dial budget
// in which it is the only machine left parked.
func TestLifecycleCordonsOneMachineAndLeavesTheOtherServing(t *testing.T) {
	f := newGatewayFixture(t, newOneUseTokens("lifecycle-token-a", "lifecycle-token-b"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	machineA := park(t, f, "lifecycle-token-a")
	machineB := park(t, f, "lifecycle-token-b")
	waitConnected(t, f.gateway, 2)
	idA, idB := runnerIDOf(t, machineA.identity), runnerIDOf(t, machineB.identity)
	if idA == idB {
		t.Fatalf("both machines enrolled as %q: two machines with one identity cannot be cordoned apart", idA)
	}

	// Cordon A. B must still be leasable.
	f.gateway.CordonRunner(idA)

	leaseCtx, cancelLease := context.WithTimeout(ctx, 10*time.Second)
	defer cancelLease()
	ch, err := f.gateway.Dial(leaseCtx, f.attempt("run_lifecycle_b", "att_lifecycle_b", 1))
	if err != nil {
		t.Fatalf("Dial after cordoning ONE machine = %v, want a lease on the other: cordon is whole-gateway today, so taking one Mac out of service takes every Mac out of service (§3.6 D15)", err)
	}
	select {
	case <-machineB.lease:
	case lease := <-machineA.lease:
		t.Fatalf("the CORDONED machine was offered lease %s", lease.LeaseID)
	case <-time.After(10 * time.Second):
		t.Fatal("neither machine was offered the lease the gateway said it granted")
	}
	_ = ch.Close()

	// And now the other side: A is the only machine parked, and a full dial budget must expire without
	// it being offered anything.
	starvedCtx, cancelStarved := context.WithTimeout(ctx, 2*time.Second)
	defer cancelStarved()
	if got, err := f.gateway.Dial(starvedCtx, f.attempt("run_lifecycle_a", "att_lifecycle_a", 2)); err == nil {
		_ = got.Close()
		t.Fatal("a cordoned machine was offered a NEW lease: cordon stops new work or it stops nothing")
	}
	select {
	case lease := <-machineA.lease:
		t.Fatalf("the cordoned machine received lease %s", lease.LeaseID)
	default:
	}
	// A cordon is not an outage: the machine is still connected, waiting to be resumed.
	if got := f.gateway.Connected(); got == 0 {
		t.Fatal("the cordoned machine's session was dropped — a cordon that drops the connection is an outage wearing a policy's name")
	}

	// Resume puts it back in the rendezvous, which is the half that makes the cordon reversible.
	f.gateway.ResumeRunner(idA)
	resumeCtx, cancelResume := context.WithTimeout(ctx, 10*time.Second)
	defer cancelResume()
	resumed, err := f.gateway.Dial(resumeCtx, f.attempt("run_lifecycle_resumed", "att_lifecycle_resumed", 3))
	if err != nil {
		t.Fatalf("Dial after resuming the cordoned machine = %v, want a lease", err)
	}
	defer resumed.Close()
	select {
	case <-machineA.lease:
	case <-time.After(10 * time.Second):
		t.Fatal("the resumed machine was never offered a lease: a cordon nothing can undo is a decommission")
	}
}

// TestLifecycleWholeGatewayDrainIsUnchanged is the §T5 non-negotiable stated as a test rather than as a
// comment: E15 T2's drain is KEPT. A control-plane swap must still drain EVERYTHING, so `Drain` cordons
// the whole gateway, blocks while ANY machine holds a lease, returns `ctx.Err()` when it cannot finish,
// and returns nil once the last lease closes — with the per-runner counter underneath it.
//
// It exists because the counter changed shape (`atomic.Int64` -> one per machine) and a sum is exactly
// the kind of refactor that quietly stops counting: a `Drain` that read one machine's counter would
// return nil while another machine was mid-lease, which is a control plane that exits during a run.
func TestLifecycleWholeGatewayDrainIsUnchanged(t *testing.T) {
	f := newGatewayFixture(t, newOneUseTokens("drain-token-a", "drain-token-b"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	park(t, f, "drain-token-a")
	park(t, f, "drain-token-b")
	waitConnected(t, f.gateway, 2)

	// A parked-and-idle fleet must not block a drain: cordon stops new leases, drain waits for work.
	idleCtx, cancelIdle := context.WithTimeout(ctx, 2*time.Second)
	defer cancelIdle()
	if err := f.gateway.Drain(idleCtx); err != nil {
		t.Fatalf("Drain with two parked-idle machines = %v, want nil", err)
	}
	f.gateway.Resume()

	// One machine takes a lease. The whole-gateway drain must wait for it.
	leaseCtx, cancelLease := context.WithTimeout(ctx, 10*time.Second)
	defer cancelLease()
	ch, err := f.gateway.Dial(leaseCtx, f.attempt("run_drain", "att_drain", 1))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	timedCtx, cancelTimed := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancelTimed()
	if err := f.gateway.Drain(timedCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Drain while a lease is in flight = %v, want DeadlineExceeded — a drain that stops counting a machine's leases is a control plane that exits mid-run", err)
	}
	if !f.gateway.Cordoned() {
		t.Fatal("Drain did not cordon the whole gateway")
	}
	_ = ch.Close()
	drained := make(chan error, 1)
	go func() { drained <- f.gateway.Drain(ctx) }()
	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("Drain after the last lease closed = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Drain did not return after the in-flight lease closed")
	}
}

// TestLifecycleRevokeCutsOneMachineAndSparesTheOther is the per-runner half of SAN-011: revoking ONE
// machine cuts ITS session and leaves every other machine serving.
//
// Revoke is IRREVERSIBLE and that is today's semantics unchanged (`runner_gateway.go:153`) — so the
// last assertion is that Resume does NOT bring it back.
func TestLifecycleRevokeCutsOneMachineAndSparesTheOther(t *testing.T) {
	f := newGatewayFixture(t, newOneUseTokens("revoke-token-a", "revoke-token-b"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	machineA := park(t, f, "revoke-token-a")
	machineB := park(t, f, "revoke-token-b")
	waitConnected(t, f.gateway, 2)
	idA := runnerIDOf(t, machineA.identity)

	f.gateway.RevokeRunner(idA)
	// The revoked machine's SESSION is cut — the hard stop cordon is not — so the connected count drops
	// to the one machine that is left.
	waitConnected(t, f.gateway, 1)

	leaseCtx, cancelLease := context.WithTimeout(ctx, 10*time.Second)
	defer cancelLease()
	ch, err := f.gateway.Dial(leaseCtx, f.attempt("run_revoke_b", "att_revoke_b", 1))
	if err != nil {
		t.Fatalf("Dial after revoking ONE machine = %v, want a lease on the other", err)
	}
	defer ch.Close()
	select {
	case <-machineB.lease:
	case <-time.After(10 * time.Second):
		t.Fatal("the surviving machine was never offered a lease: a revoke that stops the fleet is not targeted")
	}

	// A revoked machine cannot come back by reconnecting, and Resume does not un-revoke it.
	f.gateway.ResumeRunner(idA)
	reconnectCtx, cancelReconnect := context.WithTimeout(ctx, 3*time.Second)
	defer cancelReconnect()
	if _, err := f.session(machineA.identity).ReceiveLease(reconnectCtx); err == nil {
		t.Fatal("a revoked machine reconnected after Resume: a revoked runner identity is decommissioned, not paused")
	}
}

// TestLifecycleReapCutsAMachineThatStoppedAnswering is the reaper's live half. A machine whose
// connection is alive to the kernel but dead to the process — a suspended laptop, an unplugged Mac, a
// wedged runner — holds a queue slot and will be handed the next lease, which then never completes.
// Nothing before this task noticed: `readLoop` learns of a disconnect from a read ERROR, and a peer
// that has stopped answering without closing produces none.
//
// The dead machine here is a raw WebSocket client that completes the handshake and then NEVER READS.
// coder/websocket answers a ping from inside its own read loop, so a client that is not reading cannot
// answer one — which is the same observable as a machine that is gone.
//
// The assertion is on the CONSEQUENCE rather than on a state word: after the cut the pool has no
// machine, so a Dial answers ErrPoolHasNoRunner — "there is nothing here" — instead of handing a run to
// a corpse. That is also the whole of "the pool's healthy count is decremented": the count that decides
// whether a run parks or rides the retry ladder is the gateway's own membership, and the cut drops it.
func TestLifecycleReapCutsAMachineThatStoppedAnswering(t *testing.T) {
	f := newGatewayFixture(t, newOneUseTokens("reap-token"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	identity, err := runner.Enroll(ctx, f.bootstrap("reap-token"))
	if err != nil {
		t.Fatalf("enrol: %v", err)
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{identity.Certificate},
		RootCAs:      f.ca.pool,
		ServerName:   gwControllerDNS,
	}}}
	conn, _, err := websocket.Dial(ctx, f.sessionURL, &websocket.DialOptions{
		HTTPClient:   client,
		Subprotocols: []string{runner.RunnerProtocolV1},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()
	hello, _ := json.Marshal(contracts.RunnerMessage{
		Protocol: runner.RunnerProtocolV1, Type: "runner.hello",
		Time: time.Now().UTC().Format(time.RFC3339),
	})
	if err := conn.Write(ctx, websocket.MessageText, hello); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	waitConnected(t, f.gateway, 1)

	// This machine is parked and will never read again, so the ping cannot come back and the reaper must
	// cut it. A short timeout keeps the proof quick; production's is seconds.
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, cut := f.gateway.Heartbeat(ctx, 250*time.Millisecond); cut == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the reaper never cut a machine that stopped answering: a half-open session keeps its queue slot and is handed the next lease, which then never completes")
		}
		time.Sleep(100 * time.Millisecond)
	}
	waitConnected(t, f.gateway, 0)

	starvedCtx, cancelStarved := context.WithTimeout(ctx, 2*time.Second)
	defer cancelStarved()
	if _, err := f.gateway.Dial(starvedCtx, f.attempt("run_reaped", "att_reaped", 1)); !errors.Is(err, execution.ErrPoolHasNoRunner) {
		t.Fatalf("Dial after the reaper cut the pool's only machine = %v, want ErrPoolHasNoRunner: a pool whose machines are all dead is an ABSENT pool, and a run must park rather than be handed to a corpse", err)
	}
}

// TestLifecycleHeartbeatKeepsALiveMachine is the non-vacuity half of the test above, and it is not
// decoration: a reaper that cut EVERY session would satisfy every assertion there. A machine whose
// runner is reading its connection answers the ping, is reported alive, keeps its session and is still
// leasable afterwards.
func TestLifecycleHeartbeatKeepsALiveMachine(t *testing.T) {
	f := newGatewayFixture(t, newOneUseTokens("heartbeat-token"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	machine := park(t, f, "heartbeat-token")
	waitConnected(t, f.gateway, 1)

	for i := 0; i < 3; i++ {
		alive, cut := f.gateway.Heartbeat(ctx, 5*time.Second)
		if alive != 1 || cut != 0 {
			t.Fatalf("Heartbeat pass %d over one healthy machine = (%d alive, %d cut), want (1, 0)", i+1, alive, cut)
		}
	}
	if got := f.gateway.Connected(); got != 1 {
		t.Fatalf("Connected() = %d after three heartbeats, want 1", got)
	}
	leaseCtx, cancelLease := context.WithTimeout(ctx, 10*time.Second)
	defer cancelLease()
	ch, err := f.gateway.Dial(leaseCtx, f.attempt("run_heartbeat", "att_heartbeat", 1))
	if err != nil {
		t.Fatalf("Dial after three heartbeats = %v, want a lease", err)
	}
	defer ch.Close()
	select {
	case <-machine.lease:
	case <-time.After(10 * time.Second):
		t.Fatal("a heartbeaten machine was never offered a lease")
	}
}
