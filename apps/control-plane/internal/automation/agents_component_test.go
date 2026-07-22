//go:build component

package automation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/palgroup/palai/packages/coordinator"
)

// openStore opens a migrated spine, seeds an org+project, and returns the automation store scoped to it.
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
	return New(pool), org, project
}

func testID(prefix string) string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return prefix + "_" + hex.EncodeToString(raw[:])
}

// rawRevisionRow reads a revision's config columns + publish stamp as a single comparable string, so a
// test can assert the whole row is byte-stable (JSONB tools rendered ::text for a stable comparison).
func rawRevisionRow(t *testing.T, s *Store, revisionID string) string {
	t.Helper()
	var model, tools, instructions, published string
	err := s.pool.QueryRow(context.Background(),
		`SELECT model, COALESCE(tools::text,''), instructions, COALESCE(published_at::text,'') FROM agent_revisions WHERE id=$1`,
		revisionID).Scan(&model, &tools, &instructions, &published)
	if err != nil {
		t.Fatalf("read raw revision row %s: %v", revisionID, err)
	}
	return model + "\x1f" + tools + "\x1f" + instructions + "\x1f" + published
}

// TestAgentRevisionPublishIsImmutable proves the core §10 invariant: once published, a revision's config
// is frozen — a "revise" (a PATCH in API terms) creates a NEW draft revision, and the published row's
// config is byte-for-byte unchanged. Publish itself is a once-only flip: a second publish is a no-op.
func TestAgentRevisionPublishIsImmutable(t *testing.T) {
	s, org, project := openStore(t)
	ctx := context.Background()

	profileID, err := s.CreateProfile(ctx, org, project, "reviewer")
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	v1, err := s.CreateRevision(ctx, org, project, profileID, []byte(`{"model":"model-a","tools":["file"],"instructions":"v1"}`))
	if err != nil {
		t.Fatalf("create v1: %v", err)
	}
	if v1.RevisionNumber != 1 {
		t.Fatalf("first revision number = %d, want 1", v1.RevisionNumber)
	}

	// Publish v1, then snapshot its committed config.
	published, _, err := s.PublishRevision(ctx, org, project, v1.ID)
	if err != nil || !published {
		t.Fatalf("publish v1 = %v err = %v, want published", published, err)
	}
	before, ok, err := s.GetRevision(ctx, org, project, v1.ID)
	if err != nil || !ok || !before.Published {
		t.Fatalf("get v1 after publish = %+v ok=%v err=%v, want published", before, ok, err)
	}
	// The published row's raw bytes (every config column + publish stamp) — the plan's "byte-değişmez".
	rawBefore := rawRevisionRow(t, s, v1.ID)

	// A "PATCH" is a new draft revision, NOT an edit of v1. v2 carries different config and is unpublished.
	v2, err := s.CreateRevision(ctx, org, project, profileID, []byte(`{"model":"model-b","tools":["file","shell"],"instructions":"v2"}`))
	if err != nil {
		t.Fatalf("revise -> v2: %v", err)
	}
	if v2.ID == v1.ID || v2.RevisionNumber != 2 {
		t.Fatalf("revise produced id=%s number=%d, want a NEW revision 2", v2.ID, v2.RevisionNumber)
	}
	v2get, _, _ := s.GetRevision(ctx, org, project, v2.ID)
	if v2get.Published {
		t.Fatal("a revise must produce a DRAFT, but v2 came back published")
	}

	// The published v1's raw row is BYTE-identical after the revise: every config column, the tools
	// content, and the publish stamp are unchanged (not just field-equal — the whole row).
	if rawAfter := rawRevisionRow(t, s, v1.ID); rawAfter != rawBefore {
		t.Fatalf("published v1 row mutated by a later revise:\n before=%s\n after =%s", rawBefore, rawAfter)
	}

	// Publish is once-only: re-publishing v1 is a no-op (already published), never a re-stamp.
	again, _, err := s.PublishRevision(ctx, org, project, v1.ID)
	if err != nil {
		t.Fatalf("re-publish v1: %v", err)
	}
	if again {
		t.Fatal("re-publish reported a fresh publish, want a no-op on an already-published revision")
	}
}

// TestAgentRevisionPersistsExtensionFields proves the E12 rider columns are actually written (the insert
// column mapping lines up, spec §28.4): a revision carrying tool_sets + the opaque mcp_connections/skills/
// hooks stores each as a JSONB array. This is the conflict-shield persistence half — wave-2 stores its
// field through this same insert without touching the code again.
func TestAgentRevisionPersistsExtensionFields(t *testing.T) {
	s, org, project := openStore(t)
	ctx := context.Background()

	profileID, err := s.CreateProfile(ctx, org, project, "reviewer")
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	rev, err := s.CreateRevision(ctx, org, project, profileID,
		[]byte(`{"model":"m","tool_sets":["tsrev_a"],"mcp_connections":["mcpc_a"],"skills":["skill_a"],"hooks":["hook_a"]}`))
	if err != nil {
		t.Fatalf("create revision with E12 fields: %v", err)
	}
	if len(rev.ToolSets) != 1 || rev.ToolSets[0] != "tsrev_a" {
		t.Fatalf("returned tool_sets = %v, want [tsrev_a]", rev.ToolSets)
	}
	var toolSets, mcp, skills, hooks string
	err = s.pool.QueryRow(ctx,
		`SELECT tool_sets::text, mcp_connections::text, skills::text, hooks::text FROM agent_revisions WHERE id=$1`, rev.ID).
		Scan(&toolSets, &mcp, &skills, &hooks)
	if err != nil {
		t.Fatalf("read E12 columns: %v", err)
	}
	if toolSets != `["tsrev_a"]` || mcp != `["mcpc_a"]` || skills != `["skill_a"]` || hooks != `["hook_a"]` {
		t.Fatalf("persisted E12 columns = %s / %s / %s / %s, want the four JSONB arrays", toolSets, mcp, skills, hooks)
	}
}

// TestRunTemplateRevisionRejectsIdentityAndDelegation proves the profile-free template surface (AGT-003):
// a template revision publishes and resolves like an agent revision but rejects identity/delegation
// fields — it must not impersonate an agent identity (spec §32.2).
func TestRunTemplateRevisionRejectsIdentityAndDelegation(t *testing.T) {
	s, org, project := openStore(t)
	ctx := context.Background()

	if _, err := s.CreateTemplateRevision(ctx, org, project, "nightly", []byte(`{"model":"m","delegation":{"emit":["child"]}}`)); err == nil {
		t.Fatal("template accepted a delegation field, want it rejected (a template is not an agent)")
	}
	tr, err := s.CreateTemplateRevision(ctx, org, project, "nightly", []byte(`{"model":"m","tools":["file"],"instructions":"run nightly"}`))
	if err != nil {
		t.Fatalf("create template revision: %v", err)
	}
	if tr.RevisionNumber != 1 {
		t.Fatalf("template revision number = %d, want 1", tr.RevisionNumber)
	}
	published, _, err := s.PublishTemplateRevision(ctx, org, project, tr.ID)
	if err != nil || !published {
		t.Fatalf("publish template = %v err = %v, want published", published, err)
	}
}
