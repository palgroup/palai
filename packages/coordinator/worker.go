package coordinator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/palgroup/palai/storage"
)

// Handler processes one claimed job and returns the result hash to record on
// authoritative completion. A non-nil error routes the job through the retry /
// dead-letter policy. The canonical attempt count lives in the row, never in the
// handler, so a handler must not retry internally.
type Handler func(ctx context.Context, claim Claim, payload []byte) (string, error)

// WorkerConfig configures the durable claim loop. Lease is the fenced lease each
// claim holds; Heartbeat is how often it is renewed and must be well under Lease;
// PollInterval is the idle backoff when the queue is empty; Retry bounds failure
// handling.
type WorkerConfig struct {
	Owner        string
	Lease        time.Duration
	Heartbeat    time.Duration
	PollInterval time.Duration
	Retry        RetryPolicy
}

// Worker runs the durable claim -> heartbeat -> complete/fail loop over the job
// queue. One Worker processes one job at a time; run several for throughput.
type Worker struct {
	store  *Store
	cfg    WorkerConfig
	handle Handler
}

// NewWorker binds a claim loop to a handler.
func NewWorker(store *Store, cfg WorkerConfig, handle Handler) *Worker {
	return &Worker{store: store, cfg: cfg, handle: handle}
}

// ClaimNext leases the oldest ready job across the queue at a strictly higher fence,
// skipping rows locked by peer workers (bounded, database-time eligibility). The job
// queue is coordinator infrastructure, so a claim spans tenants and returns the job's
// own tenant scope for the handler to act within. It records the attempt in the
// ledger in the same transaction as the claim. ErrNoClaimableJob means the queue is
// momentarily empty.
func (s *Store) ClaimNext(ctx context.Context, owner string, lease time.Duration) (Claim, []byte, error) {
	if owner == "" || lease <= 0 {
		return Claim{}, nil, errors.New("owner and positive lease are required")
	}
	// Deliberately cross-tenant: the claim SELECTS the tenant it will work for, so it cannot be
	// scoped by one. The row it returns immediately narrows everything downstream (Worker.process).
	ctx = storage.WithSystemScope(ctx)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Claim{}, nil, fmt.Errorf("begin claim: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var claim Claim
	var payload []byte
	err = tx.QueryRow(ctx, storage.Query("ClaimNextJob"), owner, lease.Milliseconds()).Scan(
		&claim.JobID, &claim.Tenant.Organization, &claim.Tenant.Project,
		&claim.Owner, &claim.Fence, &claim.AttemptCount, &claim.LeaseExpiresAt, &payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return Claim{}, nil, ErrNoClaimableJob
	}
	if err != nil {
		return Claim{}, nil, fmt.Errorf("claim next job: %w", err)
	}
	if _, err := tx.Exec(ctx, storage.Query("RecordJobAttempt"), claim.JobID, claim.Fence, claim.Owner); err != nil {
		return Claim{}, nil, fmt.Errorf("record job attempt: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Claim{}, nil, fmt.Errorf("commit claim: %w", err)
	}
	return claim, payload, nil
}

// Run drives the claim loop until ctx is cancelled. It claims one job, processes it
// under a heartbeat that renews the lease by database time, then completes it
// authoritatively or routes it through the retry / dead-letter policy. A worker
// cancelled mid-job leaves its lease to lapse, so the job is reclaimed at a higher
// fence rather than double-completed. Cancellation returns the context error; any
// other error stops the worker.
func (w *Worker) Run(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		claim, payload, err := w.store.ClaimNext(ctx, w.cfg.Owner, w.cfg.Lease)
		switch {
		case errors.Is(err, ErrNoClaimableJob):
			if err := sleep(ctx, w.cfg.PollInterval); err != nil {
				return err
			}
			continue
		case err != nil:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		if err := w.process(ctx, claim, payload); err != nil {
			return err
		}
	}
}

// process runs one claimed job to a durable outcome. The handler runs under a
// heartbeat goroutine that renews the lease until the handler returns; success
// completes the job authoritatively, failure retries or dead-letters it. If the
// worker's context is cancelled — the worker was killed — process returns without
// completing or failing, leaving the lease to lapse so the job is reclaimed. A stale
// terminal callback (the lease was already lost) is swallowed: the live holder owns
// the outcome.
func (w *Worker) process(ctx context.Context, claim Claim, payload []byte) error {
	hbCtx, stopHeartbeat := context.WithCancel(ctx)
	hbDone := make(chan struct{})
	go func() { defer close(hbDone); w.heartbeat(hbCtx, claim) }()
	defer func() { stopHeartbeat(); <-hbDone }()

	// The queue is cross-tenant infrastructure, but the WORK is not: the handler runs under the
	// claimed job's own tenant, so everything a run touches from here down is scoped by migration
	// 000029's policies exactly as an API request would be. Only the claim/complete/fail statements
	// around it stay system-scoped (see ClaimNext).
	resultHash, handlerErr := w.handle(storage.WithTenant(ctx, claim.Tenant.Organization, claim.Tenant.Project), claim, payload)
	stopHeartbeat()
	<-hbDone

	if ctx.Err() != nil {
		return ctx.Err()
	}
	if handlerErr != nil {
		// A soft requeue (an exact stand-down) is not a failed attempt: requeue WITHOUT consuming the
		// attempt budget, so a standby never dead-letters a run whose live sibling outlasts MaxAttempts
		// (MUST-FIX #2). Any other error routes through the ordinary retry / dead-letter policy.
		if errors.Is(handlerErr, ErrSoftRequeue) {
			if err := w.store.RequeueSoft(ctx, claim); err != nil && !errors.Is(err, ErrStaleFence) {
				return err
			}
			return nil
		}
		if _, err := w.store.Fail(ctx, claim, w.cfg.Retry); err != nil && !errors.Is(err, ErrStaleFence) {
			return err
		}
		return nil
	}
	if err := w.store.Complete(ctx, claim, resultHash); err != nil && !errors.Is(err, ErrStaleFence) {
		return err
	}
	return nil
}

// heartbeat renews the claim's lease every cfg.Heartbeat until its context is
// cancelled. A renewal that finds the fence gone (the job was reclaimed) stops the
// loop: the worker has lost the lease and must stop asserting it.
func (w *Worker) heartbeat(ctx context.Context, claim Claim) {
	interval := w.cfg.Heartbeat
	if interval <= 0 {
		interval = w.cfg.Lease / 3
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := w.store.Heartbeat(ctx, claim, w.cfg.Lease); err != nil {
				return
			}
		}
	}
}

// sleep blocks for d or until ctx is cancelled, whichever comes first.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		d = 50 * time.Millisecond
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
