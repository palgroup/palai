//go:build component

package execution_test

// THE §2 NON-NEGOTIABLE, AGAINST A REAL DATABASE: "TEK RUNNER'LI DEPLOYMENT BİT-DEĞİŞMEZDİR" — a
// runner that declares no pool falls into pool_default and the deployment behaves as it does today.
// §2 says that rule "stops with a test, not a comment", so here is the test.
//
// IT USED TO START AT 000044, AND THAT HALF IS GONE WITH THE CHAIN IT STOOD ON. PALAI_MIGRATE_FAULT_AFTER=44
// stopped the boot runner one migration short, the state an existing single-runner install was in was seeded
// THERE, and the chain then resumed — which made this the UPGRADE proof the plan asked for rather than a
// fresh-install one. The chain was squashed to a two-link baseline on 2026-08-04, so there is no 44 to stop
// at and no intermediate shape for an install to be sitting in.
//
// WHAT IS LOST IS THE UPGRADE LEG, NOT THE RULE. The §2 non-negotiable is about what a runner declaring no
// pool does, and that is asserted below against the REAL bootstrap path. What can no longer be asserted here
// is that an install which predates the fleet tables survives arriving at them with its rows and its key
// intact. That was already only provable while the chain still contained the boundary it crossed; a
// release-to-release upgrade is the operator leg (scripts/test/upgrade-drill.sh), and it is where a claim
// about surviving an upgrade belongs now.
//
// AND IT WAS ALREADY RED BEFORE THE SQUASH, which is worth recording so the next reader does not credit the
// removal with a regression: the post-upgrade assertions read runner_pools.organization_id and runners
// .organization_id, and A.2 Task 6 had dropped that column from every table. Measured on the live stack at
// 19:42 on 2026-08-04, before any of this: zero columns named organization_id at chain head.
//
// Its own database is not tidiness. The first version of this test called ProvisionFirstOrg on the
// SHARED component database, claiming the singleton bootstrap identity — and because every insert in
// that path is ON CONFLICT DO NOTHING, the identity leg's own TestBootstrapFirstOrgResolvable then
// found its key already taken and went red with `VerifyAPIKey(bootstrap key) error = invalid_token`.
// A test that claims a singleton on a shared database breaks whichever leg runs after it. On a private
// database it can drive the REAL bootstrap path instead of an approximation of it.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/fleet"
	"github.com/palgroup/palai/apps/control-plane/internal/identity"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/packages/runner"
	"github.com/palgroup/palai/storage"
)

// freshDatabase creates an empty database on the component server and drops it afterwards. It is the
// same helper tests/component/postgres uses for the E15 T1 resume proof, restated here because that
// package cannot be imported (it is a _test package, and this one is in a different module path).
func freshDatabase(t *testing.T, base string) string {
	t.Helper()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect to create a fresh database: %v", err)
	}
	defer admin.Close(ctx)
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	name := "palai_e24t1_" + hex.EncodeToString(raw[:]) // an injection-free identifier
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create fresh database %s: %v", name, err)
	}
	t.Cleanup(func() {
		dropper, err := pgx.Connect(context.Background(), base)
		if err != nil {
			return
		}
		defer dropper.Close(context.Background())
		_, _ = dropper.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse component URL: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

// columnPresent WAS HERE, and it is gone with its only caller. It checked that the test really was
// standing at head 44 rather than trusting the fault hook's word for it, by looking for the first column
// the next migration adds. With the chain squashed to a baseline there is no intermediate head to stand
// at, so the question it answered has no subject.

func TestBootstrapInstallEnrollsItsRunnerIntoTheDefaultPool(t *testing.T) {
	base := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if base == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run TEST=postgres scripts/test/component")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dbURL := freshDatabase(t, base)

	spine, err := coordinator.Open(context.Background(), dbURL)
	if err != nil {
		t.Fatalf("open the spine: %v", err)
	}
	t.Cleanup(spine.Close)
	if err := spine.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate to the chain head: %v", err)
	}
	sys := storage.WithSystemScope(context.Background())

	// THE INSTALL, THROUGH THE PATH PRODUCTION USES. store.Bootstrap calls exactly this, so the four
	// identity rows and the default pool below are the rows a real first boot writes — not an
	// approximation of them assembled from INSERTs, which is what this test had to do while it stood at a
	// migration older than the bootstrap code.
	bootstrapKey := "e24t1-bootstrap-key"
	if err := identity.New(spine.Pool()).ProvisionFirstTenant(context.Background(), bootstrapKey); err != nil {
		t.Fatalf("provision the first tenant: %v", err)
	}

	// The key resolves through VerifyAPIKey rather than through a row count, because a key that does not
	// authenticate is a missing row whatever the count says.
	scope, err := spine.VerifyAPIKey(context.Background(), bootstrapKey)
	if err != nil || scope.Project != "prj_local" {
		t.Fatalf("the bootstrap key resolved to (%+v, %v), want prj_local", scope, err)
	}

	// A RUN CARRIES NO PLACEMENT UNTIL ONE IS DECIDED. runs.pool_id is nullable and stays NULL here: a
	// default written at insert time would claim a placement decision nobody took.
	const runID = "run_e24t1"
	for _, stmt := range [][]any{
		{`INSERT INTO sessions (id, project_id) VALUES ('ses_e24t1','prj_local')`},
		{`INSERT INTO runs (id, project_id, session_id) VALUES ($1,'prj_local','ses_e24t1')`, runID},
	} {
		if _, err := spine.Pool().Exec(sys, stmt[0].(string), stmt[1:]...); err != nil {
			t.Fatalf("seed the install's session and run (%s): %v", stmt[0], err)
		}
	}
	var runPool *string
	if err := spine.Pool().QueryRow(sys, `SELECT pool_id FROM runs WHERE id = $1`, runID).Scan(&runPool); err != nil {
		t.Fatalf("read the seeded run: %v", err)
	}
	if runPool != nil {
		t.Fatalf("a run was born already placed: pool_id = %q", *runPool)
	}

	// THE BOOTSTRAP TENANT HAS ITS POOL, at today's deployment stated as data. Without it this install's
	// runner has nowhere to enrol.
	var seededPosture, seededProject string
	var strict bool
	if err := spine.Pool().QueryRow(sys,
		`SELECT posture, project_id, strict_enrollment FROM runner_pools WHERE id = $1`,
		fleet.DefaultPoolID).Scan(&seededPosture, &seededProject, &strict); err != nil {
		t.Fatalf("the bootstrap tenant has no default pool: %v", err)
	}
	if seededPosture != "sandboxed-linux" || seededProject != "prj_local" || strict {
		t.Fatalf("the default pool is (%s, %s, strict=%v), want today's deployment stated as data",
			seededPosture, seededProject, strict)
	}

	// AND SO DOES EVERY LATER TENANT. CreateProject runs the same provision transaction
	// ProvisionFirstTenant does (it is what A.2 Task 6 left of CreateOrganization), so a pool arriving
	// with a tenant born here is the same claim for both populations.
	created, err := identity.New(spine.Pool()).CreateProject(sys, middleware.Scope{}, []byte(`{"display_name":"fleet-seed"}`))
	if err != nil {
		t.Fatalf("create a second project: %v", err)
	}
	var born struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created.Body, &born); err != nil || born.ID == "" {
		t.Fatalf("decode the created project: %v (%s)", err, created.Body)
	}
	var bornPools int
	if err := spine.Pool().QueryRow(sys,
		`SELECT count(*) FROM runner_pools WHERE project_id = $1 AND posture = 'sandboxed-linux'`, born.ID).Scan(&bornPools); err != nil {
		t.Fatalf("count the new tenant's pools: %v", err)
	}
	if bornPools != 1 {
		t.Fatalf("a tenant born through identity.provision has %d default pool(s), want 1 — a tenant with no pool is a tenant whose runner cannot enrol", bornPools)
	}

	f := newGatewayFixture(t, newOneUseTokens("bit-unchanged-token"))
	f.gateway.SetRegistry(fleet.NewStore(spine.Pool(), middleware.NewID, nil))

	// A runner with today's compose environment: it declares a name and NOTHING else. No pool, no
	// posture, no tenant — because none of those exist on the enrollment wire.
	identityOut, err := runner.Enroll(ctx, f.bootstrap("bit-unchanged-token"))
	if err != nil {
		t.Fatalf("today's runner could not enroll against the registry-backed gateway: %v", err)
	}
	if identityOut.Certificate.Leaf == nil || !identityOut.NotAfter.After(time.Now()) {
		t.Fatalf("enrollment returned no usable identity: %+v", identityOut)
	}

	// It landed in the seeded pool, under the bootstrap tenant, with the pool's posture — the state
	// that makes every later placement decision (T2/T4) resolvable without an operator configuring
	// anything on an existing install.
	var poolID, project, posture, label string
	if err := spine.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT pool_id, project_id, posture, label FROM runners WHERE id = $1`,
		identityOut.RunnerID).Scan(&poolID, &project, &posture, &label); err != nil {
		t.Fatalf("the enrolled runner has no registry row: %v", err)
	}
	if poolID != fleet.DefaultPoolID {
		t.Fatalf("a runner that declared no pool landed in %q, want %q", poolID, fleet.DefaultPoolID)
	}
	if project != "prj_local" {
		t.Fatalf("the runner's tenant = %q, want the bootstrap tenant prj_local", project)
	}
	if posture != "sandboxed-linux" {
		t.Fatalf("the default pool's posture = %q, want sandboxed-linux — today's container runner", posture)
	}
	// The name the machine chose survives as the operator-facing label, and it is NOT the identity.
	if label != gwRunnerID {
		t.Fatalf("the enrolling machine's own name was not recorded as a label: %q", label)
	}
	if identityOut.RunnerID == gwRunnerID {
		t.Fatalf("the runner came away holding the name it chose (%q) rather than the server's id", identityOut.RunnerID)
	}

	// AND IT KEEPS WORKING: renewal rolls the identity forward over mTLS, exactly as before, and the
	// registry's liveness stamp advances rather than the renew failing on a row it cannot match.
	renewed, err := runner.Renew(ctx, identityOut, f.renewConfig())
	if err != nil {
		t.Fatalf("renewal over the existing identity failed: %v", err)
	}
	if !renewed.NotAfter.After(identityOut.NotAfter) && !renewed.NotAfter.Equal(identityOut.NotAfter) {
		t.Fatalf("renewal moved the identity BACKWARD: %s -> %s", identityOut.NotAfter, renewed.NotAfter)
	}
	var lastSeen, certNotAfter *time.Time
	if err := spine.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT last_seen_at, cert_not_after FROM runners WHERE id = $1`, identityOut.RunnerID).
		Scan(&lastSeen, &certNotAfter); err != nil {
		t.Fatalf("read the liveness stamp: %v", err)
	}
	if lastSeen == nil || certNotAfter == nil {
		t.Fatalf("after a renew the registry records last_seen=%v cert_not_after=%v, want both set", lastSeen, certNotAfter)
	}
	// The recorded expiry is the certificate the runner now HOLDS, not the one it presented — the same
	// generation rule recordIdentity already documents, now applied to the durable row.
	if !certNotAfter.Truncate(time.Second).Equal(renewed.NotAfter.UTC().Truncate(time.Second)) {
		t.Fatalf("the registry recorded expiry %s, want the certificate the runner now holds (%s)",
			certNotAfter, renewed.NotAfter)
	}
}
