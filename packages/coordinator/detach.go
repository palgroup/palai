package coordinator

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	statemachines "github.com/palgroup/palai/packages/state-machines"
	"github.com/palgroup/palai/storage"
)

// ChildRunLookup is an existing child resolved for the re-emit rebind (spec §25.18-19, E10 T8
// DET-001): a restored parent re-emits the SAME deterministic child.request, so the controller
// binds this row rather than cloning a second child. State lets it decide (terminal ⇒ fold the
// typed result, still-running ⇒ re-release), and ResponseID keys the child's outcome read.
type ChildRunLookup struct {
	RunID      string
	State      string
	ResponseID string
	Detached   bool
}

// LookupChildByRequest resolves the child a parent already spawned for a child_request_id (E10 T8,
// the re-emit keystone). The linkage rides the child's delegation.spec JSONB (child_request_id +
// detached flag) — no separate column, no migration (the child row already carries delegation). found
// is false when no child was spawned for this request yet (the fresh-spawn path).
func (s *Store) LookupChildByRequest(ctx context.Context, tenant Tenant, parentRunID, childRequestID string) (ChildRunLookup, bool, error) {
	var out ChildRunLookup
	err := s.pool.QueryRow(ctx, storage.Query("LookupChildByRequest"),
		parentRunID, tenant.Organization, tenant.Project, childRequestID).
		Scan(&out.RunID, &out.State, &out.ResponseID, &out.Detached)
	if errors.Is(err, pgx.ErrNoRows) {
		return ChildRunLookup{}, false, nil
	}
	if err != nil {
		return ChildRunLookup{}, false, fmt.Errorf("lookup child by request %s: %w", childRequestID, err)
	}
	return out, true, nil
}

// EnqueueRunJob enqueues a response.run job for a run so a worker opens a fresh attempt on it (E10
// T8): a detached child becomes a durable job here — the parent releases its compute, and even a
// single-worker stack runs the child (the E08 T5 inline-deadlock is dissolved because the parent no
// longer holds the engine while the child dials). It mirrors the resume enqueue (commands.go).
func (s *Store) EnqueueRunJob(ctx context.Context, tenant Tenant, runID string) error {
	jobID, err := newJobID()
	if err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, storage.Query("EnqueueJob"),
		jobID, tenant.Organization, tenant.Project, "response.run", []byte(fmt.Sprintf(`{"run_id":%q}`, runID))); err != nil {
		return fmt.Errorf("enqueue run job for %s: %w", runID, err)
	}
	return nil
}

// WakeParentOfChild wakes the detached parent of a just-terminal child (E10 T8, DET-001): it resolves
// the child's parent and, if that parent is a released (WAITING) run whose children are ALL now
// terminal, re-enters it into running and enqueues its response.run job. A root child (no parent) or a
// parent not waiting is a no-op. Called from the child's finalize; the single-winner discipline lives
// in wakeDetachedParentTx.
func (s *Store) WakeParentOfChild(ctx context.Context, tenant Tenant, childRunID string) (bool, error) {
	var parentRunID *string
	if err := s.pool.QueryRow(ctx, storage.Query("RunParentRun"), childRunID, tenant.Organization, tenant.Project).
		Scan(&parentRunID); err != nil {
		return false, fmt.Errorf("read parent of %s: %w", childRunID, err)
	}
	if parentRunID == nil {
		return false, nil // a root run has no parent to wake
	}
	return s.WakeDetachedParent(ctx, tenant, *parentRunID)
}

// WakeDetachedParent re-enters a released (WAITING) parent into running and enqueues its response.run
// job, but ONLY when no non-terminal child remains — so the resumed parent re-emits every child.request
// to a terminal child (a clean fold, never a re-spawn). It is the exactly-once wake (DET-001):
//
//   - The WAITING→running transition is single-winner under the run lock, so a doubled child terminal
//     (redelivery), or the race between a child's wake and the parent's own post-release self-wake,
//     re-enters the parent EXACTLY once — the loser sees a running (not waiting) parent and no-ops.
//   - A parent not yet waiting (its child finished before it reached the release), or one with a
//     still-running sibling, is left for the last finisher (or the parent's self-wake) to pick up.
func (s *Store) WakeDetachedParent(ctx context.Context, tenant Tenant, parentRunID string) (bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, fmt.Errorf("begin wake parent: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var sessionID, state string
	var responseID *string
	if err := tx.QueryRow(ctx, storage.Query("LockRun"), parentRunID, tenant.Organization, tenant.Project).
		Scan(&sessionID, &responseID, &state); err != nil {
		return false, fmt.Errorf("lock parent for wake: %w", err)
	}
	if statemachines.RunState(state) != statemachines.RunWaiting {
		return false, nil // not released yet, or another wake already re-entered it
	}
	var hasLive bool
	if err := tx.QueryRow(ctx, storage.Query("HasNonTerminalChildRun"), parentRunID, tenant.Organization, tenant.Project).
		Scan(&hasLive); err != nil {
		return false, fmt.Errorf("check live children: %w", err)
	}
	if hasLive {
		return false, nil // still awaiting a child; the last finisher wakes it
	}
	if _, err := applyRunTransitionTx(ctx, tx, tenant, parentRunID, statemachines.RunCmdResume); err != nil {
		return false, err
	}
	jobID, err := newJobID()
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, storage.Query("EnqueueJob"),
		jobID, tenant.Organization, tenant.Project, "response.run", []byte(fmt.Sprintf(`{"run_id":%q}`, parentRunID))); err != nil {
		return false, fmt.Errorf("enqueue parent wake job: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit parent wake: %w", err)
	}
	return true, nil
}

// JournalChildCompletionOnce journals a child.completed.v1 on the PARENT's response guarded so it
// lands EXACTLY once per child, even when a detached parent restores and re-folds the terminal child
// more than once (E10 T8, DET-001). The fresh inline path folds once and never re-enters this guard;
// the detached rebind may, so the existence check keeps the parent's stream honest. Guarded by the
// parent being active (a canceled parent appends nothing after its terminal, §22.3).
func (s *Store) JournalChildCompletionOnce(ctx context.Context, tenant Tenant, sessionID, parentResponseID, parentRunID, eventType, childRunID string, payload []byte) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin child completion: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if err := guardRunActive(ctx, tx, tenant, parentRunID); err != nil {
		return err
	}
	var exists bool
	if err := tx.QueryRow(ctx, storage.Query("ChildLifecycleEventExists"),
		parentResponseID, tenant.Organization, tenant.Project, eventType, childRunID).Scan(&exists); err != nil {
		return fmt.Errorf("check child completion event: %w", err)
	}
	if exists {
		return tx.Commit(ctx) // already journaled by an earlier restore; the fold stays exactly-once
	}
	if _, err := appendEvent(ctx, tx, tenant, sessionID, parentResponseID, eventType, payload); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit child completion: %w", err)
	}
	return nil
}
