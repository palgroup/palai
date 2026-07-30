//go:build component

package execution

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/host"
	"github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
	statemachines "github.com/palgroup/palai/packages/state-machines"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"

	"github.com/palgroup/palai/storage"
)

// E26 T4 — THE WAKE: exit -> notification -> the model's next turn, against a REAL Postgres, REAL
// detached host processes and the SHIPPED reconciler.
//
// Everything a claim here rests on is measured from something that is not our own bookkeeping. The
// task is started the way a model starts one (a tool.request for palai.workspace.shell with
// `background: true`, through the production dispatcher). It FINISHES the way a build finishes — the
// test drops a sentinel file the script is polling for and the process exits with a code IT chose, so
// the exit code in the notification is one the operating system reported and not one we wrote down.
// The sweep is the shipped Reconciler.Sweep, the same pass that dead-letters jobs and expires
// approvals, and the delivery is the shipped pumpCommands at a real boundary.

// wakeScript is one shell line that records its OWN process-group id, blocks until the test drops a
// sentinel, writes a marker its notification tail must carry, and exits with a NON-ZERO, NON-SIGNAL
// code. Seven is chosen for what it is not: not 0 (which any default would produce), not 1 (which a
// failed `test -f` would produce) and not 143 (which killing the group would produce).
func wakeScript(pidFile, doneFile string) string {
	return "echo $$ > " + pidFile + "; while [ ! -f " + doneFile + " ]; do sleep 0.05; done; " +
		"echo " + wakeTailMarker + "; exit 7"
}

const wakeTailMarker = "BUILD-FINISHED-MARKER"

// wakeFixture is parkFixture plus the two rows that make an allocation ROOT resolvable from a task
// row — a workspace bound to the session and a physical allocation whose host_path is that root. They
// are seeded because production has them: the notification's tail is read by joining the row's
// allocation-relative output_path onto the root the run's allocation names, and a fixture without them
// would prove a tail read that production could never perform.
type wakeFixture struct {
	*parkFixture
	pidFile  string
	doneFile string
}

func newWakeFixture(t *testing.T) *wakeFixture {
	t.Helper()
	f := newParkFixture(t)
	sys := storage.WithSystemScope(context.Background())
	wsID := redeliveryID("ws")
	stmts := [][]any{
		{`INSERT INTO workspaces (id, organization_id, project_id, session_id, state)
		  VALUES ($1,$2,$3,$4,'ready')`, wsID, f.tenant.Organization, f.tenant.Project, f.sessionID},
		{`INSERT INTO workspace_allocations (id, workspace_id, organization_id, project_id, fence, host_path, state)
		  VALUES ($1,$2,$3,$4,1,$5,'active')`, redeliveryID("alloc"), wsID, f.tenant.Organization, f.tenant.Project, f.root},
	}
	for _, stmt := range stmts {
		if _, err := f.spine.Pool().Exec(sys, stmt[0].(string), stmt[1:]...); err != nil {
			t.Fatalf("seed the allocation: %v", err)
		}
	}
	return &wakeFixture{
		parkFixture: f,
		pidFile:     filepath.Join(f.root, "wake.pgid"),
		doneFile:    filepath.Join(f.root, "wake.done"),
	}
}

// spawnAndTerminal drives ONE whole attempt that starts a background task by tool name and then reports
// the given terminal outcome, exactly as parkFixture.runAttempt does — with this file's controllable
// script instead of an unconditional sleep.
func (f *wakeFixture) spawnAndTerminal(t *testing.T, outcome string) {
	t.Helper()
	frames := []contracts.EngineFrame{
		engineFrame(1, "engine.ready", map[string]any{
			"selected_protocol": engineProtocol, "engine": map[string]any{"version": "test"},
		}),
		engineFrame(2, "tool.request", map[string]any{
			"tool_call_id": redeliveryID("tc"), "name": "palai.workspace.shell",
			"arguments": map[string]any{"argv": []any{wakeScript(f.pidFile, f.doneFile)}, "shell": true, "background": true},
		}),
		engineFrame(3, "run.terminal", map[string]any{"outcome": outcome}),
	}
	t.Cleanup(func() {
		if pgid, err := readPgid(f.pidFile); err == nil {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
	})
	f.orch.dialer = &scriptedDialer{ch: &scriptedChannel{frames: frames}}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := f.orch.ExecuteAttempt(ctx, AttemptDescriptor{
		RunID: contracts.RunID(f.runID), AttemptID: contracts.AttemptID(redeliveryID("att")),
		Fence: 1, WorkspaceHostPath: f.root,
	}); err != nil {
		t.Fatalf("ExecuteAttempt(%s) error = %v", outcome, err)
	}
	if pgid, err := waitPgid(f.pidFile); err != nil {
		t.Fatalf("the background task never wrote its process-group id: %v", err)
	} else if syscall.Kill(-pgid, 0) != nil {
		t.Fatalf("the background task was not running when the attempt ended; every claim below would be a claim about nothing")
	}
}

// finishTask drops the sentinel the script polls for and waits for the KERNEL to stop listing the
// process group. Nothing here reads our own bookkeeping: the task is finished when `kill(-pgid, 0)`
// says so.
func (f *wakeFixture) finishTask(t *testing.T) {
	t.Helper()
	pgid, err := waitPgid(f.pidFile)
	if err != nil {
		t.Fatalf("read the task's process-group id: %v", err)
	}
	if err := os.WriteFile(f.doneFile, []byte("go"), 0o600); err != nil {
		t.Fatalf("drop the finish sentinel: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(-pgid, 0) != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the background task was still running 20s after its sentinel appeared")
}

// sweepOnce runs ONE pass of the shipped reconciler — the same Sweep that dead-letters jobs, expires
// approvals and ends capacity parks. A background notification that needed its own loop would not be
// covered by this call, and that is the point of making the test call this one.
func (f *wakeFixture) sweepOnce(t *testing.T) {
	t.Helper()
	f.sweepWith(t, f.orch)
}

// sweepWith runs one pass through an arbitrary orchestrator's observer, so a test can sweep from a
// SECOND control plane or from a restarted one.
func (f *wakeFixture) sweepWith(t *testing.T, orch *Orchestrator) {
	t.Helper()
	r := NewReconciler(f.spine, time.Hour, 5).WithBackgroundTasks(orch.BackgroundObserver())
	if _, err := r.Sweep(context.Background()); err != nil {
		t.Fatalf("Reconciler.Sweep() error = %v", err)
	}
}

// secondPlane builds a DIFFERENT control plane over the same database: its own store handle, its own
// orchestrator, its own host executor. It is what "two control planes" and "a crash-restart" both mean
// here — no in-memory state is shared with the plane that started the task.
func (f *wakeFixture) secondPlane(t *testing.T) *Orchestrator {
	t.Helper()
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	repo, err := store.Open(context.Background(), url)
	if err != nil {
		t.Fatalf("store.Open for the second plane: %v", err)
	}
	t.Cleanup(repo.Close)
	exec := host.NewExecutor(0)
	orch := NewOrchestrator(repo, nil, modelbroker.New(modelbroker.Config{}), toolbroker.New(tools.ShellTool(), tools.FileTool()))
	orch.SetShellRunner(exec)
	orch.SetBackgroundRunner(exec)
	return orch
}

// backgroundNotices reads the run's queued background_notice commands, in creation order, with their
// message text — the durable set the boundary pump will deliver.
func (f *wakeFixture) backgroundNotices(t *testing.T) []string {
	t.Helper()
	rows, err := f.spine.Pool().Query(storage.WithSystemScope(context.Background()),
		`SELECT payload->>'message' FROM commands
		 WHERE run_id = $1 AND kind = 'background_notice' ORDER BY created_at, id`, f.runID)
	if err != nil {
		t.Fatalf("read background notices: %v", err)
	}
	defer rows.Close()
	var msgs []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			t.Fatalf("scan background notice: %v", err)
		}
		msgs = append(msgs, m)
	}
	return msgs
}

// taskRow reads the one background_tasks row this fixture's run owns.
func (f *wakeFixture) taskRow(t *testing.T) (id, state, outputPath string, exitCode *int, notified bool) {
	t.Helper()
	err := f.spine.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT id, state, output_path, exit_code, notified_at IS NOT NULL
		 FROM background_tasks WHERE run_id = $1 ORDER BY started_at, id`, f.runID).
		Scan(&id, &state, &outputPath, &exitCode, &notified)
	if err != nil {
		t.Fatalf("read the background_tasks row: %v", err)
	}
	return
}

// runJobCount counts the response.run jobs enqueued for this run — the wake's other half. A
// notification that woke a run it should not have would show up here even if the run state did not.
func (f *wakeFixture) runJobCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.spine.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(*) FROM durable_jobs WHERE kind = 'response.run' AND payload->>'run_id' = $1`, f.runID).Scan(&n); err != nil {
		t.Fatalf("count response.run jobs: %v", err)
	}
	return n
}

// warningCodes reads the warning.raised.v1 codes in the session journal — where an operator meets a
// notification that had nowhere to land.
func (f *wakeFixture) warningCodes(t *testing.T) []string {
	t.Helper()
	rows, err := f.spine.Pool().Query(storage.WithSystemScope(context.Background()),
		`SELECT payload->>'code' FROM events WHERE session_id = $1 AND type = 'warning.raised.v1' ORDER BY seq`, f.sessionID)
	if err != nil {
		t.Fatalf("read warnings: %v", err)
	}
	defer rows.Close()
	var codes []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("scan warning: %v", err)
		}
		codes = append(codes, c)
	}
	return codes
}

// ---------------------------------------------------------------------------------------------------
// RED 1 (§2.3) — a parked run re-enters EXACTLY ONCE, and the turn the model sees carries the four
// things a model needs to act: which task, how it ended, where the output is, and enough of that output
// to decide without reading the file.
// ---------------------------------------------------------------------------------------------------

func TestAParkedRunReentersExactlyOnceWhenItsBackgroundTaskFinishes(t *testing.T) {
	f := newWakeFixture(t)
	f.spawnAndTerminal(t, "completed")
	if got := f.runState(t); got != "waiting" {
		t.Fatalf("run state after the terminal = %q, want waiting (T3's park); nothing below is about T4 without it", got)
	}
	taskID, _, outputPath, _, _ := f.taskRow(t)

	f.finishTask(t)
	f.sweepOnce(t)

	if got := f.runState(t); got != "running" {
		t.Fatalf("run state after the sweep = %q, want running: the exit did not re-enter the parked run", got)
	}
	if n := f.runJobCount(t); n != 1 {
		t.Fatalf("response.run jobs enqueued = %d, want exactly 1: the wake must make the run claimable exactly once", n)
	}
	notices := f.backgroundNotices(t)
	if len(notices) != 1 {
		t.Fatalf("background_notice commands = %d, want exactly 1", len(notices))
	}

	// The four things the model's turn must carry, each asserted for its own reason: the task id
	// because a run may have several, the exit code because it is the whole answer, the output path
	// because reading more is an explicit call, and the tail because the point of the notification is
	// that the model can act WITHOUT that call.
	notice := notices[0]
	for _, want := range []string{taskID, "exit code 7", outputPath, wakeTailMarker} {
		if !strings.Contains(notice, want) {
			t.Fatalf("the notification does not carry %q; it reads:\n%s", want, notice)
		}
	}
	if len(notice) > 4096 {
		t.Fatalf("the notification is %d bytes; the tail is meant to be BOUNDED (2 KiB) so a chatty build cannot flood a model's context", len(notice))
	}

	// And the model actually SEES it: the shipped boundary pump turns the queued command into the same
	// message.deliver frame a send_message produces, which is the frame the engine folds into the next
	// model request.
	st, ch := f.boundary()
	if err := f.orch.pumpCommands(context.Background(), st, "mr_after_wake"); err != nil {
		t.Fatalf("pumpCommands() error = %v", err)
	}
	delivered := ch.deliverOrder()
	if len(delivered) != 1 {
		t.Fatalf("message.deliver frames at the boundary = %d, want 1", len(delivered))
	}
	if got := ch.delivers(delivered[0]); len(got) != 1 || !strings.Contains(got[0], wakeTailMarker) {
		t.Fatalf("the delivered turn = %v, want the notification text carrying %q", got, wakeTailMarker)
	}
}

// boundary builds one attempt state and a recording channel, the way every other boundary proof in this
// package does (redelivery_component_test.go's attemptAt).
func (f *wakeFixture) boundary() (*attemptState, *recordingChannel) {
	ch := &recordingChannel{}
	return &attemptState{
		attempt:    AttemptDescriptor{RunID: contracts.RunID(f.runID), AttemptID: contracts.AttemptID(redeliveryID("att"))},
		tenant:     f.tenant,
		sessionID:  f.sessionID,
		responseID: f.responseID,
		ch:         ch,
	}, ch
}

// ---------------------------------------------------------------------------------------------------
// RED 2 — EXACTLY ONCE across two ticks, two control planes and a crash-restart.
// ---------------------------------------------------------------------------------------------------

func TestTwoTicksTwoPlanesAndARestartProduceOneBackgroundNotice(t *testing.T) {
	f := newWakeFixture(t)
	f.spawnAndTerminal(t, "completed")
	f.finishTask(t)

	// TWO PLANES, CONCURRENTLY. The second is a different store handle, a different orchestrator and a
	// different host executor over the same database — it shares no memory with the plane that started
	// the task, which is what makes this a proof about a ROW and not about a mutex.
	other := f.secondPlane(t)
	var wg sync.WaitGroup
	for _, orch := range []*Orchestrator{f.orch, other, f.orch, other} {
		wg.Add(1)
		go func(o *Orchestrator) {
			defer wg.Done()
			r := NewReconciler(f.spine, time.Hour, 5).WithBackgroundTasks(o.BackgroundObserver())
			_, _ = r.Sweep(context.Background())
		}(orch)
	}
	wg.Wait()

	// AND A CRASH-RESTART: a plane built after the fact, which never saw the task start.
	f.sweepWith(t, f.secondPlane(t))

	if notices := f.backgroundNotices(t); len(notices) != 1 {
		t.Fatalf("background_notice commands after two ticks, two planes and a restart = %d, want exactly 1", len(notices))
	}
	if n := f.runJobCount(t); n != 1 {
		t.Fatalf("response.run jobs = %d, want exactly 1: a second wake would re-enter a run that is already running", n)
	}
	if _, _, _, _, notified := f.taskRow(t); !notified {
		t.Fatalf("the task row carries no notified_at stamp; nothing would stop the next tick notifying again")
	}
}

// ---------------------------------------------------------------------------------------------------
// RED 3 — A RUNNING RUN IS NOT INTERRUPTED. The model is still working; the notification waits for the
// next boundary rather than re-entering a run that never left.
// ---------------------------------------------------------------------------------------------------

func TestARunningRunIsNotInterruptedAndTheNoticeFoldsAtTheNextBoundary(t *testing.T) {
	f := newWakeFixture(t)
	f.spawnAndTerminal(t, "completed")
	// The park, then a wake that is NOT this task's: the model is working again — a resumed attempt is
	// driving the run — and the task it left behind is still compiling.
	if _, err := f.spine.ApplyRunTransition(context.Background(), f.tenant, f.runID, statemachines.RunCmdResume); err != nil {
		t.Fatalf("resume the run: %v", err)
	}
	jobsBefore := f.runJobCount(t)

	f.finishTask(t)
	f.sweepOnce(t)

	if got := f.runState(t); got != "running" {
		t.Fatalf("run state = %q, want running: a run the model is still driving must not be transitioned by an exit notification", got)
	}
	if got := f.runJobCount(t); got != jobsBefore {
		t.Fatalf("response.run jobs went %d -> %d: a running run must not be re-enqueued, which would run two attempts of one run", jobsBefore, got)
	}
	if notices := f.backgroundNotices(t); len(notices) != 1 {
		t.Fatalf("background_notice commands = %d, want exactly 1 queued and waiting for the boundary", len(notices))
	}

	// It is not lost, it is WAITING: the next boundary of the attempt already running delivers it.
	st, ch := f.boundary()
	if err := f.orch.pumpCommands(context.Background(), st, "mr_next_boundary"); err != nil {
		t.Fatalf("pumpCommands() error = %v", err)
	}
	if got := ch.deliverOrder(); len(got) != 1 {
		t.Fatalf("message.deliver frames at the next boundary = %d, want 1 (the notice folded in without interrupting anything)", len(got))
	}
}

// ---------------------------------------------------------------------------------------------------
// RED 4 — A TERMINAL RUN. There is no turn left for a notification to land in, and dropping it silently
// is what every line of known-gaps refuses.
// ---------------------------------------------------------------------------------------------------

func TestATerminalRunsBackgroundNoticeIsStampedRatherThanDropped(t *testing.T) {
	f := newWakeFixture(t)
	// A `failed` terminal does NOT park (T3): the run ends while its task keeps compiling. This is the
	// production route to the case, not a hand-written row.
	f.spawnAndTerminal(t, "failed")
	if got := f.runState(t); got != "failed" {
		t.Fatalf("run state = %q, want failed", got)
	}

	f.finishTask(t)
	f.sweepOnce(t)

	if notices := f.backgroundNotices(t); len(notices) != 0 {
		t.Fatalf("background_notice commands on a terminal run = %d, want 0: it would sit queued until it expired unread", len(notices))
	}
	if got := f.runState(t); got != "failed" {
		t.Fatalf("run state = %q, want failed: a terminal run must not be revived by a notification", got)
	}
	// NOT LOST: the row is complete and stamped, so no tick retries it...
	id, state, _, exitCode, notified := f.taskRow(t)
	if state != "exited" || exitCode == nil || *exitCode != 7 {
		t.Fatalf("task row = (%s, state %q, exit %v), want exited with exit code 7", id, state, exitCode)
	}
	if !notified {
		t.Fatalf("task row carries no notified_at stamp: the next tick would try to notify a terminal run again, forever")
	}
	// ...and an operator MEETS it, in the journal, with the task id in the payload.
	codes := f.warningCodes(t)
	found := false
	for _, c := range codes {
		if c == "background_notice_orphaned" {
			found = true
		}
	}
	if !found {
		t.Fatalf("session journal warnings = %v, want a background_notice_orphaned: an exit nobody was told about must be visible to somebody", codes)
	}
}

// ---------------------------------------------------------------------------------------------------
// RED 5 (§3.6 D14) — THE SWEEP IS THE ONLY PATH. A deployment whose reconciler does not run receives no
// notification at all, and that is a DECLARATION rather than a surprise: PALAI_DISPATCH_WORKERS=0
// returns from startDispatch before the reconciler is built (main.go), and the shipped compose file
// sets exactly that.
// ---------------------------------------------------------------------------------------------------

func TestWithoutTheReconcilerSweepNoBackgroundNoticeEverLands(t *testing.T) {
	f := newWakeFixture(t)
	f.spawnAndTerminal(t, "completed")
	f.finishTask(t)

	// Everything a stack does EXCEPT sweep: the task has exited, its row is durable, and time passes.
	time.Sleep(500 * time.Millisecond)
	if notices := f.backgroundNotices(t); len(notices) != 0 {
		t.Fatalf("background_notice commands with no sweep = %d, want 0; if a notification can land without the reconciler, the declaration in docs/operations/background-execution.md is wrong", len(notices))
	}
	if got := f.runState(t); got != "waiting" {
		t.Fatalf("run state with no sweep = %q, want waiting: the run stays parked, which is what an operator on PALAI_DISPATCH_WORKERS=0 gets", got)
	}
	if _, _, _, _, notified := f.taskRow(t); notified {
		t.Fatalf("the task row was stamped notified without any sweep running")
	}

	// NON-VACUITY, in the same function: the identical fixture, one sweep, and the notification lands.
	// Without this half the assertions above would pass on a tree where notifications never worked.
	f.sweepOnce(t)
	if notices := f.backgroundNotices(t); len(notices) != 1 {
		t.Fatalf("after one sweep the notices = %d, want 1: the assertions above would have been free", len(notices))
	}
}
