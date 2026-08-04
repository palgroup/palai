package coordinator

// MACHINE OCCUPANCY (Faz A.4 T1, migration 000003_lease_occupancy).
//
// An occupancy is the interval in which one session held one machine. `runner_leases` has carried a
// skeleton of it since the baseline and has never had a writer — measured immediately before this file:
//
//	grep -rniE 'INSERT INTO runner_leases' --include='*.go' --include='*.sql' . | grep -v node_modules | wc -l
//	→ 0   (2026-08-04)
//
// This file is that writer. Two things are billed and until now only one of them was measured: model
// tokens settle into usage_ledger, and the MACHINE half had no row anywhere.
//
// A SESSION IS NOT AN OCCUPANCY, which is the whole reason `session_id` had to be added to a table that
// already carried `run_id`. A session is closed for idleness, its machine account is destroyed, and the
// next approval resumes it on whichever machine is free: one session, N holds, and the bill is their SUM.
// Within one hold many runs come and go, so `run_id` cannot key the interval — it stays as it was found.
//
// WHAT THIS FILE DOES NOT DO, stated because a reader will look for it here. It settles nothing into
// usage_ledger (that is the release path's task, with its own dedupe key), it runs no reaper, and it
// enforces no capacity ceiling — nothing below refuses a second occupancy of a machine that is already
// held. Those are the tasks after this one; this one is the durable record they all read and write.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/palgroup/palai/storage"
)

// ReleaseReasonIdle is the one release reason the billed interval branches on: an occupancy the idle
// reaper ended is billed to `last_activity_at`, and the quiet tail after it is not charged. It is a
// constant rather than a string at each call site so that the branch is greppable from both ends — the
// writer here and the `CASE` in storage/queries/leases.sql. The other three reasons ('closed',
// 'preempted', 'lost') need no constant: nothing branches on them, and migration 000003's CHECK is what
// keeps the column's vocabulary closed.
const ReleaseReasonIdle = "idle"

// ErrOccupancyNotFound reports that no occupancy with this id is visible to this tenant. It is distinct
// from a release that wrote nothing because the occupancy was ALREADY released: that one is not an error
// at all (see ReleaseLease), and collapsing the two would make a repeated release indistinguishable from
// an id that was never valid.
var ErrOccupancyNotFound = errors.New("occupancy not found")

// ErrMachineUnavailable reports that AcquireLease wrote no row: the machine or the session is not one
// this tenant can be served by. It is an ERROR rather than an empty id because a caller can ignore a
// string — the lesson ErrRunPoolNotRecordable in this package was written for, where a write that matched
// nothing reported success and two runs sat unreachable for thirty-one hours.
var ErrMachineUnavailable = errors.New("machine is not one this tenant can occupy")

// Occupancy is one row of runner_leases: one session's hold on one machine.
type Occupancy struct {
	ID        string
	RunnerID  string
	SessionID string
	// StartedAt is when the hold began. LastActivityAt is the last moment this session did anything on the
	// machine — the idle reaper reads it to know when to fire, and the bill reads it to know where to stop.
	StartedAt      time.Time
	LastActivityAt time.Time
	// ReleasedAt is the zero time for as long as the hold is open.
	ReleasedAt    time.Time
	ReleaseReason string
	// Billed is the interval this occupancy costs, derived in SQL from the three timestamps above rather
	// than stored. A stored duration would be a second truth that can disagree with the one it came from.
	// For an occupancy that is still open it is the elapsed time so far, which is a statement about a hold
	// in progress and not a settled amount.
	Billed time.Duration
}

func newLeaseID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate lease id: %w", err)
	}
	return "lse_" + hex.EncodeToString(raw[:]), nil
}

// AcquireLease opens an occupancy: this session now holds this machine. The id is minted HERE and never
// taken from a caller, which is the same rule the runner registry applies to a machine's own id.
//
// A machine or a session this tenant cannot see writes NO row and returns ErrMachineUnavailable — the
// statement selects both out of their own tables, so row-level security decides, rather than a predicate
// this package would have to remember to write.
func (s *Store) AcquireLease(ctx context.Context, tenant Tenant, sessionID, runnerID string) (string, error) {
	if sessionID == "" || runnerID == "" {
		return "", errors.New("a session and a machine are required to open an occupancy")
	}
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	leaseID, err := newLeaseID()
	if err != nil {
		return "", err
	}
	var stored string
	switch err := s.pool.QueryRow(ctx, storage.Query("AcquireLease"),
		leaseID, tenant.Project, runnerID, sessionID).Scan(&stored); {
	case errors.Is(err, pgx.ErrNoRows):
		return "", fmt.Errorf("%w: machine %s, session %s", ErrMachineUnavailable, runnerID, sessionID)
	case err != nil:
		return "", fmt.Errorf("acquire lease on %s: %w", runnerID, err)
	}
	return stored, nil
}

// TouchLease records that this session is still using the machine. It is a no-op on an occupancy that has
// already been released, and deliberately not an error: the touch races the reaper by construction, so a
// call that lands just after the release is ordinary rather than exceptional. What it must never do is
// move `last_activity_at` on a closed hold, because that changes a bill that was already settled.
func (s *Store) TouchLease(ctx context.Context, tenant Tenant, leaseID string) error {
	if leaseID == "" {
		return errors.New("an occupancy id is required")
	}
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	if _, err := s.pool.Exec(ctx, storage.Query("TouchLease"), leaseID, tenant.Project); err != nil {
		return fmt.Errorf("touch lease %s: %w", leaseID, err)
	}
	return nil
}

// ReleaseLease closes an occupancy with the reason it closed. The reason decides the bill:
// ReleaseReasonIdle bills to `last_activity_at`, everything else to the release itself.
//
// IT IS WRITE-ONCE AND A REPEAT IS NOT AN ERROR, and the pair is the point. Write-once because moving
// `released_at` forward extends the bill, so a redelivered release must change nothing. Not-an-error
// because a release is repeated by ordinary means — a retried command, a reaper meeting a session that
// closed itself — and turning that into a failure would make callers invent their own suppression.
//
// A no-row update is ambiguous between those two, so the ambiguous case READS the row instead of guessing:
// already released is nil, and no row at all is ErrOccupancyNotFound. It costs one extra round trip on a
// path that runs once per hold.
func (s *Store) ReleaseLease(ctx context.Context, tenant Tenant, leaseID, reason string) error {
	if leaseID == "" || reason == "" {
		return errors.New("an occupancy id and a release reason are required")
	}
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	var releasedAt time.Time
	switch err := s.pool.QueryRow(ctx, storage.Query("ReleaseLease"),
		leaseID, tenant.Project, reason).Scan(&releasedAt); {
	case errors.Is(err, pgx.ErrNoRows):
		if _, rerr := s.Occupancy(ctx, tenant, leaseID); rerr != nil {
			return rerr
		}
		return nil // already released: the first close is the one that counts
	case err != nil:
		return fmt.Errorf("release lease %s: %w", leaseID, err)
	}
	return nil
}

// Occupancy reads one hold, with its billed interval derived by the statement rather than recomputed here.
// The rule lives in SQL so that the list read below and this one cannot drift into two answers.
func (s *Store) Occupancy(ctx context.Context, tenant Tenant, leaseID string) (Occupancy, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	row := s.pool.QueryRow(ctx, storage.Query("GetOccupancy"), leaseID, tenant.Project)
	out, err := scanOccupancy(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Occupancy{}, fmt.Errorf("%w: %s", ErrOccupancyNotFound, leaseID)
	}
	if err != nil {
		return Occupancy{}, fmt.Errorf("read occupancy %s: %w", leaseID, err)
	}
	return out, nil
}

// SessionOccupancies returns every machine this session has held, oldest first. This IS the session's
// machine bill: the sum of these intervals, and never the span from the first hold to the last, which
// would charge the customer for the gaps in which they held nothing.
func (s *Store) SessionOccupancies(ctx context.Context, tenant Tenant, sessionID string) ([]Occupancy, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	rows, err := s.pool.Query(ctx, storage.Query("SessionOccupancies"), tenant.Project, sessionID)
	if err != nil {
		return nil, fmt.Errorf("read occupancies of session %s: %w", sessionID, err)
	}
	defer rows.Close()
	var out []Occupancy
	for rows.Next() {
		occupancy, err := scanOccupancy(rows)
		if err != nil {
			return nil, fmt.Errorf("scan occupancy of session %s: %w", sessionID, err)
		}
		out = append(out, occupancy)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read occupancies of session %s: %w", sessionID, err)
	}
	return out, nil
}

// scanOccupancy is the ONE place the occupancy column list is read, shared by the single read and the list
// read above — two hand-written copies is how they would come to return different shapes of the same row.
// It takes this package's existing scanRow (publication.go), which is what a pgx.Row and a pgx.Rows have in
// common.
func scanOccupancy(row scanRow) (Occupancy, error) {
	var (
		out        Occupancy
		releasedAt *time.Time
		seconds    float64
	)
	if err := row.Scan(&out.ID, &out.RunnerID, &out.SessionID, &out.StartedAt, &out.LastActivityAt,
		&releasedAt, &out.ReleaseReason, &seconds); err != nil {
		return Occupancy{}, err
	}
	if releasedAt != nil {
		out.ReleasedAt = *releasedAt
	}
	out.Billed = time.Duration(seconds * float64(time.Second))
	return out, nil
}
