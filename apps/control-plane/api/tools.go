package api

import (
	"context"
	"io"
	"net/http"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// ToolRegistryAPI is the store seam for the E12 extensibility management surface (spec §20.2, §28.2-28.4):
// Tool lineages, immutable publishable ToolRevisions, and named publishable ToolSetRevisions. The Postgres
// store implements it; production wires it, and tiers that never touch the registry pass nil so the routes
// stay unmounted. It is scoped by the verified identity, never a request-body field (§39.2).
type ToolRegistryAPI interface {
	CreateTool(ctx context.Context, scope middleware.Scope, body []byte) (ToolResult, error)
	CreateToolRevision(ctx context.Context, scope middleware.Scope, toolID string, body []byte) (ToolResult, error)
	PublishToolRevision(ctx context.Context, scope middleware.Scope, revisionID string) (ToolResult, error)
	CreateToolSetRevision(ctx context.Context, scope middleware.Scope, setName string, body []byte) (ToolResult, error)
	PublishToolSetRevision(ctx context.Context, scope middleware.Scope, revisionID string) (ToolResult, error)
	// GetTool + ListTools + ListToolSets are the E13 T4 read side, RLS-scoped. A tool-set has no
	// single-resource GET (a set is consumed by name, not fetched by revision id) — LIST only.
	GetTool(ctx context.Context, scope middleware.Scope, id string) (ToolResult, error)
	ListTools(ctx context.Context, scope middleware.Scope, q ListQuery) ([]ListRow, error)
	ListToolSets(ctx context.Context, scope middleware.Scope, q ListQuery) ([]ListRow, error)
}

// ToolResult is a management projection. Exactly one outcome is set: Body carries the created/published
// resource (2xx); BadField marks a body outside the enforced config subset or a malformed canonical name /
// widening override (400); Conflict marks a name collision or a draft pin (409); NotFound marks an absent
// tool or pinned revision (404).
type ToolResult struct {
	Body     []byte
	BadField bool
	Conflict bool
	NotFound bool
}

type toolHandler struct {
	tools ToolRegistryAPI
}

// createTool registers a named tool lineage (POST /v1/tools). Durable config, not idempotent — the API
// mints the id server-side; the model-visible short name is derived server-side.
func (h *toolHandler) createTool(w http.ResponseWriter, r *http.Request) {
	scope, raw, ok := h.begin(w, r)
	if !ok {
		return
	}
	out, err := h.tools.CreateTool(r.Context(), scope, raw)
	h.write(w, r, out, err, http.StatusCreated, "/v1/tools/")
}

// createRevision creates a DRAFT tool revision (POST /v1/tools/{tool_id}/revisions). An unsupported field
// is a 400; an unknown tool is a 404.
func (h *toolHandler) createRevision(w http.ResponseWriter, r *http.Request) {
	scope, raw, ok := h.begin(w, r)
	if !ok {
		return
	}
	out, err := h.tools.CreateToolRevision(r.Context(), scope, r.PathValue("tool_id"), raw)
	h.write(w, r, out, err, http.StatusCreated, "/v1/tool-revisions/")
}

// publishRevision publishes a draft tool revision (POST /v1/tools/{tool_id}/revisions/{revision_id}/publish).
func (h *toolHandler) publishRevision(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	out, err := h.tools.PublishToolRevision(r.Context(), scope, r.PathValue("revision_id"))
	h.write(w, r, out, err, http.StatusOK, "")
}

// createSetRevision creates a DRAFT tool-set revision (POST /v1/tool-sets/{set}/revisions).
func (h *toolHandler) createSetRevision(w http.ResponseWriter, r *http.Request) {
	scope, raw, ok := h.begin(w, r)
	if !ok {
		return
	}
	out, err := h.tools.CreateToolSetRevision(r.Context(), scope, r.PathValue("set"), raw)
	h.write(w, r, out, err, http.StatusCreated, "/v1/tool-set-revisions/")
}

// publishSetRevision publishes a draft tool-set revision.
func (h *toolHandler) publishSetRevision(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	out, err := h.tools.PublishToolSetRevision(r.Context(), scope, r.PathValue("revision_id"))
	h.write(w, r, out, err, http.StatusOK, "")
}

// getTool reads one tool lineage (GET /v1/tools/{tool_id}).
func (h *toolHandler) getTool(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	out, err := h.tools.GetTool(r.Context(), scope, r.PathValue("tool_id"))
	h.write(w, r, out, err, http.StatusOK, "")
}

// listTools returns a tenant-scoped page of tool lineages (GET /v1/tools).
func (h *toolHandler) listTools(w http.ResponseWriter, r *http.Request) {
	h.listWith(w, r, "tools", h.tools.ListTools)
}

// listToolSets returns a tenant-scoped page of tool-set revisions (GET /v1/tool-sets).
func (h *toolHandler) listToolSets(w http.ResponseWriter, r *http.Request) {
	h.listWith(w, r, "tool-sets", h.tools.ListToolSets)
}

// listWith is the shared list flow for the registry's two list routes (tools, tool-sets).
func (h *toolHandler) listWith(w http.ResponseWriter, r *http.Request, kind string, fetch func(context.Context, middleware.Scope, ListQuery) ([]ListRow, error)) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	q, ok := beginList(w, r, kind, scope)
	if !ok {
		return
	}
	rows, err := fetch(r.Context(), scope, q)
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	renderPage(w, r, kind, scope, rows, q.Limit)
}

// begin authenticates and reads the bounded body, shared by the create handlers (the agentHandler twin).
func (h *toolHandler) begin(w http.ResponseWriter, r *http.Request) (middleware.Scope, []byte, bool) {
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

// write renders a management outcome: the typed rejects first, then 2xx with the resource (and a Location
// header for a create).
func (h *toolHandler) write(w http.ResponseWriter, r *http.Request, out ToolResult, err error, okStatus int, locationPrefix string) {
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	switch {
	case out.BadField:
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the request carries an unsupported field, a malformed canonical name, or a widening override")
		return
	case out.Conflict:
		middleware.WriteProblem(w, r, http.StatusConflict, "conflict", "the tool name is already taken, shadows a built-in, or a pinned revision is not published")
		return
	case out.NotFound:
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such tool or tool revision in this project")
		return
	}
	if locationPrefix != "" {
		w.Header().Set("Location", locationPrefix+resourceIDOf(out.Body))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(okStatus)
	_, _ = w.Write(out.Body)
}
