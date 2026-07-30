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
// idempotent success (200). The optional body carries the operator's approval declaration (E23 T5) — an
// absent body is the shipped bodyless publish and stays bit-unchanged; a malformed one is a 400 rather
// than a silently ungated publish.
func (s *Store) PublishToolRevision(ctx context.Context, scope middleware.Scope, revisionID string, body []byte) (api.ToolResult, error) {
	_, exists, err := s.tools.PublishToolRevision(ctx, scope.Organization, scope.Project, revisionID, body)
	if res, mapped := toolReject(err); mapped {
		return res, nil
	}
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

// ListToolRevisions returns a tenant-scoped page of ONE lineage's revisions (spec §28.3, E25 T7). An
// unknown or foreign tool id is a NotFound rather than an empty page: the lineage's existence is checked
// in scope first, through the SAME GetTool the read route uses.
//
// THIS IS THE ROUTE docs/operations/jira-mcp-connection.md §3c HAS ALWAYS NEEDED. It told an operator to
// find ids with GET /v1/tools and publish each revision, and no response in this tree carried a revision
// id — so three of the runbook's five calls rested on one nobody could make.
func (s *Store) ListToolRevisions(ctx context.Context, scope middleware.Scope, toolID string, q api.ListQuery) ([]api.ListRow, bool, error) {
	if _, found, err := s.tools.GetTool(ctx, scope.Organization, scope.Project, toolID); err != nil || !found {
		return nil, found, err
	}
	items, err := s.tools.ListToolRevisions(ctx, scope.Organization, scope.Project, toolID, toExtensionsWindow(q))
	if err != nil {
		return nil, false, err
	}
	rows := make([]api.ListRow, 0, len(items))
	for _, it := range items {
		rows = append(rows, api.ListRow{ID: it.ID, CreatedAt: it.CreatedAt, Body: mustJSON(toolRevisionProjection(it))})
	}
	return rows, true, nil
}

// toolRevisionProjection is a tool revision's read shape, shared by the list and the single-resource read
// so the two cannot drift into different answers about the same row.
//
// description + input_schema ARE THE POINT (plan §T7): they are what an admin approves. They are also
// UNTRUSTED — an MCP server wrote them — and they cross this boundary as data: the schema is re-emitted as
// raw JSON rather than re-modelled, and the description as a string. Neither is interpreted here and
// neither may be interpreted by a renderer.
func toolRevisionProjection(it extensions.ToolRevisionItem) map[string]any {
	status := "draft"
	if it.Published {
		status = "published"
	}
	return map[string]any{
		"id": it.ID, "object": "tool_revision", "tool_id": it.ToolID,
		"revision_number": it.RevisionNumber, "executor": it.Executor,
		"description": it.Description, "input_schema": json.RawMessage(it.InputSchema),
		"digest": it.Digest, "status": status,
		"approval_required": it.ApprovalRequired, "approval_label": it.ApprovalLabel,
		"created_at": it.CreatedAt,
	}
}

// GetToolRevision reads ONE revision of ONE lineage (spec §28.3). It is the address POST
// /v1/tools/{tool_id}/revisions names in its Location header — which pointed at an unmounted
// `/v1/tool-revisions/` prefix until this route existed. A missing/foreign id, or an id belonging to a
// different lineage, is NotFound (404).
func (s *Store) GetToolRevision(ctx context.Context, scope middleware.Scope, toolID, revisionID string) (api.ToolResult, error) {
	it, found, err := s.tools.GetToolRevisionOfTool(ctx, scope.Organization, scope.Project, toolID, revisionID)
	if err != nil {
		return api.ToolResult{}, err
	}
	if !found {
		return api.ToolResult{NotFound: true}, nil
	}
	return api.ToolResult{Body: mustJSON(toolRevisionProjection(it))}, nil
}

// GetToolSetRevision reads one set revision AND ITS PINS (spec §28.4, E25 T7). A missing/foreign id, or an
// id belonging to a different set, is NotFound (404).
func (s *Store) GetToolSetRevision(ctx context.Context, scope middleware.Scope, setName, revisionID string) (api.ToolResult, error) {
	it, found, err := s.tools.GetToolSetRevision(ctx, scope.Organization, scope.Project, setName, revisionID)
	if err != nil {
		return api.ToolResult{}, err
	}
	if !found {
		return api.ToolResult{NotFound: true}, nil
	}
	status := "draft"
	if it.Published {
		status = "published"
	}
	return api.ToolResult{Body: mustJSON(map[string]any{
		"id": it.ID, "object": "tool_set_revision", "set": it.Set,
		"revision_number": it.RevisionNumber, "digest": it.Digest, "status": status,
		// The pins, verbatim — the field the LIST projection does not carry and the reason this route
		// exists. It is the create body's own shape, so an operator reads back what they wrote.
		"tools":      json.RawMessage(it.Pins),
		"created_at": it.CreatedAt,
	})}, nil
}

// ListToolSets returns a tenant-scoped page of tool-set revisions (spec §28.4, E13 T4). A set is named
// directly (no lineage table), so the list is its revisions. The single-resource read a `ponytail:` note
// here used to defer ("add one if a console needs it") is GetToolSetRevision above — E25 T7's console
// needed it, and the list projection stays as it was: identity and digest, without the pins.
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
		// ErrTimeoutTooLarge is the operator's value, not the server's fault. It was MISSING from this arm
		// while every other check on the SAME decode path (DecodeToolRevisionInput: unknown field, bad
		// replay_class) was named here, so a timeout_ms past the ceiling fell through to `default`, the
		// handler saw a non-nil error, and the answer was a 500 that ALSO said retryable:true — advice to
		// keep re-sending the one value that can never be accepted. The hook surface's twin mapper
		// (store/hooks.go:59) has named it since E12 T8; only the tool surface's did not.
		errors.Is(err, extensions.ErrTimeoutTooLarge),
		errors.Is(err, extensions.ErrApprovalLabelTooLong),
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
