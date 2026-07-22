package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/extensions"
)

// The E12 extensibility management surface (spec §20.2, §28.2-28.4). These methods adapt the tenant-scoped
// api.ToolRegistryAPI contract to the extensions store: scope → (organization, project), the typed rejects
// → api.ToolResult flags, and a committed row → its JSON projection.

// CreateTool registers a named tool lineage. A malformed canonical name / override is a BadField (400); a
// name collision or built-in shadow is a Conflict (409).
func (s *Store) CreateTool(ctx context.Context, scope middleware.Scope, body []byte) (api.ToolResult, error) {
	var req struct {
		CanonicalName string `json:"canonical_name"`
	}
	// Strict-decode: an unknown field in the create body is a 400, symmetric with every revision body
	// (DisallowUnknownFields), never silently swallowed (L2).
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return api.ToolResult{BadField: true}, nil
	}
	tool, err := s.tools.CreateTool(ctx, scope.Organization, scope.Project, req.CanonicalName)
	if res, mapped := toolReject(err); mapped {
		return res, nil
	}
	if err != nil {
		return api.ToolResult{}, err
	}
	out, _ := json.Marshal(map[string]any{
		"id": tool.ID, "object": "tool", "canonical_name": tool.CanonicalName, "model_visible_name": tool.ModelVisibleName,
	})
	return api.ToolResult{Body: out}, nil
}

// CreateToolRevision creates a draft revision under a tool.
func (s *Store) CreateToolRevision(ctx context.Context, scope middleware.Scope, toolID string, body []byte) (api.ToolResult, error) {
	rev, err := s.tools.CreateToolRevision(ctx, scope.Organization, scope.Project, toolID, body)
	if res, mapped := toolReject(err); mapped {
		return res, nil
	}
	if err != nil {
		return api.ToolResult{}, err
	}
	out, _ := json.Marshal(map[string]any{
		"id": rev.ID, "object": "tool_revision", "tool_id": toolID,
		"revision_number": rev.RevisionNumber, "executor": rev.Executor, "digest": rev.Digest, "status": "draft",
	})
	return api.ToolResult{Body: out}, nil
}

// PublishToolRevision publishes a draft revision; an unknown id is a NotFound (404), a re-publish an
// idempotent success (200).
func (s *Store) PublishToolRevision(ctx context.Context, scope middleware.Scope, revisionID string) (api.ToolResult, error) {
	_, exists, err := s.tools.PublishToolRevision(ctx, scope.Organization, scope.Project, revisionID)
	return publishToolResult(revisionID, exists, err)
}

// CreateToolSetRevision creates a draft set revision pinning exact published revisions.
func (s *Store) CreateToolSetRevision(ctx context.Context, scope middleware.Scope, setName string, body []byte) (api.ToolResult, error) {
	set, err := s.tools.CreateToolSetRevision(ctx, scope.Organization, scope.Project, setName, body)
	if res, mapped := toolReject(err); mapped {
		return res, nil
	}
	if err != nil {
		return api.ToolResult{}, err
	}
	out, _ := json.Marshal(map[string]any{
		"id": set.ID, "object": "tool_set_revision", "set": setName,
		"revision_number": set.RevisionNumber, "digest": set.Digest, "status": "draft",
	})
	return api.ToolResult{Body: out}, nil
}

// PublishToolSetRevision publishes a draft set revision (see PublishToolRevision).
func (s *Store) PublishToolSetRevision(ctx context.Context, scope middleware.Scope, revisionID string) (api.ToolResult, error) {
	_, exists, err := s.tools.PublishToolSetRevision(ctx, scope.Organization, scope.Project, revisionID)
	return publishToolResult(revisionID, exists, err)
}

// GetTool reads a tool lineage within scope (spec §28.2, E13 T4). A missing/foreign id is NotFound (404).
func (s *Store) GetTool(ctx context.Context, scope middleware.Scope, id string) (api.ToolResult, error) {
	it, found, err := s.tools.GetTool(ctx, scope.Organization, scope.Project, id)
	if err != nil {
		return api.ToolResult{}, err
	}
	if !found {
		return api.ToolResult{NotFound: true}, nil
	}
	return api.ToolResult{Body: mustJSON(toolLineageProjection(it.ID, it.CanonicalName, it.ModelVisibleName))}, nil
}

// ListTools returns a tenant-scoped page of tool lineages (spec §28.2, E13 T4).
func (s *Store) ListTools(ctx context.Context, scope middleware.Scope, q api.ListQuery) ([]api.ListRow, error) {
	items, err := s.tools.ListTools(ctx, scope.Organization, scope.Project, toExtensionsWindow(q))
	if err != nil {
		return nil, err
	}
	rows := make([]api.ListRow, 0, len(items))
	for _, it := range items {
		rows = append(rows, api.ListRow{ID: it.ID, CreatedAt: it.CreatedAt, Body: mustJSON(toolLineageProjection(it.ID, it.CanonicalName, it.ModelVisibleName))})
	}
	return rows, nil
}

// ListToolSets returns a tenant-scoped page of tool-set revisions (spec §28.4, E13 T4). A set is named
// directly (no lineage table), so the list is its revisions.
// ponytail: no single-resource GET /v1/tool-sets/{id} — a set is consumed by NAME in an agent revision,
// never fetched by revision id, so there is no resolver to expose. Add one if a console needs it.
func (s *Store) ListToolSets(ctx context.Context, scope middleware.Scope, q api.ListQuery) ([]api.ListRow, error) {
	items, err := s.tools.ListToolSetRevisions(ctx, scope.Organization, scope.Project, toExtensionsWindow(q))
	if err != nil {
		return nil, err
	}
	rows := make([]api.ListRow, 0, len(items))
	for _, it := range items {
		status := "draft"
		if it.Published {
			status = "published"
		}
		body := mustJSON(map[string]any{
			"id": it.ID, "object": "tool_set_revision", "set": it.Set,
			"revision_number": it.RevisionNumber, "digest": it.Digest, "status": status,
		})
		rows = append(rows, api.ListRow{ID: it.ID, CreatedAt: it.CreatedAt, Body: body})
	}
	return rows, nil
}

// toolLineageProjection is a tool lineage's read shape — the same fields the create projection shows.
func toolLineageProjection(id, canonicalName, modelVisibleName string) map[string]any {
	return map[string]any{
		"id": id, "object": "tool", "canonical_name": canonicalName, "model_visible_name": modelVisibleName,
	}
}

// toolReject maps a typed domain error to its api.ToolResult reject flag: bad input → 400, name/state
// conflict → 409, absent tool/revision → 404. A nil or unrecognised error is not mapped here.
func toolReject(err error) (api.ToolResult, bool) {
	switch {
	case err == nil:
		return api.ToolResult{}, false
	case errors.Is(err, extensions.ErrUnknownField),
		errors.Is(err, extensions.ErrInvalidCanonicalName),
		errors.Is(err, extensions.ErrInvalidReplayClass),
		errors.Is(err, extensions.ErrOverrideNotStricter):
		return api.ToolResult{BadField: true}, true
	case errors.Is(err, extensions.ErrNameCollision),
		errors.Is(err, extensions.ErrModelNameReserved),
		errors.Is(err, extensions.ErrRevisionNotPublished):
		return api.ToolResult{Conflict: true}, true
	case errors.Is(err, extensions.ErrToolNotFound),
		errors.Is(err, extensions.ErrUnknownToolRevision):
		return api.ToolResult{NotFound: true}, true
	default:
		return api.ToolResult{}, false
	}
}

func publishToolResult(revisionID string, exists bool, err error) (api.ToolResult, error) {
	if err != nil {
		return api.ToolResult{}, err
	}
	if !exists {
		return api.ToolResult{NotFound: true}, nil
	}
	return api.ToolResult{Body: mustJSON(map[string]any{"id": revisionID, "status": "published"})}, nil
}
