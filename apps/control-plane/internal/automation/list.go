package automation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/palgroup/palai/storage"
)

// list.go holds the read/LIST keyset reads the E13 Task 4 API surface consumes for the automation
// layer (agent profiles + revisions, triggers). Every read runs under the tenant scope (RLS confines
// the rows; the organization/project predicate is defence-in-depth) and pages by the (created_at, id)
// keyset. No status filter — only the created_at bounds the plan names.

// ListWindow is a resolved keyset page request (see the extensions sibling): the previous page's last
// row, the created_at bounds, and the row cap. It carries no tenant — the scope confines it.
type ListWindow struct {
	CreatedGTE     *time.Time
	CreatedLTE     *time.Time
	AfterCreatedAt *time.Time
	AfterID        string
	Limit          int
}

// AgentProfileItem is an agent-profile lineage's list/get projection.
type AgentProfileItem struct {
	ID        string
	Name      string
	CreatedAt time.Time
}

// GetProfile reads an agent-profile lineage within scope. found=false for a foreign/unknown id (404).
func (s *Store) GetProfile(ctx context.Context, org, project, id string) (AgentProfileItem, bool, error) {
	ctx = storage.ScopeToTenant(ctx, org, project)
	var it AgentProfileItem
	err := s.pool.QueryRow(ctx, storage.Query("GetAgentProfile"), id, org, project).
		Scan(&it.ID, &it.Name, &it.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentProfileItem{}, false, nil
	}
	if err != nil {
		return AgentProfileItem{}, false, fmt.Errorf("get agent profile: %w", err)
	}
	return it, true, nil
}

// ListProfiles returns a tenant-scoped page of agent-profile lineages newest-first (spec §10).
func (s *Store) ListProfiles(ctx context.Context, org, project string, w ListWindow) ([]AgentProfileItem, error) {
	ctx = storage.ScopeToTenant(ctx, org, project)
	rows, err := s.pool.Query(ctx, storage.Query("ListAgentProfiles"),
		org, project, w.CreatedGTE, w.CreatedLTE, w.AfterCreatedAt, w.AfterID, w.Limit)
	if err != nil {
		return nil, fmt.Errorf("list agent profiles: %w", err)
	}
	defer rows.Close()
	var out []AgentProfileItem
	for rows.Next() {
		var it AgentProfileItem
		if err := rows.Scan(&it.ID, &it.Name, &it.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan agent profile row: %w", err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// AgentRevisionItem is one row of a profile's revision list (id + number + config summary + published).
type AgentRevisionItem struct {
	ID             string
	RevisionNumber int
	Model          string
	Instructions   string
	Published      bool
	CreatedAt      time.Time
}

// ListRevisions returns a tenant-scoped page of one profile's revisions newest-first (spec §10). An
// unknown or foreign profile simply yields an empty page (the profile_id predicate under RLS).
func (s *Store) ListRevisions(ctx context.Context, org, project, profileID string, w ListWindow) ([]AgentRevisionItem, error) {
	ctx = storage.ScopeToTenant(ctx, org, project)
	rows, err := s.pool.Query(ctx, storage.Query("ListAgentRevisions"),
		org, project, profileID, w.CreatedGTE, w.CreatedLTE, w.AfterCreatedAt, w.AfterID, w.Limit)
	if err != nil {
		return nil, fmt.Errorf("list agent revisions: %w", err)
	}
	defer rows.Close()
	var out []AgentRevisionItem
	for rows.Next() {
		var it AgentRevisionItem
		if err := rows.Scan(&it.ID, &it.RevisionNumber, &it.Model, &it.Instructions, &it.Published, &it.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan agent revision row: %w", err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// TriggerListItem is one row of the trigger list (the lighter half of TriggerView).
type TriggerListItem struct {
	ID             string
	Name           string
	Type           string
	Enabled        bool
	ActiveRevision int
	CreatedAt      time.Time
}

// ListTriggers returns a tenant-scoped page of triggers newest-first (spec §20.2.2).
func (s *TriggerStore) ListTriggers(ctx context.Context, org, project string, w ListWindow) ([]TriggerListItem, error) {
	ctx = storage.ScopeToTenant(ctx, org, project)
	rows, err := s.pool.Query(ctx, storage.Query("ListTriggers"),
		org, project, w.CreatedGTE, w.CreatedLTE, w.AfterCreatedAt, w.AfterID, w.Limit)
	if err != nil {
		return nil, fmt.Errorf("list triggers: %w", err)
	}
	defer rows.Close()
	var out []TriggerListItem
	for rows.Next() {
		var it TriggerListItem
		if err := rows.Scan(&it.ID, &it.Name, &it.Type, &it.Enabled, &it.ActiveRevision, &it.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan trigger row: %w", err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}
