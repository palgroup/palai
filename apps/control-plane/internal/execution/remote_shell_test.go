package execution_test

// The control-plane half of the A.3 wire, proved against a REAL machine rather than a stand-in for
// one. Every test here drives the shipped gateway (enrollment + mTLS WebSocket + readLoop) with a
// real packages/runner lease on the other end, so what is measured is the path an attempt takes:
// RemoteShell -> gatewayChannel.StartExec -> the lease connection -> RelayInbound -> ToolServer, and
// the exec.result back through readLoop, which is the connection's SOLE reader.
//
// The fake is the EXECUTOR on the machine, never the wire. A hand-written stand-in for the transport
// would be the one thing that cannot fail here: the risk this task carries is entirely about whether
// the answer is routed by a reader that already owns the connection, and a fake transport is a fake
// reader.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/runner"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// recordingExecutor is the machine's own shell executor: it records what it was asked to run and
// answers with whatever the test wants that command's outcome to be. It stands in for the host/OCI
// executor cmd/runner wires, and for nothing else on the path.
type recordingExecutor struct {
	mu     sync.Mutex
	got    []toolbroker.ShellCommand
	result toolbroker.ShellResult
	err    error
}

func (e *recordingExecutor) Run(_ context.Context, cmd toolbroker.ShellCommand) (toolbroker.ShellResult, error) {
	e.mu.Lock()
	e.got = append(e.got, cmd)
	e.mu.Unlock()
	return e.result, e.err
}

func (e *recordingExecutor) commands() []toolbroker.ShellCommand {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]toolbroker.ShellCommand(nil), e.got...)
}

// remoteShellFixture stands a gateway up with an enrolled runner already dialling it, and returns the
// control plane's end of the lease. runnerSide is the machine's behaviour for this test.
func remoteShellFixture(t *testing.T, ctx context.Context, token, runID string, machine func(context.Context, *runner.LeaseSession)) execution.EngineChannel {
	t.Helper()
	f := newGatewayFixture(t, newOneUseTokens(token))

	identity, err := runner.Enroll(ctx, f.bootstrap(token))
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}

	machineDone := make(chan struct{})
	go func() {
		defer close(machineDone)
		lease, err := f.session(identity).OpenLease(ctx)
		if err != nil {
			return // the control plane's own assertion reports the failure; this goroutine has no t.
		}
		defer lease.Close()
		machine(ctx, lease)
	}()
	t.Cleanup(func() {
		select {
		case <-machineDone:
		case <-time.After(5 * time.Second):
			t.Error("the machine's goroutine did not finish")
		}
	})

	ch, err := f.gateway.Dial(ctx, f.attempt(runID, "att_"+runID, 1))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = ch.Close() })
	return ch
}

// remoteShellOn is the type assertion Task 3 will make: the value Dial returns is an EngineChannel,
// and the exec surface is a SIBLING of that interface rather than part of it — an exec.request sent
// as an EngineFrame would be wrapped in a controller.frame and injected into the engine's stdin,
// where nothing would run it.
func remoteShellOn(t *testing.T, ch execution.EngineChannel) *execution.RemoteShell {
	t.Helper()
	conn, ok := ch.(execution.ExecConn)
	if !ok {
		t.Fatalf("the channel Dial returned (%T) does not carry the exec surface, so no attempt could run a command on its machine", ch)
	}
	return execution.NewRemoteShell(conn)
}

// relayingMachine is the machine as cmd/runner assembles it: the shipped router, with this test's
// executor behind the shipped tool server.
func relayingMachine(exec toolbroker.ShellRunner, beforeRelay func(context.Context, *runner.LeaseSession)) func(context.Context, *runner.LeaseSession) {
	return func(ctx context.Context, lease *runner.LeaseSession) {
		if beforeRelay != nil {
			beforeRelay(ctx, lease)
		}
		inbound := make(chan contracts.EngineFrame, 4)
		go func() {
			for range inbound { // drain: this machine relays no engine frame in these tests
			}
		}()
		runner.RelayInbound(ctx, lease, runner.NewToolServer(exec), inbound, func(string, ...any) {})
	}
}

// TestRemoteShellRunsTheCommandOnTheMachineThatHoldsTheLease is the task's headline claim: a command
// the control plane dispatches reaches the machine's own executor with its argv intact, and the
// machine's result comes back as the tool's result.
//
// THE ENGINE FRAME IS RECEIVED BEFORE THE COMMAND IS SENT, deliberately and for a reason the wire
// forces: readLoop relays engine frames over an UNBUFFERED channel, so a frame nobody has taken
// stops the only goroutine that could read the exec.result behind it. An orchestrator reaches
// dispatchTool by receiving frames, so this is its ordering — and the ordering that is NOT safe is
// pinned by TestRemoteShellStallsBehindAnUnconsumedEngineFrame below.
func TestRemoteShellRunsTheCommandOnTheMachineThatHoldsTheLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	machineExec := &recordingExecutor{result: toolbroker.ShellResult{
		ExitCode: 0, Stdout: "** BUILD SUCCEEDED **", DurationMS: 1234,
	}}
	ch := remoteShellFixture(t, ctx, "rs-token-run", "run_rsrun", relayingMachine(machineExec,
		func(ctx context.Context, lease *runner.LeaseSession) {
			_ = lease.SendEngineFrame(ctx, contracts.EngineFrame{
				Protocol: "engine.v1", ID: "frm_rsready", Type: "engine.ready", Sequence: 1,
				Time: time.Now().UTC().Format(time.RFC3339), RunID: lease.Lease().RunID,
				Data: map[string]any{"selected_protocol": "engine.v1"},
			})
		}))

	if _, err := ch.Receive(ctx); err != nil {
		t.Fatalf("Receive engine.ready: %v", err)
	}

	want := toolbroker.ShellCommand{
		Argv:          []string{"xcodebuild", "-scheme", "Palai", "-destination", "generic/platform=iOS"},
		WorkspaceRoot: "/tmp/palai-workspace",
		ReadOnly:      false,
		Shell:         false,
		Env:           map[string]string{"DEVELOPER_DIR": "/Applications/Xcode.app/Contents/Developer"},
	}
	result, err := remoteShellOn(t, ch).Run(ctx, want)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	ran := machineExec.commands()
	if len(ran) != 1 {
		t.Fatalf("the machine ran %d commands, want exactly 1", len(ran))
	}
	if strings.Join(ran[0].Argv, "\x00") != strings.Join(want.Argv, "\x00") {
		t.Errorf("argv on the machine = %q, want %q", ran[0].Argv, want.Argv)
	}
	if ran[0].WorkspaceRoot != want.WorkspaceRoot || ran[0].ReadOnly != want.ReadOnly || ran[0].Shell != want.Shell {
		t.Errorf("command on the machine = %+v, want workspace/read-only/shell of %+v", ran[0], want)
	}
	if ran[0].Env["DEVELOPER_DIR"] != want.Env["DEVELOPER_DIR"] {
		t.Errorf("env on the machine = %v, want DEVELOPER_DIR=%q", ran[0].Env, want.Env["DEVELOPER_DIR"])
	}
	if result.Stdout != "** BUILD SUCCEEDED **" || result.DurationMS != 1234 {
		t.Errorf("result = %+v, want the machine's own stdout and duration", result)
	}
}

// TestRemoteShellReturnsNonZeroExitAsAResultNotAnError is the one this tree has already paid for. An
// error out of ShellRunner.Run is NOT an answer: tools/shell.go turns it into an uncertain abort,
// which ends the attempt. `false` exits 1 and completes cleanly, and it must arrive as a result whose
// exit_code the model can read.
func TestRemoteShellReturnsNonZeroExitAsAResultNotAnError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	machineExec := &recordingExecutor{result: toolbroker.ShellResult{
		ExitCode: 1, Stderr: "the command failed", DurationMS: 7,
	}}
	ch := remoteShellFixture(t, ctx, "rs-token-exit", "run_rsexit", relayingMachine(machineExec, nil))

	result, err := remoteShellOn(t, ch).Run(ctx, toolbroker.ShellCommand{
		Argv: []string{"false"}, WorkspaceRoot: "/tmp/palai-workspace",
	})
	if err != nil {
		t.Fatalf("a non-zero exit came back as an error, which aborts the attempt: %v", err)
	}
	if result.ExitCode != 1 || result.Stderr != "the command failed" {
		t.Fatalf("result = %+v, want exit code 1 with the machine's stderr", result)
	}
}

// TestRemoteShellSurfacesTheMachinesRefusal covers the wire's OTHER answer shape. A machine that was
// never wired with an executor replies with data.error rather than falling silent, and that refusal
// must reach the caller as a named failure — the run stops for a reason an operator can read, instead
// of a tool call that never returns.
func TestRemoteShellSurfacesTheMachinesRefusal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// A nil executor is the shipped "this runner runs no command" posture (runner.NewToolServer).
	ch := remoteShellFixture(t, ctx, "rs-token-refuse", "run_rsrefuse", relayingMachine(nil, nil))

	_, err := remoteShellOn(t, ch).Run(ctx, toolbroker.ShellCommand{
		Argv: []string{"true"}, WorkspaceRoot: "/tmp/palai-workspace",
	})
	if err == nil {
		t.Fatal("Run succeeded, but the machine refused to run the command")
	}
	if !strings.Contains(err.Error(), "not wired with an executor") {
		t.Fatalf("Run error = %v, want the machine's own refusal", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run error = %v, want the refusal rather than a timeout", err)
	}
}

// TestRemoteShellAnswersWhenTheLeaseConnectionDies pins the property the whole registry exists for: a
// command whose machine disappears mid-flight gets an ANSWER, not silence. The context here is
// generous on purpose — a Run that only returns when its deadline expires would pass a timeout-shaped
// assertion while wedging every real attempt, because a command this epic exists for (`xcodebuild`)
// is allowed to take minutes and its context says so.
func TestRemoteShellAnswersWhenTheLeaseConnectionDies(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ch := remoteShellFixture(t, ctx, "rs-token-dead", "run_rsdead", func(ctx context.Context, lease *runner.LeaseSession) {
		// Take the request off the wire and then drop the connection without answering it.
		if _, err := lease.ReceiveMessage(ctx); err != nil {
			return
		}
		_ = lease.Close()
	})

	type outcome struct {
		result toolbroker.ShellResult
		err    error
	}
	answered := make(chan outcome, 1)
	go func() {
		result, err := remoteShellOn(t, ch).Run(ctx, toolbroker.ShellCommand{
			Argv: []string{"sleep", "600"}, WorkspaceRoot: "/tmp/palai-workspace",
		})
		answered <- outcome{result, err}
	}()

	select {
	case got := <-answered:
		if got.err == nil {
			t.Fatalf("Run returned result %+v with no error, but the machine never answered", got.result)
		}
		if errors.Is(got.err, context.DeadlineExceeded) {
			t.Fatalf("Run error = %v, want the connection's end rather than a deadline", got.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the lease connection died — a tool call that waits forever wedges the run")
	}
}

// TestRemoteShellStallsBehindAnUnconsumedEngineFrame RECORDS A CEILING THIS TASK DOES NOT LIFT, and
// it is written as an assertion rather than a comment because a comment cannot fail.
//
// readLoop is the connection's sole reader and it relays engine frames over an UNBUFFERED channel
// (runner_gateway.go, gatewayChannel.frames). Nothing here can change that: a second reader would
// consume frames the orchestrator's own sequence discipline expects, which fails the attempt with a
// protocol violation. So while a frame sits un-received, the reader is blocked in emit and has not
// even read the exec.result behind it.
//
// Today no attempt reaches this state — dispatchTool runs on the goroutine that has just received a
// frame, so the relay is drained up to the tool.request. It becomes reachable the moment the engine
// emits anything (progress, warning, heartbeat: orchestrator.go's default arm names all three) while
// its tool call is outstanding. The fix belongs to the relay or to the dispatch loop, not here.
//
// WHEN THAT FIX LANDS THIS TEST MUST FAIL, and inverting it is the point: the answer will arrive.
func TestRemoteShellStallsBehindAnUnconsumedEngineFrame(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ch := remoteShellFixture(t, ctx, "rs-token-stall", "run_rsstall", func(ctx context.Context, lease *runner.LeaseSession) {
		message, err := lease.ReceiveMessage(ctx)
		if err != nil {
			return
		}
		// One engine frame ahead of the answer, and nobody on the control plane is receiving it.
		_ = lease.SendEngineFrame(ctx, contracts.EngineFrame{
			Protocol: "engine.v1", ID: "frm_rsstall", Type: "progress", Sequence: 1,
			Time: time.Now().UTC().Format(time.RFC3339), RunID: lease.Lease().RunID,
		})
		_ = lease.SendExecResult(ctx, runner.NewToolServer(&recordingExecutor{}).Handle(ctx, message))
		<-ctx.Done()
	})

	runCtx, runCancel := context.WithTimeout(ctx, 750*time.Millisecond)
	defer runCancel()
	_, err := remoteShellOn(t, ch).Run(runCtx, toolbroker.ShellCommand{
		Argv: []string{"true"}, WorkspaceRoot: "/tmp/palai-workspace",
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run error = %v, want the deadline — if the answer now arrives behind an unconsumed "+
			"engine frame, the relay's head-of-line stall has been fixed and this test should assert the result", err)
	}
}
