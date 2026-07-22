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
