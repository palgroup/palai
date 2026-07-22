//go:build fault

// Inbound signed-webhook durable-ack recovery (spec §34.2, AUT-002/009): a crash BETWEEN the durable
// insert (2xx earned) and the run leaves a sweepable delivery row the supervised delivery-reconciler
// finishes EXACTLY once. This joins the scheduler fault suite (package fault; `make test-fault
// CASE=scheduler`) — it exercises the REAL reconciler Tick against real Postgres, modelling the crash the
// outage_test.go way (the continuation is simply never driven, not sleep-simulated: the durable row is
// seeded and the loop starts fresh).
package fault

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/apps/control-plane/internal/automation"
	"github.com/palgroup/palai/packages/coordinator"
)

// TestAckedDeliveryCrashBeforeRunRecoversExactlyOnce seeds a durable inbound delivery in `received` (the
// state right after the durable insert, before the inline map→admit ran — the crash window) and proves the
// reconciler drives it to exactly ONE run, with a second Tick adding nothing.
func TestAckedDeliveryCrashBeforeRunRecoversExactlyOnce(t *testing.T) {
	ctx := context.Background()
	spine, err := coordinator.Open(ctx, faultURL(t))
	if err != nil {
		t.Fatalf("coordinator.Open() error = %v", err)
	}
	t.Cleanup(spine.Close)
	if err := spine.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	pool := spine.Pool()
	org, proj, principal := randID("org"), randID("prj"), randID("prin")
	exec := func(sql string, args ...any) {
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed exec %q error = %v", sql, err)
		}
	}
	exec(`INSERT INTO organizations (id) VALUES ($1)`, org)
	exec(`INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, proj, org)
	exec(`INSERT INTO principals (id, organization_id, project_id, kind) VALUES ($1, $2, $3, 'service')`, principal, org, proj)

	// A published AgentRevision + a webhook trigger pinned to it (the inbound run target), created AS the
	// principal so the delivery's principal_id resolves.
	agents := automation.New(pool)
	profileID, err := agents.CreateProfile(ctx, org, proj, randID("profile"))
	if err != nil {
		t.Fatalf("CreateProfile error = %v", err)
	}
	rev, err := agents.CreateRevision(ctx, org, proj, profileID, []byte(`{"model":"gpt-4o-mini","instructions":"inbound work"}`))
	if err != nil {
		t.Fatalf("CreateRevision error = %v", err)
	}
	if _, _, err := agents.PublishRevision(ctx, org, proj, rev.ID); err != nil {
		t.Fatalf("PublishRevision error = %v", err)
	}
	store := automation.NewTriggerStore(pool).WithAdmitter(spine)
	triggerID, err := store.CreateTrigger(ctx, org, proj, principal, randID("inbound-trg"), "webhook")
	if err != nil {
		t.Fatalf("CreateTrigger error = %v", err)
	}
	triggerRev, err := store.ReviseTrigger(ctx, org, proj, triggerID, automation.TriggerRevisionInput{
		AgentRevisionID: rev.ID,
		InputMapping:    []byte(`{"fields":{"input":{"select":"order.summary"}},"required":["input"]}`),
	})
	if err != nil {
		t.Fatalf("ReviseTrigger error = %v", err)
	}

	// The crash window: a durable inbound row (raw_payload + source cols + principal) committed, but the
	// inline continuation never ran (the loop was down). updated_at in the past so grace=0 sweeps it.
	deliveryID := randID("tdel")
	exec(`INSERT INTO trigger_deliveries
	        (id, organization_id, project_id, trigger_id, trigger_revision_id, principal_id,
	         source, source_tenant, source_event_id, raw_payload, state, received_at, updated_at)
	      VALUES ($1,$2,$3,$4,$5,$6,'harness','','evt-crash',$7,'received',
	              clock_timestamp() - interval '10 seconds', clock_timestamp() - interval '10 seconds')`,
		deliveryID, org, proj, triggerID, triggerRev.ID, principal,
		[]byte(`{"source":"harness","data":{"order":{"id":"o-crash","summary":"finish me"}}}`))

	reconciler := automation.NewDeliveryReconciler(store, time.Second, 0, 100, nil)

	// Restart: the FIRST Tick re-drives the durable row to a born run.
	if err := reconciler.Tick(ctx); err != nil {
		t.Fatalf("first Tick error = %v", err)
	}
	state, runID := deliveryStateRun(t, pool, deliveryID)
	if state != "run_created" || runID == "" {
		t.Fatalf("after recovery: state=%q run=%q, want run_created with a run", state, runID)
	}

	// Exactly once: a SECOND Tick admits no second run (the idempotency key is the delivery id).
	if err := reconciler.Tick(ctx); err != nil {
		t.Fatalf("second Tick error = %v", err)
	}
	state2, runID2 := deliveryStateRun(t, pool, deliveryID)
	if state2 != "run_created" || runID2 != runID {
		t.Fatalf("second Tick changed the outcome: state=%q run=%q (was %q)", state2, runID2, runID)
	}
	var runs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM trigger_deliveries WHERE trigger_id=$1 AND run_id <> ''`, triggerID).Scan(&runs); err != nil {
		t.Fatalf("count runs error = %v", err)
	}
	if runs != 1 {
		t.Fatalf("run count = %d, want exactly 1 (crash recovered exactly once)", runs)
	}
}

func deliveryStateRun(t *testing.T, pool *pgxpool.Pool, deliveryID string) (string, string) {
	t.Helper()
	var state, runID string
	if err := pool.QueryRow(context.Background(), `SELECT state, run_id FROM trigger_deliveries WHERE id=$1`, deliveryID).Scan(&state, &runID); err != nil {
		t.Fatalf("read delivery error = %v", err)
	}
	return state, runID
}
