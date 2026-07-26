package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/palgroup/palai/adapters/integrations/webhook"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/automation"
)

// QueueConnectionAPI is the store seam for the minimal queue-binding admin surface (E19 T6, spec §34.1).
// The automation QueueStore implements it; tiers that never touch queues pass nil and the routes stay
// unmounted — which is also what lets discovery derive `queues` from an actual mount rather than a static
// string (§2). Every method is scoped by the VERIFIED identity, never a request-body field (§39.2), so a
// connection is always created inside the caller's own tenant and its server-minted id is the only key
// anything later resolves it by.
type QueueConnectionAPI interface {
	CreateQueueConnection(ctx context.Context, org, project string, in automation.QueueConnectionInput) (string, error)
	ListQueueConnections(ctx context.Context, org, project string, w automation.ListWindow) ([]automation.QueueConnectionItem, error)
}

type queueConnectionHandler struct {
	queues QueueConnectionAPI
	// resolver vets an outbound destination hostname at registration (the webhook-endpoint fail-fast SSRF
	// gate). Injectable for tests; production uses net.DefaultResolver.
	resolver webhook.Resolver
}

// queueConfigFields are the ONLY keys a connection's config may carry, per direction. The decode is STRICT
// (an unknown key is a 400) and that is a security property rather than tidiness: config is tenant-supplied
// JSONB that the bridge reads a run target out of, so an open shape is an invitation to park a bearer token
// or a second "tenant" field in it. Secret material lives in secret_refs (000031); this column never holds
// a credential, and a strict shape is how that stays true without anyone having to remember it.
//
// Note what is NOT here: no organization_id, no project_id, no session id. A connection cannot describe a
// tenant even to itself — the row's own columns are the tenant, and they were written from the verified
// scope at create.
type queueInboundConfig struct {
	// AgentRevisionID pins the run every message on this connection admits with; PrincipalID is the
	// identity it runs as. Both are required — a binding that has not been told what to run, or as whom,
	// admits nothing (automation.ErrQueueNoRunTarget).
	AgentRevisionID string `json:"agent_revision_id"`
	PrincipalID     string `json:"principal_id"`
}

type queueOutboundConfig struct {
	DestinationURL string `json:"destination_url"`
	AllowPrivate   bool   `json:"allow_private"`
	TimeoutMS      int    `json:"timeout_ms"`
}

// createConnection registers a queue binding (POST /v1/queue-connections). Durable config, server-minted
// id, no Idempotency-Key (the sibling POST /v1/webhook-endpoints posture: a rare operator action whose
// duplicate is operator-visible).
func (h *queueConnectionHandler) createConnection(w http.ResponseWriter, r *http.Request) {
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
		Name          string          `json:"name"`
		Kind          string          `json:"kind"`
		Direction     string          `json:"direction"`
		Capacity      int             `json:"capacity"`
		VisibilitySec int             `json:"visibility_seconds"`
		MaxDeliveries int             `json:"max_deliveries"`
		Config        json.RawMessage `json:"config"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the request body is not valid JSON")
		return
	}
	if body.Name == "" {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "name is required")
		return
	}
	// `local` is the only broker class this binary can serve: the Postgres-durable reference adapter. A
	// deployment cannot register an `sqs`/`pubsub`/`kafka` binding, because no such adapter is written —
	// accepting one would create a connection that silently consumes nothing, the registration-side twin
	// of discovery advertising an unmounted surface (§6 leg 5).
	if body.Kind != "" && body.Kind != "local" {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request",
			"kind must be \"local\": no broker-product adapter is built into this deployment")
		return
	}
	if body.Direction != "inbound" && body.Direction != "outbound" {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "direction must be \"inbound\" or \"outbound\"")
		return
	}
	capacity, ok := boundOrDefault(body.Capacity, 1, 100000, 1024)
	if !ok {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "capacity must be between 1 and 100000")
		return
	}
	visibility, ok := boundOrDefault(body.VisibilitySec, 1, 3600, 30)
	if !ok {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "visibility_seconds must be between 1 and 3600")
		return
	}
	deliveries, ok := boundOrDefault(body.MaxDeliveries, 1, 100, 20)
	if !ok {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "max_deliveries must be between 1 and 100")
		return
	}
	config, detail := h.vetConfig(r, body.Direction, body.Config)
	if detail != "" {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", detail)
		return
	}

	id, err := h.queues.CreateQueueConnection(r.Context(), scope.Organization, scope.Project, automation.QueueConnectionInput{
		Name:          body.Name,
		Kind:          "local",
		Direction:     body.Direction,
		Capacity:      capacity,
		Visibility:    secondsDuration(visibility),
		MaxDeliveries: deliveries,
		Config:        config,
	})
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	w.Header().Set("Location", "/v1/queue-connections/"+id)
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "object": "queue_connection"})
}

// vetConfig strict-decodes the direction's config shape and returns the canonical bytes to store, or a
// problem detail. It refuses at the trust boundary rather than leaving a connection that can never work:
// an inbound binding without a run target admits nothing, and an outbound one without a destination
// accumulates undeliverable rows.
func (h *queueConnectionHandler) vetConfig(r *http.Request, direction string, raw json.RawMessage) ([]byte, string) {
	if len(raw) == 0 {
		return nil, "config is required"
	}
	if direction == "inbound" {
		var cfg queueInboundConfig
		if err := strictDecodeQueueConfig(raw, &cfg); err != nil {
			return nil, "config accepts only agent_revision_id and principal_id for an inbound connection"
		}
		if cfg.AgentRevisionID == "" || cfg.PrincipalID == "" {
			return nil, "config.agent_revision_id and config.principal_id are required for an inbound connection"
		}
		out, err := json.Marshal(cfg)
		if err != nil {
			return nil, "config could not be encoded"
		}
		return out, ""
	}
	var cfg queueOutboundConfig
	if err := strictDecodeQueueConfig(raw, &cfg); err != nil {
		return nil, "config accepts only destination_url, allow_private and timeout_ms for an outbound connection"
	}
	if cfg.DestinationURL == "" {
		return nil, "config.destination_url is required for an outbound connection"
	}
	if cfg.TimeoutMS != 0 {
		if _, ok := boundOrDefault(cfg.TimeoutMS, 1, 30000, 10000); !ok {
			return nil, "config.timeout_ms must be between 1 and 30000"
		}
	}
	// The same create-time egress gate POST /v1/webhook-endpoints applies (AUT-012 fail-fast half): https
	// only unless the self-host flag is set, and a private/loopback/metadata destination is refused. The
	// authoritative gate is still the sender's per-attempt re-resolve + IP pin; this is operator feedback.
	if err := webhook.VetDestination(r.Context(), h.resolver, cfg.DestinationURL, cfg.AllowPrivate); err != nil {
		return nil, "config.destination_url is not an allowed destination"
	}
	out, err := json.Marshal(cfg)
	if err != nil {
		return nil, "config could not be encoded"
	}
	return out, ""
}

// strictDecodeQueueConfig decodes into the direction's exact shape and REJECTS an unknown key. This is the
// one line that keeps the config column from becoming a free-form bag.
func strictDecodeQueueConfig(raw json.RawMessage, into any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(into)
}

func secondsDuration(s int) time.Duration { return time.Duration(s) * time.Second }

// listConnections returns a tenant-scoped page of connections (GET /v1/queue-connections), in the shared
// keyset page envelope.
func (h *queueConnectionHandler) listConnections(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	q, ok := beginList(w, r, "queue_connection", scope)
	if !ok {
		return
	}
	window := automation.ListWindow{
		CreatedGTE: q.CreatedGTE, CreatedLTE: q.CreatedLTE, Limit: q.Limit + 1, // +1 over-fetch: renderPage reads it as has_more
	}
	if q.After != nil {
		window.AfterCreatedAt, window.AfterID = &q.After.CreatedAt, q.After.ID
	}
	items, err := h.queues.ListQueueConnections(r.Context(), scope.Organization, scope.Project, window)
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	rows := make([]ListRow, 0, len(items))
	for _, it := range items {
		body, _ := json.Marshal(map[string]any{
			"id": it.ID, "object": "queue_connection", "name": it.Name, "kind": it.Kind,
			"direction": it.Direction, "capacity": it.Capacity, "visibility_seconds": it.Visibility,
			"max_deliveries": it.MaxDeliveries, "enabled": it.Enabled, "config": it.Config,
		})
		rows = append(rows, ListRow{ID: it.ID, CreatedAt: it.CreatedAt, Body: body})
	}
	renderPage(w, r, "queue_connection", scope, rows, q.Limit)
}
