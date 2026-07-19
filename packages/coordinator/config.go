package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/palgroup/palai/storage"
)

// configRevisedEvent journals a session config revision's content (the redacted, content-
// addressed snapshot with provenance) at the boundary it applied. It is not a state-machine
// transition, so it is named here alongside the other command-adjacent journal events.
const configRevisedEvent = "config.revised.v1"

// ConfigPolicy is a project's config allowlist and tools baseline (spec §9.3 typed denial,
// §14.4 project baseline). An empty AllowedModels / AllowedTools means unrestricted (so a
// project with no policy — NULL config_policy — permits any change); DefaultTools is the tools
// baseline the ConfigSnapshot resolves from. It is the layer a change_config is validated
// against at accept, before any revision is created (no silent fallback — SES-008).
type ConfigPolicy struct {
	AllowedModels []string `json:"allowed_models,omitempty"`
	AllowedTools  []string `json:"allowed_tools,omitempty"`
	DefaultTools  []string `json:"default_tools,omitempty"`
}

// AllowModel reports whether model is permitted. An empty allowlist is unrestricted, and an
// empty requested model (a tools-only change) is always permitted.
func (p ConfigPolicy) AllowModel(model string) bool {
	if len(p.AllowedModels) == 0 || model == "" {
		return true
	}
	return slices.Contains(p.AllowedModels, model)
}

// DeniedTool returns the first requested tool outside the allowlist, or "" if every tool is
// allowed. An empty allowlist is unrestricted.
func (p ConfigPolicy) DeniedTool(tools []string) string {
	if len(p.AllowedTools) == 0 {
		return ""
	}
	for _, t := range tools {
		if !slices.Contains(p.AllowedTools, t) {
			return t
		}
	}
	return ""
}

// ProjectConfig reads a project's config policy within the tenant scope. A NULL config_policy
// (the default for every existing project) yields the zero value — unrestricted, empty
// baseline. The orchestrator reads the DefaultTools baseline here for the ConfigSnapshot.
func (s *Store) ProjectConfig(ctx context.Context, tenant Tenant) (ConfigPolicy, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, storage.Query("GetProjectConfig"), tenant.Organization, tenant.Project).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return ConfigPolicy{}, nil
	}
	if err != nil {
		return ConfigPolicy{}, fmt.Errorf("read project config: %w", err)
	}
	return decodeConfigPolicy(raw), nil
}

// projectConfigTx is ProjectConfig within an open transaction, for the accept path's policy
// check (the deny must commit atomically with the command's rejection).
func projectConfigTx(ctx context.Context, tx pgx.Tx, tenant Tenant) (ConfigPolicy, error) {
	var raw []byte
	err := tx.QueryRow(ctx, storage.Query("GetProjectConfig"), tenant.Organization, tenant.Project).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return ConfigPolicy{}, nil
	}
	if err != nil {
		return ConfigPolicy{}, fmt.Errorf("read project config: %w", err)
	}
	return decodeConfigPolicy(raw), nil
}

func decodeConfigPolicy(raw []byte) ConfigPolicy {
	if len(raw) == 0 {
		return ConfigPolicy{}
	}
	var p ConfigPolicy
	_ = json.Unmarshal(raw, &p)
	return p
}

// SessionOverride is a session's effective config override — the latest config revision's
// resolved values (spec §9.3). An empty Model / nil Tools means the session never overrode
// that value, so the orchestrator's step falls back to the deployment/project default.
type SessionOverride struct {
	Model string
	Tools []string
}

// LatestSessionConfig reads a session's most recent config revision, the effective override the
// orchestrator routes a model step under. found is false when the session has no revision.
func (s *Store) LatestSessionConfig(ctx context.Context, tenant Tenant, sessionID string) (SessionOverride, bool, error) {
	var override SessionOverride
	var toolsJSON []byte
	err := s.pool.QueryRow(ctx, storage.Query("LatestSessionConfig"), sessionID, tenant.Organization, tenant.Project).
		Scan(&override.Model, &toolsJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionOverride{}, false, nil
	}
	if err != nil {
		return SessionOverride{}, false, fmt.Errorf("read latest session config: %w", err)
	}
	if len(toolsJSON) > 0 {
		_ = json.Unmarshal(toolsJSON, &override.Tools)
	}
	return override, true, nil
}

// ConfigChangePlan is the pre-resolved config revision the orchestrator commits at a boundary.
// The orchestrator (not the coordinator) owns the snapshot resolution, so the durable layer
// stays a dumb writer that never re-derives the effective config: Model/ToolsJSON are the
// resolved effective values, SnapshotHash is the content address, and RevisedPayload is the
// redacted config.revised.v1 event body (secret refs stay refs — LP secret hygiene).
type ConfigChangePlan struct {
	RevisionID     string
	Model          string
	ToolsJSON      []byte
	SnapshotHash   string
	Immediate      bool
	RevisedPayload []byte
}

// ApplyConfigChange applies a normal (boundary) config change in one transaction: it claims the
// queued change_config command (command.applied.v1, whose sequence is the applied_sequence),
// inserts the config revision at that sequence, and journals config.revised.v1 (spec §9.3,
// §22.4). It runs under guardRunActive, so a canceled run rejects the write. A command another
// path already applied returns ErrCommandNotPending. Returns the applied_sequence.
func (s *Store) ApplyConfigChange(ctx context.Context, tenant Tenant, sessionID, responseID, runID, commandID string, plan ConfigChangePlan) (int64, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, fmt.Errorf("begin apply config change: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if err := guardRunActive(ctx, tx, tenant, runID); err != nil {
		return 0, err
	}
	seq, err := applyConfigChangeInTx(ctx, tx, tenant, sessionID, responseID, commandID, plan)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit apply config change: %w", err)
	}
	return seq, nil
}

// InterruptForConfigChange applies an immediate config switch that aborted the in-flight model
// step (spec §9.3, §25.16). In one transaction it journals the partial step (partialEventType,
// carrying the streamed-so-far output — §25.16 partial-item rule), applies the change (the
// shared applyConfigChangeInTx: command.applied.v1 + config revision + config.revised.v1), and
// raises a warning (warningEventType) that the in-flight attempt was interrupted for the switch.
// It runs under guardRunActive; a command a boundary already applied returns ErrCommandNotPending.
func (s *Store) InterruptForConfigChange(ctx context.Context, tenant Tenant, sessionID, responseID, runID string, plan ConfigChangePlan, commandID, partialEventType string, partialPayload []byte, warningEventType string, warningPayload []byte) (int64, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, fmt.Errorf("begin interrupt config change: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if err := guardRunActive(ctx, tx, tenant, runID); err != nil {
		return 0, err
	}
	if _, err := appendEvent(ctx, tx, tenant, sessionID, responseID, partialEventType, partialPayload); err != nil {
		return 0, err
	}
	seq, err := applyConfigChangeInTx(ctx, tx, tenant, sessionID, responseID, commandID, plan)
	if err != nil {
		return 0, err
	}
	if _, err := appendEvent(ctx, tx, tenant, sessionID, responseID, warningEventType, warningPayload); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit interrupt config change: %w", err)
	}
	return seq, nil
}

// applyConfigChangeInTx is the single-winner config apply shared by the boundary (normal) and
// in-flight-abort (immediate) paths: it claims the queued command (command.applied.v1 at seq S,
// the applied_sequence), inserts the config revision at S, and journals config.revised.v1. A
// not-queued command (already applied, or claimed by a racing path) returns ErrCommandNotPending.
func applyConfigChangeInTx(ctx context.Context, tx pgx.Tx, tenant Tenant, sessionID, responseID, commandID string, plan ConfigChangePlan) (int64, error) {
	seq, err := applyCommandInTx(ctx, tx, tenant, sessionID, responseID, commandID)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, storage.Query("InsertConfigRevision"),
		plan.RevisionID, tenant.Organization, tenant.Project, sessionID, commandID, seq,
		plan.Model, nullableJSON(plan.ToolsJSON), plan.SnapshotHash, plan.Immediate); err != nil {
		return 0, fmt.Errorf("insert config revision: %w", err)
	}
	if _, err := appendEvent(ctx, tx, tenant, sessionID, responseID, configRevisedEvent, plan.RevisedPayload); err != nil {
		return 0, err
	}
	return seq, nil
}

// nullableJSON keeps an empty tools override NULL so a tools-untouched revision does not
// record an empty-array override the resolver would read as "intentionally select none".
func nullableJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}
