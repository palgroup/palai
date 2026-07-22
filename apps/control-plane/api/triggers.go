package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/automation"
)

// TriggerAPI is the store seam for the trigger management surface + manual/API delivery ingestion (spec
// §20.2.2, E11 Task 2). The automation TriggerStore implements it; production wires it, and tiers that do
// not touch triggers pass nil so the routes stay unmounted. Every method is scoped by the verified
// identity, never a request-body field (§39.2). A delivery admits AS the verified principal.
type TriggerAPI interface {
	CreateTrigger(ctx context.Context, org, project, principal, name, triggerType string) (string, error)
	ReviseTrigger(ctx context.Context, org, project, triggerID string, in automation.TriggerRevisionInput) (automation.TriggerRevision, error)
	GetTrigger(ctx context.Context, org, project, triggerID string) (automation.TriggerView, bool, error)
	CreateDeliveryIdempotent(ctx context.Context, org, project, principal, triggerID, idempotencyKey string, payload []byte) (automation.DeliveryResult, error)
	GetDelivery(ctx context.Context, org, project, deliveryID string) (automation.TriggerDeliveryView, bool, error)
	// IngestInbound is the unauthenticated signed-webhook receiver (E11 Task 5): the source signature is
	// the auth. Global by server-minted id; verification precedes persistence; a durable row commits before
	// the ack. Mounted on the top mux (see router.go).
	IngestInbound(ctx context.Context, triggerID string, headers map[string]string, rawBody []byte) (automation.InboundResult, error)
	// SetInboundSecretRefs rotates the trigger's inbound source-secret handles in place (the PATCH surface;
	// rotation is not a pipeline revision).
	SetInboundSecretRefs(ctx context.Context, org, project, triggerID, ref, refNext string) error
}

type triggerHandler struct {
	triggers TriggerAPI
}

// createTrigger registers a trigger lineage (POST /v1/triggers). Durable config, not an idempotent
// operation, so no Idempotency-Key — the API mints the id server-side.
func (h *triggerHandler) createTrigger(w http.ResponseWriter, r *http.Request) {
	scope, raw, ok := h.begin(w, r)
	if !ok {
		return
	}
	var body struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the request body is not valid JSON")
		return
	}
	if body.Name == "" {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "name is required")
		return
	}
	id, err := h.triggers.CreateTrigger(r.Context(), scope.Organization, scope.Project, scope.Principal, body.Name, body.Type)
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	w.Header().Set("Location", "/v1/triggers/"+id)
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

// reviseInboundSecret rotates a trigger's inbound source-secret handles (PATCH /v1/triggers/{trigger_id}).
// It accepts ONLY inbound_secret_ref / inbound_secret_ref_next — rotation is a mutable-endpoint-column
// write, NOT a config revise (a revise would mint a pipeline revision). The refs are handles, never bytes.
func (h *triggerHandler) reviseInboundSecret(w http.ResponseWriter, r *http.Request) {
	scope, raw, ok := h.begin(w, r)
	if !ok {
		return
	}
	var body struct {
		InboundSecretRef     string `json:"inbound_secret_ref"`
		InboundSecretRefNext string `json:"inbound_secret_ref_next"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the request body is not valid JSON")
		return
	}
	err := h.triggers.SetInboundSecretRefs(r.Context(), scope.Organization, scope.Project, r.PathValue("trigger_id"), body.InboundSecretRef, body.InboundSecretRefNext)
	if errors.Is(err, automation.ErrTriggerNotFound) {
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such trigger in this project")
		return
	}
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": r.PathValue("trigger_id"),
		"inbound_secret_ref": body.InboundSecretRef, "inbound_secret_ref_next": body.InboundSecretRefNext})
}

// reviseTrigger creates a NEW immutable revision under a trigger (POST /v1/triggers/{trigger_id}/revisions).
// A malformed/escape-carrying input mapping is a 400 (rejected at compile, fail-closed); an unknown
// trigger is a 404.
func (h *triggerHandler) reviseTrigger(w http.ResponseWriter, r *http.Request) {
	scope, raw, ok := h.begin(w, r)
	if !ok {
		return
	}
	var body struct {
		AgentRevisionID       string          `json:"agent_revision_id"`
		RunTemplateRevisionID string          `json:"run_template_revision_id"`
		InputMapping          json.RawMessage `json:"input_mapping"`
		DedupeKeyExpr         json.RawMessage `json:"dedupe_key_expr"`
		CorrelationMode       string          `json:"correlation_mode"`
		CorrelationKeyExpr    json.RawMessage `json:"correlation_key_expr"`
		ConcurrencyPolicy     string          `json:"concurrency_policy"`
		OutputMapping         json.RawMessage `json:"output_mapping"`
		CallbackEndpointID    string          `json:"callback_endpoint_id"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the request body is not valid JSON")
		return
	}
	// The dedupe/correlation key exprs are JSON rule objects in the same mapping language (stored as
	// TEXT); carry the raw JSON through as the expr string.
	rev, err := h.triggers.ReviseTrigger(r.Context(), scope.Organization, scope.Project, r.PathValue("trigger_id"), automation.TriggerRevisionInput{
		AgentRevisionID:       body.AgentRevisionID,
		RunTemplateRevisionID: body.RunTemplateRevisionID,
		InputMapping:          body.InputMapping,
		DedupeKeyExpr:         string(body.DedupeKeyExpr),
		CorrelationMode:       body.CorrelationMode,
		CorrelationKeyExpr:    string(body.CorrelationKeyExpr),
		ConcurrencyPolicy:     body.ConcurrencyPolicy,
		OutputMapping:         body.OutputMapping,
		CallbackEndpointID:    body.CallbackEndpointID,
	})
	if errors.Is(err, automation.ErrTriggerNotFound) {
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such trigger in this project")
		return
	}
	// A callback_endpoint_id naming an unknown/foreign endpoint is a tenant-scoped 404 (no existence
	// disclosure) — the app-side scope guard that keeps a run result from reaching a foreign URL.
	if errors.Is(err, automation.ErrCallbackEndpointNotFound) {
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such callback endpoint in this project")
		return
	}
	if err != nil {
		// A compile/validation error is a bad request (a malformed mapping / disallowed verb / both pins).
		if isBadRevision(err) {
			middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the revision config is invalid")
			return
		}
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": rev.ID, "revision_number": rev.RevisionNumber})
}

// getTrigger returns a trigger's management projection (GET /v1/triggers/{trigger_id}).
func (h *triggerHandler) getTrigger(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	view, found, err := h.triggers.GetTrigger(r.Context(), scope.Organization, scope.Project, r.PathValue("trigger_id"))
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	if !found {
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such trigger in this project")
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// createDelivery ingests a manual/API delivery (POST /v1/triggers/{trigger_id}/deliveries) and drives it
// through the pipeline to a born run. The Idempotency-Key header is required (mounted); it scopes per-key
// delivery idempotency (AUT-013) — the same key + body resolves to one delivery/run, a reused key with a
// different body is a 409. The delivery admits AS the verified principal.
func (h *triggerHandler) createDelivery(w http.ResponseWriter, r *http.Request) {
	scope, raw, ok := h.begin(w, r)
	if !ok {
		return
	}
	key := middleware.IdempotencyKey(r.Context())
	del, err := h.triggers.CreateDeliveryIdempotent(r.Context(), scope.Organization, scope.Project, scope.Principal, r.PathValue("trigger_id"), key, raw)
	switch {
	case errors.Is(err, automation.ErrTriggerNotFound):
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such trigger in this project")
		return
	case errors.Is(err, automation.ErrTriggerDisabled):
		middleware.WriteProblem(w, r, http.StatusConflict, "trigger_disabled", "the trigger is disabled")
		return
	case errors.Is(err, automation.ErrNoActiveRevision):
		middleware.WriteProblem(w, r, http.StatusConflict, "trigger_no_revision", "the trigger has no revision to run; revise it first")
		return
	case errors.Is(err, automation.ErrIdempotencyMismatch):
		middleware.WriteProblem(w, r, http.StatusConflict, "idempotency_mismatch", "the idempotency key was reused with a different request")
		return
	case err != nil:
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	w.Header().Set("Location", "/v1/trigger-deliveries/"+del.ID)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"id": del.ID, "state": del.State, "response_id": del.ResponseID, "run_id": del.RunID,
		"session_id": del.SessionID, "duplicate_of": del.DuplicateOf, "reason": del.Reason,
	})
}

// getDelivery returns a delivery's operator-facing projection (GET /v1/trigger-deliveries/{delivery_id}).
func (h *triggerHandler) getDelivery(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	view, found, err := h.triggers.GetDelivery(r.Context(), scope.Organization, scope.Project, r.PathValue("delivery_id"))
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	if !found {
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such trigger delivery")
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// begin authenticates and reads the bounded body, shared by the mutating handlers.
func (h *triggerHandler) begin(w http.ResponseWriter, r *http.Request) (middleware.Scope, []byte, bool) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return middleware.Scope{}, nil, false
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the request body could not be read")
		return middleware.Scope{}, nil, false
	}
	return scope, raw, true
}

// isBadRevision reports whether a revise error is a client-fixable config problem (a mapping compile
// error, a disallowed secret ref, or both run targets pinned) rather than an infrastructure error.
func isBadRevision(err error) bool {
	return errors.Is(err, automation.ErrMappingVerb) ||
		errors.Is(err, automation.ErrMappingSchema) ||
		errors.Is(err, automation.ErrSecretNotAllowed) ||
		errors.Is(err, automation.ErrUnknownField) ||
		errors.Is(err, automation.ErrBothPins) ||
		errors.Is(err, automation.ErrNamedSessionCannotDefer) ||
		errors.Is(err, automation.ErrInvalidCorrelationMode) ||
		errors.Is(err, automation.ErrInvalidConcurrencyPolicy) ||
		errors.Is(err, automation.ErrReplaceNeedsKey)
}
