//go:build component

package coordinator

import (
	"context"
	"errors"
	"testing"

	"github.com/palgroup/palai/storage"
)

// A WRITE THAT MATCHED NOTHING MUST NOT REPORT SUCCESS.
//
// This is the more general half of the unwakeable park, and it is the half worth fixing at the store
// rather than at the one call site. `RecordRunPool` is deliberately a no-op for a pool this tenant
// cannot be served by — that part is right, and the statement's own comment explains why (a
// config_policy naming a pool that does not exist is a typo, and a foreign-key abort would kill every
// run in the project). What was wrong is that it then returned `nil`.
//
// So on 2026-08-02 the dial went ahead into a pool it had not recorded, parked the run there, and left
// `runs.pool_id` NULL — the exact column `OldestRunAwaitingCapacity` matches on. Two runs sat `waiting`
// for thirty-one hours, unreachable by any machine joining any pool and uncovered by any reaper.
//
// Fixing only the resolver would leave the next resolver bug producing the same silent NULL, which is
// why the refusal belongs here: a caller cannot forget to check an error it must handle, and it can
// forget to check a string.
//
// THE THIRD CASE IS WHAT KEEPS THIS HONEST. Write-once is a real requirement — a resume must return to
// the SAME pool and must not be re-placed — so a second call for a run that already carries a pool
// writes nothing AND MUST NOT BE AN ERROR. A fix that turned every zero-row update into a failure
// would break resume, so the test asserts all three outcomes.

// runPoolFixtureRun seeds a tenant, optionally that tenant's own pool, and a queued run.
func runPoolFixtureRun(t *testing.T, cs *Store, withOwnPool bool) (tenant Tenant, ownPool, runID string) {
	t.Helper()
	org, project := pinTestID("org"), pinTestID("prj")
	mustExecPin(t, cs, `INSERT INTO organizations (id) VALUES ($1)`, org)
	mustExecPin(t, cs, `INSERT INTO projects (id, organization_id) VALUES ($1,$2)`, project, org)
	if withOwnPool {
		ownPool = pinTestID("pool")
		mustExecPin(t, cs, `INSERT INTO runner_pools (id, organization_id, project_id, name, posture)
		                     VALUES ($1,$2,$3,'default','sandboxed-linux')`, ownPool, org, project)
	}
	session, response := pinTestID("ses"), pinTestID("resp")
	runID = pinTestID("run")
	mustExecPin(t, cs, `INSERT INTO sessions (id, organization_id, project_id) VALUES ($1,$2,$3)`, session, org, project)
	mustExecPin(t, cs, `INSERT INTO responses (id, organization_id, project_id, session_id, state, input)
	                     VALUES ($1,$2,$3,$4,'queued','"hi"'::jsonb)`, response, org, project, session)
	mustExecPin(t, cs, `INSERT INTO runs (id, organization_id, project_id, session_id, response_id, state)
	                     VALUES ($1,$2,$3,$4,$5,'queued')`, runID, org, project, session, response)
	return Tenant{Organization: org, Project: project}, ownPool, runID
}

// TestRecordRunPoolRefusesToBeASilentNoOp is RED for the first case and green for the other two.
func TestRecordRunPoolRefusesToBeASilentNoOp(t *testing.T) {
	cs := failureReasonFixture(t)
	ctx := context.Background()

	// (1) A POOL THIS TENANT CANNOT BE SERVED BY. Another tenant owns it, which is exactly what
	// fleet.ResolvePool's constant fallback hands every project that has configured nothing.
	foreign := Tenant{Organization: pinTestID("org"), Project: pinTestID("prj")}
	foreignPool := pinTestID("pool")
	mustExecPin(t, cs, `INSERT INTO organizations (id) VALUES ($1)`, foreign.Organization)
	mustExecPin(t, cs, `INSERT INTO projects (id, organization_id) VALUES ($1,$2)`, foreign.Project, foreign.Organization)
	mustExecPin(t, cs, `INSERT INTO runner_pools (id, organization_id, project_id, name, posture)
	                     VALUES ($1,$2,$3,'default','sandboxed-linux')`, foreignPool, foreign.Organization, foreign.Project)

	tenant, _, runID := runPoolFixtureRun(t, cs, false)
	switch got, err := cs.RecordRunPool(ctx, tenant, runID, foreignPool); {
	case errors.Is(err, ErrRunPoolNotRecordable):
		// the honest answer
	case err != nil:
		t.Fatalf("RecordRunPool for another tenant's pool returned %v, want ErrRunPoolNotRecordable", err)
	default:
		t.Fatalf("RecordRunPool wrote nothing for a pool this tenant cannot be served by and reported success (pool %q): the caller then dials into it, parks the run there, and leaves runs.pool_id NULL — which is the exact column OldestRunAwaitingCapacity matches on, so nothing can ever wake it", got)
	}
	// And the row is untouched, which is what makes the refusal a refusal rather than a bad write.
	var stored *string
	if err := cs.pool.QueryRow(storage.WithSystemScope(ctx), `SELECT pool_id FROM runs WHERE id = $1`, runID).Scan(&stored); err != nil {
		t.Fatalf("read runs.pool_id: %v", err)
	}
	if stored != nil {
		t.Fatalf("runs.pool_id = %q after a refused placement, want NULL — a refusal that still wrote would be worse than the silence", *stored)
	}

	// (2) A POOL THIS TENANT OWNS records, and returns what it recorded.
	owner, ownPool, ownRun := runPoolFixtureRun(t, cs, true)
	recorded, err := cs.RecordRunPool(ctx, owner, ownRun, ownPool)
	if err != nil {
		t.Fatalf("RecordRunPool for the tenant's own pool: %v", err)
	}
	if recorded != ownPool {
		t.Fatalf("RecordRunPool recorded %q, want the tenant's own pool %q", recorded, ownPool)
	}

	// (3) WRITE-ONCE IS NOT AN ERROR, and this is the half that keeps the refusal from breaking resume.
	// A second call writes nothing — the statement's `pool_id IS NULL` excludes the row — and must
	// report the pool the run already carries, because a resume returns to the SAME pool.
	second := pinTestID("pool")
	mustExecPin(t, cs, `INSERT INTO runner_pools (id, organization_id, project_id, name, posture)
	                     VALUES ($1,$2,$3,'second','sandboxed-linux')`, second, owner.Organization, owner.Project)
	again, err := cs.RecordRunPool(ctx, owner, ownRun, second)
	if err != nil {
		t.Fatalf("a second RecordRunPool on an already-placed run returned %v, want nil — write-once is a requirement, not a failure", err)
	}
	if again != ownPool {
		t.Fatalf("the second RecordRunPool reported %q, want the pool the run already carries %q — a resume that answered with the pool it was DENIED would re-place the run", again, ownPool)
	}
}
