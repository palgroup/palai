//go:build security

// session_guc_test.go is the A.2 Task 1 corpus: a tenant connection ALWAYS carries a project. Until this
// task a connection could set palai.org_id and leave palai.project_id empty, and the RLS policy read that
// as "every project in the organization" (000029). Once organizations are gone — the point of the whole
// A.2 epic — an empty project would mean every project, full stop: the absence of a boundary, not one.
//
// UNLIKE tenancy_test.go, this file drives the REAL production connection path — storage.OpenPool's
// PrepareConn, storage/embed.go — rather than a raw pgxpool with a hand-rolled GUC set_config. That is the
// point: the invariant being asserted lives in the Go pool wrapper, not in Postgres, so a fake here would
// prove nothing about what storage.OpenPool actually does.
package tenancy

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/storage"
)

// openApplicationPool opens a REAL storage.OpenPool against the same throwaway Postgres the rest of this
// corpus uses — the exact function every control-plane process calls, not a stand-in for it.
func openApplicationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("PALAI_SECURITY_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_SECURITY_POSTGRES_URL is not set; run via `make test-security TEST=tenancy`")
	}
	pool, err := storage.OpenPool(context.Background(), url)
	if err != nil {
		t.Fatalf("storage.OpenPool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestTenantConnectionAlwaysCarriesAProject is the positive half: a connection scoped by
// storage.WithTenant to a real project publishes that exact project on palai.project_id.
func TestTenantConnectionAlwaysCarriesAProject(t *testing.T) {
	pool := openApplicationPool(t)
	ctx := storage.WithTenant(context.Background(), newID("org"), "prj_a2t1_real")

	var got string
	if err := pool.QueryRow(ctx, `SELECT current_setting('palai.project_id', true)`).Scan(&got); err != nil {
		t.Fatalf("acquire under a real project failed: %v", err)
	}
	if got != "prj_a2t1_real" {
		t.Fatalf("palai.project_id = %q, want prj_a2t1_real", got)
	}
}

// TestTenantConnectionWithNoProjectIsRefused is the negative half, TEN-001's new claim (A.2 Task 1): a
// tenant scope that names an organization but no project must not open a connection at all. Before this
// task it opened fine and the query saw the whole organization; that silent widening is exactly what this
// test exists to forbid, because it becomes "every project, full stop" once organizations are gone.
func TestTenantConnectionWithNoProjectIsRefused(t *testing.T) {
	pool := openApplicationPool(t)
	ctx := storage.WithTenant(context.Background(), newID("org"), "")

	// QueryRow, not Query: QueryRow's Scan always releases the underlying connection even on error,
	// where a bare Query's Rows must be Close()'d explicitly — an unclosed Rows here leaves the
	// acquisition outstanding and t.Cleanup(pool.Close) hangs forever waiting for it back.
	err := pool.QueryRow(ctx, `SELECT 1`).Scan(new(int))
	if err == nil {
		t.Fatal("a tenant connection with an organization but no project was acquired — that is the absence of a boundary, not one")
	}
	if !errors.Is(err, storage.ErrProjectRequired) {
		t.Fatalf("acquire failed with %v, want it to wrap storage.ErrProjectRequired", err)
	}

	// The same refusal for the fully-forgotten scope — a context nobody ever called WithTenant,
	// WithOrgScope or WithSystemScope on. Before this task that context opened fine and read zero rows;
	// now it is refused at acquisition, which is the louder failure the zero-scope doc comment promises.
	if err := pool.QueryRow(context.Background(), `SELECT 1`).Scan(new(int)); err == nil {
		t.Fatal("a completely unscoped connection was acquired — a forgotten scope must fail loudly, not silently")
	} else if !errors.Is(err, storage.ErrProjectRequired) {
		t.Fatalf("unscoped acquire failed with %v, want it to wrap storage.ErrProjectRequired", err)
	}
}

// TestSystemScopeStillOpensWithNoProject is the interface this task must not break: the system escape
// hatch (background pumps, the reaper, the dispatch worker's cross-tenant claim) keeps working with no
// project at all.
func TestSystemScopeStillOpensWithNoProject(t *testing.T) {
	pool := openApplicationPool(t)
	ctx := storage.WithSystemScope(context.Background())

	var got string
	if err := pool.QueryRow(ctx, `SELECT current_setting('palai.system', true)`).Scan(&got); err != nil {
		t.Fatalf("system-scoped acquire failed: %v", err)
	}
	if got != "on" {
		t.Fatalf("palai.system = %q, want on", got)
	}
}

// TestOrgScopeStillOpensWithNoProject proves the one deliberate, narrow exception this task carves out:
// storage.WithOrgScope (identity/provisioning, the org-scoped secret_refs store, environments) keeps
// opening a connection with palai.org_id set and palai.project_id empty, exactly as before this task.
func TestOrgScopeStillOpensWithNoProject(t *testing.T) {
	pool := openApplicationPool(t)
	org := newID("org")
	ctx := storage.WithOrgScope(context.Background(), org)

	var gotOrg, gotProject string
	if err := pool.QueryRow(ctx,
		`SELECT current_setting('palai.org_id', true), current_setting('palai.project_id', true)`,
	).Scan(&gotOrg, &gotProject); err != nil {
		t.Fatalf("org-scoped acquire failed: %v", err)
	}
	if gotOrg != org {
		t.Fatalf("palai.org_id = %q, want %q", gotOrg, org)
	}
	if gotProject != "" {
		t.Fatalf("palai.project_id = %q, want empty — WithOrgScope must not narrow by project", gotProject)
	}
}
