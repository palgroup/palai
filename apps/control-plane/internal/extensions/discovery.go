package extensions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/palgroup/palai/adapters/integrations/mcp"
	"github.com/palgroup/palai/storage"
)

// MCPClient is the seam the discovery + dispatch paths reach an MCP server through. *mcp.Manager implements
// it; a test injects a fake. It keeps the control-plane store from importing the OCI/HTTP transports
// directly — the adapter owns the untrusted-server mechanics (sandbox, egress, breaker).
type MCPClient interface {
	Discover(ctx context.Context, conn mcp.ConnConfig) ([]mcp.RemoteTool, error)
	Call(ctx context.Context, scope mcp.CallScope, conn mcp.ConnConfig, remoteName string, args map[string]any) (map[string]any, error)
}

// SetMCP injects the MCP client the discovery + dispatch paths use. A nil client leaves MCP connections
// creatable but not discoverable/executable (the binder-less posture, symmetric with T2's remote_http).
func (s *Store) SetMCP(client MCPClient) { s.mcp = client }

// DiscoverResult reports one discovery pass: the tools that produced a new draft revision, the ones already
// current (digest unchanged — no churn), and the ones REJECTED by a model-visible collision (a colliding
// single tool is a visible reject; discovery continues past it — spec §28.13 namespacing).
type DiscoverResult struct {
	NewRevisions []string
	Unchanged    []string
	Rejected     []string
}

// DiscoverConnection lists a connection's tools and materialises each as a connection-namespaced registry
// tool: canonical mcp.<connection>.<tool>, model-visible <connection>__<tool> (so two servers' `search`
// tools never collide). The untrusted description lands in a DRAFT revision — NEVER auto-published, so an
// admin must approve it before it is advertised (EXT-006). Re-discovery with a changed description/schema is
// a NEW draft (the published revision stays, re-approval required); an unchanged tool writes nothing.
func (s *Store) DiscoverConnection(ctx context.Context, org, project, connID string) (DiscoverResult, error) {
	if s.mcp == nil {
		return DiscoverResult{}, errors.New("extensions: no MCP client wired for discovery")
	}
	conn, err := s.GetMCPConnection(ctx, org, project, connID)
	if err != nil {
		return DiscoverResult{}, err
	}
	tools, err := s.mcp.Discover(ctx, connConfig(org, conn))
	if err != nil {
		return DiscoverResult{}, fmt.Errorf("discover connection %s: %w", connID, err)
	}
	var out DiscoverResult
	for _, rt := range tools {
		status, err := s.materialiseDiscoveredTool(ctx, org, project, conn.Name, connID, rt)
		if err != nil {
			return out, err
		}
		switch status {
		case "new":
			out.NewRevisions = append(out.NewRevisions, rt.Name)
		case "unchanged":
			out.Unchanged = append(out.Unchanged, rt.Name)
		case "rejected":
			out.Rejected = append(out.Rejected, rt.Name)
		}
	}
	return out, nil
}

// materialiseDiscoveredTool creates-or-reuses the tool lineage and inserts a new DRAFT revision only when
// the config digest changed. Returns "new" | "unchanged" | "rejected" (a model-visible collision).
func (s *Store) materialiseDiscoveredTool(ctx context.Context, org, project, connName, connID string, rt mcp.RemoteTool) (string, error) {
	canonical := "mcp." + connName + "." + rt.Name
	modelVisible := connName + "__" + rt.Name

	toolID, found, err := s.discoveredToolID(ctx, org, project, canonical)
	if err != nil {
		return "", err
	}
	if !found {
		toolID, err = s.createDiscoveredTool(ctx, org, project, canonical, modelVisible)
		if errors.Is(err, ErrNameCollision) || errors.Is(err, ErrModelNameReserved) {
			return "rejected", nil // a colliding single tool is a visible reject; discovery continues
		}
		if err != nil {
			return "", err
		}
	}

	// Build the revision body (untrusted description → draft) and only write when its digest changed.
	in := ToolRevisionInput{
		Executor:       "mcp",
		Description:    rt.Description,
		InputSchema:    rt.InputSchema,
		ExecutorConfig: map[string]any{"connection_id": connID, "remote_name": rt.Name},
	}
	digest := revisionDigest(in)
	latest, hasRev, err := s.latestRevisionDigest(ctx, toolID)
	if err != nil {
		return "", err
	}
	if hasRev && latest == digest {
		return "unchanged", nil // identical config — no churn, no new revision
	}
	body, _ := json.Marshal(in)
	if _, err := s.CreateToolRevision(ctx, org, project, toolID, body); err != nil {
		return "", err
	}
	return "new", nil
}

// createDiscoveredTool inserts a tool lineage with an EXPLICIT model-visible name (connName__tool) rather
// than the canonical last segment, so two connections' identically-named tools stay distinct to the model.
// The 000024 UNIQUE(model_visible) constraint still catches a genuine collision as a typed reject.
func (s *Store) createDiscoveredTool(ctx context.Context, org, project, canonical, modelVisible string) (string, error) {
	if _, err := validateCanonicalName(canonical); err != nil {
		return "", err
	}
	if s.reserved[modelVisible] {
		return "", ErrModelNameReserved
	}
	id := newID("tool")
	if _, err := s.pool.Exec(ctx, storage.Query("InsertTool"), id, org, project, canonical, modelVisible); err != nil {
		if isUniqueViolation(err) {
			return "", ErrNameCollision
		}
		return "", fmt.Errorf("insert discovered tool: %w", err)
	}
	return id, nil
}

// discoveredToolID resolves an existing lineage id by canonical name.
func (s *Store) discoveredToolID(ctx context.Context, org, project, canonical string) (string, bool, error) {
	var id string
	switch err := s.pool.QueryRow(ctx, storage.Query("MCPToolIDByCanonical"), org, project, canonical).Scan(&id); {
	case errors.Is(err, pgx.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("resolve discovered tool id: %w", err)
	}
	return id, true, nil
}

// latestRevisionDigest reads a tool's newest revision digest (for the no-churn skip).
func (s *Store) latestRevisionDigest(ctx context.Context, toolID string) (string, bool, error) {
	var digest string
	switch err := s.pool.QueryRow(ctx, storage.Query("MCPLatestToolRevisionDigest"), toolID).Scan(&digest); {
	case errors.Is(err, pgx.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("read latest revision digest: %w", err)
	}
	return digest, true, nil
}

// connConfig maps a stored connection to the adapter's dial config (the secret stays a HANDLE — the manager
// resolves it at request time).
func connConfig(org string, conn Connection) mcp.ConnConfig {
	cc := mcp.ConnConfig{
		ID:        conn.ID,
		Name:      conn.Name,
		Transport: conn.Transport,
		Org:       org,
		SecretRef: conn.SecretRef,
	}
	if digest, ok := conn.Config["image_digest"].(string); ok {
		cc.ImageDigest = digest
	}
	if url, ok := conn.Config["url"].(string); ok {
		cc.URL = url
	}
	if cmd, ok := conn.Config["cmd"].([]any); ok {
		for _, c := range cmd {
			if s, ok := c.(string); ok {
				cc.Cmd = append(cc.Cmd, s)
			}
		}
	}
	return cc
}
