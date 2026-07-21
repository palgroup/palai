package automation

import (
	"context"
	"fmt"
	"time"

	"github.com/palgroup/palai/storage"
)

// DeliveryReconciler is the supervised sweep that advances deferred trigger deliveries (spec §20.2.2,
// AUT-004). It admits the FIFO head of each (trigger, correlation-key) group once the group's gate opens
// (the currently-running delivery reached a terminal run), and it re-decides deliveries stranded in
// `mapped` past a grace window — crash remnants that reached mapping but never took the concurrency
// decision. It runs under one supervised loop named "delivery-reconciler" (T5 folds inbound-source
// sweeps into the same loop). No new framework — the coordinator reconciler's ticker shape.
type DeliveryReconciler struct {
	store    *TriggerStore
	interval time.Duration
	grace    time.Duration
	limit    int
	log      func(string, ...any)
}

// NewDeliveryReconciler builds the reconciler over a wired trigger store. grace bounds how long a delivery
// may sit in `mapped` before it is treated as a crash remnant; limit caps the stuck-remnant batch.
func NewDeliveryReconciler(store *TriggerStore, interval, grace time.Duration, limit int, log func(string, ...any)) *DeliveryReconciler {
	if log == nil {
		log = func(string, ...any) {}
	}
	return &DeliveryReconciler{store: store, interval: interval, grace: grace, limit: limit, log: log}
}

// Run drives the reconciler on its interval until ctx is cancelled (the webhook-pump loop shape). A
// transient tick error is returned so the supervisor restarts the loop; the durable rows are the source
// of truth, so a missed tick just resumes next pass.
func (r *DeliveryReconciler) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		if err := r.Tick(ctx); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Tick runs one sweep: recover stuck-mapped remnants, then admit gate-opened deferred FIFO heads.
func (r *DeliveryReconciler) Tick(ctx context.Context) error {
	if err := r.store.recoverStuckMapped(ctx, r.grace, r.limit, r.log); err != nil {
		return err
	}
	return r.store.reconcileDeferred(ctx, r.log)
}

// reconcileDeferred admits the FIFO head of each deferred (trigger, key) group whose gate is open (no
// active run for the key). It advances at most one delivery per group per pass, so the per-key ordering
// is strict: the head admits, and the next head is not admitted until this run terminates.
func (s *TriggerStore) reconcileDeferred(ctx context.Context, log func(string, ...any)) error {
	rows, err := s.pool.Query(ctx, storage.Query("DeferredDeliveryGroups"))
	if err != nil {
		return fmt.Errorf("scan deferred groups: %w", err)
	}
	type group struct{ triggerID, org, project, hash string }
	var groups []group
	for rows.Next() {
		var g group
		if err := rows.Scan(&g.triggerID, &g.org, &g.project, &g.hash); err != nil {
			rows.Close()
			return err
		}
		groups = append(groups, group{g.triggerID, g.org, g.project, g.hash})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, g := range groups {
		sc := deliveryScope{org: g.org, project: g.project, triggerID: g.triggerID}
		busy, err := s.keyBusy(ctx, sc, g.hash)
		if err != nil {
			return err
		}
		if busy {
			continue // the gate is still closed; leave the FIFO head deferred
		}
		var id, principal, revisionID string
		var mappedInput []byte
		if err := s.pool.QueryRow(ctx, storage.Query("OldestDeferredForKey"), g.triggerID, g.org, g.project, g.hash).
			Scan(&id, &principal, &revisionID, &mappedInput); err != nil {
			return fmt.Errorf("resolve FIFO head: %w", err)
		}
		if _, err := s.resumeDelivery(ctx, deliveryScope{
			org: g.org, project: g.project, principal: principal, triggerID: g.triggerID, revisionID: revisionID, deliveryID: id,
		}, g.hash, mappedInput); err != nil {
			log("delivery-reconciler: admit deferred %s: %v", id, err)
			return err
		}
	}
	return nil
}

// recoverStuckMapped re-decides deliveries stranded in `mapped` past the grace window — a crash between
// mapping and the concurrency decision. Their mapped_input + correlation hash are stored, so the decision
// re-runs from the durable row (no source payload needed).
func (s *TriggerStore) recoverStuckMapped(ctx context.Context, grace time.Duration, limit int, log func(string, ...any)) error {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, storage.Query("StuckMappedDeliveries"), grace.Seconds(), limit)
	if err != nil {
		return fmt.Errorf("scan stuck-mapped deliveries: %w", err)
	}
	type remnant struct {
		id, org, project, principal, triggerID, revisionID, hash string
		mappedInput                                              []byte
	}
	var remnants []remnant
	for rows.Next() {
		var m remnant
		if err := rows.Scan(&m.id, &m.org, &m.project, &m.principal, &m.triggerID, &m.revisionID, &m.hash, &m.mappedInput); err != nil {
			rows.Close()
			return err
		}
		remnants = append(remnants, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, m := range remnants {
		sc := deliveryScope{org: m.org, project: m.project, principal: m.principal, triggerID: m.triggerID, revisionID: m.revisionID, deliveryID: m.id}
		cfg, err := s.loadRevisionConfig(ctx, sc)
		if err != nil {
			return err
		}
		// Re-run the concurrency decision from the stored state (nil source: a stuck-mapped remnant is
		// never named_session — that mode never leaves a delivery in `mapped`).
		if _, err := s.applyPolicy(ctx, sc, cfg, nil, m.mappedInput, m.hash); err != nil {
			log("delivery-reconciler: recover stuck-mapped %s: %v", m.id, err)
			return err
		}
	}
	return nil
}

// resumeDelivery admits a deferred delivery from its stored state (its FIFO gate is open). A queued
// delivery serializes into a fresh session (the correlation mode drives the exact target); the stored
// mapped_input + hash are authoritative, so no source payload is needed.
func (s *TriggerStore) resumeDelivery(ctx context.Context, sc deliveryScope, hash string, mappedInput []byte) (DeliveryResult, error) {
	cfg, err := s.loadRevisionConfig(ctx, sc)
	if err != nil {
		return DeliveryResult{}, err
	}
	return s.correlateAdmit(ctx, sc, cfg, nil, mappedInput, hash)
}
