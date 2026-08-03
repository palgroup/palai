package coordinator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	statemachines "github.com/palgroup/palai/packages/state-machines"
	"github.com/palgroup/palai/storage"
)

// This file is the durable half of E26's background execution: the row a started process leaves behind,
// and the sweep that turns that process's EXIT into the model's next turn.
//
// THE WAKE IS NOT WRITTEN HERE. It is wakeParkedRunTx, the same function an approve, a deny and the
// approval reaper all end in (approvals.go). E23 T1 wrote that choreography, E24 T4 used it a second
// time, and this is its third user: waiting -> running plus an EnqueueJob, in the transaction that
// justified it, no-op on a run that is not waiting. A second waking body would be a second waking bug,
// and this tree has paid for that kind of divergence before.
//
// WHAT IS NEW HERE IS THE OTHER TWO THIRDS OF THE PROBLEM, and neither is a wake:
//   - a run that has NOT parked (the model is still working) must be told without being interrupted, so
//     the command is enqueued and the wake simply finds nothing to wake;
//   - a run that is already TERMINAL has no turn left for the notification to land in, so it is stamped
//     and journalled rather than queued against a run that would let it expire unread.
//
// Both live in ONE transaction with the exactly-once claim, because "notify" and "wake" committing
// separately is how a notification gets written twice or a run gets woken for nothing.

// backgroundNoticePrefix opens every notification this file writes.
//
// IT IS A CONVENTION AND NOT A CONTRACT, and the difference is worth stating where a reader meets it
// (E26 §3.6 D17). A delivered message folds into the engine as a USER TURN — commands.py's deliver
// calls context.queue_delivery, which appends {"role": "user"} — so the model cannot tell a
// control-plane notification from something a person typed. The prefix is what a prompt can be written
// against; it is not something the protocol enforces. The upgrade path has a name: a `role`/`source`
// field on message.deliver, which is a protocol version and not this epic.
const backgroundNoticePrefix = "[palai:background]"

// backgroundNoticeKind is the command kind the boundary pump delivers. It is a DISTINCT kind rather
// than a send_message for two reasons that are both about what happens at a run terminal: a
// send_message CARRIES to the next response (commands.sql ExpireQueuedCommandsForRun), which would
// replay a finished build's exit into an unrelated conversation, and an operator reading the command
// log should be able to tell what the control plane said from what a person said.
const backgroundNoticeKind = "background_notice"

// backgroundOrphanedWarning is the code an operator meets when an exit had nowhere to land. It rides
// warning.raised.v1, the event type this tree already uses for exactly this shape of fact — a queued
// message that will not be delivered the way its sender expected (commands.go warnSurvivingSendMessages).
//
// A NEW EVENT TYPE WAS THE OTHER OPTION AND WAS NOT TAKEN: the event alphabet is a published contract
// (protocols/schemas/execution/event-types.json, pinned against the AsyncAPI document in order AND
// content), and E26 T4 owns no contract change. What matters is that the fact is durable and visible,
// and warning.raised.v1 is both.
const backgroundOrphanedWarning = "background_notice_orphaned"

// BackgroundTailLimit bounds what a notification quotes from the task's log: the last 2 KiB.
//
// AND THIS EXCERPT ARRIVES ALREADY REDACTED SINCE E26 T6, which is said here because for two tasks this
// comment said the opposite. T2 recorded that a background task's log is not masked on the way out
// (§3.6 D8) — a process writing its own file bypasses both RedactSecrets and RedactValues, which act on
// a CAPTURED Go string — and T4 widened it, because this excerpt lands in a DURABLE ROW
// (commands.payload, then delivered_messages) where the synchronous path's equivalent had been masked
// before it reached tool_calls.result.
//
// T6 CLOSED BOTH LANDING SITES FROM ONE PLACE, and the redaction is the OBSERVER's rather than this
// file's: execution.redactTaskOutput masks the tail before it is handed over, and the identical call
// masks a palai.workspace.file read of the same path. That is why BackgroundTask.EnvKeys exists — key
// NAMES, never values, re-resolved at read time — and why the coordinator, which owns the transaction
// and not the credentials, never sees either.
//
// THE BOUND IS THE FEATURE, not a safety margin. A model backgrounds a five-minute build precisely so
// it does not have to hold that build's output (E26 T2's returned body carries none either); a
// notification that pasted the whole log would spend on the wake exactly what the spawn saved. Two KiB
// is the end of a build log — the error, the summary line, the test count — which is what a model needs
// to decide whether to read the rest.
const BackgroundTailLimit = 2048

// BackgroundTask is one row of background_tasks as the sweep reads it, plus the allocation root the
// row deliberately does not store (output_path is allocation-relative so no row discloses a host path,
// spec §29.9). The root is joined at read time from the run's session workspace.
type BackgroundTask struct {
	ID         string
	Tenant     Tenant
	RunID      string
	SessionID  string
	ResponseID string
	Posture    string
	Handle     string
	// MachineID is the machine whose kernel Handle names (A.3 T6). EMPTY means the row predates the
	// column and the machine is unrecoverable — the row never held it — which the observer reads as
	// `lost` rather than probing locally.
	MachineID      string
	OutputPath     string
	EnvKeys        []string
	AllocationRoot string
	// DeadlineAt is the WALL CLOCK the reaper enforces (E26 T5, §0.2), and it is the only time anything
	// here reads. NIL means the operator wrote an explicit `0` and asked for no ceiling; unset is 60
	// minutes, because the silent case has to be the bounded one — an unbounded background process is the
	// orphan this epic is named after.
	DeadlineAt *time.Time
}

// BackgroundOutcome is what an observer learned about one task from the OPERATING SYSTEM plus the
// bounded tail of its output. ExitCode is a pointer because "not known" is a real answer with no
// numeric spelling — a control plane that restarted knows a process is gone and honestly cannot know
// what it returned, and a sentinel here would become a number a model compares against zero.
type BackgroundOutcome struct {
	// State is one of the terminal background_tasks states: exited, killed, expired, lost.
	State    string
	ExitCode *int
	// Tail is the last bytes of the task's output, already bounded by the observer. TailNote explains an
	// EMPTY tail — an unreadable file, a run with no allocation — so a model is told why it is missing
	// rather than being left to assume the build printed nothing.
	Tail     string
	TailNote string
}

// BackgroundObserver answers, for one task row, what the operating system says about its handle. It is
// a function rather than an interface because the coordinator must not learn what a process group or a
// container id is: the probe and the log read both belong to the control plane's sandbox adapters, and
// the transaction belongs here.
//
// done is false while the task is still running — the overwhelmingly common answer, and the one that
// costs a tick nothing. An error means the observer could not tell (a Docker socket that went away, a
// probe that failed): the row is skipped and the next tick asks again, because guessing `exited` for a
// build that is still compiling would notify a model that its work finished when it did not.
type BackgroundObserver func(ctx context.Context, task BackgroundTask) (outcome BackgroundOutcome, done bool, err error)

// SweepFinishedBackgroundTasks is E26 T4's pass of the reconciler loop: every task the database believes
// is running is probed, and every one that has finished produces EXACTLY ONE notification.
//
// It rides Reconciler.Sweep beside the other five for E24 T5's reason, quoted because it still holds:
// that loop already exists, already spans tenants, and is already supervised. A sixth loop would be a
// sixth thing to start, to supervise and to forget to start.
//
// A NIL OBSERVER MAKES IT A NO-OP, and that is the honest reading of a deployment with no background
// runner wired: PALAI_SANDBOX_IMAGE and PALAI_SHELL_NATIVE are both unset in the shipped compose file,
// so there is no shell tool at all there and no task can exist to sweep. The cost of the opt-out is one
// nil check per tick.
//
// Each row settles in its OWN transaction so one wedged run cannot hold the whole sweep, and a per-row
// failure ends the pass rather than being swallowed — the reconciler treats a sweep error as non-fatal
// and the next tick retries. Returns the number of tasks this pass SETTLED — which is not the same as
// the number of models told, because an orphaned notice settles a task and tells nobody.
func (s *Store) SweepFinishedBackgroundTasks(ctx context.Context, observe BackgroundObserver) (int, error) {
	if observe == nil {
		return 0, nil
	}
	tasks, err := s.runningBackgroundTasks(ctx)
	if err != nil {
		return 0, err
	}
	settled := 0
	for _, task := range tasks {
		outcome, done, oerr := observe(ctx, task)
		if oerr != nil || !done {
			// Still running, or unreadable. Neither is an error for the pass: the next tick asks again,
			// and a task that is genuinely stuck is T5's deadline to end, not this sweep's to guess at.
			continue
		}
		notified, serr := s.settleBackgroundTask(ctx, task, outcome)
		if serr != nil {
			return settled, serr
		}
		if notified {
			settled++
		}
	}
	return settled, nil
}

// runningBackgroundTasks reads every still-running task across every tenant. System-scoped like the
// other reconciler reads: the sweep is a cross-tenant safety net and a tenant-scoped read would see one
// tenant's tasks per pass.
func (s *Store) runningBackgroundTasks(ctx context.Context) ([]BackgroundTask, error) {
	rows, err := s.pool.Query(storage.WithSystemScope(ctx), storage.Query("RunningBackgroundTasks"))
	if err != nil {
		return nil, fmt.Errorf("select running background tasks: %w", err)
	}
	defer rows.Close()
	var out []BackgroundTask
	for rows.Next() {
		var t BackgroundTask
		if err := rows.Scan(&t.ID, &t.Tenant.Organization, &t.Tenant.Project, &t.RunID, &t.SessionID,
			&t.ResponseID, &t.Posture, &t.Handle, &t.MachineID, &t.OutputPath, &t.EnvKeys, &t.DeadlineAt,
			&t.AllocationRoot); err != nil {
			return nil, fmt.Errorf("scan running background task: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// settleBackgroundTask is the whole of exactly-once, in one transaction.
//
// THE CLAIM COMES FIRST AND IT IS THE SINGLE WINNER. ClaimBackgroundNotice sets notified_at where it is
// still NULL; two ticks, two control planes and a crash-restart all run this statement and exactly one
// of them takes the row. Everything after it — the command, the journal, the wake — happens inside the
// same transaction as that claim, so a crash before commit leaves notified_at NULL and the next tick
// does the whole thing rather than half of it.
//
// THE RUN'S STATE THEN DECIDES WHERE THE NOTIFICATION GOES, and there are exactly three answers:
//
//	terminal  -> no command; the run has no next turn, so a queued one would sit until the terminal
//	             sweep expired it unread. A warning.raised.v1 puts it where an operator looks.
//	waiting   -> the command AND the wake: the run parked on this task (T3) and re-enters now.
//	otherwise -> the command ALONE. The model is mid-turn; the boundary pump folds the notice in at the
//	             next safe boundary, exactly as a user's steered message folds in. Interrupting a run
//	             that never stopped would be a second attempt of one run.
//
// The wake is wakeParkedRunTx and is called for both non-terminal cases, because its own first act is
// to check whether the run is waiting: the condition lives in the wake, once, rather than being
// re-derived by each of its four callers.
func (s *Store) settleBackgroundTask(ctx context.Context, task BackgroundTask, outcome BackgroundOutcome) (bool, error) {
	ctx = storage.ScopeToTenant(ctx, task.Tenant.Organization, task.Tenant.Project)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, fmt.Errorf("begin background task settle: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	// THE RUN LOCK COMES FIRST, BEFORE THE CLAIM, and the order is the point rather than a preference.
	// Every path in this package takes the run lock first (guardRunActive), so a transaction that locks a
	// run and then writes a background_tasks row — which is exactly what T5's "cancelling a run kills its
	// background work" will be — cannot deadlock against this one. Taking the claim first and the run
	// second would have made those two orders opposite, and a deadlock found by a reaper in production is
	// found at three in the morning.
	//
	// The run id is read off the TASK row rather than the claim's RETURNING for the same reason: the lock
	// has to be acquirable before anything is written.
	var runState string
	if err := tx.QueryRow(ctx, storage.Query("LockRun"), task.RunID, task.Tenant.Project).
		Scan(new(string), new(*string), &runState); err != nil {
		return false, fmt.Errorf("lock run for background notice: %w", err)
	}

	var runID, sessionID, responseID, state, outputPath string
	var exitCode *int
	switch err := tx.QueryRow(ctx, storage.Query("ClaimBackgroundNotice"),
		task.ID, task.Tenant.Project, outcome.State, outcome.ExitCode).
		Scan(&runID, &sessionID, &responseID, &state, &exitCode, &outputPath); {
	case errors.Is(err, pgx.ErrNoRows):
		// Somebody else took this task's notification. That is the design working, not a failure.
		return false, nil
	case err != nil:
		return false, fmt.Errorf("claim background notice: %w", err)
	}

	notice := backgroundNoticeText(task.ID, state, exitCode, outputPath, outcome)
	if runTerminalStates[statemachines.RunState(runState)] {
		// NOWHERE TO LAND, AND NOT DROPPED. The row is stamped (so no tick retries it) and the journal
		// carries the fact, with the task id and the exit code an operator needs to go and look.
		payload := mustMarshal(map[string]any{
			"code":               backgroundOrphanedWarning,
			"background_task_id": task.ID,
			"run_id":             runID,
			"run_state":          runState,
			"exit_code":          exitCode,
			"output_path":        outputPath,
			"detail":             "this background task finished after its run reached a terminal state; there was no model turn left for the notification to fold into, so nothing was delivered. The output file is still on the allocation.",
		})
		if _, err := appendEvent(ctx, tx, task.Tenant, sessionID, responseID, warningRaisedEvent, payload); err != nil {
			return false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("commit orphaned background notice: %w", err)
		}
		return true, nil
	}

	commandID, err := newBackgroundCommandID()
	if err != nil {
		return false, err
	}
	// delivery is `queue`, the gentlest of the three send_message modes: an exit notification waits for
	// a safe boundary rather than interrupting a model mid-step. A steer or an interrupt would make a
	// finished build cut into a sentence.
	if _, err := tx.Exec(ctx, storage.Query("InsertCommand"),
		commandID, task.Tenant.Organization, task.Tenant.Project, sessionID, runID,
		backgroundNoticeKind, "queue", mustMarshal(map[string]any{"message": notice})); err != nil {
		return false, fmt.Errorf("enqueue background notice command: %w", err)
	}
	if _, err := appendEvent(ctx, tx, task.Tenant, sessionID, responseID, commandAcceptedEvent,
		mustMarshal(map[string]any{"command_id": commandID, "kind": backgroundNoticeKind, "delivery": "queue",
			"background_task_id": task.ID, "run_id": runID})); err != nil {
		return false, err
	}
	// THE WAKE, and only if the run is waiting — which is the wake's own first question. A running run
	// takes the no-op arm and keeps running; its next boundary delivers the command written above.
	if err := wakeParkedRunTx(ctx, tx, task.Tenant, runID); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit background notice: %w", err)
	}
	return true, nil
}

// backgroundNoticeText writes the turn the model reads. It quotes the ROW rather than the observation —
// the state and exit code come back from the claim — because a reaper that classified the task first
// knows something the probe cannot re-derive, and the model must be told what is recorded.
//
// Four things and no more: WHICH task, HOW it ended, WHERE the output is, and enough of that output to
// decide without reading it. The path is the argument palai.workspace.file takes, so the next step is
// one call the model can make from what it was handed.
func backgroundNoticeText(taskID, state string, exitCode *int, outputPath string, outcome BackgroundOutcome) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s background task %s %s", backgroundNoticePrefix, taskID, state)
	if exitCode != nil {
		fmt.Fprintf(&b, " with exit code %d", *exitCode)
	} else {
		// No sentinel, in prose either: "-1" would read as a failure and "0" as a success.
		b.WriteString(" with no exit code recorded (this control plane did not observe it exit)")
	}
	fmt.Fprintf(&b, ".\nOutput file (read it with palai.workspace.file): %s\n", outputPath)
	switch {
	case outcome.Tail != "":
		fmt.Fprintf(&b, "Last %d bytes of that file:\n%s", len(outcome.Tail), outcome.Tail)
	case outcome.TailNote != "":
		fmt.Fprintf(&b, "No output excerpt: %s", outcome.TailNote)
	default:
		b.WriteString("The output file is empty.")
	}
	return b.String()
}

// newBackgroundCommandID mints the id of a command the CONTROL PLANE authored. Every other command in
// this system carries a caller-supplied id whose uniqueness is the caller's idempotency; this one has
// no caller, so the id is minted here and the idempotency is the notified_at claim above.
func newBackgroundCommandID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate background command id: %w", err)
	}
	return "cmd_" + hex.EncodeToString(raw[:]), nil
}

// BackgroundTaskInput is one started process as the spawning attempt knows it. Every id on it is
// carried rather than derived, because the row must be readable by a control plane that never saw the
// attempt: the run that OWNS the process, the call that spawned it, and the fence that attempt held.
type BackgroundTaskInput struct {
	TaskID     string
	RunID      string
	SessionID  string
	ResponseID string
	ToolCallID string
	Fence      uint64
	Posture    string
	Handle     string
	// MachineID is the machine whose kernel Handle names (A.3 T6). It is written at spawn because it can
	// only be known then: nothing later can recover which machine ran a process from the row alone.
	MachineID  string
	OutputPath string
	// DeadlineAt is the ceiling the reaper will enforce, computed by the spawning attempt from the
	// deployment's PALAI_BACKGROUND_MAX_WALL_TIME. NIL is an explicit `0` and means unbounded.
	DeadlineAt *time.Time
	// EnvKeys is KEY NAMES, NEVER VALUES (migration 000047). The read path re-resolves them to redact
	// the log file before a byte reaches the model, which is T6's; a value here would put a credential
	// in a row a read route will one day return.
	EnvKeys []string
}

// RecordBackgroundTask writes the row a started process leaves behind. It is the durable half of
// E26 T2's in-memory registry: the map dies with the process that made it, and the whole point of a
// background task is to outlive that process.
//
// UNIQUE (tool_call_id) makes a doubled write an error rather than two rows, and that is the constraint
// doing its job: one call spawns one process, and two rows under one ledger row would mean one of the
// two processes is never reaped.
func (s *Store) RecordBackgroundTask(ctx context.Context, tenant Tenant, in BackgroundTaskInput) error {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	keys := in.EnvKeys
	if keys == nil {
		keys = []string{}
	}
	if _, err := s.pool.Exec(ctx, storage.Query("InsertBackgroundTask"),
		in.TaskID, tenant.Organization, tenant.Project, in.RunID, in.SessionID, in.ResponseID,
		in.ToolCallID, int64(in.Fence), in.Posture, in.Handle, in.MachineID, in.OutputPath, keys, in.DeadlineAt); err != nil {
		return fmt.Errorf("record background task: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------------------------------
// E26 T5 — the reaper's durable half: the ceilings, the adoption lookups and the settle a KILL performs.
// ---------------------------------------------------------------------------------------------------

// ErrNoBackgroundTask reports a task id that names no row of the asking run. It is a typed error because
// its two callers mean different things by it: the kill tool turns it into a refusal a model reads, and
// nothing else may treat it as "the task is gone".
var ErrNoBackgroundTask = errors.New("no background task with that id belongs to this run")

// BackgroundKiller ends one task's operating-system object and reports WHICH terminal state that leaves
// the row in — `killed` when the signal landed, `lost` when the handle could not be proven to be ours.
//
// It is a function for the reason BackgroundObserver is one: the coordinator must not learn what a
// process group or a container id is. It returns the STATE rather than an error to classify, because the
// vocabulary of background_tasks.state belongs to this package and the meaning of a signal belongs to the
// adapter — and translating an error into a state at this end would put the two halves of that decision
// in different packages.
//
// A NIL KILLER KILLS NOTHING, and that is the honest reading of a deployment with no background runner
// wired: it cannot have started a task, so it has none to end.
type BackgroundKiller func(ctx context.Context, task BackgroundTask) (state string, err error)

// SetBackgroundKiller injects the seam a run CANCELLATION reaches a live process through (E26 T5). A
// setter rather than a constructor parameter for the reason WithCapacityParkTTL is one: every existing
// caller compiles unchanged, and the honest default is off.
func (s *Store) SetBackgroundKiller(kill BackgroundKiller) { s.background = kill }

// CountRunningBackgroundTasks answers the two ceilings of §0.3 FROM THE DATABASE: how many tasks this run
// has running, and how many are running on this machine at all.
//
// It reads under WithSystemScope, which is what makes the second number true. A tenant-scoped count would
// give every tenant its own twenty and the machine as many as there are tenants; the per-run half is
// scoped in the predicate instead, so the widened session buys exactly the one fact it has to.
//
// THE CEILING IT SERVES IS ENFORCED AND NOT DECLARED, which is the whole point of the query existing:
// E24 opened `runners.capacity` with a CHECK constraint and no Go expression ever read it (§3.6 D12).
func (s *Store) CountRunningBackgroundTasks(ctx context.Context, tenant Tenant, runID string) (perRun, perHost int, err error) {
	if err := s.pool.QueryRow(storage.WithSystemScope(ctx), storage.Query("CountRunningBackgroundTasks"),
		runID, tenant.Project).Scan(&perRun, &perHost); err != nil {
		return 0, 0, fmt.Errorf("count running background tasks: %w", err)
	}
	return perRun, perHost, nil
}

// BackgroundTaskForRun resolves one task id to the handle that can stop it, refusing an id that belongs
// to another run.
//
// THIS IS ADOPTION. Before it, the mapping lived in a map built at spawn time, so a control plane that
// restarted could not stop a build it had started — E24 T5's lesson, which this task applies verbatim: an
// in-memory flag is erased by a restart and a column is not.
func (s *Store) BackgroundTaskForRun(ctx context.Context, tenant Tenant, runID, taskID string) (BackgroundTask, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	var t BackgroundTask
	var state string
	err := s.pool.QueryRow(ctx, storage.Query("BackgroundTaskForRun"), taskID, runID, tenant.Project).
		Scan(&t.ID, &t.Posture, &t.Handle, &t.OutputPath, &state)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return BackgroundTask{}, ErrNoBackgroundTask
	case err != nil:
		return BackgroundTask{}, fmt.Errorf("read background task: %w", err)
	}
	t.Tenant, t.RunID = tenant, runID
	return t, nil
}

// RunningBackgroundTasksOfRun lists one run's live tasks. It answers the park gate's question ("is
// anything of this run still going") and the cancellation's ("what must I end"), both of which used to be
// answered by a map that a restart emptied — wrongly in two opposite directions: a run that should have
// parked did not, and a cancellation killed nothing.
func (s *Store) RunningBackgroundTasksOfRun(ctx context.Context, tenant Tenant, runID string) ([]BackgroundTask, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	rows, err := s.pool.Query(ctx, storage.Query("RunningBackgroundTasksOfRun"), runID, tenant.Project)
	if err != nil {
		return nil, fmt.Errorf("select running background tasks of run: %w", err)
	}
	defer rows.Close()
	var out []BackgroundTask
	for rows.Next() {
		t := BackgroundTask{Tenant: tenant, RunID: runID}
		if err := rows.Scan(&t.ID, &t.Posture, &t.Handle, &t.OutputPath); err != nil {
			return nil, fmt.Errorf("scan running background task of run: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SettleEndedBackgroundTask records a task THIS CONTROL PLANE ENDED. It is the counterpart of the sweep's
// claim: that one settles a task we OBSERVED finishing and tells the model; this one settles a task
// somebody deliberately ended and tells nobody, because whoever ended it already knows.
//
// It takes the run lock FIRST, before touching background_tasks, and the order is the point rather than a
// preference — it is the same order settleBackgroundTask takes, settled before the second writer existed
// rather than after two loops touched the same rows in opposite orders.
func (s *Store) SettleEndedBackgroundTask(ctx context.Context, tenant Tenant, runID, taskID, state string) error {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin background task settle: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := tx.QueryRow(ctx, storage.Query("LockRun"), runID, tenant.Project).
		Scan(new(string), new(*string), new(string)); err != nil {
		return fmt.Errorf("lock run for background settle: %w", err)
	}
	if _, err := tx.Exec(ctx, storage.Query("SettleEndedBackgroundTask"), taskID, tenant.Project, state); err != nil {
		return fmt.Errorf("settle ended background task: %w", err)
	}
	return tx.Commit(ctx)
}

// killRunBackgroundTasks ends every live task of a run and records what that left. It is the answer to
// §3.6 D10 — "a run cancellation stops running work", which was measured FALSE: CancelRunReconciled drove
// the run canceled, cancelled its children and finalized the response, and signalled no process anywhere.
// A backgrounded build survived the cancellation of the run that owned it.
//
// THE KILL HAPPENS OUTSIDE ANY TRANSACTION, deliberately. Signalling a process group or asking a daemon
// to remove a container is an operating-system round trip, and holding a row lock across one would make a
// wedged Docker socket into a stuck run lock. The rows are settled afterwards, each under the run lock,
// in the order every other writer in this package takes it.
//
// A TASK THAT CANNOT BE KILLED IS LEFT `running` AND THE CANCEL STILL SUCCEEDS. The reaper will meet it
// again on the next tick, and a cancellation that failed because a Docker socket blinked would leave the
// run itself un-cancelled, which is strictly worse than one process outliving its run for thirty seconds.
func (s *Store) killRunBackgroundTasks(ctx context.Context, tenant Tenant, runID string) error {
	if s.background == nil {
		return nil
	}
	tasks, err := s.RunningBackgroundTasksOfRun(ctx, tenant, runID)
	if err != nil {
		return err
	}
	for _, task := range tasks {
		state, kerr := s.background(ctx, task)
		if kerr != nil && state == "" {
			log.Printf("cancelling run %s: background task %s could not be ended (%v); the reaper will meet it again", runID, task.ID, kerr)
			continue
		}
		if err := s.SettleEndedBackgroundTask(ctx, tenant, runID, task.ID, state); err != nil {
			return err
		}
	}
	return nil
}

// BackgroundLogRetention is the log sweep's two inputs in one round trip: the allocation roots that may
// hold a background log, and the ids of the tasks still writing to one.
//
// THE ROOTS COME FROM THE ALLOCATIONS RATHER THAN FROM THE TASK HISTORY, and that is what makes the sweep
// stateless. There is no column marking a log as deleted — E26's one migration is T1's and this task owns
// none — so a sweep driven by rows would re-examine every task that ever ran, forever. Driven by
// directories it is self-limiting: a file it deletes is never listed again.
func (s *Store) BackgroundLogRetention(ctx context.Context) (roots []string, live map[string]bool, err error) {
	sys := storage.WithSystemScope(ctx)
	rows, err := s.pool.Query(sys, storage.Query("BackgroundLogRoots"))
	if err != nil {
		return nil, nil, fmt.Errorf("select background log roots: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var root string
		if err := rows.Scan(&root); err != nil {
			return nil, nil, fmt.Errorf("scan background log root: %w", err)
		}
		roots = append(roots, root)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	ids, err := s.pool.Query(sys, storage.Query("RunningBackgroundTaskIDs"))
	if err != nil {
		return nil, nil, fmt.Errorf("select running background task ids: %w", err)
	}
	defer ids.Close()
	live = map[string]bool{}
	for ids.Next() {
		var id string
		if err := ids.Scan(&id); err != nil {
			return nil, nil, fmt.Errorf("scan running background task id: %w", err)
		}
		live[id] = true
	}
	return roots, live, ids.Err()
}
