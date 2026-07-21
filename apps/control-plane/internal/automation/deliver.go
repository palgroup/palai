package automation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	statemachines "github.com/palgroup/palai/packages/state-machines"

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

// advance drives a received delivery through the pipeline. It is grown stage by stage across the E11 T2
// slice: A4 accepts + pins (the delivery is born received); A5 adds dedupe; A6 map + admission; A7
// correlation; A8/A9 concurrency policy. Each stage validates the transition through the TriggerDelivery
// state machine and persists the new state.
//
// DESIGN — the trigger_deliveries.state column IS the durable record (queryable via GET
// /v1/trigger-deliveries/{id}); the SM authorizes each transition (an illegal one is a bug, not a state).
// The trigger.delivery.* events are registered in the contract for downstream consumers; a delivery has
// NO session before admission (events are session-scoped, NOT NULL), so pre-admission transitions persist
// the state column only and the run-born events ride the run's session once it exists (A6). This mirrors
// the agents.go precedent (the durable fact is the row; the event is declared-but-unemitted here).
func (s *TriggerStore) advance(ctx context.Context, sc deliveryScope, payload []byte) (DeliveryResult, error) {
	cfg, err := s.loadRevisionConfig(ctx, sc)
	if err != nil {
		return DeliveryResult{}, err
	}
	source, err := decodePayload(payload)
	if err != nil {
		// A malformed source payload cannot be mapped; reject the delivery (no run).
		return s.reject(ctx, sc, statemachines.TriggerDeliveryReceived, "source payload is not a JSON object")
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
	dedupeKey, err := boundedKey(cfg.DedupeKeyExpr, source)
	if err != nil {
		return DeliveryResult{}, err
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

	// A5 stops at deduplicated. A6 adds map → admit → run_created.
	state, err := s.deliveryState(ctx, sc)
	if err != nil {
		return DeliveryResult{}, err
	}
	return DeliveryResult{ID: sc.deliveryID, State: state}, nil
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

// reject terminalizes a delivery `rejected` with a reason (auth/policy denial), via the SM.
func (s *TriggerStore) reject(ctx context.Context, sc deliveryScope, from statemachines.TriggerDeliveryState, reason string) (DeliveryResult, error) {
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
