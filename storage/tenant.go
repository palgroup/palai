package storage

import "context"

// RuntimeRole is the non-owner database role every application connection runs as (declared in
// migration 000001, made load-bearing by 000029). It owns no table and is not superuser, which is
// exactly what makes the row-level-security policies apply to it — RLS is inert for an owner or a
// superuser.
const RuntimeRole = "palai_app"

type scopeKey struct{}

// scope is the tenant a connection acquired under this context may see. The zero value declares
// nothing, and a connection that declares nothing sees no tenant row: deny is the default, so a
// forgotten scope fails loudly rather than reading the whole installation.
type scope struct {
	organization string
	project      string
	system       bool
}

// WithTenant binds the verified organization/project to ctx. Every connection acquired under it sets
// palai.org_id / palai.project_id, so the database enforces the same boundary the query's WHERE clause
// claims. The values must come from a verified credential (the auth middleware's scope, or a claimed
// job's own tenant) — never from a request body.
//
// An empty project narrows nothing: the context sees the whole organization. That is what the
// coordinator's org-wide paths need, and it is still a hard tenant boundary.
func WithTenant(ctx context.Context, organization, project string) context.Context {
	return context.WithValue(ctx, scopeKey{}, scope{organization: organization, project: project})
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

// scopeFrom reads the scope a connection should be acquired under. A context that was never marked
// yields the zero scope, which denies.
func scopeFrom(ctx context.Context) scope {
	s, _ := ctx.Value(scopeKey{}).(scope)
	return s
}
