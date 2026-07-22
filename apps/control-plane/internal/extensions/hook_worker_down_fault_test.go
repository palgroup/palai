//go:build fault

// The E12 Task 8 fault-injection proof for a down hook worker (spec §28.17, TOL-012, EXT-005). It lives
// in-package because the extensions store is internal (internal-import forces the fault test under
// apps/control-plane). It runs only under `make test-fault CASE=extensions`, which starts a throwaway
// PostgreSQL container and exports PALAI_FAULT_POSTGRES_URL. It exercises the REAL hook dispatch (Fire loads
// hooks from the real spine and invokes them over the REAL T4 signed transport) against a down/erroring local
// worker: a remote policy hook fails CLOSED, its per-hook circuit breaker trips and sheds fast, the control
// plane process stays up, and a DIFFERENT hook still flows. Honest ceiling: the hook worker is our fixture —
// what is proven is the breaker's crash-isolation, not a live model bond.

package extensions

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	remotehttp "github.com/palgroup/palai/adapters/tools/http"
	"github.com/palgroup/palai/packages/coordinator"
)

func faultID(prefix string) string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return prefix + "_" + hex.EncodeToString(raw[:])
}

// openHookFaultStore opens a migrated spine (real Postgres), seeds an org+project, wires the store with the
// REAL signed-transport executor, and returns the store + pool + scope.
func openHookFaultStore(t *testing.T) (*Store, *pgxpool.Pool, string, string) {
	t.Helper()
	url := os.Getenv("PALAI_FAULT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_FAULT_POSTGRES_URL is required; run make test-fault CASE=extensions")
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
	pool := cs.Pool()
	org, project := faultID("org"), faultID("prj")
	if _, err := pool.Exec(ctx, `INSERT INTO organizations (id) VALUES ($1)`, org); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, project, org); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	s := New(pool)
	// The REAL T4 signed transport over the real durable operation ledger. A per-invoke secret is resolved
	// fresh from a throwaway resolver (the worker does not verify it in this fixture — the component tier
	// verifies the HMAC; here the point is the down worker + the breaker).
	s.SetRemoteInvoker(
		remotehttp.NewExecutor(remotehttp.NewOperations(pool)),
		func(_, _ string) ([]byte, error) { return []byte("fault-signing-secret"), nil },
	)
	return s, pool, org, project
}

// insertRemoteFaultHook writes a remote_http hook row DIRECTLY (bypassing the create-time egress gate, which
// would reject a localhost fixture URL): config carries allow_private so the dispatch reaches the local
// httptest worker. Distinct points keep two hooks isolated in the per-point dispatch load.
func insertRemoteFaultHook(t *testing.T, pool *pgxpool.Pool, org, project, name, point, workerURL string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO hooks (id, organization_id, project_id, name, hook_point, category, executor, config, secret_ref)
		 VALUES ($1,$2,$3,$4,$5,'policy','remote_http', jsonb_build_object('url',$6::text,'allow_private',true), 'sref_hook')`,
		faultID("hook"), org, project, name, point, workerURL)
	if err != nil {
		t.Fatalf("insert remote hook %s: %v", name, err)
	}
}

// TestHookWorkerDownFailsClosedTripsBreakerControlPlaneUp is the EXT-005 hook face (spec §28.17): a down/
// erroring remote policy hook worker fails CLOSED every fire, its per-hook breaker trips and sheds FAST, the
// control plane stays up (the store keeps serving), and a DIFFERENT hook still flows — one hook's trip never
// stalls another run.
func TestHookWorkerDownFailsClosedTripsBreakerControlPlaneUp(t *testing.T) {
	ctx := context.Background()
	s, pool, org, project := openHookFaultStore(t)

	// A DOWN/erroring worker: every request is a 503 (server up but refusing to serve), so the signed invoke
	// gets a definite negative answer that counts toward the breaker.
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer down.Close()
	// A HEALTHY worker on a DIFFERENT point: it returns a valid tool-http.v1 allow decision.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"decision":"allow"}}`))
	}))
	defer up.Close()

	insertRemoteFaultHook(t, pool, org, project, "down_guard", HookPointBeforeTool, down.URL)
	insertRemoteFaultHook(t, pool, org, project, "up_guard", HookPointBeforeModel, up.URL)

	beforeTool := func() HookOutcome {
		out, err := s.Fire(ctx, HookEvent{
			Org: org, Project: project, RunID: faultID("run"), Point: HookPointBeforeTool,
			Payload: map[string]any{"tool_name": "push"},
		})
		if err != nil {
			t.Fatalf("Fire(before_tool) infra error = %v", err)
		}
		return out
	}

	// Every fire against the down worker fails CLOSED. After the failure streak trips the breaker (default
	// threshold 5), at least one later fire is SHED FAST — its reason names the open breaker, not a fresh dial.
	sawBreakerShed := false
	for i := 0; i < 8; i++ {
		out := beforeTool()
		if !out.Denied {
			t.Fatalf("fire %d against a down worker was allowed through — a policy hook must fail closed", i)
		}
		if strings.Contains(out.Reason, "circuit breaker open") {
			sawBreakerShed = true
		}
	}
	if !sawBreakerShed {
		t.Fatal("the breaker never tripped: a down worker was dialed on every fire (no fast shed)")
	}

	// The control plane is UP: the store keeps serving reads after the trips (a missing id is a clean
	// ErrHookNotFound, not a panic/hang — the pool is alive).
	if _, err := s.GetHook(ctx, org, project, "hook_missing"); err == nil {
		t.Fatal("GetHook(missing) returned no error, want ErrHookNotFound")
	}

	// A DIFFERENT hook (different id ⇒ different breaker key) on a healthy worker still FLOWS — one hook's
	// trip never sheds another.
	beforeModel, err := s.Fire(ctx, HookEvent{
		Org: org, Project: project, RunID: faultID("run"), Point: HookPointBeforeModel,
		Payload: map[string]any{"tool_count": 1},
	})
	if err != nil {
		t.Fatalf("Fire(before_model) infra error = %v", err)
	}
	if beforeModel.Denied {
		t.Fatalf("a healthy hook was denied — the down hook's breaker trip leaked across hooks: %+v", beforeModel)
	}
}
