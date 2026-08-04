package execution

// Where a command runs is a property of the ATTEMPT, not of the process. Until A.3 the orchestrator
// held one toolbroker.ShellRunner for its whole life and handed it to every attempt, so "this run on a
// Mac, that one in a container" could not be said at all — the second run got the first run's executor
// because there was only ever one.
//
// These tests drive execEnv, which is the single place a tool call learns which executor it has.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/runner"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// machineChannel is an attempt's connection to a machine that answers commands. It implements BOTH
// shipped interfaces — EngineChannel, because that is what an attempt holds, and ExecConn, because
// that is what makes the connection reach a machine — rather than a narrow invention of its own. A
// stand-in for only the second would prove nothing about the value the orchestrator actually has.
type machineChannel struct {
	answer toolbroker.ShellResult
	ran    []toolbroker.ShellCommand
}

func (c *machineChannel) Send(context.Context, contracts.EngineFrame) error { return nil }
func (c *machineChannel) Receive(context.Context) (contracts.EngineFrame, error) {
	return contracts.EngineFrame{}, io.EOF
}
func (c *machineChannel) Close() error { return nil }

func (c *machineChannel) StartExec(_ context.Context, _ string, cmd toolbroker.ShellCommand) (<-chan ExecAnswer, func(), error) {
	c.ran = append(c.ran, cmd)
	answers := make(chan ExecAnswer, 1)
	answers <- ExecAnswer{Result: c.answer}
	return answers, func() {}, nil
}

// hostMachineChannel is a test double for a machine whose executor happens to be in this process. It is
// how a component test wires an executor now: through the attempt's CONNECTION, the way production's
// arrives, rather than through a field on the orchestrator that every attempt shared.
//
// The embedded EngineChannel is whatever engine the test was already driving, so wrapping one changes
// nothing about the frames it scripts. It may be nil for a state that is only ever handed to execEnv.
type hostMachineChannel struct {
	EngineChannel
	exec toolbroker.ShellRunner
	// machineID is what makes this a MACHINE rather than an executor (A.3 T7). Until T7 an attempt only
	// had to say WHAT runs its commands; now it must also say WHERE, because a background task is started
	// by the machine and its row records that machine so a reconciler holding no lease can address the
	// right kernel later.
	//
	// EMPTY IS A MACHINELESS ATTEMPT AND STAYS EXPRESSIBLE. A fixture that leaves it unset gets an attempt
	// that can run a command and cannot leave a process behind — a real deployment shape, and the one
	// StartBackground's gate refuses.
	machineID string
}

// MachineID makes the double satisfy the interface production's gatewayChannel satisfies, so machineOf
// reads a machine off it the same way it reads one off a lease.
func (c hostMachineChannel) MachineID() string { return c.machineID }

var _ MachineIdentified = hostMachineChannel{}

func (c hostMachineChannel) StartExec(ctx context.Context, _ string, cmd toolbroker.ShellCommand) (<-chan ExecAnswer, func(), error) {
	answers := make(chan ExecAnswer, 1)
	// On its own goroutine, matching the real transport: RemoteShell selects on this channel and its
	// context, and a command that runs inline here could not be given up on.
	go func() {
		result, err := c.exec.Run(ctx, cmd)
		answers <- ExecAnswer{Result: result, Err: err}
	}()
	return answers, func() {}, nil
}

// hostMachine is the DETACHED half of the same double hostMachineChannel is the synchronous half of: a
// machine whose kernel happens to be this process's. A.3 T7 moved the spawn itself onto the machine, so
// a fixture that drives a background task now has to be able to answer the bg.* triple — before T7 the
// orchestrator held one process-wide BackgroundRunner and every fixture got it by construction.
//
// THE SHIPPED SERVER ANSWERS. A hand-written switch over the three verbs would re-implement, inside the
// test, the exact decode the machine ships (runner.BackgroundServer) — and a stand-in for the decode is a
// stand-in for the thing most likely to be wrong about a value that crossed a wire.
//
// AND THE PAYLOAD MAKES THE JSON CROSSING IT MAKES IN PRODUCTION. RunnerGateway.CallMachine marshals the
// request onto a websocket and the runner unmarshals it, so every value reaches Handle as the map a
// decoder produced. Handing the Go structs straight over would prove a round trip that never happens.
type hostMachine struct {
	id     string
	server *runner.BackgroundServer
}

// newHostMachine binds one machine id to the executor that answers for it. A nil runner is a machine
// that cannot detach; it still answers every request, with a refusal — which is what BackgroundServer
// does and what a posture without a detached executor looks like from the control plane.
func newHostMachine(id string, bg toolbroker.BackgroundRunner) *hostMachine {
	return &hostMachine{id: id, server: runner.NewBackgroundServer(bg)}
}

var _ MachineCaller = (*hostMachine)(nil)

// CallMachine answers for THIS machine and refuses every other name the way the gateway refuses it —
// ErrMachineUnreachable, which is what a powered-off, revoked or never-seen machine looks like from a
// control plane. A fleet of one is still a fleet, and the refusal is the half that keeps the routing
// honest: a fixture whose attempt names the wrong machine must not be answered by the right one.
func (m *hostMachine) CallMachine(ctx context.Context, machineID, verb string, payload map[string]any) (map[string]any, error) {
	if machineID != m.id {
		return nil, fmt.Errorf("%w: machine %s has no live session", ErrMachineUnreachable, machineID)
	}
	request, err := overTheWire(contracts.RunnerMessage{
		Protocol: runner.RunnerProtocolV1,
		Type:     verb,
		Data:     runner.BackgroundRequestData(newExecID("bg"), payload),
	})
	if err != nil {
		return nil, err
	}
	answer, err := overTheWire(m.server.Handle(ctx, request))
	if err != nil {
		return nil, err
	}
	// A REFUSAL IS NOT AN OUTCOME, and the gateway draws that line in machineRefusal rather than in its
	// callers. THE SHIPPED DECODER IS CALLED HERE, not re-written: collapsing a refusal into an outcome
	// would let "this machine runs no background task" read as "the task is gone", and flattening its kind
	// would lose the one branch a lost handle depends on — two mistakes a hand-written copy in a fixture
	// would be free to make while the real one stayed correct.
	if refusal := machineRefusal(answer.Data); refusal != nil {
		return nil, refusal
	}
	return answer.Data, nil
}

// overTheWire is the JSON crossing itself, so a value that cannot survive it fails in a fixture rather
// than on a real machine.
func overTheWire(m contracts.RunnerMessage) (contracts.RunnerMessage, error) {
	encoded, err := json.Marshal(m)
	if err != nil {
		return contracts.RunnerMessage{}, fmt.Errorf("encode %s: %w", m.Type, err)
	}
	var decoded contracts.RunnerMessage
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return contracts.RunnerMessage{}, fmt.Errorf("decode %s: %w", m.Type, err)
	}
	return decoded, nil
}

// engineOnlyChannel is an attempt whose connection reaches an ENGINE and no machine — the shape every
// channel in this tree had before A.3, and the shape a bare subprocess dialer still has. It carries no
// StartExec, deliberately.
type engineOnlyChannel struct{}

func (engineOnlyChannel) Send(context.Context, contracts.EngineFrame) error { return nil }
func (engineOnlyChannel) Receive(context.Context) (contracts.EngineFrame, error) {
	return contracts.EngineFrame{}, io.EOF
}
func (engineOnlyChannel) Close() error { return nil }

// TestTwoAttemptsOnTwoMachinesGetTwoAnswers IS A.3, stated as one assertion: one orchestrator, two
// attempts, and each one reaches its OWN machine. Before this task both lines below returned whatever
// single executor the process was started with, so the second assertion is the one that names the
// defect.
func TestTwoAttemptsOnTwoMachinesGetTwoAnswers(t *testing.T) {
	o := &Orchestrator{}

	mac := &machineChannel{answer: toolbroker.ShellResult{Stdout: "Darwin\n"}}
	linux := &machineChannel{answer: toolbroker.ShellResult{Stdout: "Linux\n"}}

	macEnv := o.execEnv(&attemptState{attempt: AttemptDescriptor{PoolID: "pool_mac"}, ch: mac})
	linuxEnv := o.execEnv(&attemptState{attempt: AttemptDescriptor{PoolID: "pool_linux"}, ch: linux})

	if macEnv.Shell == nil || linuxEnv.Shell == nil {
		t.Fatal("an attempt whose channel reaches a machine got no shell runner at all")
	}

	uname := toolbroker.ShellCommand{Argv: []string{"uname", "-s"}}
	macOut, err := macEnv.Shell.Run(context.Background(), uname)
	if err != nil {
		t.Fatalf("mac attempt: %v", err)
	}
	linuxOut, err := linuxEnv.Shell.Run(context.Background(), uname)
	if err != nil {
		t.Fatalf("linux attempt: %v", err)
	}

	if macOut.Stdout != "Darwin\n" {
		t.Errorf("the mac attempt got %q, want its own machine's answer", macOut.Stdout)
	}
	if linuxOut.Stdout != "Linux\n" {
		t.Errorf("the linux attempt got %q — ONE EXECUTOR IS STILL SHARED between attempts", linuxOut.Stdout)
	}

	// Each command reached exactly one machine: a shared executor would have collected both.
	if len(mac.ran) != 1 || len(linux.ran) != 1 {
		t.Fatalf("commands reached the machines %d/%d times, want 1 each", len(mac.ran), len(linux.ran))
	}
}

// TestAnAttemptWithNoMachineGetsNoShellAndTheToolRefuses is the half that would be easy to lose. An
// attempt whose channel reaches no machine must NOT fall back to the control plane's own host: a
// command running silently beside the control plane is exactly the behaviour this epic removes, and it
// is worse than a refusal because nothing in the run says where it ran.
//
// It drives the SHIPPED ShellTool rather than checking a nil field, because "fails cleanly" is a claim
// about what the model is told: an unavailable answer lets the run continue, an error would abort the
// attempt as uncertain.
func TestAnAttemptWithNoMachineGetsNoShellAndTheToolRefuses(t *testing.T) {
	o := &Orchestrator{}

	// THE CONTROL, so the refusal below is attributable. The same orchestrator hands a shell to an
	// attempt whose channel reaches a machine — without this line a nil Shell would prove only that
	// nothing was wired anywhere, which was true before this task for every attempt.
	if withMachine := o.execEnv(&attemptState{ch: &machineChannel{}}); withMachine.Shell == nil {
		t.Fatal("this orchestrator hands no shell to ANY attempt, so the refusal below proves nothing")
	}

	env := o.execEnv(&attemptState{
		attempt: AttemptDescriptor{WorkspaceHostPath: t.TempDir()},
		ch:      engineOnlyChannel{},
	})

	if env.Shell != nil {
		t.Fatalf("an attempt with no machine was handed a shell runner (%T) — its commands would run "+
			"beside the control plane", env.Shell)
	}

	_, err := tools.ShellTool().Exec(context.Background(), env, map[string]any{
		"argv": []any{"touch", "this-must-never-exist"},
	})
	if err == nil {
		t.Fatal("the shell tool succeeded on an attempt with no machine")
	}
	answer, ok := toolbroker.AsAnswer(err)
	if !ok || answer.Code != toolbroker.AnswerUnavailable {
		t.Fatalf("shell tool error = %v (answer=%v), want an %q answer so the run continues rather than "+
			"aborting as uncertain", err, ok, toolbroker.AnswerUnavailable)
	}
	if !strings.Contains(err.Error(), "shell") {
		t.Errorf("refusal %q does not name the shell tool", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("refusal %q is a cancellation, not a statement about wiring", err)
	}
}
