package execution

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// This file is the orchestration BETWEEN the tool surface and the two sandbox adapters (E26 T2): it
// mints a task id, derives the log path, decides whether the deployment allows a background task at all,
// and answers which handle a task id names. The adapters own process groups and container ids; the tools
// own a task id and a path; nothing crosses.
//
// SINCE T4 IT ALSO WRITES THE ROW migration 000047 opened, and reads it back on the way out. The row is
// what makes a task survive the process that started it: the in-memory registry below dies with this
// program, and the reconciler's sweep — which may be running in a DIFFERENT control plane, or in one
// that restarted since the spawn — works entirely from the row. T5 still owns the reaper's half of it
// (deadlines, adoption, cancellation).

// backgroundLogDir is the subtree a background task's merged output goes into. It is under
// `.palai-session` for a reason that was measured rather than chosen: Snapshot skips that whole subtree
// (adapters/sandboxes/oci/workspace/allocation.go), so a log does not enter a snapshot, does not enter a
// changeset and does not enter a checksum — while palai.workspace.file still reads it, because it is an
// ordinary path under the allocation root.
//
// AND THE READ OF THAT FILE IS NOT REDACTED TODAY, WHICH IS T6's AND IS WRITTEN HERE SO IT IS FOUND. A
// synchronous shell result is redacted on the way back (host/exec.go, workspace/exec.go apply
// RedactSecrets/RedactValues to the CAPTURED GO STRING); a process writing its own log file bypasses
// both, and palai.workspace.file has never redacted anything it reads. So a background command that
// echoes one of the attempt's environment values puts that value where the model can read it raw — the
// exposure E26 §3.6 D8 names, whose fix is a redacting read path that re-resolves the task's env_keys at
// READ time, because the row holds key names and never values.
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
//
// It carries the rest of the attempt's identity because the DURABLE ROW needs it (E26 T4): a
// background_tasks row names the run that owns the process, the session and response its notification
// belongs to, the call that spawned it and the fence that attempt held. Every one of those is known at
// execEnv time and none of them can be recovered later from a handle.
type backgroundTasks struct {
	orch       *Orchestrator
	tenant     coordinator.Tenant
	runID      string
	sessionID  string
	responseID string
	fence      uint64
}

var _ toolbroker.BackgroundTasks = (*backgroundTasks)(nil)

// StartBackground spawns one detached command and returns the handle the model works from.
//
// ORDER MATTERS AND IS THE POINT: the kill switch and the missing-runner refusal both happen BEFORE
// anything is started, so a refused call leaves no process, no log file and no directory behind.
func (b *backgroundTasks) StartBackground(ctx context.Context, cmd toolbroker.ShellCommand, callID contracts.ToolCallID) (toolbroker.BackgroundTicket, error) {
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

	// THE DURABLE ROW, and it is written for a reader that is not this program (E26 T4). The registry
	// above answers "which handle does this run's task id name" for as long as this process lives; the
	// row answers it for the reconciler, which may be a different control plane or this one after a
	// restart, and which is the ONLY thing that will ever notice this process finish.
	//
	// A FAILED INSERT KILLS THE PROCESS. It is the one place in this file that undoes work rather than
	// reporting around it, and the reason is what a row-less task IS: a process running under a handle
	// nothing durable holds, which no reaper will look at, no deadline will end and no cancellation will
	// reach. That is precisely the orphan this epic exists to make impossible, so a task that cannot be
	// recorded is a task that must not be left running.
	if err := b.orch.spine.RecordBackgroundTask(ctx, b.tenant, coordinator.BackgroundTaskInput{
		TaskID: taskID, RunID: b.runID, SessionID: b.sessionID, ResponseID: b.responseID,
		ToolCallID: string(callID), Fence: b.fence,
		Posture: string(handle.Posture), Handle: handle.Value, OutputPath: spec.OutputPath,
		// env_keys holds KEY NAMES so the read path can re-resolve and mask them (T6). The VALUES are in
		// cmd.Env and stay there; sorted so the column does not depend on Go's map iteration order.
		EnvKeys: sortedEnvKeys(cmd.Env),
	}); err != nil {
		if kerr := b.orch.background.Kill(ctx, handle); kerr != nil {
			// Both failures matter and neither replaces the other: the caller learns the task was refused,
			// and the log names the process that is now genuinely unaccounted for.
			log.Printf("background task %s could not be recorded (%v) AND could not be killed (%v): handle %s is now an orphan", taskID, err, kerr, handle.Value)
		}
		return toolbroker.BackgroundTicket{}, fmt.Errorf("record background task: %w", err)
	}

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

// runHasLiveBackgroundTask reports whether this run owns a background task the OPERATING SYSTEM still
// says is running (E26 T3). It is what decides whether a completed run parks instead of finishing.
//
// THE AUTHORITY IS THE PROBE, NEVER THE MAP. The registry remembers every task this process started and
// never forgets one; a task that exited ten minutes ago is still in it. Asking our own bookkeeping
// "is anything running" would park every run that ever backgrounded anything, forever. So the map
// answers only "which handles belong to this run" and the kernel — or the container daemon — answers
// the question that matters, which is the same discipline T1 built the whole seam on.
//
// A HANDLE WE CANNOT PROVE IS OURS DOES NOT PARK ANYTHING. `lost` means a process may be running and may
// be somebody else's (PID reuse), and the rule for a lost handle is already absolute: it is never
// signalled. Parking on one would be the same mistake in the other direction — the run would wait for an
// exit notification about a process nothing can watch, and only T5's reaper would ever free it.
//
// A PROBE THAT ERRORS DOES NOT PARK EITHER, and the asymmetry is deliberate: parking on an unreadable
// probe strands the run until a reaper, while not parking loses one exit notification for a run that
// finished. The second failure is recoverable by the operator reading the log file the task wrote; the
// first is a run that never ends. The error is logged rather than swallowed, so an operator whose Docker
// socket has gone away learns it here.
func (o *Orchestrator) runHasLiveBackgroundTask(ctx context.Context, runID string) bool {
	if o.background == nil {
		return false
	}
	o.bgMu.Lock()
	mine := make([]*backgroundTask, 0, len(o.bgTasks))
	for _, task := range o.bgTasks {
		if task.runID == runID {
			mine = append(mine, task)
		}
	}
	o.bgMu.Unlock()

	for _, task := range mine {
		status, err := o.background.Probe(ctx, task.handle)
		if err != nil {
			log.Printf("probe background task %s of run %s: %v", task.taskID, runID, err)
			continue
		}
		if status.State == toolbroker.BackgroundRunning {
			return true
		}
	}
	return false
}

// sortedEnvKeys is the KEY NAMES of an attempt's environment, sorted. Nothing here touches a value, and
// the sort is not cosmetic: an unsorted column would differ between two otherwise identical spawns, and
// a column that changes for no reason is a column a diff cannot be read against.
func sortedEnvKeys(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// BackgroundObserver is what this orchestrator hands the reconciler: the function that answers, for one
// background_tasks row, what the OPERATING SYSTEM says about its handle and what its output ends with
// (E26 T4). Nil when no background runner is wired, which makes the sweep a no-op — the honest reading
// of a deployment that cannot start a background task in the first place.
//
// It is a method value rather than a captured closure so that a composition-root test can ask an
// orchestrator what production would hand over, the same reason backgroundRunnerFor is a named function
// in main.go.
func (o *Orchestrator) BackgroundObserver() coordinator.BackgroundObserver {
	if o == nil || o.background == nil {
		return nil
	}
	return o.observeBackgroundTask
}

// observeBackgroundTask probes ONE row's handle and, if the task has finished, reads the bounded tail of
// its log.
//
// THE ROW IS THE ONLY INPUT. Nothing here consults the in-memory registry, and that is what makes the
// sweep work from a control plane that never saw the spawn: the handle, the posture and the output path
// all come off the row, and the answer comes off the kernel or the container daemon.
//
// A STILL-RUNNING TASK IS THE COMMON ANSWER and costs one probe. An UNREADABLE probe is an error rather
// than a guess: reporting `exited` for a build that is still compiling would tell a model its work
// finished when it did not, and the next tick can ask again. A `lost` handle IS reported as finished,
// which is not the same as claiming it died — it means this control plane can no longer prove the
// process is ours, so it will never be signalled again (T1's absolute rule) and leaving the row
// `running` forever would park its run on a question nothing can answer.
func (o *Orchestrator) observeBackgroundTask(ctx context.Context, task coordinator.BackgroundTask) (coordinator.BackgroundOutcome, bool, error) {
	if o.background == nil {
		return coordinator.BackgroundOutcome{}, false, toolbroker.ErrBackgroundUnsupported
	}
	handle := toolbroker.Handle{Posture: toolbroker.BackgroundPosture(task.Posture), Value: task.Handle}
	status, err := o.background.Probe(ctx, handle)
	if err != nil {
		return coordinator.BackgroundOutcome{}, false, err
	}
	var state string
	switch status.State {
	case toolbroker.BackgroundRunning:
		return coordinator.BackgroundOutcome{}, false, nil
	case toolbroker.BackgroundLost:
		state = string(toolbroker.BackgroundLost)
	default:
		state = string(toolbroker.BackgroundExited)
	}
	tail, note := o.backgroundTail(task)
	return coordinator.BackgroundOutcome{State: state, ExitCode: status.ExitCode, Tail: tail, TailNote: note}, true, nil
}

// backgroundTail reads the LAST bytes of a finished task's output, bounded by
// coordinator.BackgroundTailLimit, and explains itself when it can read nothing.
//
// IT IS A BOUNDED READ, NOT A READ THEN A TRUNCATION: a five-minute build can write a log larger than
// this control plane's memory, so the file is seeked to its end rather than loaded. The path is joined
// from the allocation root the sweep resolved and the row's allocation-RELATIVE output_path, which is
// the same pair every other reader of that file uses.
//
// A MISSING TAIL NEVER STOPS A NOTIFICATION. An unreadable file, a run with no allocation, a log the
// retention reaper already removed — each yields a note the model is told instead of an excerpt, because
// losing the exit is strictly worse than losing the excerpt.
//
// ponytail: THIS EXCERPT IS NOT REDACTED, AND NEITHER IS THE FILE palai.workspace.file READS. The bytes
// here cross the same boundary as an ordinary read of the same path (E26 T2's recorded gap, §3.6 D8) —
// this widens nothing — but T6 owns the redacting read path, and when it lands THIS CALL IS ONE OF THE
// TWO IT HAS TO COVER. The row's env_keys column exists for exactly that: the keys are on task.EnvKeys.
func (o *Orchestrator) backgroundTail(task coordinator.BackgroundTask) (tail string, note string) {
	if task.AllocationRoot == "" {
		return "", "this run has no workspace allocation on this machine, so its output file could not be located"
	}
	abs, err := toolbroker.BackgroundSpec{TaskID: task.ID, OutputPath: task.OutputPath}.Resolve(task.AllocationRoot)
	if err != nil {
		return "", "the output path could not be resolved against this run's allocation"
	}
	f, err := os.Open(abs)
	if err != nil {
		return "", "the output file could not be opened (it may have been removed)"
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", "the output file could not be read"
	}
	size := info.Size()
	offset := int64(0)
	if size > coordinator.BackgroundTailLimit {
		offset = size - coordinator.BackgroundTailLimit
	}
	buf := make([]byte, size-offset)
	if _, err := f.ReadAt(buf, offset); err != nil && len(buf) > 0 {
		return "", "the output file could not be read"
	}
	return string(buf), ""
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
