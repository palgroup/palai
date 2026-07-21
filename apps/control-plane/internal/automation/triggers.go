package automation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

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
)

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
	id := newID("trev")
	var number int
	if err := s.pool.QueryRow(ctx, storage.Query("InsertTriggerRevision"),
		id, org, project, triggerID,
		nullableText(in.AgentRevisionID), nullableText(in.RunTemplateRevisionID), mappingJSON(in.InputMapping),
		in.DedupeKeyExpr, defaultMode(in.CorrelationMode), in.CorrelationKeyExpr, defaultPolicy(in.ConcurrencyPolicy),
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
		return errors.New("automation: a trigger revision pins an agent revision OR a run template, never both")
	}
	if _, err := CompileMapping(in.InputMapping, nil); err != nil {
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
