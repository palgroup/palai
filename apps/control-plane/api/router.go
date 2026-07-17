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
// idempotency-key requirement is scoped to the mutating route. The event stream is
// a plain GET (no idempotency key) that reads the journal through events.
func NewRouter(verifier middleware.Verifier, admitter Admitter, events EventReader, sse SSEConfig) http.Handler {
	mux := http.NewServeMux()
	responses := &responseHandler{admitter: admitter}
	mux.Handle("POST /v1/responses", middleware.RequireIdempotencyKey(http.HandlerFunc(responses.create)))

	stream := &eventsHandler{reader: events, cfg: sse.withDefaults()}
	mux.HandleFunc("GET /v1/sessions/{session_id}/events", stream.stream)

	var root http.Handler = mux
	root = middleware.Auth(verifier)(root)
	root = middleware.RequestContext(root)
	return root
}
