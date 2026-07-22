package coordinator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/palgroup/palai/storage"
)

// SessionView is a session's projection for GET /v1/sessions/{id}. Found is false for an
// unknown or foreign id (404, no existence disclosure).
type SessionView struct {
	ID        string
	State     string
	CreatedAt time.Time
	Found     bool
}

// CreateSession opens a fresh session (spec §9.1 POST /v1/sessions). The id is caller-minted;
// the session starts active. It is the standalone counterpart of admission's implicit session
// creation, deferred from T1.
func (s *Store) CreateSession(ctx context.Context, tenant Tenant, sessionID string) (SessionView, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	if _, err := s.pool.Exec(ctx, storage.Query("InsertSession"), sessionID, tenant.Organization, tenant.Project); err != nil {
		return SessionView{}, fmt.Errorf("insert session: %w", err)
	}
	return s.GetSession(ctx, tenant, sessionID)
}

// GetSession reads a session's projection within the tenant scope (spec §9.1 GET). A foreign
// or unknown id yields Found=false, so the caller renders a 404 that leaks no cross-tenant
// existence (§39.2).
func (s *Store) GetSession(ctx context.Context, tenant Tenant, sessionID string) (SessionView, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	var v SessionView
	err := s.pool.QueryRow(ctx, storage.Query("GetSessionInScope"), sessionID, tenant.Organization, tenant.Project).
		Scan(&v.ID, &v.State, &v.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionView{}, nil
	}
	if err != nil {
		return SessionView{}, fmt.Errorf("read session %s: %w", sessionID, err)
	}
	v.Found = true
	return v, nil
}
