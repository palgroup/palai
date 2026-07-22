//go:build component

package postgres

// E13 Task 4: the run-history keyset list over the REAL coordinator + Postgres. It proves the
// tenant-scoped page (a second tenant's rows never appear), the (created_at, id) keyset paging, and
// the status filter — the durable half the conformance/e2e HTTP tiers layer the cursor on top of.

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/packages/coordinator"
)

// seedResponse inserts a response in a state under a tenant's session and returns its id. The
// default clock_timestamp() created_at increases per insert, so a sequence of calls yields a
// deterministic keyset order.
func seedResponse(t *testing.T, pool *pgxpool.Pool, tenant coordinator.Tenant, sessionID, state string) string {
	t.Helper()
	id := newID("resp")
	exec(t, pool,
		`INSERT INTO responses (id, organization_id, project_id, session_id, state, input) VALUES ($1, $2, $3, $4, $5, '{}'::jsonb)`,
		id, tenant.Organization, tenant.Project, sessionID, state)
	return id
}

func TestListResponsesTenantScopedKeysetAndFilter(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	pool := cs.Pool()

	tenantA, sessionA, _ := seedRun(t, pool)
	tenantB, sessionB, _ := seedRun(t, pool)

	// Three runs in A's history (oldest -> newest), one in B's.
	seedResponse(t, pool, tenantA, sessionA, "queued")
	aCompleted := seedResponse(t, pool, tenantA, sessionA, "completed")
	aNewest := seedResponse(t, pool, tenantA, sessionA, "failed")
	bOnly := seedResponse(t, pool, tenantB, sessionB, "completed")

	// Page 1 of A's history, newest first.
	page1, err := cs.ListResponses(ctx, tenantA, coordinator.ListParams{Limit: 2})
	if err != nil {
		t.Fatalf("ListResponses(page1) error = %v", err)
	}
	if len(page1) != 2 {
		t.Fatalf("page1 len = %d, want 2", len(page1))
	}
	if page1[0].ID != aNewest {
		t.Fatalf("page1[0] = %s, want the newest run %s (created_at DESC)", page1[0].ID, aNewest)
	}
	for _, it := range page1 {
		if it.ID == bOnly {
			t.Fatalf("A's list returned B's run %s — the page is not tenant-scoped", bOnly)
		}
	}

	// Page 2 keys off page 1's last row.
	last := page1[len(page1)-1]
	page2, err := cs.ListResponses(ctx, tenantA, coordinator.ListParams{
		Limit: 2, AfterCreatedAt: &last.CreatedAt, AfterID: last.ID,
	})
	if err != nil {
		t.Fatalf("ListResponses(page2) error = %v", err)
	}
	if len(page2) != 1 {
		t.Fatalf("page2 len = %d, want the 1 remaining A run", len(page2))
	}
	if page2[0].ID == last.ID {
		t.Fatalf("page2 repeated the keyset boundary row %s", last.ID)
	}

	// Status filter narrows to the single completed run.
	completed, err := cs.ListResponses(ctx, tenantA, coordinator.ListParams{Limit: 10, Status: "completed"})
	if err != nil {
		t.Fatalf("ListResponses(status) error = %v", err)
	}
	if len(completed) != 1 || completed[0].ID != aCompleted {
		t.Fatalf("status=completed returned %+v, want only %s", completed, aCompleted)
	}

	// B's own list sees only B's run.
	bList, err := cs.ListResponses(ctx, tenantB, coordinator.ListParams{Limit: 10})
	if err != nil {
		t.Fatalf("ListResponses(B) error = %v", err)
	}
	if len(bList) != 1 || bList[0].ID != bOnly {
		t.Fatalf("B's list = %+v, want only %s", bList, bOnly)
	}
}
