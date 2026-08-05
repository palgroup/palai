package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// This file is the host posture's implementation of toolbroker.BackgroundRunner: a command that
// OUTLIVES the call that started it. It is the sibling of exec.go's Run and shares its environment
// construction verbatim; what it does NOT share is the one line that made a background task impossible.
//
// exec.CommandContext -> exec.Command. That is the change. The synchronous path binds the process group
// to the attempt's context and kills the whole group when it ends (exec.go's Cancel), which is correct
// there and is exactly what a background task must not have: a parked attempt leaves the dispatch call,
// and a process bound to that call would die at the moment the model stopped waiting for it.
//
// WHAT REPLACES THE CONTEXT AS THE BOUND: a `deadline_at` column and a reaper that reads it (E26 §0.2).
// A context cannot do that job because a context does not survive a restart and the process does.

// The host executor is a BackgroundRunner as well as a ShellRunner. Asserted here rather than in a test
// so that it is the compiler, not a tier someone has to remember to run, that keeps it true.
var _ toolbroker.BackgroundRunner = (*Executor)(nil)

// bgSeparator splits a host handle into its two halves: the process-group id and the start time of the
// process that led it. BOTH are required and the second is the whole point — a pgid alone is a number
// the kernel hands out again.
const bgSeparator = ":"

// startTimeLayout parses `ps -o lstart=` output ("Thu Jul 30 23:14:02 2026"). It is deliberately the
// portable spelling: the same flag answers on darwin and on Linux, so the host posture needs no build
// tag and no second implementation to drift from the first.
const startTimeLayout = "Mon Jan _2 15:04:05 2006"

// exitCodes remembers what a process THIS CONTROL PLANE STARTED returned, keyed by handle value. It is
// package-level rather than per-Executor because the knowledge is a property of the PROCESS the watcher
// ran in, not of any one executor value — and because a probe from a different executor must get the
// same answer a probe from the starting one does.
//
// ponytail: in-memory, and that ceiling is the honest half of exit_code being a NULLABLE column. A
// control plane that restarted knows a task is gone and cannot know what it returned; the row then keeps
// NULL, which is the truth, instead of a -1 nobody could tell apart from an exit status. Entries are
// never evicted — a background task is a scarce object (E26 §0.3 caps them at twenty per machine), so
// the map is bounded by the same ceiling the reaper enforces.
var exitCodes sync.Map // handle value -> int

// Start runs one argv on the host DETACHED and returns the durable handle for it. It does not wait for
// the command, does not capture its output, and does not bind it to ctx.
//
// ctx bounds the SETUP ONLY — the directory creation and the start itself — and deliberately not the
// process. That is the inversion this whole task is: ownership moves from the call to the run.
func (e *Executor) Start(ctx context.Context, cmd toolbroker.ShellCommand, spec toolbroker.BackgroundSpec) (toolbroker.Handle, error) {
	if len(cmd.Argv) == 0 {
		return toolbroker.Handle{}, errors.New("shell command requires an argv")
	}
	if cmd.WorkspaceRoot == "" {
		return toolbroker.Handle{}, errors.New("shell command requires a workspace root")
	}
	// Same refusal as the synchronous path: there is no host equivalent of a read-only mount, and running
	// writable under a read-only name would report a containment the machine does not have.
	if cmd.ReadOnly {
		return toolbroker.Handle{}, ErrReadOnlyUnsupported
	}
	if err := ctx.Err(); err != nil {
		return toolbroker.Handle{}, err
	}

	logPath, err := spec.Resolve(cmd.WorkspaceRoot)
	if err != nil {
		return toolbroker.Handle{}, err
	}

	// Same rule as every other execution path here: argv form runs the executable directly, so no
	// metacharacter is interpreted unless the caller opted into a shell.
	runArgv := cmd.Argv
	if cmd.Shell {
		runArgv = []string{"/bin/sh", "-c", strings.Join(cmd.Argv, " ")}
	}

	// procAttrFor is the SYNCHRONOUS path's function, called here rather than re-implemented for the
	// reason the two env calls below are shared: a background task's identity must be bit-identical to a
	// synchronous one's. A process that outlives its attempt is the LAST place a privilege drop should
	// be written twice — it is also the one nobody is waiting on to notice that it ran as the wrong uid.
	// It resolves BEFORE the directories are made, so a refusal cannot leave a tree half handed over.
	procAttr, err := procAttrFor(cmd.RunAs, os.Geteuid())
	if err != nil {
		return toolbroker.Handle{}, err
	}

	// allowedEnv and LayerEnv are the SAME two calls the synchronous path makes, in the same order, with
	// the same reserved-name set. The environment allow-list (a variable nobody named cannot arrive) and
	// the collision refusal (a variable this posture derived cannot be replaced) are therefore not
	// re-implemented here and cannot drift; a background task's environment is bit-identical to a
	// synchronous one's. The refusal happens BEFORE the process exists, for exec.go's reason: a command
	// that ran under a caller-supplied PATH and reported an error afterwards has already run.
	env, err := allowedEnv(cmd.WorkspaceRoot, cmd.RunAs)
	if err != nil {
		return toolbroker.Handle{}, err
	}
	env, err = toolbroker.LayerEnv(env, reservedEnvNames(), cmd.Env)
	if err != nil {
		return toolbroker.Handle{}, err
	}

	// .palai-session/bg/ is created HERE because nothing else creates it: allowedEnv makes home/tmp/
	// simulators, workspace.Prepare makes repo/scratch/artifacts, and neither knows about this one. 0o700
	// matches the session directories beside it.
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return toolbroker.Handle{}, fmt.Errorf("prepare background output directory: %w", err)
	}
	// O_EXCL: a task id names exactly one file, so an id collision is an error rather than two processes
	// interleaving their output into one log a model would then read as one command's.
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return toolbroker.Handle{}, fmt.Errorf("open background output file: %w", err)
	}
	defer logFile.Close() // the CHILD holds its own descriptor after Start; ours has no further use.

	c := exec.Command(runArgv[0], runArgv[1:]...)
	c.Dir = cmd.WorkspaceRoot
	c.Env = env
	// stdout and stderr merge into ONE file, matching the harness this replicates: a model reading
	// interleaved output reads what a terminal would have shown. Separating them would need two paths
	// through the redaction the read side owes (T6) and would make "the last 2 KiB" ambiguous.
	c.Stdout, c.Stderr = logFile, logFile
	// Setpgid is kept from the synchronous path for the same reason and to a larger effect: the kill has
	// to reach a whole build tree, and here nobody is waiting around to notice if it does not.
	c.SysProcAttr = procAttr

	if err := c.Start(); err != nil {
		_ = os.Remove(logPath)
		return toolbroker.Handle{}, fmt.Errorf("start background task: %w", err)
	}
	pgid := c.Process.Pid // Setpgid makes the child its own group leader, so its pid IS the group id.

	// The start time is read AFTER the process exists and is the only evidence that later makes the pgid
	// trustworthy. If it cannot be read the task is killed rather than recorded: a handle we could never
	// prove would be a process we could never reap.
	started, err := processStartTime(pgid)
	if err != nil {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		_ = c.Wait()
		_ = os.Remove(logPath)
		return toolbroker.Handle{}, fmt.Errorf("record background task start time: %w", err)
	}
	handle := toolbroker.Handle{Posture: toolbroker.PostureUnsandboxedHost, Value: strconv.Itoa(pgid) + bgSeparator + started}

	// THE WATCHER IS NOT THE AUTHORITY. It exists to call Wait so the kernel does not keep a zombie, and
	// to remember an exit code while this process is alive to remember it. The authority is Probe, which
	// reads the operating system — because a watcher goroutine does not survive the restart that the row
	// and the process both do.
	go func() {
		err := c.Wait()
		var exitErr *exec.ExitError
		switch {
		case err == nil:
			exitCodes.Store(handle.Value, 0)
		case errors.As(err, &exitErr):
			exitCodes.Store(handle.Value, exitStatus(exitErr))
		}
		// Any other Wait failure tells us nothing about the command, so nothing is recorded and Probe
		// reports an exited task with NO exit code — the honest NULL.
	}()

	return handle, nil
}

// Probe asks the OPERATING SYSTEM what became of a handle. It reads no state Start left behind except
// the exit code, and even that only decorates an answer the kernel already gave.
func (e *Executor) Probe(ctx context.Context, handle toolbroker.Handle) (toolbroker.BackgroundStatus, error) {
	pgid, recorded, err := parseHostHandle(handle)
	if err != nil {
		return toolbroker.BackgroundStatus{}, err
	}
	live, state, err := processStartTimeAndState(pgid)
	switch {
	case errors.Is(err, errNoSuchProcess):
		// Gone. That is a real terminal answer and the exit code is a separate question: this process
		// knows it if it ran the watcher, and a restarted control plane honestly does not.
		status := toolbroker.BackgroundStatus{State: toolbroker.BackgroundExited}
		if code, ok := exitCodes.Load(handle.Value); ok {
			c := code.(int)
			status.ExitCode = &c
		}
		return status, nil
	case err != nil:
		return toolbroker.BackgroundStatus{}, err
	}
	// A zombie is a process the kernel still lists and that has already exited. Reading it as `running`
	// would park a reaper on a task that finished, so it is classified with the dead.
	if strings.HasPrefix(state, "Z") {
		status := toolbroker.BackgroundStatus{State: toolbroker.BackgroundExited}
		if code, ok := exitCodes.Load(handle.Value); ok {
			c := code.(int)
			status.ExitCode = &c
		}
		return status, nil
	}
	if live != recorded {
		// SOMETHING occupies the pid and it is not what we started. This is the answer the whole handle
		// format exists to be able to give, and everything downstream keys on it: a lost task is never
		// signalled.
		return toolbroker.BackgroundStatus{State: toolbroker.BackgroundLost}, nil
	}
	return toolbroker.BackgroundStatus{State: toolbroker.BackgroundRunning}, nil
}

// Kill terminates the whole process group behind a handle, and REFUSES to signal one it cannot prove is
// ours. It is idempotent: a task that has already gone is a caller that lost a race, not an error.
func (e *Executor) Kill(ctx context.Context, handle toolbroker.Handle) error {
	pgid, _, err := parseHostHandle(handle)
	if err != nil {
		return err
	}
	status, err := e.Probe(ctx, handle)
	if err != nil {
		return err
	}
	switch status.State {
	case toolbroker.BackgroundLost:
		// NO SIGNAL. Not a best-effort one, not a SIGTERM instead of a SIGKILL. The pid belongs to
		// something else and the only safe action is to say so.
		return fmt.Errorf("%w: pgid %d is occupied by a process that is not ours", toolbroker.ErrHandleLost, pgid)
	case toolbroker.BackgroundExited:
		return nil
	}
	// ponytail: there is a window between the probe above and this signal in which the pid could be
	// reused. It is microseconds wide and it cannot be closed without a pid file descriptor, which macOS
	// — the posture's actual target — does not offer. The narrow window is the ceiling; the wide one (a
	// handle read from a row written before a restart) is what the probe closes.
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("kill background task group %d: %w", pgid, err)
	}
	return nil
}

// exitStatus turns a finished process into the number a shell would report, and it exists because
// os.ProcessState.ExitCode() returns -1 FOR A SIGNALLED PROCESS — which is precisely the sentinel this
// whole feature refuses to write. A killed background task must not be recorded as exit_code = -1: a
// model reads an exit code by comparing it, and -1 compares as "some failure" in a column whose NULL
// already means "not known".
//
// 128 + signal is what a shell reports, what the synchronous host executor reports for the same event,
// and what the container posture's daemon reports (a SIGKILLed container exits 137). Three paths, one
// number, so a reaped build reads the same under every posture.
func exitStatus(exitErr *exec.ExitError) int {
	if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	return exitErr.ExitCode()
}

// errNoSuchProcess reports a pid the operating system does not list. It is a normal answer here, not a
// failure: the task finished.
var errNoSuchProcess = errors.New("no such process")

// processStartTime returns the canonical start-time string recorded in a handle.
func processStartTime(pid int) (string, error) {
	started, _, err := processStartTimeAndState(pid)
	return started, err
}

// processStartTimeAndState reads one process's start time and state code from `ps`.
//
// WHY `ps` AND NOT A SYSCALL: the two candidate syscalls are per-platform (sysctl KERN_PROC on darwin,
// /proc/<pid>/stat on Linux) and would mean two implementations of the single fact this posture's safety
// rests on. One of them would be the one nobody runs. `ps -o lstart=,state=` answers identically on both,
// and this is not a hot path — a probe happens on a reaper tick, not per byte of output.
//
// THE RESOLUTION IS ONE SECOND, and that is the honest ceiling under E26's BGT-P2: two processes born in
// the same second at the same pid would compare equal. Recycling a pid inside one second requires
// exhausting the pid space in that second, so the comparison holds in practice — but "in practice" is
// what it says, and the rule built on top of it (an unprovable handle is never signalled) is what makes
// the residual risk an orphan rather than a stranger's dead process.
func processStartTimeAndState(pid int) (started string, state string, err error) {
	if pid <= 0 {
		return "", "", fmt.Errorf("invalid pid %d", pid)
	}
	out, err := exec.Command("ps", "-o", "lstart=,state=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		// A non-zero exit from `ps -p` means the process is not listed. It is the answer, not an error.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", "", errNoSuchProcess
		}
		return "", "", fmt.Errorf("probe pid %d: %w", pid, err)
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) == 0 {
		return "", "", errNoSuchProcess
	}
	if len(fields) < 6 {
		return "", "", fmt.Errorf("probe pid %d: unparseable ps output %q", pid, string(out))
	}
	// lstart is five whitespace-separated fields ("Thu Jul 30 23:14:02 2026"); state is whatever follows.
	stamp := strings.Join(fields[:5], " ")
	parsed, err := time.ParseInLocation(startTimeLayout, stamp, time.Local)
	if err != nil {
		return "", "", fmt.Errorf("probe pid %d: unparseable start time %q: %w", pid, stamp, err)
	}
	// Normalised to UTC RFC3339 so the recorded value does not change meaning when the machine's timezone
	// does — a handle read back after a DST transition must still compare equal to itself.
	return parsed.UTC().Format(time.RFC3339), fields[5], nil
}

// parseHostHandle splits and validates a host handle, refusing one that belongs to the other posture. A
// container id handed to the process prober would resolve to nothing and read as "the task exited",
// which is the wrong answer rather than an error — hence the explicit posture check.
func parseHostHandle(handle toolbroker.Handle) (pgid int, started string, err error) {
	if handle.Posture != toolbroker.PostureUnsandboxedHost {
		return 0, "", fmt.Errorf("handle posture %q is not %q", handle.Posture, toolbroker.PostureUnsandboxedHost)
	}
	pgidText, started, ok := strings.Cut(handle.Value, bgSeparator)
	if !ok || started == "" {
		return 0, "", fmt.Errorf("malformed host background handle %q, want <pgid>%s<start time>", handle.Value, bgSeparator)
	}
	pgid, err = strconv.Atoi(pgidText)
	if err != nil || pgid <= 0 {
		return 0, "", fmt.Errorf("malformed host background handle %q: %q is not a process group id", handle.Value, pgidText)
	}
	return pgid, started, nil
}
