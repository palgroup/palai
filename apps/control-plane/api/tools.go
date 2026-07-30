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
	// PublishToolRevision takes an OPTIONAL body: the operator's approval declaration (E23 T5). It rides
	// the publish an operator already makes per tool rather than opening a second route, and an empty body
	// is the shipped bodyless publish.
	PublishToolRevision(ctx context.Context, scope middleware.Scope, revisionID string, body []byte) (ToolResult, error)
	CreateToolSetRevision(ctx context.Context, scope middleware.Scope, setName string, body []byte) (ToolResult, error)
	PublishToolSetRevision(ctx context.Context, scope middleware.Scope, revisionID string) (ToolResult, error)
	// GetTool + ListTools + ListToolSets are the E13 T4 read side, RLS-scoped.
	GetTool(ctx context.Context, scope middleware.Scope, id string) (ToolResult, error)
	ListTools(ctx context.Context, scope middleware.Scope, q ListQuery) ([]ListRow, error)
	ListToolSets(ctx context.Context, scope middleware.Scope, q ListQuery) ([]ListRow, error)
	// ListToolRevisions + GetToolSetRevision are the E25 T7 read side, and both close a hole that had a
	// SHIPPED DOCUMENT resting on it (plan §3.6 D14). The bool reports whether the lineage exists in
	// scope, so an unknown or foreign tool is a 404 and not an empty page.
	ListToolRevisions(ctx context.Context, scope middleware.Scope, toolID string, q ListQuery) ([]ListRow, bool, error)
	GetToolSetRevision(ctx context.Context, scope middleware.Scope, setName, revisionID string) (ToolResult, error)
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
//
// THE CEREMONY DOES NOT GROW, IT GAINS ONE QUESTION (E23 T5). The optional body carries the operator's
// approval declaration, on the call they already make once per tool — no new endpoint, no new CLI command.
// A bodyless publish decodes to the zero value, so every existing caller is bit-unchanged.
func (h *toolHandler) publishRevision(w http.ResponseWriter, r *http.Request) {
	scope, raw, ok := h.begin(w, r)
	if !ok {
		return
	}
	out, err := h.tools.PublishToolRevision(r.Context(), scope, r.PathValue("revision_id"), raw)
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

// listRevisions returns a tenant-scoped page of ONE lineage's revisions
// (GET /v1/tools/{tool_id}/revisions, E25 T7). An unknown or foreign tool id is a 404.
//
// THE PROJECTION CARRIES description AND input_schema, AND THAT IS THE ROUTE'S REASON RATHER THAN A
// CONVENIENCE: publishing a revision is the admin approval point for an untrusted description (000024's
// own words), and approving an id you cannot read is not approving anything. Those two fields are the
// remote server's bytes. They travel as DATA — a renderer that interprets them is the bug this surface
// makes reachable, which is why the console's guard is an AST test rather than a promise.
func (h *toolHandler) listRevisions(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.authorize(w, r)
	if !ok {
		return
	}
	// The cursor kind carries the tool id, for the reason agents.go's twin gives: a revisions list is
	// scoped to ONE lineage, so a cursor minted on tool A must not MAC-validate on tool B's revisions.
	kind := "tool-revisions:" + r.PathValue("tool_id")
	q, ok := beginList(w, r, kind, scope)
	if !ok {
		return
	}
	rows, found, err := h.tools.ListToolRevisions(r.Context(), scope, r.PathValue("tool_id"), q)
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	if !found {
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such tool in this project")
		return
	}
	renderPage(w, r, kind, scope, rows, q.Limit)
}

// getSetRevision reads one tool-set revision AND ITS PINS
// (GET /v1/tool-sets/{set}/revisions/{revision_id}, E25 T7). The list route projects a digest and a
// revision number; this is the only place the public API says WHICH TOOL REVISIONS a set actually grants.
func (h *toolHandler) getSetRevision(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.authorize(w, r)
	if !ok {
		return
	}
	out, err := h.tools.GetToolSetRevision(r.Context(), scope, r.PathValue("set"), r.PathValue("revision_id"))
	h.write(w, r, out, err, http.StatusOK, "")
}

// authorize resolves the verified scope and enforces the `provision` capability for the E25 T7 reads.
//
// THE ASYMMETRY IS DELIBERATE AND IT IS RECORDED HERE RATHER THAN INFERRED: the E12/E13 tool routes above
// carry no capability gate, and this task does not narrow them — retro-gating a shipped surface is a
// contract change, not a read route. What these two answer is the configuration an operator provisions
// (which upstream descriptions were approved, and what a set actually grants), so they sit with the other
// provisioned surfaces (environments, secret-refs, model-routes) rather than with the model-facing list.
// HONEST CEILING, and it is the one D8 measures: a key with an EMPTY scope set — every bootstrap and
// admin key, including the console's — holds `provision` implicitly, so this gate separates a narrowed
// key from a broad one and never an operator from an operator.
func (h *toolHandler) authorize(w http.ResponseWriter, r *http.Request) (middleware.Scope, bool) {
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
