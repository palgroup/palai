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
func (s *Store) ListMCPConnections(ctx context.Context, project string, w ListWindow) ([]MCPConnectionListItem, error) {
	ctx = storage.ScopeToTenant(ctx, project)
	rows, err := s.pool.Query(ctx, storage.Query("ListMCPConnections"),
		project, w.CreatedGTE, w.CreatedLTE, w.AfterCreatedAt, w.AfterID, w.Limit)
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
func (s *Store) GetTool(ctx context.Context, project, id string) (ToolLineageItem, bool, error) {
	ctx = storage.ScopeToTenant(ctx, project)
	var it ToolLineageItem
	err := s.pool.QueryRow(ctx, storage.Query("GetTool"), id, project).
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
func (s *Store) ListTools(ctx context.Context, project string, w ListWindow) ([]ToolLineageItem, error) {
	ctx = storage.ScopeToTenant(ctx, project)
	rows, err := s.pool.Query(ctx, storage.Query("ListTools"),
		project, w.CreatedGTE, w.CreatedLTE, w.AfterCreatedAt, w.AfterID, w.Limit)
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

// ToolRevisionItem is one row of a lineage's revision list (E25 T7). Description and InputSchema are
// UNTRUSTED — for an mcp executor they are the remote server's own words and the shape it declared — and
// they are here because they are exactly what an admin approves when they publish. Nothing in this struct
// is a credential: the MCP credential lives on the connection row, and a control_plane/remote_http
// revision's secret_ref is a handle that is deliberately NOT selected.
type ToolRevisionItem struct {
	ID               string
	ToolID           string
	RevisionNumber   int
	Executor         string
	Description      string
	InputSchema      []byte
	Digest           string
	Published        bool
	ApprovalRequired bool
	ApprovalLabel    string
	CreatedAt        time.Time
}

// ListToolRevisions returns a tenant-scoped page of ONE tool lineage's revisions newest-first (spec §28.3,
// E25 T7). The caller checks the lineage exists in scope first, so an unknown or foreign tool is a 404
// rather than an empty page — a list that cannot tell "no revisions" from "not your tool" makes the
// runbook's first step ambiguous at exactly the moment an operator is looking for a typo.
func (s *Store) ListToolRevisions(ctx context.Context, project, toolID string, w ListWindow) ([]ToolRevisionItem, error) {
	ctx = storage.ScopeToTenant(ctx, project)
	rows, err := s.pool.Query(ctx, storage.Query("ListToolRevisions"),
		toolID, project, w.CreatedGTE, w.CreatedLTE, w.AfterCreatedAt, w.AfterID, w.Limit)
	if err != nil {
		return nil, fmt.Errorf("list tool revisions: %w", err)
	}
	defer rows.Close()
	var out []ToolRevisionItem
	for rows.Next() {
		var it ToolRevisionItem
		if err := rows.Scan(&it.ID, &it.ToolID, &it.RevisionNumber, &it.Executor, &it.Description, &it.InputSchema,
			&it.Digest, &it.Published, &it.ApprovalRequired, &it.ApprovalLabel, &it.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan tool revision row: %w", err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// GetToolRevisionOfTool reads ONE revision of ONE lineage within scope, keyed by BOTH the tool id and the
// revision id. found=false for a foreign/unknown id or an id belonging to another lineage (404). The row
// shape is ToolRevisionItem, so the single-resource read and the list cannot drift into two projections of
// the same thing.
func (s *Store) GetToolRevisionOfTool(ctx context.Context, project, toolID, revisionID string) (ToolRevisionItem, bool, error) {
	ctx = storage.ScopeToTenant(ctx, project)
	var it ToolRevisionItem
	err := s.pool.QueryRow(ctx, storage.Query("GetToolRevisionOfTool"), revisionID, toolID, project).
		Scan(&it.ID, &it.ToolID, &it.RevisionNumber, &it.Executor, &it.Description, &it.InputSchema,
			&it.Digest, &it.Published, &it.ApprovalRequired, &it.ApprovalLabel, &it.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ToolRevisionItem{}, false, nil
	}
	if err != nil {
		return ToolRevisionItem{}, false, fmt.Errorf("get tool revision: %w", err)
	}
	return it, true, nil
}

// ToolSetRevisionDetail is one set revision INCLUDING its pins (E25 T7). Pins is the raw tool_pins JSONB —
// the exact [{"tool_revision_id":…,"overrides":{…}}] the set was created with — passed through rather than
// re-modelled, because the pin shape is the create body's shape and two models of one thing drift.
type ToolSetRevisionDetail struct {
	ToolSetRevisionItem
	Pins []byte
}

// GetToolSetRevision reads one set revision within scope, keyed by BOTH the set name and the revision id.
// found=false for a foreign/unknown id or an id belonging to another set (404).
func (s *Store) GetToolSetRevision(ctx context.Context, project, setName, revisionID string) (ToolSetRevisionDetail, bool, error) {
	ctx = storage.ScopeToTenant(ctx, project)
	var it ToolSetRevisionDetail
	err := s.pool.QueryRow(ctx, storage.Query("GetToolSetRevision"), revisionID, setName, project).
		Scan(&it.ID, &it.Set, &it.RevisionNumber, &it.Digest, &it.Pins, &it.Published, &it.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ToolSetRevisionDetail{}, false, nil
	}
	if err != nil {
		return ToolSetRevisionDetail{}, false, fmt.Errorf("get tool set revision: %w", err)
	}
	return it, true, nil
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
func (s *Store) ListToolSetRevisions(ctx context.Context, project string, w ListWindow) ([]ToolSetRevisionItem, error) {
	ctx = storage.ScopeToTenant(ctx, project)
	rows, err := s.pool.Query(ctx, storage.Query("ListToolSetRevisions"),
		project, w.CreatedGTE, w.CreatedLTE, w.AfterCreatedAt, w.AfterID, w.Limit)
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
