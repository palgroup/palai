//go:build security

// Package tenancy, continued (A.2 Task 2): the isolation boundary moves from ORGANIZATION to PROJECT.
// Task 1 (c766c2ec) made every WithTenant connection carry a mandatory project; this migration
// (000062_tenant_policy_by_project) rekeys the row-level-security policy itself so the database enforces
// that same boundary at the project grain rather than the organization grain — organization_id stays on
// every row and palai.org_id keeps being set (Task 4 removes both), but it is no longer what the policy
// reads.
//
// Both tests below deliberately leave palai.org_id UNSET (asProject sets ONLY palai.project_id): if the
// old organization-keyed policy were still load-bearing, a connection that never named an organization
// would see nothing at all, and a false PASS here would actually mean "still gated by org_id, project_id
// is decorative". Proving isolation holds with org_id absent is the only way to show project_id is doing
// the work.
package tenancy

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// asProject runs fn inside one transaction whose palai.project_id GUC names project. It mirrors asOrg
// above but sets the NEW tenant key instead of the old one, and never touches palai.org_id at all — see
// the package comment for why that absence is the point of every test in this file.
func (s *suite) asProject(t *testing.T, project string, fn func(tx pgx.Tx)) {
	t.Helper()
	ctx := context.Background()
	tx, err := s.app.Begin(ctx)
	if err != nil {
		t.Fatalf("begin app tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if project != "" {
		if _, err := tx.Exec(ctx, "SELECT set_config('palai.project_id', $1, true)", project); err != nil {
			t.Fatalf("set project GUC: %v", err)
		}
	}
	fn(tx)
}

// runOf reads a tenant's seeded run id as the OWNER (bypasses RLS), matching the projectOf/scheduleOf/
// bindingOf helpers in tenancy_test.go.
func (s *suite) runOf(t *testing.T, org string) string {
	t.Helper()
	var id string
	if err := s.owner.QueryRow(context.Background(),
		`SELECT id FROM runs WHERE organization_id = $1`, org).Scan(&id); err != nil {
		t.Fatalf("read run of %s: %v", org, err)
	}
	return id
}

// TestPolicyIsolatesProjectsWithinOneInstallation is A.2 Task 2's headline claim: two projects living in
// the same installation cannot see each other's rows, and the confinement holds on project_id ALONE —
// palai.org_id is never set in this test.
func TestPolicyIsolatesProjectsWithinOneInstallation(t *testing.T) {
	s := newSuite(t)
	projectA := s.projectOf(t, s.orgA)
	projectB := s.projectOf(t, s.orgB)
	runA := s.runOf(t, s.orgA)

	s.asProject(t, projectA, func(tx pgx.Tx) {
		rows, err := tx.Query(context.Background(), `SELECT id FROM runs ORDER BY id`)
		if err != nil {
			t.Fatalf("select runs: %v", err)
		}
		defer rows.Close()
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan run id: %v", err)
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate runs: %v", err)
		}
		if len(ids) != 1 || ids[0] != runA {
			t.Fatalf("project A's connection (palai.org_id UNSET) saw %v, want exactly [%s]", ids, runA)
		}

		// The deliberate attack, not the accidental omission: project B's real id, bound directly into
		// the WHERE clause, connection still scoped to project A.
		var foreign int
		if err := tx.QueryRow(context.Background(),
			`SELECT count(*) FROM runs WHERE project_id = $1`, projectB).Scan(&foreign); err != nil {
			t.Fatalf("count project B's runs named directly: %v", err)
		}
		if foreign != 0 {
			t.Fatalf("project A's connection saw %d row(s) of project B named directly; the policy did not confine it", foreign)
		}
	})
}

// TestInstallationWideRowsAreVisibleToEveryProject is the subtlety this task's brief names as the easiest
// thing to get wrong. budgets/quotas (migration 000032) use project_id = ” to mean "covers the whole
// organization"; once organizations are gone that means "the whole installation", and such a row MUST
// stay visible to every project. A naive project_id = current_setting(...) policy hides it instead — the
// budget then applies to nothing, every spend passes, and no test goes red on its own, because an
// unenforced limit is silent rather than an error.
func TestInstallationWideRowsAreVisibleToEveryProject(t *testing.T) {
	s := newSuite(t)
	projectA := s.projectOf(t, s.orgA)

	budgetID := newID("bud")
	if _, err := s.owner.Exec(context.Background(),
		`INSERT INTO budgets (id, project_id, meter_prefix, limit_quantity) VALUES ($1, '', 'model.', 1000)`,
		budgetID); err != nil {
		t.Fatalf("seed installation-wide budget: %v", err)
	}

	s.asProject(t, projectA, func(tx pgx.Tx) {
		var visible int
		if err := tx.QueryRow(context.Background(),
			`SELECT count(*) FROM budgets WHERE id = $1`, budgetID).Scan(&visible); err != nil {
			t.Fatalf("count installation-wide budget: %v", err)
		}
		if visible != 1 {
			t.Fatalf("installation-wide budget (project_id='') is invisible to project A's connection (saw %d row(s) instead of 1); "+
				"a budget that applies to nothing is a limit that fails open, and it fails open SILENTLY", visible)
		}
	})
}
