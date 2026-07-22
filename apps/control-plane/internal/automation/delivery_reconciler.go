package automation

import (
	"context"
	"errors"
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
	rawTTL   time.Duration // inbound raw-payload short-retention TTL (0 ⇒ no scrub); WithInboundRawTTL
	log      func(string, ...any)
}

// NewDeliveryReconciler builds the reconciler over a wired trigger store. grace bounds how long a delivery
// may sit in `mapped`/pre-map before it is treated as a crash remnant; limit caps the stuck-remnant batch.
func NewDeliveryReconciler(store *TriggerStore, interval, grace time.Duration, limit int, log func(string, ...any)) *DeliveryReconciler {
	if log == nil {
		log = func(string, ...any) {}
	}
	return &DeliveryReconciler{store: store, interval: interval, grace: grace, limit: limit, log: log}
}

// WithInboundRawTTL sets how long a TERMINAL inbound delivery's raw_payload is retained before the sweep
// scrubs it (short-retention; 0 disables scrubbing). Returns the reconciler for chaining.
func (r *DeliveryReconciler) WithInboundRawTTL(ttl time.Duration) *DeliveryReconciler {
	r.rawTTL = ttl
	return r
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

// Tick runs one sweep: recover stuck-mapped remnants, re-drive/scrub ack'ed inbound remnants (the T5
// fold — one localized call so T6's callback sweep composes beside it), then admit gate-opened deferred
// FIFO heads.
func (r *DeliveryReconciler) Tick(ctx context.Context) error {
	if err := r.store.recoverStuckMapped(ctx, r.grace, r.limit, r.log); err != nil {
		return err
	}
	if err := r.store.recoverStuckInbound(ctx, r.grace, r.limit, r.rawTTL, r.log); err != nil {
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

	// M2: a poison group (its head's admission errors) is logged and SKIPPED, never returned — one bad
	// delivery must not wedge the whole sweep behind a supervisor restart loop.
	for _, g := range groups {
		if err := s.admitDeferredGroup(ctx, g.triggerID, g.org, g.project, g.hash); err != nil {
			log("delivery-reconciler: deferred group %s/%s: %v", g.triggerID, g.hash, err)
		}
	}
	return nil
}

// admitDeferredGroup admits (at most one) delivery from a single deferred (trigger, key) group: it checks
// the group's policy gate and, if open, admits the FIFO head (or, for coalesce, the latest survivor while
// skipping the rest). Errors are returned to the caller, which logs + skips the group.
func (s *TriggerStore) admitDeferredGroup(ctx context.Context, triggerID, org, project, hash string) error {
	sc := deliveryScope{org: org, project: project, triggerID: triggerID}
	// The FIFO head names the group's revision + policy (which gate + which survivor to admit).
	var headID, headPrincipal, headRevision string
	var headInput []byte
	if err := s.pool.QueryRow(ctx, storage.Query("OldestDeferredForKey"), triggerID, org, project, hash).
		Scan(&headID, &headPrincipal, &headRevision, &headInput); err != nil {
		return fmt.Errorf("resolve FIFO head: %w", err)
	}
	cfg, err := s.loadRevisionConfig(ctx, deliveryScope{org: org, project: project, revisionID: headRevision})
	if err != nil {
		return err
	}

	// The gate is trigger-wide for singleton, per-key otherwise; take it under the same advisory lock the
	// inline path uses so a reconciler admit never races an inline admit for the same key (M3).
	_, err = s.withGateLock(ctx, gateLockText(cfg.ConcurrencyPolicy, triggerID, hash), func(ctx context.Context) (DeliveryResult, error) {
		var busy bool
		if cfg.ConcurrencyPolicy == "singleton" {
			busy, err = s.triggerBusy(ctx, sc)
		} else {
			busy, err = s.keyBusy(ctx, sc, hash)
		}
		if err != nil {
			return DeliveryResult{}, err
		}
		if busy {
			return DeliveryResult{}, nil // gate still closed; leave the group deferred
		}

		// coalesce collapses a burst into the LATEST (the survivor); the rest are skipped, linked to it.
		admitID, admitPrincipal, admitRevision, admitInput := headID, headPrincipal, headRevision, headInput
		if cfg.ConcurrencyPolicy == "coalesce" {
			if err := s.pool.QueryRow(ctx, storage.Query("LatestDeferredForKey"), triggerID, org, project, hash).
				Scan(&admitID, &admitPrincipal, &admitRevision, &admitInput); err != nil {
				return DeliveryResult{}, fmt.Errorf("resolve coalesce survivor: %w", err)
			}
			if _, err := s.pool.Exec(ctx, storage.Query("SkipCoalescedDeferred"), triggerID, org, project, hash, admitID); err != nil {
				return DeliveryResult{}, fmt.Errorf("skip coalesced deferred: %w", err)
			}
		}
		return s.resumeDelivery(ctx, deliveryScope{
			org: org, project: project, principal: admitPrincipal, triggerID: triggerID, revisionID: admitRevision, deliveryID: admitID,
		}, hash, admitInput)
	})
	if errors.Is(err, errGateContended) {
		return nil // an inline admit holds the gate right now; retry this group next tick
	}
	return err
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

	// M2: a poison remnant is logged and SKIPPED, never returned — one bad row must not wedge the sweep.
	for _, m := range remnants {
		sc := deliveryScope{org: m.org, project: m.project, principal: m.principal, triggerID: m.triggerID, revisionID: m.revisionID, deliveryID: m.id}
		cfg, err := s.loadRevisionConfig(ctx, sc)
		if err != nil {
			log("delivery-reconciler: recover stuck-mapped %s: %v", m.id, err)
			continue
		}
		// Re-run the concurrency decision from the stored state (nil source: a stuck-mapped remnant is
		// never named_session — that mode never leaves a delivery in `mapped`).
		if _, err := s.applyPolicy(ctx, sc, cfg, nil, m.mappedInput, m.hash); err != nil {
			log("delivery-reconciler: recover stuck-mapped %s: %v", m.id, err)
			continue
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
