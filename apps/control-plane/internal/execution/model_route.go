package execution

import (
	"context"
	"fmt"
	"strings"

	"github.com/palgroup/palai/packages/coordinator"
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

// tenantSecretRefPrefix marks a credential handle as one a TENANT ROUTE minted: it selects the DB-backed
// store over the deployment env bridge, and since 000006 the segment that FOLLOWS it names the owner.
const tenantSecretRefPrefix = "tenant:"

// TenantSecretRef marks a connection's secret-ref handle as one a TENANT ROUTE minted, so Redeem sends it
// to the DB-backed store instead of the deployment env bridge, AND names the project it must be redeemed
// for. It stays a HANDLE: no credential value is ever encoded here.
//
// THE OWNER SEGMENT IS BACK, AND THE ROUND TRIP IS WHY THIS PARAGRAPH IS LONG. It carried the owning
// ORGANIZATION originally; A.2 Task 6 removed it rather than swapping in the project, and that was the
// right call at the time for a reason worth keeping: identity's secret store had stopped enforcing any
// boundary, so a project segment would have "read as an isolation this deployment does not have". The
// segment was deleted because it had become decoration. 000006 gives secret_refs a project_id, so the
// isolation exists again and the segment is load-bearing again — this time it is not decoration, because
// RouteSecretResolver.Redeem SPENDS it on the lookup rather than only carrying it.
//
// WHY IN THE REF AT ALL, rather than in the broker's signature: modelbroker.SecretResolver is
// `Redeem(ref) (string, error)` with no tenant and no context, and it is implemented four times over
// (StaticResolver, EnvResolver, ChainResolver, this) plus the conformance corpus. The ref is the only
// channel that already reaches every implementation, and nothing persists this form except the config
// snapshot's provenance field — it is built per dispatch from the stored bare name.
//
// The separator is a single colon and the SPLIT IS ON THE FIRST ONE ONLY, because a secret name may
// legitimately contain colons: an environment value is stored under `env:<environment_id>:<key>`. A
// project id carries none (`prj_` + hex), so `tenant:<project>:<name>` parses back exactly.
func TenantSecretRef(project, name string) modelbroker.SecretRef {
	return modelbroker.SecretRef(tenantSecretRefPrefix + project + ":" + name)
}

// SplitTenantSecretRef reverses TenantSecretRef. ok=false means the ref is an unqualified deployment
// handle (the env route's), which redeems through the env bridge exactly as before.
//
// A ref carrying the prefix but NO owner segment is ALSO ok=false, and that is deliberate rather than
// lenient: it is the pre-000006 form, and the only safe reading of it is "this handle names no project".
// Treating it as a bare name would send it to the store under whatever tenant happened to be at hand,
// which is the failure this whole change exists to remove — so it falls through to the env bridge, and if
// no fallback is wired it is refused.
func SplitTenantSecretRef(ref modelbroker.SecretRef) (project, name string, ok bool) {
	rest, found := strings.CutPrefix(string(ref), tenantSecretRefPrefix)
	if !found {
		return "", "", false
	}
	project, name, split := strings.Cut(rest, ":")
	if !split || project == "" || name == "" {
		return "", "", false
	}
	return project, name, true
}

// RouteSecretResolver is the broker's credential redemption for DB-backed routes. A tenant-qualified ref
// (minted by a project's route) resolves through Lookup — the E13 T3 secret store — and every other ref
// falls through to Fallback, the deployment-default env bridge.
//
// A tenant-qualified ref the store cannot resolve FAILS CLOSED. Falling back to the deployment credential
// would silently run — and bill — one tenant's project on the operator's own key.
type RouteSecretResolver struct {
	// Lookup resolves a ref name to the credential value FOR ONE TENANT; ok=false is a clean miss.
	Lookup func(tenant coordinator.Tenant, name string) ([]byte, bool, error)
	// Fallback redeems unqualified deployment refs (the env bridge). Nil rejects every such ref.
	Fallback modelbroker.SecretResolver
}

// Redeem implements modelbroker.SecretResolver. The value lives only in the returned frame — it is never
// logged, and the error text names the ref, never the credential.
func (r RouteSecretResolver) Redeem(ref modelbroker.SecretRef) (string, error) {
	project, name, ok := SplitTenantSecretRef(ref)
	if !ok {
		if r.Fallback == nil {
			return "", fmt.Errorf("%w: %s", modelbroker.ErrUnknownSecret, ref)
		}
		return r.Fallback.Redeem(ref)
	}
	if r.Lookup == nil {
		return "", fmt.Errorf("%w: no tenant secret store is wired for %s", modelbroker.ErrUnknownSecret, name)
	}
	value, found, err := r.Lookup(coordinator.Tenant{Project: project}, name)
	if err != nil {
		return "", fmt.Errorf("redeem model connection credential %q: %w", name, err)
	}
	if !found {
		// A store outage degrades to a clean miss (the resolver bounds and logs it), so a miss means EITHER
		// no such ref OR an unreachable store — say both, or an operator spends an outage hunting for a
		// credential that is actually there. Matches the repository-connection resolver's wording.
		return "", fmt.Errorf("%w: model connection credential %q did not resolve: no such secret ref, or the secret store was unreachable (see the secret-store log)",
			modelbroker.ErrUnknownSecret, name)
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
			Secret:     TenantSecretRef(st.tenant.Project, target.SecretRef),
			BaseURL:    target.BaseURL,
			RevisionID: target.RevisionID,
			Revision:   target.Revision,
			Thinking:   modelbroker.ThinkingMode(target.Thinking),
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
