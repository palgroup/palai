// Package api is the control-plane HTTP surface. NewRouter composes the middleware
// stack around the response-admission handler; the durable work is delegated to an
// Admitter seam so the HTTP contract is exercised without a database.
package api

import (
	"net/http"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// NewRouter builds the LP-0 HTTP handler. RequestContext is outermost so every
// response — success or problem — carries the correlation headers; Auth runs
// before routing so an unauthenticated request never reaches a handler; the
// idempotency-key requirement is scoped to the mutating route.
func NewRouter(verifier middleware.Verifier, admitter Admitter) http.Handler {
	mux := http.NewServeMux()
	handler := &responseHandler{admitter: admitter}
	mux.Handle("POST /v1/responses", middleware.RequireIdempotencyKey(http.HandlerFunc(handler.create)))

	var root http.Handler = mux
	root = middleware.Auth(verifier)(root)
	root = middleware.RequestContext(root)
	return root
}
