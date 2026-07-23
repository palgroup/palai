//go:build component

// The metrics component test scrapes the real collector against a real Postgres seeded with TWO
// tenants. It proves the aggregate queries run under the WithSystemScope escape hatch across the whole
// installation (RLS admits them), and — the load-bearing guarantee — that the unauthenticated
// exposition carries the installation-wide counts but NONE of the real organization/project ids it
// counted. Runs only under `make test-component` (which exports PALAI_COMPONENT_POSTGRES_URL).
//
// palai_runs / palai_queue_ready_depth are installation-aggregate (that is the point — an operator
// wants the whole-install queue/run count), so the shared component Postgres is already populated by
// the packages that ran before this one. The test therefore measures the DELTA its own seed adds
// (scrape → seed → scrape → assert after−before), not an absolute count, so it is correct whether the
// DB starts empty (metrics run alone) or full (the postgres suite).
package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
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

	collector := New(pool, nil, nil, nil, "/")
	before := scrapeMetrics(t, collector)
	beforeRuns := runStateCounts(before)
	beforeQueue := scalarValue(t, before, "palai_queue_ready_depth")

	// Two distinct tenants, so an aggregate that GROUPs by tenant (a leak) would show two rows and a
	// scan of the output would find their ids. The metric must instead show ONE count per lifecycle state.
	orgA, projA := "org_metrics_A", "proj_metrics_A"
	orgB, projB := "org_metrics_B", "proj_metrics_B"
	seedTenantWithRuns(t, ctx, pool, orgA, projA, map[string]int{"running": 2, "completed": 1})
	seedTenantWithRuns(t, ctx, pool, orgB, projB, map[string]int{"completed": 1, "failed": 1})
	// A claimable queued job for tenant B, so the queue-depth query counts it.
	exec(t, ctx, pool, `INSERT INTO durable_jobs (id, organization_id, project_id) VALUES ($1,$2,$3)`, "job_metrics_1", orgB, projB)

	after := scrapeMetrics(t, collector)

	// DB queries succeeded against the real schema under system scope.
	if !strings.Contains(after, "palai_db_up 1") {
		t.Errorf("palai_db_up should be 1 (queries ran):\n%s", after)
	}
	// The aggregate counts MY seed across both tenants: running +2, completed +2, failed +1. Measured
	// as a delta so a shared DB already holding other packages' rows does not skew it.
	afterRuns := runStateCounts(after)
	for state, want := range map[string]int64{"running": 2, "completed": 2, "failed": 1} {
		if got := afterRuns[state] - beforeRuns[state]; got != want {
			t.Errorf("palai_runs{state=%q} delta = %d, want %d (before=%d after=%d)",
				state, got, want, beforeRuns[state], afterRuns[state])
		}
	}
	if got := scalarValue(t, after, "palai_queue_ready_depth") - beforeQueue; got != 1 {
		t.Errorf("palai_queue_ready_depth delta = %v, want 1 (the one seeded claimable job)", got)
	}

	// THE GUARANTEE: no real tenant identifier appears anywhere in the unauthenticated exposition.
	for _, id := range []string{orgA, projA, orgB, projB} {
		if strings.Contains(after, id) {
			t.Errorf("exposition leaked tenant identifier %q — /metrics must expose aggregates only:\n%s", id, after)
		}
	}
}

func scrapeMetrics(t *testing.T, collector *Collector) string {
	t.Helper()
	rec := httptest.NewRecorder()
	collector.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape status = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

var runStateLine = regexp.MustCompile(`palai_runs\{state="([^"]+)"\} (\d+)`)

// runStateCounts parses the palai_runs{state=...} lines into a state→count map.
func runStateCounts(out string) map[string]int64 {
	counts := map[string]int64{}
	for _, m := range runStateLine.FindAllStringSubmatch(out, -1) {
		n, _ := strconv.ParseInt(m[2], 10, 64)
		counts[m[1]] = n
	}
	return counts
}

// scalarValue reads a single unlabeled `name <value>` sample from the exposition.
func scalarValue(t *testing.T, out, name string) float64 {
	t.Helper()
	m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + ` (\S+)$`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("metric %q not found in exposition:\n%s", name, out)
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		t.Fatalf("metric %q value %q not a float: %v", name, m[1], err)
	}
	return v
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
