package coordinator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"

	statemachines "github.com/palgroup/palai/packages/state-machines"
	"github.com/palgroup/palai/storage"
)

// This file is the GENERIC approval spine (E23 T1, spec §22.4, §26.7). publication.go gates one thing —
// a push or a pull request — and it does that through a publications row that carries its own lifecycle.
// This gates ANY tool call, and it needs no new lifecycle at all: §26.7's tool-call machine has carried
// `approval_pending → (approve) → ready` since the spec was written, `tool_calls.state` is TEXT with no
// CHECK, and `tool_calls.request_hash` is already toolbroker.RequestHash(name, args) — which is exactly
// what an Approve button must be bound to. Two thirds of a generic approval were sitting in the schema
// with no caller. 000044's R1 is the third: an approvals row that can point at a call instead of a
// publication.
//
// THE WAKE LIVES IN ONE PLACE, wakeParkedRunTx, and every path through this file ends there: an approve,
// a deny, and the reaper. That is not tidiness — a gate whose closing and opening are written twice is a
// gate that one day only closes. The worst failure mode of an approval is not "it let something through";
// it is a run parked forever on a question nobody will ever answer.

// defaultApprovalTTL is how long a question stays live with nobody answering it. Thirty minutes is a
// judgement, and the reasoning is worth the line: it has to outlast a meeting and a lunch, because the
// cost of expiring too early is a human who clicks Approve and is told the button is dead; and it has to
// be short enough that a forgotten question releases its run the same working hour, because the cost of
// expiring too late is a run holding a session slot for a decision nobody will make. Overridable with
// PALAI_APPROVAL_TTL (any time.ParseDuration value) for deployments whose approvers are asleep in
// another timezone.
const defaultApprovalTTL = 30 * time.Minute

// ApprovalTTL resolves the deadline one approval gets — a gated tool call's (E23 T1) and a publication's
// (E23 T3) alike, from ONE definition. A malformed override falls back to the default rather than failing
// the run: an unparseable env var must not be the reason a tool call cannot be asked about, and the
// fallback is the safe direction (a deadline still exists).
func ApprovalTTL() time.Duration {
	raw := os.Getenv("PALAI_APPROVAL_TTL")
	if raw == "" {
		return defaultApprovalTTL
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return defaultApprovalTTL
	}
	return d
}

// ToolApprovalRequest opens the gate on one call. The arguments and hash are the ones the executor will
// run, captured at park time, so the button is bound to the bytes and not to an intention.
type ToolApprovalRequest struct {
	ApprovalID  string
	ToolCallID  string
	SessionID   string
	ResponseID  string
	RunID       string
	Fence       uint64
	ToolName    string
	Arguments   []byte
	ReplayClass string
	RequestHash string
	// ExpiresAt is the minutes-scale deadline (spec §22.4). Zero means no deadline — which this epic
	// never writes, because a gate with no deadline is the failure mode the reaper exists to prevent.
	ExpiresAt time.Time
	// Boundary is the model_request_id that produced the call, carried onto the ledger row exactly as the
	// pre-write carries it, so a late reconcile knows which boundary the call belongs to.
	Boundary string
}

// ToolApproval is the durable projection of a gated call: the ledger row's own facts joined to the
// binding. Every surface derives the approval SCREEN from this — nothing stores a rendered screen, so
// this read is where one comes from, and two derivations of one row cannot disagree.
type ToolApproval struct {
	ApprovalID  string
	ToolCallID  string
	RunID       string
	SessionID   string
	ResponseID  string
	ToolName    string
	Arguments   []byte
	RequestHash string
	// State is the tool_calls state: approval_pending while a human owes an answer, then ready (approved)
	// or canceled (denied, expired, or refused by a hook after the fact).
	State     string
	ExpiresAt *time.Time
	DecidedBy string
}

// ToolApprovalDecision is one principal's answer. RequestHash is the one-shot binding carried by the
// button: a mismatch authorizes nothing, because the call it was minted for is not the call now queued.
// DecidedBy is the canonical principal (T2 maps each surface's identity onto it).
type ToolApprovalDecision struct {
	ToolCallID  string
	RequestHash string
	DecidedBy   string
	Approve     bool
	// Reason rides a deny back to the model verbatim. A denial with no reason is a wall; a denial with one
	// is an instruction the agent can act on, which is the entire difference between a gate and an outage.
	Reason string
}

// RequestToolApproval parks one call: it records the ledger row at `approval_pending` with the exact
// arguments and hash, opens the binding with its deadline, and journals approval.requested.v1 +
// tool_call.approval_pending.v1 — all in ONE transaction, so "a human owes an answer" and "the call that
// is waiting for it" can never exist separately. Idempotent: a re-driven attempt that already parked this
// call re-reads its own row and asks nobody a second time.
//
// It runs under guardRunActive, so a canceled run never opens a question that will outlive it.
func (s *Store) RequestToolApproval(ctx context.Context, tenant Tenant, in ToolApprovalRequest) error {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin request tool approval: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if err := guardRunActive(ctx, tx, tenant, in.RunID); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, storage.Query("BeginToolCallApproval"),
		in.ToolCallID, tenant.Organization, tenant.Project, in.RunID, int64(in.Fence),
		in.ToolName, in.Arguments, in.ReplayClass, in.RequestHash, in.Boundary)
	if err != nil {
		return fmt.Errorf("park tool call for approval: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return tx.Commit(ctx) // already parked by an earlier attempt; the first park is authoritative
	}
	var expires any
	if !in.ExpiresAt.IsZero() {
		expires = in.ExpiresAt
	}
	if _, err := tx.Exec(ctx, storage.Query("InsertToolApproval"),
		in.ApprovalID, in.ToolCallID, tenant.Organization, tenant.Project, in.RequestHash, "", expires); err != nil {
		return fmt.Errorf("open tool approval: %w", err)
	}
	// THE ORDER TO ASK (E23 T8), committed in the SAME transaction as the approval that parks the run —
	// RequestPublication's line verbatim, and the SAME statement against the SAME table, because there is
	// one queue. What tells the pump which SCREEN to post is not this call: it is the approvals row this
	// row points at, whose 000044 CHECK ((publication_id IS NULL) <> (tool_call_id IS NULL)) already says
	// which kind of thing is being approved. So the discriminator rides the enqueue for free and no column,
	// no payload field and no second table was needed to carry it.
	//
	// UNTIL THIS TASK NOTHING WROTE THIS ROW FOR A TOOL CALL, and that was the whole of HIL-P8: T1 parked
	// the run, T4 built the screen, T5 advertised the write tools, and the question was never asked — so
	// every gated non-publication call was released half an hour later by the expiry reaper, having
	// interrupted nobody. Writing the order HERE rather than from the poster is the loss-lessness claim:
	// "a human owes an answer" and "the question is recorded for delivery" become durable together.
	//
	// A session with no Slack thread inserts ZERO rows, which is every non-Slack run in the deployment.
	if _, err := tx.Exec(ctx, storage.Query("EnqueueApprovalMessage"),
		tenant.Organization, tenant.Project, in.ApprovalID, in.RunID, in.ResponseID, in.SessionID); err != nil {
		return fmt.Errorf("enqueue tool approval message: %w", err)
	}
	// The genesis event carries the BINDING, never a rendered screen: an event stream that repeated the
	// display would be a second copy of it, and a second copy is the drift this epic refuses.
	payload := mustMarshal(map[string]any{
		"tool_call_id": in.ToolCallID, "approval_id": in.ApprovalID, "tool_name": in.ToolName,
		"request_hash": in.RequestHash, "run_id": in.RunID,
	})
	if _, err := appendEvent(ctx, tx, tenant, in.SessionID, in.ResponseID, approvalRequestedEvent, payload); err != nil {
		return err
	}
	if _, err := appendEvent(ctx, tx, tenant, in.SessionID, in.ResponseID, "tool_call.approval_pending.v1",
		mustMarshal(map[string]any{"run_id": in.RunID, "tool_call_id": in.ToolCallID})); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit request tool approval: %w", err)
	}
	return nil
}

// ToolApprovalForCall reads a gated call's projection. found is false for an unknown or foreign call, so
// a cross-tenant id leaks nothing.
func (s *Store) ToolApprovalForCall(ctx context.Context, tenant Tenant, toolCallID string) (ToolApproval, bool, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	var (
		a    ToolApproval
		args string
	)
	switch err := s.pool.QueryRow(ctx, storage.Query("ToolApprovalForCall"), toolCallID, tenant.Organization, tenant.Project).
		Scan(&a.ApprovalID, &a.ToolCallID, &a.RunID, &a.SessionID, &a.ResponseID, &a.ToolName, &args,
			&a.RequestHash, &a.State, &a.ExpiresAt, &a.DecidedBy); {
	case errors.Is(err, pgx.ErrNoRows):
		return ToolApproval{}, false, nil
	case err != nil:
		return ToolApproval{}, false, fmt.Errorf("read tool approval: %w", err)
	}
	a.Arguments = []byte(args)
	return a, true, nil
}

// DecideToolApproval applies one human's answer and WAKES the parked run, in one transaction. It is the
// single throat every generic surface passes through — Slack's click (E23 T8's SlackAdmitter.Decide, its
// first production caller) and any later command path — which is where the approver check goes for the
// reason a control placed in each caller is a control the next caller forgets.
//
// applied reports whether the decision actually moved the call. FALSE with NO ERROR is returned for every
// way a decision can be VOID: the call was already decided, the deadline passed, or the one-shot hash does
// not match. That shape is deliberate. The publication path returns an ERROR when a decision cannot land,
// and the measured consequence was a human watching Slack report a 503 while the operation stayed pending
// forever (E22's own test recorded it). A void decision is not a server fault; the surface should be able
// to say "that button is no longer live" and mean it.
//
// THE ONE EXCEPTION IS ErrApproverNotAuthorized (E23 T8), and it is a typed OUTCOME rather than a failure —
// the publication path's exact convention, documented at ErrApproverNotAuthorized itself. A caller must
// answer 200 and draw nothing. It is distinguished from the void cases because "you may not decide this"
// and "this is no longer decidable" are different facts for the operator reading the log, and collapsing
// them would hide a misconfigured approver list behind what looks like an expired button.
func (s *Store) DecideToolApproval(ctx context.Context, tenant Tenant, d ToolApprovalDecision) (bool, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, fmt.Errorf("begin decide tool approval: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	// WHO IS DECIDING (E23 T2), checked FIRST — before the row is even locked, and the ordering is
	// ApplyApprovalDecision's own reasoning applied to this path. An unauthorized principal must cause NO
	// state transition at all (not the approve, not the deny, and not the expiry one below), and every
	// unauthorized caller must get the same answer whatever hash or call id they present, or the refusal
	// becomes an oracle for which ids are real. It is the SAME function the publication path calls, so a
	// project's approver list cannot mean one thing for a push and another for a Jira transition.
	allowed, err := approverAuthorizedTx(ctx, tx, tenant, d.DecidedBy)
	if err != nil {
		return false, err
	}
	if !allowed {
		return false, ErrApproverNotAuthorized
	}

	var state, runID, name, storedHash string
	switch err := tx.QueryRow(ctx, storage.Query("LockToolCallForDecision"), d.ToolCallID, tenant.Organization, tenant.Project).
		Scan(&state, &runID, &name, new(string), &storedHash); {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil // no such call in this tenant — decides nothing, leaks nothing
	case err != nil:
		return false, fmt.Errorf("lock gated tool call: %w", err)
	}
	if state != "approval_pending" {
		return false, nil // already approved, denied, expired, or never gated
	}

	var expiresAt *time.Time
	var approvalID string
	if err := tx.QueryRow(ctx, storage.Query("ToolApprovalForCall"), d.ToolCallID, tenant.Organization, tenant.Project).
		Scan(&approvalID, new(string), new(string), new(string), new(string), new(string), new(string),
			new(string), new(string), &expiresAt, new(string)); err != nil {
		return false, fmt.Errorf("read tool approval for decision: %w", err)
	}

	sessionID, responseID, err := lockRunForWake(ctx, tx, tenant, runID)
	if err != nil {
		return false, err
	}

	// The deadline is checked BEFORE the hash, exactly as the publication path checks it: an expired
	// approval authorizes nothing regardless of how good the token is. Expiring here also WAKES the run —
	// a click that arrives one second late must not be the reason a run waits forever.
	if expiresAt != nil && expiresAt.Before(time.Now()) {
		if err := expireToolCallTx(ctx, tx, tenant, sessionID, responseID, runID, d.ToolCallID); err != nil {
			return false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("commit expired-at-decision: %w", err)
		}
		return false, nil
	}
	// The one-shot binding (REP-009): the button carries the hash of the exact (name, arguments) it was
	// minted for. Different bytes are a different call, and a click on the old button authorizes the old
	// call — which no longer exists.
	if d.RequestHash != "" && d.RequestHash != storedHash {
		return false, nil
	}

	if d.Approve {
		if _, err := tx.Exec(ctx, storage.Query("ApproveToolCall"), d.ToolCallID, tenant.Organization, tenant.Project); err != nil {
			return false, fmt.Errorf("approve tool call: %w", err)
		}
		if _, err := appendEvent(ctx, tx, tenant, sessionID, responseID, "tool_call.ready.v1",
			mustMarshal(map[string]any{"run_id": runID, "tool_call_id": d.ToolCallID})); err != nil {
			return false, err
		}
		if _, err := appendEvent(ctx, tx, tenant, sessionID, responseID, approvalApprovedEvent,
			mustMarshal(map[string]any{"tool_call_id": d.ToolCallID, "approval_id": approvalID, "tool_name": name})); err != nil {
			return false, err
		}
	} else {
		// A DENY IS AN ANSWER, not silence: the model is handed the same {"status":"denied","reason":…}
		// shape a before_tool hook deny already produces, so it continues on a fact rather than stalling on
		// an absence. The answer is written to the ledger row here — durable before it is spoken — so a
		// kill between the decision and its delivery cannot lose it.
		if _, err := tx.Exec(ctx, storage.Query("CancelToolCall"), d.ToolCallID, tenant.Organization, tenant.Project,
			mustMarshal(map[string]any{"status": "denied", "reason": d.Reason, "decided_by": d.DecidedBy})); err != nil {
			return false, fmt.Errorf("deny tool call: %w", err)
		}
		if _, err := appendEvent(ctx, tx, tenant, sessionID, responseID, "tool_call.canceled.v1",
			mustMarshal(map[string]any{"run_id": runID, "tool_call_id": d.ToolCallID})); err != nil {
			return false, err
		}
		if _, err := appendEvent(ctx, tx, tenant, sessionID, responseID, approvalDeniedEvent,
			mustMarshal(map[string]any{"tool_call_id": d.ToolCallID, "approval_id": approvalID, "tool_name": name})); err != nil {
			return false, err
		}
	}
	if _, err := tx.Exec(ctx, storage.Query("SetToolApprovalDecision"), d.ToolCallID, d.DecidedBy); err != nil {
		return false, fmt.Errorf("record tool approval decision: %w", err)
	}
	if err := wakeParkedRunTx(ctx, tx, tenant, runID); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit tool approval decision: %w", err)
	}
	return true, nil
}

// SweepExpiredToolApprovals is the SECOND HALF OF EXPIRY, and until this epic the tree had only the
// first. Publications already expired on a deadline; nothing woke the thing that was waiting, because
// nothing waited. Now a run parks, so an unanswered question is a run that stops — and a gate whose worst
// behaviour is keeping shut forever what it closed is worse than no gate at all.
//
// One reconcile pass, tenant-spanning by construction, journaling approval.expired.v1 (an EXISTING event
// type — 000013 forward-declared it and E10 T7 enforced it for publications) per row. Each row settles in
// its own transaction so one wedged run cannot hold the whole sweep. Returns the number expired.
func (s *Store) SweepExpiredToolApprovals(ctx context.Context) (int, error) {
	rows, err := s.pool.Query(storage.WithSystemScope(ctx), storage.Query("SelectExpiredToolApprovals"))
	if err != nil {
		return 0, fmt.Errorf("select expired tool approvals: %w", err)
	}
	type candidate struct {
		callID, runID, sessionID, responseID string
		tenant                               Tenant
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.callID, &c.tenant.Organization, &c.tenant.Project, &c.runID, &c.sessionID, &c.responseID); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan expired tool approval: %w", err)
		}
		candidates = append(candidates, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	swept := 0
	for _, c := range candidates {
		expired, err := s.expireOneToolApproval(ctx, c.tenant, c.sessionID, c.responseID, c.runID, c.callID)
		if err != nil {
			return swept, err
		}
		if expired {
			swept++
		}
	}
	return swept, nil
}

// expireOneToolApproval cancels one elapsed call and wakes its run, single-winner. A row a racing click
// already decided updates nothing and is not counted — the sweep reports what it MOVED, not what it read.
func (s *Store) expireOneToolApproval(ctx context.Context, tenant Tenant, sessionID, responseID, runID, callID string) (bool, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, fmt.Errorf("begin expire tool approval: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var state string
	switch err := tx.QueryRow(ctx, storage.Query("LockToolCallForDecision"), callID, tenant.Organization, tenant.Project).
		Scan(&state, new(string), new(string), new(string), new(string)); {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("lock elapsed tool call: %w", err)
	}
	if state != "approval_pending" {
		return false, nil
	}
	if _, _, err := lockRunForWake(ctx, tx, tenant, runID); err != nil {
		return false, err
	}
	if err := expireToolCallTx(ctx, tx, tenant, sessionID, responseID, runID, callID); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit expire tool approval: %w", err)
	}
	return true, nil
}

// expireToolCallTx is the shared expiry body: cancel the call with an answer the model can act on,
// journal approval.expired.v1 + tool_call.canceled.v1, and WAKE the parked run. The consume-time guard
// (a click that arrived too late) and the idle sweep both route here, so an expiry looks identical in the
// journal no matter which path observed the elapsed deadline.
func expireToolCallTx(ctx context.Context, tx pgx.Tx, tenant Tenant, sessionID, responseID, runID, callID string) error {
	if _, err := tx.Exec(ctx, storage.Query("CancelToolCall"), callID, tenant.Organization, tenant.Project,
		mustMarshal(map[string]any{
			"status": "denied",
			"reason": "this call's approval expired before anyone decided; nothing ran. Ask again if it is still wanted.",
		})); err != nil {
		return fmt.Errorf("cancel elapsed tool call: %w", err)
	}
	if _, err := appendEvent(ctx, tx, tenant, sessionID, responseID, approvalExpiredEvent,
		mustMarshal(map[string]any{"tool_call_id": callID, "run_id": runID})); err != nil {
		return err
	}
	if _, err := appendEvent(ctx, tx, tenant, sessionID, responseID, "tool_call.canceled.v1",
		mustMarshal(map[string]any{"run_id": runID, "tool_call_id": callID})); err != nil {
		return err
	}
	return wakeParkedRunTx(ctx, tx, tenant, runID)
}

// lockRunForWake locks the run and returns its session/response scope for journaling. Taking the run lock
// BEFORE the decision writes keeps the lock order identical to every other path in this package
// (guardRunActive locks the run first), so a decision and a terminal cannot deadlock each other.
func lockRunForWake(ctx context.Context, tx pgx.Tx, tenant Tenant, runID string) (string, string, error) {
	var sessionID, state string
	var responseID *string
	if err := tx.QueryRow(ctx, storage.Query("LockRun"), runID, tenant.Organization, tenant.Project).
		Scan(&sessionID, &responseID, &state); err != nil {
		return "", "", fmt.Errorf("lock parked run: %w", err)
	}
	resolved := ""
	if responseID != nil {
		resolved = *responseID
	}
	return sessionID, resolved, nil
}

// wakeParkedRunTx is THE wake — the one function an approve, a deny and the reaper all end in. It drives
// a WAITING run back to running and enqueues its response.run job in the SAME transaction as the decision
// that justified it, so the job becomes claimable only once the decision is durable: nothing dispatches
// before commit. The fresh attempt replays the committed transcript, reaches the SAME tool.request, and
// the ledger consult now finds `ready` or `canceled` instead of `approval_pending`.
//
// A run that is not waiting is a no-op rather than an error, which is what makes the wake single-winner:
// a doubled click, or a click racing the reaper, re-enters the run exactly once and the loser sees a
// running run. It mirrors applyResumeTx's body and WakeDetachedParent's discipline; it is separate from
// both because a resume answers a user's pause and a detach wake counts children, and neither question
// has anything to do with an approval.
func wakeParkedRunTx(ctx context.Context, tx pgx.Tx, tenant Tenant, runID string) error {
	var sessionID, state string
	var responseID *string
	if err := tx.QueryRow(ctx, storage.Query("LockRun"), runID, tenant.Organization, tenant.Project).
		Scan(&sessionID, &responseID, &state); err != nil {
		return fmt.Errorf("lock run for approval wake: %w", err)
	}
	if statemachines.RunState(state) != statemachines.RunWaiting {
		// Not parked (the decision beat the park, or another waker already re-entered it). The park's own
		// re-consult covers the first case: it re-reads the ledger and finds the call already decided.
		// ponytail: a user PAUSE queued against a parked run would be overridden by this wake, the race
		// WakeDetachedParent guards with a pending-pause check. Not guarded here because nothing queues a
		// pause against an approval-parked run today; add the same check if a surface ever does.
		return nil
	}
	if _, err := applyRunTransitionTx(ctx, tx, tenant, runID, statemachines.RunCmdResume); err != nil {
		return err
	}
	jobID, err := newJobID()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, storage.Query("EnqueueJob"),
		jobID, tenant.Organization, tenant.Project, "response.run", []byte(fmt.Sprintf(`{"run_id":%q}`, runID))); err != nil {
		return fmt.Errorf("enqueue approval wake job: %w", err)
	}
	return nil
}

// PendingToolApprovalForSession reads the still-open gated call a session has, matched by the one-shot
// request hash a button carried. It is the read E23 T4's modal is built from, and it is a READ: the modal
// is opened inside Slack's three-second interactivity budget against a trigger_id that dies in the same
// three seconds, so anything on that path which took a lock or wrote a row would be trading a working
// button for a slower one.
//
// found is false for an unknown session, a hash that matches nothing open, or another tenant's row — so a
// hash observed in one workspace shows a human in another workspace nothing at all.
func (s *Store) PendingToolApprovalForSession(ctx context.Context, tenant Tenant, sessionID, requestHash string) (ToolApproval, bool, error) {
	if sessionID == "" || requestHash == "" {
		return ToolApproval{}, false, nil
	}
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	var (
		a    ToolApproval
		args string
	)
	switch err := s.pool.QueryRow(ctx, storage.Query("PendingToolApprovalForSession"),
		sessionID, requestHash, tenant.Organization, tenant.Project).
		Scan(&a.ApprovalID, &a.ToolCallID, &a.RunID, &a.SessionID, &a.ResponseID, &a.ToolName, &args,
			&a.RequestHash, &a.State, &a.ExpiresAt, &a.DecidedBy); {
	case errors.Is(err, pgx.ErrNoRows):
		return ToolApproval{}, false, nil
	case err != nil:
		return ToolApproval{}, false, fmt.Errorf("read the session's pending tool approval: %w", err)
	}
	a.Arguments = []byte(args)
	return a, true, nil
}
