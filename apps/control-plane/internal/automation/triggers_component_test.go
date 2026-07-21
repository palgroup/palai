//go:build component

// Real-PostgreSQL component tests for the trigger management surface + delivery pipeline (spec §20.2.2,
// E11 Task 2). They run under `make test-component TEST=postgres`, which starts a throwaway container and
// exports PALAI_COMPONENT_POSTGRES_URL. Honest ceiling: pure infra + the durable admission seam — the
// live model bind is the CASE=trigger-dedupe-run smoke.
package automation

import (
	"context"
	"testing"
)

// seedTrigger creates a trigger with one initial revision and returns (triggerID, revisionID). The
// caller supplies the revision input (mapping/policy/pin) it needs.
func seedTrigger(t *testing.T, s *TriggerStore, org, project, name string, in TriggerRevisionInput) (string, string) {
	t.Helper()
	ctx := context.Background()
	triggerID, err := s.CreateTrigger(ctx, org, project, name, "manual_api")
	if err != nil {
		t.Fatalf("CreateTrigger error = %v", err)
	}
	rev, err := s.ReviseTrigger(ctx, org, project, triggerID, in)
	if err != nil {
		t.Fatalf("ReviseTrigger error = %v", err)
	}
	return triggerID, rev.ID
}

// TestAcceptedDeliveryPinsExactRevision pins AGT-002: a delivery pins the trigger's ACTIVE revision at
// accept, and a later revise (a NEW immutable INSERT, revision N+1) never moves the already-pinned
// delivery. The revision that processes the delivery is the one active at accept, not the latest.
func TestAcceptedDeliveryPinsExactRevision(t *testing.T) {
	pool := componentPool(t)
	store := NewTriggerStore(pool)
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)

	triggerID, rev1 := seedTrigger(t, store, org, project, "nightly", TriggerRevisionInput{})

	// The active revision is rev1; a delivery pins it.
	del, err := store.CreateDelivery(ctx, org, project, triggerID, []byte(`{"order":{"id":"o1"}}`))
	if err != nil {
		t.Fatalf("CreateDelivery error = %v", err)
	}

	// A revise creates a NEW immutable revision (N+1) — not an in-place UPDATE of rev1's config.
	rev2, err := store.ReviseTrigger(ctx, org, project, triggerID, TriggerRevisionInput{})
	if err != nil {
		t.Fatalf("ReviseTrigger error = %v", err)
	}
	if rev2.ID == rev1 || rev2.RevisionNumber != 2 {
		t.Fatalf("revise produced revision %+v, want a new id at number 2", rev2)
	}
	active, _, err := store.GetActiveRevision(ctx, org, project, triggerID)
	if err != nil {
		t.Fatalf("GetActiveRevision error = %v", err)
	}
	if active.ID != rev2.ID {
		t.Fatalf("active revision = %s, want the newest %s", active.ID, rev2.ID)
	}

	// The delivery is still pinned to rev1 — the revise did not move it (immutable pin).
	var pinned string
	if err := pool.QueryRow(ctx,
		`SELECT trigger_revision_id FROM trigger_deliveries WHERE id = $1`, del.ID).Scan(&pinned); err != nil {
		t.Fatalf("read pinned revision error = %v", err)
	}
	if pinned != rev1 {
		t.Fatalf("delivery pinned to revision %s, want the accept-time active %s", pinned, rev1)
	}

	// rev1's config columns were never rewritten: it still exists at number 1 alongside the new rev2.
	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM trigger_revisions WHERE trigger_id = $1`, triggerID).Scan(&count); err != nil {
		t.Fatalf("count revisions error = %v", err)
	}
	if count != 2 {
		t.Fatalf("trigger has %d revisions, want 2 (revise is an INSERT, not an UPDATE)", count)
	}
}
