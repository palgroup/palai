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
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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

// CurrentJournalSequence returns the highest event seq in the session's journal, or 0 for an empty
// journal — the transcript boundary a checkpoint records (spec §26.1-26.2).
func (s *Store) CurrentJournalSequence(ctx context.Context, tenant Tenant, sessionID string) (int64, error) {
	var seq int64
	if err := s.pool.QueryRow(ctx, storage.Query("CurrentJournalSequence"), sessionID, tenant.Organization, tenant.Project).Scan(&seq); err != nil {
		return 0, fmt.Errorf("read current journal sequence: %w", err)
	}
	return seq, nil
}

// RunCheckpoint is a run's newest durable checkpoint plus the §26.4 compatibility inputs the
// recovery ladder weighs (spec §26.3-26.4, E10 T4). WorkspaceSnapshotID is "" when the checkpoint
// declared NO workspace dependency (stored NULL); the ObjectKey/ContentChecksum locate + verify the
// opaque bytes the control plane fetches for a restore.
type RunCheckpoint struct {
	CheckpointID        string
	BoundaryID          string
	AttemptID           string
	Format              string
	FormatVersion       int
	ConfigSnapshotHash  string
	ProtocolVersion     string
	TranscriptSequence  int64
	WorkspaceSnapshotID string
	ContentChecksum     string
	ObjectKey           string
	SizeBytes           int64
}

// LatestRunCheckpoint reads a run's newest checkpoint (spec §26.3-26.4). found is false — with a nil
// error — for a run that has no checkpoint, so the ladder falls to transcript reconstruction rather
// than a phantom restore. The read is index-backed (checkpoints_by_run) and tenant-scoped.
func (s *Store) LatestRunCheckpoint(ctx context.Context, tenant Tenant, runID string) (RunCheckpoint, bool, error) {
	var cp RunCheckpoint
	var workspaceSnapshot *string
	err := s.pool.QueryRow(ctx, storage.Query("LatestRunCheckpoint"), runID, tenant.Organization, tenant.Project).
		Scan(&cp.CheckpointID, &cp.BoundaryID, &cp.AttemptID, &cp.Format, &cp.FormatVersion,
			&cp.ConfigSnapshotHash, &cp.ProtocolVersion, &cp.TranscriptSequence, &workspaceSnapshot,
			&cp.ContentChecksum, &cp.ObjectKey, &cp.SizeBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return RunCheckpoint{}, false, nil
	}
	if err != nil {
		return RunCheckpoint{}, false, fmt.Errorf("read latest run checkpoint: %w", err)
	}
	if workspaceSnapshot != nil {
		cp.WorkspaceSnapshotID = *workspaceSnapshot
	}
	return cp, true, nil
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

	trans, err := applyRunTransitionTx(ctx, tx, tenant, runID, command)
	if err != nil {
		return Transition{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Transition{}, fmt.Errorf("commit transition: %w", err)
	}
	return trans, nil
}

// applyRunTransitionTx applies one RunTable transition within tx: it locks the run, asks the pure
// table whether the command is legal, then writes the new state, the session event, and the
// outbox row, sweeping the run's still-queued commands when the transition is terminal (spec
// §21.1, §22.3, §22.4). It is the shared body of ApplyRunTransition and the composed lifecycle
// paths (PauseRun's wait, resume's re-entry) that pair a run transition with a command-applied in
// one transaction. A terminal run rejects with ErrRunTerminal.
func applyRunTransitionTx(ctx context.Context, tx pgx.Tx, tenant Tenant, runID string, command statemachines.RunCommand) (Transition, error) {
	var sessionID, current string
	var responseID *string
	err := tx.QueryRow(ctx, storage.Query("LockRun"), runID, tenant.Organization, tenant.Project).Scan(&sessionID, &responseID, &current)
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
		eventID, tenant.Organization, tenant.Project, sessionID, responseID, seq, event, []byte(payload)); err != nil {
		return Transition{}, fmt.Errorf("append event: %w", err)
	}

	dedupe := fmt.Sprintf("run:%s:seq:%d", runID, seq)
	if _, err := tx.Exec(ctx, storage.Query("EnqueueOutbox"),
		tenant.Organization, tenant.Project, event, dedupe, []byte(payload)); err != nil {
		return Transition{}, fmt.Errorf("enqueue outbox: %w", err)
	}

	// A terminalizing run expires its still-queued commands atomically (spec §22.4 lifecycle):
	// a mid-run-accepted command that never reached a delivery boundary must not stay queued.
	// This is the single choke point every terminal path (engine terminal, cancel, fail) routes
	// through, so the sweep runs exactly once per run (terminality is monotonic).
	if runTerminalStates[next] {
		if err := sweepQueuedCommands(ctx, tx, tenant, sessionID, responseID, runID); err != nil {
			return Transition{}, err
		}
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

func newWorkspaceID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate workspace id: %w", err)
	}
	return "ws_" + hex.EncodeToString(raw[:]), nil
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
	// SessionID is the freshly minted session id used when the response opens a new session.
	// A chained response (RequestedSessionID / PreviousResponseID set) reuses the resolved
	// existing session instead, and this minted id is discarded.
	SessionID string
	// RequestedSessionID / PreviousResponseID chain onto an existing session (spec §9, LP
	// Task 5 minor-a). At most one is set (the handler rejects both). An unknown or foreign
	// id resolves to SessionNotFound (404); a non-active session to SessionConflict (409).
	RequestedSessionID *string
	PreviousResponseID *string
	Input              []byte
	Body               []byte
	// Store is the §8.3 retention flag persisted on the response. It defaults true
	// (persistent); the caller resolves an absent request field to true before admission.
	Store bool
	// Delegations is the root run's required-delegation JSON ({"emit":[...],"budget":N}) or nil,
	// persisted on the run so the orchestrator seeds run.start (spec §25.18).
	Delegations []byte
	// RepositoryBindingID / RepositoryRef carry the contracted `repository` field (spec §30.1, E09
	// Task 10): when the binding is set, admission attaches a session-scoped coding workspace so the
	// root run auto-provisions it. Empty leaves the response non-coding — the pre-E09 behaviour.
	RepositoryBindingID string
	RepositoryRef       string
}

// Admission is the committed, replayed, conflicting, or purged admission outcome.
// Purged marks a replay whose cached result has been reaped (spec §20.9): the request
// matched but the resource is a tombstone, so it is answered 410 without re-execution,
// and ResourceTombstone names the original resource so the caller can return its identity.
type Admission struct {
	Body              []byte
	Replayed          bool
	Conflict          bool
	Purged            bool
	ResourceTombstone string
	// SessionNotFound marks a chain onto an unknown or foreign session/response (404, no
	// existence disclosure); SessionConflict a chain onto a non-active session (409). Both
	// are decided before any resource is created, so the transaction leaves nothing behind.
	SessionNotFound bool
	SessionConflict bool
	// ActiveRunConflict marks a chain onto a session that already has a non-terminal root run
	// (409, one-active-root — spec §22.3). The DB partial unique index rejects the second root
	// run insert (23505), so the whole admission rolls back and leaves nothing behind.
	ActiveRunConflict bool
	// RepositoryBindingNotFound marks a `repository` field naming an unknown or foreign binding (404,
	// no existence disclosure). Verified before the idempotency reserve, so a bad binding leaves nothing
	// behind — a coding run never starts against a binding the clone could not resolve (spec §30.1).
	RepositoryBindingNotFound bool
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

	// Resolve the target session: a chained request (session_id / previous_response_id)
	// appends to an existing session, otherwise the minted id opens a new one (spec §9, LP
	// Task 5 minor-a). Resolution is read-only and runs before the idempotency reserve, so an
	// unknown session (404) or a non-active one (409) leaves no idempotency record behind.
	// ponytail: resolve-then-reserve is correct because Task 1 has no runtime session-close
	// racing a replay; T4's one-active-root constraint + close_session command formalize it.
	sessionID := in.SessionID
	createSession := true
	if in.PreviousResponseID != nil || in.RequestedSessionID != nil {
		query, arg := "SessionForCreate", ""
		if in.PreviousResponseID != nil {
			query, arg = "SessionForPreviousResponse", *in.PreviousResponseID
		} else {
			arg = *in.RequestedSessionID
		}
		var existingID, state string
		switch err := tx.QueryRow(ctx, storage.Query(query), arg, tenant.Organization, tenant.Project).
			Scan(&existingID, &state); {
		case errors.Is(err, pgx.ErrNoRows):
			return Admission{SessionNotFound: true}, nil
		case err != nil:
			return Admission{}, fmt.Errorf("resolve chained session: %w", err)
		}
		if state != string(statemachines.SessionActive) {
			return Admission{SessionConflict: true}, nil
		}
		sessionID, createSession = existingID, false
	}

	// Verify a `repository` attachment names a real in-scope binding before reserving idempotency, so a
	// bad or foreign binding_id is a clean 404 here (no idempotency record, no run) rather than a run that
	// fails when the clone cannot resolve the binding (spec §30.1, E09 Task 10).
	if in.RepositoryBindingID != "" {
		switch err := tx.QueryRow(ctx, storage.Query("RepositoryBindingExists"),
			in.RepositoryBindingID, tenant.Organization, tenant.Project).Scan(new(int)); {
		case errors.Is(err, pgx.ErrNoRows):
			return Admission{RepositoryBindingNotFound: true}, nil
		case err != nil:
			return Admission{}, fmt.Errorf("verify repository binding: %w", err)
		}
	}

	// The response body carries the resolved session id (the minted one is only correct for a
	// fresh session), and the idempotency record must store that exact body for replay.
	body := in.Body
	if sessionID != in.SessionID {
		patched, err := withSessionID(in.Body, sessionID)
		if err != nil {
			return Admission{}, err
		}
		body = patched
	}

	err = tx.QueryRow(ctx, storage.Query("ReserveIdempotency"),
		tenant.Organization, tenant.Project, in.Principal, in.Method, in.Route, in.IdempotencyKey, in.RequestHash, body).
		Scan(new(int64))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// The key is already reserved: replay on a matching request, conflict otherwise.
		var storedHash string
		var storedBody []byte
		var resultPurgedAt *time.Time
		var resourceTombstone *string
		if err := tx.QueryRow(ctx, storage.Query("GetIdempotency"),
			tenant.Organization, tenant.Project, in.Principal, in.Method, in.Route, in.IdempotencyKey).
			Scan(&storedHash, &storedBody, &resultPurgedAt, &resourceTombstone); err != nil {
			return Admission{}, fmt.Errorf("read idempotency record: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return Admission{}, fmt.Errorf("commit idempotency read: %w", err)
		}
		if storedHash != in.RequestHash {
			return Admission{Conflict: true}, nil
		}
		// A matching replay whose result has been reaped is a tombstone: 410, no re-run.
		if resultPurgedAt != nil {
			tombstone := ""
			if resourceTombstone != nil {
				tombstone = *resourceTombstone
			}
			return Admission{Purged: true, ResourceTombstone: tombstone}, nil
		}
		return Admission{Body: storedBody, Replayed: true}, nil
	case err != nil:
		return Admission{}, fmt.Errorf("reserve idempotency key: %w", err)
	}

	// Fresh key: create the durable resources and the birth event atomically. A chained
	// response reuses the resolved session (createSession is false); a fresh one opens it.
	if createSession {
		if _, err := tx.Exec(ctx, storage.Query("InsertSession"),
			sessionID, tenant.Organization, tenant.Project); err != nil {
			return Admission{}, fmt.Errorf("insert session: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, storage.Query("InsertResponse"),
		in.ResponseID, tenant.Organization, tenant.Project, sessionID, in.Input, in.Store); err != nil {
		return Admission{}, fmt.Errorf("insert response: %w", err)
	}
	if _, err := tx.Exec(ctx, storage.Query("InsertRun"),
		in.RunID, tenant.Organization, tenant.Project, sessionID, in.ResponseID, nullableJSON(in.Delegations)); err != nil {
		// The session already holds a non-terminal root run: one-active-root (spec §22.3). The
		// partial unique index (runs_one_active_root_per_session) rejects the second root run at
		// the DB, so a concurrent chain loses here rather than in an app-code check-then-insert
		// race. The tx rolls back — no idempotency record, no response, no run persists.
		if isUniqueViolation(err) {
			return Admission{ActiveRunConflict: true}, nil
		}
		return Admission{}, fmt.Errorf("insert run: %w", err)
	}

	// Attach the session's coding workspace when the request carried a repository binding (spec §30.1,
	// E09 Task 10): the session→binding link the root run auto-provisions from. Idempotent per session
	// (WHERE NOT EXISTS), so a chained response reuses the one workspace it already has — edits persist
	// across runs. In the SAME transaction as the run, so the workspace is attached iff the run is.
	// ponytail: WHERE NOT EXISTS is a lock-free read-then-act — two concurrent first-attaches to one
	// session could both insert (no session unique index), and a chained response naming a DIFFERENT
	// binding/ref is silently ignored (a session keeps its ONE workspace). Both are benign here (one
	// active root run per session serializes attaches in practice); a partial unique index on
	// workspaces(session_id) WHERE repository_binding_id<>'' hardens the race if concurrency grows.
	if in.RepositoryBindingID != "" {
		workspaceID, err := newWorkspaceID()
		if err != nil {
			return Admission{}, err
		}
		if _, err := tx.Exec(ctx, storage.Query("AttachSessionWorkspace"),
			workspaceID, tenant.Organization, tenant.Project, sessionID, in.RepositoryBindingID, in.RepositoryRef); err != nil {
			return Admission{}, fmt.Errorf("attach session workspace: %w", err)
		}
	}

	var seq int64
	if err := tx.QueryRow(ctx, storage.Query("AllocateSequence"), sessionID).Scan(&seq); err != nil {
		return Admission{}, fmt.Errorf("allocate session sequence: %w", err)
	}
	eventID, err := newEventID()
	if err != nil {
		return Admission{}, err
	}
	payload := fmt.Sprintf(`{"run_id":%q,"state":"queued"}`, in.RunID)
	if _, err := tx.Exec(ctx, storage.Query("AppendEvent"),
		eventID, tenant.Organization, tenant.Project, sessionID, in.ResponseID, seq, "run.queued.v1", []byte(payload)); err != nil {
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
	return Admission{Body: body}, nil
}

// withSessionID rewrites the session_id of a response projection so a chained response's
// stored and returned body names the resolved existing session, not the minted candidate.
// Every other field is preserved verbatim as raw JSON; only session_id changes.
func withSessionID(body []byte, sessionID string) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, fmt.Errorf("decode response body: %w", err)
	}
	sid, err := json.Marshal(sessionID)
	if err != nil {
		return nil, err
	}
	fields["session_id"] = sid
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("encode response body: %w", err)
	}
	return out, nil
}

// isUniqueViolation reports whether err is a PostgreSQL unique_violation (SQLSTATE 23505),
// so a caller can turn a partial-unique-index rejection into a typed conflict rather than an
// opaque 500 (spec §22.3 one-active-root).
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// nullableText maps an empty string to a SQL NULL, so a session-scoped event journals a
// NULL response_id rather than an empty string.
func nullableText(s string) any {
	if s == "" {
		return nil
	}
	return s
}
