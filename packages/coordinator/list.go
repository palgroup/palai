package coordinator

import (
	"context"
	"fmt"
	"time"

	"github.com/palgroup/palai/storage"
)

// list.go holds the read/LIST keyset reads the E13 Task 4 API surface consumes. Every list runs
// under the tenant scope (RLS confines the rows; the organization/project predicate in each query is
// defence-in-depth), pages by the (created_at, id) keyset, and fetches Limit rows — the caller passes
// Limit+1 to detect a further page. There is no filter DSL: only the status and created_at bounds the
// plan names.

// ListParams is a resolved, tenant-safe keyset page request. AfterCreatedAt/AfterID is the position
// of the last row of the previous page (nil AfterCreatedAt means the first page); Status and the
// CreatedGTE/CreatedLTE bounds are the two basic filters; Limit is the row cap (the caller adds the
// +1 over-fetch). It carries no tenant — the scope on the context confines every row.
type ListParams struct {
	AfterCreatedAt *time.Time
	AfterID        string
	Status         string
	CreatedGTE     *time.Time
	CreatedLTE     *time.Time
	Limit          int
}

// ResponseListItem is one row of the run-history list: the durable response columns. model/usage/
// output are deliberately absent — they come from GetResponse, so a page is a cheap keyset scan.
type ResponseListItem struct {
	ID        string
	State     string
	SessionID string
	CreatedAt time.Time
}

// ListResponses returns a tenant-scoped page of run history newest-first (spec §22.3, E13 T4).
func (s *Store) ListResponses(ctx context.Context, tenant Tenant, p ListParams) ([]ResponseListItem, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	rows, err := s.pool.Query(ctx, storage.Query("ListResponses"),
		tenant.Organization, tenant.Project, p.Status, p.CreatedGTE, p.CreatedLTE, p.AfterCreatedAt, p.AfterID, p.Limit)
	if err != nil {
		return nil, fmt.Errorf("list responses: %w", err)
	}
	defer rows.Close()
	var out []ResponseListItem
	for rows.Next() {
		var it ResponseListItem
		if err := rows.Scan(&it.ID, &it.State, &it.SessionID, &it.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan response row: %w", err)
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate response rows: %w", err)
	}
	return out, nil
}
