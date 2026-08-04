//go:build component

// Component test for the E29 T2 singular queue-connection read against REAL PostgreSQL. The route it backs
// is the address POST /v1/queue-connections has named in its 201 Location since the family shipped and which
// nothing served: following the header reached net/http's own not-found. Opening it is one of the two honest
// closures for a dangling Location (the other is deleting the header), and the reason this family got the
// route rather than the deletion is that the list projection already existed and was already vetted as
// non-secret — so the singular read introduces no new disclosure decision, only an address.
//
// What real Postgres is required to prove, and a fake could not: that the read is confined to the caller's
// tenant. RLS is the primary confinement and the query's org/project predicate is defence in depth behind
// it; neither is exercised by an in-memory double.
package automation

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// TestQueueConnectionSingularReadIsTenantScopedAndMatchesTheList is the E29 T2 leg. It proves three things
// about the address the create's Location names: it resolves to the row that was created, it is CONFINED to
// the creating tenant, and it renders the SAME projection the list renders — because a caller that follows a
// create's Location and a caller that pages the list must not be told two different things about one row.
func TestQueueConnectionSingularReadIsTenantScopedAndMatchesTheList(t *testing.T) {
	pool := componentPool(t)
	store := NewQueueStore(pool)
	ctx := context.Background()

	project, _ := seedSession(t, pool)
	strangerProject, _ := seedSession(t, pool)

	config, err := json.Marshal(map[string]string{"agent_revision_id": "arev_1", "principal_id": "prin_1"})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	connID := mustCreateQueueConn(t, store, project, QueueConnectionInput{
		Name: "orders", Direction: "inbound", Visibility: 45 * time.Second, MaxDeliveries: 7, Config: config,
	})

	// 1. The address resolves to the row that was created.
	got, found, err := store.GetQueueConnectionItem(ctx, project, connID)
	if err != nil {
		t.Fatalf("GetQueueConnectionItem error = %v", err)
	}
	if !found {
		t.Fatal("the connection just created is not readable at the address its create advertised")
	}
	if got.ID != connID || got.Name != "orders" || got.Direction != "inbound" {
		t.Fatalf("read back %+v, want the created connection", got)
	}
	if got.MaxDeliveries != 7 || got.Visibility != 45 {
		t.Fatalf("read back visibility=%d max_deliveries=%d, want 45/7 — the singular read must carry the tuning knobs, not just an identity", got.Visibility, got.MaxDeliveries)
	}
	if !got.Enabled {
		t.Fatal("a freshly created connection reads back disabled")
	}

	// 2. It is confined to the creating tenant. A foreign id is indistinguishable from an absent one:
	// RLS makes the row invisible, so there is no oracle telling a stranger the id exists at all.
	if _, found, err := store.GetQueueConnectionItem(ctx, strangerProject, connID); err != nil {
		t.Fatalf("foreign-tenant read error = %v", err)
	} else if found {
		t.Fatal("another tenant read this connection: the singular read is not confined, and the route built on it would leak a connection's run target across tenants")
	}
	if _, found, err := store.GetQueueConnectionItem(ctx, project, "qconn_does_not_exist"); err != nil {
		t.Fatalf("unknown-id read error = %v", err)
	} else if found {
		t.Fatal("an id that was never created reads back as found")
	}

	// 3. The two projections agree field for field. They share one scan function precisely so they cannot
	// drift; this asserts the sharing rather than trusting it.
	page, err := store.ListQueueConnections(ctx, project, ListWindow{Limit: 50})
	if err != nil {
		t.Fatalf("ListQueueConnections error = %v", err)
	}
	var listed *QueueConnectionItem
	for i := range page {
		if page[i].ID == connID {
			listed = &page[i]
			break
		}
	}
	if listed == nil {
		t.Fatalf("the created connection is missing from the list of %d", len(page))
	}
	if listed.Name != got.Name || listed.Kind != got.Kind || listed.Direction != got.Direction ||
		listed.Capacity != got.Capacity || listed.Visibility != got.Visibility ||
		listed.MaxDeliveries != got.MaxDeliveries || listed.Enabled != got.Enabled ||
		!listed.CreatedAt.Equal(got.CreatedAt) || string(listed.Config) != string(got.Config) {
		t.Fatalf("the singular read and the list disagree about one row:\n  singular %+v\n  listed   %+v", got, *listed)
	}
}
