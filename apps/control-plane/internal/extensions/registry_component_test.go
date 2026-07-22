//go:build component

package extensions

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	remotehttp "github.com/palgroup/palai/adapters/tools/http"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// openStore opens a migrated spine, seeds an org+project, and returns the extensions store scoped to it.
// The store reserves the built-in short name "file" so the reserved-collision leg has a target.
func openStore(t *testing.T) (*Store, string, string) {
	t.Helper()
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-component TEST=postgres")
	}
	ctx := context.Background()
	cs, err := coordinator.Open(ctx, url)
	if err != nil {
		t.Fatalf("coordinator.Open() error = %v", err)
	}
	t.Cleanup(cs.Close)
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	org, project := testID("org"), testID("prj")
	pool := cs.Pool()
	if _, err := pool.Exec(ctx, `INSERT INTO organizations (id) VALUES ($1)`, org); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, project, org); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return New(pool, "file"), org, project
}

func testID(prefix string) string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return prefix + "_" + hex.EncodeToString(raw[:])
}

// rawRevisionRow reads a tool revision's config columns + digest + publish stamp as one comparable
// string, so a test can assert the whole row is byte-stable after a later revise.
func rawRevisionRow(t *testing.T, s *Store, revisionID string) string {
	t.Helper()
	var executor, input, digest, published string
	err := s.pool.QueryRow(context.Background(),
		`SELECT executor, input_schema::text, digest, COALESCE(published_at::text,'') FROM tool_revisions WHERE id=$1`,
		revisionID).Scan(&executor, &input, &digest, &published)
	if err != nil {
		t.Fatalf("read raw revision row %s: %v", revisionID, err)
	}
	return executor + "\x1f" + input + "\x1f" + digest + "\x1f" + published
}

// TestToolRevisionImmutableAndDigestPinned proves the core §28.4 invariant: once published, a tool
// revision's config + digest are frozen — a revise creates a NEW revision, the published row is
// byte-for-byte unchanged, and identical config yields an identical digest. Publish is a once-only flip.
func TestToolRevisionImmutableAndDigestPinned(t *testing.T) {
	s, org, project := openStore(t)
	ctx := context.Background()

	tool, err := s.CreateTool(ctx, org, project, "acme.search.fetch")
	if err != nil {
		t.Fatalf("create tool: %v", err)
	}
	body := []byte(`{"executor":"control_plane","description":"v1","input_schema":{"type":"object"},"replay_class":"pure"}`)
	v1, err := s.CreateToolRevision(ctx, org, project, tool.ID, body)
	if err != nil {
		t.Fatalf("create v1: %v", err)
	}
	if v1.RevisionNumber != 1 {
		t.Fatalf("first revision number = %d, want 1", v1.RevisionNumber)
	}
	// An identical body produces an identical digest (content address, not row identity).
	v1b, _ := s.CreateToolRevision(ctx, org, project, tool.ID, body)
	if v1b.Digest != v1.Digest {
		t.Fatalf("identical config produced different digests: %s vs %s", v1.Digest, v1b.Digest)
	}

	published, _, err := s.PublishToolRevision(ctx, org, project, v1.ID)
	if err != nil || !published {
		t.Fatalf("publish v1 = %v err = %v, want published", published, err)
	}
	rawBefore := rawRevisionRow(t, s, v1.ID)

	// A revise is a NEW revision with different config; v1's row is untouched.
	v2, err := s.CreateToolRevision(ctx, org, project, tool.ID, []byte(`{"executor":"control_plane","description":"v2","input_schema":{"type":"object","x":1},"replay_class":"idempotent"}`))
	if err != nil {
		t.Fatalf("revise -> v2: %v", err)
	}
	if v2.ID == v1.ID || v2.Digest == v1.Digest {
		t.Fatalf("revise produced id=%s digest=%s, want a NEW distinct revision", v2.ID, v2.Digest)
	}
	if rawAfter := rawRevisionRow(t, s, v1.ID); rawAfter != rawBefore {
		t.Fatalf("published v1 row mutated by a later revise:\n before=%s\n after =%s", rawBefore, rawAfter)
	}

	// Publish is once-only: re-publishing v1 is a no-op, never a re-stamp.
	again, _, err := s.PublishToolRevision(ctx, org, project, v1.ID)
	if err != nil {
		t.Fatalf("re-publish v1: %v", err)
	}
	if again {
		t.Fatal("re-publish reported a fresh publish, want a no-op on an already-published revision")
	}
}

// TestCanonicalNamespaceCollisionRejected proves the canonical-name contract: a duplicate canonical name
// in the project is a typed collision reject, and a malformed canonical name (≠3 segments, non-ASCII) is
// rejected before any write (spec §28.2).
func TestCanonicalNamespaceCollisionRejected(t *testing.T) {
	s, org, project := openStore(t)
	ctx := context.Background()

	if _, err := s.CreateTool(ctx, org, project, "acme.search.fetch"); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := s.CreateTool(ctx, org, project, "acme.search.fetch"); !errors.Is(err, ErrNameCollision) {
		t.Fatalf("duplicate canonical name: err = %v, want ErrNameCollision", err)
	}
	for name, canonical := range map[string]string{
		"two segments": "acme.fetch",
		"non-ascii":    "acme.search.fetché",
	} {
		if _, err := s.CreateTool(ctx, org, project, canonical); !errors.Is(err, ErrInvalidCanonicalName) {
			t.Errorf("%s (%q): err = %v, want ErrInvalidCanonicalName", name, canonical, err)
		}
	}
}

// TestModelVisibleShortNameDeterministicCollisionChecked proves the model-visible short name is the
// deterministic last segment with NO auto-suffix: a second tool whose last segment repeats an existing
// one in the project is rejected, and a tool whose short name shadows a code-defined built-in is rejected.
func TestModelVisibleShortNameDeterministicCollisionChecked(t *testing.T) {
	s, org, project := openStore(t)
	ctx := context.Background()

	if _, err := s.CreateTool(ctx, org, project, "acme.search.fetch"); err != nil {
		t.Fatalf("first create: %v", err)
	}
	// A different canonical name whose LAST segment is the same short name collides (no auto-suffix).
	if _, err := s.CreateTool(ctx, org, project, "acme.other.fetch"); !errors.Is(err, ErrNameCollision) {
		t.Fatalf("second *.fetch: err = %v, want ErrNameCollision (deterministic short name, no suffix)", err)
	}
	// A short name shadowing a reserved built-in (the store reserves "file") is rejected before any write.
	if _, err := s.CreateTool(ctx, org, project, "acme.workspace.file"); !errors.Is(err, ErrModelNameReserved) {
		t.Fatalf("built-in shadow: err = %v, want ErrModelNameReserved", err)
	}
}

// TestToolSetPinsExactRevisionsApprovalOnlyStricter proves a ToolSetRevision pins only PUBLISHED
// revisions with only-tightening overrides: a draft pin is rejected, an unknown pin is rejected, an
// override above the declared timeout is rejected while one at/below it is accepted, and the set revision
// is itself immutable + publishable once.
func TestToolSetPinsExactRevisionsApprovalOnlyStricter(t *testing.T) {
	s, org, project := openStore(t)
	ctx := context.Background()

	tool, err := s.CreateTool(ctx, org, project, "acme.search.fetch")
	if err != nil {
		t.Fatalf("create tool: %v", err)
	}
	rev, err := s.CreateToolRevision(ctx, org, project, tool.ID, []byte(`{"executor":"control_plane","description":"d","input_schema":{"type":"object"},"replay_class":"pure","timeout_ms":1000}`))
	if err != nil {
		t.Fatalf("create rev: %v", err)
	}

	// A pin of a DRAFT revision is rejected (only published revisions may be pinned).
	if _, err := s.CreateToolSetRevision(ctx, org, project, "reviewers", pins(rev.ID, nil)); !errors.Is(err, ErrRevisionNotPublished) {
		t.Fatalf("draft pin: err = %v, want ErrRevisionNotPublished", err)
	}
	// An unknown pin is rejected.
	if _, err := s.CreateToolSetRevision(ctx, org, project, "reviewers", pins("trev_missing", nil)); !errors.Is(err, ErrUnknownToolRevision) {
		t.Fatalf("unknown pin: err = %v, want ErrUnknownToolRevision", err)
	}

	if _, _, err := s.PublishToolRevision(ctx, org, project, rev.ID); err != nil {
		t.Fatalf("publish rev: %v", err)
	}

	// An override above the declared timeout (1000) is rejected; at/below is accepted.
	if _, err := s.CreateToolSetRevision(ctx, org, project, "reviewers", pins(rev.ID, map[string]any{"timeout_ms": 2000})); !errors.Is(err, ErrOverrideNotStricter) {
		t.Fatalf("widening override: err = %v, want ErrOverrideNotStricter", err)
	}
	set, err := s.CreateToolSetRevision(ctx, org, project, "reviewers", pins(rev.ID, map[string]any{"timeout_ms": 500}))
	if err != nil {
		t.Fatalf("stricter override rejected: %v", err)
	}
	if set.RevisionNumber != 1 {
		t.Fatalf("set revision number = %d, want 1", set.RevisionNumber)
	}

	// The set revision is publishable once.
	published, _, err := s.PublishToolSetRevision(ctx, org, project, set.ID)
	if err != nil || !published {
		t.Fatalf("publish set = %v err = %v, want published", published, err)
	}
	again, _, err := s.PublishToolSetRevision(ctx, org, project, set.ID)
	if err != nil {
		t.Fatalf("re-publish set: %v", err)
	}
	if again {
		t.Fatal("re-publish set reported a fresh publish, want a no-op")
	}
}

// pins builds a raw set-revision body pinning one revision with an optional override.
func pins(revisionID string, overrides map[string]any) []byte {
	pin := map[string]any{"tool_revision_id": revisionID}
	if overrides != nil {
		pin["overrides"] = overrides
	}
	body, _ := json.Marshal(map[string]any{"tools": []any{pin}})
	return body
}

// TestRegistryToolsLoadIntoBrokerEffectiveSet proves the EXT-002/EXT-003 registry face end-to-end: a
// published control_plane echo tool pinned into a published set that a run's agent revision names in its
// tool_sets (1) contributes its model-visible short name to the run's effective set, (2) resolves through
// the broker's per-tenant lookup and runs the SAME fenced path, while (3) a second registered+published
// tool that is NOT pinned appears in NEITHER — the effective set nor the broker lookup.
func TestRegistryToolsLoadIntoBrokerEffectiveSet(t *testing.T) {
	s, org, project := openStore(t)
	ctx := context.Background()
	pool := s.pool

	// A published control_plane echo tool.
	tool, err := s.CreateTool(ctx, org, project, "acme.search.fetch")
	if err != nil {
		t.Fatalf("create tool: %v", err)
	}
	rev, err := s.CreateToolRevision(ctx, org, project, tool.ID, []byte(`{"executor":"control_plane","input_schema":{"type":"object"},"replay_class":"pure"}`))
	if err != nil {
		t.Fatalf("create rev: %v", err)
	}
	if _, _, err := s.PublishToolRevision(ctx, org, project, rev.ID); err != nil {
		t.Fatalf("publish rev: %v", err)
	}
	// A second published tool, NOT pinned into any set.
	tool2, _ := s.CreateTool(ctx, org, project, "acme.other.lookup")
	rev2, _ := s.CreateToolRevision(ctx, org, project, tool2.ID, []byte(`{"executor":"control_plane","input_schema":{"type":"object"}}`))
	if _, _, err := s.PublishToolRevision(ctx, org, project, rev2.ID); err != nil {
		t.Fatalf("publish rev2: %v", err)
	}
	// A published set pinning ONLY the first tool.
	set, err := s.CreateToolSetRevision(ctx, org, project, "reviewers", pins(rev.ID, nil))
	if err != nil {
		t.Fatalf("create set: %v", err)
	}
	if _, _, err := s.PublishToolSetRevision(ctx, org, project, set.ID); err != nil {
		t.Fatalf("publish set: %v", err)
	}

	// A session + run pinned to an agent revision whose tool_sets names the published set.
	sessionID, runID := testID("ses"), testID("run")
	profileID, arevID := testID("aprof"), testID("arev")
	mustExec(t, pool, `INSERT INTO sessions (id, organization_id, project_id) VALUES ($1,$2,$3)`, sessionID, org, project)
	mustExec(t, pool, `INSERT INTO agent_profiles (id, organization_id, project_id, name) VALUES ($1,$2,$3,'reviewer')`, profileID, org, project)
	mustExec(t, pool, `INSERT INTO agent_revisions (id, organization_id, project_id, profile_id, revision_number, model, published_at, tool_sets)
	                   VALUES ($1,$2,$3,$4,1,'model-x',clock_timestamp(),$5::jsonb)`, arevID, org, project, profileID, `["`+set.ID+`"]`)
	mustExec(t, pool, `INSERT INTO runs (id, organization_id, project_id, session_id, agent_revision_id) VALUES ($1,$2,$3,$4,$5)`, runID, org, project, sessionID, arevID)

	// (1) The effective set: the run's pinned tool_sets contribute "fetch", never the unpinned "lookup".
	cs, err := coordinator.Open(ctx, os.Getenv("PALAI_COMPONENT_POSTGRES_URL"))
	if err != nil {
		t.Fatalf("coordinator.Open: %v", err)
	}
	t.Cleanup(cs.Close)
	_, _, _, toolSetTools, err := cs.PinnedExecConfig(ctx, coordinator.Tenant{Organization: org, Project: project}, runID)
	if err != nil {
		t.Fatalf("PinnedExecConfig: %v", err)
	}
	if !contains(toolSetTools, "fetch") {
		t.Fatalf("effective tool_set tools = %v, want the pinned short name fetch", toolSetTools)
	}
	if contains(toolSetTools, "lookup") {
		t.Fatalf("effective tool_set tools = %v, want the UNPINNED lookup absent", toolSetTools)
	}

	// (2) The broker's per-tenant lookup resolves fetch and runs it through the fenced path.
	broker := toolbroker.New()
	broker.SetLookup(func(ctx context.Context, env toolbroker.ExecEnv, name string) (toolbroker.Tool, bool, error) {
		return s.LookupTool(ctx, env.Scope.Org, env.Scope.Project, env.Scope.RunID, name)
	})
	env := toolbroker.ExecEnv{Scope: toolbroker.TaskScope{Org: org, Project: project, RunID: runID}}
	out, err := broker.Execute(ctx, contracts.ToolCallID("tc_fetch"), "fetch", map[string]any{"q": "x"}, 1, env)
	if err != nil {
		t.Fatalf("broker execute fetch: %v", err)
	}
	if out.Result["q"] != "x" {
		t.Fatalf("echo result = %v, want the input args back", out.Result)
	}

	// (3) The unpinned tool is not resolvable through the run's lookup, so the broker rejects it.
	if _, err := broker.Execute(ctx, contracts.ToolCallID("tc_lookup"), "lookup", map[string]any{}, 2, env); !errors.Is(err, toolbroker.ErrUnknownTool) {
		t.Fatalf("unpinned tool execute err = %v, want ErrUnknownTool (never advertised or resolvable)", err)
	}
}

// fakeInvoker records the last Invocation the binder built and returns a fixed result — the witness the
// registry resolved a remote_http row to the signed executor with the right non-secret wiring + secret.
type fakeInvoker struct {
	last   remotehttp.Invocation
	result map[string]any
}

func (f *fakeInvoker) Invoke(_ context.Context, in remotehttp.Invocation) (map[string]any, error) {
	f.last = in
	return f.result, nil
}

// TestRemoteHTTPToolResolvesThroughRegistryLookup proves the T4 binder wiring: a published remote_http
// tool pinned into a run's set resolves through the broker's per-tenant lookup to an EXEC-bound tool that
// reaches the injected signed executor — carrying the executor_config URL, the tool_call_id + live fence
// (broker per-call), and the secret resolved fresh from the org-scoped resolver. Without an invoker wired
// the same row stays binder-less (the T2 posture), so a remote tool is never advertised half-built.
func TestRemoteHTTPToolResolvesThroughRegistryLookup(t *testing.T) {
	s, org, project := openStore(t)
	ctx := context.Background()

	// A published remote_http tool: executor_config carries only non-secret wiring; the credential is a
	// secret_ref handle. Pin it into a published set the run names.
	tool, err := s.CreateTool(ctx, org, project, "acme.remote.lookup")
	if err != nil {
		t.Fatalf("create tool: %v", err)
	}
	body := []byte(`{"executor":"remote_http","input_schema":{"type":"object"},"output_schema":{"type":"object"},"replay_class":"idempotent","executor_config":{"url":"https://tool.example.com/invoke","allow_private":false},"secret_ref":"sig-ref","timeout_ms":2500}`)
	rev, err := s.CreateToolRevision(ctx, org, project, tool.ID, body)
	if err != nil {
		t.Fatalf("create remote_http rev: %v", err)
	}
	if _, _, err := s.PublishToolRevision(ctx, org, project, rev.ID); err != nil {
		t.Fatalf("publish rev: %v", err)
	}
	set, err := s.CreateToolSetRevision(ctx, org, project, "reviewers", pins(rev.ID, nil))
	if err != nil {
		t.Fatalf("create set: %v", err)
	}
	if _, _, err := s.PublishToolSetRevision(ctx, org, project, set.ID); err != nil {
		t.Fatalf("publish set: %v", err)
	}
	runID := seedRunPinnedToSet(t, s, org, project, set.ID)

	// Without an invoker wired, the row stays binder-less (creatable but not resolvable — the T2 posture).
	if _, found, err := s.LookupTool(ctx, org, project, runID, "lookup"); err != nil || found {
		t.Fatalf("binder-less lookup = found:%v err:%v, want found=false before the invoker is wired", found, err)
	}

	// Wire the signed executor (a fake) + an org-scoped resolver, then resolve + run through the broker.
	inv := &fakeInvoker{result: map[string]any{"echoed": true}}
	var resolvedOrg, resolvedRef string
	s.SetRemoteInvoker(inv, func(o, ref string) ([]byte, error) {
		resolvedOrg, resolvedRef = o, ref
		return []byte("resolved-secret-" + o), nil
	})

	broker := toolbroker.New()
	broker.SetLookup(func(ctx context.Context, env toolbroker.ExecEnv, name string) (toolbroker.Tool, bool, error) {
		return s.LookupTool(ctx, env.Scope.Org, env.Scope.Project, env.Scope.RunID, name)
	})
	env := toolbroker.ExecEnv{Scope: toolbroker.TaskScope{Org: org, Project: project, RunID: runID}}
	out, err := broker.Execute(ctx, contracts.ToolCallID("tc_remote_1"), "lookup", map[string]any{"q": "x"}, 9, env)
	if err != nil {
		t.Fatalf("broker execute remote_http lookup: %v", err)
	}
	if out.Result["echoed"] != true {
		t.Fatalf("remote result = %v, want the invoker's result", out.Result)
	}
	// The binder built the invocation from the executor_config + per-call identity, and resolved the secret
	// fresh from the org-scoped resolver (never held in the closure).
	if inv.last.URL != "https://tool.example.com/invoke" {
		t.Fatalf("invocation URL = %q, want the executor_config url", inv.last.URL)
	}
	if inv.last.ToolCallID != "tc_remote_1" || inv.last.Fence != 9 {
		t.Fatalf("invocation identity = call:%q fence:%d, want tc_remote_1/9 (broker per-call)", inv.last.ToolCallID, inv.last.Fence)
	}
	if inv.last.TimeoutMS != 2500 || inv.last.SecretRef != "sig-ref" {
		t.Fatalf("invocation wiring = timeout:%d ref:%q, want 2500/sig-ref", inv.last.TimeoutMS, inv.last.SecretRef)
	}
	if resolvedOrg != org || resolvedRef != "sig-ref" || string(inv.last.Secret) != "resolved-secret-"+org {
		t.Fatalf("secret resolution = org:%q ref:%q secret:%q, want org-scoped resolve of sig-ref", resolvedOrg, resolvedRef, inv.last.Secret)
	}
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// publishEcho registers a control_plane echo tool with the given canonical name and returns its published
// revision id.
func publishEcho(t *testing.T, s *Store, org, project, canonical string) string {
	t.Helper()
	ctx := context.Background()
	tool, err := s.CreateTool(ctx, org, project, canonical)
	if err != nil {
		t.Fatalf("create tool %s: %v", canonical, err)
	}
	rev, err := s.CreateToolRevision(ctx, org, project, tool.ID, []byte(`{"executor":"control_plane","input_schema":{"type":"object"},"replay_class":"pure"}`))
	if err != nil {
		t.Fatalf("create rev %s: %v", canonical, err)
	}
	if _, _, err := s.PublishToolRevision(ctx, org, project, rev.ID); err != nil {
		t.Fatalf("publish rev %s: %v", canonical, err)
	}
	return rev.ID
}

// seedRunPinnedToSet seeds a session + agent profile/revision (tool_sets = [setID]) + run, and returns the runID.
func seedRunPinnedToSet(t *testing.T, s *Store, org, project, setID string) string {
	t.Helper()
	sessionID, runID := testID("ses"), testID("run")
	profileID, arevID := testID("aprof"), testID("arev")
	mustExec(t, s.pool, `INSERT INTO sessions (id, organization_id, project_id) VALUES ($1,$2,$3)`, sessionID, org, project)
	mustExec(t, s.pool, `INSERT INTO agent_profiles (id, organization_id, project_id, name) VALUES ($1,$2,$3,'reviewer')`, profileID, org, project)
	mustExec(t, s.pool, `INSERT INTO agent_revisions (id, organization_id, project_id, profile_id, revision_number, model, published_at, tool_sets)
	                     VALUES ($1,$2,$3,$4,1,'model-x',clock_timestamp(),$5::jsonb)`, arevID, org, project, profileID, `["`+setID+`"]`)
	mustExec(t, s.pool, `INSERT INTO runs (id, organization_id, project_id, session_id, agent_revision_id) VALUES ($1,$2,$3,$4,$5)`, runID, org, project, sessionID, arevID)
	return runID
}

// TestPinnedRunConfigToolOrderStable proves M1: PinnedRunConfig returns the tool-set short names in a
// DETERMINISTIC (sorted) order — the array_agg is ORDER BY the short name — so the list that flows into
// ConfigSnapshot.Hash is stable across reads (no spurious checkpoint/config-hash divergence for a
// multi-tool set). Two tools are created in reverse-sorted order to prove the read is not insertion order.
func TestPinnedRunConfigToolOrderStable(t *testing.T) {
	s, org, project := openStore(t)
	ctx := context.Background()

	// Create "zebra" first, "apple" second — insertion order is the REVERSE of sorted order.
	revZebra := publishEcho(t, s, org, project, "acme.a.zebra")
	revApple := publishEcho(t, s, org, project, "acme.a.apple")
	set, err := s.CreateToolSetRevision(ctx, org, project, "reviewers",
		[]byte(`{"tools":[{"tool_revision_id":"`+revZebra+`"},{"tool_revision_id":"`+revApple+`"}]}`))
	if err != nil {
		t.Fatalf("create set: %v", err)
	}
	if _, _, err := s.PublishToolSetRevision(ctx, org, project, set.ID); err != nil {
		t.Fatalf("publish set: %v", err)
	}
	runID := seedRunPinnedToSet(t, s, org, project, set.ID)

	cs, err := coordinator.Open(ctx, os.Getenv("PALAI_COMPONENT_POSTGRES_URL"))
	if err != nil {
		t.Fatalf("coordinator.Open: %v", err)
	}
	t.Cleanup(cs.Close)
	tenant := coordinator.Tenant{Organization: org, Project: project}

	want := []string{"apple", "zebra"} // sorted, regardless of insertion order
	for i := 0; i < 3; i++ {
		_, _, _, tools, err := cs.PinnedExecConfig(ctx, tenant, runID)
		if err != nil {
			t.Fatalf("PinnedExecConfig read %d: %v", i, err)
		}
		if len(tools) != 2 || tools[0] != want[0] || tools[1] != want[1] {
			t.Fatalf("read %d tool_set tools = %v, want deterministic sorted %v", i, tools, want)
		}
	}
}

// TestLookupToolIsTenantIsolated proves L5: a run in tenant A cannot resolve a tool registered + pinned
// in tenant B — the lookup and PinnedRunConfig joins are tenant-pinned, so no cross-tenant tool leaks.
func TestLookupToolIsTenantIsolated(t *testing.T) {
	s, orgA, projectA := openStore(t)
	ctx := context.Background()

	// Tenant B (a second org/project on the same pool) registers + pins an echo tool.
	orgB, projectB := testID("org"), testID("prj")
	mustExec(t, s.pool, `INSERT INTO organizations (id) VALUES ($1)`, orgB)
	mustExec(t, s.pool, `INSERT INTO projects (id, organization_id) VALUES ($1,$2)`, projectB, orgB)
	revB := publishEcho(t, s, orgB, projectB, "acme.search.fetch")
	setB, _ := s.CreateToolSetRevision(ctx, orgB, projectB, "reviewers", []byte(`{"tools":[{"tool_revision_id":"`+revB+`"}]}`))
	if _, _, err := s.PublishToolSetRevision(ctx, orgB, projectB, setB.ID); err != nil {
		t.Fatalf("publish set B: %v", err)
	}

	// Tenant A has a run pinned to a set of its own (empty of B's tool). Even naming B's set id in A's
	// agent revision resolves nothing — the join is pinned to A's tenant, and setB is not in A's scope.
	runA := seedRunPinnedToSet(t, s, orgA, projectA, setB.ID)

	// A's lookup for B's tool name resolves nothing (tenant isolation).
	if _, found, err := s.LookupTool(ctx, orgA, projectA, runA, "fetch"); err != nil || found {
		t.Fatalf("tenant A lookup of B's tool = found:%v err:%v, want found=false (tenant-isolated)", found, err)
	}
	// And A's effective set carries none of B's tools.
	cs, err := coordinator.Open(ctx, os.Getenv("PALAI_COMPONENT_POSTGRES_URL"))
	if err != nil {
		t.Fatalf("coordinator.Open: %v", err)
	}
	t.Cleanup(cs.Close)
	_, _, _, tools, err := cs.PinnedExecConfig(ctx, coordinator.Tenant{Organization: orgA, Project: projectA}, runA)
	if err != nil {
		t.Fatalf("PinnedExecConfig A: %v", err)
	}
	if len(tools) != 0 {
		t.Fatalf("tenant A effective tool_set tools = %v, want empty (B's set is out of scope)", tools)
	}
}
