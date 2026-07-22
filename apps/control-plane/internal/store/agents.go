package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/automation"
)

// The automation-agent management surface (spec §20.2.1, §10). These methods adapt the tenant-scoped
// api.AgentRegistry contract to the automation store: scope → (organization, project), the strict-decode
// reject → api.AgentResult{BadField}, and a committed revision → its JSON projection.

// CreateAgentProfile registers a named profile lineage.
func (s *Store) CreateAgentProfile(ctx context.Context, scope middleware.Scope, name string) (api.AgentResult, error) {
	if strings.TrimSpace(name) == "" {
		return api.AgentResult{MissingName: true}, nil
	}
	id, err := s.agents.CreateProfile(ctx, scope.Organization, scope.Project, name)
	if err != nil {
		return api.AgentResult{}, err
	}
	body, _ := json.Marshal(map[string]any{"id": id, "object": "agent", "name": name})
	return api.AgentResult{Body: body}, nil
}

// CreateAgentRevision creates a draft revision under a profile. An unsupported field is a BadField
// (400), an unknown profile a NotFound (404).
func (s *Store) CreateAgentRevision(ctx context.Context, scope middleware.Scope, profileID string, body []byte) (api.AgentResult, error) {
	rev, err := s.agents.CreateRevision(ctx, scope.Organization, scope.Project, profileID, body)
	switch {
	case errors.Is(err, automation.ErrUnknownField):
		return api.AgentResult{BadField: true}, nil
	case errors.Is(err, automation.ErrProfileNotFound):
		return api.AgentResult{NotFound: true}, nil
	case err != nil:
		return api.AgentResult{}, err
	}
	return api.AgentResult{Body: revisionBody(rev, "agent_revision", profileID)}, nil
}

// PublishAgentRevision publishes a draft revision; an unknown id is a NotFound (404), a re-publish an
// idempotent success (200).
func (s *Store) PublishAgentRevision(ctx context.Context, scope middleware.Scope, revisionID string) (api.AgentResult, error) {
	_, exists, err := s.agents.PublishRevision(ctx, scope.Organization, scope.Project, revisionID)
	return publishResult(revisionID, exists, err)
}

// CreateRunTemplateRevision creates a draft profile-free template revision (identity/delegation rejected
// by the strict decode).
func (s *Store) CreateRunTemplateRevision(ctx context.Context, scope middleware.Scope, templateName string, body []byte) (api.AgentResult, error) {
	rev, err := s.agents.CreateTemplateRevision(ctx, scope.Organization, scope.Project, templateName, body)
	switch {
	case errors.Is(err, automation.ErrUnknownField):
		return api.AgentResult{BadField: true}, nil
	case err != nil:
		return api.AgentResult{}, err
	}
	return api.AgentResult{Body: revisionBody(rev, "run_template_revision", templateName)}, nil
}

// PublishRunTemplateRevision publishes a draft template revision (see PublishAgentRevision).
func (s *Store) PublishRunTemplateRevision(ctx context.Context, scope middleware.Scope, revisionID string) (api.AgentResult, error) {
	_, exists, err := s.agents.PublishTemplateRevision(ctx, scope.Organization, scope.Project, revisionID)
	return publishResult(revisionID, exists, err)
}

// GetAgentProfile reads a profile lineage within scope (spec §10, E13 T4). A missing/foreign id is
// NotFound (404).
func (s *Store) GetAgentProfile(ctx context.Context, scope middleware.Scope, id string) (api.AgentResult, error) {
	it, found, err := s.agents.GetProfile(ctx, scope.Organization, scope.Project, id)
	if err != nil {
		return api.AgentResult{}, err
	}
	if !found {
		return api.AgentResult{NotFound: true}, nil
	}
	return api.AgentResult{Body: mustJSON(map[string]any{"id": it.ID, "object": "agent", "name": it.Name})}, nil
}

// ListAgentProfiles returns a tenant-scoped page of agent-profile lineages (spec §10, E13 T4).
func (s *Store) ListAgentProfiles(ctx context.Context, scope middleware.Scope, q api.ListQuery) ([]api.ListRow, error) {
	items, err := s.agents.ListProfiles(ctx, scope.Organization, scope.Project, toAutomationWindow(q))
	if err != nil {
		return nil, err
	}
	rows := make([]api.ListRow, 0, len(items))
	for _, it := range items {
		rows = append(rows, api.ListRow{ID: it.ID, CreatedAt: it.CreatedAt, Body: mustJSON(map[string]any{"id": it.ID, "object": "agent", "name": it.Name})})
	}
	return rows, nil
}

// ListAgentRevisions returns a tenant-scoped page of one profile's revisions (spec §10, E13 T4). An
// unknown or foreign profile yields an empty page (no existence oracle beyond emptiness).
func (s *Store) ListAgentRevisions(ctx context.Context, scope middleware.Scope, profileID string, q api.ListQuery) ([]api.ListRow, error) {
	items, err := s.agents.ListRevisions(ctx, scope.Organization, scope.Project, profileID, toAutomationWindow(q))
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
			"id": it.ID, "object": "agent_revision", "agent_id": profileID,
			"revision_number": it.RevisionNumber, "model": it.Model, "instructions": it.Instructions, "status": status,
		})
		rows = append(rows, api.ListRow{ID: it.ID, CreatedAt: it.CreatedAt, Body: body})
	}
	return rows, nil
}

// revisionBody renders a created draft revision projection. parentKey names the owning profile
// (agent_id) or template (template) so a client can navigate back.
func revisionBody(rev automation.Revision, object, parent string) []byte {
	m := map[string]any{
		"id": rev.ID, "object": object, "revision_number": rev.RevisionNumber,
		"model": rev.Model, "tools": rev.Tools, "instructions": rev.Instructions, "status": "draft",
	}
	if object == "run_template_revision" {
		m["template"] = parent
	} else {
		m["agent_id"] = parent
	}
	return mustJSON(m)
}

func publishResult(revisionID string, exists bool, err error) (api.AgentResult, error) {
	if err != nil {
		return api.AgentResult{}, err
	}
	if !exists {
		return api.AgentResult{NotFound: true}, nil
	}
	return api.AgentResult{Body: mustJSON(map[string]any{"id": revisionID, "status": "published"})}, nil
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
