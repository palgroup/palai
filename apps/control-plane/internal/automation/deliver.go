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

	scope := deliveryScope{org: org, project: project, principal: principal, triggerID: triggerID, revisionID: rev.ID, deliveryID: deliveryID}
	return s.advance(ctx, scope, payload)
}

// deliveryScope carries the tenant + pinned-revision coordinates a delivery advances within.
type deliveryScope struct {
	org, project, principal, triggerID, revisionID, deliveryID string
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

	// Map (AUT-003): compile + evaluate the mapping into the canonical action input. A schema-invalid
	// mapping FAILS the delivery WITHOUT a run — no billable run is ever born from an unmappable event.
	mapping, err := CompileMapping(cfg.InputMapping, nil)
	if err != nil {
		return s.fail(ctx, sc, statemachines.TriggerDeliveryDeduplicated, "input mapping is invalid: "+err.Error())
	}
	mappedInput, err := mapping.Apply(source)
	if err != nil {
		return s.fail(ctx, sc, statemachines.TriggerDeliveryDeduplicated, "input mapping failed: "+err.Error())
	}
	if _, err := s.transition(ctx, sc, statemachines.TriggerDeliveryDeduplicated, statemachines.TriggerDeliveryCmdMap); err != nil {
		return DeliveryResult{}, err
	}

	// Correlate + act (spec §20.2.2): the correlation mode decides the target session — a fresh one
	// (per_event), a chained one (bounded_key_reuse), an existing named session the mapped input is
	// appended to (named_session), or a rejection when the correlated session is busy (reject_if_active).
	return s.correlate(ctx, sc, cfg, source, mappedInput)
}

// correlate resolves the target session per the revision's correlation mode and runs the shared admit /
// append / reject action. A bounded correlation key is SHA-256'd with the (project, trigger_revision,
// source_tenant) scope; ONLY the hash is stored (the raw key never lands in the DB). A correlation query
// touches only THIS tenant's deliveries, so it can never reach a foreign session (authz is not bypassed),
// and admission still enforces retention/scope on the resolved session.
func (s *TriggerStore) correlate(ctx context.Context, sc deliveryScope, cfg revisionConfig, source map[string]any, mappedInput []byte) (DeliveryResult, error) {
	switch cfg.CorrelationMode {
	case "named_session":
		key, err := boundedKey(cfg.CorrelationKeyExpr, source)
		if err != nil {
			return DeliveryResult{}, err
		}
		return s.appendToNamedSession(ctx, sc, key, mappedInput)
	case "bounded_key_reuse", "reject_if_active":
		hash, err := s.correlationHash(ctx, sc, cfg.CorrelationKeyExpr, source)
		if err != nil {
			return DeliveryResult{}, err
		}
		var prior string
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
				return s.reject(ctx, sc, statemachines.TriggerDeliveryMapped, "correlated session has an active root run")
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

// correlationHash computes and records the delivery's bounded correlation-key hash — SHA-256 over the
// (project, trigger_revision, source_tenant, raw-key) tuple, so only the hash is stored. An empty key
// expr yields "" (no correlation), recorded as the empty hash.
func (s *TriggerStore) correlationHash(ctx context.Context, sc deliveryScope, expr string, source map[string]any) (string, error) {
	key, err := CompileExpr(expr, nil)
	if err != nil {
		return "", err
	}
	raw, err := key.EvalString(source)
	if err != nil {
		return "", err
	}
	if raw == "" {
		return "", nil
	}
	// source_tenant is '' in T2 (manual/api carries no signed source envelope — T5).
	scoped := sc.project + "\x00" + sc.revisionID + "\x00" + "" + "\x00" + raw
	sum := sha256.Sum256([]byte(scoped))
	hash := hex.EncodeToString(sum[:])
	if _, err := s.pool.Exec(ctx, storage.Query("SetDeliveryCorrelationHash"), sc.deliveryID, sc.org, sc.project, hash); err != nil {
		return "", fmt.Errorf("record correlation hash: %w", err)
	}
	return hash, nil
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
		return s.fail(ctx, sc, statemachines.TriggerDeliveryMapped, "named_session correlation produced no session id")
	}
	tenant := coordinator.Tenant{Organization: sc.org, Project: sc.project}
	// Resolve the target run BEFORE the send, so the delivery records the run it joined.
	var runID, responseID string
	switch err := s.pool.QueryRow(ctx, storage.Query("ActiveRootRun"), sessionID, sc.org, sc.project).Scan(&runID, &responseID); {
	case errors.Is(err, pgx.ErrNoRows):
		return s.fail(ctx, sc, statemachines.TriggerDeliveryMapped, "named session has no active run to receive the message")
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
		return s.fail(ctx, sc, statemachines.TriggerDeliveryMapped, "named session not found in scope")
	}
	if cmd.State == "rejected" {
		return s.fail(ctx, sc, statemachines.TriggerDeliveryMapped, "named session rejected the message (no live run)")
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
	// active-run conflict on a chained session is the queue signal (A8) — surfaced to the caller.
	if adm.ActiveRunConflict {
		return DeliveryResult{ID: sc.deliveryID, State: string(statemachines.TriggerDeliveryMapped), Reason: "active_run"}, errActiveRun
	}
	if adm.Conflict || adm.SessionNotFound || adm.SessionConflict || adm.PinnedRevisionNotFound || adm.PinnedRevisionNotPublished || adm.RepositoryBindingNotFound {
		return s.fail(ctx, sc, statemachines.TriggerDeliveryMapped, admissionFailureReason(adm))
	}

	// The resolved session is authoritative (a chained response reuses an existing session; the admission
	// patches the body's session_id). Read it back so the delivery records the real session.
	resolvedSession := sessionID
	if requestedSession != nil {
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

// errActiveRun signals a chained admission lost to one-active-root — the queue/defer signal (A8).
var errActiveRun = errors.New("automation: session already has an active root run")

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

// fail terminalizes a delivery `failed` with a reason (a mapping/admission failure), via the SM. A
// failed delivery leaves NO runs row (AUT-003): admission is never reached, so no billable run is born.
func (s *TriggerStore) fail(ctx context.Context, sc deliveryScope, from statemachines.TriggerDeliveryState, reason string) (DeliveryResult, error) {
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
