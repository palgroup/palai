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
type Scope struct {
	Organization string
	Project      string
	Principal    string
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
			// request issues runs under palai.org_id / palai.project_id and migration 000029's
			// policies enforce the same boundary the handlers' WHERE clauses claim. This is the
			// ONLY place a request's tenant enters the DB scope — it comes from the credential,
			// never from a body field (spec §39.2).
			ctx := storage.WithTenant(r.Context(), scope.Organization, scope.Project)
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
