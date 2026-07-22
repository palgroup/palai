package extensions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/palgroup/palai/adapters/integrations/mcp"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
	"github.com/palgroup/palai/storage"
)

// LookupTool is the broker's per-tenant registry fallback (E12 Task 2, EXT-002): given a run's scope and a
// model-visible short name, it walks the run → pinned revision → tool_sets → published set revision → pins
// → tool_revision chain and, on a hit, builds the broker-loadable tool. It is injected into the broker via
// SetLookup, so a registered tool executes through the SAME fence/ledger machinery without entering the
// global tool map (tenant isolation, no second dispatch loop).
//
// executorBinders is the executor→binder map: control_plane ships a pure echo. An mcp row resolves through
// the mcp branch below to a per-call Exec closure gated on the run's connection rider; a remote_http row is
// still binder-less (T4), so it returns (_, false) — creatable but not executable/advertised.
func (s *Store) LookupTool(ctx context.Context, org, project, runID, name string) (toolbroker.Tool, bool, error) {
	var (
		executor    string
		description string
		inputJSON   []byte
		outputJSON  []byte
		replayClass string
		configJSON  []byte
		secretRef   *string
		timeoutMS   *int
	)
	err := s.pool.QueryRow(ctx, storage.Query("LookupRunTool"), runID, org, project, name).
		Scan(&executor, &description, &inputJSON, &outputJSON, &replayClass, &configJSON, &secretRef, &timeoutMS)
	if errors.Is(err, pgx.ErrNoRows) {
		return toolbroker.Tool{}, false, nil
	}
	if err != nil {
		return toolbroker.Tool{}, false, fmt.Errorf("lookup registry tool %q: %w", name, err)
	}

	if executor == "mcp" {
		return s.mcpTool(ctx, org, project, runID, name, description, inputJSON, outputJSON, replayClass, configJSON, timeoutMS)
	}

	binder, ok := executorBinders[executor]
	if !ok {
		return toolbroker.Tool{}, false, nil // creatable but binder-less (remote_http arrives in T4)
	}
	tool := toolbroker.Tool{
		Name:         name,
		Description:  description,
		InputSchema:  decodeSchema(inputJSON),
		OutputSchema: decodeSchema(outputJSON),
		ReplayClass:  toolbroker.ReplayClass(replayClass),
		Invoke:       binder,
	}
	return tool, true, nil
}

// mcpTool builds the broker-loadable tool for a discovered MCP tool revision. It enforces the capability
// ceiling: the tool resolves ONLY if the run's pinned AgentRevision (or template) mcp_connections rider
// names the connection the revision points at (MCPConnectionForRun) — a connection outside the rider, or
// disabled, or out of tenant, yields (_, false) so the broker returns ErrUnknownTool. The Exec closure calls
// the injected MCP client, which sandboxes the untrusted server per call; the result is output-schema-
// validated data. No MCP client wired ⇒ (_, false) (binder-less posture, never a nil-call).
func (s *Store) mcpTool(ctx context.Context, org, project, runID, name, description string, inputJSON, outputJSON []byte, replayClass string, configJSON []byte, timeoutMS *int) (toolbroker.Tool, bool, error) {
	if s.mcp == nil {
		return toolbroker.Tool{}, false, nil
	}
	var execCfg struct {
		ConnectionID string `json:"connection_id"`
		RemoteName   string `json:"remote_name"`
	}
	if err := json.Unmarshal(configJSON, &execCfg); err != nil || execCfg.ConnectionID == "" || execCfg.RemoteName == "" {
		return toolbroker.Tool{}, false, nil // a malformed mcp revision is not resolvable (never a panic)
	}

	conn, found, err := s.mcpConnectionForRun(ctx, org, project, runID, execCfg.ConnectionID)
	if err != nil {
		return toolbroker.Tool{}, false, err
	}
	if !found {
		// The connection is not in the run's rider (or is disabled / out of tenant) — capability ceiling.
		return toolbroker.Tool{}, false, nil
	}
	cc := connConfig(org, conn)
	if timeoutMS != nil {
		cc.TimeoutMS = *timeoutMS
	}
	remoteName := execCfg.RemoteName
	tool := toolbroker.Tool{
		Name:         name,
		Description:  description,
		InputSchema:  decodeSchema(inputJSON),
		OutputSchema: decodeSchema(outputJSON),
		ReplayClass:  toolbroker.ReplayClass(replayClass),
		Exec: func(ctx context.Context, env toolbroker.ExecEnv, args map[string]any) (map[string]any, error) {
			return s.mcp.Call(ctx, mcp.CallScope{
				Org: env.Scope.Org, Project: env.Scope.Project, RunID: env.Scope.RunID, CallID: env.CallID,
			}, cc, remoteName, args)
		},
	}
	return tool, true, nil
}

// mcpConnectionForRun loads an enabled connection ONLY when the run's rider names it (the capability-ceiling
// join). A miss is (_, false, nil) — the tool is not resolvable.
func (s *Store) mcpConnectionForRun(ctx context.Context, org, project, runID, connID string) (Connection, bool, error) {
	c := Connection{}
	var configJSON []byte
	var secretRef *string
	switch err := s.pool.QueryRow(ctx, storage.Query("MCPConnectionForRun"), runID, org, project, connID).
		Scan(&c.ID, &c.Name, &c.Transport, &configJSON, &secretRef, &c.TrustLevel); {
	case errors.Is(err, pgx.ErrNoRows):
		return Connection{}, false, nil
	case err != nil:
		return Connection{}, false, fmt.Errorf("resolve run mcp connection: %w", err)
	}
	c.Config = decodeSchema(configJSON)
	if secretRef != nil {
		c.SecretRef = *secretRef
	}
	return c, true, nil
}

// executorBinders maps a tool_revision executor kind to its in-process invoke surface. control_plane is the
// platform-defined pure echo (deterministic, no side effect), the load-into-broker proof. mcp is handled in
// its own branch (a per-call sandboxed Exec, not a pure func); remote_http arrives in T4.
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
