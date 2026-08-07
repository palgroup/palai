package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/packages/runner"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// This file is the orchestration BETWEEN the tool surface and the two sandbox adapters (E26 T2): it
// mints a task id, derives the log path, decides whether the deployment allows a background task at all,
// and answers which handle a task id names. The adapters own process groups and container ids; the tools
// own a task id and a path; nothing crosses.
//
// SINCE T4 IT ALSO WRITES THE ROW migration 000047 opened, and reads it back on the way out. The row is
// what makes a task survive the process that started it, and the reconciler's sweep — which may be
// running in a DIFFERENT control plane, or in one that restarted since the spawn — works entirely from it.
//
// AND SINCE T5 THE ROW IS THE ONLY RECORD THERE IS. T2's in-memory registry is gone: it answered "which
// handle does this task id name" for as long as this process lived, which meant a control plane that
// restarted could not stop a build it had started, and a run adopted after a restart would have COMPLETED
// while its build was still compiling. E24 T5's lesson, applied verbatim — an in-memory flag is erased by
// a restart and a column is not. What is left of that map is one mutex, serialising the ceiling count
// against the spawn it guards.

// backgroundLogDir is the subtree a background task's merged output goes into. It is under
// `.palai-session` for a reason that was measured rather than chosen: Snapshot skips that whole subtree
// (adapters/sandboxes/oci/workspace/allocation.go), so a log does not enter a snapshot, does not enter a
// changeset and does not enter a checksum — while palai.workspace.file still reads it, because it is an
// ordinary path under the allocation root.
//
// AND THE READ OF THAT FILE IS REDACTED SINCE T6, THE FILE ITSELF IS NOT. A synchronous shell result is
// redacted on the way back (host/exec.go, workspace/exec.go apply RedactSecrets/RedactValues to the
// CAPTURED GO STRING); a process writing its own log file bypasses both, so the redaction moved to the
// READ path — redactTaskOutput, re-resolving the task's env_keys at read time because the row holds key
// names and never values (§3.6 D8). The bytes on disk stay verbatim, which is what an operator debugging
// a build wants and is why the file is 0600 under a subtree Snapshot skips whole.
const backgroundLogDir = ".palai-session/bg"

// errBackgroundDisabled is the kill switch's answer, and it is a REFUSAL rather than a downgrade. A
// silent fall back to a synchronous run would leave the model believing it had backgrounded something
// that is in fact blocking it, which is precisely the behaviour background execution exists to prevent —
// so the deployment that turns the feature off makes the call fail, and the model can do something with
// a failure. (E26 §3.5 P5, taken from the vendor's own CLAUDE_CODE_DISABLE_BACKGROUND_TASKS.)
var errBackgroundDisabled = errors.New("background execution is disabled on this deployment (PALAI_BACKGROUND_DISABLED); the background parameter is refused rather than run synchronously")

// BackgroundDisabled reports the deployment kill switch, and it FAILS CLOSED on a value it cannot read:
// an operator who wrote `PALAI_BACKGROUND_DISABLED=yes` meant to switch the feature off, and answering
// "that is not a boolean, so the feature stays on" would be the least useful reading available.
//
// It is read here, per call, rather than captured once at wiring time — the same reason
// backgroundRunnerFor is a named function in main.go: a test that builds its own configuration never
// exercises the configuration production builds, and this way the switch is reached through the
// production path or not at all.
func BackgroundDisabled() bool {
	raw := strings.TrimSpace(os.Getenv("PALAI_BACKGROUND_DISABLED"))
	if raw == "" {
		return false
	}
	off, err := strconv.ParseBool(raw)
	return err != nil || off
}

// THE FOUR CEILINGS OF E26 T5, AND WHY THEY ARE READ HERE RATHER THAN AT THE COMPOSITION ROOT.
//
// Every one of them is read per call, through the function production calls, for BackgroundDisabled's
// reason: a test that builds its own configuration never exercises the configuration production builds.
// The plan's seam line said "main.go (three env)"; three of these four are read at SPAWN time, inside the
// dispatcher, and capturing them in main.go would have put production's own defaults out of reach of
// every component test that constructs an Orchestrator — which is exactly the trap that let a shell wall
// time be simultaneously unbounded on the host and refusing every call in the container while every
// sandbox test was green. main.go names them in a comment beside the reconciler so an operator reading
// the composition root still finds them; docs/operations/background-execution.md is where they are
// documented.
const (
	// defaultBackgroundMaxWallTime is §0.2's decision and the plan could not leave it empty. E24 T5 made
	// PALAI_FLEET_PARK_TTL default to NEVER and was right — there is no honest default for how long a
	// rented Mac takes to arrive. HERE THE SITUATION IS REVERSED: an unbounded background process is
	// precisely the named outcome, "an orphan compiling for an hour on an operator's Mac". An hour covers
	// an xcodebuild plus its test suite and does not cover an overnight orphan.
	defaultBackgroundMaxWallTime = 60 * time.Minute
	// Five parallel builds is the most a model can follow; a sixth is a signal that something is wrong
	// with the model's plan rather than a workload to admit (§0.3).
	defaultBackgroundMaxPerRun = 5
	// Twenty concurrent compiles already finishes a Mac. The number is a door, not a target (§0.3).
	defaultBackgroundMaxPerHost = 20
	// A day of build logs is what an operator debugging yesterday's failure needs; anything older is the
	// silent disk leak this sweep exists to prevent.
	defaultBackgroundLogTTL = 24 * time.Hour
)

// BackgroundMaxWallTime is the deployment's ceiling on a background task's life, and its `bounded` half
// carries the one thing a duration cannot say: whether the operator ASKED for no ceiling.
//
// UNSET IS BOUNDED. `PALAI_BACKGROUND_MAX_WALL_TIME=0` is unbounded and has to be written, so the silent
// case is the safe one — the exact inverse of PALAI_FLEET_PARK_TTL, and the difference is that here the
// unbounded case is the failure the epic is named after.
//
// A VALUE THIS CANNOT PARSE FALLS BACK TO THE CEILING RATHER THAN TO INFINITY. An operator who typed
// `60min` meant to bound something, and reading a typo as "run forever" is the least useful answer
// available.
func BackgroundMaxWallTime() (limit time.Duration, bounded bool) {
	raw := strings.TrimSpace(os.Getenv("PALAI_BACKGROUND_MAX_WALL_TIME"))
	if raw == "" {
		return defaultBackgroundMaxWallTime, true
	}
	d, err := time.ParseDuration(raw)
	switch {
	case err != nil:
		log.Printf("PALAI_BACKGROUND_MAX_WALL_TIME=%q is not a duration; using the %s default", raw, defaultBackgroundMaxWallTime)
		return defaultBackgroundMaxWallTime, true
	case d == 0:
		return 0, false // explicitly unbounded
	case d < 0:
		log.Printf("PALAI_BACKGROUND_MAX_WALL_TIME=%q is negative; using the %s default", raw, defaultBackgroundMaxWallTime)
		return defaultBackgroundMaxWallTime, true
	}
	return d, true
}

// BackgroundMaxPerRun and BackgroundMaxPerHost are §0.3's two ceilings. A non-positive value is an
// explicit opt-out and means no ceiling — the same shape as the wall time's zero, and the same rule: the
// unbounded reading requires a written value.
func BackgroundMaxPerRun() int {
	return envCeiling("PALAI_BACKGROUND_MAX_PER_RUN", defaultBackgroundMaxPerRun)
}
func BackgroundMaxPerHost() int {
	return envCeiling("PALAI_BACKGROUND_MAX_PER_HOST", defaultBackgroundMaxPerHost)
}

func envCeiling(name string, def int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		log.Printf("%s=%q is not a number; using the default of %d", name, raw, def)
		return def
	}
	return n
}

// BackgroundLogTTL bounds how long a finished task's output file stays on the allocation. Unset and
// unparseable both mean the 24h default: there is no opt-out spelling here, because the opt-out is an
// unbounded `.palai-session` that Snapshot skips (§3.6 D7) and that therefore nobody would ever see fill
// a disk. An operator who wants to keep logs longer writes a longer duration.
func BackgroundLogTTL() time.Duration {
	raw := strings.TrimSpace(os.Getenv("PALAI_BACKGROUND_LOG_TTL"))
	if raw == "" {
		return defaultBackgroundLogTTL
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		log.Printf("PALAI_BACKGROUND_LOG_TTL=%q is not a positive duration; using the %s default", raw, defaultBackgroundLogTTL)
		return defaultBackgroundLogTTL
	}
	return d
}

// errBackgroundAtRunCeiling and errBackgroundAtHostCeiling are §0.3's refusals, and they REFUSE rather
// than QUEUE. A queue would be a delay the model cannot see — it would believe five builds were running
// while one waited — and a refusal is an answer a model can act on: wait for one to finish, kill one, or
// do the work synchronously. It is also the difference between this ceiling and `runners.capacity`, which
// E24 declared in a CHECK constraint and no expression ever read (§3.6 D12).
var (
	errBackgroundAtRunCeiling  = errors.New("refused: this run is already at its background task limit (PALAI_BACKGROUND_MAX_PER_RUN); wait for one to finish or stop one with palai.workspace.background_kill")
	errBackgroundAtHostCeiling = errors.New("refused: this machine is already at its background task limit (PALAI_BACKGROUND_MAX_PER_HOST); wait for one to finish or stop one with palai.workspace.background_kill")
)

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
	orch *Orchestrator
	// machineID is the machine THIS attempt reached, learned from its channel (machineOf). A task
	// started here runs there and its row records it, which is what lets a reconciler with no lease
	// address the right kernel long after this attempt ended.
	machineID  string
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
	if BackgroundDisabled() {
		return toolbroker.BackgroundTicket{}, errBackgroundDisabled
	}
	// THE GATE IS THE MACHINE NOW, NOT THIS PROCESS (A.3 T7). It used to ask whether the CONTROL PLANE
	// could detach, which is a question about the wrong host once the task runs elsewhere. An attempt
	// with no machine has nowhere to put the process, and saying so here is what keeps a backgrounded
	// command from quietly running beside the control plane.
	if b.orch == nil || b.orch.machines == nil || b.machineID == "" {
		return toolbroker.BackgroundTicket{}, toolbroker.ErrBackgroundUnsupported
	}

	// THE CEILINGS, BEFORE ANYTHING STARTS (E26 T5, §0.3). The counts come from the DATABASE — the rows
	// that record every live process on this machine — rather than from anything this program remembers,
	// which is what makes them true for a control plane that restarted and what stops this repeating
	// `runners.capacity`, a column E24 declared and no expression ever read (§3.6 D12).
	//
	// THE MUTEX IS THE SERIALISATION POINT and its ceiling is named rather than hidden: the count and the
	// start are two statements, so two spawns racing between them could both be admitted. One process is
	// what this lock covers, and under §3.6 D4 there is exactly one control plane and one machine — the
	// execution relay that would make a second one possible is E27's, and closing the gap for real means
	// counting inside the transaction that inserts the row, which cannot happen until the handle exists.
	b.orch.bgSpawn.Lock()
	defer b.orch.bgSpawn.Unlock()
	perRun, perHost, err := b.orch.spine.CountRunningBackgroundTasks(ctx, b.tenant, b.runID)
	if err != nil {
		return toolbroker.BackgroundTicket{}, err
	}
	if limit := BackgroundMaxPerRun(); limit > 0 && perRun >= limit {
		return toolbroker.BackgroundTicket{}, fmt.Errorf("%w (%d of %d running)", errBackgroundAtRunCeiling, perRun, limit)
	}
	if limit := BackgroundMaxPerHost(); limit > 0 && perHost >= limit {
		return toolbroker.BackgroundTicket{}, fmt.Errorf("%w (%d of %d running)", errBackgroundAtHostCeiling, perHost, limit)
	}

	taskID := newExecID("bgt")
	spec := toolbroker.BackgroundSpec{TaskID: taskID, OutputPath: path.Join(backgroundLogDir, taskID+".log")}

	// THE DEADLINE IS COMPUTED BEFORE THE PROCESS EXISTS so that the row can carry it in the same insert
	// the handle arrives in. There is no window in which a running task has no ceiling recorded.
	var deadline *time.Time
	if limit, bounded := BackgroundMaxWallTime(); bounded {
		at := time.Now().Add(limit)
		deadline = &at
	}

	// UNBOUNDED PLUS A CREDENTIAL IS REFUSED (E26 T6, §0.4's price for choosing option (b)). A value handed
	// over in cmd.Env lives in the kernel's environ copy for the WHOLE LIFE of the process, so a task with
	// no ceiling is a credential with no expiry — and §0.4 bought the right to hand one over by promising
	// the exposure would be bounded. This is where that promise is kept.
	//
	// IT IS A CODE GATE AND NOT A CHECK CONSTRAINT, and the difference is a user decision the database
	// cannot see: `PALAI_BACKGROUND_MAX_WALL_TIME=0` is a legitimate choice for a task that carries nothing,
	// so `deadline_at IS NULL` cannot be forbidden by the column. What is forbidden is the PAIR, and the
	// pair is only knowable here, where the environment and the ceiling are both in hand.
	//
	// It refuses BEFORE the process exists, like every other refusal in this function.
	if deadline == nil && len(cmd.Env) > 0 {
		return toolbroker.BackgroundTicket{}, fmt.Errorf("refused: this run carries %d environment value(s) and "+
			"PALAI_BACKGROUND_MAX_WALL_TIME is 0 (unbounded). A background process holds its environment for its whole "+
			"life, so an unbounded task would hold a credential without expiry; set a ceiling or run this command "+
			"synchronously", len(cmd.Env))
	}

	// THE SPAWN CROSSES TO THE MACHINE (A.3 T7). Until now this ran in the control plane, so a task a
	// model backgrounded on a Mac was a process beside the control plane — and every row carried this
	// process's identity, which made T6's routing agree with itself and fire nowhere. It runs where the
	// attempt's synchronous commands run, and the handle it returns names THAT machine's kernel.
	answer, err := b.orch.callMachine(ctx, b.machineID, runner.BackgroundStartType, map[string]any{
		"command": cmd,
		"spec":    spec,
	})
	if err != nil {
		return toolbroker.BackgroundTicket{}, fmt.Errorf("start a background task on machine %s: %w", b.machineID, err)
	}
	handle, err := decodeMachineHandle(answer)
	if err != nil {
		return toolbroker.BackgroundTicket{}, err
	}

	// THE DURABLE ROW, and it is written for a reader that is not this program (E26 T4). It is the ONLY
	// record of the task: since T5 there is no in-memory registry beside it, because a map dies with the
	// process that made it and the whole point of a background task is to outlive that process. The row
	// answers "which handle does this run's task id name" for the reconciler, for a restarted control
	// plane, and for the kill tool.
	//
	// A FAILED INSERT KILLS THE PROCESS. It is the one place in this file that undoes work rather than
	// reporting around it, and the reason is what a row-less task IS: a process running under a handle
	// nothing durable holds, which no reaper will look at, no deadline will end and no cancellation will
	// reach. That is precisely the orphan this epic exists to make impossible, so a task that cannot be
	// recorded is a task that must not be left running.
	if err := b.orch.spine.RecordBackgroundTask(ctx, b.tenant, coordinator.BackgroundTaskInput{
		TaskID: taskID, RunID: b.runID, SessionID: b.sessionID, ResponseID: b.responseID,
		ToolCallID: string(callID), Fence: b.fence,
		Posture: string(handle.Posture), Handle: handle.Value, MachineID: b.machineID, OutputPath: spec.OutputPath,
		DeadlineAt: deadline,
		// env_keys holds KEY NAMES so the read path can re-resolve and mask them (T6). The VALUES are in
		// cmd.Env and stay there; sorted so the column does not depend on Go's map iteration order.
		EnvKeys: sortedEnvKeys(cmd.Env),
	}); err != nil {
		if _, kerr := b.orch.callMachine(ctx, b.machineID, runner.BackgroundKillType, map[string]any{"handle": handle}); kerr != nil {
			// Both failures matter and neither replaces the other: the caller learns the task was refused,
			// and the log names the process that is now genuinely unaccounted for.
			log.Printf("background task %s could not be recorded (%v) AND could not be killed (%v): handle %s is now an orphan", taskID, err, kerr, handle.Value)
		}
		return toolbroker.BackgroundTicket{}, err
	}

	// `status` is what the operating system says, not what we hope: a command that has already failed
	// reports `exited` rather than a comfortable `running`, and a model reading `running` can trust it.
	//
	// A probe that cannot answer does NOT fail the call — losing the task id of a process that is already
	// running is strictly worse than one imprecise status field, and `running` is the honest reading of
	// "Start succeeded and nothing has told us otherwise".
	state := toolbroker.BackgroundRunning
	if status, perr := b.orch.probeOnMachine(ctx, b.machineID, handle); perr == nil {
		state = status.State
	}
	return toolbroker.BackgroundTicket{TaskID: taskID, OutputPath: spec.OutputPath, State: state}, nil
}

// KillBackground stops one task of this run. An id this run did not start is REFUSED rather than
// answered: a model that mistyped an id must not be told the task is dead, and a run must not be able to
// reach into another run's processes by guessing.
//
// THE LOOKUP IS THE ROW (E26 T5). It used to be a map built at spawn time, which meant a control plane
// that restarted could not stop a build it had started — the ceiling T2 recorded as a test and this task
// closes. E24 T5's lesson applies verbatim: an in-memory flag is erased by a restart, a column is not.
func (b *backgroundTasks) KillBackground(ctx context.Context, taskID string) (toolbroker.BackgroundTicket, error) {
	if b.orch == nil || b.orch.machines == nil {
		return toolbroker.BackgroundTicket{}, toolbroker.ErrBackgroundUnsupported
	}
	task, err := b.orch.spine.BackgroundTaskForRun(ctx, b.tenant, b.runID, taskID)
	if err != nil {
		if errors.Is(err, coordinator.ErrNoBackgroundTask) {
			return toolbroker.BackgroundTicket{}, fmt.Errorf("no background task %q belongs to this run", taskID)
		}
		return toolbroker.BackgroundTicket{}, err
	}
	// A TASK ON ANOTHER MACHINE IS NOT KILLED FROM HERE (A.3 T6). The signal would go to a process group
	// of that number on THIS box, which is some other process or none — a pgid is a small integer. The
	// model is told, rather than left to believe a build it asked to stop is stopping.
	handle := toolbroker.Handle{Posture: toolbroker.BackgroundPosture(task.Posture), Value: task.Handle, MachineID: task.MachineID}
	// WAS IT STILL RUNNING WHEN WE WERE ASKED. The answer decides whether the row records a kill, and it
	// has to be taken BEFORE the signal because afterwards both cases look identical. A probe that cannot
	// answer is treated as "not ours to claim" — the sweep will say what happened.
	wasRunning := false
	if status, perr := b.orch.probeOnMachine(ctx, task.MachineID, handle); perr == nil {
		wasRunning = status.State == toolbroker.BackgroundRunning
	}
	if _, err := b.orch.callMachine(ctx, task.MachineID, runner.BackgroundKillType, map[string]any{"handle": handle}); err != nil {
		// A MACHINE WE CANNOT REACH IS NOT SIGNALLED AND THE MODEL IS TOLD (T6's rule, kept). An answer
		// rather than an error: the task the model asked to stop may still be running, and the run
		// continues knowing that instead of ending as uncertain.
		if errors.Is(err, ErrMachineUnreachable) {
			return toolbroker.BackgroundTicket{}, toolbroker.Answerf(toolbroker.AnswerUnavailable,
				"background task %s runs on a machine this control plane cannot reach, so it was not stopped", taskID)
		}
		return toolbroker.BackgroundTicket{}, err
	}
	// A TASK WE ACTUALLY STOPPED IS SETTLED AS `killed` RATHER THAN LEFT FOR THE SWEEP, and the reason is
	// what the sweep would otherwise do: notice the process gone, record `exited`, and notify the model
	// about a task the model itself had just stopped — which on a run that finished in between becomes an
	// orphaned-notice warning an operator has to explain. A kill we performed needs no notification.
	//
	// A TASK THAT HAD ALREADY FINISHED IS LEFT ALONE, and that asymmetry is the point. Recording `killed`
	// for a build that exited on its own thirty seconds earlier would put a false statement in the audit
	// record AND throw away the exit code the model asked about, because the sweep would then find the row
	// already settled. Killing a finished task stays idempotent; it simply claims nothing.
	//
	// A failure to record is logged and not fatal: the process IS dead, which is what the caller asked for.
	if wasRunning {
		if err := b.orch.spine.SettleEndedBackgroundTask(ctx, b.tenant, b.runID, task.ID, "killed"); err != nil {
			log.Printf("background task %s was killed but its row could not be settled: %v", task.ID, err)
		}
	}
	// The state is read back from the operating system rather than assumed from the kill: a handle the
	// adapter refused to signal (a pgid it cannot prove is ours) reports `lost` here, and the model is told
	// that rather than being told the task is dead.
	state := toolbroker.BackgroundExited
	if status, perr := b.orch.probeOnMachine(ctx, task.MachineID, handle); perr == nil {
		state = status.State
	}
	return toolbroker.BackgroundTicket{TaskID: task.ID, OutputPath: task.OutputPath, State: state}, nil
}

// RedactOutput masks this run's credentials in bytes read back OUT of its workspace (E26 T6). It is the
// file tool's half of the one redaction place, and it delegates without deciding anything: the policy,
// the two redactors and the fail-closed rule are all in redactTaskOutput, so this site cannot drift from
// the notice's.
//
// IT PASSES NO `carried` SET, and the reason is worth writing down rather than discovering later. The
// notice knows exactly which keys ITS task held, because the row records them; a file read knows only a
// path, and deciding which task a path names would be a path comparison deciding a security outcome —
// which every such comparison in this tree has shipped defeated at least once. So the read masks with
// every value the run resolves NOW, and the one case that leaves open is named: a key deleted from the
// environment after the task wrote its log is a value nothing can re-resolve and therefore nothing can
// mask, and the READ (unlike the notice) has no record that it was ever there.
func (b *backgroundTasks) RedactOutput(ctx context.Context, s string) (string, error) {
	if b.orch == nil {
		return s, nil
	}
	return b.orch.redactTaskOutput(ctx, b.tenant, b.runID, nil, s)
}

// BackgroundKiller is what this orchestrator hands the coordinator so that CANCELLING A RUN ENDS ITS LIVE
// WORK (E26 T5, §3.6 D10). It is the kill half of the same seam BackgroundObserver is the probe half of:
// the coordinator owns the transaction and the state vocabulary, this package owns the signal.
//
// It reports the STATE the kill leaves the row in rather than an error to be classified, because a handle
// that cannot be proven to be ours is not a failure — it is the answer `lost`, and the rule built on it is
// absolute: it receives no signal, on a deadline, on a cancellation or on a sweep.
func (o *Orchestrator) BackgroundKiller() coordinator.BackgroundKiller {
	if o == nil || o.machines == nil {
		return nil
	}
	return func(ctx context.Context, task coordinator.BackgroundTask) (string, error) {
		// Same rule as every other signal site: a handle this machine cannot prove is ours is never
		// signalled. `lost` is what the row records, which is also what it would record for a handle that
		// stopped matching locally — the run is cancelled either way, and no stranger's process is hit.
		handle := toolbroker.Handle{Posture: toolbroker.BackgroundPosture(task.Posture), Value: task.Handle, MachineID: task.MachineID}
		_, kerr := o.callMachine(ctx, task.MachineID, runner.BackgroundKillType, map[string]any{"handle": handle})
		if errors.Is(kerr, ErrMachineUnreachable) {
			// Unreachable is `lost`, which is also what a handle that stopped matching locally records.
			// The run is cancelled either way and no stranger's process is signalled.
			return string(toolbroker.BackgroundLost), nil
		}
		switch err := kerr; {
		case errors.Is(err, toolbroker.ErrHandleLost):
			return string(toolbroker.BackgroundLost), err
		case err != nil:
			return "", err
		}
		return "killed", nil
	}
}

// runHasLiveBackgroundTask reports whether this run owns a background task the OPERATING SYSTEM still
// says is running (E26 T3). It is what decides whether a completed run parks instead of finishing.
//
// THE CANDIDATE SET IS THE ROW AND THE AUTHORITY IS THE PROBE. Until E26 T5 the candidates came from a
// map built at spawn time, which answered wrongly after a restart in the direction that matters: an
// adopted run with a live build would have COMPLETED, and the exit notification would then have had no
// turn to land in. The rows survive the restart; the kernel — or the container daemon — still answers the
// question that matters, which is the discipline T1 built the whole seam on.
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
func (o *Orchestrator) runHasLiveBackgroundTask(ctx context.Context, tenant coordinator.Tenant, runID string) bool {
	if o.machines == nil {
		return false
	}
	mine, err := o.spine.RunningBackgroundTasksOfRun(ctx, tenant, runID)
	if err != nil {
		log.Printf("read the background tasks of run %s: %v", runID, err)
		return false
	}
	for _, task := range mine {
		// A task on another machine cannot be probed here, and this gate must NOT read a local answer for
		// it: an `exited` from the wrong kernel would let a run finish while its build is still compiling
		// elsewhere. Not-ours counts as not-live for the park decision — the sweep settles it as `lost`.
		handle := toolbroker.Handle{Posture: toolbroker.BackgroundPosture(task.Posture), Value: task.Handle, MachineID: task.MachineID}
		status, err := o.probeOnMachine(ctx, task.MachineID, handle)
		if err != nil {
			log.Printf("probe background task %s of run %s: %v", task.ID, runID, err)
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
	if o == nil || o.machines == nil {
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
	if o.machines == nil {
		return coordinator.BackgroundOutcome{}, false, toolbroker.ErrBackgroundUnsupported
	}
	handle := toolbroker.Handle{
		Posture:   toolbroker.BackgroundPosture(task.Posture),
		Value:     task.Handle,
		MachineID: task.MachineID,
	}
	// THE MACHINE IS CHECKED BEFORE THE KERNEL IS ASKED (A.3 T6), and it has to be BEFORE rather than
	// after: this executor's answer for another machine's handle is not an error to interpret, it is a
	// confident `exited` for a process that may still be compiling. kill(pgid, 0) returns ESRCH both for
	// a group that ended and for one that was never here, so there is nothing in the answer to tell them
	// apart afterwards.
	//
	// `lost` is not a compromise here, it is the definition: the process may still be running and this
	// control plane cannot prove it is ours. It carries the safety property too — a lost handle is never
	// signalled (T1's absolute rule), which is what stops the reaper from sending a deadline kill to a
	// pgid that belongs to something else on this box.
	// AND NO TAIL IS READ ON THIS PATH, which is a second decision and not an omission. backgroundTail
	// resolves the row's output path against an allocation root and opens it HERE; the file a task on
	// another machine wrote is on that machine. Reading anyway would usually find nothing, and on a
	// shared filesystem it could find a DIFFERENT run's file at the same relative path — an excerpt from
	// somebody else's build, handed to a model as this task's output. The note says why instead.
	status, err := o.probeOnMachine(ctx, task.MachineID, handle)
	switch {
	case errors.Is(err, ErrMachineUnreachable):
		// THE MACHINE IS NOT HERE — powered off, revoked, or a row that names none. `lost` is the honest
		// state and the definition already says why: the process may still be running and this control
		// plane cannot prove it is ours. It carries the safety rule too, because a lost handle is never
		// signalled.
		//
		// AND NO TAIL IS READ (T6's decision, kept). The file a task wrote is on its machine; reading a
		// local path anyway would usually find nothing, and on a shared filesystem it could find a
		// DIFFERENT run's output at the same relative path and hand it to a model as this task's.
		return coordinator.BackgroundOutcome{
			State:    string(toolbroker.BackgroundLost),
			TailNote: "the machine this task runs on could not be reached, so neither its state nor its output can be read from here",
		}, true, nil
	case err != nil:
		return coordinator.BackgroundOutcome{}, false, err
	}
	var state string
	switch status.State {
	case toolbroker.BackgroundRunning:
		// THE WALL CLOCK, AND THE REAPER IS ITS ONLY ENFORCER (E26 T5, §0.2 / §3.6 D2, D11). Not a
		// context: a context dies with the process that created it and a background task exists to
		// outlive that process, so the ceiling has to be a column somebody reads. This is where it is
		// read, on the loop that already spans tenants and is already supervised.
		if task.DeadlineAt == nil || time.Now().Before(*task.DeadlineAt) {
			return coordinator.BackgroundOutcome{}, false, nil
		}
		expired, eerr := o.expireBackgroundTask(ctx, task, handle)
		if eerr != nil {
			return coordinator.BackgroundOutcome{}, false, eerr
		}
		status = expired
		state = string(status.State)
	case toolbroker.BackgroundLost:
		state = string(toolbroker.BackgroundLost)
	default:
		state = string(toolbroker.BackgroundExited)
	}
	tail, note := o.backgroundTail(ctx, task)
	return coordinator.BackgroundOutcome{State: state, ExitCode: status.ExitCode, Tail: tail, TailNote: note}, true, nil
}

// probeOnMachine asks the machine that STARTED a task what became of it, over the same lease-free
// channel `callMachine` opens (A.3 T7). It dials; it does not compare.
//
// THIS COMMENT USED TO SAY THE OPPOSITE, AND THE CORRECTION IS WORTH KEEPING. It documented a
// `reachesMachine` helper — since deleted — and said "there is no protocol by which one control plane
// probes another machine's process group, so every machine that is not this one is unreachable". That
// was true of the routing seam T6 landed and FALSE the moment T7 landed the `bg.*` verbs, which is
// the function this sentence was sitting on top of. Three branches are true now and the count is the
// point: a machine that answers gives its real state, a machine that cannot be addressed is `lost`,
// and a row that names no machine is `lost` for the separate reason below.
//
// AN EMPTY MACHINE ON THE ROW IS UNREACHABLE, INCLUDING WHEN THIS PLANE IS ALSO UNNAMED. Two unknowns
// are not a match: a row written before the column existed says nothing about where it ran, and a
// deployment that never named its machine cannot claim it. Comparing them as equal would resolve the
// oldest rows — the ones with the least evidence — to a local probe, which is exactly the wrong answer
// this seam exists to stop.
func (o *Orchestrator) probeOnMachine(ctx context.Context, machineID string, handle toolbroker.Handle) (toolbroker.BackgroundStatus, error) {
	answer, err := o.callMachine(ctx, machineID, runner.BackgroundProbeType, map[string]any{"handle": handle})
	if err != nil {
		return toolbroker.BackgroundStatus{}, err
	}
	raw, ok := answer["status"]
	if !ok {
		return toolbroker.BackgroundStatus{}, errors.New("the machine answered a probe and named no status")
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return toolbroker.BackgroundStatus{}, fmt.Errorf("encode the machine's status: %w", err)
	}
	var status toolbroker.BackgroundStatus
	if err := json.Unmarshal(encoded, &status); err != nil {
		return toolbroker.BackgroundStatus{}, fmt.Errorf("decode the machine's status: %w", err)
	}
	return status, nil
}

// backgroundExpired is the state a task reaches when the reaper ended it for outliving its ceiling. It is
// spelled here rather than in packages/tool-broker because it is not something a PROBE can ever report:
// the operating system knows a process was killed, and only the row knows the reaper decided to.
const backgroundExpired = "expired"

// expireBackgroundTask ends a task that outlived its deadline and reports how that left it.
//
// A LOST HANDLE IS NOT KILLED, on a deadline any more than anywhere else. The rule is absolute and this
// is one of the three places that has to honour it: the recorded evidence no longer matches the live
// process, so the process may be anybody's, and an orphan of ours is a better outcome than a stranger's
// dead process.
//
// THE EXIT CODE IS RE-READ FROM THE OPERATING SYSTEM AND MAY BE NULL. On the host the watcher may not have
// reaped the group yet; in the container posture the kill removes the container and takes its exit status
// with it. Both give NULL, which is the honest answer — the row's `expired` already carries the fact that
// matters, and inventing a number would put a sentinel in the one column whose whole point is not having
// one.
func (o *Orchestrator) expireBackgroundTask(ctx context.Context, task coordinator.BackgroundTask, handle toolbroker.Handle) (toolbroker.BackgroundStatus, error) {
	_, kerr := o.callMachine(ctx, task.MachineID, runner.BackgroundKillType, map[string]any{"handle": handle})
	switch err := kerr; {
	case errors.Is(err, toolbroker.ErrHandleLost):
		return toolbroker.BackgroundStatus{State: toolbroker.BackgroundLost}, nil
	case err != nil:
		// The next tick asks again. A deadline that could not be enforced this pass is not a reason to
		// stop the whole sweep, and it is certainly not a reason to record a task as ended that is not.
		return toolbroker.BackgroundStatus{}, fmt.Errorf("expire background task %s: %w", task.ID, err)
	}
	out := toolbroker.BackgroundStatus{State: backgroundExpired}
	if status, perr := o.probeOnMachine(ctx, task.MachineID, handle); perr == nil {
		out.ExitCode = status.ExitCode
	}
	return out, nil
}

// sweepBackgroundLogs deletes the output files of tasks that finished longer ago than
// PALAI_BACKGROUND_LOG_TTL. Without it `.palai-session` grows without bound — and because Snapshot skips
// that subtree as a whole (§3.6 D7) it enters no snapshot, no changeset and no checksum, so NOBODY WOULD
// NOTICE. That is precisely what a silent disk leak looks like.
//
// IT IS DRIVEN BY DIRECTORIES AND NOT BY ROWS, and that is what makes it stateless. There is no column
// marking a log as deleted — E26's one migration belongs to T1 and this task owns none — so a sweep
// driven by task rows would re-examine every task that ever ran, on every tick, forever. Driven by the
// allocations that exist, it is self-limiting: a file it deletes is never listed again, and an allocation
// that is gone took its logs with it.
//
// THE CLOCK IS THE FILE'S MTIME rather than the row's finished_at, for the same reason, and the exclusion
// is by TASK ID rather than by age: a build that prints nothing for a day is not a finished build, so a
// still-running task's log is skipped no matter how old its last line is.
//
// The container posture's other half of the garbage question is already shipped and is not repeated here:
// KillDetached is a ContainerKill followed by a ContainerRemove, so a killed container leaves no writable
// layer behind, and tests/component/background counts sandbox containers before and after to prove it.
func sweepBackgroundLogs(ctx context.Context, store ReconcileStore) error {
	ttl := BackgroundLogTTL()
	roots, live, err := store.BackgroundLogRetention(ctx)
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-ttl)
	for _, root := range roots {
		dir := filepath.Join(root, backgroundLogDir)
		entries, err := os.ReadDir(dir)
		if err != nil {
			// No bg subtree on this allocation, or one this process cannot read. Neither is an error for
			// the pass: an allocation that never ran a background task is the common case.
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			if live[strings.TrimSuffix(entry.Name(), ".log")] {
				continue
			}
			info, err := entry.Info()
			if err != nil || info.ModTime().After(cutoff) {
				continue
			}
			if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !os.IsNotExist(err) {
				log.Printf("remove expired background log %s: %v", entry.Name(), err)
			}
		}
	}
	return nil
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
// IT IS REDACTED FROM THE SAME PLACE THE FILE READ IS (E26 T6, §3.6 D8), and that is the whole of what
// this task changed here: the excerpt below lands in commands.payload and then in delivered_messages —
// DURABLE rows — where a synchronous shell result would already have been masked before reaching
// tool_calls.result. redactTaskOutput re-resolves the row's env_keys at THIS moment, because the row
// holds key names and never values, and it is the identical call palai.workspace.file makes.
//
// A FAILURE TO REDACT WITHHOLDS THE EXCERPT AND KEEPS THE NOTIFICATION. That is the fail-closed direction
// in the shape this function already had one for: an unreadable file, a missing allocation and now an
// unmaskable value all produce a NOTE the model is told, and the exit — which is the thing the model is
// waiting for — still lands.
func (o *Orchestrator) backgroundTail(ctx context.Context, task coordinator.BackgroundTask) (tail string, note string) {
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
	redacted, err := o.redactTaskOutput(ctx, task.Tenant, task.RunID, task.EnvKeys, string(buf))
	if err != nil {
		log.Printf("redact the output excerpt of background task %s: %v", task.ID, err)
		return "", "the output excerpt was withheld because this run's environment values could not be re-resolved to mask them"
	}
	return redacted, ""
}

// decodeMachineHandle reads the handle a machine returned from bg.start. It re-marshals before
// decoding, the same way every other value that has crossed this wire is read.
func decodeMachineHandle(answer map[string]any) (toolbroker.Handle, error) {
	raw, ok := answer["handle"]
	if !ok {
		return toolbroker.Handle{}, errors.New("the machine started a task and named no handle")
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return toolbroker.Handle{}, fmt.Errorf("encode the machine's handle: %w", err)
	}
	var handle toolbroker.Handle
	if err := json.Unmarshal(encoded, &handle); err != nil {
		return toolbroker.Handle{}, fmt.Errorf("decode the machine's handle: %w", err)
	}
	return handle, nil
}
