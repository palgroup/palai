//go:build component

package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"

	"github.com/palgroup/palai/storage"
)

// E26 T5 — THE REAPER: ceilings, orphans, restart and cancellation, against a REAL Postgres and REAL
// detached host processes.
//
// FOUR OF THE FIVE CLAIMS HERE ARE CLAIMS ABOUT A PROCESS, so four of them are measured from the
// operating system. `kill(-pgid, 0)` is the existence check throughout — the pgid read back from a file
// the PROCESS wrote — and the fifth (the log garbage) is measured with os.Stat on a real file. Nothing
// below asks our own bookkeeping whether something is running, which is the rule T1 built the seam on
// and the only reason adoption means anything: a control plane that just restarted has no bookkeeping.
//
// The ceilings are read through PRODUCTION'S OWN reading of the environment. A test that built its own
// limit would prove a number it had itself chosen; these drive the shipped dispatcher with the shipped
// defaults, which is how a wall time managed to be simultaneously unbounded on the host and refusing
// every call in the container while every sandbox test was green.

// reaperFixture is the wake fixture — a real spine, a parked run, a real allocation whose root a task
// row resolves to — plus what a reaper needs on top: the ability to spawn several tasks at once, to
// corrupt a handle the way a restart does, and to read every row of a run rather than only the first.
type reaperFixture struct{ *wakeFixture }

func newReaperFixture(t *testing.T) *reaperFixture {
	t.Helper()
	return &reaperFixture{wakeFixture: newWakeFixture(t)}
}

// spawnTasks drives ONE attempt that starts n background tasks by tool name and then reports
// `completed`, so the run parks on them. It returns the process-group ids the tasks themselves wrote.
//
// Every pgid is verified live BEFORE the test proceeds: a reaper proof against a task that had already
// exited would pass for the wrong reason.
func (f *reaperFixture) spawnTasks(t *testing.T, n int) []int {
	t.Helper()
	if err := f.runAttempt(t, "completed", n); err != nil {
		t.Fatalf("ExecuteAttempt spawning %d background tasks: %v", n, err)
	}
	pgids := make([]int, 0, n)
	for i := 0; i < n; i++ {
		pgid, err := waitPgid(filepath.Join(f.root, fmt.Sprintf("task-%d.pgid", i)))
		if err != nil {
			t.Fatalf("background task %d never wrote its process-group id: %v", i, err)
		}
		if !alive(pgid) {
			t.Fatalf("background task %d was not running before the reaper ran; every claim here would be about nothing", i)
		}
		pgids = append(pgids, pgid)
	}
	return pgids
}

// runAttemptWithScript drives ONE attempt that backgrounds a single arbitrary command and then reports
// the given terminal. It exists for the one proof that needs a task to FINISH on its own rather than
// block: an exit code the command chose is the only kind this control plane can honestly record.
func (f *reaperFixture) runAttemptWithScript(t *testing.T, outcome, work string) error {
	t.Helper()
	frames := []contracts.EngineFrame{
		engineFrame(1, "engine.ready", map[string]any{
			"selected_protocol": engineProtocol, "engine": map[string]any{"version": "test"},
		}),
		engineFrame(2, "tool.request", map[string]any{
			"tool_call_id": redeliveryID("tc"), "name": "palai.workspace.shell",
			"arguments": map[string]any{"argv": []any{work}, "shell": true, "background": true},
		}),
		engineFrame(3, "run.terminal", map[string]any{"outcome": outcome}),
	}
	f.orch.dialer = &scriptedDialer{ch: &scriptedChannel{frames: frames}}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return f.orch.ExecuteAttempt(ctx, AttemptDescriptor{
		RunID: contracts.RunID(f.runID), AttemptID: contracts.AttemptID(redeliveryID("att")),
		Fence: 1, WorkspaceHostPath: f.root,
	})
}

// taskRows reads every background_tasks row of this fixture's run, oldest first, as the database holds
// it. IT IS THE ONLY PLACE THE TESTS BELOW LEARN A STATE FROM, and the ordering is explicit because a
// LIMIT or an implicit order has decided an outcome in this tree more than once.
type taskRow struct {
	id       string
	state    string
	handle   string
	exitCode *int
	deadline *time.Time
	notified bool
	output   string
}

func (f *reaperFixture) taskRows(t *testing.T) []taskRow {
	t.Helper()
	rows, err := f.spine.Pool().Query(storage.WithSystemScope(context.Background()),
		`SELECT id, state, handle, exit_code, deadline_at, notified_at IS NOT NULL, output_path
		 FROM background_tasks WHERE run_id = $1 ORDER BY started_at, id`, f.runID)
	if err != nil {
		t.Fatalf("read background_tasks: %v", err)
	}
	defer rows.Close()
	var out []taskRow
	for rows.Next() {
		var r taskRow
		if err := rows.Scan(&r.id, &r.state, &r.handle, &r.exitCode, &r.deadline, &r.notified, &r.output); err != nil {
			t.Fatalf("scan background_tasks: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// runningRowCount counts what THE DATABASE believes is running, across every tenant. It is one half of
// the concurrency proof; the process table is the other, and neither is a number we returned.
func (f *reaperFixture) runningRowCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.spine.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(*) FROM background_tasks WHERE state = 'running'`).Scan(&n); err != nil {
		t.Fatalf("count running background tasks: %v", err)
	}
	return n
}

// runningRowCountOfTenant is the same count restricted to THIS fixture's tenant. The difference between
// it and runningRowCount is what makes the machine ceiling a machine ceiling.
func (f *reaperFixture) runningRowCountOfTenant(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.spine.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(*) FROM background_tasks WHERE state = 'running' AND organization_id = $1 AND project_id = $2`,
		f.tenant.Organization, f.tenant.Project).Scan(&n); err != nil {
		t.Fatalf("count this tenant's running background tasks: %v", err)
	}
	return n
}

// rewriteHandle replaces one task's handle with a value naming something else. It is how a restart is
// reproduced without restarting: a row written by a control plane that is gone, holding a handle this
// process never started and therefore knows nothing about.
func (f *reaperFixture) rewriteHandle(t *testing.T, taskID, handle string) {
	t.Helper()
	if _, err := f.spine.Pool().Exec(storage.WithSystemScope(context.Background()),
		`UPDATE background_tasks SET handle = $2 WHERE id = $1`, taskID, handle); err != nil {
		t.Fatalf("rewrite the task handle: %v", err)
	}
}

// deadHostHandle starts a throwaway process group, records the handle format the host posture uses for
// it, then kills and REAPS it. What comes back names a process that genuinely existed and is genuinely
// gone — and, crucially, one that no watcher in THIS process ever waited on, so nothing here knows what
// it returned. That is exactly the position a restarted control plane is in.
func deadHostHandle(t *testing.T) (handle string, pid int) {
	t.Helper()
	c := exec.Command("sleep", "30")
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := c.Start(); err != nil {
		t.Fatalf("start the throwaway process: %v", err)
	}
	pid = c.Process.Pid
	started := psStartTime(t, pid)
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = c.Wait()
	for i := 0; i < 200 && alive(pid); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if alive(pid) {
		t.Fatalf("the throwaway process group %d would not die", pid)
	}
	return strconv.Itoa(pid) + ":" + started, pid
}

// psStartTime spells the handle's start-time half the way adapters/sandboxes/host does: `ps -o lstart=`,
// parsed in the local zone and normalised to UTC RFC3339. It is duplicated here deliberately — a test
// that asked the adapter to format the value it is about to parse would prove the two halves agree with
// themselves rather than with the operating system.
func psStartTime(t *testing.T, pid int) string {
	t.Helper()
	out, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		t.Fatalf("read the start time of pid %d: %v", pid, err)
	}
	stamp := strings.Join(strings.Fields(strings.TrimSpace(string(out)))[:5], " ")
	parsed, err := time.ParseInLocation("Mon Jan _2 15:04:05 2006", stamp, time.Local)
	if err != nil {
		t.Fatalf("parse the start time %q of pid %d: %v", stamp, pid, err)
	}
	return parsed.UTC().Format(time.RFC3339)
}

// ---------------------------------------------------------------------------------------------------
// RED 1 — THE WALL CLOCK. A task past its deadline is killed, recorded `expired`, and the MODEL is told.
// The enforcer is the reaper reading a COLUMN, not a context: a context dies with the process that made
// it and the whole point of a background task is to outlive that process (§3.6 D2, D11).
// ---------------------------------------------------------------------------------------------------

func TestATaskPastItsDeadlineIsKilledMarkedExpiredAndTheModelIsTold(t *testing.T) {
	// A ceiling short enough to measure, read through production's own reading of the environment.
	t.Setenv("PALAI_BACKGROUND_MAX_WALL_TIME", "900ms")
	f := newReaperFixture(t)
	pgids := f.spawnTasks(t, 1)

	rows := f.taskRows(t)
	if len(rows) != 1 {
		t.Fatalf("background_tasks rows = %d, want 1", len(rows))
	}
	if rows[0].deadline == nil {
		t.Fatal("deadline_at is NULL on a task started under PALAI_BACKGROUND_MAX_WALL_TIME=900ms: " +
			"nothing wrote the only time the reaper reads, so no ceiling can ever be enforced")
	}

	// Past the ceiling, and the process is STILL RUNNING — which is the whole point: an unbounded
	// background process is the orphan this feature is named after.
	time.Sleep(1200 * time.Millisecond)
	if !alive(pgids[0]) {
		t.Fatal("the task died on its own; a wall-clock proof needs a process that would otherwise keep going")
	}

	f.sweepOnce(t)

	if alive(pgids[0]) {
		t.Fatalf("process group %d is still alive after the reaper swept it past its deadline; "+
			"measured with kill(-pgid, 0), not with anything we recorded", pgids[0])
	}
	rows = f.taskRows(t)
	if rows[0].state != "expired" {
		t.Fatalf("state after the deadline = %q, want expired (killed and exited are different facts: "+
			"one is a decision, the other is what the command did)", rows[0].state)
	}
	notices := f.backgroundNotices(t)
	if len(notices) != 1 {
		t.Fatalf("background notices = %d, want 1: the model must LEARN its task was ended, not discover it", len(notices))
	}
	if !strings.Contains(notices[0], "expired") {
		t.Fatalf("the notice does not say the task expired, so the model cannot tell an ended build from a finished one: %q", notices[0])
	}
	if got := f.runState(t); got != "running" {
		t.Fatalf("run state after the notice = %q, want running: the parked run must be woken by an expiry too", got)
	}
}

// ---------------------------------------------------------------------------------------------------
// RED 1b — THE DEFAULT IS A CEILING AND UNBOUNDED HAS TO BE ASKED FOR. §0.2: unset means 60m, never
// "forever"; `0` means unbounded and must be WRITTEN. The silent case is the bounded one.
// ---------------------------------------------------------------------------------------------------

func TestUnsetMeansSixtyMinutesAndUnboundedMustBeWrittenExplicitly(t *testing.T) {
	bounded := newReaperFixture(t)
	bounded.spawnTasks(t, 1)
	rows := bounded.taskRows(t)
	if rows[0].deadline == nil {
		t.Fatal("a task started with PALAI_BACKGROUND_MAX_WALL_TIME unset carries no deadline: " +
			"the silent default is UNBOUNDED, which is precisely the orphan §0.2 refuses")
	}
	if d := time.Until(*rows[0].deadline); d < 55*time.Minute || d > 60*time.Minute {
		t.Fatalf("the unset default gives a %s ceiling, want ~60m", d)
	}

	t.Setenv("PALAI_BACKGROUND_MAX_WALL_TIME", "0")
	unbounded := newReaperFixture(t)
	unbounded.spawnTasks(t, 1)
	rows = unbounded.taskRows(t)
	if rows[0].deadline != nil {
		t.Fatalf("PALAI_BACKGROUND_MAX_WALL_TIME=0 still wrote a deadline (%s); an operator who asked for "+
			"unbounded and silently got a ceiling would have builds killed with no line naming why", *rows[0].deadline)
	}
}

// ---------------------------------------------------------------------------------------------------
// RED 2 — CANCELLATION. A cancelled run's live tasks are killed. Today CancelRunReconciled signals no
// process anywhere (§3.6 D10), so a backgrounded build survives the cancellation of the run that owns
// it. THE MEASUREMENT IS THE KERNEL'S.
// ---------------------------------------------------------------------------------------------------

func TestCancellingARunKillsEveryLiveBackgroundTaskOfIt(t *testing.T) {
	f := newReaperFixture(t)
	pgids := f.spawnTasks(t, 3)

	// THROUGH THE ORCHESTRATOR'S OWN SPINE, which is the store production cancels through: main.go's
	// startDispatch builds the orchestrator over repo and cancels over repo.Spine(), one value. This
	// fixture's f.spine is a second handle it opened for seeding, and using it here would prove a
	// cancellation against a store production never builds.
	if _, err := f.orch.spine.CancelRunReconciled(context.Background(), f.tenant, f.responseID, f.runID,
		[]byte(`{"status":"canceled"}`), []byte(`{"status":"uncertain"}`)); err != nil {
		t.Fatalf("CancelRunReconciled: %v", err)
	}

	// The kernel, not our rows: a cancellation that only wrote `killed` into a table would leave three
	// compilers running on an operator's Mac and report success.
	deadline := time.Now().Add(10 * time.Second)
	for _, pgid := range pgids {
		for time.Now().Before(deadline) && alive(pgid) {
			time.Sleep(20 * time.Millisecond)
		}
	}
	for i, pgid := range pgids {
		if alive(pgid) {
			t.Fatalf("background task %d (process group %d) survived the cancellation of the run that owns it", i, pgid)
		}
	}
	for i, r := range f.taskRows(t) {
		if r.state != "killed" {
			t.Fatalf("task %d state after the cancel = %q, want killed", i, r.state)
		}
		if !r.notified {
			t.Fatalf("task %d was left unsettled after the cancel, so the reaper will keep picking it up "+
				"and will journal an orphaned notice for a task an operator deliberately ended", i)
		}
	}
}

// ---------------------------------------------------------------------------------------------------
// RED 3 — RESTART / ADOPT. A control plane that restarted holds NO handle in memory, and the row does.
// E24 T5's lesson verbatim: an in-memory flag is erased by a restart, a column is not.
//
// This is the direct inverse of E26 T2's TestABackgroundTaskCannotBeKilledByIdAfterAControlPlaneRestart,
// which pinned the ceiling this task closes and is deleted by this commit rather than relaxed.
// ---------------------------------------------------------------------------------------------------

func TestARestartedControlPlaneAdoptsItsRunningTasksFromTheRowRatherThanFromMemory(t *testing.T) {
	f := newReaperFixture(t)
	pgids := f.spawnTasks(t, 1)
	rows := f.taskRows(t)

	// THE RESTART. A different store handle, a different orchestrator, a different executor value — the
	// only thing shared with the plane that spawned the task is the database and the operating system.
	plane := f.secondPlane(t)

	// Alive stays alive: the adopting plane must not reap what is still working.
	f.sweepWith(t, plane)
	if !alive(pgids[0]) {
		t.Fatalf("process group %d was killed by a plane that merely adopted it", pgids[0])
	}
	if got := f.taskRows(t)[0].state; got != "running" {
		t.Fatalf("state after an adopting sweep = %q, want running", got)
	}

	// And the ceiling T2 recorded is closed: the restarted plane can KILL BY ID, because the handle is
	// on the row rather than in the map that died with the process that made it.
	killed, err := plane.killAdoptedTask(context.Background(), f.tenant, f.runID, rows[0].id)
	if err != nil {
		t.Fatalf("a restarted control plane could not kill its own task by id: %v", err)
	}
	if killed.State != "exited" {
		t.Fatalf("the adopted kill reported %q, want exited", killed.State)
	}
	if got := f.taskRows(t)[0].state; got != "killed" {
		t.Fatalf("state after an adopted kill = %q, want killed: a kill WE performed settles the row so the "+
			"sweep does not notify a model about a task it stopped itself", got)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && alive(pgids[0]) {
		time.Sleep(20 * time.Millisecond)
	}
	if alive(pgids[0]) {
		t.Fatalf("process group %d survived a kill by the plane that adopted it", pgids[0])
	}
}

// TestADeadTaskNoWatcherEverSawGetsNoInventedExitCode is the second half of adoption and the one the
// migration's own comment is about: a control plane that restarted knows a process is gone and honestly
// cannot know what it returned. NULL is that answer. A sentinel would become a number a model compares
// against zero.
//
// IT PASSED AT THE FORK POINT AND IS HERE AS THE GUARD ADOPTION MUST NOT BREAK — T4's sweep already
// works from the row, so a handle nothing waited on already yielded NULL. That green would be worthless
// on its own, because a column NOTHING ever writes also reads NULL. The second half is the non-vacuity:
// a task this plane DID watch records a real number, so the NULL above is a distinction the code makes
// rather than a feature it lacks.
func TestADeadTaskNoWatcherEverSawGetsNoInventedExitCode(t *testing.T) {
	f := newReaperFixture(t)
	pgids := f.spawnTasks(t, 1)
	rows := f.taskRows(t)

	// A handle for a process that existed and is gone, and that nothing in this program ever waited on.
	orphaned, deadPid := deadHostHandle(t)
	f.rewriteHandle(t, rows[0].id, orphaned)
	_ = syscall.Kill(-pgids[0], syscall.SIGKILL)

	f.sweepOnce(t)

	got := f.taskRows(t)[0]
	if got.state != "exited" {
		t.Fatalf("state of a gone process = %q, want exited (pid %d)", got.state, deadPid)
	}
	if got.exitCode != nil {
		t.Fatalf("exit_code = %d for a process nothing in this program ever waited on; NULL is the truth "+
			"and every line of known-gaps exists to refuse the alternative", *got.exitCode)
	}

	// THE NON-VACUITY HALF: the same column, on a task this plane started and watched, carries the number
	// the command chose. Seven, because it is neither the 0 a default would give nor the 143 a kill would.
	watched := newReaperFixture(t)
	if err := watched.runAttemptWithScript(t, "completed", "exit 7"); err != nil {
		t.Fatalf("ExecuteAttempt: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		watched.sweepOnce(t)
		if r := watched.taskRows(t)[0]; r.state != "running" {
			if r.exitCode == nil || *r.exitCode != 7 {
				t.Fatalf("exit_code of a task this plane watched = %v, want 7: if this column is never "+
					"written at all, the NULL asserted above is vacuous", r.exitCode)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the watched task never settled")
}

// TestKillingATaskThatAlreadyFinishedClaimsNothing is the asymmetry the kill path owes the audit record.
// Killing a finished task must stay idempotent — §3.5 P7's "cancelling twice is idempotent" — but it must
// NOT record `killed` for a build that exited on its own, because that is a false statement in a row an
// operator reads AND it throws away the exit code, by settling the row before the sweep can read one.
func TestKillingATaskThatAlreadyFinishedClaimsNothing(t *testing.T) {
	f := newReaperFixture(t)
	if err := f.runAttemptWithScript(t, "completed", "exit 7"); err != nil {
		t.Fatalf("ExecuteAttempt: %v", err)
	}
	rows := f.taskRows(t)
	// Wait for the OPERATING SYSTEM to stop listing it, rather than for anything we recorded.
	deadline := time.Now().Add(10 * time.Second)
	plane := f.secondPlane(t)
	for time.Now().Before(deadline) {
		if _, err := plane.killAdoptedTask(context.Background(), f.tenant, f.runID, rows[0].id); err != nil {
			t.Fatalf("killing a finished task is not idempotent: %v", err)
		}
		if got := f.taskRows(t)[0]; got.state != "running" {
			t.Fatalf("killing a task that had already finished recorded %q; the row now lies about how the "+
				"task ended and the sweep will never read its exit code", got.state)
		}
		if !aliveInTable(t, rows[0].handle) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// And the sweep still records the truth: the code the command chose, not the kill.
	for time.Now().Before(deadline) {
		f.sweepOnce(t)
		if got := f.taskRows(t)[0]; got.state != "running" {
			if got.state != "exited" || got.exitCode == nil || *got.exitCode != 7 {
				t.Fatalf("after a kill of an already-finished task the sweep recorded state=%q exit_code=%v, want exited/7", got.state, got.exitCode)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the finished task never settled")
}

// aliveInTable asks the kernel about the pgid half of a host handle. It parses the handle the way the row
// stores it, so the question is asked about the number the ROW holds rather than one the test kept.
func aliveInTable(t *testing.T, handle string) bool {
	t.Helper()
	pgidText, _, ok := strings.Cut(handle, ":")
	if !ok {
		t.Fatalf("malformed host handle %q", handle)
	}
	pgid, err := strconv.Atoi(pgidText)
	if err != nil {
		t.Fatalf("malformed host handle %q: %v", handle, err)
	}
	return alive(pgid)
}

// TestAnUnprovableHandleBecomesLostAndReceivesNoSignal is the rule the honest ceiling rests on, and it
// is absolute: not killed on a deadline, not killed on a cancellation, not killed on a sweep. PID reuse
// is real, and killing a stranger's process is worse than leaving our own orphan alive.
func TestAnUnprovableHandleBecomesLostAndReceivesNoSignal(t *testing.T) {
	f := newReaperFixture(t)
	pgids := f.spawnTasks(t, 1)
	rows := f.taskRows(t)

	// The pgid is OURS and alive; the recorded start time is not its. That is exactly what a reused pid
	// looks like from a row, and it is the only evidence the host posture has.
	f.rewriteHandle(t, rows[0].id, strconv.Itoa(pgids[0])+":1999-01-01T00:00:00Z")

	f.sweepOnce(t)

	if got := f.taskRows(t)[0].state; got != "lost" {
		t.Fatalf("state of a handle we cannot prove is ours = %q, want lost", got)
	}
	if !alive(pgids[0]) {
		t.Fatalf("process group %d WAS SIGNALLED although the row could not be proven to name it; "+
			"on a real machine that is somebody else's process", pgids[0])
	}

	// And an explicit kill of the same row must refuse FOR THE RIGHT REASON. Before adoption this call
	// failed with "no such task belongs to this run" — a lookup miss, not a refusal — and a test that
	// accepted any error would have called that a proof about signalling. It must be ErrHandleLost.
	plane := f.secondPlane(t)
	_, err := plane.killAdoptedTask(context.Background(), f.tenant, f.runID, rows[0].id)
	if !errors.Is(err, toolbroker.ErrHandleLost) {
		t.Fatalf("killing a lost handle returned %v, want ErrHandleLost: the row must be FOUND and then "+
			"REFUSED, because a lookup miss would report the same green for a plane that lost the handle", err)
	}
	if !alive(pgids[0]) {
		t.Fatalf("process group %d was signalled by an explicit kill of a lost handle", pgids[0])
	}
}

// ---------------------------------------------------------------------------------------------------
// RED 4 — CONCURRENCY, ENFORCED RATHER THAN DECLARED (§3.6 D12). E24 opened `runners.capacity` and no Go
// expression ever read it. THE COUNTS BELOW COME FROM THE DATABASE AND FROM THE PROCESS TABLE.
// ---------------------------------------------------------------------------------------------------

func TestTheSixthBackgroundTaskOfARunIsRefusedRatherThanQueued(t *testing.T) {
	f := newReaperFixture(t)
	// A DELTA RATHER THAN AN ABSOLUTE, because the machine-wide count is shared with every other test in
	// this package and an absolute number would make this proof depend on what ran before it.
	base := f.runningRowCount(t)
	// The SHIPPED default of five, not a number this test chose.
	pgids := f.spawnTasks(t, 5)

	if n := f.runningRowCount(t); n != base+5 {
		t.Fatalf("the database holds %d running tasks after five spawns, want %d", n, base+5)
	}
	for i, pgid := range pgids {
		if !alive(pgid) {
			t.Fatalf("task %d (process group %d) is not in the process table; the ceiling proof needs five real processes", i, pgid)
		}
	}

	// The sixth, through the shipped dispatcher, on the same run.
	sixth := filepath.Join(f.root, "sixth.pgid")
	out, err := f.dispatchBackground(t, bgScript(sixth, "sleep 30"))
	if err == nil && out["status"] != nil {
		t.Fatalf("the sixth background task of a run was ACCEPTED (%v); §0.3 refuses rather than queues, "+
			"because a queue is a delay the model cannot see", out)
	}
	if n := f.runningRowCount(t); n != base+5 {
		t.Fatalf("the database holds %d running tasks after the sixth was refused, want %d", n, base+5)
	}
	if _, statErr := os.Stat(sixth); statErr == nil {
		t.Fatal("the sixth command RAN: a refusal that starts the process first is not a refusal")
	}
}

func TestTheMachineCeilingIsCountedAcrossTenantsFromTheDatabase(t *testing.T) {
	// TWO TASKS OF A DIFFERENT TENANT, started first and under the shipped default. The machine ceiling is
	// about the MACHINE: a count that saw only the caller's tenant would give every tenant its own twenty
	// and the Mac as many as there are tenants.
	other := newReaperFixture(t)
	other.spawnTasks(t, 2)

	f := newReaperFixture(t)
	if mine := f.runningRowCount(t) - f.runningRowCountOfTenant(t); mine == 0 {
		t.Fatal("no other tenant holds a running task, so nothing here would distinguish a machine ceiling from a per-run one")
	}
	if own := f.runningRowCountOfTenant(t); own != 0 {
		t.Fatalf("this fixture's tenant already holds %d running tasks; the refusal below could then be the per-run ceiling", own)
	}

	// The machine is now exactly AT its ceiling, and not one of the rows that fill it belongs to the run
	// about to ask. Its own per-run count is zero, so only the machine ceiling can refuse it.
	t.Setenv("PALAI_BACKGROUND_MAX_PER_HOST", strconv.Itoa(f.runningRowCount(t)))
	before := f.runningRowCount(t)

	first := filepath.Join(f.root, "first.pgid")
	out, err := f.dispatchBackground(t, bgScript(first, "sleep 30"))
	if err == nil && out["status"] != nil {
		t.Fatalf("a task was accepted with the machine already at its ceiling (%v)", out)
	}
	if _, statErr := os.Stat(first); statErr == nil {
		t.Fatal("the refused command RAN")
	}
	if n := f.runningRowCount(t); n != before {
		t.Fatalf("the database holds %d running tasks after the refusal, want %d", n, before)
	}
}

// ---------------------------------------------------------------------------------------------------
// RED 5 — GARBAGE. A finished task's log is deleted after PALAI_BACKGROUND_LOG_TTL; a running task's is
// not. Without it `.palai-session` grows without bound — and because Snapshot skips that subtree (§3.6
// D7) NOBODY WOULD NOTICE, which is exactly what a silent disk leak looks like.
//
// The container half of this claim is already shipped and is proven on a real daemon by T1's
// TestKillingADetachedTaskRemovesTheContainerAndIsIdempotent (tests/component/background): a kill is a
// ContainerKill followed by a ContainerRemove, and the suite's own before/after container count is the
// assertion. It is not re-proven here.
// ---------------------------------------------------------------------------------------------------

func TestAFinishedTasksLogIsDeletedAfterItsTTLAndARunningTasksIsNot(t *testing.T) {
	t.Setenv("PALAI_BACKGROUND_LOG_TTL", "1h")
	f := newReaperFixture(t)
	pgids := f.spawnTasks(t, 2)
	rows := f.taskRows(t)

	// The first task finishes; the second keeps running. Both logs are then backdated well past the TTL,
	// so the ONLY thing that can distinguish them is whether the task is still running.
	_ = syscall.Kill(-pgids[0], syscall.SIGKILL)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && alive(pgids[0]) {
		time.Sleep(20 * time.Millisecond)
	}
	f.sweepOnce(t) // settles the first task, leaves the second running

	finished := filepath.Join(f.root, rows[0].output)
	running := filepath.Join(f.root, rows[1].output)
	old := time.Now().Add(-48 * time.Hour)
	for _, p := range []string{finished, running} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatalf("backdate %s: %v", p, err)
		}
	}

	f.sweepOnce(t)

	if _, err := os.Stat(finished); err == nil {
		t.Fatalf("the log of a settled task is still on disk %s past its TTL; .palai-session grows without "+
			"bound and the snapshot skips it, so nobody would ever see it", time.Since(old))
	}
	if _, err := os.Stat(running); err != nil {
		t.Fatalf("the log of a STILL RUNNING task was deleted (%v): a build writing nothing for a day is "+
			"not a finished build, and truncating its output mid-flight is worse than keeping it", err)
	}
}

// dispatchBackground drives ONE background shell call through the PRODUCTION dispatcher — the same
// dispatchTool that resolves the tool out of the registered broker, runs the approval gate and writes the
// ledger row — and returns the decoded body the model would have received plus the dispatcher's own
// error. A ceiling refusal may surface as either, and the callers above accept either; what they do not
// accept is a process.
func (f *reaperFixture) dispatchBackground(t *testing.T, script string) (map[string]any, error) {
	t.Helper()
	ch := &recordingChannel{}
	st := &attemptState{
		attempt: AttemptDescriptor{
			RunID: contracts.RunID(f.runID), AttemptID: contracts.AttemptID(redeliveryID("att")),
			Fence: 1, WorkspaceHostPath: f.root,
		},
		tenant: f.tenant, sessionID: f.sessionID, responseID: f.responseID, ch: ch,
	}
	err := f.orch.dispatchTool(context.Background(), st,
		toolRequestFrame(redeliveryID("tc"), "palai.workspace.shell", map[string]any{
			"argv": []any{script}, "shell": true, "background": true,
		}))
	if err != nil {
		return nil, err
	}
	results := toolResults(ch)
	if len(results) != 1 {
		t.Fatalf("the shell tool delivered %d tool.result frames, want 1: %+v", len(results), results)
	}
	var out map[string]any
	if uerr := json.Unmarshal([]byte(results[0].content), &out); uerr != nil {
		t.Fatalf("decode the shell result %q: %v", results[0].content, uerr)
	}
	return out, nil
}

// killAdoptedTask is the kill a RESTARTED control plane performs: by task id, through the orchestrator's
// own background seam, with nothing in memory from the plane that started the task.
func (o *Orchestrator) killAdoptedTask(ctx context.Context, tenant coordinator.Tenant, runID, taskID string) (toolbroker.BackgroundTicket, error) {
	b := &backgroundTasks{orch: o, tenant: tenant, runID: runID}
	return b.KillBackground(ctx, taskID)
}
