//go:build fault

// Package subagents holds the fault-injection proof for ChildRun cancel propagation
// (spec §25.18-19, SUB-005). It runs only under `make test-fault TEST=subagents` (CASE is
// honored too), which starts a throwaway PostgreSQL container and exports
// PALAI_FAULT_POSTGRES_URL. The build tag keeps it out of the credential-free, Docker-free
// unit tier.
//
// It exercises packages/coordinator against real Postgres: a parent run with a live ChildRun is
// canceled, the cancel propagates to the child, and the terminal accounting stays consistent —
// exactly one terminal per run, and a late child completion (its in-flight attempt finishing
// after the cancel) is dropped at the database rather than overwriting the canceled row.
package subagents

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/packages/coordinator"
	statemachines "github.com/palgroup/palai/packages/state-machines"
)

func faultURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("PALAI_FAULT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_FAULT_POSTGRES_URL is required; run make test-fault TEST=subagents")
	}
	return url
}

func openStore(t *testing.T) *coordinator.Store {
	t.Helper()
	store, err := coordinator.Open(context.Background(), faultURL(t))
	if err != nil {
		t.Fatalf("coordinator.Open() error = %v", err)
	}
	t.Cleanup(store.Close)
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	return store
}

func newID(prefix string) string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return prefix + "_" + hex.EncodeToString(raw[:])
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q error = %v", sql, err)
	}
}

func count(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count %q error = %v", sql, err)
	}
	return n
}

func runState(t *testing.T, pool *pgxpool.Pool, runID string) string {
	t.Helper()
	var state string
	if err := pool.QueryRow(context.Background(), `SELECT state FROM runs WHERE id=$1`, runID).Scan(&state); err != nil {
		t.Fatalf("read run state %s error = %v", runID, err)
	}
	return state
}

// seedParentWithChild creates org -> project -> session -> parent (root) run + response and one
// live ChildRun + response, both running. Returns the tenant and the run/response ids.
func seedParentWithChild(t *testing.T, pool *pgxpool.Pool) (tenant coordinator.Tenant, parentRun, parentResp, childRun, childResp string) {
	t.Helper()
	tenant = coordinator.Tenant{Organization: newID("org"), Project: newID("prj")}
	sessionID := newID("ses")
	parentRun, parentResp = newID("run"), newID("resp")
	childRun, childResp = newID("run"), newID("resp")
	exec(t, pool, `INSERT INTO organizations (id) VALUES ($1)`, tenant.Organization)
	exec(t, pool, `INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, tenant.Project, tenant.Organization)
	exec(t, pool, `INSERT INTO sessions (id, organization_id, project_id) VALUES ($1, $2, $3)`, sessionID, tenant.Organization, tenant.Project)
	exec(t, pool, `INSERT INTO responses (id, organization_id, project_id, session_id, state, input) VALUES ($1,$2,$3,$4,'in_progress','{}')`,
		parentResp, tenant.Organization, tenant.Project, sessionID)
	exec(t, pool, `INSERT INTO runs (id, organization_id, project_id, session_id, response_id, state) VALUES ($1,$2,$3,$4,$5,'running')`,
		parentRun, tenant.Organization, tenant.Project, sessionID, parentResp)
	// The ChildRun shares the session (parent_run_id set, so it does not consume the root slot).
	exec(t, pool, `INSERT INTO responses (id, organization_id, project_id, session_id, state, input) VALUES ($1,$2,$3,$4,'in_progress','{}')`,
		childResp, tenant.Organization, tenant.Project, sessionID)
	exec(t, pool, `INSERT INTO runs (id, organization_id, project_id, session_id, response_id, state, parent_run_id, depth) VALUES ($1,$2,$3,$4,$5,'running',$6,1)`,
		childRun, tenant.Organization, tenant.Project, sessionID, childResp, parentRun)
	return tenant, parentRun, parentResp, childRun, childResp
}

// TestParentCancelPropagatesToChildren proves SUB-005 (spec §25.18-19): canceling a parent run
// drives its live ChildRun to the canceled terminal, the terminal accounting is consistent (one
// terminal per run), a late child completion is dropped at the database, and a repeated cancel is
// a monotonic no-op.
func TestParentCancelPropagatesToChildren(t *testing.T) {
	store := openStore(t)
	pool := store.Pool()
	ctx := context.Background()
	tenant, parentRun, parentResp, childRun, childResp := seedParentWithChild(t, pool)
	canceled, _ := json.Marshal(map[string]any{"output": []any{}, "usage": map[string]any{}, "model": ""})

	// Cancel the parent — the run transition, its terminal projection, then propagation to children.
	if _, err := store.ApplyRunTransition(ctx, tenant, parentRun, statemachines.RunCmdCancel); err != nil {
		t.Fatalf("cancel parent run error = %v", err)
	}
	if err := store.FinalizeResponse(ctx, tenant, parentResp, "canceled", canceled); err != nil {
		t.Fatalf("finalize parent canceled error = %v", err)
	}
	n, err := store.CancelChildren(ctx, tenant, parentRun, canceled)
	if err != nil {
		t.Fatalf("CancelChildren error = %v", err)
	}
	if n != 1 {
		t.Fatalf("CancelChildren canceled = %d children, want 1", n)
	}

	// Both runs reached the canceled terminal, each with exactly one terminal event.
	if s := runState(t, pool, parentRun); s != "canceled" {
		t.Fatalf("parent run state = %q, want canceled", s)
	}
	if s := runState(t, pool, childRun); s != "canceled" {
		t.Fatalf("child run state = %q, want canceled (propagation)", s)
	}
	if c := count(t, pool, `SELECT count(*) FROM events WHERE response_id=$1 AND type='run.canceled.v1'`, childResp); c != 1 {
		t.Fatalf("child run.canceled.v1 count = %d, want 1 (single terminal)", c)
	}

	// A late child completion — its in-flight attempt finishing just after the cancel — must be
	// dropped at the database (conditional UpdateResponse), leaving the canceled row intact.
	completed, _ := json.Marshal(map[string]any{"output": []any{map[string]any{"type": "message", "content": "late"}}, "model": "fake"})
	if err := store.FinalizeResponse(ctx, tenant, childResp, "completed", completed); err != nil {
		t.Fatalf("late child finalize returned error = %v, want a silent no-op", err)
	}
	if c := count(t, pool, `SELECT count(*) FROM responses WHERE id=$1 AND state='canceled'`, childResp); c != 1 {
		t.Fatalf("child response after late completion is not canceled — the late terminal overwrote the canceled row")
	}

	// A repeated cancel is a monotonic no-op: no second terminal, no re-canceled child.
	if n, err := store.CancelChildren(ctx, tenant, parentRun, canceled); err != nil || n != 0 {
		t.Fatalf("re-cancel CancelChildren = (%d, %v), want (0, nil) — the child is already terminal", n, err)
	}
	if c := count(t, pool, `SELECT count(*) FROM events WHERE response_id=$1 AND type='run.canceled.v1'`, childResp); c != 1 {
		t.Fatalf("child run.canceled.v1 after re-cancel = %d, want 1 (monotonic terminal)", c)
	}
}
