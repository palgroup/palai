//go:build component

package execution_test

// THE §2 NON-NEGOTIABLE, AGAINST A REAL DATABASE: "TEK RUNNER'LI DEPLOYMENT BİT-DEĞİŞMEZDİR" — a
// runner that declares no pool falls into pool_default and the deployment behaves as it does today.
// §2 says that rule "stops with a test, not a comment", so here is the test.
//
// It drives the real pieces end to end with NO pool configuration anywhere: BOTH of R6's seeding
// populations (the migration's, for an install upgrading from 000044; identity.provision's, for a
// tenant born fresh), the production fleet.Store, and the REAL gateway over REAL mTLS against a REAL
// packages/runner enrollment. Nothing the runner sends names a pool — that is the point. The only
// thing that could make it pass is the seed actually existing and the gateway actually resolving it.

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/fleet"
	"github.com/palgroup/palai/apps/control-plane/internal/identity"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/packages/runner"
	"github.com/palgroup/palai/storage"
)

func TestBootstrapInstallEnrollsItsRunnerIntoTheDefaultPool(t *testing.T) {
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run TEST=postgres scripts/test/component")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	spine, err := coordinator.Open(context.Background(), url)
	if err != nil {
		t.Fatalf("open spine: %v", err)
	}
	t.Cleanup(spine.Close)
	if err := spine.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// THE STATE AN INSTALL UPGRADING FROM 000044 IS IN: the bootstrap tenant exists (identity/store.go
	// seeded it on some earlier boot) and there is no pool anywhere, because until 000045 no migration
	// had ever made one.
	//
	// Seeded with plain SQL, and the reason is a bug this test caused and then had to be corrected for:
	// ProvisionFirstOrg would ALSO mint the SINGLETON bootstrap API key, and this package shares one
	// Postgres with every other component leg. The identity leg's own TestBootstrapFirstOrgResolvable
	// provisions that key and verifies it resolves — and because every insert is ON CONFLICT DO NOTHING,
	// whichever leg gets there first wins and the other's key resolves to nothing. Running this leg first
	// turned that test red with `VerifyAPIKey(bootstrap key) error = invalid_token`. A test that claims a
	// singleton on a shared database breaks whichever leg runs after it, so this one claims none: the org
	// and project are all it needs, and the api_keys/principals rows it does NOT write are what leave the
	// identity leg's own bootstrap intact.
	seed := storage.WithSystemScope(context.Background())
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO organizations (id) VALUES ($1) ON CONFLICT DO NOTHING`, []any{"org_local"}},
		{`INSERT INTO projects (id, organization_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, []any{"prj_local", "org_local"}},
	} {
		if _, err := spine.Pool().Exec(seed, stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed the bootstrap tenant: %v", err)
		}
	}
	// Re-run the chain. R6 seeds pool_default for exactly this population — an install that already has
	// org_local/prj_local and is arriving at 000045 for the first time.
	if err := spine.Migrate(context.Background()); err != nil {
		t.Fatalf("re-migrate onto the seeded bootstrap tenant: %v", err)
	}
	var seededPosture, seededOrg, seededProject string
	var strict bool
	if err := spine.Pool().QueryRow(seed,
		`SELECT posture, organization_id, project_id, strict_enrollment FROM runner_pools WHERE id = $1`,
		fleet.DefaultPoolID).Scan(&seededPosture, &seededOrg, &seededProject, &strict); err != nil {
		t.Fatalf("migration 000045 R6 seeded no default pool for the bootstrap tenant: %v", err)
	}
	if seededPosture != "sandboxed-linux" || seededOrg != "org_local" || seededProject != "prj_local" || strict {
		t.Fatalf("the seeded default pool is (%s, %s, %s, strict=%v), want today's deployment stated as data",
			seededPosture, seededOrg, seededProject, strict)
	}

	// R6's OTHER population is identity.provision's — a FRESH stack, where the migrations run before
	// there is any organization to reference at all. It is asserted here on a NEW tenant rather than on
	// the bootstrap one, for the same shared-database reason: CreateOrganization runs the SAME provision
	// transaction ProvisionFirstOrg does, so what it proves about the pool it proves about both.
	created, err := identity.New(spine.Pool()).CreateOrganization(seed, middleware.Scope{}, []byte(`{"display_name":"fleet-seed"}`))
	if err != nil {
		t.Fatalf("create a second organization: %v", err)
	}
	var born struct {
		ID               string `json:"id"`
		DefaultProjectID string `json:"default_project_id"`
	}
	if err := json.Unmarshal(created.Body, &born); err != nil || born.ID == "" {
		t.Fatalf("decode the created organization: %v (%s)", err, created.Body)
	}
	var bornPools int
	if err := spine.Pool().QueryRow(seed,
		`SELECT count(*) FROM runner_pools WHERE organization_id = $1 AND project_id = $2 AND posture = 'sandboxed-linux'`,
		born.ID, born.DefaultProjectID).Scan(&bornPools); err != nil {
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
