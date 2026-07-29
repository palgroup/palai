package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/storage"
)

// Store is the Postgres-backed registry (migration 000045).
//
// THE SCOPE DECISION, because it is the one thing here a reviewer should push on: Register and
// RecordSeen run SYSTEM-SCOPED, and Get/List run tenant-scoped. The reason is not convenience — it is
// that the runner plane genuinely has no tenant on it yet (§3.6 D8: the enrolment request is
// {runner_id, public_key}, the lease offer carries no org, Dial checks nothing). A system-scoped
// write is the tree's named, greppable escape hatch for exactly this: infrastructure paths that
// legitimately precede a verified tenant. What keeps it honest is that the tenant these writes use is
// NEVER taken from the wire — it is read off the POOL row, which is why an unknown pool is a refusal
// rather than a default. Get and List are reached through the public API, where the verified bearer
// scope is the only tenant authority, so they publish it and RLS confines them.
type Store struct {
	pool  *pgxpool.Pool
	newID func(prefix string) string
	now   func() time.Time
}

// NewStore builds the registry over the durable spine's pool. newID mints row ids (pass
// middleware.NewID in production); now defaults to time.Now.
func NewStore(pool *pgxpool.Pool, newID func(prefix string) string, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{pool: pool, newID: newID, now: now}
}

// Register mints the runner id, writes the row, and appends the `issued` journal entry in ONE
// transaction. The atomicity is the point: a certificate the journal does not record is a certificate
// no targeted revoke can ever find, which is the gap §3.6 D5 names.
func (s *Store) Register(ctx context.Context, reg Registration) (Runner, error) {
	if reg.PoolID == "" {
		return Runner{}, ErrUnknownPool
	}
	// The id/DNS pairing, checked before anything is written. `runners.runner_dns` is what every later
	// request resolves this row by, so a row whose SAN does not name its own id is a row that is found
	// exactly never — see ErrIdentityMismatch.
	if reg.ID == "" || reg.DNS == "" || !strings.HasPrefix(reg.DNS, reg.ID+".") {
		return Runner{}, ErrIdentityMismatch
	}
	if reg.Capacity <= 0 {
		reg.Capacity = 1
	}
	// System scope: the enrolment carries no tenant, so the pool is what resolves one (see the type
	// comment). Every statement below still names its own predicate.
	ctx = storage.WithSystemScope(ctx)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Runner{}, fmt.Errorf("begin runner registration: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var pool struct {
		id, org, project, posture, os, arch string
		strict                              bool
	}
	err = tx.QueryRow(ctx, storage.Query("ResolveRunnerPool"), reg.PoolID).
		Scan(&pool.id, &pool.org, &pool.project, &pool.posture, &pool.os, &pool.arch, &pool.strict)
	if errors.Is(err, pgx.ErrNoRows) {
		return Runner{}, ErrUnknownPool
	}
	if err != nil {
		return Runner{}, fmt.Errorf("resolve runner pool: %w", err)
	}
	if pool.project == "" {
		// A pool with no project cannot give a row a project_id, and 000045's tenant policy for
		// `runners` narrows on one. Refusing beats writing a row that its own tenant cannot read back.
		return Runner{}, ErrUnknownPool
	}

	// THE POSTURE IS COMPARED, NOT VERIFIED (E24 T2). A pool IS a posture (§1 R1), so a machine that
	// declares a different one is a machine in the wrong pool — and the realistic way that happens is
	// an operator handing a Mac the Linux pool's enrolment key. It is refused at the door, and the
	// refusal is JOURNALLED, because an enrolment that "just fails" leaves an operator with nothing to
	// read. What is NOT checked is whether the declaration is true; see ErrPostureMismatch.
	if !postureMatches(reg.Posture, pool.posture) {
		detail, err := refusalDetail(reg, pool.posture)
		if err != nil {
			return Runner{}, fmt.Errorf("encode refusal detail: %w", err)
		}
		if _, err := tx.Exec(ctx, storage.Query("AppendRunnerEnrollment"),
			s.mintID("renr"), pool.org, pool.project, reg.ID, pool.id, reg.KeyID, "refused", detail); err != nil {
			return Runner{}, fmt.Errorf("append refusal entry: %w", err)
		}
		// Commit the refusal: the record of a machine turned away is the only thing this transaction
		// produced, and rolling it back would make the refusal invisible. Returning the helper's error
		// directly under a deferred Rollback is how a write is silently dropped in this tree — so the
		// commit is named here and its failure is returned in place of the refusal.
		if err := tx.Commit(ctx); err != nil {
			return Runner{}, fmt.Errorf("commit enrollment refusal: %w", err)
		}
		return Runner{}, ErrPostureMismatch
	}

	// The machine inherits the pool's posture: having agreed (or said nothing), what it IS is what the
	// pool is. Its own os/arch are recorded as reported — they are inventory, and T4 is where a
	// placement decision may compare them.
	row := Runner{
		ID: reg.ID, Organization: pool.org, Project: pool.project, PoolID: pool.id,
		Label: reg.Label, DNS: reg.DNS, PublicKeySHA256: reg.PublicKeySHA256,
		State: "active", OS: reg.OS, Arch: reg.Arch, Posture: pool.posture, Capacity: reg.Capacity,
	}
	if row.OS == "" {
		row.OS = pool.os
	}
	if row.Arch == "" {
		row.Arch = pool.arch
	}
	var created time.Time
	if err := tx.QueryRow(ctx, storage.Query("InsertRunner"),
		row.ID, row.Organization, row.Project, row.PoolID, row.Label, row.DNS, row.PublicKeySHA256,
		row.State, row.OS, row.Arch, row.Posture, row.Capacity, nil, reg.KeyID,
	).Scan(&created, &row.EnrolledAt, &row.LastSeenAt); err != nil {
		return Runner{}, fmt.Errorf("insert runner: %w", err)
	}

	// The journal entry. detail carries the label the machine claimed and the identity it was issued —
	// both public — and there is no field here a credential could be put in. key_id is the KEY, never the
	// key's value: an id is what a revocation names.
	detail, err := json.Marshal(map[string]string{"label": reg.Label, "runner_dns": reg.DNS, "public_key_sha256": reg.PublicKeySHA256})
	if err != nil {
		return Runner{}, fmt.Errorf("encode enrollment detail: %w", err)
	}
	if _, err := tx.Exec(ctx, storage.Query("AppendRunnerEnrollment"),
		s.mintID("renr"), row.Organization, row.Project, row.ID, row.PoolID, reg.KeyID, "issued", detail); err != nil {
		return Runner{}, fmt.Errorf("append enrollment entry: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Runner{}, fmt.Errorf("commit runner registration: %w", err)
	}
	return row, nil
}

// RecordSeen advances the liveness stamp for the runner holding dns. It reports found=false rather
// than an error for an unknown DNS: a certificate the registry has no row for is the pre-E24
// deployment's runner reconnecting after an upgrade, and refusing it would break the very
// bit-unchanged rule §2 makes non-negotiable. The gateway treats it as "nothing to record".
func (s *Store) RecordSeen(ctx context.Context, dns string, certNotAfter, at time.Time) (Runner, bool, error) {
	if dns == "" {
		return Runner{}, false, nil
	}
	if at.IsZero() {
		at = s.now()
	}
	var notAfter *time.Time
	if !certNotAfter.IsZero() {
		notAfter = &certNotAfter
	}
	ctx = storage.WithSystemScope(ctx)
	row, err := scanRunner(s.pool.QueryRow(ctx, storage.Query("RecordRunnerSeen"), dns, at, notAfter), false)
	if errors.Is(err, pgx.ErrNoRows) {
		return Runner{}, false, nil
	}
	if err != nil {
		return Runner{}, false, fmt.Errorf("record runner seen: %w", err)
	}
	return row, true, nil
}

// Get resolves one runner inside the caller's verified scope.
func (s *Store) Get(ctx context.Context, org, project, id string) (Runner, bool, error) {
	ctx = storage.WithTenant(ctx, org, project)
	row, err := scanRunner(s.pool.QueryRow(ctx, storage.Query("GetRunner"), id, org, project), true)
	if errors.Is(err, pgx.ErrNoRows) {
		return Runner{}, false, nil
	}
	if err != nil {
		return Runner{}, false, fmt.Errorf("get runner: %w", err)
	}
	return row, true, nil
}

// List returns the tenant-scoped keyset page, newest first.
func (s *Store) List(ctx context.Context, org, project string, window ListWindow) ([]Runner, error) {
	if window.Limit <= 0 {
		window.Limit = 21
	}
	ctx = storage.WithTenant(ctx, org, project)
	rows, err := s.pool.Query(ctx, storage.Query("ListRunners"),
		org, project, window.CreatedGTE, window.CreatedLTE, window.AfterCreatedAt, window.AfterID, window.Limit)
	if err != nil {
		return nil, fmt.Errorf("list runners: %w", err)
	}
	defer rows.Close()
	out := []Runner{}
	for rows.Next() {
		row, err := scanRunner(rows, true)
		if err != nil {
			return nil, fmt.Errorf("scan runner: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate runners: %w", err)
	}
	return out, nil
}

func (s *Store) mintID(prefix string) string {
	if s.newID != nil {
		return s.newID(prefix)
	}
	return fmt.Sprintf("%s_%d", prefix, s.now().UnixNano())
}

// scanRow is the shape both pgx.Row and pgx.Rows satisfy, so one scanner serves the single-row reads
// and the list.
type scanRow interface{ Scan(dest ...any) error }

// scanRunner reads a registry row. withCreatedAt distinguishes the two projections: the reads that
// page carry created_at (the keyset coordinate); the UPDATE ... RETURNING does not.
func scanRunner(row scanRow, withCreatedAt bool) (Runner, error) {
	var r Runner
	var certNotAfter, lastSeen *time.Time
	dest := []any{
		&r.ID, &r.Organization, &r.Project, &r.PoolID, &r.Label, &r.DNS, &r.PublicKeySHA256,
		&r.State, &r.OS, &r.Arch, &r.Posture, &r.Capacity, &certNotAfter, &r.EnrolledAt, &lastSeen,
	}
	if withCreatedAt {
		dest = append(dest, &r.CreatedAt)
	}
	if err := row.Scan(dest...); err != nil {
		return Runner{}, err
	}
	if certNotAfter != nil {
		r.CertNotAfter = *certNotAfter
	}
	if lastSeen != nil {
		r.LastSeenAt = *lastSeen
	}
	return r, nil
}
