//go:build live

// CASE=mcp-tool-roundtrip (E12 Task 5, TOL-008/EXT-006): an MCP connection to a REAL stdio server (the
// fixture, run inside a hardened, network-less OCI container) is discovered into a connection-namespaced
// registry tool, published + pinned into a set a run's AgentRevision names (with the connection in its
// mcp_connections rider), advertised to a REAL provider-one run, and — when the model SPONTANEOUSLY calls
// it (no tool_choice) — executed through the broker's per-tenant lookup, which sandboxes the untrusted
// server per call and returns its result through the fenced path.
//
// HONEST CEILING (mandatory, spec §10.2; brief §6): this proves connection→discover→publish→pin→rider →
// advertised → SPONTANEOUS model call → OCI-sandboxed MCP execution → canonical fenced completion. It does
// NOT claim the "result → model CONTINUES" half: that threaded continuation needs T1b (engine-wire
// tool_call-id), which is not merged — the engine-wire ToolCall carries name+arguments only, so a provider
// tool_call id is dropped in toEngineToolCalls and a re-threaded assistant tool_call turn is broken for the
// real chat API (the spontaneous-tool-roundtrip / registry-tool-roundtrip precedent). Re-run for the
// continuation half once T1b merges (T10 regression). Single provider; the MCP server is our fixture (a
// third-party live server is the T10 journey). The credential is an opaque leak-scan needle, never printed.

package live

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	mcpclient "github.com/palgroup/palai/adapters/integrations/mcp"
	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	"github.com/palgroup/palai/adapters/sandboxes/oci"
	"github.com/palgroup/palai/apps/control-plane/internal/extensions"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

const mcpConnName = "fixture"
const mcpEchoShortName = mcpConnName + "__echo" // the model-visible name discovery mints

// TestLiveMCPToolRoundtripSpontaneous discovers a real stdio MCP server's echo tool, advertises it to a real
// provider run, and executes the model's SPONTANEOUS call through the per-tenant lookup into an OCI-sandboxed
// server — the connection → discover → publish → pin → rider → spontaneous → sandboxed-execution round-trip.
func TestLiveMCPToolRoundtripSpontaneous(t *testing.T) {
	secret := os.Getenv(credentialEnv)
	if secret == "" {
		t.Fatalf("%s is unset; the live tier loads it from .env.local at runtime", credentialEnv)
	}
	fixtureImage := os.Getenv("PALAI_MCP_FIXTURE_IMAGE_ID")
	if fixtureImage == "" {
		t.Skip("PALAI_MCP_FIXTURE_IMAGE_ID is required; run make test-live-provider PROVIDER=provider-one CASE=mcp-tool-roundtrip")
	}
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Fatalf("PALAI_COMPONENT_POSTGRES_URL is unset; the CASE runs under run_live_with_mcp")
	}
	ctx := context.Background()

	cs, err := coordinator.Open(ctx, url)
	if err != nil {
		t.Fatalf("coordinator.Open: %v", err)
	}
	t.Cleanup(cs.Close)
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool := cs.Pool()

	org, project := liveID("org"), liveID("prj")
	sessionID, runID := liveID("ses"), liveID("run")
	profileID, arevID := liveID("aprof"), liveID("arev")
	execLive(t, pool, `INSERT INTO organizations (id) VALUES ($1)`, org)
	execLive(t, pool, `INSERT INTO projects (id, organization_id) VALUES ($1,$2)`, project, org)
	execLive(t, pool, `INSERT INTO sessions (id, organization_id, project_id) VALUES ($1,$2,$3)`, sessionID, org, project)
	execLive(t, pool, `INSERT INTO agent_profiles (id, organization_id, project_id, name) VALUES ($1,$2,$3,$4)`, profileID, org, project, profileID)

	// A REAL MCP manager: the stdio transport runs the fixture in a hardened, network-less OCI container.
	driver, err := oci.NewDockerInteractiveDriver()
	if err != nil {
		t.Fatalf("docker interactive driver: %v", err)
	}
	manager := mcpclient.NewManager(mcpclient.Config{
		Driver:         driver,
		DefaultTimeout: 20 * time.Second,
		Limits:         oci.Limits{WallTime: 20 * time.Second, MaxMemoryBytes: 256 << 20, MaxProcessCount: 32, NanoCPUs: 1_000_000_000},
	})

	reg := extensions.New(pool)
	reg.SetMCP(manager)

	// Register the connection, discover its tools (draft revisions), publish echo, pin it into a set the
	// agent revision names, and put the connection in the revision's mcp_connections rider.
	connBody := []byte(`{"name":"` + mcpConnName + `","transport":"stdio","config":{"image_digest":"` + fixtureImage + `","cmd":["/mcp"]}}`)
	conn, err := reg.CreateMCPConnection(ctx, org, project, connBody)
	if err != nil {
		t.Fatalf("create connection: %v", err)
	}
	if _, err := reg.DiscoverConnection(ctx, org, project, conn.ID); err != nil {
		t.Fatalf("discover connection: %v", err)
	}
	revID := latestMCPRevisionID(t, pool, org, project, "mcp."+mcpConnName+".echo")
	if _, _, err := reg.PublishToolRevision(ctx, org, project, revID); err != nil {
		t.Fatalf("publish echo revision: %v", err)
	}
	set, err := reg.CreateToolSetRevision(ctx, org, project, "mcptools", []byte(`{"tools":[{"tool_revision_id":"`+revID+`"}]}`))
	if err != nil {
		t.Fatalf("create set: %v", err)
	}
	if _, _, err := reg.PublishToolSetRevision(ctx, org, project, set.ID); err != nil {
		t.Fatalf("publish set: %v", err)
	}
	execLive(t, pool, `INSERT INTO agent_revisions (id, organization_id, project_id, profile_id, revision_number, model, published_at, tool_sets, mcp_connections)
	                   VALUES ($1,$2,$3,$4,1,$5,clock_timestamp(),$6::jsonb,$7::jsonb)`,
		arevID, org, project, profileID, liveModel(), `["`+set.ID+`"]`, `["`+conn.ID+`"]`)
	execLive(t, pool, `INSERT INTO runs (id, organization_id, project_id, session_id, agent_revision_id) VALUES ($1,$2,$3,$4,$5)`, runID, org, project, sessionID, arevID)

	// A REAL provider run that ADVERTISES the discovered MCP tool and lets the model choose SPONTANEOUSLY
	// (no ForceToolCall — the E12 T1 advertising path this task's SchemaResolved fallback makes possible).
	req := modelbroker.Request{
		ModelRequestID: contracts.ModelRequestID("mreq_mcp_tool_roundtrip"),
		RouteRevision:  1, ModelStepID: "step-mcp", Model: liveModel(),
		Messages: []modelbroker.Message{
			{Role: "user", Content: "Use the " + mcpEchoShortName + " tool to echo the message \"hello mcp\". Call the tool."},
		},
		Tools:       []modelbroker.ToolSchema{mcpEchoToolSchema()},
		Deadline:    time.Now().Add(60 * time.Second),
		Reservation: modelbroker.Reservation{MaxTotalTokens: 2000},
		Secret:      modelbroker.SecretRef("provider-one"),
	}
	mb := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"provider-one": providerone.Adapter{}},
		Secrets:  modelbroker.EnvResolver{"provider-one": credentialEnv},
	})
	res, err := mb.Route(ctx, "provider-one", req, func(modelbroker.Delta) {})
	if err != nil {
		t.Fatalf("route mcp-tool turn: %v", err)
	}
	assertRealCompletion(t, res, "mcp-tool-roundtrip (spontaneous)")
	call := requireToolCall(t, res, "mcp-tool-roundtrip (spontaneous)")
	if call.Name != mcpEchoShortName {
		t.Fatalf("spontaneous call = %q, want the advertised MCP tool %q", call.Name, mcpEchoShortName)
	}

	// Execute the SPONTANEOUS call through the broker's per-tenant lookup — routed to the real, sandboxed
	// MCP server via the connection rider, and completed through the fenced path (canonical completion).
	tb := toolbroker.New()
	tb.SetLookup(func(ctx context.Context, env toolbroker.ExecEnv, name string) (toolbroker.Tool, bool, error) {
		return reg.LookupTool(ctx, env.Scope.Org, env.Scope.Project, env.Scope.RunID, name)
	})
	var args map[string]any
	if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil || args == nil {
		args = map[string]any{"message": "hello mcp"}
	}
	callID := call.ID
	if callID == "" {
		callID = "tc_live_mcp_echo"
	}
	env := toolbroker.ExecEnv{Scope: toolbroker.TaskScope{Org: org, Project: project, SessionID: sessionID, RunID: runID}}
	out, err := tb.Execute(ctx, contracts.ToolCallID(callID), mcpEchoShortName, args, 1, env)
	if err != nil {
		t.Fatalf("execute the MCP tool through the sandboxed lookup: %v", err)
	}
	if out.State != "completed" {
		t.Fatalf("mcp tool state = %q, want completed (fenced round-trip)", out.State)
	}
	if out.Result["echo"] == nil {
		t.Fatalf("mcp echo result carries no echo field: %v (the sandboxed server did not run)", out.Result)
	}

	t.Logf("live mcp-tool round-trip PASS (real provider SPONTANEOUS call → OCI-sandboxed MCP server → fenced completion; "+
		"model-continues NOT claimed, needs T1b): completion=%s… tool=%s echo=%v model=%s",
		safePrefix(res.ProviderRequestID), call.Name, out.Result["echo"], res.Model)
}

// latestMCPRevisionID reads the newest revision id for a canonical tool name (tenant-scoped).
func latestMCPRevisionID(t *testing.T, pool *pgxpool.Pool, org, project, canonical string) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(),
		`SELECT tr.id FROM tools t JOIN tool_revisions tr ON tr.tool_id=t.id
		 WHERE t.canonical_name=$1 AND t.organization_id=$2 AND t.project_id=$3
		 ORDER BY tr.revision_number DESC LIMIT 1`, canonical, org, project).Scan(&id)
	if err != nil {
		t.Fatalf("read latest revision id for %s: %v", canonical, err)
	}
	return id
}

// mcpEchoToolSchema advertises the discovered MCP echo tool's model-visible name + input shape so the real
// provider can spontaneously choose it.
func mcpEchoToolSchema() modelbroker.ToolSchema {
	return modelbroker.ToolSchema{
		Name:        mcpEchoShortName,
		Description: "Echo a message back via the connected MCP server.",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"message": map[string]any{"type": "string"}},
			"required":   []any{"message"},
		},
	}
}
