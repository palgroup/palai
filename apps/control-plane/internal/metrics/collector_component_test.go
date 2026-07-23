//go:build component

// The metrics component test scrapes the real collector against a throwaway Postgres seeded with TWO
// tenants. It proves the aggregate queries run under the WithSystemScope escape hatch across the whole
// installation (RLS admits them), and — the load-bearing guarantee — that the unauthenticated
// exposition carries the installation-wide counts but NONE of the real organization/project ids it
// counted. Runs only under `make test-component` (which exports PALAI_COMPONENT_POSTGRES_URL).
package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/apps/control-plane/internal/store"
	"github.com/palgroup/palai/storage"
)

func TestMetricsScrapeAggregatesAllTenantsWithoutLeak(t *testing.T) {
	pgURL := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-component TEST=metrics")
	}
	ctx := context.Background()
	repo, err := store.Open(ctx, pgURL)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	pool := repo.Spine().Pool()

	// Two distinct tenants, so an aggregate that GROUPs by tenant (a leak) would show two rows and a
	// scan of the output would find their ids. The metric must instead show ONE count per lifecycle state.
	orgA, projA := "org_metrics_A", "proj_metrics_A"
	orgB, projB := "org_metrics_B", "proj_metrics_B"
	seedTenantWithRuns(t, ctx, pool, orgA, projA, map[string]int{"running": 2, "completed": 1})
	seedTenantWithRuns(t, ctx, pool, orgB, projB, map[string]int{"completed": 1, "failed": 1})
	// A claimable queued job for tenant B, so the queue-depth query returns a real number.
	exec(t, ctx, pool, `INSERT INTO durable_jobs (id, organization_id, project_id) VALUES ($1,$2,$3)`, "job_metrics_1", orgB, projB)

	rec := httptest.NewRecorder()
	New(pool, nil, nil, nil, "/").ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape status = %d, want 200", rec.Code)
	}
	out := rec.Body.String()

	// DB queries succeeded against the real schema under system scope.
	if !strings.Contains(out, "palai_db_up 1") {
		t.Errorf("palai_db_up should be 1 (queries ran):\n%s", out)
	}
	// Installation-aggregate counts, summed across BOTH tenants: running=2, completed=2, failed=1.
	for _, want := range []string{
		`palai_runs{state="running"} 2`,
		`palai_runs{state="completed"} 2`,
		`palai_runs{state="failed"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing aggregate %q\n%s", want, out)
		}
	}
	if !regexp.MustCompile(`palai_queue_ready_depth [1-9]`).MatchString(out) {
		t.Errorf("palai_queue_ready_depth should reflect the seeded queued job:\n%s", out)
	}

	// THE GUARANTEE: no real tenant identifier appears anywhere in the unauthenticated exposition.
	for _, id := range []string{orgA, projA, orgB, projB} {
		if strings.Contains(out, id) {
			t.Errorf("exposition leaked tenant identifier %q — /metrics must expose aggregates only:\n%s", id, out)
		}
	}
}

func seedTenantWithRuns(t *testing.T, ctx context.Context, pool *pgxpool.Pool, org, project string, statesByCount map[string]int) {
	t.Helper()
	exec(t, ctx, pool, `INSERT INTO organizations (id) VALUES ($1)`, org)
	exec(t, ctx, pool, `INSERT INTO projects (id, organization_id) VALUES ($1,$2)`, project, org)
	// One session per run: runs_one_active_root_per_session forbids two active root runs sharing a
	// session, and the aggregate counts runs, not sessions, so a session apiece keeps the seed honest.
	i := 0
	for state, count := range statesByCount {
		for n := 0; n < count; n++ {
			i++
			suffix := state + string(rune('a'+i))
			session := project + "_sess_" + suffix
			exec(t, ctx, pool, `INSERT INTO sessions (id, organization_id, project_id) VALUES ($1,$2,$3)`, session, org, project)
			exec(t, ctx, pool, `INSERT INTO runs (id, organization_id, project_id, session_id, state) VALUES ($1,$2,$3,$4,$5)`,
				project+"_run_"+suffix, org, project, session, state)
		}
	}
}

func exec(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(storage.WithSystemScope(ctx), sql, args...); err != nil {
		t.Fatalf("seed exec %q: %v", sql, err)
	}
}
