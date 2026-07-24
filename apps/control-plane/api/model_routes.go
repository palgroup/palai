package api

import (
	"context"
	"io"
	"net/http"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// ModelRouteAPI is the store seam for the DB-backed model-routing write surface (E13 Task 8, spec §27.2 /
// §27.6): per-project connections (a provider family bound to a secret REF) and routes carrying immutable,
// publishable revisions — the E11 revision shape. The Postgres store implements it; production wires it via
// WithModelRoutes, and tiers that never touch routing leave it unset so the routes stay unmounted.
//
// Every method is scoped by the verified identity, never a body field: a route or connection id from
// another tenant is answered exactly like one that never existed.
type ModelRouteAPI interface {
	CreateModelConnection(ctx context.Context, scope middleware.Scope, body []byte) (ProvisionResult, error)
	CreateModelRoute(ctx context.Context, scope middleware.Scope, body []byte) (ProvisionResult, error)
	CreateModelRouteRevision(ctx context.Context, scope middleware.Scope, routeID string, body []byte) (ProvisionResult, error)
	PublishModelRouteRevision(ctx context.Context, scope middleware.Scope, routeID, revisionID string) (ProvisionResult, error)

	// The E16 T1 read-back half (the E13 T10 write-only gap): every connection/route/revision is
	// readable within the caller's scope. LISTs render the admin ListView envelope (a full, small,
	// tenant-scoped set — no cursor); a singular GET renders one projection, or NotFound (404) for an
	// absent/foreign id. A connection projection carries the secret REF name only, never a value.
	ListModelConnections(ctx context.Context, scope middleware.Scope) (ProvisionResult, error)
	GetModelConnection(ctx context.Context, scope middleware.Scope, connectionID string) (ProvisionResult, error)
	ListModelRoutes(ctx context.Context, scope middleware.Scope) (ProvisionResult, error)
	GetModelRoute(ctx context.Context, scope middleware.Scope, routeID string) (ProvisionResult, error)
	ListModelRouteRevisions(ctx context.Context, scope middleware.Scope, routeID string) (ProvisionResult, error)
	GetModelRouteRevision(ctx context.Context, scope middleware.Scope, routeID, revisionID string) (ProvisionResult, error)
}

// modelRouteHandler renders the model-routing routes. Like the secret-ref surface it reuses ProvisionResult
// and the provision-capability gate — choosing a project's model and credential is an org-admin operation,
// not something a run's own key does — and keeps its auth/render plumbing local rather than shared with the
// other two management handlers, so this surface stays decoupled from theirs.
type modelRouteHandler struct {
	routes ModelRouteAPI
}

// createConnection binds a provider family to a secret-ref handle (POST /v1/model-connections). The body
// carries a REFERENCE; a request that tries to inline a credential value is rejected by the store's strict
// decode as an unsupported field.
func (h *modelRouteHandler) createConnection(w http.ResponseWriter, r *http.Request) {
	scope, raw, ok := h.begin(w, r)
	if !ok {
		return
	}
	out, err := h.routes.CreateModelConnection(r.Context(), scope, raw)
	h.write(w, r, out, err, http.StatusCreated)
}

// createRoute opens a named route alias for the project (POST /v1/model-routes).
func (h *modelRouteHandler) createRoute(w http.ResponseWriter, r *http.Request) {
	scope, raw, ok := h.begin(w, r)
	if !ok {
		return
	}
	out, err := h.routes.CreateModelRoute(r.Context(), scope, raw)
	h.write(w, r, out, err, http.StatusCreated)
}

// createRevision adds a DRAFT revision to a route (POST /v1/model-routes/{route_id}/revisions). A draft
// never steers a run; publishing it does.
func (h *modelRouteHandler) createRevision(w http.ResponseWriter, r *http.Request) {
	scope, raw, ok := h.begin(w, r)
	if !ok {
		return
	}
	out, err := h.routes.CreateModelRouteRevision(r.Context(), scope, r.PathValue("route_id"), raw)
	h.write(w, r, out, err, http.StatusCreated)
}

// publishRevision makes a draft revision the project's routed target
// (POST /v1/model-routes/{route_id}/revisions/{revision_id}/publish). Re-publishing is idempotent.
func (h *modelRouteHandler) publishRevision(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.authorize(w, r)
	if !ok {
		return
	}
	out, err := h.routes.PublishModelRouteRevision(r.Context(), scope, r.PathValue("route_id"), r.PathValue("revision_id"))
	h.write(w, r, out, err, http.StatusOK)
}

// The read-back handlers (E16 T1). Each authorizes (provision capability), then renders the store's
// ListView envelope (LIST) or single projection / NotFound (GET) through the shared write helper — the
// scope comes from the verified identity, never a path or query field.
func (h *modelRouteHandler) listConnections(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.authorize(w, r)
	if !ok {
		return
	}
	out, err := h.routes.ListModelConnections(r.Context(), scope)
	h.write(w, r, out, err, http.StatusOK)
}

func (h *modelRouteHandler) getConnection(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.authorize(w, r)
	if !ok {
		return
	}
	out, err := h.routes.GetModelConnection(r.Context(), scope, r.PathValue("connection_id"))
	h.write(w, r, out, err, http.StatusOK)
}

func (h *modelRouteHandler) listRoutes(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.authorize(w, r)
	if !ok {
		return
	}
	out, err := h.routes.ListModelRoutes(r.Context(), scope)
	h.write(w, r, out, err, http.StatusOK)
}

func (h *modelRouteHandler) getRoute(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.authorize(w, r)
	if !ok {
		return
	}
	out, err := h.routes.GetModelRoute(r.Context(), scope, r.PathValue("route_id"))
	h.write(w, r, out, err, http.StatusOK)
}

func (h *modelRouteHandler) listRevisions(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.authorize(w, r)
	if !ok {
		return
	}
	out, err := h.routes.ListModelRouteRevisions(r.Context(), scope, r.PathValue("route_id"))
	h.write(w, r, out, err, http.StatusOK)
}

func (h *modelRouteHandler) getRevision(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.authorize(w, r)
	if !ok {
		return
	}
	out, err := h.routes.GetModelRouteRevision(r.Context(), scope, r.PathValue("route_id"), r.PathValue("revision_id"))
	h.write(w, r, out, err, http.StatusOK)
}

func (h *modelRouteHandler) authorize(w http.ResponseWriter, r *http.Request) (middleware.Scope, bool) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return middleware.Scope{}, false
	}
	if !scope.HasScope(provisionScope) {
		middleware.WriteProblem(w, r, http.StatusForbidden, "insufficient_scope", "this API key lacks the provision capability")
		return middleware.Scope{}, false
	}
	return scope, true
}

func (h *modelRouteHandler) begin(w http.ResponseWriter, r *http.Request) (middleware.Scope, []byte, bool) {
	scope, ok := h.authorize(w, r)
	if !ok {
		return middleware.Scope{}, nil, false
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the request body could not be read")
		return middleware.Scope{}, nil, false
	}
	return scope, raw, true
}

// write renders a routing outcome: the typed rejects first, then 2xx with the projection. NotFound covers
// both an absent id and one owned by another tenant — the same 404, so no read confirms another tenant's
// route exists.
func (h *modelRouteHandler) write(w http.ResponseWriter, r *http.Request, out ProvisionResult, err error, okStatus int) {
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	switch {
	case out.MissingField != "":
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", out.MissingField+" is required")
		return
	case out.BadField:
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the request body carries an unsupported field")
		return
	case out.NotFound:
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such model route, revision, or connection in this scope")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(okStatus)
	_, _ = w.Write(out.Body)
}
