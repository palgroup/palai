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
	// A MACHINE THAT DECLARES NOTHING STORES NOTHING (Faz A.4 T5). This used to clamp <=0 to 1, and since
	// no enrolment has ever carried a capacity — the handler above set every other field — that clamp was
	// the sole author of every value in the column: 44 machines, one distinct value, on the stack it was
	// measured on. A placement ceiling reading that would have enforced a number nobody chose. Zero now
	// means "undeclared" and is storable (000005_capacity_declaration relaxed the CHECK that refused it),
	// and only a positive number binds. Negative is refused rather than clamped, because a machine asking
	// for a nonsense ceiling is a machine an operator has misconfigured and should hear about.
	if reg.Capacity < 0 {
		return Runner{}, ErrCapacityNotDeclarable
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
		id, project, posture, os, arch, isolation string
		strict                                    bool
	}
	err = tx.QueryRow(ctx, storage.Query("ResolveRunnerPool"), reg.PoolID).
		Scan(&pool.id, &pool.project, &pool.posture, &pool.os, &pool.arch, &pool.strict, &pool.isolation)
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
			s.mintID("renr"), pool.project, reg.ID, pool.id, reg.KeyID, "refused", detail); err != nil {
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

	// THE MEASURED CHECK, and it is the only refusal on this path that is not a comparison of two
	// declarations. A pool that names an isolation_mode is a pool whose sessions need that mechanism; a
	// machine that measured its own modes and does not have it cannot serve them. Refusing HERE rather
	// than at placement is DoD 9's sentence — a machine that cannot execute never appears as ready
	// capacity — and refusing before the INSERT is what keeps that true of the row as well as of the
	// certificate.
	//
	// EMPTY ON EITHER SIDE ADMITS. A pool with no requirement is every pool that exists the day 000007
	// applies, and a machine that measured nothing is every runner built before packages/device. A check
	// that refused either would refuse every machine in every deployment for a mechanism none of them
	// declared.
	if !isolationSatisfied(pool.isolation, reg.IsolationModes) {
		detail, err := json.Marshal(map[string]string{
			"label": reg.Label, "pool_isolation_mode": pool.isolation,
			"machine_isolation_modes": strings.Join(reg.IsolationModes, ","),
		})
		if err != nil {
			return Runner{}, fmt.Errorf("encode refusal detail: %w", err)
		}
		if _, err := tx.Exec(ctx, storage.Query("AppendRunnerEnrollment"),
			s.mintID("renr"), pool.project, reg.ID, pool.id, reg.KeyID, "refused", detail); err != nil {
			return Runner{}, fmt.Errorf("append refusal entry: %w", err)
		}
		// Committed for the reason the posture refusal above is: the record of a machine turned away is
		// the only thing this transaction produced, and rolling it back makes the refusal invisible.
		if err := tx.Commit(ctx); err != nil {
			return Runner{}, fmt.Errorf("commit enrollment refusal: %w", err)
		}
		return Runner{}, ErrIsolationUnsupported
	}

	// ‼️ RECOVERY BEFORE CREATION. A device key the registry already holds in this pool is the SAME
	// machine, and everything below this block is what happens when it is not. Before device keys this
	// branch could not exist: every enrolment carried a fresh keypair, so every restart was a new row.
	recovered, err := s.recover(ctx, tx, pool.project, pool.id, reg)
	if err != nil {
		return Runner{}, err
	}
	if recovered.ID != "" {
		if err := tx.Commit(ctx); err != nil {
			return Runner{}, fmt.Errorf("commit runner recovery: %w", err)
		}
		return recovered, nil
	}

	// A CLAIM THIS FINGERPRINT CANNOT SUPPORT IS REFUSED RATHER THAN TURNED INTO A NEW MACHINE. Reaching
	// here with a non-empty RecoverRunnerID means the fingerprint matched no row and the machine still
	// named an identity — a disk that kept identity.json and lost device.key, or a copied identity file.
	// Minting a new id for it would be the friendlier answer and the wrong one: it hides the fact that
	// the machine believes it is something it cannot prove, and on the copied-file shape it is the exact
	// request an attacker makes.
	if reg.RecoverRunnerID != "" {
		return Runner{}, ErrIdentityNotRecoverable
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
		ID: reg.ID, Project: pool.project, PoolID: pool.id,
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
		row.ID, row.Project, row.PoolID, row.Label, row.DNS, row.PublicKeySHA256,
		row.State, row.OS, row.Arch, row.Posture, row.Capacity, nil, reg.KeyID,
		reg.Version, strings.Join(reg.IsolationModes, ","),
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
		s.mintID("renr"), row.Project, row.ID, row.PoolID, reg.KeyID, "issued", detail); err != nil {
		return Runner{}, fmt.Errorf("append enrollment entry: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Runner{}, fmt.Errorf("commit runner registration: %w", err)
	}
	return row, nil
}

// recover resolves the row this device key already owns in this pool and re-enrols it under its
// existing id, or reports "no such row" as a zero Runner and a nil error.
//
// ‼️ IT RUNS INSIDE THE CALLER'S TRANSACTION. The lookup and the update have to see one another or two
// concurrent enrolments of one key both miss and both insert — and the UNIQUE index 000007 adds is the
// second half of that same guarantee, for the case where the two enrolments reach two replicas.
//
// THE THREE REFUSALS ARE ORDERED, and the order is what makes each of them mean something:
//
//  1. A REVOKED row refuses, before anything is written. A pool key is reusable by design, so without
//     this a decommissioned Mac with the key still on it comes back on its next restart. This is the
//     line that makes "revoke that machine" survive the machine disagreeing.
//  2. A CLAIMED id that is not this row's refuses. The fingerprint decided; the claim is checked against
//     it rather than honoured, which is what stops a copied identity file from being an identity.
//  3. Everything else re-enrols under the existing id, and `state` is deliberately untouched — see
//     RecoverRunner in storage/queries/runners.sql for why a restart must not promote a machine out of a
//     strict pool's waiting room.
func (s *Store) recover(ctx context.Context, tx pgx.Tx, project, poolID string, reg Registration) (Runner, error) {
	if reg.PublicKeySHA256 == "" {
		// A machine with no fingerprint has no durable identity to recover, which is every runner built
		// before packages/device and every control plane wired without a registry-recording issuer. It
		// takes the create path exactly as it always did.
		return Runner{}, nil
	}
	var existing struct{ id, state, dns string }
	err := tx.QueryRow(ctx, storage.Query("ResolveRunnerByFingerprint"), poolID, reg.PublicKeySHA256).
		Scan(&existing.id, &existing.state, &existing.dns)
	if errors.Is(err, pgx.ErrNoRows) {
		return Runner{}, nil
	}
	if err != nil {
		return Runner{}, fmt.Errorf("resolve runner by device key: %w", err)
	}
	if existing.state == "revoked" {
		return Runner{}, ErrRunnerRevoked
	}
	if reg.RecoverRunnerID != "" && reg.RecoverRunnerID != existing.id {
		return Runner{}, ErrIdentityNotRecoverable
	}

	row, err := scanRunner(tx.QueryRow(ctx, storage.Query("RecoverRunner"),
		existing.id, reg.Label, reg.OS, reg.Arch, reg.Version,
		strings.Join(reg.IsolationModes, ","), reg.KeyID), false)
	if err != nil {
		return Runner{}, fmt.Errorf("recover runner: %w", err)
	}

	// The journal entry, with the same fields the `issued` entry carries and a kind that says what
	// happened. 000045 R4's CHECK admits ('requested','approved','refused','issued','revoked','renewed'),
	// so a re-enrolment is journalled as `renewed` — there is no kind for "recovered" and inventing one
	// would take a second migration for a word. What matters is that the event is RECORDED: a machine
	// whose identity was reissued and whose journal says nothing is a certificate no audit can explain.
	detail, err := json.Marshal(map[string]string{
		"label": reg.Label, "runner_dns": existing.dns,
		"public_key_sha256": reg.PublicKeySHA256, "recovered": "device-key",
	})
	if err != nil {
		return Runner{}, fmt.Errorf("encode recovery detail: %w", err)
	}
	if _, err := tx.Exec(ctx, storage.Query("AppendRunnerEnrollment"),
		s.mintID("renr"), project, existing.id, poolID, reg.KeyID, "renewed", detail); err != nil {
		return Runner{}, fmt.Errorf("append recovery entry: %w", err)
	}
	return row, nil
}

// isolationSatisfied reports whether a machine's MEASURED modes cover the pool's requirement.
//
// BOTH EMPTIES ADMIT, and each for its own reason. An empty requirement is every pool that exists the day
// 000007 applies — a pool nobody configured must refuse nobody. An empty measurement is every runner
// built before packages/device: it declared nothing, and a pool that requires nothing has nothing to
// compare it against. The pairing that refuses is the one where the pool asked and the machine measured
// its modes and the mode asked for is not among them.
func isolationSatisfied(required string, measured []string) bool {
	if required == "" || len(measured) == 0 {
		return true
	}
	for _, mode := range measured {
		if mode == required {
			return true
		}
	}
	return false
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
func (s *Store) SetState(ctx context.Context, project, id, action string) (Runner, bool, error) {
	state, ok := runnerStateFor[action]
	if !ok {
		return Runner{}, false, fmt.Errorf("%w: %q", ErrUnknownLifecycleAction, action)
	}
	ctx = storage.WithTenant(ctx, project)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Runner{}, false, fmt.Errorf("begin runner lifecycle write: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	row, err := scanRunner(tx.QueryRow(ctx, storage.Query("SetRunnerState"), id, project, state), true)
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
			s.mintID("renr"), row.Project, row.ID, row.PoolID, "revoked", detail); err != nil {
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
func (s *Store) Get(ctx context.Context, project, id string) (Runner, bool, error) {
	ctx = storage.WithTenant(ctx, project)
	row, err := scanRunner(s.pool.QueryRow(ctx, storage.Query("GetRunner"), id, project), true)
	if errors.Is(err, pgx.ErrNoRows) {
		return Runner{}, false, nil
	}
	if err != nil {
		return Runner{}, false, fmt.Errorf("get runner: %w", err)
	}
	return row, true, nil
}

// List returns the tenant-scoped keyset page, newest first.
func (s *Store) List(ctx context.Context, project string, window ListWindow) ([]Runner, error) {
	if window.Limit <= 0 {
		window.Limit = 21
	}
	ctx = storage.WithTenant(ctx, project)
	rows, err := s.pool.Query(ctx, storage.Query("ListRunners"),
		project, window.CreatedGTE, window.CreatedLTE, window.AfterCreatedAt, window.AfterID, window.Limit)
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
	var certNotAfter, lastSeen, configReportedAt *time.Time
	var configRevision *int64
	var configApplied []byte
	dest := []any{
		&r.ID, &r.Project, &r.PoolID, &r.Label, &r.DNS, &r.PublicKeySHA256,
		&r.State, &r.OS, &r.Arch, &r.Posture, &r.Capacity, &certNotAfter, &r.EnrolledAt, &lastSeen,
		&configRevision, &configApplied, &configReportedAt,
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
	if configRevision != nil {
		r.ConfigRevision = *configRevision
	}
	if configReportedAt != nil {
		r.ConfigReportedAt = *configReportedAt
	}
	// A NULL column stays a nil map, and an empty JSON object becomes an empty non-nil one. The two are
	// different answers — never reported, versus reported that the plane had no document — and collapsing
	// them here would make a machine that has never been heard from indistinguishable from one that is
	// polling happily with nothing to apply.
	if len(configApplied) > 0 {
		if err := json.Unmarshal(configApplied, &r.ConfigApplied); err != nil {
			return Runner{}, fmt.Errorf("decode runner %s configuration report: %w", r.ID, err)
		}
		if r.ConfigApplied == nil {
			r.ConfigApplied = map[string]string{}
		}
	}
	return r, nil
}
