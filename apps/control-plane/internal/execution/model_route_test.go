package execution

import (
	"errors"
	"testing"

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

// TestTenantSecretRefIsTenantQualified proves a route's credential handle carries the org that owns it,
// so the broker's redemption is scoped to the run's own tenant and one tenant's ref name can never
// redeem another's credential. The ref is a HANDLE — it must never carry a credential value.
func TestTenantSecretRefIsTenantQualified(t *testing.T) {
	ref := TenantSecretRef("org_a", "openai")
	org, name, ok := SplitTenantSecretRef(ref)
	if !ok || org != "org_a" || name != "openai" {
		t.Fatalf("SplitTenantSecretRef(%q) = (%q, %q, %v), want (org_a, openai, true)", ref, org, name, ok)
	}
	// An env deployment-default ref is NOT tenant-qualified and must stay untouched.
	if _, _, ok := SplitTenantSecretRef(modelbroker.SecretRef("provider-one")); ok {
		t.Fatal("the env deployment-default ref must not parse as tenant-qualified")
	}
}

// TestRouteSecretResolverScopesAndFailsClosed proves the broker-side redemption rules:
//  1. a tenant-qualified ref (minted by a DB route) redeems through the T3 secret store under ITS OWN org;
//  2. a plain ref (the env deployment default) still redeems through the env fallback, unchanged;
//  3. a tenant-qualified ref the store MISSES fails closed — it must never fall back to the deployment
//     credential, or one tenant's run would silently bill and authenticate as the deployment default.
func TestRouteSecretResolverScopesAndFailsClosed(t *testing.T) {
	var askedOrg, askedName string
	resolver := RouteSecretResolver{
		Lookup: func(org, name string) ([]byte, bool, error) {
			askedOrg, askedName = org, name
			if org == "org_a" && name == "openai" {
				return []byte("tenant-credential"), true, nil
			}
			return nil, false, nil
		},
		Fallback: modelbroker.StaticResolver{"provider-one": "env-credential"},
	}

	got, err := resolver.Redeem(TenantSecretRef("org_a", "openai"))
	if err != nil || got != "tenant-credential" {
		t.Fatalf("Redeem(tenant ref) = (%q, %v), want the tenant's own credential", got, err)
	}
	if askedOrg != "org_a" || askedName != "openai" {
		t.Fatalf("store was asked for (%q, %q), want (org_a, openai)", askedOrg, askedName)
	}

	if got, err := resolver.Redeem(modelbroker.SecretRef("provider-one")); err != nil || got != "env-credential" {
		t.Fatalf("Redeem(env ref) = (%q, %v), want the env deployment-default credential", got, err)
	}

	if _, err := resolver.Redeem(TenantSecretRef("org_b", "openai")); !errors.Is(err, modelbroker.ErrUnknownSecret) {
		t.Fatalf("Redeem(missing tenant ref) error = %v, want ErrUnknownSecret (fail closed, never the env credential)", err)
	}
}
