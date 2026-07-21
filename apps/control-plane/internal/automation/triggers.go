package automation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/storage"
)

// TriggerStore is the pgx-backed store for the trigger management surface + the delivery pipeline (spec
// §20.2.2, E11 Task 2). It shares the durable spine's pool. Management writes (create/revise) and the
// delivery accept/advance are tenant-scoped by the verified identity. A delivery is born via the SAME
// §20.9 admission path as /v1/responses: the pipeline builds a coordinator.AdmissionInput and calls the
// admitter seam, so a triggered run and a POST /v1/responses run are the same durable object.
type TriggerStore struct {
	pool     *pgxpool.Pool
	admitter RunAdmitter
}

// NewTriggerStore wraps a shared connection pool. The admitter (the durable coordinator) is wired
// separately via WithAdmitter so the pure management surface is testable without it.
func NewTriggerStore(pool *pgxpool.Pool) *TriggerStore { return &TriggerStore{pool: pool} }

// WithAdmitter binds the run-admission seam the delivery pipeline admits through (the coordinator spine
// in production). Returns the store for chaining.
func (s *TriggerStore) WithAdmitter(a RunAdmitter) *TriggerStore {
	s.admitter = a
	return s
}

var (
	// ErrTriggerNotFound is returned when a delivery/revision targets a trigger absent from the scope.
	ErrTriggerNotFound = errors.New("automation: trigger not found in scope")
	// ErrTriggerDisabled is returned when a delivery targets a disabled trigger.
	ErrTriggerDisabled = errors.New("automation: trigger is disabled")
	// ErrNoActiveRevision is returned when a delivery targets a trigger with no revision to pin.
	ErrNoActiveRevision = errors.New("automation: trigger has no revision to pin")
	// ErrBothPins is returned when a revision pins BOTH an agent revision and a run template.
	ErrBothPins = errors.New("automation: a trigger revision pins an agent revision OR a run template, never both")
	// ErrNamedSessionCannotDefer is returned when named_session correlation is combined with a deferring
	// concurrency policy (queue/singleton/coalesce) — a deferred delivery would lose its target session id.
	ErrNamedSessionCannotDefer = errors.New("automation: named_session cannot combine with a deferring concurrency policy")
	// ErrInvalidCorrelationMode / ErrInvalidConcurrencyPolicy reject an out-of-enum value at revise (a 400),
	// rather than letting it fall through to a DB CHECK 500 (m8).
	ErrInvalidCorrelationMode   = errors.New("automation: invalid correlation_mode")
	ErrInvalidConcurrencyPolicy = errors.New("automation: invalid concurrency_policy")
	// ErrReplaceNeedsKey rejects a replace policy with no correlation_key_expr — with no key there is no
	// active run to identify, so replace would silently degenerate to allow (m10).
	ErrReplaceNeedsKey = errors.New("automation: replace concurrency policy requires a correlation_key_expr")
	// ErrCallbackEndpointNotFound rejects a revise naming a callback_endpoint_id absent from the tenant's
	// scope. The FK is global, so this app-side check is the ONLY thing stopping a run result from being
	// delivered to a foreign tenant's URL (a foreign/unknown id discloses no existence — a not-found).
	ErrCallbackEndpointNotFound = errors.New("automation: callback endpoint not found in scope")
	// ErrIdempotencyMismatch is returned when a delivery Idempotency-Key is reused with a different request
	// body (AUT-013): the same key must map to one delivery, so a body change is a typed conflict, not a
	// silent second action.
	ErrIdempotencyMismatch = errors.New("automation: idempotency key reused with a different request")
)

// validCorrelationModes / validConcurrencyPolicies mirror the 000021 CHECK constraints.
var (
	validCorrelationModes    = map[string]bool{"per_event": true, "bounded_key_reuse": true, "named_session": true, "reject_if_active": true}
	validConcurrencyPolicies = map[string]bool{"allow": true, "queue": true, "replace": true, "drop_if_running": true, "coalesce": true, "singleton": true}
)

// deferringPolicy reports whether a concurrency policy can DEFER a delivery for the reconciler.
func deferringPolicy(policy string) bool {
	switch policy {
	case "queue", "singleton", "coalesce":
		return true
	default:
		return false
	}
}

// TriggerRevisionInput is the executable config a revision carries (spec §20.2.2). At most one run
// target is pinned (validated before insert). InputMapping is the bounded mapping document; the two key
// exprs are single-rule expressions in the SAME language.
type TriggerRevisionInput struct {
	AgentRevisionID       string
	RunTemplateRevisionID string
	InputMapping          json.RawMessage
	DedupeKeyExpr         string
	CorrelationMode       string
	CorrelationKeyExpr    string
	ConcurrencyPolicy     string
	// T6 (callback execution + output shaping, spec §20.2.2). OutputMapping shapes the run's terminal
	// projection into the callback envelope's data through the SAME bounded mapping language the input
	// uses (no second language). CallbackEndpointID names a registered webhook endpoint the shaped
	// callback is delivered to; it is app-side scope-checked at revise (the FK is global).
	OutputMapping      json.RawMessage
	CallbackEndpointID string
}

// Revision identifies a stored trigger revision (management projection + the pin assertion).
type TriggerRevision struct {
	ID             string
	RevisionNumber int
}

// CreateTrigger inserts a named trigger lineage and returns its id. triggerType defaults to manual_api.
func (s *TriggerStore) CreateTrigger(ctx context.Context, org, project, name, triggerType string) (string, error) {
	if triggerType == "" {
		triggerType = "manual_api"
	}
	id := newID("trg")
	if _, err := s.pool.Exec(ctx, storage.Query("InsertTrigger"), id, org, project, name, triggerType); err != nil {
		return "", fmt.Errorf("insert trigger: %w", err)
	}
	return id, nil
}

// ReviseTrigger inserts a NEW immutable revision under a trigger (revise = new INSERT, never an in-place
// UPDATE — the 000019 discipline). It verifies the trigger is in scope first, then compiles the mapping +
// key exprs so a malformed/escape-carrying mapping is rejected before it is stored (fail-closed). The
// active revision is simply the highest revision_number; there is no publish flag (AGT-002).
func (s *TriggerStore) ReviseTrigger(ctx context.Context, org, project, triggerID string, in TriggerRevisionInput) (TriggerRevision, error) {
	if _, err := s.triggerEnabled(ctx, org, project, triggerID); err != nil {
		return TriggerRevision{}, err
	}
	if err := validateRevisionInput(in); err != nil {
		return TriggerRevision{}, err
	}
	// A callback endpoint must belong to THIS tenant (the FK is global — this app-side check is what stops
	// a run result from being delivered to a foreign tenant's URL). Done after the pure validation so a
	// malformed mapping is still the first error surfaced.
	if err := s.verifyCallbackEndpointInScope(ctx, org, project, in.CallbackEndpointID); err != nil {
		return TriggerRevision{}, err
	}
	id := newID("trev")
	var number int
	if err := s.pool.QueryRow(ctx, storage.Query("InsertTriggerRevision"),
		id, org, project, triggerID,
		nullableText(in.AgentRevisionID), nullableText(in.RunTemplateRevisionID), mappingJSON(in.InputMapping),
		in.DedupeKeyExpr, defaultMode(in.CorrelationMode), in.CorrelationKeyExpr, defaultPolicy(in.ConcurrencyPolicy),
		mappingJSON(in.OutputMapping), nullableText(in.CallbackEndpointID),
	).Scan(&number); err != nil {
		return TriggerRevision{}, fmt.Errorf("insert trigger revision: %w", err)
	}
	return TriggerRevision{ID: id, RevisionNumber: number}, nil
}

// GetActiveRevision resolves a trigger's ACTIVE revision (highest revision_number) — the revision a new
// delivery pins at accept. found=false when the trigger has no revision yet.
func (s *TriggerStore) GetActiveRevision(ctx context.Context, org, project, triggerID string) (TriggerRevision, bool, error) {
	var rev TriggerRevision
	switch err := s.pool.QueryRow(ctx, storage.Query("ActiveTriggerRevision"), triggerID, org, project).
		Scan(&rev.ID, &rev.RevisionNumber); {
	case errors.Is(err, pgx.ErrNoRows):
		return TriggerRevision{}, false, nil
	case err != nil:
		return TriggerRevision{}, false, fmt.Errorf("resolve active trigger revision: %w", err)
	}
	return rev, true, nil
}

// TriggerView is a trigger's management projection (GET /v1/triggers/{id}).
type TriggerView struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Type           string `json:"type"`
	Enabled        bool   `json:"enabled"`
	ActiveRevision int    `json:"active_revision"`
}

// GetTrigger reads a trigger's management projection, or found=false when it is absent from the scope.
func (s *TriggerStore) GetTrigger(ctx context.Context, org, project, triggerID string) (TriggerView, bool, error) {
	v := TriggerView{ID: triggerID}
	switch err := s.pool.QueryRow(ctx, storage.Query("GetTrigger"), triggerID, org, project).
		Scan(&v.Name, &v.Type, &v.Enabled, &v.ActiveRevision); {
	case errors.Is(err, pgx.ErrNoRows):
		return TriggerView{}, false, nil
	case err != nil:
		return TriggerView{}, false, fmt.Errorf("read trigger: %w", err)
	}
	return v, true, nil
}

// TriggerDeliveryView is a delivery's operator-facing projection (GET /v1/trigger-deliveries/{id}).
type TriggerDeliveryView struct {
	ID          string `json:"id"`
	TriggerID   string `json:"trigger_id"`
	RevisionID  string `json:"trigger_revision_id"`
	State       string `json:"state"`
	ResponseID  string `json:"response_id,omitempty"`
	RunID       string `json:"run_id,omitempty"`
	SessionID   string `json:"session_id,omitempty"`
	DuplicateOf string `json:"duplicate_of,omitempty"`
	Reason      string `json:"reason,omitempty"`
	// CallbackState is the post-run callback's own terminal (''/pending/delivered/dead), independent of the
	// delivery State — a callback that dead-letters never rewinds run_created (AUT-011 link-half).
	CallbackState string    `json:"callback_state,omitempty"`
	ReceivedAt    time.Time `json:"received_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// GetDelivery reads a delivery's projection, or found=false when it is absent from the scope.
func (s *TriggerStore) GetDelivery(ctx context.Context, org, project, deliveryID string) (TriggerDeliveryView, bool, error) {
	v := TriggerDeliveryView{ID: deliveryID}
	switch err := s.pool.QueryRow(ctx, storage.Query("GetTriggerDelivery"), deliveryID, org, project).
		Scan(&v.TriggerID, &v.RevisionID, &v.State, &v.ResponseID, &v.RunID, &v.SessionID, &v.DuplicateOf, &v.Reason, &v.CallbackState, &v.ReceivedAt, &v.UpdatedAt); {
	case errors.Is(err, pgx.ErrNoRows):
		return TriggerDeliveryView{}, false, nil
	case err != nil:
		return TriggerDeliveryView{}, false, fmt.Errorf("read trigger delivery: %w", err)
	}
	return v, true, nil
}

// triggerEnabled verifies a trigger is in scope and returns its enabled flag, mapping absence to
// ErrTriggerNotFound (a foreign/unknown trigger discloses no existence).
func (s *TriggerStore) triggerEnabled(ctx context.Context, org, project, triggerID string) (bool, error) {
	var enabled bool
	switch err := s.pool.QueryRow(ctx, storage.Query("TriggerForDelivery"), triggerID, org, project).Scan(&enabled); {
	case errors.Is(err, pgx.ErrNoRows):
		return false, ErrTriggerNotFound
	case err != nil:
		return false, fmt.Errorf("verify trigger: %w", err)
	}
	return enabled, nil
}

// validateRevisionInput enforces the at-most-one-pin invariant and compiles the mapping + key exprs so a
// malformed or escape-carrying mapping never reaches the database (fail-closed, the mapping-language
// security bound). The allowlist for secret refs is the trigger's — empty here (secret refs are a T6
// concern); a mapping naming any secret is rejected until an allowlist is provisioned.
func validateRevisionInput(in TriggerRevisionInput) error {
	if in.AgentRevisionID != "" && in.RunTemplateRevisionID != "" {
		return ErrBothPins
	}
	// Reject out-of-enum mode/policy at revise (a 400), not at the DB CHECK (a 500) — m8.
	if in.CorrelationMode != "" && !validCorrelationModes[in.CorrelationMode] {
		return ErrInvalidCorrelationMode
	}
	if in.ConcurrencyPolicy != "" && !validConcurrencyPolicies[in.ConcurrencyPolicy] {
		return ErrInvalidConcurrencyPolicy
	}
	// named_session appends to an existing session immediately; it can never DEFER (a deferred delivery
	// resumes without its source payload, so the target session id — derived from the source — is lost).
	// Reject the combo at revise so it can never be stored (M4).
	if in.CorrelationMode == "named_session" && deferringPolicy(in.ConcurrencyPolicy) {
		return ErrNamedSessionCannotDefer
	}
	if in.ConcurrencyPolicy == "replace" && in.CorrelationKeyExpr == "" {
		return ErrReplaceNeedsKey
	}
	if _, err := CompileMapping(in.InputMapping, nil); err != nil {
		return err
	}
	// The output_mapping is compiled through the SAME bounded language (no second language): a malformed
	// or escape-carrying output mapping is rejected here, exactly as the input mapping is (T6).
	if _, err := CompileMapping(in.OutputMapping, nil); err != nil {
		return err
	}
	if _, err := CompileExpr(in.DedupeKeyExpr, nil); err != nil {
		return err
	}
	if _, err := CompileExpr(in.CorrelationKeyExpr, nil); err != nil {
		return err
	}
	return nil
}

// verifyCallbackEndpointInScope confirms a named callback endpoint belongs to the tenant. An empty id
// (no callback configured) is a no-op; a foreign/unknown id is ErrCallbackEndpointNotFound (the FK is
// global, so this is the only cross-tenant guard — a not-found discloses no existence).
func (s *TriggerStore) verifyCallbackEndpointInScope(ctx context.Context, org, project, endpointID string) error {
	if endpointID == "" {
		return nil
	}
	switch err := s.pool.QueryRow(ctx, storage.Query("WebhookEndpointInScope"), endpointID, org, project).Scan(new(string)); {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrCallbackEndpointNotFound
	case err != nil:
		return fmt.Errorf("verify callback endpoint scope: %w", err)
	}
	return nil
}

// nullableText keeps an empty string NULL (an unset optional FK/column) and a non-empty string a stored
// value.
func nullableText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// mappingJSON defaults an empty mapping document to '{}' so the JSONB column is always valid JSON.
func mappingJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return []byte("{}")
	}
	return []byte(raw)
}

func defaultMode(mode string) string {
	if mode == "" {
		return "per_event"
	}
	return mode
}

func defaultPolicy(policy string) string {
	if policy == "" {
		return "allow"
	}
	return policy
}
