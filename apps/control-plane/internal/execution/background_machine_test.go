package execution

// THE SECOND EXECUTION PLANE. A.3 moved a synchronous command onto the machine holding the attempt's
// lease and left `background` process-wide, so a task's row records WHAT was started and never WHERE.
//
// That is survivable while there is exactly one control plane — background.go says so itself, and today
// the spawn and the probe are the same process. It stops being survivable the moment a second one exists,
// and the tree already models that: background_wake_component_test.go's `secondPlane` is "what 'two
// control planes' and 'a crash-restart' both mean here". The sweep that plane runs is system-scoped
// (coordinator.runningBackgroundTasks), so it probes EVERY still-running row against ITS OWN kernel.
//
// A kernel cannot tell "this pgid was never mine" from "this pgid has exited" — both are ESRCH. So the
// wrong machine does not get an error, it gets a WRONG ANSWER, which is exactly what migration 000001's
// own comment warns about for the posture check: "probing the wrong object is not an error, it is a
// WRONG ANSWER."

import (
	"context"
	"testing"
	"time"

	"github.com/palgroup/palai/packages/coordinator"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// machineBoundRunner is ONE machine's background executor. It answers only for the handles that machine
// actually started, which is the whole of the fidelity here: a host probe reaches this kernel and no
// other, and a pgid it never issued is simply absent.
type machineBoundRunner struct {
	live      map[string]bool
	signalled []string
}

func (r *machineBoundRunner) Start(context.Context, toolbroker.ShellCommand, toolbroker.BackgroundSpec) (toolbroker.Handle, error) {
	return toolbroker.Handle{}, nil
}

// Probe reports Exited for a handle this machine never started, and that is not a shortcut: `kill(pgid, 0)`
// returns ESRCH both for a group that ended and for one that never existed here. The executor has no
// third answer to give.
func (r *machineBoundRunner) Probe(_ context.Context, h toolbroker.Handle) (toolbroker.BackgroundStatus, error) {
	if r.live[h.Value] {
		return toolbroker.BackgroundStatus{State: toolbroker.BackgroundRunning}, nil
	}
	return toolbroker.BackgroundStatus{State: toolbroker.BackgroundExited}, nil
}

func (r *machineBoundRunner) Kill(_ context.Context, h toolbroker.Handle) error {
	r.signalled = append(r.signalled, h.Value)
	return nil
}

// foreignTask is a row a DIFFERENT machine wrote: its handle names a process group on that machine, and
// this control plane has never seen it.
// NO ALLOCATION ROOT, AND THAT IS LOAD-BEARING RATHER THAN LAZY. backgroundTail re-resolves the run's
// environment keys through the spine to redact the output, so a task with a root drags a database into a
// test about ROUTING — and worse, it hides what a perturbation is saying: with the machine comparison
// removed these tests died on a nil-spine panic instead of on their own assertions, which points a
// reader at the wrong file entirely. An empty root makes backgroundTail return its note immediately, so
// a broken routing fails HERE, by name.
func foreignTask(t *testing.T, handle string, deadline *time.Time) coordinator.BackgroundTask {
	t.Helper()
	return coordinator.BackgroundTask{
		ID:             "bgt_foreign",
		Tenant:         coordinator.Tenant{Organization: "org_1", Project: "prj_1"},
		RunID:          "run_1",
		SessionID:      "ses_1",
		ResponseID:     "resp_1",
		Posture:        string(toolbroker.PostureUnsandboxedHost),
		Handle:         handle,
		OutputPath:     "logs/bgt_foreign.log",
		AllocationRoot: "",
		DeadlineAt:     deadline,
		// THE MACHINE THAT STARTED IT. Mac-A; this control plane is on Mac-B.
		MachineID: "mac-A",
	}
}

// TestATaskOnAnotherMachineIsLostRatherThanExited is the wrong answer, stated as an assertion.
//
// Mac-A started a task and it is still compiling. Mac-B's reconciler probes the row against its own
// kernel, finds no such process group, and today reports `exited` — a model is told its build finished
// while the compiler is still running, and the run continues on an answer that is not about its task.
//
// `lost` is the honest state and it already means precisely this: the process may still be running and
// this control plane cannot prove it is ours. It also carries the safety property — a lost handle is
// never signalled.
func TestATaskOnAnotherMachineIsLostRatherThanExited(t *testing.T) {
	macB := &machineBoundRunner{live: map[string]bool{}} // this machine started nothing
	o := &Orchestrator{background: macB, machineID: "mac-B"}

	outcome, done, err := o.observeBackgroundTask(context.Background(), foreignTask(t, "4242:1700000000", nil))
	if err != nil {
		t.Fatalf("observeBackgroundTask: %v", err)
	}
	if !done {
		t.Fatal("a task this machine cannot probe was reported as still running, so its run waits forever")
	}
	if outcome.State == string(toolbroker.BackgroundExited) {
		t.Fatalf("a task started on mac-A was reported %q by mac-B: a kernel cannot tell 'never mine' from "+
			"'exited', so this tells a model its build finished while it is still compiling", outcome.State)
	}
	if outcome.State != string(toolbroker.BackgroundLost) {
		t.Fatalf("outcome state = %q, want %q", outcome.State, toolbroker.BackgroundLost)
	}
}

// TestAnotherMachinesTaskIsNeverSignalled is the second failure and the more expensive one. A pgid is a
// small integer and the start time that makes it trustworthy is a reading of ONE machine's clock, so two
// machines can hold the same handle value for unrelated processes.
//
// When they do, mac-B's probe says `running`, the deadline expires, and the reaper signals a process
// group that belongs to something else entirely. The reaper spans tenants, so this is a hygiene failure
// rather than a consistency one: the thing it kills need not be Palai's at all.
func TestAnotherMachinesTaskIsNeverSignalled(t *testing.T) {
	const handle = "4242:1700000000"
	// The coincidence: mac-B has a live process group with the same value, for something unrelated.
	macB := &machineBoundRunner{live: map[string]bool{handle: true}}
	o := &Orchestrator{background: macB, machineID: "mac-B"}

	expired := time.Now().Add(-time.Hour)
	outcome, done, err := o.observeBackgroundTask(context.Background(), foreignTask(t, handle, &expired))
	if err != nil {
		t.Fatalf("observeBackgroundTask: %v", err)
	}
	if len(macB.signalled) != 0 {
		t.Fatalf("mac-B signalled %v for a task mac-A started: a pgid is a small integer and this reaper "+
			"spans tenants, so the process it killed need not be Palai's at all", macB.signalled)
	}
	if !done {
		t.Fatal("a task this machine cannot probe was left running, so its run waits forever")
	}
	if outcome.State != string(toolbroker.BackgroundLost) {
		t.Fatalf("outcome state = %q, want %q — this machine cannot prove the process is ours", outcome.State, toolbroker.BackgroundLost)
	}
}

// TestThisMachinesOwnTaskIsStillProbedNormally is the control, and without it the two tests above could
// be satisfied by reporting `lost` for everything — which would break the crash-restart property the
// wake suite proves: a control plane that restarted on the SAME machine must still settle its own tasks.
func TestThisMachinesOwnTaskIsStillProbedNormally(t *testing.T) {
	const handle = "777:1700000000"
	mine := &machineBoundRunner{live: map[string]bool{}} // started here, and now finished
	o := &Orchestrator{background: mine, machineID: "mac-A"}

	task := foreignTask(t, handle, nil)
	task.MachineID = "mac-A" // this machine's own row

	outcome, done, err := o.observeBackgroundTask(context.Background(), task)
	if err != nil {
		t.Fatalf("observeBackgroundTask: %v", err)
	}
	if !done || outcome.State != string(toolbroker.BackgroundExited) {
		t.Fatalf("this machine's own finished task reported done=%v state=%q, want an exit: a restarted "+
			"control plane on the same machine must still settle what it started", done, outcome.State)
	}
	if outcome.TailNote == "" {
		t.Error("a settled task with no readable output carried no note, so the model is told nothing about why")
	}
}
