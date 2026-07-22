package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	statemachines "github.com/palgroup/palai/packages/state-machines"
	"github.com/palgroup/palai/storage"
)

// RunIDForResponse resolves a response's root run within the tenant scope. found is false
// for an unknown or foreign response, so the caller renders the same 404 as retrieval and
// never leaks a cross-tenant resource's existence (spec §39.2). LP's response:run is 1:1.
func (s *Store) RunIDForResponse(ctx context.Context, tenant Tenant, responseID string) (string, bool, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	var runID string
	err := s.pool.QueryRow(ctx, storage.Query("RunIDForResponse"), responseID, tenant.Organization, tenant.Project).Scan(&runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("resolve run for response %s: %w", responseID, err)
	}
	return runID, true, nil
}

// guardRunActive locks the run and rejects a commit whose run has already reached a terminal
// state, returning ErrRunTerminal. It is the shared precondition of the commit-before-deliver
// primitives below: once a run is terminal (e.g. canceled mid-attempt), no result event may
// be appended after its terminal event, so the "terminal is the journal's end" contract holds
// even when a cancel races an in-flight attempt (spec §22.3 monotonic terminality). The FOR
// UPDATE lock serializes against ApplyRunTransition, so a cancel and a commit cannot both win.
func guardRunActive(ctx context.Context, tx pgx.Tx, tenant Tenant, runID string) error {
	var sessionID, state string
	var responseID *string
	err := tx.QueryRow(ctx, storage.Query("LockRun"), runID, tenant.Organization, tenant.Project).Scan(&sessionID, &responseID, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("run %s not found in tenant scope", runID)
	}
	if err != nil {
		return fmt.Errorf("lock run: %w", err)
	}
	if runTerminalStates[statemachines.RunState(state)] {
		return fmt.Errorf("%w: run %s is %s", ErrRunTerminal, runID, state)
	}
	return nil
}

// RunContext resolves a run's durable execution context for the orchestrator: its
// tenant scope, session, response, current run state, and admitted input. The run id
// comes from the claimed durable job, so this by-primary-key read is the same
// cross-tenant infrastructure read the job claim performs; it establishes the scope
// every later orchestrator write is gated by. The state lets the attempt bail before
// driving a run a pause already parked in waiting (a redelivered/reclaimed job).
func (s *Store) RunContext(ctx context.Context, runID string) (Tenant, string, string, string, []byte, error) {
	var (
		tenant     Tenant
		sessionID  string
		responseID string
		state      string
		input      []byte
	)
	err := s.pool.QueryRow(ctx, storage.Query("RunContext"), runID).
		Scan(&tenant.Organization, &tenant.Project, &sessionID, &responseID, &state, &input)
	if err != nil {
		return Tenant{}, "", "", "", nil, fmt.Errorf("read run context for %s: %w", runID, err)
	}
	return tenant, sessionID, responseID, state, input, nil
}

// RunDelegation reads a run's delegation context by primary key (spec §25.18): its depth and the
// delegation JSON the orchestrator seeds into run.start (a root run's {"emit":[...]}) or routes a
// child's own model/budget from ({"spec":{...}}). A plain run carries NULL delegation.
func (s *Store) RunDelegation(ctx context.Context, runID string) (int, []byte, error) {
	var depth int
	var delegation []byte
	if err := s.pool.QueryRow(ctx, storage.Query("RunDelegation"), runID).Scan(&depth, &delegation); err != nil {
		return 0, nil, fmt.Errorf("read run delegation for %s: %w", runID, err)
	}
	return depth, delegation, nil
}

// ChildRunInput is the durable creation of one ChildRun (spec §25.18-19): a runs row carrying
// parent_run_id/depth/delegation plus its own response, in the parent's session. The caller mints
// the ids and passes the objective as the child's run.start input.
type ChildRunInput struct {
	ParentRunID      string
	ParentResponseID string
	SessionID        string
	ChildRunID       string
	ChildResponseID  string
	Depth            int
	Input            []byte
	Delegation       []byte
	Store            bool
	// EnqueueRun enqueues the child's response.run job in the SAME transaction as the child row (E10 T8
	// DET-001, MF-3): a detached child that committed its row but not its job (a crash/transient error in
	// a separate enqueue) would have no worker and no waker — a permanently hung parent. Atomic ⇒ no orphan.
	EnqueueRun bool
}

// CreateChildRun creates a ChildRun's response and run and journals the child-requested event on
// the PARENT's response (so the parent journal carries the child lifecycle, not the child's own
// steps — §25.19). It guards the parent active, so a canceled parent spawns no child, and runs in
// one transaction: the row and its birth event are atomic. eventType/payload are the caller's
// child.requested.v1.
func (s *Store) CreateChildRun(ctx context.Context, tenant Tenant, in ChildRunInput, eventType string, payload []byte) error {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin child run: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if err := guardRunActive(ctx, tx, tenant, in.ParentRunID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, storage.Query("InsertResponse"),
		in.ChildResponseID, tenant.Organization, tenant.Project, in.SessionID, in.Input, in.Store); err != nil {
		return fmt.Errorf("insert child response: %w", err)
	}
	if _, err := tx.Exec(ctx, storage.Query("InsertChildRun"),
		in.ChildRunID, tenant.Organization, tenant.Project, in.SessionID, in.ChildResponseID, in.ParentRunID, in.Depth, in.Delegation); err != nil {
		return fmt.Errorf("insert child run: %w", err)
	}
	if _, err := appendEvent(ctx, tx, tenant, in.SessionID, in.ParentResponseID, eventType, payload); err != nil {
		return err
	}
	// A detached child's job commits WITH its row (MF-3): no window where the row exists jobless.
	if in.EnqueueRun {
		jobID, err := newJobID()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, storage.Query("EnqueueJob"),
			jobID, tenant.Organization, tenant.Project, "response.run", []byte(fmt.Sprintf(`{"run_id":%q}`, in.ChildRunID))); err != nil {
			return fmt.Errorf("enqueue detached child job: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit child run: %w", err)
	}
	return nil
}

// JournalChildEvent appends a child-lifecycle event (child.denied.v1 / child.completed.v1) to the
// PARENT's response journal (spec §25.19), guarded by the parent run being active so a canceled
// parent appends nothing after its terminal (monotonic terminality, §22.3). child.requested.v1 is
// journaled by CreateChildRun instead — atomically with the child row.
func (s *Store) JournalChildEvent(ctx context.Context, tenant Tenant, sessionID, parentResponseID, parentRunID, eventType string, payload []byte) error {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin child event: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if err := guardRunActive(ctx, tx, tenant, parentRunID); err != nil {
		return err
	}
	if _, err := appendEvent(ctx, tx, tenant, sessionID, parentResponseID, eventType, payload); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit child event: %w", err)
	}
	return nil
}

// JournalRunEvent appends a run-scoped diagnostic event (e.g. policy.denied.v1 for a hook deny, spec
// §28.17) to the run's response journal, guarded by the run being active so a canceled run appends nothing
// after its terminal (monotonic terminality, §22.3). It is the generic twin of JournalChildEvent — a hook
// deny is not child-specific — and returns ErrRunTerminal on a raced cancel, which the orchestrator maps to
// a clean attempt end.
func (s *Store) JournalRunEvent(ctx context.Context, tenant Tenant, sessionID, responseID, runID, eventType string, payload []byte) error {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin run event: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if err := guardRunActive(ctx, tx, tenant, runID); err != nil {
		return err
	}
	if _, err := appendEvent(ctx, tx, tenant, sessionID, responseID, eventType, payload); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit run event: %w", err)
	}
	return nil
}

// ChildRunOutcome reads a finished ChildRun's terminal run state and response projection so the
// parent folds its typed result (spec §25.19). Tenant-scoped by primary key.
func (s *Store) ChildRunOutcome(ctx context.Context, tenant Tenant, childRunID string) (string, []byte, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	var state string
	var output []byte
	err := s.pool.QueryRow(ctx, storage.Query("ChildRunOutcome"), childRunID, tenant.Organization, tenant.Project).Scan(&state, &output)
	if err != nil {
		return "", nil, fmt.Errorf("read child run outcome for %s: %w", childRunID, err)
	}
	return state, output, nil
}

// ChildRunRef identifies a non-terminal ChildRun for cancel propagation (spec §25.18, SUB-005).
type ChildRunRef struct {
	RunID      string
	ResponseID string
}

// NonTerminalChildRuns returns every non-terminal descendant of a run (recursively), so a parent
// cancel propagates to all its children (SUB-005). Tenant-scoped; a run with no live children
// yields no rows.
func (s *Store) NonTerminalChildRuns(ctx context.Context, tenant Tenant, parentRunID string) ([]ChildRunRef, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	rows, err := s.pool.Query(ctx, storage.Query("NonTerminalDescendantRuns"), parentRunID, tenant.Organization, tenant.Project)
	if err != nil {
		return nil, fmt.Errorf("read non-terminal child runs: %w", err)
	}
	defer rows.Close()
	var out []ChildRunRef
	for rows.Next() {
		var ref ChildRunRef
		if err := rows.Scan(&ref.RunID, &ref.ResponseID); err != nil {
			return nil, fmt.Errorf("scan child run ref: %w", err)
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// CancelChildren propagates a parent's cancel to all its non-terminal descendant ChildRuns
// (spec §25.18, SUB-005): each is driven to the canceled terminal (run transition + response
// projection), monotonically — a descendant a racing terminal already finished is skipped
// (ErrRunTerminal), so the count reflects only the runs this call canceled. canceledProjection is
// the caller's canonical canceled body, applied to every child so a GET reads the same terminal.
// A child's own in-flight attempt then loses its next commit to the run-terminal guard, and its
// response UPDATE is conditional, so a late child terminal cannot overwrite the canceled row.
func (s *Store) CancelChildren(ctx context.Context, tenant Tenant, parentRunID string, canceledProjection []byte) (int, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	children, err := s.NonTerminalChildRuns(ctx, tenant, parentRunID)
	if err != nil {
		return 0, err
	}
	canceled := 0
	for _, child := range children {
		switch _, err := s.ApplyRunTransition(ctx, tenant, child.RunID, statemachines.RunCmdCancel); {
		case errors.Is(err, ErrRunTerminal):
			continue // a racing terminal already finished this child; nothing to cancel
		case err != nil:
			return canceled, err
		}
		if err := s.FinalizeResponse(ctx, tenant, child.ResponseID, "canceled", canceledProjection); err != nil {
			return canceled, err
		}
		canceled++
	}
	return canceled, nil
}

// CancelRunReconciled cancels a run during/after a kill and reconciles its active external ops to a
// SINGLE monotonic terminal (spec §26.10 steps 8-9, SES-010, E10 T7): `canceled` when nothing is left
// uncertain, or `failed_with_uncertain_side_effect` when an irreversible/interactive tool_call is still
// uncertain (its effect may have landed; the run must NOT claim a clean cancel). It drives the run to
// canceled (monotonic — a run a racing terminal already finished is left alone), propagates the cancel to
// children (each to a single terminal, E08 SUB-005), and finalizes the response with the caller's
// projection — but ONLY if the run is genuinely canceled: a run that COMPLETED/failed concurrently keeps
// its own projection (no empty-canceled clobber of a completion's output). It returns the response's
// ACTUAL terminal, so a racing completion is reflected honestly. It is the single production cancel path
// (CancelResponse routes here).
func (s *Store) CancelRunReconciled(ctx context.Context, tenant Tenant, responseID, runID string, canceledProjection, uncertainProjection []byte) (string, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	uncertain, err := s.hasUncertainSideEffect(ctx, tenant, runID)
	if err != nil {
		return "", err
	}
	terminal, projection := "canceled", canceledProjection
	if uncertain {
		terminal, projection = "failed_with_uncertain_side_effect", uncertainProjection
	}
	// Drive the run canceled, monotonically. A run a racing terminal already finished is left alone
	// (ErrRunTerminal) — it may have COMPLETED concurrently, which the finalize step below must respect.
	switch _, err := s.ApplyRunTransition(ctx, tenant, runID, statemachines.RunCmdCancel); {
	case errors.Is(err, ErrRunTerminal), errors.Is(err, statemachines.ErrInvalidState):
	case err != nil:
		return "", err
	}
	// Propagate the cancel to descendants (E08 SUB-005): a child is canceled (not uncertain), so it gets
	// the canonical canceled projection. Runs regardless — a re-cancel reconciles a child a first missed.
	if _, err := s.CancelChildren(ctx, tenant, runID, canceledProjection); err != nil {
		return "", err
	}
	// Finalize the response as canceled/uncertain ONLY if the run is GENUINELY canceled. finalize.go
	// applies run→completed and then compiles the changeset BEFORE finalizing the response, so a cancel
	// landing in that gap sees run=completed but response still open; writing an empty canceled projection
	// then would clobber the completion's output (run/response divergence + lost output). Read the run's
	// actual terminal and skip the finalize when it completed/failed — leave the completion's projection.
	var runState string
	if err := s.pool.QueryRow(ctx, `SELECT state FROM runs WHERE id=$1 AND organization_id=$2 AND project_id=$3`,
		runID, tenant.Organization, tenant.Project).Scan(&runState); err != nil {
		return "", fmt.Errorf("read run state for cancel finalize: %w", err)
	}
	if runState == string(statemachines.RunCanceled) {
		// The conditional UpdateResponse still makes this a monotonic no-op if the response was already
		// finalized by another cancel path (the late-terminal DB guard).
		if err := s.FinalizeResponse(ctx, tenant, responseID, terminal, projection); err != nil {
			return "", err
		}
	}
	// Return the ACTUAL response terminal (whatever won), so a racing completion is reflected honestly.
	var actual string
	if err := s.pool.QueryRow(ctx, `SELECT state FROM responses WHERE id=$1 AND organization_id=$2 AND project_id=$3`,
		responseID, tenant.Organization, tenant.Project).Scan(&actual); err != nil {
		return "", fmt.Errorf("read terminal response state: %w", err)
	}
	return actual, nil
}

// hasUncertainSideEffect reports whether a run has an unresolved uncertain irreversible/interactive
// tool_call — the SES-010 terminal discriminator (spec §26.10).
func (s *Store) hasUncertainSideEffect(ctx context.Context, tenant Tenant, runID string) (bool, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	var exists bool
	if err := s.pool.QueryRow(ctx, storage.Query("HasUncertainSideEffect"), runID, tenant.Organization, tenant.Project).Scan(&exists); err != nil {
		return false, fmt.Errorf("check uncertain side effect: %w", err)
	}
	return exists, nil
}

// PriorResponse is one earlier response in a session, as run.start history needs it:
// Output is the stored terminal projection (nil once purged or not yet terminal), and
// Purged marks a reaped response whose content is a redacted_content marker (spec §22.2).
type PriorResponse struct {
	Output []byte
	Purged bool
}

// SessionHistory returns a session's responses created before responseID, in creation
// order, so run.start can carry them as conversation history (spec §9, §22.2). It is
// tenant-scoped; a foreign session or response yields no rows.
func (s *Store) SessionHistory(ctx context.Context, tenant Tenant, sessionID, responseID string) ([]PriorResponse, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	rows, err := s.pool.Query(ctx, storage.Query("SessionHistory"), sessionID, tenant.Organization, tenant.Project, responseID)
	if err != nil {
		return nil, fmt.Errorf("read session history: %w", err)
	}
	defer rows.Close()
	var out []PriorResponse
	for rows.Next() {
		var p PriorResponse
		if err := rows.Scan(&p.Output, &p.Purged); err != nil {
			return nil, fmt.Errorf("scan session history: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// appendEvent journals one event under a freshly allocated, gap-free session sequence
// inside tx. It is the shared body of the commit-before-deliver primitives below.
// responseID keys the event to its owning response so the retention purge is per-response
// (spec §22.2); an empty string journals a session-scoped event with a NULL response_id.
func appendEvent(ctx context.Context, tx pgx.Tx, tenant Tenant, sessionID, responseID, eventType string, payload []byte) (int64, error) {
	var seq int64
	if err := tx.QueryRow(ctx, storage.Query("AllocateSequence"), sessionID).Scan(&seq); err != nil {
		return 0, fmt.Errorf("allocate session sequence: %w", err)
	}
	eventID, err := newEventID()
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, storage.Query("AppendEvent"),
		eventID, tenant.Organization, tenant.Project, sessionID, nullableText(responseID), seq, eventType, payload); err != nil {
		return 0, fmt.Errorf("append event: %w", err)
	}
	return seq, nil
}

// CommitModelRequest records a model request and its journal event before the provider
// is called, so the request has a durable trace and a reclaimed attempt can dedup
// against a committed result (spec §24.7 order, §53.4). The row is idempotent; the
// event is journaled only on the fresh insert, so a re-derived request adds nothing.
func (s *Store) CommitModelRequest(ctx context.Context, tenant Tenant, sessionID, responseID, runID, requestID, eventType string, payload []byte) error {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin model request: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if err := guardRunActive(ctx, tx, tenant, runID); err != nil {
		return err
	}
	err = tx.QueryRow(ctx, storage.Query("InsertModelRequest"),
		requestID, tenant.Organization, tenant.Project, runID).Scan(new(string))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // already recorded by an earlier attempt; nothing new to journal
	}
	if err != nil {
		return fmt.Errorf("insert model request: %w", err)
	}
	if _, err := appendEvent(ctx, tx, tenant, sessionID, responseID, eventType, payload); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit model request: %w", err)
	}
	return nil
}

// LookupModelResult returns a committed model result for replay, if one exists. A
// reclaimed attempt re-derives the same stable model_request_id and finds the result
// here, so the provider is never dispatched twice for one logical request (spec §53.4).
func (s *Store) LookupModelResult(ctx context.Context, tenant Tenant, requestID string) ([]byte, bool, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	var state string
	var result []byte
	err := s.pool.QueryRow(ctx, storage.Query("GetModelResult"), requestID, tenant.Organization, tenant.Project).
		Scan(&state, &result)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read model result: %w", err)
	}
	if state != "completed" {
		return nil, false, nil
	}
	return result, true, nil
}

// CommitModelResult completes the model request row with its result and journals the
// result event in one transaction. The orchestrator calls it before delivering
// model.result to the engine, so no provider result reaches the engine until its state
// is durable (spec §24.7).
func (s *Store) CommitModelResult(ctx context.Context, tenant Tenant, sessionID, responseID, runID, requestID string, result []byte, eventType string, payload []byte) (int64, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, fmt.Errorf("begin model commit: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if err := guardRunActive(ctx, tx, tenant, runID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, storage.Query("CompleteModelRequest"),
		requestID, tenant.Organization, tenant.Project, result); err != nil {
		return 0, fmt.Errorf("complete model request: %w", err)
	}
	// A committed step folds every message delivered at a prior boundary into the request it just
	// answered, so mark the run's still-'delivered' rows 'folded' in this same transaction (spec
	// §26.9, E10 Task 2): the fold state and the result it belongs to move together. This is what
	// distinguishes variant-1 (crash before this commit — the row stays 'delivered') from R1 (crash
	// after — 'folded'); redelivery refolds either at its boundary, but the state is the honest record.
	if _, err := tx.Exec(ctx, storage.Query("MarkDeliveredMessagesFolded"),
		runID, tenant.Organization, tenant.Project); err != nil {
		return 0, fmt.Errorf("mark delivered messages folded: %w", err)
	}
	seq, err := appendEvent(ctx, tx, tenant, sessionID, responseID, eventType, payload)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit model result: %w", err)
	}
	return seq, nil
}

// ErrStaleToolCommit reports a tool result committed under a fence the ledger has since advanced past —
// a late callback from a superseded attempt (spec §26.7, TOL-017). The reclaiming attempt's higher
// fence wins; the stale commit is rejected rather than overwriting the newer row.
var ErrStaleToolCommit = errors.New("stale_tool_commit")

// CommitToolResult persists a completed tool_call row and its journal event in one
// transaction. The orchestrator calls it before delivering tool.result to the engine
// (commit-before-deliver). A pure tool INSERTs 'completed' fresh; a side-effecting tool pre-written
// 'executing' (BeginToolCall) is advanced to completed. A stale-fence late callback (TOL-017) changes 0
// rows and returns ErrStaleToolCommit; a benign re-commit of an already-resolved row is an idempotent
// no-op (0 rows, nil, no second event). Only a real completion journals its event.
func (s *Store) CommitToolResult(ctx context.Context, tenant Tenant, sessionID, responseID, runID string, fence uint64, callID, name string, arguments, result []byte, replayClass, requestHash, eventType string, payload []byte) (int64, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, fmt.Errorf("begin tool commit: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if err := guardRunActive(ctx, tx, tenant, runID); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, storage.Query("UpsertToolCall"),
		callID, tenant.Organization, tenant.Project, runID, int64(fence), name, arguments, result, replayClass, requestHash)
	if err != nil {
		return 0, fmt.Errorf("upsert tool call: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// The upsert changed nothing. Only a plain `completed` row is a benign idempotent re-drive (the
		// durable result already stands). ANY other state means this committer is stale: executing/leased
		// = the fence advanced past it (TOL-017), and uncertain/manual_resolution/reconciled_* = a
		// RECLAIMING attempt parked or resolved the call after this (stale) attempt passed the consult and
		// executed — so returning nil here would deliver a non-durable fresh result to the superseded
		// engine (commit-before-deliver breach) and let it reason on a result the reclaimer blocked
		// (§26.7 continuation-block, TOL-003). Reject it.
		var state string
		if err := tx.QueryRow(ctx, storage.Query("LookupToolCall"), callID, tenant.Organization, tenant.Project).
			Scan(&state, new(string), new(string), new(int64), new(string)); err != nil {
			return 0, fmt.Errorf("classify unchanged tool commit: %w", err)
		}
		if state != "completed" {
			return 0, ErrStaleToolCommit
		}
		if err := tx.Commit(ctx); err != nil { // already completed: idempotent no-op, no second event
			return 0, fmt.Errorf("commit tool result (idempotent): %w", err)
		}
		return 0, nil
	}
	seq, err := appendEvent(ctx, tx, tenant, sessionID, responseID, eventType, payload)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit tool result: %w", err)
	}
	return seq, nil
}

// LookupToolCall reads a tool_call's durable ledger row for the pre-execute consult (spec §26.7, E10
// T7): a completed row replays cached (never re-fires), an `uncertain` row blocks the call, an
// `executing` row (a kill mid-execute) is classified by replayClass. requestHash is the row's canonical
// (name, args) digest, so the replay path can reject a same-id call whose content diverged (TOL-016).
// found is false for a fresh call.
func (s *Store) LookupToolCall(ctx context.Context, tenant Tenant, callID string) (state, result, replayClass string, fence int64, requestHash string, found bool, err error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	switch e := s.pool.QueryRow(ctx, storage.Query("LookupToolCall"), callID, tenant.Organization, tenant.Project).
		Scan(&state, &result, &replayClass, &fence, &requestHash); {
	case errors.Is(e, pgx.ErrNoRows):
		return "", "", "", 0, "", false, nil
	case e != nil:
		return "", "", "", 0, "", false, fmt.Errorf("lookup tool call: %w", e)
	}
	return state, result, replayClass, fence, requestHash, true, nil
}

// BeginToolCall records the durable PRE-EXECUTE marker for a side-effecting tool (spec §26.6-26.7, E10
// T7): the row goes to 'executing' BEFORE the external effect, so a kill between execute and commit is
// detectable as uncertain. It journals tool_call.executing.v1 on a fresh pre-write. Runs under
// guardRunActive. Idempotent: a redelivered pre-write advances the fence but does not reopen a resolved row.
func (s *Store) BeginToolCall(ctx context.Context, tenant Tenant, sessionID, responseID, runID string, fence uint64, callID, name string, arguments []byte, replayClass, requestHash, externalKey, boundary string) error {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin tool pre-write: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if err := guardRunActive(ctx, tx, tenant, runID); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, storage.Query("BeginToolCall"),
		callID, tenant.Organization, tenant.Project, runID, int64(fence), name, arguments, replayClass, requestHash, externalKey, fmt.Sprintf("%d", fence), boundary)
	if err != nil {
		return fmt.Errorf("pre-write tool call: %w", err)
	}
	if tag.RowsAffected() > 0 {
		if _, err := appendEvent(ctx, tx, tenant, sessionID, responseID, "tool_call.executing.v1",
			mustMarshal(map[string]any{"run_id": runID, "tool_call_id": callID, "replay_class": replayClass})); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tool pre-write: %w", err)
	}
	return nil
}

// MarkToolCallUncertain drives an in-flight (executing/leased) tool_call to `uncertain` and journals
// tool_call.uncertain.v1 (spec §26.7): a kill-after-execute for a class that must not auto-replay. It
// returns whether it transitioned a row (false when a racing path already resolved it). Runs under
// guardRunActive.
func (s *Store) MarkToolCallUncertain(ctx context.Context, tenant Tenant, sessionID, responseID, runID, callID string) (bool, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, fmt.Errorf("begin mark uncertain: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if err := guardRunActive(ctx, tx, tenant, runID); err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, storage.Query("MarkToolCallUncertain"), callID, tenant.Organization, tenant.Project)
	if err != nil {
		return false, fmt.Errorf("mark tool call uncertain: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil // already resolved by a racing path
	}
	if _, err := appendEvent(ctx, tx, tenant, sessionID, responseID, "tool_call.uncertain.v1",
		mustMarshal(map[string]any{"run_id": runID, "tool_call_id": callID})); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit mark uncertain: %w", err)
	}
	return true, nil
}

// RunResolvedToolCalls returns a run's resolved tool_calls as class-labelled "tool_call_id:replay_class"
// strings — the RecoveryProof's reused-tool accounting (spec §26.12, E10 T7). On a transcript
// reconstruction these are the calls whose committed result is REUSED from the ledger (replayed, never
// re-executed). Empty (a run with no tool calls, or a compatible restore) is honest evidence too.
func (s *Store) RunResolvedToolCalls(ctx context.Context, tenant Tenant, runID string) ([]string, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	rows, err := s.pool.Query(ctx, storage.Query("RunResolvedToolCalls"), runID, tenant.Organization, tenant.Project)
	if err != nil {
		return nil, fmt.Errorf("read resolved tool calls: %w", err)
	}
	defer rows.Close()
	labels := []string{}
	for rows.Next() {
		var id, class string
		if err := rows.Scan(&id, &class); err != nil {
			return nil, fmt.Errorf("scan resolved tool call: %w", err)
		}
		labels = append(labels, id+":"+class)
	}
	return labels, rows.Err()
}

// ReenqueueResponseRun enqueues a fresh response.run job for a run so a new attempt continues it (spec
// §26.7, E10 T7): after the reconcile loop resolves an uncertain tool_call, the run — left running when
// its attempt STOPPED on the uncertain call — needs a fresh attempt to reconstruct and proceed (the
// resolved row now replays or re-executes). Idempotent-safe: a duplicate job exact-stands-down against
// any live one (RunHasLiveResponseJob), so an over-enqueue never double-drives.
func (s *Store) ReenqueueResponseRun(ctx context.Context, tenant Tenant, runID string) error {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	jobID, err := newJobID()
	if err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, storage.Query("EnqueueJob"),
		jobID, tenant.Organization, tenant.Project, "response.run", []byte(fmt.Sprintf(`{"run_id":%q}`, runID))); err != nil {
		return fmt.Errorf("re-enqueue response run: %w", err)
	}
	return nil
}

// UncertainToolCall is one uncertain tool_call the reconcile loop must resolve (spec §26.7, E10 T7). It
// carries the session/response the resolution event is journaled under and the run the reconcile
// re-enqueues.
type UncertainToolCall struct {
	CallID      string
	Tenant      Tenant
	RunID       string
	SessionID   string
	ResponseID  string
	Name        string
	ReplayClass string
	ExternalKey string
}

// UncertainToolCalls reads up to limit uncertain tool_calls awaiting reconciliation across all tenants —
// the reconcile loop's sweep read (spec §26.7). Ordered oldest-first so resolution is deterministic.
func (s *Store) UncertainToolCalls(ctx context.Context, limit int) ([]UncertainToolCall, error) {
	rows, err := s.pool.Query(ctx, storage.Query("SelectUncertainToolCalls"), limit)
	if err != nil {
		return nil, fmt.Errorf("select uncertain tool calls: %w", err)
	}
	defer rows.Close()
	var out []UncertainToolCall
	for rows.Next() {
		var u UncertainToolCall
		if err := rows.Scan(&u.CallID, &u.Tenant.Organization, &u.Tenant.Project, &u.RunID, &u.SessionID, &u.ResponseID, &u.Name, &u.ReplayClass, &u.ExternalKey); err != nil {
			return nil, fmt.Errorf("scan uncertain tool call: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ReconcileToolCall resolves an `uncertain` tool_call to one of the §26.7 exits and journals the
// matching event (E10 T7): "reconciled_completed" (the destination applied it — its result re-enters
// reasoning), "reconciled_not_applied" (it did not — a typed not-applied result), or "manual_resolution"
// (a human must decide — the irreversible default). Single winner on 'uncertain', so a racing reconcile
// settles once (RowsAffected 0 → a no-op). result is optional. It does NOT run under guardRunActive: a
// reconcile settles a durable ledger row even for a run paused/waiting on it.
func (s *Store) ReconcileToolCall(ctx context.Context, tenant Tenant, sessionID, responseID, runID, callID, resolution string, result []byte) error {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	var newState, event string
	switch resolution {
	case "reconciled_completed":
		newState, event = "reconciled_completed", "tool_call.reconciled_completed.v1"
	case "reconciled_not_applied":
		newState, event = "reconciled_not_applied", "tool_call.reconciled_not_applied.v1"
	case "manual_resolution":
		newState, event = "manual_resolution", "tool_call.manual_resolution.v1"
	default:
		return fmt.Errorf("unknown tool reconciliation %q", resolution)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin reconcile tool call: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var resultArg any
	if len(result) > 0 {
		resultArg = result
	}
	tag, err := tx.Exec(ctx, storage.Query("ReconcileToolCall"),
		callID, tenant.Organization, tenant.Project, newState, newState, resultArg)
	if err != nil {
		return fmt.Errorf("reconcile tool call: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil // already resolved by a racing reconcile
	}
	if _, err := appendEvent(ctx, tx, tenant, sessionID, responseID, event,
		mustMarshal(map[string]any{"run_id": runID, "tool_call_id": callID, "resolution": resolution})); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit reconcile tool call: %w", err)
	}
	return nil
}

// PendingToolOperations returns a run's UNRESOLVED tool operations as a JSON array — the checkpoint's
// pending_operations content (spec §26.2, §26.4, E10 T7). Each element is
// {tool_call_id, name, replay_class, reconciliation_state} for a row still `uncertain` or in
// `manual_resolution`. A run with none returns "[]" (never null), so a checkpoint always records a
// well-formed array and a RESTORE that reads it back can honestly report zero in-flight effects. This is
// CP-resolved at persist time — the engine never sees the ledger (§24).
func (s *Store) PendingToolOperations(ctx context.Context, tenant Tenant, runID string) ([]byte, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	rows, err := s.pool.Query(ctx, storage.Query("PendingToolOperationsForRun"), runID, tenant.Organization, tenant.Project)
	if err != nil {
		return nil, fmt.Errorf("read pending tool operations: %w", err)
	}
	defer rows.Close()
	ops := []map[string]any{}
	for rows.Next() {
		var id, name, replayClass, reconciliationState string
		if err := rows.Scan(&id, &name, &replayClass, &reconciliationState); err != nil {
			return nil, fmt.Errorf("scan pending tool operation: %w", err)
		}
		ops = append(ops, map[string]any{
			"tool_call_id": id, "name": name,
			"replay_class": replayClass, "reconciliation_state": reconciliationState,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return json.Marshal(ops)
}

// FinalizeResponse writes the terminal Response projection built from committed run,
// output, and usage. It is the last durable write of a run, so a restart reads the
// same terminal status and body (spec §24.7, LP-008).
func (s *Store) FinalizeResponse(ctx context.Context, tenant Tenant, responseID, state string, projection []byte) error {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	if _, err := s.pool.Exec(ctx, storage.Query("UpdateResponse"),
		responseID, tenant.Organization, tenant.Project, state, projection); err != nil {
		return fmt.Errorf("finalize response %s: %w", responseID, err)
	}
	return nil
}
