//go:build component

package extensions

import (
	"context"
	"errors"
	"testing"

	"github.com/palgroup/palai/adapters/integrations/mcp"
	"github.com/palgroup/palai/packages/contracts"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// fakeMCP is a deterministic MCP client double: Discover returns a canned tool list, Call returns a canned
// data-only result echoing the connection + remote name it was routed to. It proves the DISPATCH + DISCOVERY
// chains without a real container (the container isolation is the stdio component tier's proof).
type fakeMCP struct {
	tools      []mcp.RemoteTool
	calls      int
	lastConn   mcp.ConnConfig
	lastRemote string
	lastScope  mcp.CallScope
}

func (f *fakeMCP) Discover(_ context.Context, conn mcp.ConnConfig) ([]mcp.RemoteTool, error) {
	f.lastConn = conn
	return f.tools, nil
}

func (f *fakeMCP) Call(_ context.Context, scope mcp.CallScope, conn mcp.ConnConfig, remoteName string, args map[string]any) (map[string]any, error) {
	f.calls++
	f.lastConn = conn
	f.lastRemote = remoteName
	f.lastScope = scope
	return map[string]any{"echoed": args["message"], "conn": conn.ID, "remote": remoteName}, nil
}

// createStdioConnection registers a stdio connection with the given name and returns its id.
func createStdioConnection(t *testing.T, s *Store, org, project, name string) string {
	t.Helper()
	body := []byte(`{"name":"` + name + `","transport":"stdio","config":{"image_digest":"sha256:` + hex64() + `","cmd":["/mcp"]}}`)
	conn, err := s.CreateMCPConnection(context.Background(), org, project, body)
	if err != nil {
		t.Fatalf("create connection %s: %v", name, err)
	}
	return conn.ID
}

// latestRevisionID reads the newest revision id for a canonical tool name (a discovery materialises one).
func latestRevisionID(t *testing.T, s *Store, canonical string) string {
	t.Helper()
	var id string
	err := s.pool.QueryRow(context.Background(),
		`SELECT tr.id FROM tools t JOIN tool_revisions tr ON tr.tool_id=t.id
		 WHERE t.canonical_name=$1 ORDER BY tr.revision_number DESC LIMIT 1`, canonical).Scan(&id)
	if err != nil {
		t.Fatalf("read latest revision id for %s: %v", canonical, err)
	}
	return id
}

// seedRunWithMCPRider seeds a session + agent revision (tool_sets=[setID], mcp_connections=riders) + run.
func seedRunWithMCPRider(t *testing.T, s *Store, org, project, setID string, riders string) string {
	t.Helper()
	sessionID, runID := testID("ses"), testID("run")
	profileID, arevID := testID("aprof"), testID("arev")
	mustExec(t, s.pool, `INSERT INTO sessions (id, organization_id, project_id) VALUES ($1,$2,$3)`, sessionID, org, project)
	mustExec(t, s.pool, `INSERT INTO agent_profiles (id, organization_id, project_id, name) VALUES ($1,$2,$3,'reviewer')`, profileID, org, project)
	mustExec(t, s.pool, `INSERT INTO agent_revisions (id, organization_id, project_id, profile_id, revision_number, model, published_at, tool_sets, mcp_connections)
	                     VALUES ($1,$2,$3,$4,1,'model-x',clock_timestamp(),$5::jsonb,$6::jsonb)`, arevID, org, project, profileID, `["`+setID+`"]`, riders)
	mustExec(t, s.pool, `INSERT INTO runs (id, organization_id, project_id, session_id, agent_revision_id) VALUES ($1,$2,$3,$4,$5)`, runID, org, project, sessionID, arevID)
	return runID
}

// publishDiscoveredIntoSet discovers the connection, publishes the given canonical tool's revision, pins it
// into a published set, and returns the set id.
func publishDiscoveredIntoSet(t *testing.T, s *Store, org, project, connID, canonical string) string {
	t.Helper()
	ctx := context.Background()
	if _, err := s.DiscoverConnection(ctx, org, project, connID); err != nil {
		t.Fatalf("discover: %v", err)
	}
	revID := latestRevisionID(t, s, canonical)
	if _, _, err := s.PublishToolRevision(ctx, org, project, revID); err != nil {
		t.Fatalf("publish discovered revision: %v", err)
	}
	set, err := s.CreateToolSetRevision(ctx, org, project, "mcpset", pins(revID, nil))
	if err != nil {
		t.Fatalf("create set: %v", err)
	}
	if _, _, err := s.PublishToolSetRevision(ctx, org, project, set.ID); err != nil {
		t.Fatalf("publish set: %v", err)
	}
	return set.ID
}

// brokerWithLookup builds a broker whose registry lookup is this store's per-tenant LookupTool.
func brokerWithLookup(s *Store) *toolbroker.Broker {
	broker := toolbroker.New()
	broker.SetLookup(func(ctx context.Context, env toolbroker.ExecEnv, name string) (toolbroker.Tool, bool, error) {
		return s.LookupTool(ctx, env.Scope.Org, env.Scope.Project, env.Scope.RunID, name)
	})
	return broker
}

// TestMCPCallFlowsThroughStandardDispatch proves TOL-008's dispatch face: a published + pinned MCP tool,
// named by a run whose AgentRevision rider includes the connection, resolves through the per-tenant lookup
// and executes via the SAME broker fence/ledger path (no second dispatch loop) — routed to the right
// connection + remote name, with the tool_call_id threaded into the call scope. A run whose rider does NOT
// include the connection cannot resolve it (capability ceiling): ErrUnknownTool.
func TestMCPCallFlowsThroughStandardDispatch(t *testing.T) {
	s, org, project := openStore(t)
	ctx := context.Background()
	fake := &fakeMCP{tools: []mcp.RemoteTool{{Name: "echo", Description: "echoes", InputSchema: map[string]any{"type": "object"}}}}
	s.SetMCP(fake)

	connID := createStdioConnection(t, s, org, project, "docs")
	setID := publishDiscoveredIntoSet(t, s, org, project, connID, "mcp.docs.echo")

	// A run whose rider INCLUDES the connection resolves + executes the tool through the fenced broker path.
	runID := seedRunWithMCPRider(t, s, org, project, setID, `["`+connID+`"]`)
	broker := brokerWithLookup(s)
	env := toolbroker.ExecEnv{Scope: toolbroker.TaskScope{Org: org, Project: project, RunID: runID}}
	out, err := broker.Execute(ctx, contracts.ToolCallID("tc_mcp1"), "docs__echo", map[string]any{"message": "hi"}, 1, env)
	if err != nil {
		t.Fatalf("dispatch mcp tool: %v", err)
	}
	if out.Result["conn"] != connID || out.Result["remote"] != "echo" {
		t.Fatalf("mcp call routed to conn=%v remote=%v, want %s/echo", out.Result["conn"], out.Result["remote"], connID)
	}
	if fake.lastScope.CallID != "tc_mcp1" {
		t.Fatalf("call scope CallID = %q, want the dispatched tool_call_id tc_mcp1", fake.lastScope.CallID)
	}

	// A run whose rider is EMPTY cannot resolve the same tool — the connection is outside its ceiling.
	runNoRider := seedRunWithMCPRider(t, s, org, project, setID, `[]`)
	envNo := toolbroker.ExecEnv{Scope: toolbroker.TaskScope{Org: org, Project: project, RunID: runNoRider}}
	if _, err := broker.Execute(ctx, contracts.ToolCallID("tc_mcp2"), "docs__echo", map[string]any{}, 1, envNo); !errors.Is(err, toolbroker.ErrUnknownTool) {
		t.Fatalf("rider-excluded mcp tool err = %v, want ErrUnknownTool (capability ceiling)", err)
	}
}

// TestDiscoveredToolsNamespacedByConnectionNoCollision proves TOL-008 namespacing: two connections that each
// expose a `search` tool materialise as two DISTINCT lineages — canonical mcp.<conn>.search, model-visible
// <conn>__search — so neither collides, and both resolve to their OWN connection.
func TestDiscoveredToolsNamespacedByConnectionNoCollision(t *testing.T) {
	s, org, project := openStore(t)
	ctx := context.Background()
	fake := &fakeMCP{tools: []mcp.RemoteTool{{Name: "search", InputSchema: map[string]any{"type": "object"}}}}
	s.SetMCP(fake)

	connA := createStdioConnection(t, s, org, project, "alpha")
	connB := createStdioConnection(t, s, org, project, "beta")
	setA := publishDiscoveredIntoSet(t, s, org, project, connA, "mcp.alpha.search")
	setB := publishDiscoveredIntoSet(t, s, org, project, connB, "mcp.beta.search")

	// Each run pins its own connection's set + rider and resolves ITS namespaced tool.
	runA := seedRunWithMCPRider(t, s, org, project, setA, `["`+connA+`"]`)
	if tool, found, err := s.LookupTool(ctx, org, project, runA, "alpha__search"); err != nil || !found {
		t.Fatalf("alpha__search resolve found=%v err=%v, want a hit", found, err)
	} else if tool.Name != "alpha__search" {
		t.Fatalf("resolved tool = %q, want alpha__search", tool.Name)
	}
	runB := seedRunWithMCPRider(t, s, org, project, setB, `["`+connB+`"]`)
	if _, found, err := s.LookupTool(ctx, org, project, runB, "beta__search"); err != nil || !found {
		t.Fatalf("beta__search resolve found=%v err=%v, want a hit", found, err)
	}
	// alpha's run cannot see beta's namespaced tool (distinct model-visible names, distinct pins).
	if _, found, _ := s.LookupTool(ctx, org, project, runA, "beta__search"); found {
		t.Fatal("alpha run resolved beta__search — namespacing failed")
	}
}

// TestAnnotationChangeRequiresNewRevisionAndReapproval proves EXT-006: re-discovery with an UNCHANGED tool
// writes no new revision (no churn); a CHANGED description produces a NEW draft revision while the published
// one stays published — so the changed (untrusted) description is not advertised until re-approved.
func TestAnnotationChangeRequiresNewRevisionAndReapproval(t *testing.T) {
	s, org, project := openStore(t)
	ctx := context.Background()
	fake := &fakeMCP{tools: []mcp.RemoteTool{{Name: "echo", Description: "v1 desc", InputSchema: map[string]any{"type": "object"}}}}
	s.SetMCP(fake)
	connID := createStdioConnection(t, s, org, project, "docs")

	first, err := s.DiscoverConnection(ctx, org, project, connID)
	if err != nil || len(first.NewRevisions) != 1 {
		t.Fatalf("first discover = %+v err=%v, want one new revision", first, err)
	}
	rev1 := latestRevisionID(t, s, "mcp.docs.echo")
	if _, _, err := s.PublishToolRevision(ctx, org, project, rev1); err != nil {
		t.Fatalf("publish v1: %v", err)
	}

	// Re-discovery with the SAME description writes nothing (idempotent — no manifest churn).
	again, err := s.DiscoverConnection(ctx, org, project, connID)
	if err != nil || len(again.NewRevisions) != 0 || len(again.Unchanged) != 1 {
		t.Fatalf("re-discover unchanged = %+v err=%v, want zero new, one unchanged", again, err)
	}

	// A changed (untrusted) description on re-discovery is a NEW DRAFT; the published v1 stays published.
	fake.tools[0].Description = "v2 desc — now asks for more"
	changed, err := s.DiscoverConnection(ctx, org, project, connID)
	if err != nil || len(changed.NewRevisions) != 1 {
		t.Fatalf("re-discover changed = %+v err=%v, want one new draft revision", changed, err)
	}
	rev2 := latestRevisionID(t, s, "mcp.docs.echo")
	if rev2 == rev1 {
		t.Fatal("changed description did not create a new revision")
	}
	// v1 is still published; v2 is a draft (not published) — re-approval is required.
	v1, _, _ := s.GetToolRevision(ctx, org, project, rev1)
	v2, _, _ := s.GetToolRevision(ctx, org, project, rev2)
	if !v1.Published {
		t.Fatal("published v1 lost its publish stamp after a re-discovery")
	}
	if v2.Published {
		t.Fatal("a re-discovered changed revision was auto-published — capability drift without re-approval")
	}
}

// TestMCPDiscoveredToolAdvertisedOnlyWhenPublishedAndPinned proves the untrusted-description-to-approval
// gate at the ADVERTISING seam: a DRAFT discovered tool (never published/pinned) does not resolve through
// SchemaResolved, so it is never offered to the model; once published + pinned + rider-named it resolves and
// carries its approved schema.
func TestMCPDiscoveredToolAdvertisedOnlyWhenPublishedAndPinned(t *testing.T) {
	s, org, project := openStore(t)
	ctx := context.Background()
	fake := &fakeMCP{tools: []mcp.RemoteTool{{Name: "echo", Description: "d", InputSchema: map[string]any{"type": "object"}}}}
	s.SetMCP(fake)
	connID := createStdioConnection(t, s, org, project, "docs")

	// Discover only (draft) — a run naming the connection resolves nothing for the draft tool.
	if _, err := s.DiscoverConnection(ctx, org, project, connID); err != nil {
		t.Fatalf("discover: %v", err)
	}
	broker := brokerWithLookup(s)
	// A run with the rider but NO published set: the draft is not pinnable, so not advertised.
	draftRun := seedRunWithMCPRider(t, s, org, project, "tsrev_none", `["`+connID+`"]`)
	envDraft := toolbroker.ExecEnv{Scope: toolbroker.TaskScope{Org: org, Project: project, RunID: draftRun}}
	if _, found, err := broker.SchemaResolved(ctx, envDraft, "docs__echo"); err != nil || found {
		t.Fatalf("draft mcp tool SchemaResolved found=%v err=%v, want NOT advertised (unapproved)", found, err)
	}

	// Publish + pin + rider → now it resolves and advertises with its schema.
	setID := publishDiscoveredIntoSet(t, s, org, project, connID, "mcp.docs.echo")
	pubRun := seedRunWithMCPRider(t, s, org, project, setID, `["`+connID+`"]`)
	envPub := toolbroker.ExecEnv{Scope: toolbroker.TaskScope{Org: org, Project: project, RunID: pubRun}}
	tool, found, err := broker.SchemaResolved(ctx, envPub, "docs__echo")
	if err != nil || !found {
		t.Fatalf("published+pinned mcp tool SchemaResolved found=%v err=%v, want advertised", found, err)
	}
	if tool.InputSchema == nil {
		t.Fatal("advertised mcp tool has no input schema")
	}
}

// TestMCPToolTenantIsolated proves a run in tenant A cannot resolve an MCP tool registered + pinned in
// tenant B — the lookup joins are tenant-pinned, so no cross-tenant MCP tool leaks.
func TestMCPToolTenantIsolated(t *testing.T) {
	s, orgA, projectA := openStore(t)
	ctx := context.Background()
	fake := &fakeMCP{tools: []mcp.RemoteTool{{Name: "echo", InputSchema: map[string]any{"type": "object"}}}}
	s.SetMCP(fake)

	orgB, projectB := testID("org"), testID("prj")
	mustExec(t, s.pool, `INSERT INTO organizations (id) VALUES ($1)`, orgB)
	mustExec(t, s.pool, `INSERT INTO projects (id, organization_id) VALUES ($1,$2)`, projectB, orgB)
	connB := createStdioConnection(t, s, orgB, projectB, "docs")
	setB := publishDiscoveredIntoSet(t, s, orgB, projectB, connB, "mcp.docs.echo")

	// Tenant A run even naming B's set + connection id resolves nothing (the joins are tenant A-pinned).
	runA := seedRunWithMCPRider(t, s, orgA, projectA, setB, `["`+connB+`"]`)
	if _, found, err := s.LookupTool(ctx, orgA, projectA, runA, "docs__echo"); err != nil || found {
		t.Fatalf("tenant A resolve of B's mcp tool found=%v err=%v, want found=false (tenant-isolated)", found, err)
	}
}
