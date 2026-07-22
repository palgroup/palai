package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/extensions"
)

// The E12 Task 5 MCP connection management surface (spec §28.13-28.14). These adapt the tenant-scoped
// api.MCPConnectionAPI contract to the extensions store: scope → (organization, project), the typed rejects
// → api.MCPConnectionResult flags, and a committed row / discovery summary → its JSON projection.

// SetMCP injects the MCP client the discover route reaches an upstream server through (compose wires the
// shared manager, so the admin discover API and the dispatch lookup use the same sandboxed client).
func (s *Store) SetMCP(client extensions.MCPClient) { s.tools.SetMCP(client) }

// CreateMCPConnection registers an MCP connection. An invalid transport/config/name or an inline secret is a
// BadField (400); a name collision is a Conflict (409).
func (s *Store) CreateMCPConnection(ctx context.Context, scope middleware.Scope, body []byte) (api.MCPConnectionResult, error) {
	conn, err := s.tools.CreateMCPConnection(ctx, scope.Organization, scope.Project, body)
	if res, mapped := mcpReject(err); mapped {
		return res, nil
	}
	if err != nil {
		return api.MCPConnectionResult{}, err
	}
	out, _ := json.Marshal(map[string]any{
		"id": conn.ID, "object": "mcp_connection", "name": conn.Name,
		"transport": conn.Transport, "trust_level": conn.TrustLevel,
	})
	return api.MCPConnectionResult{Body: out}, nil
}

// DiscoverMCPConnection lists a connection's tools and materialises them as draft revisions. An unknown
// connection is a NotFound (404); a dial/protocol failure surfaces as a 500 (the handler maps err).
func (s *Store) DiscoverMCPConnection(ctx context.Context, scope middleware.Scope, id string) (api.MCPConnectionResult, error) {
	result, err := s.tools.DiscoverConnection(ctx, scope.Organization, scope.Project, id)
	if res, mapped := mcpReject(err); mapped {
		return res, nil
	}
	if err != nil {
		return api.MCPConnectionResult{}, err
	}
	out, _ := json.Marshal(map[string]any{
		"object": "mcp_discovery", "connection_id": id,
		"new_revisions": result.NewRevisions, "unchanged": result.Unchanged, "rejected": result.Rejected,
	})
	return api.MCPConnectionResult{Body: out}, nil
}

// GetMCPConnection reads a connection's non-secret metadata within scope (spec §28.13, E13 T4). A
// missing/foreign id is NotFound (404).
func (s *Store) GetMCPConnection(ctx context.Context, scope middleware.Scope, id string) (api.MCPConnectionResult, error) {
	conn, err := s.tools.GetMCPConnection(ctx, scope.Organization, scope.Project, id)
	if errors.Is(err, extensions.ErrConnectionNotFound) {
		return api.MCPConnectionResult{NotFound: true}, nil
	}
	if err != nil {
		return api.MCPConnectionResult{}, err
	}
	out, _ := json.Marshal(mcpConnectionProjection(conn.ID, conn.Name, conn.Transport, conn.TrustLevel, conn.Disabled))
	return api.MCPConnectionResult{Body: out}, nil
}

// ListMCPConnections returns a tenant-scoped page of MCP connections (spec §28.13, E13 T4). The
// secret_ref is never surfaced — a list carries only the non-secret metadata.
func (s *Store) ListMCPConnections(ctx context.Context, scope middleware.Scope, q api.ListQuery) ([]api.ListRow, error) {
	items, err := s.tools.ListMCPConnections(ctx, scope.Organization, scope.Project, toExtensionsWindow(q))
	if err != nil {
		return nil, err
	}
	rows := make([]api.ListRow, 0, len(items))
	for _, it := range items {
		body, _ := json.Marshal(mcpConnectionProjection(it.ID, it.Name, it.Transport, it.TrustLevel, it.Disabled))
		rows = append(rows, api.ListRow{ID: it.ID, CreatedAt: it.CreatedAt, Body: body})
	}
	return rows, nil
}

// mcpConnectionProjection is the connection's read shape — the same fields the create projection shows,
// plus disabled. It never carries the secret_ref handle.
func mcpConnectionProjection(id, name, transport, trustLevel string, disabled bool) map[string]any {
	return map[string]any{
		"id": id, "object": "mcp_connection", "name": name,
		"transport": transport, "trust_level": trustLevel, "disabled": disabled,
	}
}

// mcpReject maps a typed domain error to its api.MCPConnectionResult reject flag.
func mcpReject(err error) (api.MCPConnectionResult, bool) {
	switch {
	case err == nil:
		return api.MCPConnectionResult{}, false
	case errors.Is(err, extensions.ErrUnknownField),
		errors.Is(err, extensions.ErrInvalidTransport),
		errors.Is(err, extensions.ErrInvalidConnectionName),
		errors.Is(err, extensions.ErrInvalidConnectionConfig):
		return api.MCPConnectionResult{BadField: true}, true
	case errors.Is(err, extensions.ErrConnectionNameCollision):
		return api.MCPConnectionResult{Conflict: true}, true
	case errors.Is(err, extensions.ErrConnectionNotFound):
		return api.MCPConnectionResult{NotFound: true}, true
	default:
		return api.MCPConnectionResult{}, false
	}
}
