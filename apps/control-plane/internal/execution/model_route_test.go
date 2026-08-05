package execution

import (
	"errors"
	"testing"

	"github.com/palgroup/palai/packages/coordinator"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// TestResolveLayersProjectRouteAboveDeployment proves the E13 T8 layering: the env-selected deployment
// default is the BOTTOM layer, and a project's DB-backed model route sits directly above it (spec §14,
// §27.6). A project with no published route resolves bit-identically to before (the deployment default,
// deployment provenance); a project WITH one routes its own model + credential ref and says so in the
// provenance — a project route is not a deployment default, and the snapshot must not claim it is.
func TestResolveLayersProjectRouteAboveDeployment(t *testing.T) {
	deployment := ResolveInput{DeploymentModel: "env-default", DeploymentSecret: "provider-one"}

	base := Resolve(deployment)
	if base.Model != "env-default" || base.Provenance["model"] != layerDeployment {
		t.Fatalf("no route: model = %q prov = %q, want env-default from %s", base.Model, base.Provenance["model"], layerDeployment)
	}

	routed := deployment
	routed.ProjectRouteModel, routed.ProjectRouteSecret = "route-model", "tenant:org_a/openai"
	snap := Resolve(routed)
	if snap.Model != "route-model" {
		t.Fatalf("project route: model = %q, want the route's model", snap.Model)
	}
	if snap.Provenance["model"] != layerProjectRoute {
		t.Fatalf("project route: model provenance = %q, want %q", snap.Provenance["model"], layerProjectRoute)
	}
	if snap.SecretRef != "tenant:org_a/openai" {
		t.Fatalf("project route: secret ref = %q, want the route connection's ref", snap.SecretRef)
	}
	if snap.Hash == base.Hash {
		t.Fatal("routing a project onto its own model + credential must change the content address")
	}

	// A session override still wins over the project route (the layer is BELOW session, not above it).
	over := routed
	over.SessionModel = "session-model"
	if s := Resolve(over); s.Model != "session-model" || s.Provenance["model"] != layerSession {
		t.Fatalf("session over route: model = %q prov = %q, want session-model from session", s.Model, s.Provenance["model"])
	}
}

// TestTenantSecretRefCarriesItsOwnerAndTheOldFormIsRefused proves the ref draws TWO distinctions since
// 000006, where between A.2 Task 6 and that migration it drew one.
//
// THIS TEST ASSERTED THE OPPOSITE FOR ONE PHASE AND THAT IS RECORDED RATHER THAN OVERWRITTEN. It was
// called TestTenantSecretRefMarksARouteMintedHandle and its comment read "The org path segment is GONE —
// see TenantSecretRef for why it was removed rather than replaced by a project." That was correct while
// secret_refs had no tenant column: an owner segment would have named an isolation the redemption did not
// perform. 000006 restores the column, so the segment is load-bearing again — and this time the assertion
// below checks that Redeem SPENDS it, which is what the original org segment never had.
//
// The ref is still a HANDLE and never carries a credential value.
func TestTenantSecretRefCarriesItsOwnerAndTheOldFormIsRefused(t *testing.T) {
	ref := TenantSecretRef("prj_a", "openai")
	project, name, ok := SplitTenantSecretRef(ref)
	if !ok || project != "prj_a" || name != "openai" {
		t.Fatalf("SplitTenantSecretRef(%q) = (%q, %q, %v), want (prj_a, openai, true)", ref, project, name, ok)
	}

	// A NAME MAY CONTAIN COLONS. An environment value is stored under `env:<environment_id>:<key>`, so a
	// split on the LAST colon — or on all of them — would hand the store a truncated name and resolve
	// nothing. This is the case that makes the "first colon only" rule load-bearing rather than tidy.
	envRef := TenantSecretRef("prj_a", "env:env_deadbeef:JIRA_TOKEN")
	project, name, ok = SplitTenantSecretRef(envRef)
	if !ok || project != "prj_a" || name != "env:env_deadbeef:JIRA_TOKEN" {
		t.Fatalf("SplitTenantSecretRef(%q) = (%q, %q, %v), want the derived name intact", envRef, project, name, ok)
	}

	// An env deployment-default ref is NOT tenant-qualified and must stay untouched.
	if _, _, ok := SplitTenantSecretRef(modelbroker.SecretRef("provider-one")); ok {
		t.Fatal("the env deployment-default ref must not parse as tenant-qualified")
	}

	// THE PRE-000006 FORM IS REFUSED, and refusing it is the point: `tenant:openai` names a route-minted
	// handle with NO owner. Reading it as a bare name would send it to the store under whichever tenant
	// happened to be at hand, which is the defect this whole change removes. It falls through to the env
	// bridge instead, where a missing fallback refuses it outright.
	if _, _, ok := SplitTenantSecretRef(modelbroker.SecretRef("tenant:openai")); ok {
		t.Fatal("the ownerless pre-000006 form parsed as tenant-qualified — it must not, or it would redeem under an arbitrary tenant")
	}
}

// TestRouteSecretResolverScopesAndFailsClosed proves the broker-side redemption rules:
//  1. a route-minted ref redeems through the T3 secret store;
//  2. a plain ref (the env deployment default) still redeems through the env fallback, unchanged;
//  3. a tenant-qualified ref the store MISSES fails closed — it must never fall back to the deployment
//     credential, or one tenant's run would silently bill and authenticate as the deployment default.
func TestRouteSecretResolverScopesAndFailsClosed(t *testing.T) {
	var askedName, askedProject string
	resolver := RouteSecretResolver{
		Lookup: func(tenant coordinator.Tenant, name string) ([]byte, bool, error) {
			askedName, askedProject = name, tenant.Project
			if name == "openai" && tenant.Project == "prj_a" {
				return []byte("tenant-credential"), true, nil
			}
			return nil, false, nil
		},
		Fallback: modelbroker.StaticResolver{"provider-one": "env-credential"},
	}

	got, err := resolver.Redeem(TenantSecretRef("prj_a", "openai"))
	if err != nil || got != "tenant-credential" {
		t.Fatalf("Redeem(tenant ref) = (%q, %v), want the tenant's own credential", got, err)
	}
	if askedName != "openai" {
		t.Fatalf("store was asked for %q, want openai", askedName)
	}
	// THE ASSERTION THIS TEST DID NOT HAVE BEFORE 000006. Redeem carrying the project is not the same as
	// Redeem SPENDING it: the org segment it used to carry was parsed and discarded for a whole phase, and
	// nothing here would have noticed. What is checked is the value that reached the store.
	if askedProject != "prj_a" {
		t.Fatalf("store was asked on behalf of %q, want prj_a — the owner segment is being parsed and dropped", askedProject)
	}

	// A DIFFERENT PROJECT NAMING THE SAME REF GETS NOTHING, which is the boundary rather than the routing
	// mark. The fake keys on both, so a Redeem that forwarded the wrong owner — or none — misses and fails
	// closed here rather than silently returning prj_a's credential.
	if _, err := resolver.Redeem(TenantSecretRef("prj_b", "openai")); !errors.Is(err, modelbroker.ErrUnknownSecret) {
		t.Fatalf("Redeem(another project's ref by the same name) error = %v, want ErrUnknownSecret", err)
	}

	if got, err := resolver.Redeem(modelbroker.SecretRef("provider-one")); err != nil || got != "env-credential" {
		t.Fatalf("Redeem(env ref) = (%q, %v), want the env deployment-default credential", got, err)
	}

	if _, err := resolver.Redeem(TenantSecretRef("prj_a", "unprovisioned")); !errors.Is(err, modelbroker.ErrUnknownSecret) {
		t.Fatalf("Redeem(missing tenant ref) error = %v, want ErrUnknownSecret (fail closed, never the env credential)", err)
	}
}
