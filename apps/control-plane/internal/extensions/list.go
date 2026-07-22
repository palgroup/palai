package extensions

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/palgroup/palai/storage"
)

// list.go holds the read/LIST keyset reads the E13 Task 4 API surface consumes for the extension
// registry (tools, tool-sets, MCP connections). Every read runs under the tenant scope (RLS confines
// the rows; the organization/project predicate is defence-in-depth) and pages by the (created_at, id)
// keyset. There is no status filter — these resources carry no single lifecycle-state column; only the
// created_at bounds the plan names apply.

// ListWindow is a resolved keyset page request. AfterCreatedAt/AfterID is the previous page's last row
// (nil AfterCreatedAt means the first page); the CreatedGTE/CreatedLTE bounds are the created_at filter;
// Limit is the row cap (the caller adds the +1 over-fetch). It carries no tenant — the scope confines it.
type ListWindow struct {
	CreatedGTE     *time.Time
	CreatedLTE     *time.Time
	AfterCreatedAt *time.Time
	AfterID        string
	Limit          int
}

// MCPConnectionListItem is one row of the MCP connection list: the non-secret metadata plus the keyset
// created_at. The secret_ref is deliberately absent — a list never surfaces a credential handle.
type MCPConnectionListItem struct {
	ID         string
	Name       string
	Transport  string
	TrustLevel string
	Disabled   bool
	CreatedAt  time.Time
}

// ListMCPConnections returns a tenant-scoped page of MCP connections newest-first (spec §28.13).
func (s *Store) ListMCPConnections(ctx context.Context, org, project string, w ListWindow) ([]MCPConnectionListItem, error) {
	ctx = storage.ScopeToTenant(ctx, org, project)
	rows, err := s.pool.Query(ctx, storage.Query("ListMCPConnections"),
		org, project, w.CreatedGTE, w.CreatedLTE, w.AfterCreatedAt, w.AfterID, w.Limit)
	if err != nil {
		return nil, fmt.Errorf("list mcp connections: %w", err)
	}
	defer rows.Close()
	var out []MCPConnectionListItem
	for rows.Next() {
		var it MCPConnectionListItem
		if err := rows.Scan(&it.ID, &it.Name, &it.Transport, &it.TrustLevel, &it.Disabled, &it.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan mcp connection row: %w", err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// ToolLineageItem is a tool lineage's list/get projection (id + names + keyset created_at).
type ToolLineageItem struct {
	ID               string
	CanonicalName    string
	ModelVisibleName string
	CreatedAt        time.Time
}

// GetTool reads a tool lineage within scope. found=false for a foreign or unknown id (404).
func (s *Store) GetTool(ctx context.Context, org, project, id string) (ToolLineageItem, bool, error) {
	ctx = storage.ScopeToTenant(ctx, org, project)
	var it ToolLineageItem
	err := s.pool.QueryRow(ctx, storage.Query("GetTool"), id, org, project).
		Scan(&it.ID, &it.CanonicalName, &it.ModelVisibleName, &it.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ToolLineageItem{}, false, nil
	}
	if err != nil {
		return ToolLineageItem{}, false, fmt.Errorf("get tool: %w", err)
	}
	return it, true, nil
}

// ListTools returns a tenant-scoped page of tool lineages newest-first (spec §28.2).
func (s *Store) ListTools(ctx context.Context, org, project string, w ListWindow) ([]ToolLineageItem, error) {
	ctx = storage.ScopeToTenant(ctx, org, project)
	rows, err := s.pool.Query(ctx, storage.Query("ListTools"),
		org, project, w.CreatedGTE, w.CreatedLTE, w.AfterCreatedAt, w.AfterID, w.Limit)
	if err != nil {
		return nil, fmt.Errorf("list tools: %w", err)
	}
	defer rows.Close()
	var out []ToolLineageItem
	for rows.Next() {
		var it ToolLineageItem
		if err := rows.Scan(&it.ID, &it.CanonicalName, &it.ModelVisibleName, &it.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan tool row: %w", err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// ToolSetRevisionItem is one row of the tool-set list: a set revision's identity + published flag.
type ToolSetRevisionItem struct {
	ID             string
	Set            string
	RevisionNumber int
	Digest         string
	Published      bool
	CreatedAt      time.Time
}

// ListToolSetRevisions returns a tenant-scoped page of tool-set revisions newest-first (spec §28.4).
func (s *Store) ListToolSetRevisions(ctx context.Context, org, project string, w ListWindow) ([]ToolSetRevisionItem, error) {
	ctx = storage.ScopeToTenant(ctx, org, project)
	rows, err := s.pool.Query(ctx, storage.Query("ListToolSetRevisions"),
		org, project, w.CreatedGTE, w.CreatedLTE, w.AfterCreatedAt, w.AfterID, w.Limit)
	if err != nil {
		return nil, fmt.Errorf("list tool-set revisions: %w", err)
	}
	defer rows.Close()
	var out []ToolSetRevisionItem
	for rows.Next() {
		var it ToolSetRevisionItem
		if err := rows.Scan(&it.ID, &it.Set, &it.RevisionNumber, &it.Digest, &it.Published, &it.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan tool-set revision row: %w", err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}
