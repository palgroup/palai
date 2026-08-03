//go:build component

// Package identity_test also holds the real-PostgreSQL component test for storage.OrganizationForProject's
// cache (A.2 Task 3 follow-up: the function was measured uncached at 96 call sites, worst on the SSE events
// pump, which re-resolves every 500ms poll tick). identity already has a neighbour that drives
// OrganizationForProject directly against a real spine (secrets_component_test.go's
// TestSecretRefCrossOrgResolveDenied), so this rides the SAME `go test ./apps/control-plane/internal/identity`
// invocation in scripts/test/component — no allow-list edit needed for it to actually run.
package identity_test

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/apps/control-plane/internal/identity"
	"github.com/palgroup/palai/storage"
)

// queryCounter is a pgx QueryTracer that counts how many times each distinct SQL statement runs on its pool.
type queryCounter struct {
	mu     sync.Mutex
	counts map[string]int
}

func (c *queryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	c.mu.Lock()
	if c.counts == nil {
		c.counts = map[string]int{}
	}
	c.counts[strings.TrimSpace(data.SQL)]++
	c.mu.Unlock()
	return ctx
}

func (c *queryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (c *queryCounter) count(sql string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[strings.TrimSpace(sql)]
}

// openTracedPool opens a SEPARATE pool against the same component database, with every query it runs
// counted. It has to be a separate pool from openHarness's: pgxpool.Config.ConnConfig.Tracer can only be
// set at pool creation, and openHarness's pool is already open by the time a test provisions a tenant.
func openTracedPool(t *testing.T, counter *queryCounter) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-component TEST=postgres")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse pool config: %v", err)
	}
	cfg.ConnConfig.Tracer = counter
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open traced pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestOrganizationForProjectResolvesTheDatabaseAtMostOnce proves the cache: resolving the SAME project twice
// queries `SELECT organization_id FROM projects` exactly ONCE, not twice. Against the pre-cache
// implementation this is RED (2 queries) — the regression this guards is the SSE events pump re-resolving
// every 500ms poll tick (apps/control-plane/api/events.go), which is the query this test counts.
func TestOrganizationForProjectResolvesTheDatabaseAtMostOnce(t *testing.T) {
	cs := openHarness(t)
	idstore := identity.New(cs.Pool())
	_, project, _ := provisionOrg(t, idstore, "org-cache-hit")

	var counter queryCounter
	traced := openTracedPool(t, &counter)
	ctx := context.Background()

	org1, err := storage.OrganizationForProject(ctx, traced, project)
	if err != nil {
		t.Fatalf("OrganizationForProject (1st call) error = %v", err)
	}
	org2, err := storage.OrganizationForProject(ctx, traced, project)
	if err != nil {
		t.Fatalf("OrganizationForProject (2nd call) error = %v", err)
	}
	if org1 != org2 {
		t.Fatalf("OrganizationForProject(%s) = %q then %q, want the same organization both times", project, org1, org2)
	}

	if got := counter.count(storage.Query("OrganizationForProject")); got != 1 {
		t.Fatalf("OrganizationForProject queried the database %d times for the same project, want 1 "+
			"(the second call should be served from the cache)", got)
	}
}

// TestOrganizationForProjectDoesNotCacheAMiss proves the cache's other half: a project that does not exist
// yet must not be cached as an answer. It resolves the SAME id before and after the project is seeded — a
// cache that stored the miss would keep returning the earlier error forever.
func TestOrganizationForProjectDoesNotCacheAMiss(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()

	var counter queryCounter
	traced := openTracedPool(t, &counter)

	project := newID("proj")
	if _, err := storage.OrganizationForProject(ctx, traced, project); err == nil {
		t.Fatalf("OrganizationForProject(%s) succeeded before the project existed", project)
	}

	org := newID("org")
	if _, err := cs.Pool().Exec(storage.WithSystemScope(ctx), `INSERT INTO organizations (id) VALUES ($1)`, org); err != nil {
		t.Fatalf("seed organization: %v", err)
	}
	if _, err := cs.Pool().Exec(storage.WithSystemScope(ctx), `INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, project, org); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	got, err := storage.OrganizationForProject(ctx, traced, project)
	if err != nil {
		t.Fatalf("OrganizationForProject(%s) error after the project was created = %v", project, err)
	}
	if got != org {
		t.Fatalf("OrganizationForProject(%s) = %q, want %q", project, got, org)
	}
}
