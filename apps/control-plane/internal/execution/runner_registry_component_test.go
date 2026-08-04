//go:build component

package execution_test

// THE §2 NON-NEGOTIABLE, AGAINST A REAL DATABASE: "TEK RUNNER'LI DEPLOYMENT BİT-DEĞİŞMEZDİR" — a
// runner that declares no pool falls into pool_default and the deployment behaves as it does today.
// §2 says that rule "stops with a test, not a comment", so here is the test.
//
// IT STARTS AT 000044, ON A DATABASE OF ITS OWN, and both halves of that are load-bearing.
//
// Starting at 44 is what makes this the UPGRADE proof the plan asks for rather than a fresh-install
// one: PALAI_MIGRATE_FAULT_AFTER=44 stops the boot runner one migration short, the state an existing
// single-runner install is in is seeded THERE, and then the chain resumes. What survives that step is
// measured, not assumed.
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

// columnPresent is how the test checks it really is standing at head 44 rather than trusting the fault
// hook's word for it: 000045's first column is the one that must NOT be there yet.
func columnPresent(t *testing.T, spine *coordinator.Store, table, column string) bool {
	t.Helper()
	var exists bool
	if err := spine.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		                 WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2)`,
		table, column).Scan(&exists); err != nil {
		t.Fatalf("check %s.%s: %v", table, column, err)
	}
	return exists
}

func TestBootstrapInstallEnrollsItsRunnerIntoTheDefaultPool(t *testing.T) {
	base := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if base == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run TEST=postgres scripts/test/component")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dbURL := freshDatabase(t, base)

	// FIRST BOOT, STOPPED AT 000044 — the release before this task. The chain runs migration by
	// migration in its own transaction, so this leaves a database at exactly the head an existing
	// install is sitting on.
	t.Setenv("PALAI_MIGRATE_FAULT_AFTER", "44")
	stopped, err := coordinator.Open(context.Background(), dbURL)
	if err != nil {
		t.Fatalf("open the 000044 spine: %v", err)
	}
	if err := stopped.Migrate(context.Background()); err == nil {
		t.Fatal("Migrate() reached the head, want the injected stop after 000044")
	}
	sys := storage.WithSystemScope(context.Background())
	var head int
	if err := stopped.Pool().QueryRow(sys, `SELECT coalesce(max(version), 0) FROM schema_migrations`).Scan(&head); err != nil {
		t.Fatalf("read the chain head: %v", err)
	}
	if head != 44 {
		t.Fatalf("chain head = %d, want 44", head)
	}
	if columnPresent(t, stopped, "runner_pools", "posture") {
		t.Fatal("runner_pools.posture exists at head 44; the fault hook did not stop before 000045")
	}
	const runID = "run_e24t1"

	// THE EXISTING INSTALL, at the 44 shape: the four identity rows a bootstrap seeds, plus a session and
	// a run — the rows an upgrade must not disturb, on the table R5 adds a column to.
	//
	// Written as SQL rather than through ProvisionFirstOrg, and the reason is a MEASUREMENT this test
	// made: identity.provision now inserts the default pool in the SAME transaction, naming
	// runner_pools.posture, so calling it at head 44 fails with `column "posture" of relation
	// "runner_pools" does not exist`. That is harmless in production — the boot runner reaches the head
	// before store.Bootstrap runs — but it does mean the bootstrap path is 000045-or-later code, and a
	// test standing at 44 cannot use it. The key hash is minted the way the real path mints it, so the
	// row this seeds is the row that path would have written.
	bootstrapKey := "e24t1-bootstrap-key"
	for _, stmt := range [][]any{
		{`INSERT INTO projects (id) VALUES ('prj_local')`},
		{`INSERT INTO principals (id, project_id, kind) VALUES ('prin_local','prj_local','service')`},
		{`INSERT INTO api_keys (id, project_id, principal_id, key_hash, scopes)
		  VALUES ('key_local','prj_local','prin_local',$1,$2)`, coordinator.HashAPIKey(bootstrapKey), []string{}},
		{`INSERT INTO sessions (id, project_id) VALUES ('ses_e24t1','prj_local')`},
		{`INSERT INTO runs (id, project_id, session_id) VALUES ($1,'prj_local','ses_e24t1')`, runID},
	} {
		if _, err := stopped.Pool().Exec(sys, stmt[0].(string), stmt[1:]...); err != nil {
			t.Fatalf("seed the pre-upgrade install (%s): %v", stmt[0], err)
		}
	}
	var poolsAt44 int
	if err := stopped.Pool().QueryRow(sys, `SELECT count(*) FROM runner_pools`).Scan(&poolsAt44); err != nil {
		t.Fatalf("count pools at head 44: %v", err)
	}
	if poolsAt44 != 0 {
		t.Fatalf("%d runner pool(s) exist at head 44; this install is meant to have none", poolsAt44)
	}
	var runStateBefore string
	if err := stopped.Pool().QueryRow(sys, `SELECT state FROM runs WHERE id = $1`, runID).Scan(&runStateBefore); err != nil {
		t.Fatalf("read the seeded run: %v", err)
	}
	stopped.Close()

	// THE UPGRADE. Same database, no fault, chain resumes to the head.
	t.Setenv("PALAI_MIGRATE_FAULT_AFTER", "")
	spine, err := coordinator.Open(context.Background(), dbURL)
	if err != nil {
		t.Fatalf("open the upgraded spine: %v", err)
	}
	t.Cleanup(spine.Close)
	if err := spine.Migrate(context.Background()); err != nil {
		t.Fatalf("upgrade from 000044: %v", err)
	}

	// NOTHING WAS LOST. The identity rows the install booted with still resolve — through VerifyAPIKey,
	// not through a row count, because a key that no longer authenticates is a lost row whatever the
	// count says — and the run is untouched.
	scope, err := spine.VerifyAPIKey(context.Background(), bootstrapKey)
	if err != nil || scope.Project != "prj_local" {
		t.Fatalf("the pre-upgrade bootstrap key resolved to (%+v, %v) after the upgrade, want prj_local", scope, err)
	}
	var runStateAfter, runPool *string
	if err := spine.Pool().QueryRow(sys, `SELECT state, pool_id FROM runs WHERE id = $1`, runID).Scan(&runStateAfter, &runPool); err != nil {
		t.Fatalf("the pre-upgrade run is gone: %v", err)
	}
	if runStateAfter == nil || *runStateAfter != runStateBefore {
		t.Fatalf("the pre-upgrade run's state changed: %v -> %v", runStateBefore, runStateAfter)
	}
	// R5's column is NULL on it, and that is deliberate: a backfilled default would claim a placement
	// decision that was never taken.
	if runPool != nil {
		t.Fatalf("the upgrade backfilled a placement decision onto an existing run: pool_id = %q", *runPool)
	}

	// R6 SEEDED THE POOL FOR EXACTLY THIS POPULATION — an install that already had org_local/prj_local
	// and is arriving at 000045 for the first time. Without it, this install's runner has nowhere to go.
	var seededPosture, seededOrg, seededProject string
	var strict bool
	if err := spine.Pool().QueryRow(sys,
		`SELECT posture, organization_id, project_id, strict_enrollment FROM runner_pools WHERE id = $1`,
		fleet.DefaultPoolID).Scan(&seededPosture, &seededOrg, &seededProject, &strict); err != nil {
		t.Fatalf("migration 000045 R6 seeded no default pool for the upgraded install: %v", err)
	}
	if seededPosture != "sandboxed-linux" || seededOrg != "org_local" || seededProject != "prj_local" || strict {
		t.Fatalf("the seeded default pool is (%s, %s, %s, strict=%v), want today's deployment stated as data",
			seededPosture, seededOrg, seededProject, strict)
	}

	// R6's OTHER population is identity.provision's — a stack born AFTER 000045, where the migration's
	// guarded seed cannot help because the migrations run before there is any tenant to reference.
	// CreateProject runs the same provision transaction ProvisionFirstTenant does (it is what A.2 Task 6
	// left of CreateOrganization), so a pool arriving with a tenant born here is the same claim for both.
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
	var poolID, org, project, posture, label string
	if err := spine.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT pool_id, organization_id, project_id, posture, label FROM runners WHERE id = $1`,
		identityOut.RunnerID).Scan(&poolID, &org, &project, &posture, &label); err != nil {
		t.Fatalf("the enrolled runner has no registry row: %v", err)
	}
	if poolID != fleet.DefaultPoolID {
		t.Fatalf("a runner that declared no pool landed in %q, want %q", poolID, fleet.DefaultPoolID)
	}
	if org != "org_local" || project != "prj_local" {
		t.Fatalf("the runner's tenant = (%q,%q), want the bootstrap tenant (org_local, prj_local)", org, project)
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
