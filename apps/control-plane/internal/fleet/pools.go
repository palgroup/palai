package fleet

// Pools: what a pool IS, and the one rule enrolment enforces about one.
//
// A POOL IS A POSTURE. `sandboxed-linux` is today's container runner; `unsandboxed-host` is a rented
// Mac. That is why placement is a refusal rather than a preference (§2): an attempt that needs a host
// is not nearly satisfied by a container, so "the nearest free machine" is not a weaker answer, it is
// the wrong machine.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/palgroup/palai/storage"
)

// ErrPostureMismatch is returned by Register when the enrolling machine DECLARES a posture the pool
// does not have.
//
// THE TWO CLAIMS THIS ERROR SITS BETWEEN, kept apart here because the code cannot tell them apart:
//
//   - What is enforced: a machine states a posture, the store compares it with the pool's, and a
//     disagreement refuses the enrolment and is recorded in the journal as `refused`.
//   - What is NOT enforced: that the statement is TRUE. There is no attestation on the enrolment wire,
//     so a machine that lies — or simply says nothing, which every runner built before E24 does —
//     enrols into whatever pool its credential names and runs whatever that host actually is.
//
// So this catches a MISTAKE, not an attack, and the realistic mistake is the one worth catching: an
// operator hands a Mac the Linux pool's enrolment key, and it is refused at the door instead of
// discovered when a run produces the wrong artefact. The unverifiable half is `FLT-P2` in
// docs/operations/known-gaps-1.0.md.
var ErrPostureMismatch = errors.New("fleet: declared posture is not the pool's")

// Pool is one runner pool: the posture its machines have, the shape of machine it expects, and the
// name that is unique within its project.
type Pool struct {
	ID           string
	Project      string
	Name         string
	Posture      string
	OS           string
	Arch         string
	// StrictEnrollment is T6's waiting room: with it set an enrolment needs a human. It is read here so
	// the surface does not have to grow a field later, and NOTHING in E24 T2 acts on it — T6 does.
	StrictEnrollment bool
	CreatedAt        time.Time
}

// postureMatches reports whether a machine's DECLARED posture is compatible with the pool's. An empty
// declaration says nothing and is therefore compatible with everything: that is not leniency, it is
// §2's bit-unchanged rule, because every runner in every deployment today declares nothing.
func postureMatches(declared, pool string) bool { return declared == "" || declared == pool }

// refusalDetail is the journal payload for a refused enrolment: what was declared, what the pool is,
// and the name the machine called itself. Public facts, all three — there is no field here a
// credential could be put in, which is how §2's "the journal writes key_id and never a key value"
// stays true as this grows.
func refusalDetail(reg Registration, poolPosture string) ([]byte, error) {
	return json.Marshal(map[string]string{
		"label":            reg.Label,
		"declared_posture": reg.Posture,
		"pool_posture":     poolPosture,
		"reason":           "posture_mismatch",
	})
}

// Pool resolves one pool by id and reports the tenant it belongs to (E24 T4). It reuses the statement
// Register already resolves an enrolling machine's pool with — one query, one place, so the pool→tenant
// answer cannot diverge between enrolment and placement.
//
// SYSTEM-SCOPED, and the type comment's rule is why that is not a widening: the read publishes no
// tenant because on this plane the POOL is what establishes one, and the caller is the gateway, which
// has no verified tenant to publish. The predicate is the primary key, so there is no ordering
// ambiguity and nothing a stranger supplies.
func (s *Store) Pool(ctx context.Context, poolID string) (Pool, bool, error) {
	if poolID == "" {
		return Pool{}, false, nil
	}
	ctx = storage.WithSystemScope(ctx)
	var p Pool
	err := s.pool.QueryRow(ctx, storage.Query("ResolveRunnerPool"), poolID).
		Scan(&p.ID, &p.Project, &p.Posture, &p.OS, &p.Arch, &p.StrictEnrollment)
	if errors.Is(err, pgx.ErrNoRows) {
		return Pool{}, false, nil
	}
	if err != nil {
		return Pool{}, false, fmt.Errorf("resolve runner pool: %w", err)
	}
	return p, true, nil
}

// ErrPoolNameTaken is returned by CreatePool when this project already has a pool by that name — the
// runner_pools UNIQUE (project_id, name) index surfacing as an answer rather than as a 500. It is a
// typed value rather than a raw pgx error so the handler renders a 409 without parsing a SQLSTATE.
//
// 000045 R1 DECLARED that index as (organization_id, project_id, name); 000065 rebuilt it without the
// organization, and `runner_pools_name_key ON runner_pools USING btree (project_id, name)` is what a
// reader finds in pg_indexes today. Its api-layer twin (api.ErrRunnerPoolNameTaken) already said so.
var ErrPoolNameTaken = errors.New("fleet: a runner pool with that name already exists in this project")

// CreatePool writes ONE operator-created pool and returns it (E28 T1).
//
// WHY THIS EXISTS AT ALL, because it is the measurement this task was written for: before it, the only
// statement that wrote a pool row was InsertDefaultRunnerPool, which writes 'default', 'sandboxed-linux'
// and false as LITERALS, and there was no UPDATE anywhere. So a second pool could not exist, an
// `unsandboxed-host` (rented Mac) pool could not exist, and `strict_enrollment` could not be turned on —
// which left E24 T6's approve route deciding a state no operator could produce.
//
// TENANT-SCOPED, unlike Register: this is reached through the public API, where the verified bearer scope
// is the only tenant authority, so it publishes it and RLS confines it. Register runs system-scoped because
// the enrolment wire genuinely carries no tenant; an operator's request does.
func (s *Store) CreatePool(ctx context.Context, project string, in Pool) (Pool, error) {
	if project == "" {
		// A project-less pool is one nothing can enrol into (Register refuses it), so refusing here keeps a
		// created pool USABLE rather than merely written.
		return Pool{}, ErrUnknownPool
	}
	row := Pool{
		ID: s.mintID("pool"), Project: project, Name: in.Name,
		Posture: in.Posture, OS: in.OS, Arch: in.Arch, StrictEnrollment: in.StrictEnrollment,
	}
	ctx = storage.WithTenant(ctx, project)
	err := s.pool.QueryRow(ctx, storage.Query("InsertRunnerPool"),
		row.ID, row.Project, row.Name, row.Posture, row.OS, row.Arch, row.StrictEnrollment).
		Scan(&row.CreatedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return Pool{}, ErrPoolNameTaken
	}
	if err != nil {
		return Pool{}, fmt.Errorf("insert runner pool: %w", err)
	}
	return row, nil
}

// SetStrictEnrollment opens or closes ONE pool's waiting room (E28 T1) and returns the pool as it now
// stands. found=false is a pool that is not the caller's, or is not there — the same non-disclosing answer
// the machine lifecycle gives, for the same reason.
//
// THE ONLY MUTABLE FIELD, and that is a correctness requirement: Register makes a machine inherit the
// pool's posture, so changing a populated pool's posture would retroactively change what its machines ARE.
func (s *Store) SetStrictEnrollment(ctx context.Context, project, poolID string, strict bool) (Pool, bool, error) {
	if poolID == "" {
		return Pool{}, false, nil
	}
	ctx = storage.WithTenant(ctx, project)
	var p Pool
	err := s.pool.QueryRow(ctx, storage.Query("SetRunnerPoolStrictEnrollment"), project, poolID, strict).
		Scan(&p.ID, &p.Project, &p.Name, &p.Posture, &p.OS, &p.Arch, &p.StrictEnrollment, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Pool{}, false, nil
	}
	if err != nil {
		return Pool{}, false, fmt.Errorf("set runner pool strict enrollment: %w", err)
	}
	return p, true, nil
}

// ListPools returns the tenant-scoped keyset page of pools, newest first — the read behind
// GET /v1/runner-pools.
func (s *Store) ListPools(ctx context.Context, project string, window ListWindow) ([]Pool, error) {
	if window.Limit <= 0 {
		window.Limit = 21
	}
	ctx = storage.WithTenant(ctx, project)
	rows, err := s.pool.Query(ctx, storage.Query("ListRunnerPools"),
		project, window.CreatedGTE, window.CreatedLTE, window.AfterCreatedAt, window.AfterID, window.Limit)
	if err != nil {
		return nil, fmt.Errorf("list runner pools: %w", err)
	}
	defer rows.Close()
	out := []Pool{}
	for rows.Next() {
		var p Pool
		if err := rows.Scan(&p.ID, &p.Project, &p.Name, &p.Posture, &p.OS, &p.Arch,
			&p.StrictEnrollment, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan runner pool: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate runner pools: %w", err)
	}
	return out, nil
}
