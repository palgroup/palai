//go:build component

package execution_test

// THE §2 NON-NEGOTIABLE, AGAINST A REAL DATABASE: "TEK RUNNER'LI DEPLOYMENT BİT-DEĞİŞMEZDİR" — a
// runner that declares no pool falls into pool_default and the deployment behaves as it does today.
// §2 says that rule "stops with a test, not a comment", so here is the test.
//
// It drives the three real pieces end to end with NO pool configuration anywhere: the identity
// bootstrap that a fresh install runs, the production fleet.Store, and the REAL gateway over REAL
// mTLS against a REAL packages/runner enrollment. Nothing in it names a pool — that is the point. The
// only thing that could make it pass is the seed actually existing and the gateway actually resolving
// it, which is what R6 is for.

import (
	"context"
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
	// The identity bootstrap a FRESH install runs. It is idempotent (every insert is ON CONFLICT DO
	// NOTHING), so re-running it against this shared component database is a no-op — which is also the
	// re-boot-against-a-retained-volume path in production.
	if err := identity.New(spine.Pool()).ProvisionFirstOrg(context.Background(), "component-bootstrap-key"); err != nil {
		t.Fatalf("provision the bootstrap organization: %v", err)
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
