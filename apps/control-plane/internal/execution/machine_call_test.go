package execution_test

// Addressing a machine that holds NO LEASE, over the shipped gateway and a real packages/runner
// session. This is the capability A.3 T7 found missing: every message the control plane could send a
// runner rode an attempt's lease, and a background probe happens when there is no attempt.
//
// The fake here is the machine's BACKGROUND EXECUTOR and nothing else — the wire, the gateway, the
// websocket and the runner's own relay are all the shipped ones. A stand-in for the transport would be
// a stand-in for the exact thing that was broken.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/runner"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// recordingBackground is the machine's detached executor. It answers for the handles this machine
// started, which is the whole of the fidelity: a probe reaches this kernel and no other.
type recordingBackground struct {
	probed []toolbroker.Handle
	status toolbroker.BackgroundStatus
	killed []toolbroker.Handle
}

func (b *recordingBackground) Start(context.Context, toolbroker.ShellCommand, toolbroker.BackgroundSpec) (toolbroker.Handle, error) {
	return toolbroker.Handle{Posture: toolbroker.PostureUnsandboxedHost, Value: "4242:1700000000"}, nil
}

func (b *recordingBackground) Probe(_ context.Context, h toolbroker.Handle) (toolbroker.BackgroundStatus, error) {
	b.probed = append(b.probed, h)
	return b.status, nil
}

func (b *recordingBackground) Kill(_ context.Context, h toolbroker.Handle) error {
	b.killed = append(b.killed, h)
	return nil
}

// parkedMachine enrols a real runner and leaves it PARKED — it opens a lease session and never gets a
// lease, which is the state a machine spends most of its life in and the state a background probe has
// to work in.
// It returns the id the GATEWAY minted, which is not the one the fixture chose: the identity is derived
// from the certificate SAN (runnerIDFromDNS), never from anything the connecting party said. Asserting
// against a hand-written constant would be asserting against a claim rather than an identity.
func parkedMachine(t *testing.T, ctx context.Context, token string, background toolbroker.BackgroundRunner) (*gatewayFixture, string) {
	t.Helper()
	f := newGatewayFixture(t, newOneUseTokens(token))
	identity, err := runner.Enroll(ctx, f.bootstrap(token))
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		session := f.session(identity)
		session.Background = runner.NewBackgroundServer(background)
		lease, err := session.OpenLease(ctx)
		if err != nil {
			return
		}
		defer lease.Close()
		inbound := make(chan contracts.EngineFrame, 4)
		go func() {
			for range inbound {
			}
		}()
		runner.RelayInboundWithBackground(ctx, lease, runner.NewToolServer(nil), runner.NewBackgroundServer(background), inbound, func(string, ...any) {})
	}()
	t.Cleanup(func() {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})
	// The machine has to be CONNECTED before it can be addressed; the session count is the gateway's own
	// answer to that, and waiting on it makes the test about routing rather than about a race.
	for i := 0; i < 300 && f.gateway.Connected() == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if f.gateway.Connected() == 0 {
		t.Fatal("the machine never connected, so nothing below is about addressing")
	}
	seen, ok := f.gateway.LastRunnerIdentity()
	if !ok {
		t.Fatal("the gateway saw no runner certificate, so it has no machine to address")
	}
	// The id is the SAN with the internal zone stripped, which is what runnerDNS/runnerIDFromDNS do
	// inside the package. Mirroring it here rather than hard-coding a name keeps this test asserting
	// against the identity the gateway MINTED — and if that derivation ever changes, this fails loudly
	// instead of addressing a machine that is not there.
	machineID := strings.TrimSuffix(seen.RunnerDNS, ".runners.palai.internal")
	if machineID == seen.RunnerDNS {
		t.Fatalf("the certificate SAN %q does not carry the internal zone, so this fixture no longer "+
			"derives the id the way the gateway does", seen.RunnerDNS)
	}
	return f, machineID
}

// TestAParkedMachineAnswersABackgroundProbe is the capability itself. No attempt exists, no lease was
// dialled, and the control plane still reaches the machine by name and gets its kernel's answer.
func TestAParkedMachineAnswersABackgroundProbe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	machine := &recordingBackground{status: toolbroker.BackgroundStatus{State: toolbroker.BackgroundRunning}}
	f, machineID := parkedMachine(t, ctx, "mc-token-probe", machine)

	handle := toolbroker.Handle{Posture: toolbroker.PostureUnsandboxedHost, Value: "4242:1700000000", MachineID: machineID}
	answer, err := f.gateway.CallMachine(ctx, machineID, runner.BackgroundProbeType, map[string]any{"handle": handle})
	if err != nil {
		t.Fatalf("CallMachine(bg.probe): %v — a parked machine is the state a background probe always finds", err)
	}
	status, ok := answer["status"].(map[string]any)
	if !ok {
		t.Fatalf("the answer carried no status: %+v", answer)
	}
	if got, _ := status["State"].(string); got != string(toolbroker.BackgroundRunning) {
		t.Fatalf("probe state = %q, want %q", got, toolbroker.BackgroundRunning)
	}
	// The handle reached the machine INTACT — the probe is only meaningful if the machine looked at the
	// process group the row names.
	if len(machine.probed) != 1 || machine.probed[0].Value != handle.Value || machine.probed[0].MachineID != handle.MachineID {
		t.Fatalf("the machine probed %+v, want exactly the handle that was sent", machine.probed)
	}
}

// TestACordonedMachineStillAnswers is the first lifecycle rule, and it is the one that would be easy to
// get backwards. A cordon stops new PLACEMENT, not running work — a machine is cordoned precisely so it
// can be emptied safely, and a gateway that went silent here would orphan every background task on the
// machine an operator is trying to drain.
func TestACordonedMachineStillAnswers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	machine := &recordingBackground{status: toolbroker.BackgroundStatus{State: toolbroker.BackgroundRunning}}
	f, machineID := parkedMachine(t, ctx, "mc-token-cordon", machine)
	f.gateway.CordonRunner(machineID)

	handle := toolbroker.Handle{Posture: toolbroker.PostureUnsandboxedHost, Value: "4242:1700000000", MachineID: machineID}
	if _, err := f.gateway.CallMachine(ctx, machineID, runner.BackgroundProbeType, map[string]any{"handle": handle}); err != nil {
		t.Fatalf("a CORDONED machine refused a probe (%v): cordon stops placement, not running work — "+
			"going silent here orphans every task on the machine an operator is draining", err)
	}
}

// TestARevokedMachineIsUnreachable is the second rule and the opposite one. Revoke is the hard stop
// (SAN-011): the machine is no longer ours. ErrMachineUnreachable is what the caller turns into `lost`,
// which already means "the process may still be running and we cannot prove it is ours" — and which
// carries the rule that a lost handle is never signalled.
func TestARevokedMachineIsUnreachable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	machine := &recordingBackground{status: toolbroker.BackgroundStatus{State: toolbroker.BackgroundRunning}}
	f, machineID := parkedMachine(t, ctx, "mc-token-revoke", machine)
	f.gateway.RevokeRunner(machineID)

	handle := toolbroker.Handle{Posture: toolbroker.PostureUnsandboxedHost, Value: "4242:1700000000", MachineID: machineID}
	_, err := f.gateway.CallMachine(ctx, machineID, runner.BackgroundKillType, map[string]any{"handle": handle})
	if err == nil {
		t.Fatal("a REVOKED machine was signalled: it is no longer ours, and the process the pgid names may be anybody's")
	}
	if !errors.Is(err, execution.ErrMachineUnreachable) {
		t.Fatalf("revoked machine error = %v, want ErrMachineUnreachable so the caller reports lost", err)
	}
	if len(machine.killed) != 0 {
		t.Fatalf("the machine was signalled anyway: %+v", machine.killed)
	}
}

// TestAnUnknownMachineIsUnreachableRatherThanASilentWait is the answer for a machine that is simply not
// here — powered off, or never enrolled. A caller that hung instead would park a reconciler sweep on a
// row nothing can settle.
func TestAnUnknownMachineIsUnreachableRatherThanASilentWait(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f, _ := parkedMachine(t, ctx, "mc-token-unknown", &recordingBackground{})

	_, err := f.gateway.CallMachine(ctx, "rnr_a_machine_that_is_not_here", runner.BackgroundProbeType,
		map[string]any{"handle": toolbroker.Handle{Posture: toolbroker.PostureUnsandboxedHost, Value: "1:1"}})
	if !errors.Is(err, execution.ErrMachineUnreachable) {
		t.Fatalf("unknown machine error = %v, want ErrMachineUnreachable", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("the call waited for a machine that is not here rather than saying so")
	}
}

// TestAMachineWithNoBackgroundExecutorRefusesRatherThanFallingSilent is the wire's own discipline,
// inherited from the exec pair: every request produces a message. A machine that cannot run background
// tasks says so, and the control plane learns it will get no result instead of blocking a sweep.
func TestAMachineWithNoBackgroundExecutorRefusesRatherThanFallingSilent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f, machineID := parkedMachine(t, ctx, "mc-token-unwired", nil) // no background executor on this machine

	_, err := f.gateway.CallMachine(ctx, machineID, runner.BackgroundProbeType,
		map[string]any{"handle": toolbroker.Handle{Posture: toolbroker.PostureUnsandboxedHost, Value: "1:1"}})
	if err == nil {
		t.Fatal("a machine with no background executor answered as though it had one")
	}
	if !strings.Contains(err.Error(), "not wired with a background executor") {
		t.Fatalf("error = %v, want the machine's own refusal", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want the refusal rather than a timeout: silence would block a sweep forever", err)
	}
}
