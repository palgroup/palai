// Package coordinator is the durable job engine for the Palai control plane
// (spec §24.4). It productionizes the postgres-coordinator spike: worker claims
// use database-clock leases and monotonic fencing tokens, and an authoritative
// completion writes result state and its outbox row in one transaction.
package coordinator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	statemachines "github.com/palgroup/palai/packages/state-machines"
	"github.com/palgroup/palai/storage"
)

var (
	// ErrNoClaimableJob is returned when no job matches the claim predicate.
	ErrNoClaimableJob = errors.New("no claimable job")
	// ErrStaleFence is returned when a completion offers a fence that no longer
	// owns the job. Its stable Problem code aligns with lease_conflict.
	ErrStaleFence = errors.New("lease_conflict")
)

// Tenant is the organization/project scope every query is keyed by (spec §39.2).
type Tenant struct {
	Organization string
	Project      string
}

// Store owns a connection pool against the durable spine schema.
type Store struct {
	pool *pgxpool.Pool
}

// Claim is a fenced lease grant over a durable job.
type Claim struct {
	Tenant         Tenant
	JobID          string
	Owner          string
	Fence          int64
	AttemptCount   int
	LeaseExpiresAt time.Time
}

// Snapshot is the authoritative job state for assertions and recovery.
type Snapshot struct {
	Status       string
	Fence        int64
	AttemptCount int
	ResultHash   *string
}

// Open connects a Store. databaseURL carries a local throwaway credential.
func Open(ctx context.Context, databaseURL string) (*Store, error) {
	pool, err := storage.OpenPool(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	return &Store{pool: pool}, nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Pool exposes the underlying pool for sibling stores that share a connection.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Migrate applies the forward core migration. It is safe to run repeatedly.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, storage.MigrationUp()); err != nil {
		return fmt.Errorf("apply migration: %w", err)
	}
	return nil
}

// Rollback reverses the core migration.
func (s *Store) Rollback(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, storage.MigrationDown()); err != nil {
		return fmt.Errorf("roll back migration: %w", err)
	}
	return nil
}

// Enqueue inserts a queued job.
func (s *Store) Enqueue(ctx context.Context, tenant Tenant, jobID, kind string) error {
	if strings.TrimSpace(jobID) == "" {
		return errors.New("job ID is required")
	}
	_, err := s.pool.Exec(ctx, storage.Query("EnqueueJob"),
		jobID, tenant.Organization, tenant.Project, kind, []byte("{}"))
	if err != nil {
		return fmt.Errorf("enqueue job: %w", err)
	}
	return nil
}

// Claim leases a job, incrementing its fence and attempt count, and records the
// attempt in the same transaction. A reclaim after lease expiry always returns a
// strictly higher fence than the previous holder (spec §53.5).
func (s *Store) Claim(ctx context.Context, tenant Tenant, jobID, owner string, lease time.Duration) (Claim, error) {
	if strings.TrimSpace(jobID) == "" || strings.TrimSpace(owner) == "" || lease <= 0 {
		return Claim{}, errors.New("job ID, owner and positive lease are required")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Claim{}, fmt.Errorf("begin claim: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	claim := Claim{Tenant: tenant}
	err = tx.QueryRow(ctx, storage.Query("ClaimJob"),
		jobID, tenant.Organization, tenant.Project, owner, lease.Milliseconds()).
		Scan(&claim.JobID, &claim.Owner, &claim.Fence, &claim.AttemptCount, &claim.LeaseExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Claim{}, ErrNoClaimableJob
	}
	if err != nil {
		return Claim{}, fmt.Errorf("claim job: %w", err)
	}
	if _, err := tx.Exec(ctx, storage.Query("RecordJobAttempt"), claim.JobID, claim.Fence, claim.Owner); err != nil {
		return Claim{}, fmt.Errorf("record job attempt: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Claim{}, fmt.Errorf("commit claim: %w", err)
	}
	return claim, nil
}

// LeaseExpired reports whether the job's lease has lapsed by database time.
func (s *Store) LeaseExpired(ctx context.Context, tenant Tenant, jobID string) (bool, error) {
	var expired bool
	err := s.pool.QueryRow(ctx, storage.Query("JobLeaseExpired"), jobID, tenant.Organization, tenant.Project).Scan(&expired)
	if err != nil {
		return false, fmt.Errorf("read lease expiry: %w", err)
	}
	return expired, nil
}

// Complete records the authoritative result and its outbox row in one
// transaction. A completion whose fence no longer owns the job affects zero rows
// and returns ErrStaleFence without writing result or outbox state.
func (s *Store) Complete(ctx context.Context, claim Claim, resultHash string) error {
	if claim.JobID == "" || claim.Fence < 1 || resultHash == "" {
		return errors.New("valid claim and result hash are required")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin completion: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	tag, err := tx.Exec(ctx, storage.Query("CompleteJob"),
		claim.JobID, claim.Tenant.Organization, claim.Tenant.Project, claim.Fence, resultHash)
	if err != nil {
		return fmt.Errorf("update completed job: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrStaleFence
	}
	dedupe := fmt.Sprintf("job:%s:fence:%d:completed", claim.JobID, claim.Fence)
	payload := fmt.Sprintf(`{"job_id":%q,"result_hash":%q}`, claim.JobID, resultHash)
	if _, err := tx.Exec(ctx, storage.Query("EnqueueOutbox"),
		claim.Tenant.Organization, claim.Tenant.Project, "job.completed", dedupe, []byte(payload)); err != nil {
		return fmt.Errorf("insert completion outbox: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit completion: %w", err)
	}
	return nil
}

// Snapshot reads authoritative job state.
func (s *Store) Snapshot(ctx context.Context, tenant Tenant, jobID string) (Snapshot, error) {
	var snap Snapshot
	err := s.pool.QueryRow(ctx, storage.Query("JobSnapshot"), jobID, tenant.Organization, tenant.Project).
		Scan(&snap.Status, &snap.Fence, &snap.AttemptCount, &snap.ResultHash)
	if err != nil {
		return Snapshot{}, fmt.Errorf("read job snapshot: %w", err)
	}
	return snap, nil
}

// Transition is the committed outcome of a run state change.
type Transition struct {
	To       statemachines.RunState
	Event    string
	Sequence int64
}

// AllocateSequence hands out the next strictly-increasing sequence for a session.
// The allocation is a single atomic upsert, so concurrent callers on one session
// receive unique, gap-free numbers (spec §21.1).
func (s *Store) AllocateSequence(ctx context.Context, sessionID string) (int64, error) {
	var seq int64
	if err := s.pool.QueryRow(ctx, storage.Query("AllocateSequence"), sessionID).Scan(&seq); err != nil {
		return 0, fmt.Errorf("allocate session sequence: %w", err)
	}
	return seq, nil
}

// ApplyRunTransition is the transactional transition callback (spec §21.1, §24.5).
// It locks the run, asks the pure RunTable whether the command is legal, then
// writes the new state, the session event, and the outbox row in one transaction.
// The committed Transition is returned only after commit succeeds; a rejected
// command or a failed commit leaves no state, event, or outbox row behind.
func (s *Store) ApplyRunTransition(ctx context.Context, tenant Tenant, runID string, command statemachines.RunCommand) (Transition, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Transition{}, fmt.Errorf("begin transition: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var sessionID, current string
	err = tx.QueryRow(ctx, storage.Query("LockRun"), runID, tenant.Organization, tenant.Project).Scan(&sessionID, &current)
	if errors.Is(err, pgx.ErrNoRows) {
		return Transition{}, fmt.Errorf("run %s not found in tenant scope", runID)
	}
	if err != nil {
		return Transition{}, fmt.Errorf("lock run: %w", err)
	}

	next, event, err := statemachines.Apply(statemachines.RunState(current), command, statemachines.RunTable)
	if err != nil {
		return Transition{}, err
	}

	if _, err := tx.Exec(ctx, storage.Query("UpdateRunState"), runID, tenant.Organization, tenant.Project, string(next)); err != nil {
		return Transition{}, fmt.Errorf("update run state: %w", err)
	}

	var seq int64
	if err := tx.QueryRow(ctx, storage.Query("AllocateSequence"), sessionID).Scan(&seq); err != nil {
		return Transition{}, fmt.Errorf("allocate session sequence: %w", err)
	}

	eventID, err := newEventID()
	if err != nil {
		return Transition{}, err
	}
	payload := fmt.Sprintf(`{"run_id":%q,"state":%q}`, runID, next)
	if _, err := tx.Exec(ctx, storage.Query("AppendEvent"),
		eventID, tenant.Organization, tenant.Project, sessionID, seq, event, []byte(payload)); err != nil {
		return Transition{}, fmt.Errorf("append event: %w", err)
	}

	dedupe := fmt.Sprintf("run:%s:seq:%d", runID, seq)
	if _, err := tx.Exec(ctx, storage.Query("EnqueueOutbox"),
		tenant.Organization, tenant.Project, event, dedupe, []byte(payload)); err != nil {
		return Transition{}, fmt.Errorf("enqueue outbox: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Transition{}, fmt.Errorf("commit transition: %w", err)
	}
	return Transition{To: next, Event: event, Sequence: seq}, nil
}

func newEventID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate event id: %w", err)
	}
	return "evt_" + hex.EncodeToString(raw[:]), nil
}
