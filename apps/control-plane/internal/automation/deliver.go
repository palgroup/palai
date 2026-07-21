package automation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	statemachines "github.com/palgroup/palai/packages/state-machines"
	"github.com/palgroup/palai/storage"
)

// createRoute is the admission route a triggered run is born on — the SAME route a POST /v1/responses
// takes, so a triggered run and an API-posted run share one admission path (spec §20.9, §20.2.2).
const createRoute = "/v1/responses"

// RunAdmitter is the seam the delivery pipeline admits a run through — the SAME §20.9 admission path a
// POST /v1/responses takes (spec §20.2.2). The coordinator spine implements it; a triggered run is born
// identically (a queued run + run.queued.v1 birth event + a dispatch job), so a delivery never gets a
// second, divergent run-creation path. It is defined here (not imported from api) so the automation
// package stays below api in the import graph.
type RunAdmitter interface {
	AdmitResponse(ctx context.Context, tenant coordinator.Tenant, in coordinator.AdmissionInput) (coordinator.Admission, error)
	// AcceptCommand is the send_message accept path a named_session delivery appends through (spec
	// §20.2.2 correlation named_session — no new command kind, no new run). The coordinator implements it.
	AcceptCommand(ctx context.Context, tenant coordinator.Tenant, sessionID string, in coordinator.CommandInput) (coordinator.Command, error)
	// CancelRunReconciled cancels an active run to a single monotonic terminal — the `replace` policy
	// cancels the running delivery before admitting the new one. The coordinator implements it.
	CancelRunReconciled(ctx context.Context, tenant coordinator.Tenant, responseID, runID string, canceledProjection, uncertainProjection []byte) (string, error)
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
func (s *TriggerStore) CreateDelivery(ctx context.Context, org, project, principal, triggerID string, payload []byte) (DeliveryResult, error) {
	return s.createDelivery(ctx, org, project, principal, triggerID, payload, "")
}

// CreateScheduledDelivery is the schedule ticker's admission handoff (E11 Task 3): it fires the schedule's
// trigger through the SAME §20.2.2 delivery pipeline a manual/API POST takes — NO second admission path —
// but FORCES the delivery's dedupe_key to the deterministic occurrence_id. That makes T2's canonical
// UNIQUE(trigger_id, dedupe_key) collapse any double handoff (a crash-retry, a jitter re-sweep, or two
// ticker replicas) to ONE canonical delivery with ONE run — the third exactly-once defense line behind the
// occurrence unique index and durable-before-run (§5). The occurrence's own trigger-configured
// dedupe_key_expr is intentionally overridden: a scheduled firing dedupes on its occurrence identity, not
// on payload content.
func (s *TriggerStore) CreateScheduledDelivery(ctx context.Context, org, project, principal, triggerID, occurrenceID string, payload []byte) (DeliveryResult, error) {
	return s.createDelivery(ctx, org, project, principal, triggerID, payload, occurrenceID)
}

// createDelivery accepts a delivery for a trigger and drives it through the pipeline. dedupeOverride, when
// non-empty, forces the canonical dedupe_key (the scheduled-firing occurrence_id); "" leaves the trigger's
// configured dedupe_key_expr to decide (the manual/API path).
func (s *TriggerStore) createDelivery(ctx context.Context, org, project, principal, triggerID string, payload []byte, dedupeOverride string) (DeliveryResult, error) {
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
	if _, err := s.pool.Exec(ctx, storage.Query("InsertTriggerDelivery"), deliveryID, org, project, triggerID, rev.ID, principal); err != nil {
		return DeliveryResult{}, fmt.Errorf("insert delivery: %w", err)
	}

	scope := deliveryScope{org: org, project: project, principal: principal, triggerID: triggerID, revisionID: rev.ID, deliveryID: deliveryID, dedupeOverride: dedupeOverride}
	return s.advance(ctx, scope, payload)
}

// deliveryScope carries the tenant + pinned-revision coordinates a delivery advances within. dedupeOverride
// forces the canonical dedupe_key for a scheduled firing (the occurrence_id); "" for the manual/API path.
type deliveryScope struct {
	org, project, principal, triggerID, revisionID, deliveryID string
	dedupeOverride                                             string
}

// revisionConfig is a pinned revision's delivery-pipeline config (mapping + key exprs + modes).
type revisionConfig struct {
	AgentRevisionID       string
	RunTemplateRevisionID string
	InputMapping          []byte
	DedupeKeyExpr         string
	CorrelationMode       string
	CorrelationKeyExpr    string
	ConcurrencyPolicy     string
}

// advance drives a received delivery through the pipeline: authenticate → dedupe → map → concurrency
// policy → correlate → admit (A4-A9). The single-column stage moves (authenticate/deduplicate/map/defer/
// reject/fail/skip) go through the TriggerDelivery state machine (statemachines.Apply validates them); a
// FEW atomic MULTI-column writes — ClaimCanonicalDelivery (dedupe_key + state in one ON CONFLICT UPDATE),
// MarkDeliveryDuplicate (state + duplicate_of), RecordDeliveryAdmitted (response/run/session + state) —
// set their resulting state DIRECTLY, because splitting the atomic write to route through Apply would
// reopen a race (m5). The SM edge each of those lands on still exists in the table.
//
// DESIGN — the trigger_deliveries.state column IS the durable record (queryable via GET
// /v1/trigger-deliveries/{id}). The trigger.delivery.* events are registered in the contract for
// downstream consumers; a delivery has NO session before admission (events are session-scoped, NOT NULL),
// so pre-admission transitions persist the state column only and the run-born events ride the run's
// session once it exists (A6). This mirrors the agents.go precedent (the durable fact is the row; the
// event is declared-but-unemitted here).
//
// ponytail (m9) — a delivery that crashes BEFORE map (received/authenticated/deduplicated) is a ZOMBIE:
// its source payload is not persisted in T2, so the reconciler (which sweeps only mapped + deferred) can
// never re-decide it and it stays non-terminal forever. Closing this needs the durable raw_payload the
// T5 signed-inbound receiver owns (the column is pre-provisioned in 000021); until then a manual/API
// delivery that crashes in the first three states is an accepted, named ceiling — not a silent gap.
func (s *TriggerStore) advance(ctx context.Context, sc deliveryScope, payload []byte) (DeliveryResult, error) {
	cfg, err := s.loadRevisionConfig(ctx, sc)
	if err != nil {
		return DeliveryResult{}, err
	}
	source, err := decodePayload(payload)
	if err != nil {
		// A malformed source payload cannot be mapped; reject the delivery (no run).
		return s.reject(ctx, sc, "source payload is not a JSON object")
	}

	// Authenticate. A manual/API delivery is authenticated by the verified API scope that reached this
	// method (the HTTP surface already authorized it); a signed inbound source is T5. So this transition
	// is trivially satisfied here, but it is a real SM edge so the pipeline shape matches the spec.
	if _, err := s.transition(ctx, sc, statemachines.TriggerDeliveryReceived, statemachines.TriggerDeliveryCmdAuthenticate); err != nil {
		return DeliveryResult{}, err
	}

	// Dedupe (AUT-001): compute the dedupe key and try to become the LIVE canonical row. A loser links to
	// the canonical original and terminalizes `duplicate`, so a redelivered source event yields no second
	// action. An empty key means dedupe is not configured — the delivery passes straight to deduplicated.
	// A scheduled firing FORCES the key to its occurrence_id (dedupeOverride), so a double handoff of the
	// same occurrence collapses to one canonical delivery (E11 Task 3, §5 third defense line).
	dedupeKey := sc.dedupeOverride
	if dedupeKey == "" {
		dedupeKey, err = boundedKey(cfg.DedupeKeyExpr, source)
		if err != nil {
			return DeliveryResult{}, err
		}
	}
	if dedupeKey != "" {
		switch err := s.pool.QueryRow(ctx, storage.Query("ClaimCanonicalDelivery"),
			sc.deliveryID, sc.org, sc.project, dedupeKey).Scan(new(string)); {
		case isUniqueViolation(err):
			return s.markDuplicate(ctx, sc, dedupeKey)
		case err != nil:
			return DeliveryResult{}, fmt.Errorf("claim canonical delivery: %w", err)
		}
	} else if _, err := s.transition(ctx, sc, statemachines.TriggerDeliveryAuthenticated, statemachines.TriggerDeliveryCmdDeduplicate); err != nil {
		return DeliveryResult{}, err
	}

	// Map (AUT-003): compile + evaluate the mapping into the canonical action input. A schema-invalid
	// mapping FAILS the delivery WITHOUT a run — no billable run is ever born from an unmappable event.
	mapping, err := CompileMapping(cfg.InputMapping, nil)
	if err != nil {
		return s.fail(ctx, sc, "input mapping is invalid: "+err.Error())
	}
	mappedInput, err := mapping.Apply(source)
	if err != nil {
		return s.fail(ctx, sc, "input mapping failed: "+err.Error())
	}
	// Validate the deduplicated → mapped transition, then persist the mapped input + correlation hash
	// TOGETHER (a delivery that crashes after this is a recoverable remnant the reconciler re-decides
	// from the stored state, without the now-gone source payload).
	if _, _, err := statemachines.Apply(statemachines.TriggerDeliveryDeduplicated, statemachines.TriggerDeliveryCmdMap, statemachines.TriggerDeliveryTable); err != nil {
		return DeliveryResult{}, err
	}
	hash := computeCorrelationHash(sc, cfg.CorrelationKeyExpr, source)
	if _, err := s.pool.Exec(ctx, storage.Query("RecordDeliveryMapped"), sc.deliveryID, sc.org, sc.project, mappedInput, hash); err != nil {
		return DeliveryResult{}, fmt.Errorf("record mapped delivery: %w", err)
	}

	// Apply the concurrency policy at the admit gate, then correlate + admit.
	return s.applyPolicy(ctx, sc, cfg, source, mappedInput, hash)
}

// correlateAdmit resolves the target session per the revision's correlation mode and runs the shared
// admit / append / reject action, using the pre-computed correlation hash (recorded at map). A
// correlation query touches only THIS tenant's deliveries, so it can never reach a foreign session (authz
// is not bypassed), and admission still enforces retention/scope on the resolved session. source may be
// nil on a reconciler resume — only named_session needs it, and named_session can never defer (rejected at
// revise by ErrNamedSessionCannotDefer), so a resumed (deferred) delivery is never named_session (M4).
func (s *TriggerStore) correlateAdmit(ctx context.Context, sc deliveryScope, cfg revisionConfig, source map[string]any, mappedInput []byte, hash string) (DeliveryResult, error) {
	switch cfg.CorrelationMode {
	case "named_session":
		key, err := boundedKey(cfg.CorrelationKeyExpr, source)
		if err != nil {
			return DeliveryResult{}, err
		}
		return s.appendToNamedSession(ctx, sc, key, mappedInput)
	case "bounded_key_reuse", "reject_if_active":
		var prior string
		var err error
		if hash != "" {
			if prior, err = s.findCorrelatedSession(ctx, sc, hash); err != nil {
				return DeliveryResult{}, err
			}
		}
		if cfg.CorrelationMode == "reject_if_active" && prior != "" {
			active, err := s.hasActiveRootRun(ctx, sc, prior)
			if err != nil {
				return DeliveryResult{}, err
			}
			if active {
				return s.reject(ctx, sc, "correlated session has an active root run")
			}
		}
		var requested *string
		if prior != "" {
			requested = &prior
		}
		return s.admitChained(ctx, sc, cfg, mappedInput, requested)
	default: // per_event
		return s.admit(ctx, sc, cfg, mappedInput)
	}
}

// computeCorrelationHash is the pure bounded correlation-key hash — SHA-256 over the (project,
// trigger_revision, source_tenant, raw-key) tuple, so only the hash is ever stored. An empty key expr (or
// an empty evaluated key) yields "" (no correlation grouping). The expr was compiled at revise time, so a
// compile error here is impossible — it is swallowed to "" rather than propagated.
func computeCorrelationHash(sc deliveryScope, expr string, source map[string]any) string {
	compiled, err := CompileExpr(expr, nil)
	if err != nil {
		return ""
	}
	raw, err := compiled.EvalString(source)
	if err != nil || raw == "" {
		return ""
	}
	// source_tenant is '' in T2 (manual/api carries no signed source envelope — T5).
	scoped := sc.project + "\x00" + sc.revisionID + "\x00" + "" + "\x00" + raw
	sum := sha256.Sum256([]byte(scoped))
	return hex.EncodeToString(sum[:])
}

// findCorrelatedSession resolves the session a prior delivery with the same correlation hash landed in.
func (s *TriggerStore) findCorrelatedSession(ctx context.Context, sc deliveryScope, hash string) (string, error) {
	var session string
	switch err := s.pool.QueryRow(ctx, storage.Query("FindCorrelatedSession"),
		sc.triggerID, sc.org, sc.project, hash, sc.deliveryID).Scan(&session); {
	case errors.Is(err, pgx.ErrNoRows):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("resolve correlated session: %w", err)
	}
	return session, nil
}

// hasActiveRootRun reports whether a session holds a non-terminal root run (the reject_if_active gate).
func (s *TriggerStore) hasActiveRootRun(ctx context.Context, sc deliveryScope, sessionID string) (bool, error) {
	switch err := s.pool.QueryRow(ctx, storage.Query("ActiveRootRun"), sessionID, sc.org, sc.project).Scan(new(string), new(string)); {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("check active root run: %w", err)
	}
	return true, nil
}

// appendToNamedSession appends the mapped input to an EXISTING named session's active root run via the
// send_message accept path (no new command kind, no new run — spec §20.2.2 named_session). A missing
// session or one with no live run fails the delivery (there is no loop to receive the message).
func (s *TriggerStore) appendToNamedSession(ctx context.Context, sc deliveryScope, sessionID string, mappedInput []byte) (DeliveryResult, error) {
	if s.admitter == nil {
		return DeliveryResult{}, errors.New("automation: delivery admitter is not wired")
	}
	if sessionID == "" {
		return s.fail(ctx, sc, "named_session correlation produced no session id")
	}
	tenant := coordinator.Tenant{Organization: sc.org, Project: sc.project}
	// Resolve the target run BEFORE the send, so the delivery records the run it joined.
	var runID, responseID string
	switch err := s.pool.QueryRow(ctx, storage.Query("ActiveRootRun"), sessionID, sc.org, sc.project).Scan(&runID, &responseID); {
	case errors.Is(err, pgx.ErrNoRows):
		return s.fail(ctx, sc, "named session has no active run to receive the message")
	case err != nil:
		return DeliveryResult{}, fmt.Errorf("resolve named session run: %w", err)
	}
	payload, err := json.Marshal(map[string]any{"message": string(mappedInput)})
	if err != nil {
		return DeliveryResult{}, err
	}
	cmd, err := s.admitter.AcceptCommand(ctx, tenant, sessionID, coordinator.CommandInput{
		CommandID: "trigger-delivery:" + sc.deliveryID,
		Kind:      "send_message",
		Delivery:  "queue",
		Payload:   payload,
	})
	if err != nil {
		return DeliveryResult{}, fmt.Errorf("append to named session: %w", err)
	}
	if cmd.SessionNotFound {
		return s.fail(ctx, sc, "named session not found in scope")
	}
	if cmd.State == "rejected" {
		return s.fail(ctx, sc, "named session rejected the message (no live run)")
	}
	if _, err := s.pool.Exec(ctx, storage.Query("RecordDeliveryAdmitted"),
		sc.deliveryID, sc.org, sc.project, responseID, runID, sessionID, mappedInput); err != nil {
		return DeliveryResult{}, fmt.Errorf("record named-session delivery: %w", err)
	}
	if _, err := s.transition(ctx, sc, statemachines.TriggerDeliveryAdmitted, statemachines.TriggerDeliveryCmdCreateRun); err != nil {
		return DeliveryResult{}, err
	}
	return DeliveryResult{
		ID: sc.deliveryID, State: string(statemachines.TriggerDeliveryRunCreated),
		ResponseID: responseID, RunID: runID, SessionID: sessionID,
	}, nil
}

// applyPolicy runs the revision's concurrency policy at the admit gate (spec §20.2.2, AUT-004/005). It
// decides — using the pre-computed correlation hash as the grouping key — whether to admit now, defer for
// the reconciler, skip, reject, or (A9) replace/coalesce. `allow` admits immediately through the
// correlation mode; `queue` serializes per key (a busy key defers, the reconciler admits the FIFO head
// when the gate opens). The remaining policies land in A9.
func (s *TriggerStore) applyPolicy(ctx context.Context, sc deliveryScope, cfg revisionConfig, source map[string]any, mappedInput []byte, hash string) (DeliveryResult, error) {
	// `allow` has no gate — the DB one-active-root constraint serializes any bounded_key_reuse chain — so
	// it admits without the lock (over-locking allow would needlessly serialize independent runs). Every
	// other policy takes the gate under a per-scope advisory lock (M3): the gate-check + admit run inside
	// it, so two concurrent same-key deliveries serialize — one admits, the rest see the active run and
	// defer/drop per policy.
	if cfg.ConcurrencyPolicy == "" || cfg.ConcurrencyPolicy == "allow" {
		return s.correlateAdmit(ctx, sc, cfg, source, mappedInput, hash)
	}
	res, err := s.withGateLock(ctx, gateLockText(cfg.ConcurrencyPolicy, sc.triggerID, hash), func(ctx context.Context) (DeliveryResult, error) {
		return s.applyGatedPolicy(ctx, sc, cfg, source, mappedInput, hash)
	})
	if errors.Is(err, errGateContended) {
		// Another same-key delivery holds the gate mid-admit — treat contention as the gate being busy:
		// drop_if_running skips (the concurrent winner runs); the rest defer (the reconciler admits FIFO).
		// ponytail: a concurrent same-key `replace` burst defers the loser instead of canceling the
		// winner's run — the reconciler admits it after that run terminates. Rare (two replaces racing one
		// key); the common sequential-replace path cancels correctly. Tighten with a per-key serial queue
		// if concurrent replace ever matters.
		if cfg.ConcurrencyPolicy == "drop_if_running" {
			return s.skip_(ctx, sc, "", "dropped: key gate contended")
		}
		return s.defer_(ctx, sc, "key gate contended; queued")
	}
	return res, err
}

// applyGatedPolicy is the gate-check + admit decision for a non-allow policy, run under the gate lock.
func (s *TriggerStore) applyGatedPolicy(ctx context.Context, sc deliveryScope, cfg revisionConfig, source map[string]any, mappedInput []byte, hash string) (DeliveryResult, error) {
	switch cfg.ConcurrencyPolicy {
	case "queue":
		busy, err := s.keyBusy(ctx, sc, hash)
		if err != nil {
			return DeliveryResult{}, err
		}
		if busy {
			return s.defer_(ctx, sc, "key has an active run; queued FIFO")
		}
		return s.correlateAdmit(ctx, sc, cfg, source, mappedInput, hash)
	case "drop_if_running":
		// A busy key SKIPs the new event (nothing was wrong — a policy skip, the AUT-005 honest-naming
		// terminal, not a rejection). A free key admits.
		busy, err := s.keyBusy(ctx, sc, hash)
		if err != nil {
			return DeliveryResult{}, err
		}
		if busy {
			return s.skip_(ctx, sc, "", "dropped: key has an active run")
		}
		return s.correlateAdmit(ctx, sc, cfg, source, mappedInput, hash)
	case "singleton":
		// Trigger-wide single active: any active run on the trigger defers the new event (the reconciler
		// admits it once the trigger is free). The gate is trigger-wide, not per-key.
		busy, err := s.triggerBusy(ctx, sc)
		if err != nil {
			return DeliveryResult{}, err
		}
		if busy {
			return s.defer_(ctx, sc, "trigger has an active run; singleton")
		}
		return s.correlateAdmit(ctx, sc, cfg, source, mappedInput, hash)
	case "coalesce":
		// A busy key defers; the reconciler collapses a burst of deferred events into ONE survivor (the
		// latest — the deterministic reducer) and skips the rest linked to it.
		busy, err := s.keyBusy(ctx, sc, hash)
		if err != nil {
			return DeliveryResult{}, err
		}
		if busy {
			return s.defer_(ctx, sc, "key has an active run; coalescing")
		}
		return s.correlateAdmit(ctx, sc, cfg, source, mappedInput, hash)
	case "replace":
		// Cancel the key's active run, then admit the new event in its place. (Post-irreversible-effect
		// replace/coalesce prohibition is T6's guard via tool_calls.replay_class — a policy field + doc
		// here, not enforced.)
		if err := s.cancelActiveForKey(ctx, sc, hash); err != nil {
			return DeliveryResult{}, err
		}
		return s.correlateAdmit(ctx, sc, cfg, source, mappedInput, hash)
	default: // allow
		return s.correlateAdmit(ctx, sc, cfg, source, mappedInput, hash)
	}
}

// skip_ terminalizes a delivery `skipped` with a reason and an optional survivor link (AUT-005 honest
// naming: a policy skip is distinct from a rejection), via the SM, from the delivery's ACTUAL current
// state (M4).
func (s *TriggerStore) skip_(ctx context.Context, sc deliveryScope, survivor, reason string) (DeliveryResult, error) {
	from, err := s.currentState(ctx, sc)
	if err != nil {
		return DeliveryResult{}, err
	}
	if _, _, err := statemachines.Apply(from, statemachines.TriggerDeliveryCmdSkip, statemachines.TriggerDeliveryTable); err != nil {
		return DeliveryResult{}, err
	}
	if _, err := s.pool.Exec(ctx, storage.Query("SkipDelivery"), sc.deliveryID, sc.org, sc.project, nullableText(survivor), reason); err != nil {
		return DeliveryResult{}, fmt.Errorf("skip delivery: %w", err)
	}
	return DeliveryResult{ID: sc.deliveryID, State: string(statemachines.TriggerDeliverySkipped), DuplicateOf: survivor, Reason: reason}, nil
}

// triggerBusy reports whether ANY delivery of the trigger has a non-terminal run (the singleton gate).
func (s *TriggerStore) triggerBusy(ctx context.Context, sc deliveryScope) (bool, error) {
	switch err := s.pool.QueryRow(ctx, storage.Query("TriggerHasActiveRun"), sc.triggerID, sc.org, sc.project).Scan(new(int)); {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("check trigger busy: %w", err)
	}
	return true, nil
}

// cancelActiveForKey cancels the key's currently-active run (the `replace` policy). No active run is a
// no-op. It reconciles the run to a single monotonic terminal via the admitter seam.
func (s *TriggerStore) cancelActiveForKey(ctx context.Context, sc deliveryScope, hash string) error {
	if hash == "" {
		return nil
	}
	var responseID, runID string
	switch err := s.pool.QueryRow(ctx, storage.Query("ActiveDeliveryRunForKey"), sc.triggerID, sc.org, sc.project, hash).Scan(&responseID, &runID); {
	case errors.Is(err, pgx.ErrNoRows):
		return nil // nothing to replace
	case err != nil:
		return fmt.Errorf("resolve active run to replace: %w", err)
	}
	canceled, uncertain, err := cancelProjections()
	if err != nil {
		return err
	}
	if _, err := s.admitter.CancelRunReconciled(ctx, coordinator.Tenant{Organization: sc.org, Project: sc.project}, responseID, runID, canceled, uncertain); err != nil {
		return fmt.Errorf("replace: cancel active run: %w", err)
	}
	return nil
}

// cancelProjections builds the canceled + uncertain-side-effect terminal projections a replace-cancel
// finalizes (the same shapes store.go's endpoint cancel uses).
func cancelProjections() (canceled, uncertain []byte, err error) {
	canceled, err = json.Marshal(map[string]any{"output": []contracts.ContentItem{}, "usage": contracts.Usage{}, "model": "", "error": contracts.CanceledProblem()})
	if err != nil {
		return nil, nil, err
	}
	uncertain, err = json.Marshal(map[string]any{"output": []contracts.ContentItem{}, "usage": contracts.Usage{}, "model": "", "error": contracts.UncertainSideEffectProblem()})
	if err != nil {
		return nil, nil, err
	}
	return canceled, uncertain, nil
}

// errGateContended signals that another same-key delivery holds the concurrency gate mid-admit — the
// caller treats it as the gate being busy (defer/drop per policy).
var errGateContended = errors.New("automation: concurrency gate contended")

// withGateLock runs fn while holding a Postgres session advisory lock keyed on the concurrency gate's
// scope, so concurrent same-key deliveries serialize through the gate-check + admit: one admits, the rest
// are contended (→ defer/drop per policy) (M3). It uses the NON-BLOCKING pg_try_advisory_lock: a blocking
// acquire would hold a pool connection while waiting AND the holder needs a second connection to admit, so
// N concurrent same-key deliveries would exhaust the pool and deadlock. Try-and-defer bounds each delivery
// to one connection and never waits. ponytail: one advisory lock per (trigger,key) — fine at trigger
// cadence; shard the key space if a single hot key ever bottlenecks.
func (s *TriggerStore) withGateLock(ctx context.Context, lockText string, fn func(context.Context) (DeliveryResult, error)) (DeliveryResult, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return DeliveryResult{}, fmt.Errorf("acquire gate-lock conn: %w", err)
	}
	defer conn.Release()
	var got bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock(hashtext($1)::bigint)", lockText).Scan(&got); err != nil {
		return DeliveryResult{}, fmt.Errorf("try gate lock: %w", err)
	}
	if !got {
		return DeliveryResult{}, errGateContended
	}
	defer func() {
		_, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock(hashtext($1)::bigint)", lockText)
	}()
	return fn(ctx)
}

// gateLockText is the advisory-lock key for a policy's gate scope: trigger-wide for singleton, per-key
// otherwise.
func gateLockText(policy, triggerID, hash string) string {
	if policy == "singleton" {
		return "trigger-gate:" + triggerID
	}
	// hashtext() runs on this text, so it must be valid UTF-8 (no null byte); ':' separates the ids.
	return "trigger-gate:" + triggerID + ":" + hash
}

// keyBusy reports whether the (trigger, correlation-key) group already has a delivery with a non-terminal
// run. An empty hash means no grouping key, so the group is never "busy" (nothing to serialize against).
func (s *TriggerStore) keyBusy(ctx context.Context, sc deliveryScope, hash string) (bool, error) {
	if hash == "" {
		return false, nil
	}
	switch err := s.pool.QueryRow(ctx, storage.Query("KeyHasActiveRun"), sc.triggerID, sc.org, sc.project, hash).Scan(new(int)); {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("check key busy: %w", err)
	}
	return true, nil
}

// defer_ gates a delivery behind a busy key (state → deferred). Its mapped_input + hash are already
// stored (recordMapped), so the reconciler admits it FIFO once the gate opens. It reads the ACTUAL
// current state so the SM transition is valid on whichever path reached it (sync 'mapped' or a
// reconciler-resume re-conflict, where the row is already 'deferred' → a no-op that retries next tick).
func (s *TriggerStore) defer_(ctx context.Context, sc deliveryScope, reason string) (DeliveryResult, error) {
	from, err := s.currentState(ctx, sc)
	if err != nil {
		return DeliveryResult{}, err
	}
	if from == statemachines.TriggerDeliveryDeferred {
		return DeliveryResult{ID: sc.deliveryID, State: string(from), Reason: reason}, nil
	}
	if _, _, err := statemachines.Apply(from, statemachines.TriggerDeliveryCmdDefer, statemachines.TriggerDeliveryTable); err != nil {
		return DeliveryResult{}, err
	}
	if _, err := s.pool.Exec(ctx, storage.Query("DeferDelivery"), sc.deliveryID, sc.org, sc.project, reason); err != nil {
		return DeliveryResult{}, fmt.Errorf("defer delivery: %w", err)
	}
	return DeliveryResult{ID: sc.deliveryID, State: string(statemachines.TriggerDeliveryDeferred), Reason: reason}, nil
}

// currentState reads the delivery's actual persisted SM state (so reject/fail/defer/skip transition from
// the real state, not a hardcoded guess that a resume path would violate — M4).
func (s *TriggerStore) currentState(ctx context.Context, sc deliveryScope) (statemachines.TriggerDeliveryState, error) {
	st, err := s.deliveryState(ctx, sc)
	if err != nil {
		return "", err
	}
	return statemachines.TriggerDeliveryState(st), nil
}

// admit builds a coordinator.AdmissionInput from the mapped canonical input + the pinned run target and
// reserves the run through the RunAdmitter seam — the SAME §20.9 admission transaction a POST
// /v1/responses runs. The run is born identically (a queued run + run.queued.v1 birth event + a dispatch
// job the existing workers claim), so a triggered run and an API-posted run are one durable object. The
// idempotency key is the delivery id, so a reconciler re-run REPLAYS rather than double-admitting.
//
// A6 admits per_event (a fresh session). Correlation modes (A7) and concurrency policy (A8/A9) decide,
// BEFORE this call, whether to admit, defer, skip, or reject — this method is the shared admit action.
func (s *TriggerStore) admit(ctx context.Context, sc deliveryScope, cfg revisionConfig, mappedInput []byte) (DeliveryResult, error) {
	if s.admitter == nil {
		return DeliveryResult{}, errors.New("automation: delivery admitter is not wired")
	}
	return s.admitChained(ctx, sc, cfg, mappedInput, nil)
}

// admitChained is admit with an optional existing session to chain onto (correlation bounded_key_reuse).
// requestedSession nil opens a fresh session (per_event).
func (s *TriggerStore) admitChained(ctx context.Context, sc deliveryScope, cfg revisionConfig, mappedInput []byte, requestedSession *string) (DeliveryResult, error) {
	responseID, runID, sessionID := newID("resp"), newID("run"), newID("ses")
	projection := contracts.Response{
		ID:             contracts.ResponseID(responseID),
		Object:         "response",
		Status:         "queued",
		CreatedAt:      time.Now().UTC().Format(time.RFC3339Nano),
		Output:         []contracts.ContentItem{},
		Usage:          contracts.Usage{},
		SessionID:      contracts.SessionID(sessionID),
		RunID:          contracts.RunID(runID),
		OrganizationID: contracts.OrganizationID(sc.org),
		ProjectID:      contracts.ProjectID(sc.project),
	}
	body, err := json.Marshal(projection)
	if err != nil {
		return DeliveryResult{}, err
	}
	sum := sha256.Sum256(mappedInput)
	adm, err := s.admitter.AdmitResponse(ctx, coordinator.Tenant{Organization: sc.org, Project: sc.project}, coordinator.AdmissionInput{
		Principal:             sc.principal,
		IdempotencyKey:        "trigger-delivery:" + sc.deliveryID,
		Method:                "POST",
		Route:                 createRoute,
		RequestHash:           hex.EncodeToString(sum[:]),
		ResponseID:            responseID,
		RunID:                 runID,
		SessionID:             sessionID,
		RequestedSessionID:    requestedSession,
		Input:                 mappedInput,
		Body:                  body,
		Store:                 true,
		AgentRevisionID:       cfg.AgentRevisionID,
		RunTemplateRevisionID: cfg.RunTemplateRevisionID,
	})
	if err != nil {
		return DeliveryResult{}, fmt.Errorf("admit triggered run: %w", err)
	}
	// A pin/session error is a delivery failure, not a run: no billable run was born (AUT-003). An
	// active-run conflict on a chained session is the queue signal (M2): the chain lost one-active-root, so
	// the delivery DEFERS (the reconciler admits it once the prior run terminates) rather than 500ing.
	if adm.ActiveRunConflict {
		return s.defer_(ctx, sc, "chained session has an active root run; queued")
	}
	if adm.Conflict || adm.SessionNotFound || adm.SessionConflict || adm.PinnedRevisionNotFound || adm.PinnedRevisionNotPublished || adm.RepositoryBindingNotFound {
		return s.fail(ctx, sc, admissionFailureReason(adm))
	}

	// On a REPLAY (M1: a crash between AdmitResponse-commit and the record below, then a reconciler
	// re-admit under the same idempotency key), the coordinator created NO new run and returns the ORIGINAL
	// body. Use its ids, not the freshly minted ones, so the delivery records the real run/session — a
	// ghost id would 404 at /v1/responses and make the KeyHasActiveRun / FindCorrelatedSession joins miss.
	if adm.Replayed {
		if id := responseField(adm.Body, "id"); id != "" {
			responseID = id
		}
		if rid := responseField(adm.Body, "run_id"); rid != "" {
			runID = rid
		}
	}
	// The resolved session is authoritative (a chained response reuses an existing session, and a replay
	// carries the original session; both are patched into the body's session_id). Read it back.
	resolvedSession := sessionID
	if requestedSession != nil || adm.Replayed {
		if sid := responseField(adm.Body, "session_id"); sid != "" {
			resolvedSession = sid
		}
	}
	if _, err := s.pool.Exec(ctx, storage.Query("RecordDeliveryAdmitted"),
		sc.deliveryID, sc.org, sc.project, responseID, runID, resolvedSession, mappedInput); err != nil {
		return DeliveryResult{}, fmt.Errorf("record admitted delivery: %w", err)
	}
	if _, err := s.transition(ctx, sc, statemachines.TriggerDeliveryAdmitted, statemachines.TriggerDeliveryCmdCreateRun); err != nil {
		return DeliveryResult{}, err
	}
	// The delivery now has a session, so its run_created event rides the run's own journal (a delivery is
	// trigger-born; an operator watching the run sees it). Best-effort — the durable delivery row is the
	// source of truth, so a failed emit does not fail the run.
	_ = s.emitRunCreated(ctx, sc, resolvedSession, responseID)

	return DeliveryResult{
		ID: sc.deliveryID, State: string(statemachines.TriggerDeliveryRunCreated),
		ResponseID: responseID, RunID: runID, SessionID: resolvedSession,
	}, nil
}

// emitRunCreated journals trigger.delivery.run_created.v1 into the run's session (the seq-then-append
// shape the coordinator uses), so a triggered run is visible as such in its own stream.
func (s *TriggerStore) emitRunCreated(ctx context.Context, sc deliveryScope, sessionID, responseID string) error {
	payload := fmt.Sprintf(`{"delivery_id":%q,"trigger_id":%q,"response_id":%q}`, sc.deliveryID, sc.triggerID, responseID)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var seq int64
	if err := tx.QueryRow(ctx, storage.Query("AllocateSequence"), sessionID).Scan(&seq); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, storage.Query("AppendEvent"),
		newID("evt"), sc.org, sc.project, sessionID, responseID, seq, "trigger.delivery.run_created.v1", []byte(payload)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// admissionFailureReason maps a typed admission rejection to a delivery failure reason.
func admissionFailureReason(adm coordinator.Admission) string {
	switch {
	case adm.PinnedRevisionNotFound:
		return "pinned revision not found"
	case adm.PinnedRevisionNotPublished:
		return "pinned revision is a draft"
	case adm.SessionNotFound:
		return "correlation session not found"
	case adm.SessionConflict:
		return "correlation session not active"
	case adm.RepositoryBindingNotFound:
		return "repository binding not found"
	default:
		return "admission conflict"
	}
}

// responseField reads a top-level string field from a response projection body.
func responseField(body []byte, field string) string {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return ""
	}
	var v string
	_ = json.Unmarshal(fields[field], &v)
	return v
}

// fail terminalizes a delivery `failed` with a reason (a mapping/admission failure), via the SM, from the
// delivery's ACTUAL current state (M4 — never a hardcoded guess a resume path would violate). A failed
// delivery leaves NO runs row (AUT-003): admission is never reached, so no billable run is born.
func (s *TriggerStore) fail(ctx context.Context, sc deliveryScope, reason string) (DeliveryResult, error) {
	from, err := s.currentState(ctx, sc)
	if err != nil {
		return DeliveryResult{}, err
	}
	to, _, err := statemachines.Apply(from, statemachines.TriggerDeliveryCmdFail, statemachines.TriggerDeliveryTable)
	if err != nil {
		return DeliveryResult{}, err
	}
	if _, err := s.pool.Exec(ctx, storage.Query("SetDeliveryReason"), sc.deliveryID, sc.org, sc.project, string(to), reason); err != nil {
		return DeliveryResult{}, fmt.Errorf("fail delivery: %w", err)
	}
	return DeliveryResult{ID: sc.deliveryID, State: string(to), Reason: reason}, nil
}

// markDuplicate links a losing delivery to its canonical original and terminalizes it `duplicate`.
func (s *TriggerStore) markDuplicate(ctx context.Context, sc deliveryScope, dedupeKey string) (DeliveryResult, error) {
	var original string
	if err := s.pool.QueryRow(ctx, storage.Query("FindCanonicalDelivery"),
		sc.triggerID, sc.org, sc.project, dedupeKey).Scan(&original); err != nil {
		return DeliveryResult{}, fmt.Errorf("resolve canonical original: %w", err)
	}
	if _, err := s.pool.Exec(ctx, storage.Query("MarkDeliveryDuplicate"),
		sc.deliveryID, sc.org, sc.project, original, dedupeKey, "duplicate of "+original); err != nil {
		return DeliveryResult{}, fmt.Errorf("mark delivery duplicate: %w", err)
	}
	return DeliveryResult{ID: sc.deliveryID, State: string(statemachines.TriggerDeliveryDuplicate), DuplicateOf: original}, nil
}

// reject terminalizes a delivery `rejected` with a reason (auth/policy denial), via the SM, from the
// delivery's ACTUAL current state (M4).
func (s *TriggerStore) reject(ctx context.Context, sc deliveryScope, reason string) (DeliveryResult, error) {
	from, err := s.currentState(ctx, sc)
	if err != nil {
		return DeliveryResult{}, err
	}
	to, _, err := statemachines.Apply(from, statemachines.TriggerDeliveryCmdReject, statemachines.TriggerDeliveryTable)
	if err != nil {
		return DeliveryResult{}, err
	}
	if _, err := s.pool.Exec(ctx, storage.Query("SetDeliveryReason"), sc.deliveryID, sc.org, sc.project, string(to), reason); err != nil {
		return DeliveryResult{}, fmt.Errorf("reject delivery: %w", err)
	}
	return DeliveryResult{ID: sc.deliveryID, State: string(to), Reason: reason}, nil
}

// transition validates a state change through the TriggerDelivery table and persists the new state.
func (s *TriggerStore) transition(ctx context.Context, sc deliveryScope, from statemachines.TriggerDeliveryState, cmd statemachines.TriggerDeliveryCommand) (statemachines.TriggerDeliveryState, error) {
	to, _, err := statemachines.Apply(from, cmd, statemachines.TriggerDeliveryTable)
	if err != nil {
		return "", fmt.Errorf("trigger delivery transition: %w", err)
	}
	if _, err := s.pool.Exec(ctx, storage.Query("SetDeliveryState"), sc.deliveryID, sc.org, sc.project, string(to)); err != nil {
		return "", fmt.Errorf("persist delivery state: %w", err)
	}
	return to, nil
}

// loadRevisionConfig reads the pinned revision's delivery-pipeline config.
func (s *TriggerStore) loadRevisionConfig(ctx context.Context, sc deliveryScope) (revisionConfig, error) {
	var (
		cfg                   revisionConfig
		agentRev, templateRev *string
	)
	if err := s.pool.QueryRow(ctx, storage.Query("GetTriggerRevision"), sc.revisionID, sc.org, sc.project).
		Scan(&agentRev, &templateRev, &cfg.InputMapping, &cfg.DedupeKeyExpr, &cfg.CorrelationMode, &cfg.CorrelationKeyExpr, &cfg.ConcurrencyPolicy); err != nil {
		return revisionConfig{}, fmt.Errorf("load revision config: %w", err)
	}
	if agentRev != nil {
		cfg.AgentRevisionID = *agentRev
	}
	if templateRev != nil {
		cfg.RunTemplateRevisionID = *templateRev
	}
	return cfg, nil
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

// decodePayload parses a manual/API delivery payload into a source map for the mapping language. A
// non-object payload is an error (the mapping language selects fields from an object).
func decodePayload(payload []byte) (map[string]any, error) {
	if len(payload) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// boundedKey evaluates a key expression and bounds it: an over-long raw key is replaced by its SHA-256
// hex so an index entry can never grow unbounded (a tenant-authored expr could otherwise yield a huge
// string). An empty expr yields "".
func boundedKey(expr string, source map[string]any) (string, error) {
	compiled, err := CompileExpr(expr, nil)
	if err != nil {
		return "", err
	}
	key, err := compiled.EvalString(source)
	if err != nil {
		return "", err
	}
	if len(key) > 256 {
		sum := sha256.Sum256([]byte(key))
		return "sha256:" + hex.EncodeToString(sum[:]), nil
	}
	return key, nil
}

// isUniqueViolation reports whether err is a Postgres 23505 unique_violation.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
