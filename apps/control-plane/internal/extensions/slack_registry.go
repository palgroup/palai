package extensions

import (
	"context"
	"errors"
	"fmt"

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
func (r *SlackRegistry) CreateSlackConnection(ctx context.Context, org, project string, raw []byte) (string, error) {
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
func (r *SlackRegistry) ListSlackConnections(ctx context.Context, org, project string, w api.SlackListWindow) ([]api.SlackConnectionItem, error) {
	ctx = storage.ScopeToTenant(ctx, org, project)
	rows, err := r.store.pool.Query(ctx, storage.Query("ListSlackConnections"),
		org, project, w.CreatedGTE, w.CreatedLTE, w.AfterCreatedAt, w.AfterID, w.Limit)
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
