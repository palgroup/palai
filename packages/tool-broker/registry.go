package toolbroker

import "context"

// This file is the durable task/todo registry seam behind the in-process broker (spec §11, master
// plan line 410). The broker stays dependency-light: it defines the seam a model-facing registry tool
// needs but owns no persistence. The concrete DB-backed registry (session-scoped, durable, journaled
// so multiple attached clients see ordered updates) lives in the control plane and is injected per
// attempt through ExecEnv.

// TaskScope binds a durable task/todo operation to its tenant, session, and the active run/response
// (a mutation is journaled on the response and guarded against a canceled run). The durable primitives
// are session-scoped (they outlive a run, spec §11); primitive fields keep the broker free of the
// coordinator's tenant type.
type TaskScope struct {
	Org        string
	Project    string
	SessionID  string
	RunID      string
	ResponseID string
}

// TaskRegistry is the durable, session-scoped task/todo store a model-facing registry tool persists
// through. The concrete implementation lives outside this package (the control plane, DB-backed); the
// seam keeps the broker free of persistence. ApplyTask interprets one operation (add/update/list) and
// returns the tool result — the CURRENT durable state — so a context-reset attempt reads what is done
// and what is not straight from the store (REG-001). A nil registry makes a registry tool fail cleanly.
type TaskRegistry interface {
	ApplyTask(ctx context.Context, scope TaskScope, op map[string]any) (map[string]any, error)
}
