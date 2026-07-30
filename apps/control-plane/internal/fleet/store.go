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
	//
	// THE POOL DECIDES WHETHER THE MACHINE IS ADMITTED OR ONLY RECORDED (E24 T6). A strict pool writes
	// `pending`, and the certificate is still issued below — that pairing is the whole design: a machine
	// with no certificate could never RENEW, so a machine that waited longer than one certificate lifetime
	// for a human would have to re-enrol, and the operator's approval would be spent on a row nothing can
	// reach. `pending` is what keeps it out of the rendezvous, not the absence of an identity.
	row := Runner{
		ID: reg.ID, Organization: pool.org, Project: pool.project, PoolID: pool.id,
		Label: reg.Label, DNS: reg.DNS, PublicKeySHA256: reg.PublicKeySHA256,
		State: "active", OS: reg.OS, Arch: reg.Arch, Posture: pool.posture, Capacity: reg.Capacity,
	}
	if pool.strict {
		row.State = "pending"
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

// SetState is the DURABLE half of a lifecycle decision (E24 T5): cordon, resume or revoke ONE machine,
// with a revoke appended to the enrolment journal, in one transaction.
//
// WHY DURABLE AT ALL, said once here because it is the whole task: `cordoned` and `revoked` were in-memory
// `atomic.Bool`s on the gateway, so a restart erased a revocation (§3.6 D15) — and `restart: always` plus
// a host reboot replaces that process on a schedule. A decommissioned Mac that comes back into service
// when the control plane is upgraded is not decommissioned.
//
// THE ROW IS WRITTEN BEFORE THE LIVE GATEWAY IS TOLD, and the ordering is the recovery story: a crash
// between the two leaves a machine whose row says revoked and whose session is still up, which the next
// connect resolves (handleConnect adopts the row). The other order would leave a cut session with no
// record — a revocation that a restart undoes, which is the bug this replaces.
//
// A revoke is journalled and a cordon is not, and that is a limit rather than a choice: 000045 R4's
// `entry_kind` CHECK admits ('requested','approved','refused','issued','revoked','renewed'), so there is
// no kind a cordon could be written as, and E24 owns exactly one migration (T1's). A cordon is reversible
// and observable in `state`; a revoke is neither, which is why it is the one that had to be recorded.
//
// NONE OF THE THREE VERBS CAN ADMIT A `pending` MACHINE (E24 T6), and the refusal is in the statement
// rather than here — see SetRunnerState. A cordon or a resume against a machine still in the waiting room
// returns found=false, which the caller renders as the same non-disclosing 404 it renders for a machine
// that is not there; the operator's route for that machine is `approve` (Approve, strict.go) or `revoke`.
func (s *Store) SetState(ctx context.Context, org, project, id, action string) (Runner, bool, error) {
	state, ok := runnerStateFor[action]
	if !ok {
		return Runner{}, false, fmt.Errorf("%w: %q", ErrUnknownLifecycleAction, action)
	}
	ctx = storage.WithTenant(ctx, org, project)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Runner{}, false, fmt.Errorf("begin runner lifecycle write: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	row, err := scanRunner(tx.QueryRow(ctx, storage.Query("SetRunnerState"), id, org, project, state), true)
	if errors.Is(err, pgx.ErrNoRows) {
		// Not this tenant's, not there, or already decommissioned and being asked to move — all of which
		// the caller renders as a 404 that discloses nothing about which.
		return Runner{}, false, nil
	}
	if err != nil {
		return Runner{}, false, fmt.Errorf("set runner state: %w", err)
	}
	// The journal entry for a decommission. The statement itself appends AT MOST ONCE per machine, so a
	// repeated revoke — which has to succeed, or an operator cannot confirm one — records nothing the
	// second time rather than counting their confidence as fleet history.
	if action == "revoke" {
		detail, err := json.Marshal(map[string]string{"runner_dns": row.DNS, "label": row.Label})
		if err != nil {
			return Runner{}, false, fmt.Errorf("encode revocation detail: %w", err)
		}
		if _, err := tx.Exec(ctx, storage.Query("AppendRunnerDecision"),
			s.mintID("renr"), row.Organization, row.Project, row.ID, row.PoolID, "revoked", detail); err != nil {
			return Runner{}, false, fmt.Errorf("append revocation entry: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Runner{}, false, fmt.Errorf("commit runner lifecycle write: %w", err)
	}
	return row, true, nil
}

// runnerStateFor maps the operator's VERB to the durable state it writes. A map rather than a switch so
// an unknown action is a refusal at the door: the action reaches this store from a URL path segment, and
// `runners.state` is CHECK-constrained, so an unmapped verb would otherwise be a 500 from Postgres
// instead of a 400 from the surface that took it.
var runnerStateFor = map[string]string{
	"cordon": "cordoned",
	"resume": "active",
	"revoke": "revoked",
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
