package execution

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"

	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// This file is the orchestration BETWEEN the tool surface and the two sandbox adapters (E26 T2): it
// mints a task id, derives the log path, decides whether the deployment allows a background task at all,
// and answers which handle a task id names. The adapters own process groups and container ids; the tools
// own a task id and a path; nothing crosses.
//
// WHAT THIS LAYER DELIBERATELY DOES NOT DO YET, named rather than implied: it does not write the
// background_tasks row migration 000047 opened. That row is what makes a task survive the process that
// started it, and the three things that read it — the park gate, the exit notification and the reaper —
// are T3, T4 and T5. Until then the ceiling below is real and is stated where it can be read.

// backgroundLogDir is the subtree a background task's merged output goes into. It is under
// `.palai-session` for a reason that was measured rather than chosen: Snapshot skips that whole subtree
// (adapters/sandboxes/oci/workspace/allocation.go), so a log does not enter a snapshot, does not enter a
// changeset and does not enter a checksum — while palai.workspace.file still reads it, because it is an
// ordinary path under the allocation root.
const backgroundLogDir = ".palai-session/bg"

// errBackgroundDisabled is the kill switch's answer, and it is a REFUSAL rather than a downgrade. A
// silent fall back to a synchronous run would leave the model believing it had backgrounded something
// that is in fact blocking it, which is precisely the behaviour background execution exists to prevent —
// so the deployment that turns the feature off makes the call fail, and the model can do something with
// a failure. (E26 §3.5 P5, taken from the vendor's own CLAUDE_CODE_DISABLE_BACKGROUND_TASKS.)
var errBackgroundDisabled = errors.New("background execution is disabled on this deployment (PALAI_BACKGROUND_DISABLED); the background parameter is refused rather than run synchronously")

// backgroundDisabled reports the deployment kill switch, and it FAILS CLOSED on a value it cannot read:
// an operator who wrote `PALAI_BACKGROUND_DISABLED=yes` meant to switch the feature off, and answering
// "that is not a boolean, so the feature stays on" would be the least useful reading available.
//
// It is read here, per call, rather than captured once at wiring time — the same reason
// backgroundRunnerFor is a named function in main.go: a test that builds its own configuration never
// exercises the configuration production builds, and this way the switch is reached through the
// production path or not at all.
func backgroundDisabled() bool {
	raw := strings.TrimSpace(os.Getenv("PALAI_BACKGROUND_DISABLED"))
	if raw == "" {
		return false
	}
	off, err := strconv.ParseBool(raw)
	return err != nil || off
}

// backgroundTask is one started process as this control plane remembers it: the durable name the adapter
// gave us, the run that owns it, and where its output lands.
type backgroundTask struct {
	taskID     string
	runID      string
	handle     toolbroker.Handle
	outputPath string
}

// backgroundTasks is the per-dispatch view of the orchestrator's background registry, bound to the run
// that is calling. The run id is what makes "kill task X" a question with a safe answer: a task started
// by one run cannot be named by another, and the check is here rather than in the tool because the tool
// is handed a string by a model.
type backgroundTasks struct {
	orch  *Orchestrator
	runID string
}

var _ toolbroker.BackgroundTasks = (*backgroundTasks)(nil)

// StartBackground spawns one detached command and returns the handle the model works from.
//
// ORDER MATTERS AND IS THE POINT: the kill switch and the missing-runner refusal both happen BEFORE
// anything is started, so a refused call leaves no process, no log file and no directory behind.
func (b *backgroundTasks) StartBackground(ctx context.Context, cmd toolbroker.ShellCommand) (toolbroker.BackgroundTicket, error) {
	if backgroundDisabled() {
		return toolbroker.BackgroundTicket{}, errBackgroundDisabled
	}
	if b.orch == nil || b.orch.background == nil {
		return toolbroker.BackgroundTicket{}, toolbroker.ErrBackgroundUnsupported
	}
	taskID := newExecID("bgt")
	spec := toolbroker.BackgroundSpec{TaskID: taskID, OutputPath: path.Join(backgroundLogDir, taskID+".log")}

	handle, err := b.orch.background.Start(ctx, cmd, spec)
	if err != nil {
		return toolbroker.BackgroundTicket{}, err
	}
	// Registered BEFORE the probe: from the instant a process exists, the id that can stop it must be
	// resolvable. A probe that failed in between would otherwise leave a running process whose handle
	// nobody holds, which is the orphan this epic is about.
	b.orch.rememberBackgroundTask(&backgroundTask{taskID: taskID, runID: b.runID, handle: handle, outputPath: spec.OutputPath})

	// `status` is what the operating system says, not what we hope: a command that has already failed
	// reports `exited` rather than a comfortable `running`, and a model reading `running` can trust it.
	//
	// A probe that cannot answer does NOT fail the call — losing the task id of a process that is already
	// running is strictly worse than one imprecise status field, and `running` is the honest reading of
	// "Start succeeded and nothing has told us otherwise".
	state := toolbroker.BackgroundRunning
	if status, perr := b.orch.background.Probe(ctx, handle); perr == nil {
		state = status.State
	}
	return toolbroker.BackgroundTicket{TaskID: taskID, OutputPath: spec.OutputPath, State: state}, nil
}

// KillBackground stops one task of this run. An id this run did not start is REFUSED rather than
// answered: a model that mistyped an id must not be told the task is dead, and a run must not be able to
// reach into another run's processes by guessing.
func (b *backgroundTasks) KillBackground(ctx context.Context, taskID string) (toolbroker.BackgroundTicket, error) {
	if b.orch == nil || b.orch.background == nil {
		return toolbroker.BackgroundTicket{}, toolbroker.ErrBackgroundUnsupported
	}
	task, ok := b.orch.backgroundTaskOf(b.runID, taskID)
	if !ok {
		return toolbroker.BackgroundTicket{}, fmt.Errorf("no background task %q belongs to this run", taskID)
	}
	if err := b.orch.background.Kill(ctx, task.handle); err != nil {
		return toolbroker.BackgroundTicket{}, err
	}
	// The state is read back from the operating system rather than assumed from the kill: a handle the
	// adapter refused to signal (a pgid it cannot prove is ours) reports `lost` here, and the model is told
	// that rather than being told the task is dead.
	state := toolbroker.BackgroundExited
	if status, perr := b.orch.background.Probe(ctx, task.handle); perr == nil {
		state = status.State
	}
	return toolbroker.BackgroundTicket{TaskID: task.taskID, OutputPath: task.outputPath, State: state}, nil
}

// rememberBackgroundTask records a started task so its id can later name its handle.
//
// ponytail: IN-MEMORY, AND THE CEILING IS THE HONEST HALF OF THIS TASK. Migration 000047 already opened
// the background_tasks table that makes this durable, and the three consumers of those rows are T3 (the
// park gate), T4 (the exit notification) and T5 (the reaper). Until they land, a control-plane restart
// leaves running background tasks with nobody holding their handles: the PROCESSES keep running — that is
// T1's whole point and it is unaffected — but this control plane can no longer kill them by id, and
// nothing reaps them. Upgrade path: replace this map with the background_tasks row, which is the only
// change T5's adopt path needs from here.
func (o *Orchestrator) rememberBackgroundTask(task *backgroundTask) {
	o.bgMu.Lock()
	defer o.bgMu.Unlock()
	if o.bgTasks == nil {
		o.bgTasks = map[string]*backgroundTask{}
	}
	o.bgTasks[task.taskID] = task
}

// backgroundTaskOf resolves a task id AND checks it belongs to the asking run in one step, so no caller
// can perform the lookup without performing the check.
func (o *Orchestrator) backgroundTaskOf(runID, taskID string) (*backgroundTask, bool) {
	o.bgMu.Lock()
	defer o.bgMu.Unlock()
	task, ok := o.bgTasks[taskID]
	if !ok || task.runID != runID {
		return nil, false
	}
	return task, true
}
