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
	dbURL := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if dbURL == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-component TEST=postgres")
	}
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
	tenant := coordinator.Tenant{Organization: "org_local", Project: "prj_local"}

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
	// The machine SAYS it applied 3. What proves it is that it now parks three leases at once, which is a
	// property of the gateway's own dial path rather than of anything the machine reports. A claim of
	// `applied` that had not resized is the precise defect the verdict exists to prevent, and asserting the
	// claim alone would assert that the machine says what it says.
	var channels []interface{ Close() error }
	t.Cleanup(func() {
		for _, ch := range channels {
			_ = ch.Close()
		}
	})
	for i := range 3 {
		ch, ok := tryDialInPool(f, poolID, tenant, uint64(i+1))
		if !ok {
			t.Fatalf("the machine reported PALAI_RUNNER_CONCURRENCY=3 applied but lease %d of 3 was never taken. "+
				"The number reached the panel's table and not the park loops, which is a screen reporting a machine "+
				"did something it did not", i+1)
		}
		channels = append(channels, ch)
	}

	// --- 5. THE MACHINE DOCUMENT OVERRIDES ITS POOL'S -------------------------------------------------
	// "This Mac is different from the rest of its pool" is the whole reason the machine scope exists. The
	// pool still says 3; this machine is told 5.
	putDesired(t, repo, ctx, `{"plane":"runner_machine","scope_id":"`+identity.RunnerID+`","settings":{"PALAI_RUNNER_CONCURRENCY":"5"}}`)
	waitFor(t, 15*time.Second, "the machine document to overlay its pool's", func() bool {
		ch, ok := tryDialInPool(f, poolID, tenant, 9)
		if ok {
			channels = append(channels, ch)
		}
		return ok
	})

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

// tryDialInPool offers a waiting attempt to the machine on the queue its registry row actually places it
// on. Each call uses a distinct run/attempt id so three concurrent offers are three leases rather than one
// repeated.
func tryDialInPool(f *gatewayFixture, poolID string, tenant coordinator.Tenant, fence uint64) (execution.EngineChannel, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ch, err := f.gateway.Dial(ctx, execution.AttemptDescriptor{
		RunID:       contracts.RunID(fmt.Sprintf("run_set_%d", fence)),
		AttemptID:   contracts.AttemptID(fmt.Sprintf("att_set_%d", fence)),
		Fence:       fence,
		Tenant:      tenant,
		PoolID:      poolID,
		ImageDigest: "sha256:" + strings.Repeat("a", 64),
	})
	return ch, err == nil
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
