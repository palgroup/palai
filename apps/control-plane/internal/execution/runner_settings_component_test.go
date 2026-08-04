//go:build component

package execution_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/apps/control-plane/internal/fleet"
	"github.com/palgroup/palai/apps/control-plane/internal/identity"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/packages/runner"
	"github.com/palgroup/palai/storage"
)

// THE JOURNEY THIS FILE PROVES, end to end and against real everything: a machine ENROLS, an operator
// EDITS its configuration in the panel while it is running, and the machine acts on the edit and says so —
// with no re-enrolment, no restart, and nothing in its environment carrying the value.
//
// WHAT IS REAL HERE, because a proof of delivery assembled out of fakes proves the fakes agree: the
// gateway is execution.RunnerGateway over real TLS with a real CA; the machine is packages/runner's own
// Enroll and Serve; the document is written through store.PutDesiredConfig — the SAME method the panel's
// PUT /v1/deployment/desired reaches — into real Postgres; and the answer the machine receives comes back
// through store.DesiredSettingsForMachine. The only substitution is the engine sandbox, which is not on
// this path.
//
// AND THE ONE TRAP THIS FILE IS WRITTEN AROUND. Measured on the live stack 2026-08-03: the runner
// container holds PALAI_RUNNER_CONCURRENCY=4 in its compose environment, and the pool document asks for 4.
// A test that started a machine and waited for "4" would pass on a machine that had received nothing at
// all, because planeIntDefault falls back to the environment. So every assertion below is a TRANSITION
// from a value the machine WAS started with to one it was not, and the environment is explicitly cleared.

// settingsSpine opens the component tier's Postgres twice: once as the migration/queries spine the gateway
// registry runs on, and once as the api-level store that owns the desired document.
func settingsSpine(t *testing.T) (*coordinator.Store, *store.Store, string) {
	t.Helper()
	base := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if base == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-component TEST=postgres")
	}
	// ITS OWN DATABASE, AND THIS IS NOT TIDINESS — it is the same lesson runner_registry_component_test.go's
	// header records, learned again the same way. This file provisions the bootstrap organization because a
	// machine cannot enrol into a deployment with no tenant, and ProvisionFirstOrg claims a SINGLETON whose
	// every insert is ON CONFLICT DO NOTHING. On the shared component database that silently takes the
	// identity leg's bootstrap key, and TestBootstrapFirstOrgResolvable — which runs AFTER this package in
	// the chain — goes red with `VerifyAPIKey(bootstrap key) error = invalid_token`, naming a test that has
	// nothing to do with fleet configuration. It did exactly that on the first full-suite run of this branch.
	//
	// A test that claims a singleton on a shared database breaks whichever leg runs after it, and the damage
	// is invisible to the selector (`PALAI_SUITE_RUN` runs this package alone, where it passes every time).
	dbURL := freshDatabase(t, base)
	ctx := context.Background()
	spine, err := coordinator.Open(ctx, dbURL)
	if err != nil {
		t.Fatalf("open spine: %v", err)
	}
	t.Cleanup(spine.Close)
	if err := spine.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// A TENANT MUST EXIST BEFORE A MACHINE CAN ENROL, and finding that out is worth recording: without
	// this the enrolment 503s with "enrollment could not be recorded", because the registry has no default
	// pool to place the machine in and refuses to issue a certificate it cannot record. That refusal is the
	// gateway behaving exactly as designed (registry-writes-before-certificate), and it is what a genuinely
	// fresh install looks like before `palai up` provisions the first organization.
	if err := identity.New(spine.Pool()).ProvisionFirstOrg(ctx, "sk-component-settings"); err != nil {
		t.Fatalf("provision the bootstrap organization: %v", err)
	}
	repo, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return spine, repo, dbURL
}

// TestAPanelEditReachesAMachineThatIsAlreadyRunning is the whole requirement in one test.
func TestAPanelEditReachesAMachineThatIsAlreadyRunning(t *testing.T) {
	// THE ENVIRONMENT IS CLEARED FIRST AND THAT IS AN ASSERTION, not hygiene. cmd/runner falls back to
	// PALAI_RUNNER_CONCURRENCY from its own environment when the plane sends nothing, so a machine whose
	// environment carried the number under test could reach it without a byte arriving. Clearing it means
	// every value observed below can only have come down the wire.
	t.Setenv("PALAI_RUNNER_CONCURRENCY", "")

	spine, repo, _ := settingsSpine(t)
	ctx := context.Background()
	sys := storage.WithSystemScope(ctx)

	f := newGatewayFixture(t, newOneUseTokens("settings-token"))
	f.gateway.SetRegistry(fleet.NewStore(spine.Pool(), middleware.NewID, nil))
	f.gateway.SetPoolSettings(repo)

	// --- 1. THE MACHINE ENROLS, and its pool has no document -----------------------------------------
	// It therefore starts on the built-in default of 1. That is the state an operator's Mac is in the
	// moment it joins a fleet nobody has configured yet.
	identity, err := runner.Enroll(ctx, f.bootstrap("settings-token"))
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if got := identity.Settings["PALAI_RUNNER_CONCURRENCY"]; got != "" {
		t.Fatalf("the enrolment answer already carried PALAI_RUNNER_CONCURRENCY=%q. This test's premise is a "+
			"machine that starts UNCONFIGURED, so that what it picks up later can only have come from the edit", got)
	}
	poolID := runnerPoolOf(t, spine, identity.RunnerID)
	// THE ATTEMPT MUST CARRY THE MACHINE'S TENANT AND POOL, and discovering that is itself worth
	// recording: with a registry wired, the enrolled machine parks on a queue keyed by (tenant, pool)
	// read from its REGISTRY ROW, and a Dial with an empty tenant parks on a different key and blocks
	// forever. That is E24's §3.6 D8 fix working — before it, any enrolled machine could take any
	// tenant's attempt — so a test that dialled tenant-less would be measuring a hole rather than a lease.
	tenant := coordinator.Tenant{Project: "prj_local"}

	// The machine serves leases for its lifetime with the settings poll running, exactly as cmd/runner
	// wires it. Backoff and interval are short so the test does not sleep for thirty seconds.
	serveCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runner.ServeConfig{
			Session:     f.session(identity),
			Supervisor:  runner.NewStreamSupervisor(blockingDriver{}),
			Now:         time.Now,
			Log:         t.Logf,
			Backoff:     20 * time.Millisecond,
			Concurrency: 1,
			Settings: func(ctx context.Context, current runner.Identity, report runner.Settings) (runner.Settings, error) {
				return runner.FetchSettings(ctx, current, report, f.settingsConfig())
			},
			SettingsInterval: 50 * time.Millisecond,
		}.Serve(serveCtx)
	}()
	t.Cleanup(func() { cancel(); <-done })

	// --- 2. THE MACHINE REPORTS BEFORE ANYTHING IS WRITTEN --------------------------------------------
	// It is polling and there is no document, so what must land is an EMPTY report rather than no report:
	// "this machine is up and running its own defaults" and "this machine has never been heard from" are
	// different facts and the panel shows them differently.
	waitFor(t, 10*time.Second, "the machine's first report to land", func() bool {
		_, reported, _ := configReport(t, spine, identity.RunnerID)
		return reported
	})
	if revision, _, applied := configReport(t, spine, identity.RunnerID); revision != 0 || len(applied) != 0 {
		t.Fatalf("an unconfigured machine reported revision %d with %d verdicts, want 0 and none: it has been "+
			"sent nothing, and a verdict for a document nobody wrote would make `applied` mean `intends to`",
			revision, len(applied))
	}

	// --- 3. THE PANEL EDIT ----------------------------------------------------------------------------
	// Written through the store method PUT /v1/deployment/desired reaches, with the `provision` capability
	// the route checks — not by inserting a row, which would prove the machine reads a table rather than
	// that the operator's write path arrives.
	//
	// THREE IS THE POINT. The machine was started at 1 and its environment carries nothing, so 3 exists
	// nowhere on that machine and can only arrive from here.
	putDesired(t, repo, ctx, `{"plane":"runner_pool","scope_id":"`+poolID+`","settings":{"PALAI_RUNNER_CONCURRENCY":"3"}}`)

	waitFor(t, 15*time.Second, "the running machine to report the pool edit as applied", func() bool {
		_, _, applied := configReport(t, spine, identity.RunnerID)
		return applied["PALAI_RUNNER_CONCURRENCY"] == runner.VerdictApplied
	})

	// --- 4. AND THE VERDICT IS NOT TAKEN ON TRUST -----------------------------------------------------
	// The machine SAYS it applied 3. What proves it is the gateway's own dial path, which knows nothing
	// about what the machine reported.
	//
	// THE CEILING IS THE ASSERTION AND THE COUNT IS NOT, and this paragraph is here because the first
	// version of this test got it wrong in a way that stayed green under perturbation. It dialled three
	// times and asserted three successes — which passes at ANY concurrency of one or more, because a
	// released slot is immediately reusable. What distinguishes a machine running 3 from a machine running
	// 1 is that the FOURTH lease is refused, so that refusal is what is asserted. A test that counts
	// successes measures that leases work; only the boundary measures the number.
	var channels []interface{ Close() error }
	t.Cleanup(func() {
		for _, ch := range channels {
			_ = ch.Close()
		}
	})
	channels = append(channels, holdLeases(t, f, poolID, tenant, 1, 3,
		"the machine reported PALAI_RUNNER_CONCURRENCY=3 applied but lease %d was never taken. The number "+
			"reached the panel's table and not the park loops, which is a screen reporting a machine did "+
			"something it did not")...)
	if ch, ok := tryDialInPool(t, f, poolID, tenant, 4); ok {
		channels = append(channels, ch)
		t.Fatalf("a FOURTH lease was taken while the document asks for 3. The machine is serving more than it " +
			"was told to, so `applied` names a number that reached the report and not the lease pool")
	}

	// --- 5. THE MACHINE DOCUMENT OVERRIDES ITS POOL'S -------------------------------------------------
	// "This Mac is different from the rest of its pool" is the whole reason the machine scope exists. The
	// pool still says 3; this machine is told 5.
	//
	// THE WAIT IS ON THE REPORT AND NOT ON A DIAL, and that is a correction rather than a preference. An
	// earlier version polled `tryDialInPool` every 25ms for fifteen seconds, which queues six hundred
	// competing waiters on the pool — so when any slot freed, several were served at once and the failure
	// surfaced three steps later as "a SIXTH lease was taken", naming the wrong thing entirely. The
	// machine's own reported revision is a durable, single-valued fact; the ceiling is then measured ONCE,
	// with each lease dialled exactly once.
	poolRevision, _, _ := configReport(t, spine, identity.RunnerID)
	putDesired(t, repo, ctx, `{"plane":"runner_machine","scope_id":"`+identity.RunnerID+`","settings":{"PALAI_RUNNER_CONCURRENCY":"5"}}`)
	waitFor(t, 15*time.Second, "the machine to report the revision its own document produced", func() bool {
		revision, _, _ := configReport(t, spine, identity.RunnerID)
		return revision > poolRevision
	})

	// THE THREE LEASES FROM STEP 4 ARE STILL HELD, which is what makes this mean anything: a fourth and
	// fifth lease can only be taken if this machine's ceiling actually ROSE above its pool's. With the
	// overlay inverted — the pool winning over the machine — the ceiling stays at 3 and lease 4 never lands.
	channels = append(channels, holdLeases(t, f, poolID, tenant, 4, 2,
		"this machine's own document asks for 5 and its pool's asks for 3, but lease %d was refused. The "+
			"machine document did not override its pool's, so configuring one Mac is not expressible")...)
	if extra, ok := tryDialInPool(t, f, poolID, tenant, 6); ok {
		channels = append(channels, extra)
		t.Fatal("a SIXTH lease was taken while this machine's document asks for 5")
	}

	// The POOL document is unchanged by the machine document, which is what makes the overlay an overlay
	// rather than a move: another machine in this pool still gets 3.
	poolDoc, err := repo.GetDesiredConfig(sys, middleware.Scope{Scopes: []string{"provision"}}, "runner_pool", poolID)
	if err != nil || poolDoc == nil {
		t.Fatalf("read the pool document back: %v", err)
	}
	if got := poolDoc.Settings["PALAI_RUNNER_CONCURRENCY"]; got != "3" {
		t.Errorf("the pool document now says %q, want 3 — a machine document that rewrote its pool's would "+
			"reconfigure every other machine in the fleet as a side effect of configuring one", got)
	}
}

// TestAMachineDocumentNamingNoMachineIsRefused — RecordRunPool's lesson on this surface.
//
// A runner id is MINTED BY THE SERVER at enrolment, so an operator cannot know one before the machine
// exists: an id that matches nothing is a typo or a stale copy, every time. Landing it in an append-only
// journal would leave a document nothing can ever resolve and a panel showing a machine as configured.
func TestAMachineDocumentNamingNoMachineIsRefused(t *testing.T) {
	_, repo, _ := settingsSpine(t)
	ctx := context.Background()
	scope := middleware.Scope{Principal: "prin_test", Scopes: []string{"provision"}}

	out, err := repo.PutDesiredConfig(ctx, scope,
		[]byte(`{"plane":"runner_machine","scope_id":"rnr_this_machine_does_not_exist","settings":{"PALAI_RUNNER_CONCURRENCY":"2"}}`))
	if err != nil {
		t.Fatalf("PutDesiredConfig: %v", err)
	}
	if out.MissingField == "" {
		t.Fatal("a machine document naming an id no runner carries was ACCEPTED. A write that matches no row " +
			"must refuse rather than land silently — the row is append-only, so the document would sit in the " +
			"journal forever while the panel showed a machine as configured")
	}
	// The refusal names the id, because an operator who pasted the wrong one needs to see which.
	if !strings.Contains(out.MissingField, "rnr_this_machine_does_not_exist") {
		t.Errorf("the refusal does not name the id that was not found: %q", out.MissingField)
	}
	// And it offers the move that works, rather than only saying no.
	if !strings.Contains(out.MissingField, "POOL") {
		t.Errorf("the refusal does not point at the pool document, which is what an operator configuring a "+
			"machine that has not enrolled yet actually wants: %q", out.MissingField)
	}
}

// putDesired writes one document through the operator's own write path and fails on a refusal.
func putDesired(t *testing.T, repo *store.Store, ctx context.Context, body string) {
	t.Helper()
	out, err := repo.PutDesiredConfig(ctx, middleware.Scope{Principal: "prin_test", Scopes: []string{"provision"}}, []byte(body))
	if err != nil {
		t.Fatalf("write the desired document: %v", err)
	}
	if out.MissingField != "" {
		t.Fatalf("the desired document was refused: %s", out.MissingField)
	}
}

// configReport reads the machine's own answer straight out of its registry row: the revision it resolved,
// whether it has reported at all, and its verdict per setting.
func configReport(t *testing.T, spine *coordinator.Store, runnerID string) (int64, bool, map[string]string) {
	t.Helper()
	var revision *int64
	var applied []byte
	var at *time.Time
	err := spine.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT config_revision, config_applied, config_reported_at FROM runners WHERE id = $1`, runnerID).
		Scan(&revision, &applied, &at)
	if err != nil {
		t.Fatalf("read runner %s's configuration report: %v", runnerID, err)
	}
	if at == nil {
		return 0, false, nil
	}
	verdicts := map[string]string{}
	if len(applied) > 0 {
		if err := json.Unmarshal(applied, &verdicts); err != nil {
			t.Fatalf("decode the report: %v", err)
		}
	}
	var rev int64
	if revision != nil {
		rev = *revision
	}
	return rev, true, verdicts
}

func runnerPoolOf(t *testing.T, spine *coordinator.Store, runnerID string) string {
	t.Helper()
	var poolID string
	if err := spine.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT pool_id FROM runners WHERE id = $1`, runnerID).Scan(&poolID); err != nil {
		t.Fatalf("read runner %s's pool: %v", runnerID, err)
	}
	return poolID
}

// holdLeases takes `count` leases starting at `firstFence` and KEEPS them, returning the channels so the
// caller can close them at the end. Holding is the point: a lease released between calls is a slot the
// next call reuses, so releasing would make any concurrency look like any other.
func holdLeases(t *testing.T, f *gatewayFixture, poolID string, tenant coordinator.Tenant, firstFence, count uint64, failure string) []interface{ Close() error } {
	t.Helper()
	var held []interface{ Close() error }
	for fence := firstFence; fence < firstFence+count; fence++ {
		ch, ok := tryDialInPool(t, f, poolID, tenant, fence)
		if !ok {
			t.Fatalf(failure, fence)
		}
		held = append(held, ch)
	}
	return held
}

// tryDialInPool offers a waiting attempt to the machine on the queue its registry row actually places it
// on. Each call uses a distinct run/attempt id so three concurrent offers are three leases rather than one
// repeated.
func tryDialInPool(t *testing.T, f *gatewayFixture, poolID string, tenant coordinator.Tenant, fence uint64) (execution.EngineChannel, bool) {
	t.Helper()
	// THE LEASE CONTEXT OUTLIVES THIS CALL, and that is the whole reason this helper is not the two-line
	// `tryDial` beside it. A dial context cancelled on return tears its own lease down, so the next dial
	// reuses the slot and EVERY concurrency looks unbounded — which is precisely how the first version of
	// this test passed a perturbation that inverted the overlay. Here the context is cancelled only at
	// test cleanup, so a lease taken stays taken and the ceiling is observable.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	type result struct {
		ch  execution.EngineChannel
		err error
	}
	done := make(chan result, 1)
	go func() {
		ch, err := f.gateway.Dial(ctx, execution.AttemptDescriptor{
			RunID:       contracts.RunID(fmt.Sprintf("run_set_%d", fence)),
			AttemptID:   contracts.AttemptID(fmt.Sprintf("att_set_%d", fence)),
			Fence:       fence,
			Tenant:      tenant,
			PoolID:      poolID,
			ImageDigest: "sha256:" + strings.Repeat("a", 64),
			// LIMITS ARE NOT OPTIONAL AND OMITTING THEM DOES NOT FAIL THE DIAL, which is worth a sentence
			// because it cost this test a wrong green. A descriptor with zero bounds is REFUSED BY THE MACHINE
			// ("all lease resource and output bounds must be positive"), which re-parks the loop — so the
			// gateway's Dial returns a channel, every call looks like a taken lease, and the ceiling is
			// unobservable because no slot is ever occupied. The assertion was measuring offers the gateway
			// handed out, not leases a machine served.
			Limits: gwLimits(),
		})
		done <- result{ch, err}
	}()
	select {
	case r := <-done:
		return r.ch, r.err == nil
	case <-time.After(3 * time.Second):
		// Nothing was leased, so the waiter is abandoned rather than left parked on the pool's queue where
		// it would take the next machine's offer out from under a later assertion.
		cancel()
		return nil, false
	}
}

// waitFor polls a condition to a deadline. The message is written as the thing that did NOT happen, so a
// timeout reads as the defect rather than as "waitFor timed out".
func waitFor(t *testing.T, limit time.Duration, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", limit, what)
}
