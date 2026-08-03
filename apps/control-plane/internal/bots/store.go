// Package bots is the kind-agnostic bot registry (migration 000061, 2026-08-03 plan Task 4): a project's
// registered bots, one row per relay process the console can create. It lives in its own package for the
// same reason internal/fleet does — it is not tool/skill/hook/channel-adapter management
// (internal/extensions) and it is not a runner (internal/fleet); it is a fourth, unrelated inventory, and
// folding it into an existing package would make an unrelated import direction the only thing keeping it
// "generic".
//
// THE ONE RULE: this package never learns what a bot IS. `Kind` is a bare string nothing here branches
// on, and `Config` is carried as json.RawMessage from the wire to the row and back — no method decodes
// it, so a field one channel's adapter needs never requires a change here.
package bots

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/storage"
)

// Bot is one registered row.
type Bot struct {
	ID                  string
	Organization        string
	Project             string
	Name                string
	Kind                string
	AgentRevisionID     string
	RepositoryBindingID string
	PrincipalID         string
	Config              json.RawMessage
	Disabled            bool
	CreatedAt           time.Time
}

// Patch is a partial revision: a nil field is unchanged, Config nil means unchanged. Kind carries no
// field — see queries/bots.sql's UpdateBot for why.
type Patch struct {
	Name                *string
	AgentRevisionID     *string
	RepositoryBindingID *string
	PrincipalID         *string
	Config              json.RawMessage
	Disabled            *bool
}

// ListWindow is the keyset page window List takes, matching the fleet.ListWindow idiom.
type ListWindow struct {
	CreatedGTE     *time.Time
	CreatedLTE     *time.Time
	AfterCreatedAt *time.Time
	AfterID        string
	Limit          int
}

// ErrNameTaken is returned by Create when this project already has a bot by that name — migration
// 000061's UNIQUE (organization_id, project_id, name) surfacing as an answer rather than as a 500.
var ErrNameTaken = errors.New("bots: a bot with that name already exists in this project")

// Store is the Postgres-backed registry.
type Store struct {
	pool  *pgxpool.Pool
	newID func(prefix string) string
	now   func() time.Time
}

// NewStore builds the registry over the durable spine's pool. newID mints row ids (pass
// middleware.NewID in production); now defaults to time.Now when nil.
func NewStore(pool *pgxpool.Pool, newID func(prefix string) string, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{pool: pool, newID: newID, now: now}
}

// Create writes a bot within the tenant and returns it as written. An empty Config is stored as `{}`,
// matching the column's default — a create that names no config is indistinguishable from one that
// named an empty document, which is the honest reading: nothing has configured this bot's channel yet.
func (s *Store) Create(ctx context.Context, org, project string, in Bot) (Bot, error) {
	row := Bot{
		ID: s.newID("bot"), Organization: org, Project: project, Name: in.Name, Kind: in.Kind,
		AgentRevisionID: in.AgentRevisionID, RepositoryBindingID: in.RepositoryBindingID,
		PrincipalID: in.PrincipalID, Config: in.Config,
	}
	if len(row.Config) == 0 {
		row.Config = json.RawMessage(`{}`)
	}
	ctx = storage.WithTenant(ctx, org, project)
	err := s.pool.QueryRow(ctx, storage.Query("InsertBot"),
		row.ID, row.Organization, row.Project, row.Name, row.Kind, row.AgentRevisionID,
		row.RepositoryBindingID, row.PrincipalID, []byte(row.Config)).Scan(&row.CreatedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return Bot{}, ErrNameTaken
	}
	if err != nil {
		return Bot{}, fmt.Errorf("insert bot: %w", err)
	}
	return row, nil
}

// Get resolves one bot within the tenant. found=false is a foreign or unknown id, indistinguishable so a
// caller cannot learn which.
func (s *Store) Get(ctx context.Context, org, project, id string) (Bot, bool, error) {
	if id == "" {
		return Bot{}, false, nil
	}
	ctx = storage.WithTenant(ctx, org, project)
	row := Bot{Organization: org, Project: project}
	var config []byte
	err := s.pool.QueryRow(ctx, storage.Query("GetBot"), id, org, project).Scan(
		&row.ID, &row.Name, &row.Kind, &row.AgentRevisionID, &row.RepositoryBindingID, &row.PrincipalID,
		&config, &row.Disabled, &row.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Bot{}, false, nil
	}
	if err != nil {
		return Bot{}, false, fmt.Errorf("get bot: %w", err)
	}
	row.Config = json.RawMessage(config)
	return row, true, nil
}

// List pages a project's bots newest-first, over-fetching Limit+1 so the caller can detect a further
// page — the shared keyset idiom every list surface in this tree uses.
func (s *Store) List(ctx context.Context, org, project string, w ListWindow) ([]Bot, error) {
	ctx = storage.WithTenant(ctx, org, project)
	rows, err := s.pool.Query(ctx, storage.Query("ListBots"),
		org, project, w.CreatedGTE, w.CreatedLTE, w.AfterCreatedAt, w.AfterID, w.Limit)
	if err != nil {
		return nil, fmt.Errorf("list bots: %w", err)
	}
	defer rows.Close()
	var out []Bot
	for rows.Next() {
		row := Bot{Organization: org, Project: project}
		var config []byte
		if err := rows.Scan(&row.ID, &row.Name, &row.Kind, &row.AgentRevisionID, &row.RepositoryBindingID,
			&row.PrincipalID, &config, &row.Disabled, &row.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan bot row: %w", err)
		}
		row.Config = json.RawMessage(config)
		out = append(out, row)
	}
	return out, rows.Err()
}

// Update applies a partial revision and returns the row as it now stands. found=false is a foreign or
// unknown id. It does not read-modify-write: every unmentioned field is a SQL NULL parameter, and the
// statement's own COALESCE resolves it against the stored value, so a concurrent revision can never be
// silently reverted by a stale read.
func (s *Store) Update(ctx context.Context, org, project, id string, patch Patch) (Bot, bool, error) {
	if id == "" {
		return Bot{}, false, nil
	}
	ctx = storage.WithTenant(ctx, org, project)
	var config []byte
	if len(patch.Config) > 0 {
		config = []byte(patch.Config)
	}
	var updated string
	err := s.pool.QueryRow(ctx, storage.Query("UpdateBot"),
		id, org, project, patch.Name, patch.AgentRevisionID, patch.RepositoryBindingID, patch.PrincipalID,
		config, patch.Disabled).Scan(&updated)
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		return Bot{}, false, ErrNameTaken
	case errors.Is(err, pgx.ErrNoRows):
		return Bot{}, false, nil
	case err != nil:
		return Bot{}, false, fmt.Errorf("update bot: %w", err)
	}
	return s.Get(ctx, org, project, id)
}

// Delete unregisters a bot. found=false is a foreign or unknown id. A hard delete — unlike repository
// bindings' archive — because nothing in this schema holds a foreign key onto integration_bots (the
// plan's own rule), so there is no provenance row a delete could falsify.
func (s *Store) Delete(ctx context.Context, org, project, id string) (bool, error) {
	if id == "" {
		return false, nil
	}
	ctx = storage.WithTenant(ctx, org, project)
	var deleted string
	err := s.pool.QueryRow(ctx, storage.Query("DeleteBot"), id, org, project).Scan(&deleted)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("delete bot: %w", err)
	}
	return true, nil
}
