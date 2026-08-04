package storage

import (
	"context"
	"errors"
)

// ErrProjectRequired is returned by a connection acquisition when the acquiring context carries a
// non-system tenant scope with no project (A.2 Task 1). Before this task an empty project silently
// widened a query to "every project in the organization" (the RLS policy's own coalesce-to-empty
// fallback, storage/migrations/000029_row_level_security.up.sql); with organizations gone that
// widening would mean "every project, full stop" — the absence of a boundary, not one.
// WithInstallationScope is the one deliberate, narrow exception: it is not this error's target, and
// PrepareConn skips the check for it on purpose.
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
// Task 1): the zero value has no project and is not installation-wide, so acquiring under it is refused
// outright rather than silently opened and left to return zero rows.
type scope struct {
	project string
	system  bool
	// installation marks a scope built by WithInstallationScope: a deliberate, narrow exception to the
	// project-required rule below, for the two tables that carry no project column at all. Never set by
	// WithTenant.
	installation bool
}

// WithTenant binds the verified project to ctx. Every connection acquired under it sets
// palai.project_id, so the database enforces the same boundary the query's WHERE clause claims. The
// value must come from a verified credential (the auth middleware's scope, or a claimed job's own
// tenant) — never from a request body.
//
// project MUST be non-empty (A.2 Task 1): PrepareConn refuses to acquire a connection under a scope
// that names no project, returning ErrProjectRequired. An empty project would publish an empty
// palai.project_id, which the policies read as a row whose own project_id is the empty string — not as
// a boundary. A caller that genuinely needs the reach of a table with no project column
// (identity's secret store, automation's environment check) uses WithInstallationScope instead, not an
// empty string here.
func WithTenant(ctx context.Context, project string) context.Context {
	return context.WithValue(ctx, scopeKey{}, scope{project: project})
}

// WithInstallationScope binds ctx to the whole installation, with no project — the one deliberate,
// narrow exception to WithTenant's project-required rule. It is what A.2 Task 6 left of WithOrgScope.
//
// ITS LIST OF PRODUCTION CALLERS IS TWO, and both read a table with no project_id at all:
// identity.SecretStore.Resolve over secret_refs (000031) and automation's verifyEnvironment over
// environments (000046). Migration 000066 keys BOTH on the installation, so what this supplies for them
// is no longer a boundary — it is only a scope declaration, which those policies still require (they
// admit `palai.project_id IS NOT NULL`, and set_config with an empty string satisfies that while a
// context that declared nothing does not).
//
// SAYING THAT PLAINLY: on this installation every project can read every environment, every environment
// value and every secret ref. That is the same reach these tables had before A.2 (one installation held
// one organization), but it is now the ABSENCE of a boundary rather than one. An installation meant to
// hold two customers must give those tables a project_id before it does.
//
// Like WithSystemScope, it is deliberately greppable: every call site is a place the per-project
// boundary does NOT apply, so each should stay as narrow as it is today.
func WithInstallationScope(ctx context.Context) context.Context {
	return context.WithValue(ctx, scopeKey{}, scope{installation: true})
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
func ScopeToTenant(ctx context.Context, project string) context.Context {
	if s := scopeFrom(ctx); s.system || s.installation || s.project != "" {
		return ctx
	}
	return WithTenant(ctx, project)
}

// scopeFrom reads the scope a connection should be acquired under. A context that was never marked
// yields the zero scope, which PrepareConn refuses to acquire a connection under at all.
func scopeFrom(ctx context.Context) scope {
	s, _ := ctx.Value(scopeKey{}).(scope)
	return s
}
