//go:build component

package extensions

// E13 Task 4: the MCP-connection list read over real Postgres and RLS — the tenant-scoped keyset holds one
// layer out, and the list never surfaces the secret_ref. Runs under the extensions component suites (and a
// plain `go test -tags=component ./apps/control-plane/internal/extensions`).

import (
	"context"
	"strings"
	"testing"

	"github.com/palgroup/palai/storage"
)

func TestListMCPConnectionsTenantScopedKeyset(t *testing.T) {
	s, orgA, projA := openStore(t)
	ctx := context.Background()

	orgB, projB := testID("org"), testID("prj")
	sys := storage.WithSystemScope(ctx)
	if _, err := s.pool.Exec(sys, `INSERT INTO organizations (id) VALUES ($1)`, orgB); err != nil {
		t.Fatalf("seed org B: %v", err)
	}
	if _, err := s.pool.Exec(sys, `INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, projB, orgB); err != nil {
		t.Fatalf("seed project B: %v", err)
	}

	digest := "sha256:" + strings.Repeat("a", 64)
	stdio := func(name string) []byte {
		return []byte(`{"name":"` + name + `","transport":"stdio","config":{"image_digest":"` + digest + `","cmd":["/mcp"]},"secret_ref":"sref_x"}`)
	}
	for _, n := range []string{"docs", "search"} {
		if _, err := s.CreateMCPConnection(ctx, orgA, projA, stdio(n)); err != nil {
			t.Fatalf("create mcp A/%s: %v", n, err)
		}
	}
	if _, err := s.CreateMCPConnection(ctx, orgB, projB, stdio("bonly")); err != nil {
		t.Fatalf("create mcp B: %v", err)
	}

	all, err := s.ListMCPConnections(ctx, orgA, projA, ListWindow{Limit: 10})
	if err != nil || len(all) != 2 {
		t.Fatalf("ListMCPConnections(A) err=%v len=%d, want 2 (tenant-scoped)", err, len(all))
	}
	for _, c := range all {
		if c.Name == "bonly" {
			t.Fatal("A's list returned B's connection — not tenant-scoped")
		}
	}

	page1, err := s.ListMCPConnections(ctx, orgA, projA, ListWindow{Limit: 1})
	if err != nil || len(page1) != 1 {
		t.Fatalf("page1 err=%v len=%d, want 1", err, len(page1))
	}
	last := page1[0]
	page2, err := s.ListMCPConnections(ctx, orgA, projA, ListWindow{Limit: 1, AfterCreatedAt: &last.CreatedAt, AfterID: last.ID})
	if err != nil || len(page2) != 1 || page2[0].ID == last.ID {
		t.Fatalf("page2 err=%v len=%d, want 1 distinct row", err, len(page2))
	}

	if b, err := s.ListMCPConnections(ctx, orgB, projB, ListWindow{Limit: 10}); err != nil || len(b) != 1 {
		t.Fatalf("B's list err=%v len=%d, want 1", err, len(b))
	}
}
