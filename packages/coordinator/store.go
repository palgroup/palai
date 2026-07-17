// Package coordinator is the durable job engine for the Palai control plane
// (spec §24.4). It productionizes the postgres-coordinator spike: worker claims
// use database-clock leases and monotonic fencing tokens, and an authoritative
// completion writes result state and its outbox row in one transaction.
package coordinator

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
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
	// ErrRunTerminal is returned when a run transition is rejected because the run is
	// already terminal. It wraps statemachines.ErrInvalidState, so existing
	// invalid-state checks still match, while letting a job handler tell "already
	// terminal" apart from "already advanced past this step": a terminal run (e.g.
	// canceled before dispatch) must not be silently treated as successfully assigned.
	ErrRunTerminal = errors.New("run_terminal")
)

// runTerminalStates marks the terminal run states (table destinations with no
// outgoing transition), derived once from the canonical table rather than hardcoded.
var runTerminalStates = statemachines.TerminalStates(statemachines.RunTable)

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
		if runTerminalStates[statemachines.RunState(current)] {
			return Transition{}, fmt.Errorf("%w: %w", ErrRunTerminal, err)
		}
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

func newJobID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate job id: %w", err)
	}
	return "job_" + hex.EncodeToString(raw[:]), nil
}

// Identity is the tenant an API key resolves to (spec §39.2).
type Identity struct {
	Organization string
	Project      string
	Principal    string
}

// ErrInvalidToken is returned when a bearer key matches no live api_keys row. Its
// stable Problem code is invalid_token.
var ErrInvalidToken = errors.New("invalid_token")

// HashAPIKey derives the stored verifier for a bearer key. api_keys.key_hash holds
// this digest; the full key is never persisted (spec §20 security). API keys are
// high-entropy tokens, so a fast digest is the standard verifier, not a password KDF.
func HashAPIKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// VerifyAPIKey resolves a bearer key to its tenant. It is the sole read keyed by
// the credential hash rather than the tenant, because it establishes the tenant
// every later query is scoped by; the body's project_id can never override it.
// A key with no project is rejected: the LP-0 surface only admits project-scoped keys.
func (s *Store) VerifyAPIKey(ctx context.Context, token string) (Identity, error) {
	var id Identity
	var project *string
	err := s.pool.QueryRow(ctx, storage.Query("VerifyAPIKey"), HashAPIKey(token)).
		Scan(&id.Organization, &project, &id.Principal)
	if errors.Is(err, pgx.ErrNoRows) {
		return Identity{}, ErrInvalidToken
	}
	if err != nil {
		return Identity{}, fmt.Errorf("verify api key: %w", err)
	}
	if project == nil {
		return Identity{}, ErrInvalidToken
	}
	id.Project = *project
	return id, nil
}

// AdmissionInput is a fully-resolved response admission (spec §20.9, §8.3). The
// caller mints the IDs and the response Body so the idempotency record can store
// the exact body a replay must return.
type AdmissionInput struct {
	Principal      string
	IdempotencyKey string
	Method         string
	Route          string
	RequestHash    string
	ResponseID     string
	RunID          string
	SessionID      string
	Input          []byte
	Body           []byte
}

// Admission is the committed, replayed, or conflicting admission outcome.
type Admission struct {
	Body     []byte
	Replayed bool
	Conflict bool
}

// AdmitResponse atomically reserves the idempotency key and, on a fresh key,
// creates the transient session, response, and root run plus the run.queued.v1
// birth event in one transaction (spec §20.9 step 3, §8.3). The reservation blocks
// concurrent duplicates on the unique index, so a reused key with the same request
// replays the stored body and a reused key with a different request reports a
// conflict — both without a second side effect. Nothing is dispatched here: the
// outbox row is the post-commit handoff, so dispatch never begins before commit.
func (s *Store) AdmitResponse(ctx context.Context, tenant Tenant, in AdmissionInput) (Admission, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Admission{}, fmt.Errorf("begin admission: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	err = tx.QueryRow(ctx, storage.Query("ReserveIdempotency"),
		tenant.Organization, tenant.Project, in.Principal, in.Method, in.Route, in.IdempotencyKey, in.RequestHash, in.Body).
		Scan(new(int64))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// The key is already reserved: replay on a matching request, conflict otherwise.
		var storedHash string
		var storedBody []byte
		if err := tx.QueryRow(ctx, storage.Query("GetIdempotency"),
			tenant.Organization, tenant.Project, in.Principal, in.Method, in.Route, in.IdempotencyKey).
			Scan(&storedHash, &storedBody); err != nil {
			return Admission{}, fmt.Errorf("read idempotency record: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return Admission{}, fmt.Errorf("commit idempotency read: %w", err)
		}
		if storedHash != in.RequestHash {
			return Admission{Conflict: true}, nil
		}
		return Admission{Body: storedBody, Replayed: true}, nil
	case err != nil:
		return Admission{}, fmt.Errorf("reserve idempotency key: %w", err)
	}

	// Fresh key: create the durable resources and the birth event atomically.
	if _, err := tx.Exec(ctx, storage.Query("InsertSession"),
		in.SessionID, tenant.Organization, tenant.Project); err != nil {
		return Admission{}, fmt.Errorf("insert session: %w", err)
	}
	if _, err := tx.Exec(ctx, storage.Query("InsertResponse"),
		in.ResponseID, tenant.Organization, tenant.Project, in.SessionID, in.Input); err != nil {
		return Admission{}, fmt.Errorf("insert response: %w", err)
	}
	if _, err := tx.Exec(ctx, storage.Query("InsertRun"),
		in.RunID, tenant.Organization, tenant.Project, in.SessionID, in.ResponseID); err != nil {
		return Admission{}, fmt.Errorf("insert run: %w", err)
	}

	var seq int64
	if err := tx.QueryRow(ctx, storage.Query("AllocateSequence"), in.SessionID).Scan(&seq); err != nil {
		return Admission{}, fmt.Errorf("allocate session sequence: %w", err)
	}
	eventID, err := newEventID()
	if err != nil {
		return Admission{}, err
	}
	payload := fmt.Sprintf(`{"run_id":%q,"state":"queued"}`, in.RunID)
	if _, err := tx.Exec(ctx, storage.Query("AppendEvent"),
		eventID, tenant.Organization, tenant.Project, in.SessionID, seq, "run.queued.v1", []byte(payload)); err != nil {
		return Admission{}, fmt.Errorf("append queued event: %w", err)
	}
	dedupe := fmt.Sprintf("run:%s:seq:%d", in.RunID, seq)
	if _, err := tx.Exec(ctx, storage.Query("EnqueueOutbox"),
		tenant.Organization, tenant.Project, "run.queued.v1", dedupe, []byte(payload)); err != nil {
		return Admission{}, fmt.Errorf("enqueue admission outbox: %w", err)
	}

	// Enqueue the durable dispatch job in the same transaction, so the queued run is
	// actually assigned by a worker once committed. Nothing runs before commit: the
	// job becomes claimable only when the admission is durable (spec §24.4, §24.5).
	jobID, err := newJobID()
	if err != nil {
		return Admission{}, err
	}
	jobPayload := fmt.Sprintf(`{"run_id":%q}`, in.RunID)
	if _, err := tx.Exec(ctx, storage.Query("EnqueueJob"),
		jobID, tenant.Organization, tenant.Project, "response.run", []byte(jobPayload)); err != nil {
		return Admission{}, fmt.Errorf("enqueue dispatch job: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Admission{}, fmt.Errorf("commit admission: %w", err)
	}
	return Admission{Body: in.Body}, nil
}
