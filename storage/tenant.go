package storage

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrProjectRequired is returned by a connection acquisition when the acquiring context carries a
// non-system tenant scope with no project (A.2 Task 1). Before this task an empty project silently
// widened a query to "every project in the organization" (the RLS policy's own coalesce-to-empty
// fallback, storage/migrations/000029_row_level_security.up.sql); once organizations are gone that
// widening would mean "every project, full stop" — the absence of a boundary, not one. WithOrgScope is
// the one deliberate, narrow exception: it is not this error's target, and PrepareConn skips the check
// for it on purpose.
var ErrProjectRequired = errors.New("storage: tenant scope requires a project")

// RuntimeRole is the non-owner database role every application connection runs as (declared in
// migration 000001, made load-bearing by 000029). It owns no table and is not superuser, which is
// exactly what makes the row-level-security policies apply to it — RLS is inert for an owner or a
// superuser.
const RuntimeRole = "palai_app"

type scopeKey struct{}

// scope is the tenant a connection acquired under this context may see. The zero value declares
// nothing, and a connection that declares nothing sees no tenant row: deny is the default, so a
// forgotten scope fails loudly rather than reading the whole installation. Now more loudly still (A.2
// Task 1): the zero value has no project and is not orgOnly, so acquiring under it is refused outright
// rather than silently opened and left to return zero rows.
type scope struct {
	organization string
	project      string
	system       bool
	// orgOnly marks a scope built by WithOrgScope: a deliberate, narrow exception to the project-required
	// rule below, for the handful of surfaces that predate per-project structure. Never set by WithTenant.
	orgOnly bool
}

// WithTenant binds the verified organization/project to ctx. Every connection acquired under it sets
// palai.org_id / palai.project_id, so the database enforces the same boundary the query's WHERE clause
// claims. The values must come from a verified credential (the auth middleware's scope, or a claimed
// job's own tenant) — never from a request body.
//
// project MUST be non-empty (A.2 Task 1): PrepareConn refuses to acquire a connection under a scope that
// names an organization (or nothing at all) but no project, returning ErrProjectRequired. Before this task
// an empty project silently widened a query to "every project in the organization" — once organizations
// are gone that widening means "every project, full stop", so the boundary's absence is now an error
// instead of an unbounded read. A caller that genuinely needs the pre-project-era org-wide reach (identity
// provisioning, the org-scoped secret_refs store) uses WithOrgScope instead, not an empty string here.
func WithTenant(ctx context.Context, organization, project string) context.Context {
	return context.WithValue(ctx, scopeKey{}, scope{organization: organization, project: project})
}

// WithOrgScope binds ctx to organization alone, with no project — the one deliberate, narrow exception to
// WithTenant's project-required rule. It exists for the surfaces that still span every project in an
// organization and predate per-project structure: identity/provisioning's own tables (organizations,
// api_keys, principals — orgScope in apps/control-plane/internal/identity/store.go), the org-scoped
// secret_refs store fronting the env-file bridge (identity.SecretStore.Resolve, secret_refs carries no
// project_id at all — storage/migrations/000031_secret_refs.up.sql), and environments/environment_values
// (000046, likewise no project_id column). Task 3 of this epic removes the Go-side organization concept
// and is expected to retire this function along with its callers; until then it keeps those three
// surfaces working exactly as they do today without reopening WithTenant's contract for every other
// caller. Like WithSystemScope, it is deliberately greppable: every call site is a place the per-project
// boundary does NOT apply, so each should stay as narrow as it is today.
func WithOrgScope(ctx context.Context, organization string) context.Context {
	return context.WithValue(ctx, scopeKey{}, scope{organization: organization, orgOnly: true})
}

// WithSystemScope marks ctx as one of the control plane's genuinely cross-tenant infrastructure paths:
// the durable job claim loop, the retention sweep, the outbox/webhook/schedule pumps, the migration and
// bootstrap steps, and API-key verification (which must read a credential before any tenant is known).
// Connections acquired under it set palai.system=on and every tenant policy admits them.
//
// This is the deliberate escape hatch from the isolation 000029 installs. It is greppable on purpose:
// every call site is a place where the tenant boundary is NOT protecting the query, so each one should
// be as narrow as it can be — the run worker, for example, claims under a system scope but hands the
// handler a WithTenant context built from the claimed job's own tenant.
func WithSystemScope(ctx context.Context) context.Context {
	return context.WithValue(ctx, scopeKey{}, scope{system: true})
}

// ScopeToTenant narrows ctx to the tenant a repository method was called for, but ONLY when the
// context does not already carry a scope. Precedence is the point:
//
//   - On an HTTP request the auth middleware has already published the scope verified from the API
//     key, and that wins. A method invoked with a different tenant than the credential proves then
//     reads zero rows — the database catches the mismatch instead of trusting the argument.
//   - The background worker's per-job scope and the explicit system scopes win the same way.
//   - Everything else — an internal caller or a test driving the repository directly — is scoped by
//     the tenant it declared in the call, so its queries run under the policies rather than around
//     them.
//
// A caller that declares neither still gets the zero scope, and the zero scope now refuses to acquire a
// connection at all (A.2 Task 1) rather than silently seeing nothing.
func ScopeToTenant(ctx context.Context, organization, project string) context.Context {
	if s := scopeFrom(ctx); s.system || s.organization != "" {
		return ctx
	}
	return WithTenant(ctx, organization, project)
}

// scopeFrom reads the scope a connection should be acquired under. A context that was never marked
// yields the zero scope, which PrepareConn refuses to acquire a connection under at all.
func scopeFrom(ctx context.Context) scope {
	s, _ := ctx.Value(scopeKey{}).(scope)
	return s
}

// OrganizationForProject resolves the organization a project belongs to (A.2 Task 3). It is the one place
// Go code still needs a real organization value now that the request scope no longer resolves one
// (middleware.Scope dropped its Organization field): identity's own org-wide provisioning surface and the
// handful of call sites that still populate a coordinator.Tenant/wire-rendered organization_id use this in
// place of the removed Scope field. It runs system-scoped, mirroring VerifyAPIKey and bootstrap:
// establishing which organization owns a project cannot itself be gated behind that organization.
//
// IT IS SCAFFOLDING, NOT ARCHITECTURE, AND IT HAS A NAMED END. Palai is becoming single-tenant: the
// organization_id columns are dropped in A.2 Task 5, and this function goes with them. Nothing new
// should be built on it.
//
// ITS COST, MEASURED RATHER THAN WAVED AT: 96 call sites, no cache, so one store-method call is one
// extra `SELECT organization_id FROM projects`. That price is paid to keep feeding a column already
// scheduled for deletion, which is exactly why it is temporary and why the count is written here —
// a bridge whose toll is undocumented reads like a design.
//
//	grep -rn 'OrganizationForProject(' --include='*.go' . | grep -v _test | grep -v 'func ' | wc -l  -> 96
func OrganizationForProject(ctx context.Context, pool *pgxpool.Pool, project string) (string, error) {
	var organization string
	err := pool.QueryRow(WithSystemScope(ctx), Query("OrganizationForProject"), project).Scan(&organization)
	return organization, err
}
