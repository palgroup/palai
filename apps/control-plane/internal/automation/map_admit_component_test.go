//go:build component

package automation

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/storage"
)

// TestDeliveryRunEntersSameAdmissionPath pins the headline of AUT (spec §20.2.2): a triggered run is born
// via the SAME §20.9 admission path a POST /v1/responses takes. After a manual delivery, the response +
// run rows exist with the queued shape, the run.queued.v1 birth event is journaled, a response.run
// dispatch job is enqueued (so the SAME workers execute it — no separate loop), and the pinned agent
// revision flows through onto the run (AGT-001: the run pins exactly the revision the trigger named).
func TestDeliveryRunEntersSameAdmissionPath(t *testing.T) {
	store, pool := wiredTriggerStore(t)
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)
	principal := seedPrincipal(t, pool, org, project)

	// A published agent revision to pin the triggered run to.
	agentRev := seedPublishedAgentRevision(t, pool, org, project)

	triggerID, _ := seedTrigger(t, store, org, project, "orders", TriggerRevisionInput{
		AgentRevisionID: agentRev,
		InputMapping:    []byte(`{"fields":{"input":{"select":"order.summary"}},"required":["input"]}`),
	})

	del, err := store.CreateDelivery(ctx, org, project, principal, triggerID, []byte(`{"order":{"summary":"fulfil order 42"}}`))
	if err != nil {
		t.Fatalf("CreateDelivery error = %v", err)
	}
	if del.State != "run_created" {
		t.Fatalf("delivery state = %q, want run_created", del.State)
	}

	// A response row exists, queued, in the delivery's session (same shape as /v1/responses).
	var respState, respSession string
	if err := pool.QueryRow(storage.WithSystemScope(ctx),
		`SELECT state, session_id FROM responses WHERE id = $1 AND organization_id = $2 AND project_id = $3`,
		del.ResponseID, org, project).Scan(&respState, &respSession); err != nil {
		t.Fatalf("read response error = %v", err)
	}
	if respState != "queued" {
		t.Fatalf("response state = %q, want queued", respState)
	}
	if respSession != del.SessionID {
		t.Fatalf("response session = %q, want the delivery session %q", respSession, del.SessionID)
	}

	// The run row exists and pins EXACTLY the trigger's agent revision (AGT-001).
	var pinnedAgentRev *string
	if err := pool.QueryRow(storage.WithSystemScope(ctx),
		`SELECT agent_revision_id FROM runs WHERE id = $1 AND organization_id = $2 AND project_id = $3`,
		del.RunID, org, project).Scan(&pinnedAgentRev); err != nil {
		t.Fatalf("read run error = %v", err)
	}
	if pinnedAgentRev == nil || *pinnedAgentRev != agentRev {
		t.Fatalf("run pinned agent revision = %v, want %q", pinnedAgentRev, agentRev)
	}

	// The birth event + dispatch job — the SAME handoff a POST /v1/responses produces.
	if got := count(t, pool, `SELECT count(*) FROM events WHERE session_id=$1 AND type='run.queued.v1'`, del.SessionID); got != 1 {
		t.Fatalf("run.queued.v1 events = %d, want 1", got)
	}
	if got := count(t, pool, `SELECT count(*) FROM durable_jobs WHERE organization_id=$1 AND kind='response.run'`, org); got < 1 {
		t.Fatal("no response.run dispatch job enqueued (a triggered run must reach the same workers)")
	}
}

// TestMappingFailureFailedDeliveryNoRunEndToEnd pins the pipeline half of AUT-003 (the pure typed-error
// half is TestMappingFailureFailedDeliveryNoRun in mapping_test.go): a schema-invalid mapping fails the
// delivery and leaves NO runs row — no billable run is ever born from an unmappable event.
func TestMappingFailureFailedDeliveryNoRunEndToEnd(t *testing.T) {
	store, pool := wiredTriggerStore(t)
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)
	principal := seedPrincipal(t, pool, org, project)

	// The mapping requires `input` from order.summary, but the payload supplies no summary.
	triggerID, _ := seedTrigger(t, store, org, project, "orders", TriggerRevisionInput{
		InputMapping: []byte(`{"fields":{"input":{"select":"order.summary"}},"required":["input"]}`),
	})

	before := count(t, pool, `SELECT count(*) FROM runs WHERE organization_id=$1 AND project_id=$2`, org, project)
	del, err := store.CreateDelivery(ctx, org, project, principal, triggerID, []byte(`{"order":{"id":"o1"}}`))
	if err != nil {
		t.Fatalf("CreateDelivery error = %v", err)
	}
	if del.State != "failed" {
		t.Fatalf("delivery state = %q, want failed", del.State)
	}
	if del.RunID != "" {
		t.Fatal("a failed delivery must not carry a run")
	}
	after := count(t, pool, `SELECT count(*) FROM runs WHERE organization_id=$1 AND project_id=$2`, org, project)
	if after != before {
		t.Fatalf("runs count changed by %d, want 0 (a failed delivery bills no run)", after-before)
	}
}

// count is a small scalar-count helper.
func count(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(storage.WithSystemScope(context.Background()), query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q error = %v", query, err)
	}
	return n
}

// seedPublishedAgentRevision creates an agent profile + a published revision in scope and returns the
// revision id — a pin a triggered run can resolve (the admission requires a PUBLISHED revision).
func seedPublishedAgentRevision(t *testing.T, pool *pgxpool.Pool, org, project string) string {
	t.Helper()
	ctx := context.Background()
	agents := New(pool)
	profileID, err := agents.CreateProfile(ctx, org, project, randID("profile"))
	if err != nil {
		t.Fatalf("CreateProfile error = %v", err)
	}
	rev, err := agents.CreateRevision(ctx, org, project, profileID, []byte(`{"model":"gpt-4o-mini","instructions":"fulfil orders"}`))
	if err != nil {
		t.Fatalf("CreateRevision error = %v", err)
	}
	if _, _, err := agents.PublishRevision(ctx, org, project, rev.ID); err != nil {
		t.Fatalf("PublishRevision error = %v", err)
	}
	return rev.ID
}
