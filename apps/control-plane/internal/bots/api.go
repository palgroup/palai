package bots

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// Compile-time proof Store satisfies the router's seam.
var _ api.BotRegistry = (*Store)(nil)

// wireBot is the resource this package renders — the ONE projection GET, LIST, POST and PATCH all
// return, matching the repository-binding precedent (a list row is the same shape a single read is).
// Organization/project are deliberately absent: a bot cannot describe a tenant even to itself — a row's
// tenant is always the verified scope, never a field on the row.
type wireBot struct {
	ID                  string          `json:"id"`
	Object              string          `json:"object"`
	Name                string          `json:"name"`
	Kind                string          `json:"kind"`
	AgentRevisionID     string          `json:"agent_revision_id"`
	RepositoryBindingID string          `json:"repository_binding_id"`
	PrincipalID         string          `json:"principal_id"`
	Config              json.RawMessage `json:"config"`
	Disabled            bool            `json:"disabled"`
	CreatedAt           time.Time       `json:"created_at"`
}

func render(b Bot) ([]byte, error) {
	body, err := json.Marshal(wireBot{
		ID: b.ID, Object: "bot", Name: b.Name, Kind: b.Kind, AgentRevisionID: b.AgentRevisionID,
		RepositoryBindingID: b.RepositoryBindingID, PrincipalID: b.PrincipalID, Config: b.Config,
		Disabled: b.Disabled, CreatedAt: b.CreatedAt,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal bot: %w", err)
	}
	return body, nil
}

// CreateBot implements api.BotRegistry. name and kind are required — a bot with neither identifies
// nothing and speaks as nothing, so the store is never asked to write one.
func (s *Store) CreateBot(ctx context.Context, scope middleware.Scope, req api.BotCreate) (api.BotResult, error) {
	if req.Name == "" || req.Kind == "" {
		return api.BotResult{Invalid: true}, nil
	}
	row, err := s.Create(ctx, scope.Organization, scope.Project, Bot{
		Name: req.Name, Kind: req.Kind, AgentRevisionID: req.AgentRevisionID,
		RepositoryBindingID: req.RepositoryBindingID, PrincipalID: req.PrincipalID, Config: req.Config,
	})
	if errors.Is(err, ErrNameTaken) {
		return api.BotResult{}, api.ErrBotNameTaken
	}
	if err != nil {
		return api.BotResult{}, err
	}
	body, err := render(row)
	if err != nil {
		return api.BotResult{}, err
	}
	return api.BotResult{Body: body}, nil
}

// GetBot implements api.BotRegistry.
func (s *Store) GetBot(ctx context.Context, scope middleware.Scope, id string) (api.BotResult, error) {
	row, found, err := s.Get(ctx, scope.Organization, scope.Project, id)
	if err != nil {
		return api.BotResult{}, err
	}
	if !found {
		return api.BotResult{NotFound: true}, nil
	}
	body, err := render(row)
	if err != nil {
		return api.BotResult{}, err
	}
	return api.BotResult{Body: body}, nil
}

// ListBots implements api.BotRegistry.
func (s *Store) ListBots(ctx context.Context, scope middleware.Scope, q api.ListQuery) ([]api.ListRow, error) {
	window := ListWindow{CreatedGTE: q.CreatedGTE, CreatedLTE: q.CreatedLTE, Limit: q.Limit + 1} // +1 over-fetch: renderPage reads it as has_more
	if q.After != nil {
		window.AfterCreatedAt, window.AfterID = &q.After.CreatedAt, q.After.ID
	}
	rows, err := s.List(ctx, scope.Organization, scope.Project, window)
	if err != nil {
		return nil, err
	}
	out := make([]api.ListRow, 0, len(rows))
	for _, row := range rows {
		body, err := render(row)
		if err != nil {
			return nil, err
		}
		out = append(out, api.ListRow{ID: row.ID, CreatedAt: row.CreatedAt, Body: body})
	}
	return out, nil
}

// PatchBot implements api.BotRegistry.
func (s *Store) PatchBot(ctx context.Context, scope middleware.Scope, id string, patch api.BotPatch) (api.BotResult, error) {
	row, found, err := s.Update(ctx, scope.Organization, scope.Project, id, Patch{
		Name: patch.Name, AgentRevisionID: patch.AgentRevisionID, RepositoryBindingID: patch.RepositoryBindingID,
		PrincipalID: patch.PrincipalID, Config: patch.Config, Disabled: patch.Disabled,
	})
	if errors.Is(err, ErrNameTaken) {
		return api.BotResult{}, api.ErrBotNameTaken
	}
	if err != nil {
		return api.BotResult{}, err
	}
	if !found {
		return api.BotResult{NotFound: true}, nil
	}
	body, err := render(row)
	if err != nil {
		return api.BotResult{}, err
	}
	return api.BotResult{Body: body}, nil
}

// DeleteBot implements api.BotRegistry.
func (s *Store) DeleteBot(ctx context.Context, scope middleware.Scope, id string) (bool, error) {
	return s.Delete(ctx, scope.Organization, scope.Project, id)
}
