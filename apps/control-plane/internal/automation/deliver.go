package automation

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/storage"
)

// RunAdmitter is the seam the delivery pipeline admits a run through — the SAME §20.9 admission path a
// POST /v1/responses takes (spec §20.2.2). The coordinator spine implements it; a triggered run is born
// identically (a queued run + run.queued.v1 birth event + a dispatch job), so a delivery never gets a
// second, divergent run-creation path. It is defined here (not imported from api) so the automation
// package stays below api in the import graph.
type RunAdmitter interface {
	AdmitResponse(ctx context.Context, tenant coordinator.Tenant, in coordinator.AdmissionInput) (coordinator.Admission, error)
}

// DeliveryResult is the outcome of accepting + processing a delivery. State is the terminal (or current)
// TriggerDelivery state; ResponseID/RunID/SessionID name the born run once the delivery reaches
// run_created; DuplicateOf links a duplicate to its canonical original; Reason carries a skip/reject/fail
// explanation.
type DeliveryResult struct {
	ID          string
	State       string
	ResponseID  string
	RunID       string
	SessionID   string
	DuplicateOf string
	Reason      string
}

// CreateDelivery accepts a manual/API delivery for a trigger and drives it through the ingestion
// pipeline. It PINS the trigger's active revision at accept (AGT-002 — a later revise does not move a
// pinned delivery), then advances the delivery through authenticate → dedupe → map → admit → run_created
// (or a rejected/duplicate/failed/deferred/skipped branch). A disabled or unknown trigger is a typed
// error; a trigger with no revision cannot accept a delivery.
func (s *TriggerStore) CreateDelivery(ctx context.Context, org, project, triggerID string, payload []byte) (DeliveryResult, error) {
	enabled, err := s.triggerEnabled(ctx, org, project, triggerID)
	if err != nil {
		return DeliveryResult{}, err
	}
	if !enabled {
		return DeliveryResult{}, ErrTriggerDisabled
	}
	rev, ok, err := s.GetActiveRevision(ctx, org, project, triggerID)
	if err != nil {
		return DeliveryResult{}, err
	}
	if !ok {
		return DeliveryResult{}, ErrNoActiveRevision
	}

	deliveryID := newID("tdel")
	if _, err := s.pool.Exec(ctx, storage.Query("InsertDelivery"), deliveryID, org, project, triggerID, rev.ID); err != nil {
		return DeliveryResult{}, fmt.Errorf("insert delivery: %w", err)
	}

	scope := deliveryScope{org: org, project: project, triggerID: triggerID, revisionID: rev.ID, deliveryID: deliveryID}
	return s.advance(ctx, scope, payload)
}

// deliveryScope carries the tenant + pinned-revision coordinates a delivery advances within.
type deliveryScope struct {
	org, project, triggerID, revisionID, deliveryID string
}

// advance drives a received delivery through the pipeline. It is grown stage by stage across the E11 T2
// slice: A4 accepts + pins (the delivery is born received); A5 adds dedupe; A6 map + admission; A7
// correlation; A8/A9 concurrency policy. Each stage persists the SM transition + journals its
// trigger.delivery.* event.
func (s *TriggerStore) advance(ctx context.Context, sc deliveryScope, payload []byte) (DeliveryResult, error) {
	// A4: the delivery is accepted + pinned; later stages advance it. Read back the pinned state so the
	// caller sees the durable row.
	state, err := s.deliveryState(ctx, sc)
	if err != nil {
		return DeliveryResult{}, err
	}
	return DeliveryResult{ID: sc.deliveryID, State: state}, nil
}

// deliveryState reads a delivery's current state within scope.
func (s *TriggerStore) deliveryState(ctx context.Context, sc deliveryScope) (string, error) {
	var revisionID, state string
	switch err := s.pool.QueryRow(ctx, storage.Query("GetDeliveryPin"), sc.deliveryID, sc.org, sc.project).
		Scan(&revisionID, &state); {
	case errors.Is(err, pgx.ErrNoRows):
		return "", fmt.Errorf("delivery %s vanished after accept", sc.deliveryID)
	case err != nil:
		return "", fmt.Errorf("read delivery state: %w", err)
	}
	return state, nil
}
