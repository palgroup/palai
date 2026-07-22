package execution

import (
	"context"
	"fmt"
	"strings"

	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// DB-backed model routing, controller half (E13 Task 8; spec §27.2/§27.6/§27.7 — the LP §7.3 carve-out).
//
// SCOPE, HONESTLY: this decides WHICH MODEL ID and WHICH CREDENTIAL a project's run uses. It is one
// provider family (provider-one); a second independent adapter and the §27.5 capability probe are E16, and
// nothing here ranks candidates, hedges, or fails over.
//
// LAYERING: the env-selected route (PALAI_MODEL_PROVIDER / PALAI_MODEL, set on the Orchestrator by the
// composition root) is no longer the only answer — it is the deployment-default FALLBACK underneath the
// project's route. A project with no published route runs exactly as it did before this task.

// tenantSecretRefPrefix marks a credential handle as belonging to one organization. A route's connection
// ref is qualified with the run's own org so the broker's redemption is tenant-scoped: one tenant's ref
// NAME can never redeem another tenant's credential, even if both call it "openai".
const tenantSecretRefPrefix = "tenant:"

// TenantSecretRef qualifies a connection's secret-ref handle with the organization that owns it. The org is
// server-minted from the run's own tenant — never a request body — so the handle is not forgeable by a
// tenant. It stays a HANDLE: no credential value is ever encoded here.
func TenantSecretRef(org, name string) modelbroker.SecretRef {
	return modelbroker.SecretRef(tenantSecretRefPrefix + org + "/" + name)
}

// SplitTenantSecretRef reverses TenantSecretRef. ok=false means the ref is an unqualified deployment
// handle (the env route's), which redeems through the env bridge exactly as before.
func SplitTenantSecretRef(ref modelbroker.SecretRef) (org, name string, ok bool) {
	rest, found := strings.CutPrefix(string(ref), tenantSecretRefPrefix)
	if !found {
		return "", "", false
	}
	org, name, found = strings.Cut(rest, "/")
	if !found || org == "" || name == "" {
		return "", "", false
	}
	return org, name, true
}

// RouteSecretResolver is the broker's credential redemption for DB-backed routes. A tenant-qualified ref
// (minted by a project's route) resolves through Lookup — the E13 T3 secret store, scoped to that org —
// and every other ref falls through to Fallback, the deployment-default env bridge.
//
// A tenant-qualified ref the store cannot resolve FAILS CLOSED. Falling back to the deployment credential
// would silently run — and bill — one tenant's project on the operator's own key.
type RouteSecretResolver struct {
	// Lookup resolves (org, ref name) to the credential value; ok=false is a clean miss.
	Lookup func(org, name string) ([]byte, bool, error)
	// Fallback redeems unqualified deployment refs (the env bridge). Nil rejects every such ref.
	Fallback modelbroker.SecretResolver
}

// Redeem implements modelbroker.SecretResolver. The value lives only in the returned frame — it is never
// logged, and the error text names the ref, never the credential.
func (r RouteSecretResolver) Redeem(ref modelbroker.SecretRef) (string, error) {
	org, name, ok := SplitTenantSecretRef(ref)
	if !ok {
		if r.Fallback == nil {
			return "", fmt.Errorf("%w: %s", modelbroker.ErrUnknownSecret, ref)
		}
		return r.Fallback.Redeem(ref)
	}
	if r.Lookup == nil {
		return "", fmt.Errorf("%w: no tenant secret store is wired for %s", modelbroker.ErrUnknownSecret, name)
	}
	value, found, err := r.Lookup(org, name)
	if err != nil {
		return "", fmt.Errorf("redeem model connection credential %q: %w", name, err)
	}
	if !found {
		return "", fmt.Errorf("%w: model connection credential %q is not provisioned for this tenant", modelbroker.ErrUnknownSecret, name)
	}
	return string(value), nil
}

// effectiveRoute resolves the route this run's model steps dispatch through: the project's published
// model route when it has one, else the deployment default the composition root set from the environment.
// It is resolved ONCE per attempt and cached, so every boundary of one attempt (dispatch, the effective
// model, the checkpointed config hash) agrees on the same target.
//
// ponytail: attempt-scoped, not run-scoped — a route republished between two attempts of the same run is
// picked up by the next attempt. Pinning the revision on the run row (the other half of §27.6) needs a
// column 000001 does not have, so it is not claimed; each model step DOES record the revision it selected.
func (o *Orchestrator) effectiveRoute(ctx context.Context, st *attemptState) (ModelRoute, error) {
	if st.routeResolved {
		return st.route, nil
	}
	target, found, err := o.spine.ProjectModelRoute(ctx, st.tenant)
	if err != nil {
		return ModelRoute{}, fmt.Errorf("resolve model route for project %s: %w", st.tenant.Project, err)
	}
	route := o.route
	if found {
		route = ModelRoute{
			Provider:   target.Provider,
			Model:      target.Model,
			Secret:     TenantSecretRef(st.tenant.Organization, target.SecretRef),
			RevisionID: target.RevisionID,
			Revision:   target.Revision,
		}
	}
	st.route, st.routeResolved = route, true
	return route, nil
}

// routeLayers projects the effective route onto the config resolver's two lowest layers (spec §14): the
// env deployment default always occupies `deployment`, and a DB-resolved project route occupies
// `project_route` directly above it. A project route is NOT a deployment default, and the snapshot's
// provenance must not claim it is.
func (o *Orchestrator) routeLayers(route ModelRoute, in *ResolveInput) {
	in.DeploymentModel, in.DeploymentSecret = o.route.Model, string(o.route.Secret)
	if route.RevisionID != "" {
		in.ProjectRouteModel, in.ProjectRouteSecret = route.Model, string(route.Secret)
	}
}
