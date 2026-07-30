//go:build component

package execution_test

// DURABILITY (E24 T5), against a REAL Postgres, the REAL gateway and real machines.
//
// §3.6 D15's second half: `revoked` is an in-memory `atomic.Bool`, so A RESTART ERASES A REVOCATION.
// "Decommission that Mac" survived exactly as long as the process that was told, and `restart: always`
// plus a host reboot means the process is replaced on a schedule (the same measurement §3.6 D6 made
// about the enrolment token's in-memory rate limiter).
//
// A RESTART HERE IS A SECOND GATEWAY ON THE SAME CA AND THE SAME DATABASE, which is exactly what a
// control-plane swap is from a machine's point of view: the certificate it holds is still valid, the
// process that knew anything about it is gone, and the only thing left is the row.
//
// Every test in this file is named `TestLifecycle*` on purpose: scripts/test/component's postgres leg is
// an ALLOW-LIST, and a component test whose name it does not match never runs and reports the same green
// as one that passes. That trap has been sprung in this repository more than once.

import (
	"context"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/storage"
)

// restartControlPlane stands a SECOND gateway up on the fixture's CA and registry — a new process for
// the same fleet. The tokens set is deliberately one no machine will present: a restart must not need a
// re-enrolment to re-learn a decision.
func restartControlPlane(t *testing.T, f *poolKeyFixture) *gatewayFixture {
	t.Helper()
	restarted := newGatewayFixtureWithCA(t, f.ca, newOneUseTokens("restarted-token-never-presented"))
	restarted.gateway.SetRegistry(f.registry)
	restarted.gateway.SetPoolKeys(f.keys)
	return restarted
}

// runnerState reads one machine's durable state.
func runnerState(t *testing.T, f *poolKeyFixture, id string) string {
	t.Helper()
	var state string
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT state FROM runners WHERE id = $1`, id).Scan(&state); err != nil {
		t.Fatalf("read runner state: %v", err)
	}
	return state
}

// TestLifecycleRevocationSurvivesAControlPlaneRestart is RED (2), DURABILITY.
//
// It is two-sided, and the second side is what stops a green-by-refusing-everything: the SAME restarted
// gateway must still admit a machine that was never revoked. A build that refused every connect after a
// restart would satisfy the first assertion and would be a total outage.
func TestLifecycleRevocationSurvivesAControlPlaneRestart(t *testing.T) {
	f := newLifecycleFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tenant, poolID, key := placementTenant(t, f)

	doomed, err := f.enrolAsking(ctx, key, poolID)
	if err != nil {
		t.Fatalf("enrol the machine to be revoked: %v", err)
	}
	spared, err := f.enrolAsking(ctx, key, poolID)
	if err != nil {
		t.Fatalf("enrol the machine that keeps working: %v", err)
	}
	doomedID := runnerIDOf(t, doomed)

	// The operator's decision, through the surface an operator reaches: the store write the API route
	// performs. It has to be DURABLE before it is applied in memory, because the process is about to die.
	if _, found, err := f.registry.SetState(ctx, tenant.Organization, tenant.Project, doomedID, "revoke"); err != nil || !found {
		t.Fatalf("revoke %s: found=%v err=%v", doomedID, found, err)
	}
	if got := runnerState(t, f, doomedID); got != "revoked" {
		t.Fatalf("runners.state = %q after a revoke, want revoked — a revocation that is not a row is a revocation a restart erases", got)
	}

	// THE RESTART. A new process, the same certificates, the same database.
	restarted := restartControlPlane(t, f)

	connectCtx, cancelConnect := context.WithTimeout(ctx, 5*time.Second)
	defer cancelConnect()
	if _, err := restarted.session(doomed).ReceiveLease(connectCtx); err == nil {
		t.Fatal("a REVOKED machine connected to the restarted control plane: `revoked` is an in-memory atomic.Bool, so a restart erases a revocation (§3.6 D15)")
	}

	// The other machine is untouched — a revocation is targeted or it is an outage.
	survivor := parkIdentity(t, restarted, spared)
	waitConnected(t, restarted.gateway, 1)
	attempt := f.attempt("run_restart_ok", "att_restart_ok", 1)
	attempt.Tenant, attempt.PoolID = tenant, poolID
	leaseCtx, cancelLease := context.WithTimeout(ctx, 15*time.Second)
	defer cancelLease()
	ch, err := restarted.gateway.Dial(leaseCtx, attempt)
	if err != nil {
		t.Fatalf("the machine that was NOT revoked could not take a lease after the restart: %v", err)
	}
	defer ch.Close()
	select {
	case <-survivor.lease:
	case <-time.After(15 * time.Second):
		t.Fatal("the spared machine was never offered a lease after the restart")
	}
}

// TestLifecycleCordonSurvivesAControlPlaneRestartAndResumeClearsIt is the reversible half of the same
// durability claim, and it carries the one thing a revoke cannot: a cordoned machine is still a member
// of its pool, so it stays CONNECTED across the restart and simply takes no work — and a resume, which
// is also a row, puts it back.
func TestLifecycleCordonSurvivesAControlPlaneRestartAndResumeClearsIt(t *testing.T) {
	f := newLifecycleFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tenant, poolID, key := placementTenant(t, f)

	identity, err := f.enrolAsking(ctx, key, poolID)
	if err != nil {
		t.Fatalf("enrol: %v", err)
	}
	id := runnerIDOf(t, identity)
	if _, found, err := f.registry.SetState(ctx, tenant.Organization, tenant.Project, id, "cordon"); err != nil || !found {
		t.Fatalf("cordon %s: found=%v err=%v", id, found, err)
	}
	if got := runnerState(t, f, id); got != "cordoned" {
		t.Fatalf("runners.state = %q after a cordon, want cordoned", got)
	}

	restarted := restartControlPlane(t, f)
	machine := parkIdentity(t, restarted, identity)
	waitConnected(t, restarted.gateway, 1)

	attempt := f.attempt("run_restart_cordoned", "att_restart_cordoned", 1)
	attempt.Tenant, attempt.PoolID = tenant, poolID
	starvedCtx, cancelStarved := context.WithTimeout(ctx, 3*time.Second)
	defer cancelStarved()
	if got, err := restarted.gateway.Dial(starvedCtx, attempt); err == nil {
		_ = got.Close()
		t.Fatal("a machine cordoned BEFORE the restart was offered a lease after it: a cordon that a restart forgets is a Mac that comes back into service on its own")
	}
	select {
	case lease := <-machine.lease:
		t.Fatalf("the cordoned machine received lease %s after the restart", lease.LeaseID)
	default:
	}
	if got := restarted.gateway.Connected(); got != 1 {
		t.Fatalf("Connected() = %d, want 1 — a cordoned machine stays connected, which is what makes the cordon reversible", got)
	}

	// The resume is a row too, so it reaches the machine through the same seam the API uses.
	if _, found, err := f.registry.SetState(ctx, tenant.Organization, tenant.Project, id, "resume"); err != nil || !found {
		t.Fatalf("resume %s: found=%v err=%v", id, found, err)
	}
	restarted.gateway.ResumeRunner(id)
	resumeCtx, cancelResume := context.WithTimeout(ctx, 15*time.Second)
	defer cancelResume()
	resumed, err := restarted.gateway.Dial(resumeCtx, f.tenantAttempt(tenant, poolID, "run_restart_resumed", "att_restart_resumed"))
	if err != nil {
		t.Fatalf("Dial after the resume = %v, want a lease", err)
	}
	defer resumed.Close()
	select {
	case <-machine.lease:
	case <-time.After(15 * time.Second):
		t.Fatal("the resumed machine was never offered a lease")
	}
	if got := runnerState(t, f, id); got != "active" {
		t.Fatalf("runners.state = %q after a resume, want active", got)
	}
}

// TestLifecycleRevokeIsIrreversibleAndJournalled pins the two properties a decommission has to have.
// Irreversible: a resume against a revoked machine changes nothing, which is today's in-memory
// semantics (`runner_gateway.go:153`) made durable rather than a new rule. Journalled: the enrolment
// journal is what an operator reads afterwards, and 000045 R4 already has the `revoked` entry kind — a
// revocation nothing records is a decommission nobody can audit.
func TestLifecycleRevokeIsIrreversibleAndJournalled(t *testing.T) {
	f := newLifecycleFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tenant, poolID, key := placementTenant(t, f)

	identity, err := f.enrolAsking(ctx, key, poolID)
	if err != nil {
		t.Fatalf("enrol: %v", err)
	}
	id := runnerIDOf(t, identity)
	if _, found, err := f.registry.SetState(ctx, tenant.Organization, tenant.Project, id, "revoke"); err != nil || !found {
		t.Fatalf("revoke: found=%v err=%v", found, err)
	}
	// Idempotent: a second revoke is not an error and not a 404.
	if _, found, err := f.registry.SetState(ctx, tenant.Organization, tenant.Project, id, "revoke"); err != nil || !found {
		t.Fatalf("a second revoke: found=%v err=%v — a revoke an operator cannot repeat is a revoke they cannot confirm", found, err)
	}
	if _, _, err := f.registry.SetState(ctx, tenant.Organization, tenant.Project, id, "resume"); err != nil {
		t.Fatalf("resume against a revoked machine returned an error: %v", err)
	}
	if got := runnerState(t, f, id); got != "revoked" {
		t.Fatalf("runners.state = %q after resuming a REVOKED machine, want revoked — a revoked runner identity is decommissioned, not paused", got)
	}

	var revocations int
	if err := f.pool.QueryRow(storage.WithSystemScope(ctx),
		`SELECT count(*) FROM runner_enrollments WHERE runner_id = $1 AND entry_kind = 'revoked'`, id).Scan(&revocations); err != nil {
		t.Fatalf("read the enrolment journal: %v", err)
	}
	if revocations != 1 {
		t.Fatalf("the journal carries %d revocation entries for %s, want exactly 1 (idempotent: the second revoke changed nothing, so it recorded nothing)", revocations, id)
	}

	// Another tenant cannot revoke this machine, and the answer is a non-disclosing not-found.
	other, _, _ := placementTenant(t, f)
	if _, found, err := f.registry.SetState(ctx, other.Organization, other.Project, id, "revoke"); err != nil || found {
		t.Fatalf("another tenant revoked machine %s: found=%v err=%v", id, found, err)
	}
}

// TestLifecycleHeartbeatAdvancesTheDurableLivenessStamp is the durable half of the heartbeat. §T5 asks
// for `runners.last_seen_at` to advance on connect, renew AND the heartbeat, because that stamp is what
// tells an operator a machine is still there — and before this task it froze the moment a machine
// finished connecting, however long it then sat parked.
func TestLifecycleHeartbeatAdvancesTheDurableLivenessStamp(t *testing.T) {
	f := newLifecycleFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, poolID, key := placementTenant(t, f)

	identity, err := f.enrolAsking(ctx, key, poolID)
	if err != nil {
		t.Fatalf("enrol: %v", err)
	}
	id := runnerIDOf(t, identity)
	parkIdentity(t, f.gatewayFixture, identity)
	waitConnected(t, f.gateway, 1)

	before := lastSeen(t, f, id)
	// Rewind the stamp so the advance is unambiguous: without this the two reads can land inside one
	// clock tick and a heartbeat that wrote nothing would look identical to one that wrote.
	if _, err := f.pool.Exec(storage.WithSystemScope(ctx),
		`UPDATE runners SET last_seen_at = last_seen_at - interval '1 hour' WHERE id = $1`, id); err != nil {
		t.Fatalf("rewind the liveness stamp: %v", err)
	}
	if alive, cut := f.gateway.Heartbeat(ctx, 5*time.Second); alive != 1 || cut != 0 {
		t.Fatalf("Heartbeat = (%d alive, %d cut), want (1, 0)", alive, cut)
	}
	after := lastSeen(t, f, id)
	if !after.After(before.Add(-time.Second)) {
		t.Fatalf("last_seen_at = %s after a heartbeat, still behind the pre-rewind value %s: the stamp freezes the moment a machine finishes connecting, so a machine parked for an hour looks an hour dead", after, before)
	}
	if got := runnerState(t, f, id); got != "active" {
		t.Fatalf("runners.state = %q after a heartbeat, want active", got)
	}
}

func lastSeen(t *testing.T, f *poolKeyFixture, id string) time.Time {
	t.Helper()
	var at *time.Time
	if err := f.pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT last_seen_at FROM runners WHERE id = $1`, id).Scan(&at); err != nil {
		t.Fatalf("read last_seen_at: %v", err)
	}
	if at == nil {
		t.Fatalf("runner %s has no last_seen_at at all", id)
	}
	return *at
}

// newLifecycleFixture is the placement fixture: a real registry, a real pool-key chain and the capacity
// waker wired exactly as main.go wires it, over a real Postgres.
func newLifecycleFixture(t *testing.T) *poolKeyFixture {
	t.Helper()
	return newPlacementFixture(t)
}

// tenantAttempt is f.attempt with the tenant and pool a PLACED attempt carries.
func (f *poolKeyFixture) tenantAttempt(tenant coordinator.Tenant, poolID, runID, attemptID string) execution.AttemptDescriptor {
	a := f.attempt(runID, attemptID, 1)
	a.Tenant, a.PoolID = tenant, poolID
	return a
}
