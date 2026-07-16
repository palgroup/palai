// Package middleware holds the control-plane HTTP plumbing: request context,
// bearer authentication, idempotency-key enforcement, and RFC 9457 problem
// rendering. Each middleware is a single responsibility so the router composes
// them explicitly (spec §20.10, §20.9).
package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

// APIVersion is the dated API version this build serves (spec §20.13).
const APIVersion = "2026-07-16"

type requestIDKey struct{}

// RequestContext assigns a request id and stamps the correlation headers on every
// response, including error responses, then threads the id through the context so
// problem bodies can carry it.
func RequestContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := NewID("req")
		w.Header().Set("Request-Id", id)
		w.Header().Set("API-Version", APIVersion)
		ctx := context.WithValue(r.Context(), requestIDKey{}, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequestID returns the correlation id assigned by RequestContext, or "" if unset.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// NewID mints an opaque, prefixed, URL-safe identifier. crypto/rand never fails on
// supported platforms (it panics internally on OS entropy failure).
func NewID(prefix string) string {
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	return prefix + "_" + hex.EncodeToString(raw[:])
}
