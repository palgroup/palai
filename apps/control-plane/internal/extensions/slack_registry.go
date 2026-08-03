package extensions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/storage"
)

// SlackRegistry is the api.SlackConnectionAPI adapter over the connection store (E19 T9): the ONE
// production caller CreateSlackConnection never had. Before it, registering a workspace meant hand-written
// SQL against slack_connections — which made the phase's handover promise ("supply the credentials, run the
// live legs unchanged") untrue on its first step.
//
// It is deliberately NOT folded into SlackAdmitter. That type is the ADMISSION bridge: it resolves a
// connection for an unauthenticated callback and drives runs. Registration is a bearer-scoped operator
// action with a different tenant authority, and a seam that did both would be one nil check away from an
// admission path that could also write connection rows.
type SlackRegistry struct{ store *Store }

// NewSlackRegistry wires the registration surface onto the connection store.
func NewSlackRegistry(store *Store) *SlackRegistry { return &SlackRegistry{store: store} }

// Compile-time proof this satisfies the router's seam.
var _ api.SlackConnectionAPI = (*SlackRegistry)(nil)

// CreateSlackConnection registers a workspace binding in the CALLER's tenant and maps the store's typed
// refusals onto the two the api surface knows.
//
// BOTH conflicts map to ONE error, and that is the security property rather than a shortcut: the store
// distinguishes "already bound in your own project" from "already bound by a DIFFERENT tenant" (the E19 T1
// cross-tenant hijack fix), and the registering admin must not learn which — the second answer proves
// another customer exists and holds that workspace. Collapsing it here means the HTTP layer is structurally
// incapable of telling them apart. The control-plane-side log inside CreateSlackConnection still names the
// colliding connection id for the operator who has to resolve it.
func (r *SlackRegistry) CreateSlackConnection(ctx context.Context, project string, raw []byte) (string, error) {
	org, err := storage.OrganizationForProject(ctx, r.store.pool, project)
	if err != nil {
		return "", err
	}
	conn, err := r.store.CreateSlackConnection(ctx, org, project, raw)
	switch {
	case errors.Is(err, ErrSlackConnectionExists), errors.Is(err, ErrSlackWorkspaceBoundElsewhere):
		return "", api.ErrSlackRegistrationConflict
	case errors.Is(err, ErrInvalidSlackConfig):
		return "", api.ErrSlackRegistrationInvalid
	case err != nil:
		return "", err
	}
	return conn.ID, nil
}

// ListSlackConnections returns a tenant-scoped page of registered workspaces, newest-first. The projection
// carries no secret-ref handles (storage/queries/slack.sql omits them from the list).
func (r *SlackRegistry) ListSlackConnections(ctx context.Context, project string, w api.SlackListWindow) ([]api.SlackConnectionItem, error) {
	org, err := storage.OrganizationForProject(ctx, r.store.pool, project)
	if err != nil {
		return nil, err
	}
	ctx = storage.ScopeToTenant(ctx, org, project)
	rows, err := r.store.pool.Query(ctx, storage.Query("ListSlackConnections"),
		project, w.CreatedGTE, w.CreatedLTE, w.AfterCreatedAt, w.AfterID, w.Limit)
	if err != nil {
		return nil, fmt.Errorf("list slack connections: %w", err)
	}
	defer rows.Close()
	var out []api.SlackConnectionItem
	for rows.Next() {
		var it api.SlackConnectionItem
		if err := rows.Scan(&it.ID, &it.TeamID, &it.EnterpriseID, &it.BotUserID, &it.Disabled, &it.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan slack connection row: %w", err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// GetSlackConnection reads one binding's handles for the repair surface. The store method it wraps already
// scopes to the tenant and answers ErrSlackConnectionNotFound for a foreign id, so a cross-tenant read is
// indistinguishable here from a nonexistent one — which is what the 404 upstream depends on.
func (r *SlackRegistry) GetSlackConnection(ctx context.Context, project, id string) (api.SlackConnectionDetail, bool, error) {
	org, err := storage.OrganizationForProject(ctx, r.store.pool, project)
	if err != nil {
		return api.SlackConnectionDetail{}, false, err
	}
	conn, err := r.store.GetSlackConnection(ctx, org, project, id)
	switch {
	case errors.Is(err, ErrSlackConnectionNotFound):
		return api.SlackConnectionDetail{}, false, nil
	case err != nil:
		return api.SlackConnectionDetail{}, false, err
	}
	return api.SlackConnectionDetail{
		ID: conn.ID, TeamID: conn.TeamID, EnterpriseID: conn.EnterpriseID, BotUserID: conn.BotUserID,
		SigningSecretRef: conn.SigningSecretRef, BotTokenRef: conn.BotTokenRef, AppTokenRef: conn.AppTokenRef,
		Scopes: conn.Scopes, Disabled: conn.Disabled,
	}, true, nil
}

// UpdateSlackConnection applies a partial revision. Every nil field COALESCEs to the stored value in the
// statement, so this method converts "absent" to SQL NULL and nothing else — it does not read-modify-write,
// which would race a concurrent revise and silently restore whatever it had read.
func (r *SlackRegistry) UpdateSlackConnection(ctx context.Context, project, id string, patch api.SlackConnectionPatch) (bool, error) {
	org, err := storage.OrganizationForProject(ctx, r.store.pool, project)
	if err != nil {
		return false, err
	}
	channels, err := jsonListOrNil(patch.AllowedChannels)
	if err != nil {
		return false, err
	}
	users, err := jsonListOrNil(patch.AllowedUsers)
	if err != nil {
		return false, err
	}
	var policy []byte
	if len(patch.DefaultPolicy) > 0 {
		policy = patch.DefaultPolicy
	}
	var updated string
	switch err := r.store.pool.QueryRow(storage.ScopeToTenant(ctx, org, project),
		storage.Query("UpdateSlackConnection"), id, project,
		patch.BotUserID, patch.SigningSecretRef, patch.BotTokenRef, patch.AppTokenRef, patch.Scopes,
		channels, users, policy, patch.Disabled).Scan(&updated); {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("update slack connection: %w", err)
	}
	return true, nil
}

// DeleteSlackConnection unbinds a workspace. The thread correlations go first (000035's FK is a plain
// reference, so the connection cannot be deleted while any thread points at it) and both statements share
// ONE transaction: a half-applied unbind would leave correlations pointing at a connection that is gone,
// and every later event in those threads would resolve a row the registry no longer has.
func (r *SlackRegistry) DeleteSlackConnection(ctx context.Context, project, id string) (bool, error) {
	org, err := storage.OrganizationForProject(ctx, r.store.pool, project)
	if err != nil {
		return false, err
	}
	ctx = storage.ScopeToTenant(ctx, org, project)
	tx, err := r.store.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin slack connection delete: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, storage.Query("DeleteSlackConnectionThreads"), id, project); err != nil {
		return false, fmt.Errorf("delete slack connection threads: %w", err)
	}
	var deleted string
	switch err := tx.QueryRow(ctx, storage.Query("DeleteSlackConnection"), id, project).Scan(&deleted); {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil // nothing committed: the rollback above undoes the thread delete too
	case err != nil:
		return false, fmt.Errorf("delete slack connection: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit slack connection delete: %w", err)
	}
	return true, nil
}

// jsonListOrNil encodes an allow-list for the JSONB parameter. nil stays nil (SQL NULL ⇒ COALESCE keeps the
// stored list); a present-but-empty list encodes as `[]`, which is a real revision meaning "no restriction"
// for channels and "nobody" for users — the asymmetry SlackAuthorizationPolicy documents.
func jsonListOrNil(list []string) ([]byte, error) {
	if list == nil {
		return nil, nil
	}
	raw, err := json.Marshal(list)
	if err != nil {
		return nil, fmt.Errorf("encode slack allow-list: %w", err)
	}
	return raw, nil
}
