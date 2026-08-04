//go:build component

package fleet_test

// WHO MAY OPEN THE WAITING ROOM (E24 T6), against a REAL Postgres.
//
// The approval path is separate from E23's throat and the approver POLICY is not (see fleet/strict.go for
// why): `config_policy.approvers` is evaluated by the one function that evaluates it everywhere else,
// coordinator.ConfigPolicy.ApproverAllowed. So what has to be proved here is that this path actually asks
// it, that it asks about the RIGHT project, and that E23 T2's asymmetry survives — no list configured means
// bit-unchanged behaviour, and a list means deny by default.
//
// This package's component leg runs with NO -run filter (scripts/test/component's postgres suite gives
// ./apps/control-plane/internal/fleet a leg of its own), so a new test here runs by construction rather
// than by remembering to name it.

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/apps/control-plane/internal/fleet"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/storage"
)

// strictPoolFixture seeds a tenant whose pool demands a human, and enrols one machine into it.
func strictPoolFixture(t *testing.T, registry *fleet.Store, poolID string) fleet.Runner {
	t.Helper()
	row, err := registry.Register(context.Background(), enrollment(poolID, "waiting-mac"))
	if err != nil {
		t.Fatalf("enrol into a strict pool: %v", err)
	}
	if row.State != "pending" {
		t.Fatalf("enrolled state = %q, want pending", row.State)
	}
	return row
}

// setApprovers writes the project's approver allow-list through the same JSONB the shipped write path
// writes (identity.configPolicyInput). It is written as a document rather than as a column so the decode
// under proof is the production one.
func setApprovers(t *testing.T, project, document string) {
	t.Helper()
	pool := openPool(t)
	if _, err := pool.Exec(storage.WithSystemScope(context.Background()),
		`UPDATE projects SET config_policy = $2::jsonb WHERE id = $1`, project, document); err != nil {
		t.Fatalf("write config_policy: %v", err)
	}
}

// TestApprovalRefusesAPrincipalOutsideTheProjectsApproverList is the authorization claim, and it is
// two-sided because a path that refused everybody would satisfy the refusal half on its own: the SAME
// machine, the SAME project and the SAME list admit the principal the list names.
//
// THE REFUSAL MUST NOT MOVE THE ROW. That is asserted separately from the error, because a build that
// admitted the machine and then returned an error would look identical to a caller reading only the error —
// and the machine would be taking work.
func TestApprovalRefusesAPrincipalOutsideTheProjectsApproverList(t *testing.T) {
	pool := openPool(t)
	project, poolID := strictTenantFixture(t)
	registry := fleet.NewStore(pool, newID, nil)
	row := strictPoolFixture(t, registry, poolID)
	setApprovers(t, project, `{"approvers":["key:ak_allowed"]}`)

	if _, err := registry.Approve(context.Background(), project, row.ID, "key:ak_stranger"); !errors.Is(err, coordinator.ErrApproverNotAuthorized) {
		t.Fatalf("Approve by a principal outside the list = %v, want ErrApproverNotAuthorized", err)
	}
	if got := stateOf(t, pool, row.ID); got != "pending" {
		t.Fatalf("runners.state = %q after a REFUSED approval, want pending — a refusal that still admits the machine is not a refusal", got)
	}
	// An unidentified caller decides nothing even against a list that happens to carry an empty entry.
	if _, err := registry.Approve(context.Background(), project, row.ID, ""); !errors.Is(err, coordinator.ErrApproverNotAuthorized) {
		t.Fatalf("Approve by an unresolved principal = %v, want ErrApproverNotAuthorized", err)
	}

	admission, err := registry.Approve(context.Background(), project, row.ID, "key:ak_allowed")
	if err != nil || !admission.Found || !admission.Admitted {
		t.Fatalf("Approve by the principal the list names: %+v err=%v", admission, err)
	}
	if got := stateOf(t, pool, row.ID); got != "active" {
		t.Fatalf("runners.state = %q after an authorized approval, want active", got)
	}
}

// TestApprovalWithNoApproverListIsBitUnchanged is E23 T2's asymmetry, restated for this surface because it
// is the half that carries every deployment alive today: a project with NO list (a NULL config_policy, which
// is every project in existence) admits any principal, exactly as it would have before an approver check
// existed anywhere.
func TestApprovalWithNoApproverListIsBitUnchanged(t *testing.T) {
	pool := openPool(t)
	project, poolID := strictTenantFixture(t)
	registry := fleet.NewStore(pool, newID, nil)
	row := strictPoolFixture(t, registry, poolID)

	admission, err := registry.Approve(context.Background(), project, row.ID, "key:ak_anybody")
	if err != nil || !admission.Found || !admission.Admitted {
		t.Fatalf("Approve with no approver list configured: %+v err=%v — an unconfigured deployment must behave as it did", admission, err)
	}
	// A policy that configures OTHER things and no approvers is the same case, and it is the realistic one:
	// `pool` and `default_tools` are both written through the same document.
	other := strictPoolFixture(t, registry, poolID)
	setApprovers(t, project, `{"pool":"`+poolID+`","default_tools":["palai.workspace.shell"]}`)
	if admission, err := registry.Approve(context.Background(), project, other.ID, "key:ak_anybody"); err != nil || !admission.Admitted {
		t.Fatalf("Approve against a policy with no approvers key: %+v err=%v", admission, err)
	}
}

// TestApprovalIsIdempotentAndJournalledOnce pins the repeat. An operator whose request timed out must be
// able to send it again and read the same answer — a 404 would say "the id was wrong" — and the journal must
// not count their confidence as fleet history.
func TestApprovalIsIdempotentAndJournalledOnce(t *testing.T) {
	pool := openPool(t)
	project, poolID := strictTenantFixture(t)
	registry := fleet.NewStore(pool, newID, nil)
	row := strictPoolFixture(t, registry, poolID)

	first, err := registry.Approve(context.Background(), project, row.ID, "key:ak_operator")
	if err != nil || !first.Admitted {
		t.Fatalf("first approve: %+v err=%v", first, err)
	}
	second, err := registry.Approve(context.Background(), project, row.ID, "key:ak_operator")
	if err != nil || !second.Found {
		t.Fatalf("second approve: %+v err=%v — an approve an operator cannot repeat is an approve they cannot confirm", second, err)
	}
	if second.Admitted {
		t.Error("the second approve reports having ADMITTED the machine: it moved nothing, and reporting otherwise would tell the live gateway a transition happened twice")
	}
	var entries int
	if err := pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(*) FROM runner_enrollments WHERE runner_id = $1 AND entry_kind = 'approved'`, row.ID).Scan(&entries); err != nil {
		t.Fatalf("read the enrolment journal: %v", err)
	}
	if entries != 1 {
		t.Fatalf("the journal carries %d `approved` entries, want exactly 1", entries)
	}
}

// TestApprovalCannotBeReachedByAnotherTenantOrByTheLifecycleVerbs is the two ways in that must not exist.
//
// The second is the one worth the test: `resume` is an operator verb with NO approver check on it, so a
// `pending` machine it could move to `active` would be strict enrolment bypassed by a different route — and
// a `cordon` would be worse, because it would ERASE the fact that nobody had admitted the machine and the
// resume after it would then be legitimate. A REVOKE is asserted to still work, because refusing an
// enrolment is exactly what an operator does with a machine they did not order.
func TestApprovalCannotBeReachedByAnotherTenantOrByTheLifecycleVerbs(t *testing.T) {
	pool := openPool(t)
	project, poolID := strictTenantFixture(t)
	registry := fleet.NewStore(pool, newID, nil)
	row := strictPoolFixture(t, registry, poolID)

	otherProject, _ := tenantFixture(t, pool, "sandboxed-linux")
	if admission, err := registry.Approve(context.Background(), otherProject, row.ID, "key:ak_intruder"); err != nil || admission.Found {
		t.Fatalf("another tenant approved machine %s: %+v err=%v", row.ID, admission, err)
	}

	for _, action := range []string{"resume", "cordon"} {
		if _, found, err := registry.SetState(context.Background(), project, row.ID, action); err != nil || found {
			t.Fatalf("SetState(%q) on a PENDING machine: found=%v err=%v — a verb with no approver check on it must not open the waiting room", action, found, err)
		}
		if got := stateOf(t, pool, row.ID); got != "pending" {
			t.Fatalf("runners.state = %q after %s on a pending machine, want pending", got, action)
		}
	}
	if _, found, err := registry.SetState(context.Background(), project, row.ID, "revoke"); err != nil || !found {
		t.Fatalf("revoke a PENDING machine: found=%v err=%v — refusing an enrolment is what an operator does with a machine they did not order", found, err)
	}
	if got := stateOf(t, pool, row.ID); got != "revoked" {
		t.Fatalf("runners.state = %q after revoking a pending machine, want revoked", got)
	}
	// And an approval must not resurrect it.
	if admission, err := registry.Approve(context.Background(), project, row.ID, "key:ak_operator"); err != nil || admission.Found {
		t.Fatalf("a REVOKED machine was approved: %+v err=%v", admission, err)
	}
}

// stateOf reads one machine's durable state.
func stateOf(t *testing.T, pool *pgxpool.Pool, id string) string {
	t.Helper()
	var state string
	if err := pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT state FROM runners WHERE id = $1`, id).Scan(&state); err != nil {
		t.Fatalf("read runner state: %v", err)
	}
	return state
}

// strictTenantFixture is tenantFixture with strict_enrollment on the pool.
func strictTenantFixture(t *testing.T) (project, poolID string) {
	t.Helper()
	pool := openPool(t)
	project, poolID = tenantFixture(t, pool, "sandboxed-linux")
	if _, err := pool.Exec(storage.WithSystemScope(context.Background()),
		`UPDATE runner_pools SET strict_enrollment = true WHERE id = $1`, poolID); err != nil {
		t.Fatalf("make pool %s strict: %v", poolID, err)
	}
	return project, poolID
}
