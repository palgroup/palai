package store

import (
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
	_ = json.Unmarshal(body, &req)
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

// toolReject maps a typed domain error to its api.ToolResult reject flag: bad input → 400, name/state
// conflict → 409, absent tool/revision → 404. A nil or unrecognised error is not mapped here.
func toolReject(err error) (api.ToolResult, bool) {
	switch {
	case err == nil:
		return api.ToolResult{}, false
	case errors.Is(err, extensions.ErrUnknownField),
		errors.Is(err, extensions.ErrInvalidCanonicalName),
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
