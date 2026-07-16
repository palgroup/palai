package coordinator

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNoClaimableJob = errors.New("no claimable job")
	ErrStaleFence     = errors.New("stale job fence")
)

//go:embed schema.sql
var schemaSQL string

type Store struct {
	pool *pgxpool.Pool
}

type Claim struct {
	JobID          string    `json:"job_id"`
	Owner          string    `json:"owner"`
	Fence          int64     `json:"fence"`
	AttemptCount   int       `json:"attempt_count"`
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
}

type JobSnapshot struct {
	JobID        string
	Status       string
	LeaseOwner   string
	Fence        int64
	AttemptCount int
	ResultHash   *string
	OutboxCount  int64
	OutboxFence  *int64
}

func NewStore(ctx context.Context, databaseURL string) (*Store, error) {
	if strings.TrimSpace(databaseURL) == "" {
		return nil, errors.New("database URL is required")
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database URL: %w", err)
	}
	config.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("open database pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (store *Store) Close() {
	store.pool.Close()
}

func (store *Store) ApplySchema(ctx context.Context) error {
	if _, err := store.pool.Exec(ctx, schemaSQL); err != nil {
		return fmt.Errorf("apply coordinator schema: %w", err)
	}
	return nil
}

func (store *Store) Enqueue(ctx context.Context, jobID string) error {
	if strings.TrimSpace(jobID) == "" {
		return errors.New("job ID is required")
	}
	_, err := store.pool.Exec(ctx, `
        INSERT INTO jobs (id, status)
        VALUES ($1, 'queued')
    `, jobID)
	if err != nil {
		return fmt.Errorf("enqueue job: %w", err)
	}
	return nil
}

func (store *Store) Claim(
	ctx context.Context,
	jobID string,
	owner string,
	leaseDuration time.Duration,
) (Claim, error) {
	if strings.TrimSpace(jobID) == "" || strings.TrimSpace(owner) == "" || leaseDuration <= 0 {
		return Claim{}, errors.New("job ID, owner and positive lease duration are required")
	}
	var claim Claim
	err := store.pool.QueryRow(ctx, `
        WITH claimable AS (
            SELECT id
            FROM jobs
            WHERE id = $1
              AND (
                status = 'queued'
                OR (status = 'running' AND lease_expires_at <= clock_timestamp())
              )
            FOR UPDATE SKIP LOCKED
        )
        UPDATE jobs AS job
        SET status = 'running',
            lease_owner = $2,
            lease_expires_at = clock_timestamp() + ($3::bigint * interval '1 millisecond'),
            fence = job.fence + 1,
            attempt_count = job.attempt_count + 1,
            updated_at = clock_timestamp()
        FROM claimable
        WHERE job.id = claimable.id
        RETURNING job.id, job.lease_owner, job.fence, job.attempt_count, job.lease_expires_at
    `, jobID, owner, leaseDuration.Milliseconds()).Scan(
		&claim.JobID,
		&claim.Owner,
		&claim.Fence,
		&claim.AttemptCount,
		&claim.LeaseExpiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Claim{}, ErrNoClaimableJob
	}
	if err != nil {
		return Claim{}, fmt.Errorf("claim job: %w", err)
	}
	return claim, nil
}

func (store *Store) Complete(ctx context.Context, claim Claim, resultHash string) error {
	transaction, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin completion: %w", err)
	}
	defer func() { _ = transaction.Rollback(context.Background()) }()
	if err := stageCompletion(ctx, transaction, claim, resultHash); err != nil {
		return err
	}
	if err := transaction.Commit(ctx); err != nil {
		return fmt.Errorf("commit completion: %w", err)
	}
	return nil
}

func stageCompletion(ctx context.Context, transaction pgx.Tx, claim Claim, resultHash string) error {
	if claim.JobID == "" || claim.Owner == "" || claim.Fence < 1 || resultHash == "" {
		return errors.New("valid claim and result hash are required")
	}
	commandTag, err := transaction.Exec(ctx, `
        UPDATE jobs
        SET status = 'completed',
            lease_owner = NULL,
            lease_expires_at = NULL,
            result_hash = $4,
            updated_at = clock_timestamp()
        WHERE id = $1
          AND fence = $2
          AND lease_owner = $3
          AND status = 'running'
    `, claim.JobID, claim.Fence, claim.Owner, resultHash)
	if err != nil {
		return fmt.Errorf("update completed job: %w", err)
	}
	if commandTag.RowsAffected() != 1 {
		return ErrStaleFence
	}
	_, err = transaction.Exec(ctx, `
        INSERT INTO outbox (job_id, fence, event_type, payload)
        VALUES ($1, $2, 'job.completed', jsonb_build_object('result_hash', $3::text))
    `, claim.JobID, claim.Fence, resultHash)
	if err != nil {
		return fmt.Errorf("insert completion outbox: %w", err)
	}
	return nil
}

func (store *Store) LeaseExpired(ctx context.Context, jobID string) (bool, error) {
	var expired bool
	err := store.pool.QueryRow(ctx, `
        SELECT lease_expires_at IS NOT NULL AND clock_timestamp() >= lease_expires_at
        FROM jobs
        WHERE id = $1
    `, jobID).Scan(&expired)
	if err != nil {
		return false, fmt.Errorf("read lease expiry: %w", err)
	}
	return expired, nil
}

func (store *Store) Snapshot(ctx context.Context, jobID string) (JobSnapshot, error) {
	var snapshot JobSnapshot
	err := store.pool.QueryRow(ctx, `
        SELECT job.id,
               job.status,
               COALESCE(job.lease_owner, ''),
               job.fence,
               job.attempt_count,
               job.result_hash,
               COUNT(event.id),
               MIN(event.fence)
        FROM jobs AS job
        LEFT JOIN outbox AS event ON event.job_id = job.id
        WHERE job.id = $1
        GROUP BY job.id
    `, jobID).Scan(
		&snapshot.JobID,
		&snapshot.Status,
		&snapshot.LeaseOwner,
		&snapshot.Fence,
		&snapshot.AttemptCount,
		&snapshot.ResultHash,
		&snapshot.OutboxCount,
		&snapshot.OutboxFence,
	)
	if err != nil {
		return JobSnapshot{}, fmt.Errorf("read job snapshot: %w", err)
	}
	return snapshot, nil
}
