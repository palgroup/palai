package host_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/host"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// bgWorkspace is one throwaway allocation plus the spec a background start needs. Every case here uses
// the same shape so the differences between them are the assertions rather than the setup.
func bgWorkspace(t *testing.T) (string, toolbroker.BackgroundSpec) {
	t.Helper()
	root := t.TempDir()
	id := "bgt-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	return root, toolbroker.BackgroundSpec{TaskID: id, OutputPath: ".palai-session/bg/" + id + ".log"}
}

// pgidFrom reads the process-group id a started shell wrote into a file, waiting for it to appear. It
// exists because the ONLY honest source of the number is the process itself: reading it from anything we
// returned would make every measurement below a measurement of our own bookkeeping.
func pgidFrom(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		if err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw))); convErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no pid was ever written to %s; the command never ran", path)
	return 0
}

// alive asks the KERNEL whether a process group still exists. Signal 0 performs the permission and
// existence checks and delivers nothing, which is the only probe here that is not also an effect.
func alive(pgid int) bool { return syscall.Kill(-pgid, 0) == nil }

// TestTodayAHostProcessGroupDiesWithTheAttempt is the MEASUREMENT the whole task rests on, and it is
// deliberately a test of the code as it stands rather than of anything E26 adds: a command started
// through the synchronous shell runner is killed — the whole GROUP — when the attempt context is
// cancelled. exec.CommandContext + a Cancel that sends SIGKILL to -pgid is what does it, and the file's
// own package comment calls that a feature, which it is.
//
// It is the reason "park the run and keep the build running" was a CONTRADICTION rather than a missing
// feature: process ownership was bound to the ShellRunner.Run CALL, and a parked attempt leaves that
// call. It stays in the suite as a guard: the synchronous posture must keep reaping its tree, and E26
// must not have quietly turned every shell call into a detached one.
func TestTodayAHostProcessGroupDiesWithTheAttempt(t *testing.T) {
	root := t.TempDir()
	pidFile := filepath.Join(root, "pgid")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Unbounded wall time: the ONLY thing that may end this command is the context, so the measurement
		// cannot be confused with a timeout.
		_, _ = host.NewExecutor(0).Run(ctx, toolbroker.ShellCommand{
			Argv:          []string{"echo $$ >", pidFile, "; sleep 30"},
			WorkspaceRoot: root,
			Shell:         true,
		})
	}()

	pgid := pgidFrom(t, pidFile)
	if !alive(pgid) {
		t.Fatalf("process group %d is not alive while Run is still executing; the fixture never started", pgid)
	}

	cancel()
	<-done

	if alive(pgid) {
		t.Fatalf("process group %d survived the attempt; the synchronous posture must reap its whole tree", pgid)
	}
}

// TestAHostBackgroundProcessSurvivesTheAttempt is E26's first RED, and §3.5 P10's measurement.
//
// The published os/exec reference could not be fetched when this plan was written (ECONNRESET,
// 2026-07-30), so POSIX reparenting is NOT cited here as a source. It is PROVEN here as a consequence:
// the process group is alive after Start returns, and still alive after the context that carried the
// attempt is cancelled. If this test had failed, the host posture would have had to be dropped and the
// container posture would have been the only one.
func TestAHostBackgroundProcessSurvivesTheAttempt(t *testing.T) {
	root, spec := bgWorkspace(t)
	pidFile := filepath.Join(root, "pgid")

	ctx, cancel := context.WithCancel(context.Background())
	handle, err := host.NewExecutor(0).Start(ctx, toolbroker.ShellCommand{
		Argv:          []string{"echo $$ >", pidFile, "; sleep 30"},
		WorkspaceRoot: root,
		Shell:         true,
	}, spec)
	if err != nil {
		t.Fatalf("start background task: %v", err)
	}

	// (1) Alive after Start RETURNED. Start is not allowed to wait for the command, so this is the
	// difference between a handle and a result.
	pgid := pgidFrom(t, pidFile)
	if !alive(pgid) {
		t.Fatalf("process group %d is not alive after Start returned", pgid)
	}

	// (2) Alive after the ATTEMPT ends. This is the half no exec.CommandContext can give: the context
	// that dispatched the tool call is cancelled and the process does not notice.
	cancel()
	time.Sleep(500 * time.Millisecond) // WaitDelay in the synchronous posture is 2s; a kill would land well inside this.
	if !alive(pgid) {
		t.Fatalf("process group %d died when the attempt context was cancelled; ownership is still bound to the call", pgid)
	}

	// And the handle a restarted control plane would work from says the same thing, read from the
	// operating system rather than from anything Start remembered.
	status, err := host.NewExecutor(0).Probe(context.Background(), handle)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if status.State != toolbroker.BackgroundRunning {
		t.Fatalf("probe state = %q, want running", status.State)
	}

	if err := host.NewExecutor(0).Kill(context.Background(), handle); err != nil {
		t.Fatalf("kill: %v", err)
	}
	waitUntilGone(t, pgid)
}

// TestABackgroundTaskWritesItsOwnLogAndCreatesTheSessionDirectory pins the output decision (§3.2): the
// merged stream lands in a file under the allocation's .palai-session subtree — which the snapshot walker
// skips wholesale — and the bg/ directory is created AT SPAWN, because nothing else creates it. The host
// executor makes home/tmp/simulators under the same parent for a synchronous call; bg/ is not one of them.
func TestABackgroundTaskWritesItsOwnLogAndCreatesTheSessionDirectory(t *testing.T) {
	root, spec := bgWorkspace(t)
	e := host.NewExecutor(0)

	handle, err := e.Start(context.Background(), toolbroker.ShellCommand{
		Argv:          []string{"echo", "alpha"},
		WorkspaceRoot: root,
	}, spec)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = e.Kill(context.Background(), handle) })

	logPath := filepath.Join(root, spec.OutputPath)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(logPath); err == nil && strings.Contains(string(data), "alpha") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never carried the command's output; the log file is the only way a model can read it", logPath)
}

// TestAnEscapingOutputPathIsRefusedBeforeAnythingStarts keeps the containment where both postures share
// it. The caller is the orchestrator rather than the model, which is exactly the sentence that precedes
// every traversal that ever shipped.
//
// AND IT CARRIES ITS OWN NON-VACUITY HALF, ADDED BY THE E26 T7 EXIT GATE AFTER MEASURING THAT IT DID NOT.
// The three refusals below are ALL this test asserted, so an executor whose `Start` returned an error for
// every input satisfied it — demonstrated by making `Start` refuse unconditionally, at which point this
// function still reported `ok`. A refusal-only proof is the same shape as E26 T2's approval-gate RED, which
// PASSED at the fork point because there was no spawn path at all: a guard that cannot fail reports the same
// green as one that passes. The legal path on the SAME executor must therefore start exactly one process.
func TestAnEscapingOutputPathIsRefusedBeforeAnythingStarts(t *testing.T) {
	root := t.TempDir()
	e := host.NewExecutor(0)
	for _, path := range []string{"../escape.log", "/etc/escape.log", ""} {
		_, err := e.Start(context.Background(), toolbroker.ShellCommand{
			Argv:          []string{"true"},
			WorkspaceRoot: root,
		}, toolbroker.BackgroundSpec{TaskID: "bgt-escape", OutputPath: path})
		if err == nil {
			t.Fatalf("output path %q was accepted; it resolves outside the allocation", path)
		}
	}

	// THE CONTROL: the same executor, the same command, a path INSIDE the allocation — one process starts.
	if err := os.MkdirAll(filepath.Join(root, ".palai-session", "bg"), 0o755); err != nil {
		t.Fatalf("create the session dir: %v", err)
	}
	handle, err := e.Start(context.Background(), toolbroker.ShellCommand{
		Argv:          []string{"true"},
		WorkspaceRoot: root,
	}, toolbroker.BackgroundSpec{TaskID: "bgt-escape-control", OutputPath: filepath.Join(".palai-session", "bg", "bgt-escape-control.log")})
	if err != nil {
		t.Fatalf("a LEGAL output path was refused too (%v) — this executor refuses everything, so the three "+
			"refusals above are statements about it rather than about path containment", err)
	}
	t.Cleanup(func() { _ = e.Kill(context.Background(), handle) })
	if handle.Value == "" {
		t.Fatal("the control start returned an empty handle: nothing was actually started")
	}
}

// TestABackgroundTaskGetsTheSameClosedEnvironmentAsASynchronousOne proves the reuse claim rather than
// asserting it: allowedEnv + LayerEnv are the SAME two calls the synchronous path makes, so the
// environment allow-list (a variable nobody named cannot arrive) and the collision refusal (a variable
// this posture derived cannot be replaced) are bit-identical across the two.
//
// The measurement sweeps the whole environment the command actually saw rather than the names somebody
// remembered — the discipline E22 T1 set for this posture and the only one that catches an inheritance.
func TestABackgroundTaskGetsTheSameClosedEnvironmentAsASynchronousOne(t *testing.T) {
	t.Setenv("PALAI_BG_LEAK_CANARY", "operator-secret")
	root, spec := bgWorkspace(t)
	e := host.NewExecutor(0)

	handle, err := e.Start(context.Background(), toolbroker.ShellCommand{
		Argv:          []string{"env"},
		WorkspaceRoot: root,
		Env:           map[string]string{"JIRA_TOKEN": "task-scoped-value"},
	}, spec)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = e.Kill(context.Background(), handle) })

	body := waitForLog(t, filepath.Join(root, spec.OutputPath), "JIRA_TOKEN=")
	if strings.Contains(body, "PALAI_BG_LEAK_CANARY") {
		t.Fatalf("the background task inherited the control plane's own environment:\n%s", body)
	}
	for _, derived := range []string{"HOME=", "TMPDIR=", "PALAI_SIMCTL_SET=", "PALAI_DERIVED_DATA="} {
		if !strings.Contains(body, derived) {
			t.Fatalf("the background task is missing the per-session %s the synchronous posture derives:\n%s", derived, body)
		}
	}

	// The collision refusal, unchanged: a caller-named key that this posture CLAIMS is refused before a
	// process exists, whether or not the control plane happens to carry a value for it.
	_, err = e.Start(context.Background(), toolbroker.ShellCommand{
		Argv:          []string{"true"},
		WorkspaceRoot: root,
		Env:           map[string]string{"PATH": "/attacker/bin"},
	}, toolbroker.BackgroundSpec{TaskID: "bgt-collide", OutputPath: ".palai-session/bg/collide.log"})
	if err == nil {
		t.Fatal("an environment key colliding with the posture's own PATH was accepted for a background task")
	}
}

// TestAPgidWeCannotProveIsOursIsNeverSignalled is E26's fourth RED and the sharpest one, because what it
// asserts is an ABSENCE: no signal is sent at all.
//
// PID numbers are reused. A background_tasks row that survived a restart names a pgid that may now
// belong to anything on the machine, and the only evidence we have that it is still ours is the start
// time recorded beside it. When that evidence does not match, the handle is `lost` and Kill must do
// NOTHING — the measurement is a real process, a fabricated start time, and that process still running
// afterwards.
//
// The honest ceiling is written here rather than in a doc: start-time comparison is BEST EFFORT. It
// leaves us the orphan, and not killing a stranger's process matters more than reaping our own.
func TestAPgidWeCannotProveIsOursIsNeverSignalled(t *testing.T) {
	// A REAL process in its own group, standing in for whatever inherited the pid after a reuse. It is
	// started with os/exec directly and not through the executor: the point is that it is NOT ours.
	innocent := exec.Command("sleep", "30")
	innocent.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := innocent.Start(); err != nil {
		t.Fatalf("start the innocent process: %v", err)
	}
	pgid := innocent.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		_ = innocent.Wait()
	})
	if !alive(pgid) {
		t.Fatal("the innocent process is not alive; the fixture proves nothing")
	}

	// A handle naming that real, live pgid with a start time that is not its own — exactly the shape a
	// reused pid produces.
	handle := toolbroker.Handle{
		Posture: toolbroker.PostureUnsandboxedHost,
		Value:   strconv.Itoa(pgid) + ":1999-01-01T00:00:00Z",
	}

	e := host.NewExecutor(0)
	status, err := e.Probe(context.Background(), handle)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if status.State != toolbroker.BackgroundLost {
		t.Fatalf("probe state = %q, want lost: the recorded start time does not match the live process", status.State)
	}

	// Kill must refuse rather than succeed quietly — a caller that learns nothing would retry.
	if err := e.Kill(context.Background(), handle); err == nil {
		t.Fatal("Kill accepted a handle it cannot prove it owns")
	}

	// THE ASSERTION THAT MATTERS: the process is still there.
	time.Sleep(300 * time.Millisecond)
	if !alive(pgid) {
		t.Fatalf("process group %d was signalled; a handle we cannot prove is ours must never receive one", pgid)
	}
	if innocent.ProcessState != nil {
		t.Fatalf("the innocent process terminated: %v", innocent.ProcessState)
	}
}

// TestAFinishedBackgroundTaskReportsItsExitCodeOnlyWhenItIsKnown pins the shape of the answer rather
// than only the answer: a task this control plane started and watched reports its real exit code, and a
// handle nobody watched — the shape of every row that outlived a restart — reports EXITED WITH NO CODE.
//
// The second half is the one worth a test. A nil ExitCode is what keeps background_tasks.exit_code NULL
// instead of -1, and a -1 is a number a model compares against zero.
func TestAFinishedBackgroundTaskReportsItsExitCodeOnlyWhenItIsKnown(t *testing.T) {
	root, spec := bgWorkspace(t)
	e := host.NewExecutor(0)

	handle, err := e.Start(context.Background(), toolbroker.ShellCommand{
		Argv:          []string{"exit 7"},
		WorkspaceRoot: root,
		Shell:         true,
	}, spec)
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	var status toolbroker.BackgroundStatus
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		status, err = e.Probe(context.Background(), handle)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if status.State == toolbroker.BackgroundExited && status.ExitCode != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status.State != toolbroker.BackgroundExited {
		t.Fatalf("state = %q, want exited", status.State)
	}
	if status.ExitCode == nil || *status.ExitCode != 7 {
		t.Fatalf("exit code = %v, want 7 (this process started and watched the task)", status.ExitCode)
	}

	// A handle for a pid nothing here ever started: gone, and honestly without a code.
	unknown := toolbroker.Handle{Posture: toolbroker.PostureUnsandboxedHost, Value: "2147483646:2020-01-01T00:00:00Z"}
	status, err = e.Probe(context.Background(), unknown)
	if err != nil {
		t.Fatalf("probe unknown: %v", err)
	}
	if status.State != toolbroker.BackgroundExited {
		t.Fatalf("unknown handle state = %q, want exited", status.State)
	}
	if status.ExitCode != nil {
		t.Fatalf("unknown handle exit code = %d, want nil: an invented exit code is worse than no exit code", *status.ExitCode)
	}
}

// TestKillingABackgroundTaskTwiceIsKillingItOnce pins the idempotence the vendor contract states for the
// same operation ("Cancelling twice is idempotent") and that a reaper needs: the second call is a caller
// that lost a race, not an error to escalate.
func TestKillingABackgroundTaskTwiceIsKillingItOnce(t *testing.T) {
	root, spec := bgWorkspace(t)
	pidFile := filepath.Join(root, "pgid")
	e := host.NewExecutor(0)

	handle, err := e.Start(context.Background(), toolbroker.ShellCommand{
		Argv:          []string{"echo $$ >", pidFile, "; sleep 30"},
		WorkspaceRoot: root,
		Shell:         true,
	}, spec)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	pgid := pgidFrom(t, pidFile)

	if err := e.Kill(context.Background(), handle); err != nil {
		t.Fatalf("first kill: %v", err)
	}
	waitUntilGone(t, pgid)
	if err := e.Kill(context.Background(), handle); err != nil {
		t.Fatalf("second kill: %v (killing twice must be killing once)", err)
	}

	status, err := e.Probe(context.Background(), handle)
	if err != nil {
		t.Fatalf("probe after kill: %v", err)
	}
	if status.State != toolbroker.BackgroundExited {
		t.Fatalf("probe state after kill = %q, want exited", status.State)
	}
	// A KILLED TASK MUST NOT BE RECORDED AS -1, and it would have been: os.ProcessState.ExitCode()
	// returns -1 for a signalled process, and -1 is the sentinel this feature's whole design refuses —
	// exit_code's NULL already means "not known", and a model reads an exit code by comparing it.
	// 128 + SIGKILL is what a shell reports, what the synchronous host executor reports for the same
	// event, and what the daemon reports for a SIGKILLed container.
	if status.ExitCode == nil {
		t.Fatal("a task this process killed and watched reports no exit code")
	}
	if *status.ExitCode != 128+int(syscall.SIGKILL) {
		t.Fatalf("exit code after kill = %d, want %d (128 + SIGKILL); -1 is the sentinel this column exists "+
			"to avoid, and it is what ProcessState.ExitCode() returns for a signalled process",
			*status.ExitCode, 128+int(syscall.SIGKILL))
	}
}

// TestTheKillReachesTheWholeProcessGroup keeps the property the synchronous posture's own header calls
// out: a reaped build must not leave a compiler behind. A background task is where that matters most,
// because nothing else is going to notice.
func TestTheKillReachesTheWholeProcessGroup(t *testing.T) {
	root, spec := bgWorkspace(t)
	pidFile := filepath.Join(root, "pgid")
	childFile := filepath.Join(root, "childpid")
	e := host.NewExecutor(0)

	handle, err := e.Start(context.Background(), toolbroker.ShellCommand{
		Argv: []string{
			"echo $$ >", pidFile, ";",
			"sh -c 'echo $$ >", childFile, "; sleep 30' &",
			"sleep 30",
		},
		WorkspaceRoot: root,
		Shell:         true,
	}, spec)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	pgid := pgidFrom(t, pidFile)
	child := pgidFrom(t, childFile)

	if err := e.Kill(context.Background(), handle); err != nil {
		t.Fatalf("kill: %v", err)
	}
	waitUntilGone(t, pgid)
	// The child was in the same group, so the group kill reached it too. It is checked as a single pid
	// because it is not a group leader.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(child, 0) != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("child %d outlived the group kill; a reaped build left a compiler behind", child)
}

// waitUntilGone waits for a process group to disappear. A kill is asynchronous — the signal is delivered
// and the kernel tears the group down afterwards — so asserting immediately would be asserting on timing.
func waitUntilGone(t *testing.T, pgid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !alive(pgid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process group %d is still alive after the kill", pgid)
}

// waitForLog waits until the task's log file contains a marker and returns the whole body.
func waitForLog(t *testing.T, path, marker string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && strings.Contains(string(data), marker) {
			return string(data)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never contained %q", path, marker)
	return ""
}

// TestAnEnvironmentValueOutlivesTheExecuteCallThatHandedItOver is E26 T6's FIRST RED, and what it
// measures is a SENTENCE this tree wrote about itself (§3.6 D9, §0.4):
//
//	"Scope and expiry are the ATTEMPT itself … There is no handle to redeem and no deadline to check
//	 because the value's whole life is one Execute call."  (tool-broker/sandbox_exec.go, E25 T3)
//
// For a background task that is STRUCTURALLY FALSE and no code change can make it true: a value handed
// over in exec.Cmd.Env lives in the kernel's environ copy for the whole life of the PROCESS, and Start
// returns while that process is still running.
//
// THE MEASUREMENT IS AN ORDERING, NOT A SLEEP. The command blocks on a file only the TEST can create,
// so the moment it reads its own environment is strictly after Start returned: nothing here is timing.
//
// WHY THE PROCESS READS ITS OWN ENVIRON RATHER THAN US READING IT FROM OUTSIDE: `ps -Eww` on this
// posture's actual target discloses no environment at all (macOS withholds it even for one's own
// processes), and /proc/<pid>/environ is Linux-only. The process's own read proves the same kernel copy
// exists — it is where printenv reads from — and it is the one spelling that answers on both platforms,
// which is the rule processStartTimeAndState already follows for `ps`.
func TestAnEnvironmentValueOutlivesTheExecuteCallThatHandedItOver(t *testing.T) {
	const sentinel = "e26-t6-host-environ-sentinel-93b1f0"
	root, spec := bgWorkspace(t)
	e := host.NewExecutor(0)

	release := filepath.Join(root, "release")
	// `printenv` reads the environment the KERNEL holds for this process, and it runs only once the test
	// has created a file that does not exist yet.
	handle, err := e.Start(context.Background(), toolbroker.ShellCommand{
		Argv:          []string{"while [ ! -f " + release + " ]; do sleep 0.05; done; printenv DEPLOY_TOKEN"},
		Shell:         true,
		WorkspaceRoot: root,
		Env:           map[string]string{"DEPLOY_TOKEN": sentinel},
	}, spec)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = e.Kill(context.Background(), handle) })

	// START HAS RETURNED. Under the sentence above, the value's life ended here.
	logPath := filepath.Join(root, spec.OutputPath)
	if data, rerr := os.ReadFile(logPath); rerr == nil && strings.Contains(string(data), sentinel) {
		t.Fatalf("the command read its environment BEFORE the release file existed, so this proves nothing about ordering:\n%s", data)
	}
	if err := os.WriteFile(release, []byte("go"), 0o600); err != nil {
		t.Fatalf("release the waiting command: %v", err)
	}

	body := waitForLog(t, logPath, sentinel)
	if !strings.Contains(body, sentinel) {
		t.Fatalf("the value was gone from the process environ after Start returned:\n%s", body)
	}
	// AND THE CORRECTION IS PART OF THE PROOF. A comment that still claims the old life is a comment that
	// rotted; this tree's pattern is to renew the sentence in place (host/exec.go, workspace/exec.go), and
	// E24 T8 found the same belief written in twenty-six places because nobody did.
	source, err := os.ReadFile("../../../packages/tool-broker/sandbox_exec.go")
	if err != nil {
		t.Fatalf("read the sentence under test: %v", err)
	}
	if strings.Contains(string(source), "the value's whole life is one Execute call") {
		t.Fatal("sandbox_exec.go still claims the value's whole life is one Execute call, which the process above just disproved")
	}
}
