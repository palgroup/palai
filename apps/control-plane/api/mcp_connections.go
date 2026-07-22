package api

import (
	"context"
	"io"
	"net/http"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// MCPConnectionAPI is the store seam for the E12 Task 5 MCP connection management surface (spec §28.13):
// admin registration of upstream MCP servers and the admin discover action that materialises their tools as
// connection-namespaced draft revisions. It is an ADMIN surface — there is deliberately NO model-facing
// MCP-add/discover tool (a test pins that the tool broker exposes no such name). Scoped by the verified
// identity, never a request-body field (§39.2). nil in tiers that never touch MCP.
type MCPConnectionAPI interface {
	CreateMCPConnection(ctx context.Context, scope middleware.Scope, body []byte) (MCPConnectionResult, error)
	DiscoverMCPConnection(ctx context.Context, scope middleware.Scope, id string) (MCPConnectionResult, error)
	// GetMCPConnection + ListMCPConnections are the E13 T4 read side — non-secret metadata only, RLS-scoped.
	GetMCPConnection(ctx context.Context, scope middleware.Scope, id string) (MCPConnectionResult, error)
	ListMCPConnections(ctx context.Context, scope middleware.Scope, q ListQuery) ([]ListRow, error)
}

// MCPConnectionResult is a management projection. Exactly one outcome is set: Body carries the created
// connection or the discovery summary (2xx); BadField marks an invalid transport/config/name or an inline
// secret (400); Conflict marks a name collision (409); NotFound marks an absent connection (404).
type MCPConnectionResult struct {
	Body     []byte
	BadField bool
	Conflict bool
	NotFound bool
}

type mcpConnectionHandler struct {
	mcp MCPConnectionAPI
}

// createConnection registers an MCP connection (POST /v1/mcp-connections). Durable config, server-minted id.
func (h *mcpConnectionHandler) createConnection(w http.ResponseWriter, r *http.Request) {
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
	out, err := h.mcp.CreateMCPConnection(r.Context(), scope, raw)
	h.write(w, r, out, err, http.StatusCreated, "/v1/mcp-connections/")
}

// discoverConnection lists a connection's tools and materialises them as draft revisions (admin action,
// POST /v1/mcp-connections/{id}/discover). NOT a model-facing surface.
func (h *mcpConnectionHandler) discoverConnection(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	out, err := h.mcp.DiscoverMCPConnection(r.Context(), scope, r.PathValue("id"))
	h.write(w, r, out, err, http.StatusOK, "")
}

// getConnection reads one connection's non-secret metadata (GET /v1/mcp-connections/{id}).
func (h *mcpConnectionHandler) getConnection(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	out, err := h.mcp.GetMCPConnection(r.Context(), scope, r.PathValue("id"))
	h.write(w, r, out, err, http.StatusOK, "")
}

// listConnections returns a tenant-scoped page of connections (GET /v1/mcp-connections).
func (h *mcpConnectionHandler) listConnections(w http.ResponseWriter, r *http.Request) {
	scope, ok := middleware.ScopeFrom(r.Context())
	if !ok {
		middleware.WriteProblem(w, r, http.StatusUnauthorized, "authentication_required", "a bearer API key is required")
		return
	}
	q, ok := beginList(w, r, "mcp-connections", scope)
	if !ok {
		return
	}
	rows, err := h.mcp.ListMCPConnections(r.Context(), scope, q)
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	renderPage(w, r, "mcp-connections", scope, rows, q.Limit)
}

// write renders a management outcome: the typed rejects first, then 2xx with the resource.
func (h *mcpConnectionHandler) write(w http.ResponseWriter, r *http.Request, out MCPConnectionResult, err error, okStatus int, locationPrefix string) {
	if err != nil {
		middleware.WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "")
		return
	}
	switch {
	case out.BadField:
		middleware.WriteProblem(w, r, http.StatusBadRequest, "invalid_request", "the request carries an unsupported field, a malformed name, or an invalid transport/config")
		return
	case out.Conflict:
		middleware.WriteProblem(w, r, http.StatusConflict, "conflict", "an MCP connection with this name already exists")
		return
	case out.NotFound:
		middleware.WriteProblem(w, r, http.StatusNotFound, "not_found", "no such MCP connection in this project")
		return
	}
	if locationPrefix != "" {
		w.Header().Set("Location", locationPrefix+resourceIDOf(out.Body))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(okStatus)
	_, _ = w.Write(out.Body)
}
