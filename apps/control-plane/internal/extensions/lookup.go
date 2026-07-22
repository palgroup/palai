package extensions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	toolbroker "github.com/palgroup/palai/packages/tool-broker"
	"github.com/palgroup/palai/storage"
)

// LookupTool is the broker's per-tenant registry fallback (E12 Task 2, EXT-002): given a run's scope and a
// model-visible short name, it walks the run → pinned revision → tool_sets → published set revision → pins
// → tool_revision chain and, on a hit, builds the broker-loadable tool. It is injected into the broker via
// SetLookup, so a registered tool executes through the SAME fence/ledger machinery without entering the
// global tool map (tenant isolation, no second dispatch loop).
//
// executorBinders is the executor→binder map: T2 ships ONLY the control_plane binder (a pure echo). A
// remote_http / mcp row resolves to a real tool_revision here but has no binder yet, so it returns (_,
// false) — creatable but not executable/advertised until T4/T5 add its binder (no dead advertisement).
func (s *Store) LookupTool(ctx context.Context, org, project, runID, name string) (toolbroker.Tool, bool, error) {
	var (
		executor    string
		inputJSON   []byte
		outputJSON  []byte
		replayClass string
	)
	err := s.pool.QueryRow(ctx, storage.Query("LookupRunTool"), runID, org, project, name).
		Scan(&executor, &inputJSON, &outputJSON, &replayClass)
	if errors.Is(err, pgx.ErrNoRows) {
		return toolbroker.Tool{}, false, nil
	}
	if err != nil {
		return toolbroker.Tool{}, false, fmt.Errorf("lookup registry tool %q: %w", name, err)
	}
	binder, ok := executorBinders[executor]
	if !ok {
		return toolbroker.Tool{}, false, nil // creatable but binder-less (remote_http/mcp arrive in T4/T5)
	}
	tool := toolbroker.Tool{
		Name:         name,
		InputSchema:  decodeSchema(inputJSON),
		OutputSchema: decodeSchema(outputJSON),
		ReplayClass:  toolbroker.ReplayClass(replayClass),
		Invoke:       binder,
	}
	return tool, true, nil
}

// executorBinders maps a tool_revision executor kind to its in-process invoke surface. Only control_plane
// has one in T2 — the platform-defined pure echo (deterministic, no side effect), the load-into-broker
// proof. T4/T5 add remote_http / mcp binders here.
var executorBinders = map[string]func(args map[string]any) (map[string]any, error){
	"control_plane": echoInvoke,
}

// echoInvoke is the T2 control_plane binder: a pure identity that returns its arguments unchanged. It is
// the minimal executor that proves a registered tool round-trips through the broker's fence/ledger.
func echoInvoke(args map[string]any) (map[string]any, error) { return args, nil }

// decodeSchema unmarshals a JSONB schema column into a map, or nil for a NULL/empty column (a nil schema
// imposes no constraint in the broker validator).
func decodeSchema(raw []byte) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}
