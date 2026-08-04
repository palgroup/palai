package middleware

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/palgroup/palai/storage"
)

// Scope is the verified tenant an API key resolves to. It comes from identity,
// never from a request-body field (spec §39.2), and is the only source handlers
// use to scope writes.
//
// NO ORGANIZATION (A.2 Task 3): the request scope resolves a project and nothing above it. Palai is
// becoming single-tenant per installation (storage/migrations/000062's header), so the HTTP layer no
// longer needs to know which organization a project belongs to — the handful of call sites that
// genuinely still need that value (identity's org-wide provisioning listing, a coordinator.Tenant
// construction, a wire-rendered organization_id) resolve it fresh via storage.OrganizationForProject,
// keyed off Project, rather than reading it from here.
type Scope struct {
	Project   string
	Principal string
	// APIKeyID is the id of the key this request authenticated with (E23 T2). It is not the same as
	// Principal — several keys may share a principal — and it is what an approver list names, because a
	// key is what an operator revokes.
	APIKeyID string
	// Scopes is the key's coarse capability set (E13 T2). Empty means unrestricted (the ConfigPolicy
	// §9.3 idiom); the tenancy provisioning surface requires the `provision` capability. HONEST CEILING:
	// basic scopes only — named roles, relationships, and OIDC are E13-H/E17.
	Scopes []string
}

// HasScope reports whether the key may perform an operation guarded by capability. An empty scope set is
// unrestricted (an admin/bootstrap key), matching how an empty ConfigPolicy allowlist permits everything.
func (s Scope) HasScope(capability string) bool {
	if len(s.Scopes) == 0 {
		return true
	}
	for _, c := range s.Scopes {
		if c == capability {
			return true
		}
	}
	return false
}

// ScopeSystem is the PLATFORM's own capability. It is deliberately outside the empty-set rule above:
// an empty scope set means "every tenant capability", and a tenant admin key is exactly that. Handing
// it the platform capability too would make every customer's admin key able to open new tenants and
// read the fleet, which is what this constant exists to prevent.
const ScopeSystem = "system"

// HasSystem reports whether this key carries the platform capability. Unlike HasScope, an empty set is
// NOT unrestricted here — system must be granted explicitly, never inherited.
func (s Scope) HasSystem() bool {
	for _, c := range s.Scopes {
		if c == ScopeSystem {
			return true
		}
	}
	return false
}

// Verifier resolves a bearer token to its tenant scope. The stored verifier is a
// hash; the full key is never persisted (spec §20 security).
type Verifier interface {
	VerifyAPIKey(ctx context.Context, token string) (Scope, error)
}

// ErrInvalidToken reports a bearer key that matches no live credential.
var ErrInvalidToken = errors.New("invalid_token")

type scopeKey struct{}

// Auth requires a valid bearer API key. A missing or malformed Authorization
// header is authentication_required; a syntactically present but unrecognized key
// is invalid_token. Neither problem echoes the presented credential.
func Auth(v Verifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r.Header.Get("Authorization"))
			if !ok {
				WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
				return
			}
			scope, err := v.VerifyAPIKey(r.Context(), token)
			if err != nil {
				WriteProblem(w, r, http.StatusUnauthorized, "invalid_token", "the API key is not valid")
				return
			}
			// The verified scope is also published to the database layer, so every query this
			// request issues runs under palai.project_id and migration 000062's policies enforce the
			// same boundary the handlers' WHERE clauses claim. This is the ONLY place a request's
			// tenant enters the DB scope — it comes from the credential, never from a body field
			// (spec §39.2). There is no organization to publish (A.2 Task 3) and no palai.org_id left
			// to publish it into (A.2 Task 6): every tenant policy reads palai.project_id, and the
			// three tables that carry no project column at all (environments, environment_values,
			// secret_refs) are reached only through storage.WithInstallationScope.
			ctx := storage.WithTenant(r.Context(), scope.Project)
			ctx = context.WithValue(ctx, scopeKey{}, scope)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// ScopeFrom returns the verified scope set by Auth.
func ScopeFrom(ctx context.Context) (Scope, bool) {
	scope, ok := ctx.Value(scopeKey{}).(Scope)
	return scope, ok
}

// bearerToken extracts the token from an "Authorization: Bearer <token>" header.
func bearerToken(header string) (string, bool) {
	const prefix = "Bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	if token == "" {
		return "", false
	}
	return token, true
}
