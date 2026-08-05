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
	ctx := storage.WithTenant(context.Background(), "prj_a2t1_real")

	var got string
	if err := pool.QueryRow(ctx, `SELECT current_setting('palai.project_id', true)`).Scan(&got); err != nil {
		t.Fatalf("acquire under a real project failed: %v", err)
	}
	if got != "prj_a2t1_real" {
		t.Fatalf("palai.project_id = %q, want prj_a2t1_real", got)
	}
}

// TestTenantConnectionWithNoProjectIsRefused is the negative half, TEN-001's new claim (A.2 Task 1): a
// tenant scope that names no project must not open a connection at all. Before Task 1 it opened fine and
// the query saw the whole organization; now that A.2 Task 6 has removed organizations there is nothing
// left for it to mean but "every project, full stop" — the absence of a boundary, which is why the
// acquisition is refused rather than allowed to read wide.
func TestTenantConnectionWithNoProjectIsRefused(t *testing.T) {
	pool := openApplicationPool(t)
	ctx := storage.WithTenant(context.Background(), "")

	// QueryRow, not Query: QueryRow's Scan always releases the underlying connection even on error,
	// where a bare Query's Rows must be Close()'d explicitly — an unclosed Rows here leaves the
	// acquisition outstanding and t.Cleanup(pool.Close) hangs forever waiting for it back.
	err := pool.QueryRow(ctx, `SELECT 1`).Scan(new(int))
	if err == nil {
		t.Fatal("a tenant connection with no project was acquired — that is the absence of a boundary, not one")
	}
	if !errors.Is(err, storage.ErrProjectRequired) {
		t.Fatalf("acquire failed with %v, want it to wrap storage.ErrProjectRequired", err)
	}

	// The same refusal for the fully-forgotten scope — a context nobody ever called WithTenant,
	// WithInstallationScope or WithSystemScope on. Before this task that context opened fine and read zero rows;
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

// TestInstallationScopeStillOpensWithNoProject proves the one deliberate, narrow exception to the
// project-required rule survives. A.2 Task 6 replaced storage.WithOrgScope with
// storage.WithInstallationScope, which takes NO id at all.
//
// THE JUSTIFICATION THIS COMMENT USED TO GIVE HAS BEEN FALSIFIED, and it is restated rather than dropped
// because the exception is still here and still needs one. It said the two production callers read
// tables carrying no project column — secret_refs via identity.SecretStore.Resolve, environments via
// automation's verifyEnvironment. Both halves stopped being true:
//
//   - the TABLES gained the column. secret_refs and environments each carry project_id now, under
//     `tenant_isolation` keyed on it, with secret_refs_name_version_key UNIQUE (project_id, name,
//     version) and environments_name_key UNIQUE (project_id, name).
//   - the CALLERS went with it. The only production caller left is the host quarantine
//     (packages/coordinator/workspace.go: QuarantineHost, IsHostQuarantined, ListQuarantinedHosts), and
//     host_quarantine really does carry no project column, because a machine belongs to no tenant.
//     storage/tenant.go's own scope struct says exactly that, and is the authority to check against.
//
// The one thing that did NOT change is what this test asserts, which is why it stays: the connection
// opens and publishes an EMPTY palai.project_id rather than being refused. There is no second GUC left
// to check — palai.org_id went with Task 6, applyScope having been its only writer.
//
// A CAVEAT THE NEW SENTENCE MUST CARRY, or it is as wrong as the old one: "keyed on project_id" is the
// rule for rows written from now on. Rows that predate it keep an EMPTY project_id, which 000002's own
// contract makes readable to every scope — so an installation with legacy secrets still resolves those
// installation-wide, and nothing in this test or that column name says otherwise.
func TestInstallationScopeStillOpensWithNoProject(t *testing.T) {
	pool := openApplicationPool(t)
	ctx := storage.WithInstallationScope(context.Background())

	var gotProject string
	if err := pool.QueryRow(ctx,
		`SELECT current_setting('palai.project_id', true)`,
	).Scan(&gotProject); err != nil {
		t.Fatalf("installation-scoped acquire failed: %v", err)
	}
	if gotProject != "" {
		t.Fatalf("palai.project_id = %q, want empty — WithInstallationScope must not narrow by project", gotProject)
	}
}

// TestOrgIDGUCIsGone is the other half of that removal, and it is the half a deletion cannot prove on its
// own: applyScope no longer writes palai.org_id, so an acquisition must leave it UNSET rather than
// carrying a stale value from whatever the pooled connection published last.
//
// current_setting(..., true) answers NULL for a GUC nothing has set, which is why this scans into a
// *string and asserts nil rather than comparing to the empty string: a set_config of an empty palai.org_id would
// answer the empty string and pass a comparison, and that is exactly the residue being forbidden.
func TestOrgIDGUCIsGone(t *testing.T) {
	pool := openApplicationPool(t)
	ctx := storage.WithTenant(context.Background(), "prj_a2t6_no_org_guc")

	var gotOrg *string
	if err := pool.QueryRow(ctx, `SELECT current_setting('palai.org_id', true)`).Scan(&gotOrg); err != nil {
		t.Fatalf("acquire failed: %v", err)
	}
	if gotOrg != nil {
		t.Fatalf("palai.org_id = %q, want unset — nothing writes it since A.2 Task 6, and a GUC that "+
			"still answers reads as a boundary that is still there", *gotOrg)
	}
}
