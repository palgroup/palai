package middleware

import (
	"context"
	"net/http"
	"strings"
)

type idempotencyKeyCtx struct{}

// RequireIdempotencyKey rejects a mutation that omits the Idempotency-Key header
// (spec §20.9: all externally visible mutations must accept one). The reservation
// itself happens later, atomically with the side effect.
func RequireIdempotencyKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if key == "" {
			WriteProblem(w, r, http.StatusBadRequest, "missing_idempotency_key", "the Idempotency-Key header is required")
			return
		}
		ctx := context.WithValue(r.Context(), idempotencyKeyCtx{}, key)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// IdempotencyKey returns the key required by RequireIdempotencyKey.
func IdempotencyKey(ctx context.Context) string {
	key, _ := ctx.Value(idempotencyKeyCtx{}).(string)
	return key
}
