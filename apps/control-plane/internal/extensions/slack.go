package extensions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/palgroup/palai/storage"
)

// The Slack connection registry + thread↔session correlation store (spec §36, E17 Task 1, SLK-001..008). A
// connection is an admin-registered workspace binding whose signing secret + bot token are secret_ref
// HANDLES only — a credential can never enter a row (DisallowUnknownFields rejects an inline value field),
// so it also never reaches a log or an evidence bundle. This is the tenant-scoped, RLS-forced store the
// PURE adapters/integrations/slack package's verify + mapping are wired against control-plane-side; the
// adapter itself holds no database, exactly like the webhook seam.

var (
	// ErrInvalidSlackConfig is a missing team id or signing-secret ref, or a malformed body.
	ErrInvalidSlackConfig = errors.New("extensions: slack connection config is invalid")
	// ErrSlackConnectionExists is a workspace already bound in this project (team_id + enterprise_id).
	ErrSlackConnectionExists = errors.New("extensions: slack connection already exists for this workspace")
	// ErrSlackConnectionNotFound is a get/resolve for a connection absent from scope.
	ErrSlackConnectionNotFound = errors.New("extensions: slack connection not found in scope")
)

// SlackConnection is a registered workspace binding's committed shape. The refs are handles, never values.
type SlackConnection struct {
	ID               string
	TeamID           string
	EnterpriseID     string
	BotUserID        string
	SigningSecretRef string
	BotTokenRef      string
	Scopes           string
	Disabled         bool
}

// SlackConnectionInput is the strict-decoded create body. A field outside this struct — including a raw
// `signing_secret` / `bot_token` VALUE — is rejected, so a credential can only enter as a *_ref handle.
type SlackConnectionInput struct {
	TeamID           string         `json:"team_id"`
	EnterpriseID     string         `json:"enterprise_id"`
	BotUserID        string         `json:"bot_user_id"`
	SigningSecretRef string         `json:"signing_secret_ref"`
	BotTokenRef      string         `json:"bot_token_ref"`
	Scopes           string         `json:"scopes"`
	AllowedChannels  []string       `json:"allowed_channels"`
	AllowedUsers     []string       `json:"allowed_users"`
	DefaultPolicy    map[string]any `json:"default_policy"`
}

// CreateSlackConnection registers a workspace binding. It is an admin action — never reachable from a tool
// the model can call. A team already bound in the project is a typed collision.
func (s *Store) CreateSlackConnection(ctx context.Context, org, project string, raw []byte) (SlackConnection, error) {
	ctx = storage.ScopeToTenant(ctx, org, project)
	in, err := decodeSlackInput(raw)
	if err != nil {
		return SlackConnection{}, err
	}
	if in.TeamID == "" {
		return SlackConnection{}, fmt.Errorf("%w: team_id is required", ErrInvalidSlackConfig)
	}
	if in.SigningSecretRef == "" {
		return SlackConnection{}, fmt.Errorf("%w: signing_secret_ref is required (the v0 verify resolves it)", ErrInvalidSlackConfig)
	}
	id := newID("slkc")
	if _, err := s.pool.Exec(ctx, storage.Query("InsertSlackConnection"),
		id, org, project, in.TeamID, in.EnterpriseID, in.BotUserID,
		in.SigningSecretRef, in.BotTokenRef, in.Scopes,
		marshalJSON(orEmptyList(in.AllowedChannels)), marshalJSON(orEmptyList(in.AllowedUsers)),
		marshalJSON(orEmptyObject(in.DefaultPolicy))); err != nil {
		if isUniqueViolation(err) {
			return SlackConnection{}, ErrSlackConnectionExists
		}
		return SlackConnection{}, fmt.Errorf("insert slack connection: %w", err)
	}
	return SlackConnection{
		ID: id, TeamID: in.TeamID, EnterpriseID: in.EnterpriseID, BotUserID: in.BotUserID,
		SigningSecretRef: in.SigningSecretRef, BotTokenRef: in.BotTokenRef, Scopes: in.Scopes,
	}, nil
}

// GetSlackConnection reads a connection's metadata within scope (the refs are handles, safe to return).
func (s *Store) GetSlackConnection(ctx context.Context, org, project, id string) (SlackConnection, error) {
	ctx = storage.ScopeToTenant(ctx, org, project)
	c := SlackConnection{ID: id}
	err := s.pool.QueryRow(ctx, storage.Query("GetSlackConnection"), id, org, project).
		Scan(&c.ID, &c.TeamID, &c.EnterpriseID, &c.BotUserID, &c.SigningSecretRef, &c.BotTokenRef, &c.Scopes, &c.Disabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return SlackConnection{}, ErrSlackConnectionNotFound
	}
	if err != nil {
		return SlackConnection{}, fmt.Errorf("read slack connection: %w", err)
	}
	return c, nil
}

// ResolvedSlackConnection is what the UNAUTHENTICATED inbound path learns from a Slack team id before it has
// a tenant: the org/project it belongs to and the secret_ref handles the caller verifies + replies under.
type ResolvedSlackConnection struct {
	ID               string
	Org              string
	Project          string
	SigningSecretRef string
	BotTokenRef      string
	BotUserID        string
	Disabled         bool
}

// ResolveSlackConnectionByTeam establishes the tenant for a signed inbound Slack callback, keyed by the
// team + enterprise id the callback carries (the resolveInboundTrigger idiom). It runs SYSTEM-scoped because
// there is no tenant yet; the caller must still present a valid v0 signature over the returned
// signing_secret_ref before anything is written — the signature is the auth, not this lookup.
func (s *Store) ResolveSlackConnectionByTeam(ctx context.Context, teamID, enterpriseID string) (ResolvedSlackConnection, bool, error) {
	ctx = storage.WithSystemScope(ctx)
	var r ResolvedSlackConnection
	switch err := s.pool.QueryRow(ctx, storage.Query("ResolveSlackConnectionByTeam"), teamID, enterpriseID).
		Scan(&r.ID, &r.Org, &r.Project, &r.SigningSecretRef, &r.BotTokenRef, &r.BotUserID, &r.Disabled); {
	case errors.Is(err, pgx.ErrNoRows):
		return ResolvedSlackConnection{}, false, nil
	case err != nil:
		return ResolvedSlackConnection{}, false, fmt.Errorf("resolve slack connection: %w", err)
	}
	return r, true, nil
}

// CorrelateThreadSession resolves the canonical session for a (team, channel, thread), creating the mapping
// on the first event and REUSING it on every later event in the same thread (SLK-003). The claim is
// single-winner: a concurrent race collapses at the unique index, so the loser reads the winner's session
// rather than opening a second one. It returns the canonical session id and whether this call created it.
func (s *Store) CorrelateThreadSession(ctx context.Context, org, project, connID, team, channel, thread, sessionID string) (string, bool, error) {
	ctx = storage.ScopeToTenant(ctx, org, project)
	id := newID("slkts")
	var inserted string
	err := s.pool.QueryRow(ctx, storage.Query("CorrelateThreadSession"),
		id, org, project, connID, team, channel, thread, sessionID).Scan(&inserted)
	switch {
	case err == nil:
		return sessionID, true, nil // fresh claim — this call's session is canonical
	case errors.Is(err, pgx.ErrNoRows):
		// The thread was already correlated (ON CONFLICT DO NOTHING returned no row): reuse the winner.
		existing, _, rerr := s.threadSession(ctx, org, project, team, channel, thread)
		if rerr != nil {
			return "", false, rerr
		}
		return existing, false, nil
	default:
		return "", false, fmt.Errorf("correlate thread session: %w", err)
	}
}

// threadSession reads the canonical session (and its last visible bot message ts) a thread resolved to.
func (s *Store) threadSession(ctx context.Context, org, project, team, channel, thread string) (string, string, error) {
	var sessionID, lastTS string
	err := s.pool.QueryRow(ctx, storage.Query("GetThreadSession"), org, project, team, channel, thread).Scan(&sessionID, &lastTS)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrSlackConnectionNotFound
	}
	if err != nil {
		return "", "", fmt.Errorf("read thread session: %w", err)
	}
	return sessionID, lastTS, nil
}

// decodeSlackInput strict-decodes the create body; an unknown field (an inline secret VALUE among them) is
// rejected, so a credential can only ever arrive as a *_ref handle.
func decodeSlackInput(raw []byte) (SlackConnectionInput, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var in SlackConnectionInput
	if err := dec.Decode(&in); err != nil {
		// An inline `signing_secret`/`bot_token` VALUE (or any other unknown field) lands here — the
		// decodeMCPConnectionInput precedent maps every strict-decode failure to ErrUnknownField.
		return SlackConnectionInput{}, fmt.Errorf("%w: %v", ErrUnknownField, err)
	}
	return in, nil
}

func orEmptyList(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func orEmptyObject(v map[string]any) map[string]any {
	if v == nil {
		return map[string]any{}
	}
	return v
}
