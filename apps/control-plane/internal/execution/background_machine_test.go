package execution

// TWO MACHINES, TWO TASKS. A.3 T6 put the machine on the row and routed four readers by comparing it to
// THIS process's identity; T7 moved the execution itself, so those four now ASK the row's machine.
// What survives the rewrite is the pair of rules that made T6 worth doing, and both are asserted here
// because they are exactly what a rewrite loses:
//
//   - an unreachable machine is `lost`, NEVER `exited` — a kernel cannot tell "never mine" from
//     "exited" (both are ESRCH), so an answer taken from the wrong one tells a model its build
//     finished while the compiler is still running;
//   - a task on an unreachable machine is NEVER SIGNALLED, and its output is never read — that file is
//     on that machine, and on a shared filesystem the same relative path could be another run's.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/packages/runner"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// fleetOfOne is a control plane that can reach exactly one machine. Every other name is refused the way
// the gateway refuses it — ErrMachineUnreachable — which is what a powered-off, revoked or never-seen
// machine looks like from here.
type fleetOfOne struct {
	reachable string
	status    toolbroker.BackgroundState
	probed    []string
	killed    []string
}

func (f *fleetOfOne) CallMachine(_ context.Context, machineID, verb string, _ map[string]any) (map[string]any, error) {
	if machineID != f.reachable {
		return nil, ErrMachineUnreachable
	}
	switch verb {
	case runner.BackgroundProbeType:
		f.probed = append(f.probed, machineID)
		return map[string]any{"status": map[string]any{"State": string(f.status)}}, nil
	case runner.BackgroundKillType:
		f.killed = append(f.killed, machineID)
		return map[string]any{"killed": true}, nil
	}
	return nil, errors.New("unexpected verb " + verb)
}

// taskOn is a row a machine wrote. NO ALLOCATION ROOT, and that is load-bearing rather than lazy:
// backgroundTail re-resolves the run's environment keys through the spine, so a root would drag a
// database into a test about ROUTING — and worse, it would make a broken routing die on a nil-spine
// panic instead of on the assertion below, pointing a reader at the wrong file entirely. That happened
// once already, in T6, and cost a separate commit to undo.
func taskOn(machineID, handle string, deadline *time.Time) coordinator.BackgroundTask {
	return coordinator.BackgroundTask{
		ID:         "bgt_1",
		Tenant:     coordinator.Tenant{Organization: "org_1", Project: "prj_1"},
		RunID:      "run_1",
		SessionID:  "ses_1",
		ResponseID: "resp_1",
		Posture:    string(toolbroker.PostureUnsandboxedHost),
		Handle:     handle,
		MachineID:  machineID,
		OutputPath: "logs/bgt_1.log",
		DeadlineAt: deadline,
	}
}

// TestATaskOnAnUnreachableMachineIsLostRatherThanExited is the wrong answer, stated as an assertion.
func TestATaskOnAnUnreachableMachineIsLostRatherThanExited(t *testing.T) {
	fleet := &fleetOfOne{reachable: "mac-B", status: toolbroker.BackgroundExited}
	o := &Orchestrator{machines: fleet}

	outcome, done, err := o.observeBackgroundTask(context.Background(), taskOn("mac-A", "4242:1700000000", nil))
	if err != nil {
		t.Fatalf("observeBackgroundTask: %v", err)
	}
	if !done {
		t.Fatal("a task this control plane cannot reach was left running, so its run waits forever")
	}
	if outcome.State == string(toolbroker.BackgroundExited) {
		t.Fatalf("a task on mac-A was reported %q: the machine was never asked, so this tells a model its "+
			"build finished while it may still be compiling", outcome.State)
	}
	if outcome.State != string(toolbroker.BackgroundLost) {
		t.Fatalf("outcome state = %q, want %q", outcome.State, toolbroker.BackgroundLost)
	}
	if len(fleet.probed) != 0 {
		t.Errorf("a machine was probed for a row that names a different one: %v", fleet.probed)
	}
	if outcome.Tail != "" {
		t.Errorf("output was read for a task on another machine (%q): that file is on that machine, and the "+
			"same relative path here could be another run's", outcome.Tail)
	}
	if outcome.TailNote == "" {
		t.Error("the model is told nothing about why there is no output")
	}
}

// TestAnUnreachableMachinesTaskIsNeverSignalled is the second failure and the more expensive one: a pgid
// is a small integer and the reaper spans tenants, so a signal sent on the wrong box need not land on
// anything of ours at all.
func TestAnUnreachableMachinesTaskIsNeverSignalled(t *testing.T) {
	fleet := &fleetOfOne{reachable: "mac-B", status: toolbroker.BackgroundRunning}
	o := &Orchestrator{machines: fleet}

	expired := time.Now().Add(-time.Hour)
	outcome, done, err := o.observeBackgroundTask(context.Background(), taskOn("mac-A", "4242:1700000000", &expired))
	if err != nil {
		t.Fatalf("observeBackgroundTask: %v", err)
	}
	if len(fleet.killed) != 0 {
		t.Fatalf("a kill was sent for a task on mac-A: %v", fleet.killed)
	}
	if !done || outcome.State != string(toolbroker.BackgroundLost) {
		t.Fatalf("done=%v state=%q, want a settled `lost`", done, outcome.State)
	}
}

// TestARowThatNamesNoMachineIsUnreachable keeps T6's sharpest rule: TWO UNKNOWNS DO NOT MATCH. A row
// written before the machine column existed says nothing about where it ran, and resolving that to any
// machine would apply the wrong answer to the rows carrying the least evidence.
func TestARowThatNamesNoMachineIsUnreachable(t *testing.T) {
	fleet := &fleetOfOne{reachable: "", status: toolbroker.BackgroundExited}
	o := &Orchestrator{machines: fleet}

	outcome, done, err := o.observeBackgroundTask(context.Background(), taskOn("", "4242:1700000000", nil))
	if err != nil {
		t.Fatalf("observeBackgroundTask: %v", err)
	}
	if !done || outcome.State != string(toolbroker.BackgroundLost) {
		t.Fatalf("a row naming no machine settled as done=%v state=%q, want `lost`", done, outcome.State)
	}
	if len(fleet.probed) != 0 {
		t.Errorf("an empty machine name was matched against a reachable one: %v", fleet.probed)
	}
}

// TestAReachableMachineIsAskedAndItsAnswerUsed is the control, and without it every test above could be
// satisfied by reporting `lost` for everything — which would settle every task in the fleet as lost.
func TestAReachableMachineIsAskedAndItsAnswerUsed(t *testing.T) {
	fleet := &fleetOfOne{reachable: "mac-A", status: toolbroker.BackgroundExited}
	o := &Orchestrator{machines: fleet}

	outcome, done, err := o.observeBackgroundTask(context.Background(), taskOn("mac-A", "777:1700000000", nil))
	if err != nil {
		t.Fatalf("observeBackgroundTask: %v", err)
	}
	if !done || outcome.State != string(toolbroker.BackgroundExited) {
		t.Fatalf("done=%v state=%q, want the machine's own `exited`", done, outcome.State)
	}
	if len(fleet.probed) != 1 || fleet.probed[0] != "mac-A" {
		t.Fatalf("the machine asked was %v, want exactly mac-A once", fleet.probed)
	}
}

// TestADeploymentWithNoGatewayStartsNoBackgroundTask is the other end of the same rule. A control plane
// that can address no machine has nowhere to put a detached process, and saying so is what keeps a
// backgrounded command from quietly running beside the control plane — the behaviour A.3 removes.
func TestADeploymentWithNoGatewayStartsNoBackgroundTask(t *testing.T) {
	o := &Orchestrator{} // no MachineCaller wired
	tasks := &backgroundTasks{orch: o, machineID: "mac-A"}

	_, err := tasks.StartBackground(context.Background(), toolbroker.ShellCommand{Argv: []string{"sleep", "60"}}, "tcall_1")
	if !errors.Is(err, toolbroker.ErrBackgroundUnsupported) {
		t.Fatalf("StartBackground error = %v, want ErrBackgroundUnsupported", err)
	}

	// And an attempt with no MACHINE is refused for the same reason even where a gateway exists: the
	// process would have nowhere to run but here.
	withGateway := &backgroundTasks{orch: &Orchestrator{machines: &fleetOfOne{}}, machineID: ""}
	if _, err := withGateway.StartBackground(context.Background(), toolbroker.ShellCommand{Argv: []string{"sleep", "60"}}, "tcall_2"); !errors.Is(err, toolbroker.ErrBackgroundUnsupported) {
		t.Fatalf("an attempt with no machine started a task anyway: %v", err)
	}
}
