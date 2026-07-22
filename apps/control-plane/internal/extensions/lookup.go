package extensions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/palgroup/palai/adapters/integrations/mcp"
	remotehttp "github.com/palgroup/palai/adapters/tools/http"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
	"github.com/palgroup/palai/storage"
)

// RemoteInvoker signs and dispatches a tool-http.v1 invoke to a remote tool server (E12 T4).
// *remotehttp.Executor satisfies it; a test fakes it. The extensions store depends only on this narrow
// surface so it stays free of the transport's HTTP/egress mechanics.
type RemoteInvoker interface {
	Invoke(ctx context.Context, in remotehttp.Invocation) (map[string]any, error)
}

// SecretResolver bridges a tool_revision.secret_ref handle to the signing-secret bytes at invoke time
// (the org-scoped file-secret bridge, spec §28.4). It mirrors main.go's webhook/inbound resolvers; the
// bytes are resolved fresh per invoke and never held in the binder closure.
type SecretResolver func(org, ref string) ([]byte, error)

// SetRemoteInvoker wires the remote_http executor + its secret resolver (E12 T4). A nil invoker keeps the
// binder-less behaviour: a remote_http revision is creatable but not resolvable/advertised (the T2
// posture), so existing tests are bit-unchanged.
func (s *Store) SetRemoteInvoker(inv RemoteInvoker, resolver SecretResolver) {
	s.remoteInvoker = inv
	s.remoteSecret = resolver
}

// LookupTool is the broker's per-tenant registry fallback (E12 Task 2, EXT-002): given a run's scope and a
// model-visible short name, it walks the run → pinned revision → tool_sets → published set revision → pins
// → tool_revision chain and, on a hit, builds the broker-loadable tool. It is injected into the broker via
// SetLookup, so a registered tool executes through the SAME fence/ledger machinery without entering the
// global tool map (tenant isolation, no second dispatch loop).
//
// A control_plane row binds the pure echo (T2). A remote_http row binds the signed HTTP executor (T4)
// WHEN an invoker is wired; without one it stays binder-less (returns _, false). An mcp row resolves through
// the mcp branch to a per-call sandboxed Exec closure gated on the run's connection rider (T5).
func (s *Store) LookupTool(ctx context.Context, org, project, runID, name string) (toolbroker.Tool, bool, error) {
	ctx = storage.ScopeToTenant(ctx, org, project)
	var (
		executor       string
		description    string
		inputJSON      []byte
		outputJSON     []byte
		replayClass    string
		configJSON     []byte
		secretRef      *string
		timeoutMS      *int
		canonicalName  string
		revisionNumber int
	)
	err := s.pool.QueryRow(ctx, storage.Query("LookupRunTool"), runID, org, project, name).
		Scan(&executor, &description, &inputJSON, &outputJSON, &replayClass, &configJSON, &secretRef, &timeoutMS, &canonicalName, &revisionNumber)
	if errors.Is(err, pgx.ErrNoRows) {
		return toolbroker.Tool{}, false, nil
	}
	if err != nil {
		return toolbroker.Tool{}, false, fmt.Errorf("lookup registry tool %q: %w", name, err)
	}

	if executor == "mcp" {
		return s.mcpTool(ctx, org, project, runID, name, description, inputJSON, outputJSON, replayClass, configJSON, timeoutMS)
	}

	tool := toolbroker.Tool{
		Name:         name,
		Description:  description,
		InputSchema:  decodeSchema(inputJSON),
		OutputSchema: decodeSchema(outputJSON),
		ReplayClass:  toolbroker.ReplayClass(replayClass),
	}
	switch executor {
	case "control_plane":
		tool.Invoke = echoInvoke
	case "remote_http":
		if s.remoteInvoker == nil || s.remoteSecret == nil {
			return toolbroker.Tool{}, false, nil // binder-less until the T4 executor is wired
		}
		tool.Exec = s.remoteExec(name, canonicalName, revisionNumber, configJSON, secretRef, timeoutMS)
		// A remote_http invoke keys its HTTP Idempotency-Key on the tool_call_id, so its durable pre-write
		// records external_idempotency_key = tool_call_id for reconcile correlation (E12 T4).
		tool.ExternalKeyed = true
	default:
		return toolbroker.Tool{}, false, nil // mcp/etc. — creatable but binder-less (T5)
	}
	return tool, true, nil
}

// remoteExec binds a remote_http revision to the signed HTTP executor. The closure holds only NON-secret
// wiring (URL, self-host flag, revision identity, timeout); the signing secret is resolved fresh per
// invoke through the org-scoped resolver and never captured. The tool_call_id + live fence arrive on
// ExecEnv (broker per-call), so the invoke keys its Idempotency-Key and stamps its operation row.
func (s *Store) remoteExec(name, canonical string, revisionNumber int, configJSON []byte, secretRef *string, timeoutMS *int) func(context.Context, toolbroker.ExecEnv, map[string]any) (map[string]any, error) {
	return func(ctx context.Context, env toolbroker.ExecEnv, args map[string]any) (map[string]any, error) {
		cfg := decodeSchema(configJSON) // executor_config carries only non-secret wiring
		url, _ := cfg["url"].(string)
		allowPrivate, _ := cfg["allow_private"].(bool)
		ref := ""
		if secretRef != nil {
			ref = *secretRef
		}
		if ref == "" {
			return nil, fmt.Errorf("remote_http tool %q has no secret_ref (a signed transport needs a secret)", name)
		}
		secret, err := s.remoteSecret(env.Scope.Org, ref)
		if err != nil {
			return nil, fmt.Errorf("resolve remote tool secret for %q: %w", name, err)
		}
		timeout := 0
		if timeoutMS != nil {
			timeout = *timeoutMS
		}
		return s.remoteInvoker.Invoke(ctx, remotehttp.Invocation{
			URL:          url,
			AllowPrivate: allowPrivate,
			Secret:       secret,
			ToolCallID:   string(env.CallID),
			ToolRevision: fmt.Sprintf("%s@%d", canonical, revisionNumber),
			RunID:        env.Scope.RunID,
			// The fence uniquely identifies the attempt within a run, so run#fence is a valid attempt id
			// without threading the attempt row into ExecEnv (ponytail).
			AttemptID:   fmt.Sprintf("%s#%d", env.Scope.RunID, env.Fence),
			RequestHash: toolbroker.RequestHash(name, args),
			Arguments:   args,
			Org:         env.Scope.Org,
			Project:     env.Scope.Project,
			SecretRef:   ref,
			Fence:       env.Fence,
			TimeoutMS:   timeout,
		})
	}
}

// mcpTool builds the broker-loadable tool for a discovered MCP tool revision. It enforces the capability
// ceiling: the tool resolves ONLY if the run's pinned AgentRevision (or template) mcp_connections rider
// names the connection the revision points at (MCPConnectionForRun) — a connection outside the rider, or
// disabled, or out of tenant, yields (_, false) so the broker returns ErrUnknownTool. The Exec closure calls
// the injected MCP client, which sandboxes the untrusted server per call; the result is output-schema-
// validated data. No MCP client wired ⇒ (_, false) (binder-less posture, never a nil-call).
func (s *Store) mcpTool(ctx context.Context, org, project, runID, name, description string, inputJSON, outputJSON []byte, replayClass string, configJSON []byte, timeoutMS *int) (toolbroker.Tool, bool, error) {
	ctx = storage.ScopeToTenant(ctx, org, project)
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
				Org: env.Scope.Org, Project: env.Scope.Project,
				SessionID: env.Scope.SessionID, ResponseID: env.Scope.ResponseID,
				RunID: env.Scope.RunID, CallID: string(env.CallID),
			}, cc, remoteName, args)
		},
	}
	return tool, true, nil
}

// mcpConnectionForRun loads an enabled connection ONLY when the run's rider names it (the capability-ceiling
// join). A miss is (_, false, nil) — the tool is not resolvable.
func (s *Store) mcpConnectionForRun(ctx context.Context, org, project, runID, connID string) (Connection, bool, error) {
	ctx = storage.ScopeToTenant(ctx, org, project)
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

// echoInvoke is the T2 control_plane binder: a pure identity that returns its arguments unchanged. It is
// the minimal executor that proves a registered tool round-trips through the broker's fence/ledger.
func echoInvoke(args map[string]any) (map[string]any, error) { return args, nil }

// decodeSchema unmarshals a JSONB column (schema or executor_config) into a map, or nil for a NULL/empty
// column (a nil schema imposes no constraint in the broker validator).
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
