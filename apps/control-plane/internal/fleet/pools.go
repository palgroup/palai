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
	Organization string
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

// ListPools returns the tenant-scoped keyset page of pools, newest first — the read behind
// GET /v1/runner-pools. Reads only: creating and deleting a pool is an operator action T5/T6 own, and
// a surface that can only be read cannot be mis-used to move a fleet.
func (s *Store) ListPools(ctx context.Context, org, project string, window ListWindow) ([]Pool, error) {
	if window.Limit <= 0 {
		window.Limit = 21
	}
	ctx = storage.WithTenant(ctx, org, project)
	rows, err := s.pool.Query(ctx, storage.Query("ListRunnerPools"),
		org, project, window.CreatedGTE, window.CreatedLTE, window.AfterCreatedAt, window.AfterID, window.Limit)
	if err != nil {
		return nil, fmt.Errorf("list runner pools: %w", err)
	}
	defer rows.Close()
	out := []Pool{}
	for rows.Next() {
		var p Pool
		if err := rows.Scan(&p.ID, &p.Organization, &p.Project, &p.Name, &p.Posture, &p.OS, &p.Arch,
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
