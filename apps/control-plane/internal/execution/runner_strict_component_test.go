//go:build component

package execution_test

// STRICT ENROLMENT — THE WAITING ROOM (E24 T6), against a REAL Postgres, the REAL gateway and a real
// machine.
//
// D3 applied AND OFF BY DEFAULT. Waiting for a human per machine in a pool that autoscales cancels the
// autoscale, so `runner_pools.strict_enrollment` defaults to false and every deployment alive today
// behaves exactly as it did — which is the SECOND claim here, and the one that needs a proof of its own
// rather than an assurance.
//
// WHAT `pending` IS, structurally: the machine gets its certificate (otherwise the `renew` path never
// opens and a waiting machine could not survive its own certificate lifetime) and NEVER ENTERS THE
// RENDEZVOUS. That is T5's choice for a cordon and T2's for the pool, made here for the same reason: a
// machine that is not in the queue cannot be reached from a `Dial`, so no later change can soften the
// waiting room into a preference.
//
// Every test here is named `TestStrict*` on purpose: scripts/test/component's postgres leg is an
// ALLOW-LIST, and a component test whose name it does not match never runs and reports the same green as
// one that passes. That trap has been sprung in this repository more than once.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/packages/runner"
	"github.com/palgroup/palai/storage"
)

// strictTenant seeds a tenant whose 'default' pool demands a human. It is placementTenant with one
// column changed, and the column is the whole subject of this file.
func strictTenant(t *testing.T, f *poolKeyFixture, strict bool) (coordinator.Tenant, string, string) {
	t.Helper()
	tenant, poolID, key := placementTenant(t, f)
	if _, err := f.pool.Exec(storage.WithSystemScope(context.Background()),
		`UPDATE runner_pools SET strict_enrollment = $2 WHERE id = $1`, poolID, strict); err != nil {
		t.Fatalf("set strict_enrollment=%v on %s: %v", strict, poolID, err)
	}
	return tenant, poolID, key
}

// approvedEntries counts the machine's `approved` journal entries.
func approvedEntries(t *testing.T, f *poolKeyFixture, id string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(*) FROM runner_enrollments WHERE runner_id = $1 AND entry_kind = 'approved'`, id).Scan(&n); err != nil {
		t.Fatalf("read the enrolment journal: %v", err)
	}
	return n
}

// TestStrictEnrolmentHoldsTheMachineOutOfTheRendezvousUntilAHumanApproves is RED (1).
//
// Four claims in one machine's life, because they are four halves of one behaviour and separating them
// would let a build satisfy each alone: the row says `pending`, the machine HOLDS A CERTIFICATE, a Dial
// on its own tenant and its own pool reaches NOTHING — and the answer is `ErrPoolHasNoRunner`, not a
// timeout, because a pool whose only machine is waiting for a human has no capacity and the run must PARK
// rather than spend the retry ladder in ~2.5 minutes while the human is at lunch.
//
// The fourth is what stops a green-by-refusing-everything: after the approval the SAME machine takes the
// SAME tenant's lease, over the connection it has been holding the whole time.
func TestStrictEnrolmentHoldsTheMachineOutOfTheRendezvousUntilAHumanApproves(t *testing.T) {
	f := newPlacementFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tenant, poolID, key := strictTenant(t, f, true)

	identity, err := f.enrolAsking(ctx, key, poolID)
	if err != nil {
		t.Fatalf("enrol into a strict pool: %v — a strict pool must still ISSUE the certificate, or the renew path never opens for a machine that is waiting", err)
	}
	if identity.Certificate.Leaf == nil {
		t.Fatal("a machine enrolling into a strict pool received no certificate: `renew` authenticates with the certificate the machine holds, so a waiting machine with none can never recover from an expiry")
	}
	id := runnerIDOf(t, identity)
	if got := runnerState(t, f, id); got != "pending" {
		t.Fatalf("runners.state = %q after enrolling into a pool with strict_enrollment=true, want pending", got)
	}
	// AND THE CERTIFICATE IS USABLE, which is the half that makes issuing it the point rather than a
	// concession: renewal authenticates with the certificate the machine already holds, so a machine that
	// waits longer than one certificate lifetime for a human has to be able to renew WHILE PENDING or the
	// approval would eventually land on an identity that can no longer connect. Asserting only that a
	// certificate came back would not have said this.
	renewed, err := runner.Renew(ctx, identity, f.renewConfig())
	if err != nil {
		t.Fatalf("a PENDING machine could not renew: %v — a waiting machine that cannot roll its certificate forward is a machine whose approval expires with it", err)
	}
	if renewed.Certificate.Leaf.SerialNumber.Cmp(identity.Certificate.Leaf.SerialNumber) == 0 {
		t.Fatal("the renewal returned the same certificate, so nothing was re-issued")
	}
	if got := runnerState(t, f, id); got != "pending" {
		t.Fatalf("runners.state = %q after a PENDING machine renewed, want pending — a renewal must not admit anybody", got)
	}

	// The machine connects and holds its session open, exactly as a real runner's park loop does. It is
	// CONNECTED — an operator has to be able to see that the Mac is there — and it takes no work.
	machine := parkIdentity(t, f.gatewayFixture, identity)
	waitConnected(t, f.gateway, 1)

	starved, cancelStarved := context.WithTimeout(ctx, 3*time.Second)
	defer cancelStarved()
	_, err = f.gateway.Dial(starved, f.tenantAttempt(tenant, poolID, "run_strict", "att_strict"))
	if !errors.Is(err, execution.ErrPoolHasNoRunner) {
		t.Fatalf("Dial into a pool whose only machine is PENDING = %v, want ErrPoolHasNoRunner: a machine waiting for a human is absent capacity, so the run must park rather than ride the retry ladder to a dead letter", err)
	}
	select {
	case lease := <-machine.lease:
		t.Fatalf("a PENDING machine was offered lease %s: strict enrolment that a Dial can still reach is not a waiting room", lease.LeaseID)
	default:
	}
	if got := f.gateway.Connected(); got != 1 {
		t.Fatalf("Connected() = %d, want 1 — a pending machine stays connected, which is what lets it be admitted without re-enrolling", got)
	}

	// THE HUMAN. Through the surface an operator reaches: the store write the route performs, with a
	// principal the project's (absent) approver list admits.
	admission, err := f.registry.Approve(ctx, tenant.Project, id, "key:ak_operator")
	if err != nil || !admission.Found || !admission.Admitted {
		t.Fatalf("approve %s: %+v err=%v", id, admission, err)
	}
	f.gateway.ApproveRunner(id)
	if got := runnerState(t, f, id); got != "active" {
		t.Fatalf("runners.state = %q after an approval, want active", got)
	}
	if got := approvedEntries(t, f, id); got != 1 {
		t.Fatalf("the enrolment journal carries %d `approved` entries for %s, want exactly 1 — an admission nothing records is an admission nobody can audit", got, id)
	}

	admitted, cancelAdmitted := context.WithTimeout(ctx, 15*time.Second)
	defer cancelAdmitted()
	ch, err := f.gateway.Dial(admitted, f.tenantAttempt(tenant, poolID, "run_strict_ok", "att_strict_ok"))
	if err != nil {
		t.Fatalf("Dial after the approval = %v, want a lease: an approval that does not reach the machine's own session is an approval that takes effect at the next reboot", err)
	}
	defer ch.Close()
	select {
	case <-machine.lease:
	case <-time.After(15 * time.Second):
		t.Fatal("the approved machine was never offered a lease")
	}
}

// TestStrictOffIsBitUnchanged is RED (2), BIT-UNCHANGEDNESS. With strict_enrollment=false — the default,
// and the value migration 000045 R6 and identity.Store.provision both write — the machine's life is
// EXACTLY T3's and T5's: it enrols `active`, it is never asked about, and it takes a lease with no human
// anywhere in the path.
//
// It runs the same script as the test above so the two are comparable line for line, and it asserts the
// journal carries NO `approved` entry: a build that admitted every machine through the approval path
// would satisfy "it took a lease" and would have moved every existing deployment onto a new code path.
func TestStrictOffIsBitUnchanged(t *testing.T) {
	f := newPlacementFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tenant, poolID, key := strictTenant(t, f, false)

	identity, err := f.enrolAsking(ctx, key, poolID)
	if err != nil {
		t.Fatalf("enrol into a non-strict pool: %v", err)
	}
	id := runnerIDOf(t, identity)
	if got := runnerState(t, f, id); got != "active" {
		t.Fatalf("runners.state = %q on enrolment into a pool with strict_enrollment=false, want active — the default must be bit-unchanged", got)
	}

	machine := parkIdentity(t, f.gatewayFixture, identity)
	waitConnected(t, f.gateway, 1)
	leased, cancelLeased := context.WithTimeout(ctx, 15*time.Second)
	defer cancelLeased()
	ch, err := f.gateway.Dial(leased, f.tenantAttempt(tenant, poolID, "run_default", "att_default"))
	if err != nil {
		t.Fatalf("Dial into a non-strict pool = %v, want a lease with no human in the path", err)
	}
	defer ch.Close()
	select {
	case <-machine.lease:
	case <-time.After(15 * time.Second):
		t.Fatal("a machine in a non-strict pool was never offered a lease: strict mode has changed the default")
	}
	if got := approvedEntries(t, f, id); got != 0 {
		t.Fatalf("the journal carries %d `approved` entries for a machine nobody approved, want 0", got)
	}
}

// TestStrictIsOffOnEveryPoolThisTreeCreates is the other half of "off by default", and it is a claim about
// the SEEDS rather than about a code path: the bootstrap pool migration 000045 R6 writes and every pool
// identity.Store.provision creates must be non-strict, or an upgrade would put a human in front of a fleet
// that was already running.
//
// IT NAMES ROWS RATHER THAN COUNTING THE TABLE. A `count(*) WHERE strict_enrollment` over a shared
// component database would be answered by whatever the other tests in this package had just seeded — the
// every-tenant sweep this repository has shipped twice — so the two claims are made on rows created HERE:
// one that does not mention the column (the UPGRADE population — 000045's ALTER TABLE gave every pool that
// already existed the column's DEFAULT) and one created through the production statement every new
// organization's pool comes from.
//
// The bootstrap pool 000045 R6 seeds is deliberately NOT read: that statement selects from org_local/
// prj_local, which migrations run before, so on a freshly migrated database the row does not exist and an
// assertion on it would have been a test of the fixture.
func TestStrictIsOffOnEveryPoolThisTreeCreates(t *testing.T) {
	f := newPlacementFixture(t)
	ctx := storage.WithSystemScope(context.Background())
	org, project := poolKeyID("org"), poolKeyID("prj")
	upgraded, provisioned := poolKeyID("pool"), poolKeyID("pool")
	for _, stmt := range [][]any{
		{`INSERT INTO organizations (id) VALUES ($1)`, org},
		{`INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, project, org},
		// No strict_enrollment named: the column's DEFAULT is what every pool row that predates 000045 got.
		{`INSERT INTO runner_pools (id, organization_id, project_id, name, posture)
		  VALUES ($1,$2,$3,'upgraded','sandboxed-linux')`, upgraded, org, project},
		{storage.Query("InsertDefaultRunnerPool"), provisioned, org, project},
	} {
		if _, err := f.pool.Exec(ctx, stmt[0].(string), stmt[1:]...); err != nil {
			t.Fatalf("seed a provisioned tenant: %v", err)
		}
	}
	for _, tc := range []struct{ id, why string }{
		{upgraded, "a pool that does not name the column is what an UPGRADE produced: 000045's ALTER TABLE would have put a human in front of a fleet that was already running"},
		{provisioned, "InsertDefaultRunnerPool creates a STRICT pool: every new organization would need a human before its first machine could take work"},
	} {
		var strict bool
		if err := f.pool.QueryRow(ctx, `SELECT strict_enrollment FROM runner_pools WHERE id = $1`, tc.id).Scan(&strict); err != nil {
			t.Fatalf("read pool %s: %v", tc.id, err)
		}
		if strict {
			t.Errorf("pool %s demands a human enrolment approval: %s", tc.id, tc.why)
		}
	}
}
