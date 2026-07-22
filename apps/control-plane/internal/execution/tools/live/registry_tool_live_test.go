//go:build live

// CASE=registry-tool-roundtrip (E12 Task 2, EXT-002/EXT-003): a control_plane echo tool REGISTERED in
// the durable extensibility registry, pinned into a published ToolSetRevision that a run's AgentRevision
// names in its tool_sets, is resolved through the broker's per-tenant lookup and executed through the SAME
// fenced ledger path — driven by a REAL provider-one forced tool call. This proves the registry → broker
// load round-trip on a live stack (real Postgres registry rows + a genuine chatcmpl completion).
//
// HONEST CEILING (mandatory, spec §10.2; brief §6 forced-seam, PRE-T1): T1 (tool advertising) is NOT
// merged, so the provider does not SPONTANEOUSLY choose the registered tool — the call is FORCED
// (tool_choice:required), the E09 T4 broker-seam pattern. The "advertised registry face + a second
// unpinned tool never offered" half is proven deterministically in the component tier
// (TestRegistryToolsLoadIntoBrokerEffectiveSet); re-run this case after T1 merges for the spontaneous +
// advertising half (T10 regression makes it mandatory). Single provider (provider-one); the echo executor
// is PURE — no remote_http / mcp execution is claimed (those kinds are creatable but binder-less in T2).
// The credential is an opaque needle for the leak scan and is never printed.

package live

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	"github.com/palgroup/palai/apps/control-plane/internal/extensions"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"

	"github.com/palgroup/palai/storage"
)

const registryEchoShortName = "fetch"

// TestLiveRegistryToolRoundtripForcedPreT1 registers a control_plane echo tool, pins it into a published
// set a run's agent revision names, forces a real provider tool call for it, and executes that call
// through the broker's per-tenant registry lookup — the registry → broker → fenced-ledger round-trip.
func TestLiveRegistryToolRoundtripForcedPreT1(t *testing.T) {
	secret := os.Getenv(credentialEnv)
	if secret == "" {
		t.Fatalf("%s is unset; the live tier loads it from .env.local at runtime", credentialEnv)
	}
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Fatalf("PALAI_COMPONENT_POSTGRES_URL is unset; the CASE runs under run_live_with_postgres")
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
	execLive(t, pool, `INSERT INTO agent_profiles (id, organization_id, project_id, name) VALUES ($1,$2,$3,'reviewer')`, profileID, org, project)

	// Register the echo tool + a published control_plane revision, and a published set that pins it.
	reg := extensions.New(pool)
	tool, err := reg.CreateTool(ctx, org, project, "acme.search."+registryEchoShortName)
	if err != nil {
		t.Fatalf("create tool: %v", err)
	}
	rev, err := reg.CreateToolRevision(ctx, org, project, tool.ID, []byte(`{"executor":"control_plane","input_schema":{"type":"object"},"replay_class":"pure"}`))
	if err != nil {
		t.Fatalf("create tool revision: %v", err)
	}
	if _, _, err := reg.PublishToolRevision(ctx, org, project, rev.ID); err != nil {
		t.Fatalf("publish tool revision: %v", err)
	}
	set, err := reg.CreateToolSetRevision(ctx, org, project, "reviewers", []byte(`{"tools":[{"tool_revision_id":"`+rev.ID+`"}]}`))
	if err != nil {
		t.Fatalf("create set revision: %v", err)
	}
	if _, _, err := reg.PublishToolSetRevision(ctx, org, project, set.ID); err != nil {
		t.Fatalf("publish set revision: %v", err)
	}
	// The run pins an agent revision naming the published set in tool_sets.
	execLive(t, pool, `INSERT INTO agent_revisions (id, organization_id, project_id, profile_id, revision_number, model, published_at, tool_sets)
	                   VALUES ($1,$2,$3,$4,1,$5,clock_timestamp(),$6::jsonb)`, arevID, org, project, profileID, liveModel(), `["`+set.ID+`"]`)
	execLive(t, pool, `INSERT INTO runs (id, organization_id, project_id, session_id, agent_revision_id) VALUES ($1,$2,$3,$4,$5)`, runID, org, project, sessionID, arevID)

	// A REAL provider forced tool call for the registered short name (the live element).
	req := modelbroker.Request{
		ModelRequestID: contracts.ModelRequestID("mreq_registry_tool_roundtrip"),
		RouteRevision:  1, ModelStepID: "step-registry", Model: liveModel(),
		Messages:      []modelbroker.Message{{Role: "user", Content: "Call the fetch tool with query \"hello\"."}},
		Tools:         []modelbroker.ToolSchema{echoToolSchema()},
		ForceToolCall: true,
		Deadline:      time.Now().Add(60 * time.Second),
		Reservation:   modelbroker.Reservation{MaxTotalTokens: 2000},
		Secret:        modelbroker.SecretRef("provider-one"),
	}
	mb := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"provider-one": providerone.Adapter{}},
		Secrets:  modelbroker.EnvResolver{"provider-one": credentialEnv},
	})
	res, err := mb.Route(ctx, "provider-one", req, func(modelbroker.Delta) {})
	if err != nil {
		t.Fatalf("route registry-tool turn: %v", err)
	}
	assertRealCompletion(t, res, "registry-tool-roundtrip (forced, pre-T1)")
	call := requireToolCall(t, res, "registry-tool-roundtrip (forced, pre-T1)")
	if call.Name != registryEchoShortName {
		t.Fatalf("forced call = %q, want the registered tool %q", call.Name, registryEchoShortName)
	}

	// Execute the forced call through the broker's per-tenant registry lookup — the fenced round-trip.
	tb := toolbroker.New()
	tb.SetLookup(func(ctx context.Context, env toolbroker.ExecEnv, name string) (toolbroker.Tool, bool, error) {
		return reg.LookupTool(ctx, env.Scope.Org, env.Scope.Project, env.Scope.RunID, name)
	})
	var args map[string]any
	if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil || args == nil {
		args = map[string]any{"query": "hello"} // the model's args echo back regardless; a safe default
	}
	callID := call.ID
	if callID == "" {
		callID = "tc_live_fetch"
	}
	env := toolbroker.ExecEnv{Scope: toolbroker.TaskScope{Org: org, Project: project, RunID: runID}}
	out, err := tb.Execute(ctx, contracts.ToolCallID(callID), registryEchoShortName, args, 1, env)
	if err != nil {
		t.Fatalf("execute registered echo through the broker lookup: %v", err)
	}
	if out.State != "completed" {
		t.Fatalf("echo tool state = %q, want completed (fenced round-trip)", out.State)
	}
	// The unpinned name never resolves through the run's lookup (registry face negative half).
	if _, err := tb.Execute(ctx, contracts.ToolCallID("tc_live_unpinned"), "not-registered", map[string]any{}, 2, env); err == nil {
		t.Fatal("an unregistered tool resolved through the lookup, want ErrUnknownTool")
	}
}

// echoToolSchema advertises the registered echo tool's short name so the forced provider call names it.
func echoToolSchema() modelbroker.ToolSchema {
	return modelbroker.ToolSchema{
		Name:        registryEchoShortName,
		Description: "Echo a query back. Registered control_plane tool.",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"query": map[string]any{"type": "string"}},
			"required":   []any{"query"},
		},
	}
}

func liveID(prefix string) string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return prefix + "_" + hex.EncodeToString(raw[:])
}

func execLive(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(storage.WithSystemScope(context.Background()), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}
