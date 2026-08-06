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
// NO ORGANIZATION (A.2 Task 3, completed by Task 6): the request scope resolves a project and nothing
// above it. Palai is single-tenant per installation (storage/migrations/000062's header), and a project
// IS the tenant — there is no longer anything above it to resolve.
//
// THE CALL SITES THIS PARAGRAPH USED TO SEND ELSEWHERE ARE GONE, not relocated: it named
// storage.OrganizationForProject as where the handful of remaining readers went for the value, and
// migration 000067 dropped both the column that statement read and the organizations table it pointed at.
// The provisioning listing lists projects, coordinator.Tenant holds a project alone, and no response body
// carries an organization_id.
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
func (s Scope) HasSystem() bool { return s.carries(ScopeSystem) }

// carries is literal possession, with no empty-set rule. It is what a NEVER-INHERITED capability is
// tested with.
func (s Scope) carries(capability string) bool {
	for _, c := range s.Scopes {
		if c == capability {
			return true
		}
	}
	return false
}

// neverInherited are the capabilities the empty-set rule does NOT hand out. A key with no scopes is an
// unrestricted TENANT admin; it is not the platform, and it is not whatever privileged capability comes
// next. Membership here is the single place that distinction is declared, so adding a second privileged
// capability is one line rather than a second gate somebody has to remember to write.
var neverInherited = map[string]bool{ScopeSystem: true}

// CanGrant reports whether this key may MINT another key carrying capability.
//
// THE RULE IS THAT A KEY CANNOT HAND OUT WHAT IT DOES NOT HOLD, and it is deliberately expressed as
// "the caller must satisfy the same test that GATES the capability" rather than as a check against
// `system` by name — a rule written around one capability leaves the next privileged one undefended.
//
// The two arms differ because the underlying capabilities differ, not as a special case:
//   - a never-inherited capability needs LITERAL possession, exactly as its own gate reads it;
//   - every other capability goes through HasScope, so an unrestricted tenant-admin key (empty set) can
//     still mint the ordinary keys it has always been able to mint.
//
// Writing this with HasScope alone would defeat it entirely: HasScope answers TRUE for an empty set, so
// the least-scoped key in the system would be the one able to grant everything.
func (s Scope) CanGrant(capability string) bool {
	if neverInherited[capability] {
		return s.carries(capability)
	}
	return s.HasScope(capability)
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
			switch {
			case errors.Is(err, ErrInvalidToken):
				WriteProblem(w, r, http.StatusUnauthorized, "invalid_token", "the API key is not valid")
				return
			case err != nil:
				// ‼️ A VERIFIER THAT COULD NOT LOOK IS NOT A KEY THAT IS WRONG, AND EVERY ERROR ANSWERED
				// 401 UNTIL 2026-08-07. The store already draws the line — ErrInvalidToken for a hash that
				// matches no live credential, a wrapped error for anything else — and this branch threw it
				// away, so a control plane whose database had gone told every caller their credential was
				// invalid.
				//
				// Measured that night: the plane's Postgres container was stopped by a disk cleanup, and a
				// key that had worked all evening started answering `401 the API key is not valid`. The
				// hour that followed was spent on the key — its bytes, its trailing newline, the tunnel in
				// front of it, its row in a database that was not running — because the answer named the
				// credential. The plane's own /healthz said `ok` throughout.
				//
				// 503 is the honest code: the fault is the plane's, it is retryable, and it must not send
				// a paying customer to rotate a credential that is fine. The detail names the verification
				// and not the store, because "which dependency" is an operator's question and a caller
				// holding a valid key only needs to know it was not them.
				WriteProblem(w, r, http.StatusServiceUnavailable, "verification_unavailable",
					"the API key could not be verified — this is the control plane's fault, not the key's; retry shortly")
				return
			}
			// The verified scope is also published to the database layer, so every query this
			// request issues runs under palai.project_id and migration 000062's policies enforce the
			// same boundary the handlers' WHERE clauses claim. This is the ONLY place a request's
			// tenant enters the DB scope — it comes from the credential, never from a body field
			// (spec §39.2). There is no organization to publish (A.2 Task 3) and no palai.org_id left
			// to publish it into (A.2 Task 6): every tenant policy reads palai.project_id.
			//
			// IT NAMED THREE EXCEPTIONS AND THERE ARE NONE ON THIS PATH. environments,
			// environment_values and secret_refs carried no project column between A.2 and migration
			// 000006 and were reached through storage.WithInstallationScope; 000006 keys all three on
			// project_id (environment_values through its parent), so every table a request touches is
			// now covered by the scope published here. The only remaining WithInstallationScope caller
			// is coordinator's host quarantine, which no request path reaches.
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
