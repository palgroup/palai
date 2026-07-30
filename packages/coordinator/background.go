package coordinator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

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
// AND THIS EXCERPT IS NOT REDACTED, WHICH IS SAID HERE RATHER THAN LEFT TO BE FOUND. T2 recorded that a
// background task's log file is not redacted on the way out (§3.6 D8): a process writing its own file
// bypasses both RedactSecrets and RedactValues, which act on a CAPTURED Go string, and
// palai.workspace.file has never redacted anything it reads. The tail below crosses the same boundary
// those bytes already cross — so a model learns nothing here it could not read from the file — but it
// does so into a DURABLE ROW: commands.payload, and then delivered_messages when the boundary pump
// applies it. THAT IS AN ASYMMETRY WITH THE SYNCHRONOUS PATH AND IT IS NEW: a synchronous shell result
// is redacted before it reaches tool_calls.result, and this one is not.
//
// It is T6's to close and T6 must close BOTH landing sites — the notice composed here and the file read
// — from the same place: background_tasks.env_keys holds the key NAMES precisely so a read path can
// re-resolve the values and mask them, and coordinator.BackgroundTask carries them to this file's
// caller. Until then an operator running background tasks that carry credentials should read
// docs/operations/background-execution.md's ceiling section.
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
	ID             string
	Tenant         Tenant
	RunID          string
	SessionID      string
	ResponseID     string
	Posture        string
	Handle         string
	OutputPath     string
	EnvKeys        []string
	AllocationRoot string
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
			&t.ResponseID, &t.Posture, &t.Handle, &t.OutputPath, &t.EnvKeys, &t.AllocationRoot); err != nil {
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
	if err := tx.QueryRow(ctx, storage.Query("LockRun"), task.RunID, task.Tenant.Organization, task.Tenant.Project).
		Scan(new(string), new(*string), &runState); err != nil {
		return false, fmt.Errorf("lock run for background notice: %w", err)
	}

	var runID, sessionID, responseID, state, outputPath string
	var exitCode *int
	switch err := tx.QueryRow(ctx, storage.Query("ClaimBackgroundNotice"),
		task.ID, task.Tenant.Organization, task.Tenant.Project, outcome.State, outcome.ExitCode).
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
	OutputPath string
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
		in.ToolCallID, int64(in.Fence), in.Posture, in.Handle, in.OutputPath, keys, nil); err != nil {
		return fmt.Errorf("record background task: %w", err)
	}
	return nil
}
