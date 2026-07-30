//go:build component

package execution

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	mcpclient "github.com/palgroup/palai/adapters/integrations/mcp"
	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
)

// THE SHIPPED RUNBOOK, EXECUTED — docs/operations/jira-mcp-connection.md §3, step for step, over the PUBLIC
// API and nothing else (E25 T7, plan §3.6 D14).
//
// WHY THIS FILE EXISTS, and it is a measurement rather than a preference. The runbook has shipped since E22
// telling an operator to "find the ids with GET /v1/tools, then publish each revision" — and no route in the
// tree returned a revision id. `GET /v1/tools` projects {id, object, canonical_name, model_visible_name};
// `GET /v1/tools/{id}` projects the same four. So step (c) named a `$REV_ID` an operator could not obtain,
// step (d) pinned it, and step (e) granted the set it went into. Three steps rested on one that could not be
// performed. The end-to-end proof the runbook already cites
// (extensions/mcp_jira_component_test.go TestJiraMCPConnectionEndToEnd) does the whole chain — through the
// STORE, with the revision id read out of Postgres by a test helper, which is exactly the step an operator
// does not have. A document is executable or it is not, and the only way to know which is to execute it.
//
// This test WAS RED at the revision-publish step before GET /v1/tools/{tool_id}/revisions existed, and the
// status it reported is worth keeping because it is SHARPER than the one that was predicted:
//
//	=== RUN   TestTheJiraRunbookRunsOnThePublicAPIAlone
//	    jira_runbook_component_test.go:134: GET /v1/tools/tool_ff1a012d…/revisions = 405, want 200: null
//	--- FAIL: TestTheJiraRunbookRunsOnThePublicAPIAlone (34.66s)
//
// 405, NOT 404 — Go's ServeMux answers a registered PATH with an unregistered METHOD by naming the methods
// it does serve. That path was already mounted for POST (create a draft), so the tree could WRITE a
// revision at exactly the address it refused to READ one from. The runbook's step (c) — "find the ids with
// GET /v1/tools, then publish each revision" — named an id that only the database held.
//
// WHAT IS FAKE HERE AND WHAT IS NOT. The MCP CLIENT is a double (fakeRunbookMCP below): the wire is proven
// elsewhere and better — extensions/mcp_jira_component_test.go drives the real Manager over real TLS against
// a server written to the published protocol, and adapters/integrations/mcp/jira_live_test.go dials
// Atlassian itself. Neither of those can answer THIS question, because neither goes through an HTTP route.
// Everything below the client is production: the shipped router, the shipped store, real Postgres, real RLS.
// Discovering against a real MCP server from a console remains §6 leg 3.

// fakeRunbookMCP is the discovery double. It is deliberately minimal — this file is about the ROUTES, and a
// second fake server would prove the wire a third time while proving nothing new about the surface.
type fakeRunbookMCP struct {
	tools []mcpclient.RemoteTool
	// vetted counts VetConnection calls, so the SSRF gate being ON the admin path is observed rather than
	// assumed (create and discover each vet once).
	vetted int
}

func (f *fakeRunbookMCP) Discover(context.Context, mcpclient.ConnConfig) ([]mcpclient.RemoteTool, error) {
	return f.tools, nil
}

func (f *fakeRunbookMCP) VetConnection(context.Context, mcpclient.ConnConfig) error {
	f.vetted++
	return nil
}

func (f *fakeRunbookMCP) Call(context.Context, mcpclient.CallScope, mcpclient.ConnConfig, string, map[string]any) (map[string]any, error) {
	return map[string]any{"key": "PAL-42"}, nil
}

// runbookRouter is the SHIPPED router carrying exactly the families the runbook's five calls address:
// mcp-connections, tools/tool-sets, and agents. Nothing else is mounted, so a route answered here is the
// route the runbook names rather than something a neighbouring mount happens to serve.
func runbookRouter(repo *store.Store) http.Handler {
	return api.NewRouter(repo, nil, nil, nil, nil, repo, nil, nil, nil, repo, repo, nil, nil, nil, nil,
		api.SSEConfig{}, nil, nil)
}

// The untrusted description, carried through every projection below. It is an ATTACKER'S TEXT in the shape
// the runbook warns about (§3b: "a tool's own description are both text somebody else wrote"), and this test
// asserts it travels as DATA — bytes on the wire, unchanged, never interpreted. The browser half of that
// claim (that the console renders it inert) is tests/mcp-tools.spec.ts.
const runbookHostileDescription = `Get a Jira issue. <script>alert(1)</script> <a href="https://evil.example/">click</a>`

// TestTheJiraRunbookRunsOnThePublicAPIAlone walks docs/operations/jira-mcp-connection.md §3 (a)–(e) with an
// HTTP client and a bearer token, and nothing else. Each leg is labelled with the runbook step it is, so a
// failure names the sentence in the document that stopped being true.
func TestTheJiraRunbookRunsOnThePublicAPIAlone(t *testing.T) {
	cs, tenant, _ := openPinnedSpine(t)
	repo := approverHTTP(t)
	fake := &fakeRunbookMCP{tools: []mcpclient.RemoteTool{
		{
			Name:        "getJiraIssue",
			Description: runbookHostileDescription,
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{"issueKey": map[string]any{"type": "string"}}},
		},
		{
			Name:        "transitionJiraIssue",
			Description: "Move a Jira issue to another status.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{"issueKey": map[string]any{"type": "string"}}},
		},
	}}
	repo.SetMCP(fake)

	caller := mintScopedKey(t, repo, cs, tenant, []string{"provision"})
	srv := httptest.NewServer(runbookRouter(repo))
	t.Cleanup(srv.Close)
	client := &consoleClient{t: t, base: srv.URL, token: caller.token}

	// --- RUNBOOK STEP (a): register the connection. The credential is a HANDLE, never inline. ------------
	conn := client.post(201, "/v1/mcp-connections", map[string]any{
		"name":       "jira",
		"transport":  "http",
		"config":     map[string]any{"url": "https://mcp.atlassian.example/v1/mcp"},
		"secret_ref": "jira-api-token",
	})
	connID, _ := conn["id"].(string)
	if connID == "" {
		t.Fatalf("RUNBOOK STEP (a) — POST /v1/mcp-connections returned no id: %v", conn)
	}

	// --- RUNBOOK STEP (b): discover. Each tool becomes a DRAFT revision; nothing is advertised yet. ------
	discovered := client.post(200, "/v1/mcp-connections/"+connID+"/discover", map[string]any{})
	newRevisions, _ := discovered["new_revisions"].([]any)
	if len(newRevisions) != 2 {
		t.Fatalf("RUNBOOK STEP (b) — discover reported %v new revisions, want the two tools the server offered", discovered["new_revisions"])
	}

	// --- RUNBOOK STEP (c): "Find the ids with GET /v1/tools, then publish each revision." ----------------
	//
	// The lineage ids come from the list, exactly as the document says.
	tools := client.get(200, "/v1/tools")
	readToolID := findToolID(t, tools, "mcp.jira.getJiraIssue")
	writeToolID := findToolID(t, tools, "mcp.jira.transitionJiraIssue")

	// AND THIS IS THE STEP THAT COULD NOT BE PERFORMED. A revision id is what the publish route's path
	// carries, and until this route existed no /v1 response contained one.
	revisions := client.get(200, "/v1/tools/"+readToolID+"/revisions")
	readRev := onlyRevision(t, revisions, readToolID)

	// WHAT IS BEING APPROVED IS ON THE WIRE. The description and the input schema are the two things an
	// admin is deciding about — the untrusted prose that will enter a model's context and the shape the
	// model will be able to call. A list that showed neither would ask for approval of an id.
	if readRev["description"] != runbookHostileDescription {
		t.Fatalf("RUNBOOK STEP (c) — the revision list does not carry the server's description verbatim: %v", readRev["description"])
	}
	if _, ok := readRev["input_schema"].(map[string]any); !ok {
		t.Fatalf("RUNBOOK STEP (c) — the revision list carries no input_schema: %v", readRev["input_schema"])
	}
	if readRev["status"] != "draft" {
		t.Fatalf("RUNBOOK STEP (c) — a discovered revision is %v, want draft (EXT-006: discovery never auto-publishes)", readRev["status"])
	}
	if readRev["approval_required"] != false {
		t.Fatalf("RUNBOOK STEP (c) — an unpublished revision reports approval_required=%v, want false", readRev["approval_required"])
	}
	if readRev["executor"] != "mcp" {
		t.Fatalf("RUNBOOK STEP (c) — executor = %v, want mcp", readRev["executor"])
	}
	readRevID, _ := readRev["id"].(string)

	// A READ tool needs no body (the runbook's own sentence).
	client.post(200, "/v1/tools/"+readToolID+"/revisions/"+readRevID+"/publish", map[string]any{})

	// A WRITE tool takes one more answer on the SAME call.
	writeRevisions := client.get(200, "/v1/tools/"+writeToolID+"/revisions")
	writeRevID, _ := onlyRevision(t, writeRevisions, writeToolID)["id"].(string)
	client.post(200, "/v1/tools/"+writeToolID+"/revisions/"+writeRevID+"/publish", map[string]any{
		"approval_required": true,
		"approval_label":    "the shared Jira service account may move tickets in PAL",
	})

	// THE DECLARATION READS BACK, which is what makes the flag auditable rather than a write-and-pray. An
	// operator who cannot re-read the gate they set cannot tell a published write tool from an ungated one.
	published := onlyRevision(t, client.get(200, "/v1/tools/"+writeToolID+"/revisions"), writeToolID)
	if published["status"] != "published" || published["approval_required"] != true {
		t.Fatalf("RUNBOOK STEP (c) — the published write revision reads back as status=%v approval_required=%v, want published/true", published["status"], published["approval_required"])
	}
	if published["approval_label"] != "the shared Jira service account may move tickets in PAL" {
		t.Fatalf("RUNBOOK STEP (c) — the operator's label did not read back: %v", published["approval_label"])
	}

	// --- RUNBOOK STEP (d): pin the approved revisions into a tool set, and publish the set. --------------
	set := client.post(201, "/v1/tool-sets/jira/revisions", map[string]any{
		"tools": []any{map[string]any{"tool_revision_id": readRevID}, map[string]any{"tool_revision_id": writeRevID}},
	})
	setRevID, _ := set["id"].(string)
	if setRevID == "" {
		t.Fatalf("RUNBOOK STEP (d) — POST /v1/tool-sets/{set}/revisions returned no id: %v", set)
	}
	client.post(200, "/v1/tool-sets/jira/revisions/"+setRevID+"/publish", map[string]any{})

	// THE SET'S CONTENTS. Until this route existed, `GET /v1/tool-sets` projected a digest and a revision
	// number and NOT the pins — so "which tools did I actually grant?" had no answer on the public API, and
	// the runbook's step (e) granted a set nobody could read back.
	setDetail := client.get(200, "/v1/tool-sets/jira/revisions/"+setRevID)
	pins, _ := setDetail["tools"].([]any)
	if len(pins) != 2 {
		t.Fatalf("RUNBOOK STEP (d) — the set revision carries %d pin(s), want the two that were pinned: %v", len(pins), setDetail["tools"])
	}
	if setDetail["status"] != "published" {
		t.Fatalf("RUNBOOK STEP (d) — the set revision reads back as %v, want published", setDetail["status"])
	}
	pinned := map[string]bool{}
	for _, p := range pins {
		row, _ := p.(map[string]any)
		id, _ := row["tool_revision_id"].(string)
		pinned[id] = true
	}
	if !pinned[readRevID] || !pinned[writeRevID] {
		t.Fatalf("RUNBOOK STEP (d) — the pins do not name the two published revisions: %v", pins)
	}

	// --- RUNBOOK STEP (e): grant them to an agent revision. BOTH fields are required. --------------------
	agent := client.post(201, "/v1/agents", map[string]any{"name": "jira-reader"})
	agentID, _ := agent["id"].(string)
	revision := client.post(201, "/v1/agents/"+agentID+"/revisions", map[string]any{
		"model":           "runbook-pinned-model",
		"tool_sets":       []any{setRevID},
		"mcp_connections": []any{connID},
	})
	agentRevID, _ := revision["id"].(string)
	client.post(200, "/v1/agents/"+agentID+"/revisions/"+agentRevID+"/publish", map[string]any{})

	// The rider reads back, so the runbook's §4 warning ("both fields, or nothing happens") is checkable
	// from the API rather than only from a failure inside a run.
	granted := client.get(200, "/v1/agents/"+agentID+"/revisions")
	grantedRows, _ := granted["data"].([]any)
	if len(grantedRows) == 0 {
		t.Fatal("RUNBOOK STEP (e) — the agent revision list is empty")
	}
	first, _ := grantedRows[0].(map[string]any)
	rendered, _ := json.Marshal(first)
	for _, want := range []string{setRevID, connID} {
		if !strings.Contains(string(rendered), want) {
			t.Fatalf("RUNBOOK STEP (e) — the published revision does not name %s: %s", want, rendered)
		}
	}

	// THE SSRF GATE IS ON THE ADMIN PATH, observed rather than assumed: create vets once, discover vets once.
	if fake.vetted < 2 {
		t.Fatalf("VetConnection ran %d time(s); the fail-fast egress gate must run on BOTH create and discover", fake.vetted)
	}

	// NO CREDENTIAL CROSSED. `secret_ref` is a handle and the runbook says so; this asserts it on the BYTES
	// of every response this client received rather than on the field list of any one of them.
	for path, body := range client.seen {
		if strings.Contains(body, "jira-api-token") && !strings.HasPrefix(path, "POST /v1/mcp-connections") {
			t.Fatalf("the secret handle came back on %s: %s", path, body)
		}
	}
}

// TestAForeignTenantsToolRevisionIsNotFound is the cross-tenant half of the two new read routes. RLS confines
// the rows and the handlers name the tenant too, but a route is not scoped until something has tried: the
// foreign key sees a 404 for a tool id and a set revision id that BOTH exist, in another organization.
func TestAForeignTenantsToolRevisionIsNotFound(t *testing.T) {
	cs, tenant, _ := openPinnedSpine(t)
	repo := approverHTTP(t)
	repo.SetMCP(&fakeRunbookMCP{tools: []mcpclient.RemoteTool{
		{Name: "getJiraIssue", Description: "read one issue", InputSchema: map[string]any{"type": "object"}},
	}})

	owner := mintScopedKey(t, repo, cs, tenant, []string{"provision"})
	stranger := seedForeignTenantKey(t, repo, cs)
	srv := httptest.NewServer(runbookRouter(repo))
	t.Cleanup(srv.Close)
	mine := &consoleClient{t: t, base: srv.URL, token: owner.token}
	theirs := &consoleClient{t: t, base: srv.URL, token: stranger.token}

	conn := mine.post(201, "/v1/mcp-connections", map[string]any{
		"name":       "jira",
		"transport":  "http",
		"config":     map[string]any{"url": "https://mcp.atlassian.example/v1/mcp"},
		"secret_ref": "jira-api-token",
	})
	connID, _ := conn["id"].(string)
	mine.post(200, "/v1/mcp-connections/"+connID+"/discover", map[string]any{})
	toolID := findToolID(t, mine.get(200, "/v1/tools"), "mcp.jira.getJiraIssue")
	revID, _ := onlyRevision(t, mine.get(200, "/v1/tools/"+toolID+"/revisions"), toolID)["id"].(string)
	mine.post(200, "/v1/tools/"+toolID+"/revisions/"+revID+"/publish", map[string]any{})
	setRevID, _ := mine.post(201, "/v1/tool-sets/jira/revisions", map[string]any{
		"tools": []any{map[string]any{"tool_revision_id": revID}},
	})["id"].(string)

	// A 404 rather than an empty page: the id is real and the answer must not distinguish "not yours" from
	// "does not exist" — nor may it be an existence oracle by answering differently for a made-up id.
	theirs.get(404, "/v1/tools/"+toolID+"/revisions")
	theirs.get(404, "/v1/tools/tool_does_not_exist_at_all/revisions")
	theirs.get(404, "/v1/tool-sets/jira/revisions/"+setRevID)
	theirs.get(404, "/v1/tool-sets/jira/revisions/tsrev_does_not_exist_at_all")
	// The single-revision read carries the same two properties, and it is the address a create's Location
	// hands out — so it is reachable by anyone who ever saw a 201 body.
	theirs.get(404, "/v1/tools/"+toolID+"/revisions/"+revID)
	theirs.get(404, "/v1/tools/"+toolID+"/revisions/trev_does_not_exist_at_all")

	// And the owner still sees them — otherwise the refusals above would be satisfied by a route that
	// refuses everybody.
	mine.get(200, "/v1/tools/"+toolID+"/revisions")
	mine.get(200, "/v1/tool-sets/jira/revisions/"+setRevID)
	mine.get(200, "/v1/tools/"+toolID+"/revisions/"+revID)

	// THE LINEAGE ID IS PART OF THE IDENTITY, the tool-side twin of the set-name claim below: a revision of
	// tool A must not be readable under tool B's id, or the {tool_id} segment would be decorative and a
	// console could show one tool's revision under another tool's heading.
	otherTool := mine.post(201, "/v1/tools", map[string]any{"canonical_name": "acme.other.lineage"})
	otherToolID, _ := otherTool["id"].(string)
	mine.get(404, "/v1/tools/"+otherToolID+"/revisions/"+revID)

	// THE SET NAME IS PART OF THE IDENTITY. A revision id from set "jira" must not be readable under another
	// set's name, or the path would be decorative and a console could show one set's contents under another.
	mine.get(404, "/v1/tool-sets/other/revisions/"+setRevID)
}

// TestTheToolRevisionReadRoutesRequireTheProvisionCapability pins the gate the two new routes carry. A key
// with a non-empty scope set that does NOT name `provision` is refused; the same key reaching the older,
// UNGATED /v1/tools is not — which is what makes this a measurement of the new routes rather than of the
// middleware.
func TestTheToolRevisionReadRoutesRequireTheProvisionCapability(t *testing.T) {
	cs, tenant, _ := openPinnedSpine(t)
	repo := approverHTTP(t)
	narrow := mintScopedKey(t, repo, cs, tenant, []string{"responses"})
	srv := httptest.NewServer(runbookRouter(repo))
	t.Cleanup(srv.Close)
	client := &consoleClient{t: t, base: srv.URL, token: narrow.token}

	client.get(403, "/v1/tools/tool_whatever/revisions")
	client.get(403, "/v1/tool-sets/jira/revisions/tsrev_whatever")
	client.get(403, "/v1/tools/tool_whatever/revisions/trev_whatever")
	// The pre-existing surface is unchanged by this task, and saying so here is what keeps the asymmetry a
	// recorded decision instead of an accident: GET /v1/tools has never been capability-gated.
	client.get(200, "/v1/tools")
}

// findToolID resolves a lineage id from the list page by canonical name, failing with what it did see.
func findToolID(t *testing.T, page map[string]any, canonical string) string {
	t.Helper()
	rows, _ := page["data"].([]any)
	var names []string
	for _, r := range rows {
		row, _ := r.(map[string]any)
		name, _ := row["canonical_name"].(string)
		names = append(names, name)
		if name == canonical {
			id, _ := row["id"].(string)
			return id
		}
	}
	t.Fatalf("GET /v1/tools does not list %q; it listed %v", canonical, names)
	return ""
}

// onlyRevision asserts the revision page holds exactly one row and returns it. Every lineage in this file is
// discovered once, so a second row would mean discovery churned — the failure mode the digest check exists
// to prevent, and one worth failing on rather than indexing past.
func onlyRevision(t *testing.T, page map[string]any, toolID string) map[string]any {
	t.Helper()
	rows, _ := page["data"].([]any)
	if len(rows) != 1 {
		t.Fatalf("GET /v1/tools/%s/revisions returned %d row(s), want exactly one: %v", toolID, len(rows), page)
	}
	row, _ := rows[0].(map[string]any)
	if row["tool_id"] != toolID {
		t.Fatalf("the revision row names tool_id=%v, want %s", row["tool_id"], toolID)
	}
	return row
}
