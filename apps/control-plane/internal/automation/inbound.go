package automation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/palgroup/palai/adapters/integrations/webhook"
	statemachines "github.com/palgroup/palai/packages/state-machines"
	"github.com/palgroup/palai/storage"
)

// The inbound-receiver rejection modes IngestInbound distinguishes, so the HTTP surface maps each without
// leaking a config oracle to an unauthenticated caller (AUT-002/010, brief §4):
//   - ErrInboundNotAvailable   → 404 generic (unresolvable / non-webhook / disabled / secret-less / no
//     revision). No existence disclosure for a source that failed to authenticate.
//   - ErrInboundUnauthenticated → 401 (bad/stale signature). Verified BEFORE any persistence; a sanitized
//     audit line is emitted (trigger id + reason, never payload/signature/secret bytes).
//   - ErrInboundMalformed       → 400 (signed but unusable envelope — the client authenticated).
//   - ErrInboundBackpressure    → 429 + Retry-After + queue depth/oldest-age.
var (
	ErrInboundNotAvailable    = errors.New("automation: inbound trigger is not available")
	ErrInboundUnauthenticated = errors.New("automation: inbound signature rejected")
	ErrInboundMalformed       = errors.New("automation: inbound event envelope is malformed")
)

// ErrInboundBackpressure is the AUT-010 admission-shed signal: the trigger's durable non-terminal inbound
// backlog is at its ceiling (or the in-flight semaphore is full). The handler renders 429 + Retry-After and
// reports Depth/OldestAge so a sender can back off. Other triggers keep flowing (the ceiling is per-trigger).
type ErrInboundBackpressure struct {
	Depth      int
	OldestAge  time.Duration
	RetryAfter time.Duration
}

func (e ErrInboundBackpressure) Error() string {
	return fmt.Sprintf("automation: inbound backpressure (depth=%d)", e.Depth)
}

// InboundResult is the outcome of ingesting a signed inbound event (the durable delivery + its inline
// continuation). State is the delivery's current/terminal state; the run coordinates name the born run;
// DuplicateOf links a redelivery to its canonical original.
type InboundResult struct {
	DeliveryID  string
	State       string
	ResponseID  string
	RunID       string
	SessionID   string
	DuplicateOf string
	Reason      string
}

// inboundTrigger is the global-by-id resolution of an inbound trigger (the unauthenticated route carries
// no tenant scope; the source signature is the auth).
type inboundTrigger struct {
	org, project, typ, createdBy, secretRef, secretRefNext string
	enabled                                                bool
}

// IngestInbound is the signed-inbound entry (spec §20.2.2/§21.7, §34.2-34.4). Ordered so verification runs
// strictly BEFORE any persistence and the 2xx is earned only after a durable row commits:
//
//	semaphore (bounded memory) → resolve trigger → resolve secrets → VERIFY+normalize → pin revision →
//	backlog gate (429) → durable INSERT (source-dedupe by the unique index) → inline map→admit.
//
// A crash after the INSERT leaves a durable, sweepable row the delivery-reconciler finishes exactly once,
// so a transient inline-continuation error still acks 2xx (the row is durable).
func (s *TriggerStore) IngestInbound(ctx context.Context, triggerID string, headers map[string]string, rawBody []byte) (InboundResult, error) {
	// In-flight semaphore: bounds concurrent-request memory under a flood (AUT-010). A full semaphore sheds
	// as backpressure rather than growing the heap.
	if s.inboundInflight != nil {
		select {
		case s.inboundInflight <- struct{}{}:
			defer func() { <-s.inboundInflight }()
		default:
			return InboundResult{}, ErrInboundBackpressure{RetryAfter: time.Second}
		}
	}

	tr, ok, err := s.resolveInboundTrigger(ctx, triggerID)
	if err != nil {
		return InboundResult{}, err
	}
	// A source that failed to authenticate learns nothing: every disqualifier is the SAME generic 404.
	if !ok || tr.typ != "webhook" || !tr.enabled || tr.secretRef == "" {
		return InboundResult{}, ErrInboundNotAvailable
	}
	secrets := s.inboundSecretsFor(tr.org, tr.secretRef, tr.secretRefNext)
	if len(secrets) == 0 {
		return InboundResult{}, ErrInboundNotAvailable // a set-but-unresolvable ref: no oracle, log server-side
	}

	// Verify + normalize BEFORE persistence (AUT-002). A bad/stale signature creates NOTHING.
	ev, err := webhook.ParseInbound(headers, rawBody, secrets, time.Now(), s.tolerance())
	switch {
	case errors.Is(err, webhook.ErrStaleTimestamp) || errors.Is(err, webhook.ErrBadSignature):
		s.auditInboundReject(triggerID, err)
		return InboundResult{}, ErrInboundUnauthenticated
	case errors.Is(err, webhook.ErrMalformedInbound):
		return InboundResult{}, ErrInboundMalformed
	case err != nil:
		return InboundResult{}, err
	}

	rev, ok, err := s.GetActiveRevision(ctx, tr.org, tr.project, triggerID)
	if err != nil {
		return InboundResult{}, err
	}
	if !ok {
		return InboundResult{}, ErrInboundNotAvailable // no run target ⇒ not receivable (no oracle)
	}

	// Backpressure: durable backlog ceiling (per-trigger). Checked AFTER auth so an unauthenticated flood
	// cannot probe the gauge, BEFORE persist so an over-threshold event is not admitted.
	if s.inboundBacklog > 0 {
		depth, oldest, err := s.inboundBacklogDepth(ctx, triggerID)
		if err != nil {
			return InboundResult{}, err
		}
		if depth >= s.inboundBacklog {
			return InboundResult{}, ErrInboundBackpressure{Depth: depth, OldestAge: oldest, RetryAfter: time.Second}
		}
	}

	// Durable insert. The source-dedupe UNIQUE partial index decides canonical-vs-duplicate race-free at the
	// DB (23505 → link a duplicate), the T2 ClaimCanonicalDelivery idiom on the source index.
	deliveryID := newID("tdel")
	_, err = s.pool.Exec(ctx, storage.Query("InsertInboundDelivery"),
		deliveryID, tr.org, tr.project, triggerID, rev.ID, tr.createdBy,
		ev.Source, ev.SourceTenant, ev.SourceEventID, rawBody)
	switch {
	case isUniqueViolation(err):
		return s.linkInboundDuplicate(ctx, tr, triggerID, rev.ID, ev, rawBody)
	case err != nil:
		return InboundResult{}, fmt.Errorf("insert inbound delivery: %w", err)
	}
	// The row (raw_payload + source cols + principal) is durable → the 2xx is earned.

	// Inline continuation (map→admit). DB-fast; the RUN is already async by architecture (§34.2). A
	// transient infra error leaves the durable row for the sweep — still a 2xx (at-least-once from the sender).
	sc := deliveryScope{org: tr.org, project: tr.project, principal: tr.createdBy, triggerID: triggerID,
		revisionID: rev.ID, deliveryID: deliveryID, sourceTenant: ev.SourceTenant}
	res, err := s.advanceInbound(ctx, sc, ev.Data)
	if err != nil {
		s.auditInboundReject(triggerID, err) // sanitized; the reconciler completes the durable row
		return InboundResult{DeliveryID: deliveryID, State: string(statemachines.TriggerDeliveryReceived)}, nil
	}
	return InboundResult{DeliveryID: deliveryID, State: res.State, ResponseID: res.ResponseID,
		RunID: res.RunID, SessionID: res.SessionID, DuplicateOf: res.DuplicateOf, Reason: res.Reason}, nil
}

// advanceInbound drives an inbound delivery from its CURRENT pre-map state (received → authenticated →
// deduplicated) into the shared map→admit pipeline, resuming safely from wherever the last run left it
// (the sweep re-drives from the durable raw envelope). Source-dedupe is already decided by the unique
// index at insert, so the deduplicated edge is bookkeeping, not a second claim. data is the event's opaque
// payload (ev.Data) — the mapping operates on it exactly as a manual/API delivery maps its posted body.
func (s *TriggerStore) advanceInbound(ctx context.Context, sc deliveryScope, data []byte) (DeliveryResult, error) {
	cfg, err := s.loadRevisionConfig(ctx, sc)
	if err != nil {
		return DeliveryResult{}, err
	}
	source, err := decodePayload(data)
	if err != nil {
		// An unmappable payload is poison: terminalize `failed` (the dead-letter view IS the row, §34.3).
		return s.fail(ctx, sc, "source payload is not a JSON object")
	}
	state, err := s.currentState(ctx, sc)
	if err != nil {
		return DeliveryResult{}, err
	}
	if state == statemachines.TriggerDeliveryReceived {
		if _, err := s.transition(ctx, sc, statemachines.TriggerDeliveryReceived, statemachines.TriggerDeliveryCmdAuthenticate); err != nil {
			return DeliveryResult{}, err
		}
		state = statemachines.TriggerDeliveryAuthenticated
	}
	if state == statemachines.TriggerDeliveryAuthenticated {
		if _, err := s.transition(ctx, sc, statemachines.TriggerDeliveryAuthenticated, statemachines.TriggerDeliveryCmdDeduplicate); err != nil {
			return DeliveryResult{}, err
		}
		state = statemachines.TriggerDeliveryDeduplicated
	}
	if state == statemachines.TriggerDeliveryDeduplicated {
		return s.mapAndAdmit(ctx, sc, cfg, source)
	}
	// Past deduplicated (mapped / admitted / terminal): another path owns it — recoverStuckMapped re-drives
	// `mapped`, and admitted/terminal are done. Report the current state without re-driving.
	return DeliveryResult{ID: sc.deliveryID, State: string(state)}, nil
}

// linkInboundDuplicate records a redelivered/duplicate source event linked to its canonical original — a
// 2xx with duplicate linkage and NO second run (AUT-001 inbound / AUT-009 redelivery). Ack-on-duplicate is
// honest because the original is a durable, sweepable row (a lost-ack retry finds it).
func (s *TriggerStore) linkInboundDuplicate(ctx context.Context, tr inboundTrigger, triggerID, revID string, ev webhook.InboundEvent, rawBody []byte) (InboundResult, error) {
	var original string
	if err := s.pool.QueryRow(ctx, storage.Query("FindCanonicalInboundDelivery"),
		triggerID, tr.org, tr.project, ev.Source, ev.SourceTenant, ev.SourceEventID).Scan(&original); err != nil {
		return InboundResult{}, fmt.Errorf("resolve canonical inbound original: %w", err)
	}
	dupID := newID("tdel")
	if _, err := s.pool.Exec(ctx, storage.Query("InsertInboundDuplicate"),
		dupID, tr.org, tr.project, triggerID, revID, tr.createdBy,
		ev.Source, ev.SourceTenant, ev.SourceEventID, rawBody, original, "duplicate of "+original); err != nil {
		return InboundResult{}, fmt.Errorf("insert inbound duplicate: %w", err)
	}
	return InboundResult{DeliveryID: dupID, State: string(statemachines.TriggerDeliveryDuplicate), DuplicateOf: original}, nil
}

// recoverStuckInbound is the delivery-reconciler's inbound fold (ONE localized Tick() call, so T6's
// callback sweep composes beside it). It (1) scrubs expired terminal raw payloads (short-retention
// behavior — encryption-at-rest is E13), then (2) re-drives ack'ed inbound deliveries stranded pre-map
// past the grace window from their DURABLE raw envelope, so a crash between the durable insert and the run
// finishes EXACTLY once (the T2 zombie ceiling closes for inbound — the payload persists here). A poison
// remnant (unparseable raw_payload) terminalizes `failed` instead of retrying forever. A bad remnant is
// logged + skipped, never returned — one row must not wedge the sweep behind a supervisor restart.
func (s *TriggerStore) recoverStuckInbound(ctx context.Context, grace time.Duration, limit int, rawTTL time.Duration, log func(string, ...any)) error {
	if rawTTL > 0 {
		if _, err := s.pool.Exec(ctx, storage.Query("ScrubInboundRawPayload"), rawTTL.Seconds()); err != nil {
			return fmt.Errorf("scrub inbound raw payloads: %w", err)
		}
	}
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, storage.Query("StuckInboundDeliveries"), grace.Seconds(), limit)
	if err != nil {
		return fmt.Errorf("scan stuck-inbound deliveries: %w", err)
	}
	type remnant struct {
		id, org, project, principal, triggerID, revisionID, sourceTenant, state string
		raw                                                                     []byte
	}
	var remnants []remnant
	for rows.Next() {
		var m remnant
		if err := rows.Scan(&m.id, &m.org, &m.project, &m.principal, &m.triggerID, &m.revisionID, &m.sourceTenant, &m.raw, &m.state); err != nil {
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
		sc := deliveryScope{org: m.org, project: m.project, principal: m.principal, triggerID: m.triggerID,
			revisionID: m.revisionID, deliveryID: m.id, sourceTenant: m.sourceTenant}
		var ev webhook.InboundEvent
		if err := json.Unmarshal(m.raw, &ev); err != nil {
			// Poison remnant: a durable row whose raw envelope can no longer be parsed. Terminalize `failed`
			// rather than re-driving it forever.
			if _, err := s.fail(ctx, sc, "inbound raw payload is unparseable"); err != nil {
				log("delivery-reconciler: fail poison inbound %s: %v", m.id, err)
			}
			continue
		}
		if _, err := s.advanceInbound(ctx, sc, ev.Data); err != nil {
			log("delivery-reconciler: recover stuck-inbound %s: %v", m.id, err)
			continue
		}
	}
	return nil
}

// resolveInboundTrigger resolves a trigger globally by its server-minted id (unauthenticated route).
func (s *TriggerStore) resolveInboundTrigger(ctx context.Context, triggerID string) (inboundTrigger, bool, error) {
	var tr inboundTrigger
	switch err := s.pool.QueryRow(ctx, storage.Query("ResolveInboundTrigger"), triggerID).
		Scan(&tr.org, &tr.project, &tr.enabled, &tr.typ, &tr.createdBy, &tr.secretRef, &tr.secretRefNext); {
	case errors.Is(err, pgx.ErrNoRows):
		return inboundTrigger{}, false, nil
	case err != nil:
		return inboundTrigger{}, false, fmt.Errorf("resolve inbound trigger: %w", err)
	}
	return tr, true, nil
}

// inboundSecretsFor redeems the trigger's 1-2 active source-secret handles to bytes via the resolver. A
// ref that fails to resolve is skipped (a rotation may reference a not-yet-provisioned secret); the caller
// treats an empty result as unavailable. Secret bytes never leave this call.
func (s *TriggerStore) inboundSecretsFor(org, ref, refNext string) [][]byte {
	if s.inboundSecrets == nil {
		return nil
	}
	var out [][]byte
	for _, r := range []string{ref, refNext} {
		if r == "" {
			continue
		}
		if b, err := s.inboundSecrets(org, r); err == nil && len(b) > 0 {
			out = append(out, b)
		}
	}
	return out
}

// inboundBacklogDepth reads a trigger's durable non-terminal inbound backlog COUNT + oldest-row age.
func (s *TriggerStore) inboundBacklogDepth(ctx context.Context, triggerID string) (int, time.Duration, error) {
	var depth int
	var oldestSecs int64
	if err := s.pool.QueryRow(ctx, storage.Query("InboundBacklogDepth"), triggerID).Scan(&depth, &oldestSecs); err != nil {
		return 0, 0, fmt.Errorf("read inbound backlog: %w", err)
	}
	return depth, time.Duration(oldestSecs) * time.Second, nil
}

// tolerance is the replay-window timestamp tolerance (default 5m, spec §21.5).
func (s *TriggerStore) tolerance() time.Duration {
	if s.inboundTolerance > 0 {
		return s.inboundTolerance
	}
	return 5 * time.Minute
}

// auditInboundReject emits the AUT-002 sanitized audit line: trigger id + a fixed typed reason, NEVER the
// payload, signature, or secret bytes (the webhook sentinel errors carry no such bytes). A named ceiling: a
// durable audit-log store is E13/E15 — this is a structured log line.
func (s *TriggerStore) auditInboundReject(triggerID string, reason error) {
	if s.inboundAudit == nil {
		return
	}
	s.inboundAudit("inbound rejected: trigger=%s reason=%s", triggerID, reason)
}
