package api

import (
	"context"
	"encoding/json"
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
type WebhookAPI interface {
	CreateEndpoint(ctx context.Context, org, project string, c automation.EndpointCreate) (string, error)
	ListEndpoints(ctx context.Context, org, project string) ([]automation.EndpointView, error)
	ListDeliveries(ctx context.Context, org, project, state string, limit int) ([]automation.DeliveryView, error)
	GetDelivery(ctx context.Context, org, project, id string) (*automation.DeliveryView, bool, error)
	ListAttempts(ctx context.Context, org, project, deliveryID string) ([]automation.AttemptView, error)
	Redeliver(ctx context.Context, org, project, id string) (bool, error)
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
	id, err := h.webhooks.CreateEndpoint(r.Context(), scope.Organization, scope.Project, automation.EndpointCreate{
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
	w.Header().Set("Location", "/v1/webhook-endpoints/"+id)
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

// listEndpoints returns the scope's endpoints (spec §21.4 GET /v1/webhook-endpoints).
func (h *webhookHandler) listEndpoints(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	endpoints, err := h.webhooks.ListEndpoints(r.Context(), scope.Organization, scope.Project)
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
	deliveries, err := h.webhooks.ListDeliveries(r.Context(), scope.Organization, scope.Project, r.URL.Query().Get("state"), limit)
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
	delivery, found, err := h.webhooks.GetDelivery(r.Context(), scope.Organization, scope.Project, id)
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	if !found {
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such webhook delivery")
		return
	}
	attempts, err := h.webhooks.ListAttempts(r.Context(), scope.Organization, scope.Project, id)
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
	found, err := h.webhooks.Redeliver(r.Context(), scope.Organization, scope.Project, id)
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	if !found {
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
