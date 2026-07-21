// Package api is the control-plane HTTP surface. NewRouter composes the middleware
// stack around the response-admission handler; the durable work is delegated to an
// Admitter seam so the HTTP contract is exercised without a database.
package api

import (
	"net"
	"net/http"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// NewRouter builds the LP-0 HTTP handler. RequestContext is outermost so every
// response — success or problem — carries the correlation headers; Auth runs
// before routing so an unauthenticated request never reaches a handler; the
// idempotency-key requirement is scoped to the mutating route. The event stream is
// a plain GET (no idempotency key) that reads the journal through events.
//
// runner, when non-nil, is the runner gateway surface (enrollment + mTLS session):
// it is mounted under /v1/runner/ ahead of and bypassing the public API auth and
// correlation middleware, because it carries its own one-use-token and mTLS identity.
// It is served over a separate mutually-authenticated listener; binding the CA and that
// listener is Task 12, so production passes nil until then.
func NewRouter(verifier middleware.Verifier, admitter Admitter, events EventReader, sessions SessionManager, bindings BindingRegistrar, agents AgentRegistry, webhooks WebhookAPI, sse SSEConfig, runner http.Handler) http.Handler {
	mux := http.NewServeMux()
	responses := &responseHandler{admitter: admitter}
	mux.Handle("POST /v1/responses", middleware.RequireIdempotencyKey(http.HandlerFunc(responses.create)))
	mux.HandleFunc("GET /v1/responses/{response_id}", responses.get)
	// Cancel is naturally idempotent (a canceled terminal is monotonic), so it is not wrapped
	// with RequireIdempotencyKey; the OpenAPI cancelResponse operation defines no key parameter.
	mux.HandleFunc("POST /v1/responses/{response_id}/cancel", responses.cancel)
	mux.HandleFunc("GET /v1/capabilities", capabilities)

	// Repository-binding registration (spec §30.1): a project registers the external repository its
	// coding sessions attach via the `repository` field. A durable, unkeyed create — nil in tiers that
	// do not touch bindings (the Docker-free conformance HTTP tier).
	if bindings != nil {
		bh := &bindingHandler{bindings: bindings}
		mux.HandleFunc("POST /v1/repository-bindings", bh.create)
	}

	// The automation-agent management surface (spec §20.2.1, §10, E11 Task 1): AgentProfiles +
	// immutable publishable AgentRevisions + profile-free RunTemplateRevisions. Durable config, not
	// idempotent operations, so no Idempotency-Key. nil in tiers that never touch agents.
	if agents != nil {
		ah := &agentHandler{agents: agents}
		mux.HandleFunc("POST /v1/agents", ah.createProfile)
		mux.HandleFunc("POST /v1/agents/{agent_id}/revisions", ah.createRevision)
		mux.HandleFunc("POST /v1/agents/{agent_id}/revisions/{revision_id}/publish", ah.publishRevision)
		mux.HandleFunc("POST /v1/run-templates/{template}/revisions", ah.createTemplateRevision)
		mux.HandleFunc("POST /v1/run-templates/{template}/revisions/{revision_id}/publish", ah.publishTemplateRevision)
	}

	stream := &eventsHandler{reader: events, cfg: sse.withDefaults()}
	mux.HandleFunc("GET /v1/sessions/{session_id}/events", stream.stream)

	// Outbound webhook endpoints + deliveries (spec §21.4-21.6). Durable project configuration and an
	// operator-facing delivery view + idempotent redelivery — nil in tiers that do not exercise
	// webhooks (the Docker-free conformance HTTP tier, the SSE read-path e2e).
	if webhooks != nil {
		wh := &webhookHandler{webhooks: webhooks, resolver: net.DefaultResolver}
		mux.HandleFunc("POST /v1/webhook-endpoints", wh.createEndpoint)
		mux.HandleFunc("GET /v1/webhook-endpoints", wh.listEndpoints)
		mux.HandleFunc("GET /v1/webhook-deliveries", wh.listDeliveries)
		mux.HandleFunc("GET /v1/webhook-deliveries/{delivery_id}", wh.getDelivery)
		mux.HandleFunc("POST /v1/webhook-deliveries/{delivery_id}/redeliver", wh.redeliver)
	}

	// The standalone session resource and its durable commands (spec §9.1, §22.4). Commands
	// carry their own idempotency (command_id), so the POST needs no Idempotency-Key header.
	// nil in tiers that do not exercise sessions (the Docker-free conformance HTTP tier).
	if sessions != nil {
		sh := &sessionHandler{sessions: sessions}
		mux.HandleFunc("POST /v1/sessions", sh.create)
		mux.HandleFunc("GET /v1/sessions/{session_id}", sh.get)
		mux.HandleFunc("POST /v1/sessions/{session_id}/commands", sh.command)
	}

	var root http.Handler = mux
	root = middleware.Auth(verifier)(root)
	root = middleware.RequestContext(root)

	// /healthz is an unauthenticated liveness probe the Compose stack's healthcheck
	// polls; it carries no contract surface (not in the OpenAPI spec) and bypasses auth
	// and correlation so a probe needs no credential. The runner gateway, when present,
	// mounts ahead of the public router with its own token/mTLS identity (see doc above).
	top := http.NewServeMux()
	top.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok"))
	})
	if runner != nil {
		top.Handle("/v1/runner/", runner)
	}
	top.Handle("/", root)
	return top
}
