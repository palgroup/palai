package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/palgroup/palai/adapters/integrations/webhook"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/automation"
)

// WebhookAPI is the store seam for the outbound-webhook resources (spec §21.4-21.6). The automation
// WebhookStore implements it; production wires it, and tiers that do not touch webhooks (the
// conformance HTTP tier, the SSE read-path e2e) pass nil, so the routes stay unmounted there. Every
// method is scoped by the verified identity, never a request-body field (§39.2).
//
// No organization parameter (A.2 Task 3): the request scope no longer resolves one, and WebhookStore
// resolves it fresh from project where it still needs one internally.
type WebhookAPI interface {
	CreateEndpoint(ctx context.Context, project string, c automation.EndpointCreate) (string, error)
	ListEndpoints(ctx context.Context, project string) ([]automation.EndpointView, error)
	GetEndpoint(ctx context.Context, project, id string) (*automation.EndpointView, bool, error)
	DeleteEndpoint(ctx context.Context, project, id string) (bool, error)
	ListDeliveries(ctx context.Context, project, state string, limit int) ([]automation.DeliveryView, error)
	GetDelivery(ctx context.Context, project, id string) (*automation.DeliveryView, bool, error)
	ListAttempts(ctx context.Context, project, deliveryID string) ([]automation.AttemptView, error)
	Redeliver(ctx context.Context, project, id string) (bool, error)
}

type webhookHandler struct {
	webhooks WebhookAPI
	// resolver vets a registration-time hostname against the egress policy (fail-fast SSRF gate). It
	// is injectable so a test drives a deterministic resolution; production uses net.DefaultResolver.
	resolver webhook.Resolver
}

// createEndpoint registers a webhook endpoint (spec §21.4 POST /v1/webhook-endpoints). The URL is
// egress-vetted at create time (AUT-012 static half): a private/loopback/metadata destination is
// rejected unless the endpoint sets allow_private_destination (the self-host allowlist flag). The
// signing secrets arrive as SecretRef handles, never plaintext — the pump resolves them at delivery.
func (h *webhookHandler) createEndpoint(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the request body could not be read")
		return
	}
	var body struct {
		URL                     string            `json:"url"`
		EventFilter             []string          `json:"event_filter"`
		APIRevision             string            `json:"api_revision"`
		SigningSecretRef        string            `json:"signing_secret_ref"`
		SigningSecretRefNext    string            `json:"signing_secret_ref_next"`
		FixedHeaders            map[string]string `json:"fixed_headers"`
		TimeoutMS               int               `json:"timeout_ms"`
		MaxAttempts             int               `json:"max_attempts"`
		RetryWindowSeconds      int               `json:"retry_window_seconds"`
		AllowPrivateDestination bool              `json:"allow_private_destination"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the request body is not valid JSON")
		return
	}
	if body.URL == "" {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "url is required")
		return
	}
	// Create-time egress gate (AUT-012 fail-fast half): https is required (http only with the flag),
	// and a private/loopback/link-local/metadata destination — a literal IP OR a host that already
	// resolves into one of those ranges — is denied unless the self-host allowlist flag is set (spec
	// §21.4). Attempt-time re-resolution + IP pinning (the pump's sender) is the authoritative gate
	// that closes DNS rebinding; this one is fast operator feedback. The rejection is typed and never
	// echoes the target URL back.
	if err := webhook.VetDestination(r.Context(), h.resolver, body.URL, body.AllowPrivateDestination); err != nil {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "url is not an allowed webhook destination")
		return
	}
	// Bound the delivery policy at the trust boundary (F4/F9): an out-of-range value is a typed 400, not
	// a DB-CHECK 500; an omitted (0) value takes the platform default. This also caps timeout_ms so no
	// endpoint can hold a delivery worker longer than the platform maximum (a tarpit-amplification bound).
	timeout, ok := boundOrDefault(body.TimeoutMS, 1, 30000, 10000)
	if !ok {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "timeout_ms must be between 1 and 30000")
		return
	}
	attempts, ok := boundOrDefault(body.MaxAttempts, 1, 50, 20)
	if !ok {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "max_attempts must be between 1 and 50")
		return
	}
	window, ok := boundOrDefault(body.RetryWindowSeconds, 1, 7*24*3600, 72*3600)
	if !ok {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "retry_window_seconds is out of range")
		return
	}

	// ponytail (F10): no Idempotency-Key — this matches the sibling durable create POST
	// /v1/repository-bindings (a re-post registers a distinct resource). Endpoint creation is a rare
	// operator action, and a duplicate endpoint is operator-visible + deletable. Full idempotent-create
	// via the idempotency_records admission tx (the /v1/responses path) is the upgrade path, deferred.
	id, err := h.webhooks.CreateEndpoint(r.Context(), scope.Project, automation.EndpointCreate{
		URL:                     body.URL,
		EventFilter:             body.EventFilter,
		APIRevision:             body.APIRevision,
		SigningSecretRef:        body.SigningSecretRef,
		SigningSecretRefNext:    body.SigningSecretRefNext,
		FixedHeaders:            body.FixedHeaders,
		TimeoutMS:               timeout,
		MaxAttempts:             attempts,
		RetryWindowSeconds:      window,
		AllowPrivateDestination: body.AllowPrivateDestination,
	})
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	// The Location header, returned by E29 T3 on exactly the condition E29 T2 named when it removed it: the
	// family now has a singular read, so `/v1/webhook-endpoints/<id>` resolves and following the header lands
	// on the resource instead of Go's bare mux miss. The dangling-Location guard accepts it by probing the
	// shipped router, not by being told.
	w.Header().Set("Location", "/v1/webhook-endpoints/"+id)
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

// getEndpoint reads one endpoint (spec §21.4 GET /v1/webhook-endpoints/{endpoint_id}). It renders the SAME
// automation.EndpointView the list does — the projection lives in one place precisely so the two routes
// cannot drift into describing two different resources.
//
// It is NOT provision-gated, and deliberately so: it is the singular form of a list that has been readable
// by any key since E11, and a caller that can enumerate every endpoint but not fetch one by id would be
// meeting an incoherent surface. The DELETE beside it IS gated, because destroying configuration is the
// org-admin act, not reading it.
func (h *webhookHandler) getEndpoint(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	endpoint, found, err := h.webhooks.GetEndpoint(r.Context(), scope.Project, r.PathValue("endpoint_id"))
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	if !found {
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such webhook endpoint in this project")
		return
	}
	writeJSON(w, http.StatusOK, endpoint)
}

// deleteEndpoint unregisters an endpoint (spec §21.4 DELETE /v1/webhook-endpoints/{endpoint_id}). It is the
// capability the create comment above has claimed since E11 and which nothing provided until E29 T3.
//
// 204 THEN 404, WHICH IS WHAT IDEMPOTENT MEANS HERE. DELETE is idempotent because a second call produces no
// further EFFECT — not because it produces the same status. The second call finds nothing, changes nothing,
// and says the resource is absent, which is true. The two destructive routes this tree already ships
// (DELETE /v1/schedules/{id}, DELETE /v1/slack-connections/{id}) answer exactly this way, and one posture
// across three routes is worth more to an operator than re-arguing it here.
//
// 409 WHEN A TRIGGER REVISION PINS IT. That is the one referrer whose foreign key still refuses the delete,
// and the refusal is kept: a trigger revision is immutable live configuration that will send to this
// endpoint the next time it fires. The alternative — deleting anyway — would leave a trigger pointed at
// nothing. The typed answer names what is in the way; a leaked constraint violation would be a 500 nobody
// can act on.
//
// WHAT IT DOES NOT TAKE. Delivery rows survive their endpoint, with the deleted id still on them. A delivery
// records a payload this deployment actually sent, and unregistering the address does not un-send it.
// Migration 000052 is what makes that true — the ON DELETE CASCADE it removed would have erased the trail.
func (h *webhookHandler) deleteEndpoint(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	if !scope.HasScope(provisionScope) {
		middleware.WriteProblem(w, r, http.StatusForbidden, "insufficient_scope", "this API key lacks the provision capability")
		return
	}
	deleted, err := h.webhooks.DeleteEndpoint(r.Context(), scope.Project, r.PathValue("endpoint_id"))
	switch {
	case errors.Is(err, automation.ErrEndpointPinned):
		middleware.WriteProblem(w, r, http.StatusConflict, "endpoint_pinned",
			"a trigger revision names this endpoint as its callback target; remove or re-publish that trigger first")
		return
	case err != nil:
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	case !deleted:
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such webhook endpoint in this project")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listEndpoints returns the scope's endpoints (spec §21.4 GET /v1/webhook-endpoints).
func (h *webhookHandler) listEndpoints(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	endpoints, err := h.webhooks.ListEndpoints(r.Context(), scope.Project)
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": endpoints})
}

// listDeliveries returns the scope's deliveries, optionally filtered by ?state= (spec §21.6 GET
// /v1/webhook-deliveries) — the dead-letter view is ?state=dead.
func (h *webhookHandler) listDeliveries(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	deliveries, err := h.webhooks.ListDeliveries(r.Context(), scope.Project, r.URL.Query().Get("state"), limit)
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": deliveries})
}

// getDelivery returns one delivery and its sanitized attempt view (spec §21.6). The attempt view
// carries status/duration/excerpt only — the signing secret is structurally absent.
func (h *webhookHandler) getDelivery(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	id := r.PathValue("delivery_id")
	delivery, found, err := h.webhooks.GetDelivery(r.Context(), scope.Project, id)
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	if !found {
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such webhook delivery")
		return
	}
	attempts, err := h.webhooks.ListAttempts(r.Context(), scope.Project, id)
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"delivery": delivery, "attempts": attempts})
}

// redeliver revives a delivery with the same id + payload (spec §21.6 POST
// /v1/webhook-deliveries/{id}/redeliver). It is naturally idempotent (re-queuing an already-pending
// row is a no-op), so it needs no Idempotency-Key.
func (h *webhookHandler) redeliver(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	id := r.PathValue("delivery_id")
	found, err := h.webhooks.Redeliver(r.Context(), scope.Project, id)
	switch {
	// A delivery can outlive its endpoint from E29 T3 onward, and re-queuing one of those would answer 202
	// for work that can never happen — the pump's due-scan joins webhook_endpoints, so nothing would ever
	// attempt it. The delivery record is still readable; there is simply nowhere to send it.
	case errors.Is(err, automation.ErrDeliveryEndpointDeleted):
		middleware.WriteProblem(w, r, http.StatusConflict, "endpoint_deleted",
			"the endpoint this delivery was addressed to has been deleted, so it cannot be sent again")
		return
	case err != nil:
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	case !found:
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such webhook delivery")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"id": id, "state": "pending"})
}

// boundOrDefault returns (def, true) for a zero/unset value, (v, true) for v within [min,max], and
// (0, false) for an out-of-range value the caller maps to a 400.
func boundOrDefault(v, min, max, def int) (int, bool) {
	if v == 0 {
		return def, true
	}
	if v < min || v > max {
		return 0, false
	}
	return v, true
}

// writeJSON writes a JSON body with the given status.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
