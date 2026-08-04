//go:build security

// Package tenancy, continued (A.2 Task 3): the two-directional sweep Task 2's own migration comment
// promises but does not itself prove. 000062 swept every table carrying project_id onto the new
// project-keyed policy, EXCLUDING three named tables (api_keys, principals, usage_ledger) kept on the old
// organization-keyed rule on purpose. This file asks the two questions a one-directional catalogue walk
// cannot both answer:
//
//   - a table carrying project_id but NOT under row-level security at all — a walk over pg_class finds
//     this directly, the same shape TestEveryTenantTableIsRowLevelSecured already guards.
//   - a table carrying organization_id but NO project_id — the one 000062's sweep, keyed off project_id's
//     presence, could never have reached, because it never looks at a table lacking that column at all.
//     Such a table is NOT caught by TestEveryTenantTableIsRowLevelSecured either: that test only fails a
//     table that is not under RLS at all, and an organization_id-only table nobody migrated is still very
//     much under the OLD org-keyed policy — secured, just against the boundary this epic is retiring.
package tenancy

import (
	"context"
	"testing"
)

// installationWideOnly are the tables the sweep below finds carrying organization_id and NO project_id —
// the shape no catalogue-driven policy rule could have reached, so each needs a decision recorded by hand.
// Three of the four are installation-wide, which is what names the list; `projects` is the exception and
// says so at its own entry. A.2 Task 3 found them by sweeping for organization_id with no
// project_id; A.2 Task 6 removed the organization and 000066 keys them where the sweep said they were.
//
// THE COST IS NAMED HERE BECAUSE THIS IS THE LIST A READER CHECKS. While organizations existed these tables
// had a real boundary and an installation could hold two tenants apart on them. It cannot any more: within
// one installation every project reads every row of these three. That is exactly today's observable
// behaviour (one installation has held one organization since A.2 Task 1), and it is acceptable only under
// Palai's post-A.2 model of one installation per customer. An installation that ever hosts two customers
// needs project_id on these tables FIRST.
//
// A NEW table that lands here unnamed is what this allowlist exists to catch: it forces the same three-way
// decision (project column / installation-wide / drop) rather than letting it slide as an accident.
var installationWideOnly = map[string]bool{
	// secret_refs (000031) — the secret store fronting the env-file bridge (identity.SecretStore.Resolve).
	// A.2 Task 6's 000066 keys it on the INSTALLATION: with organizations gone and no project_id column,
	// there is nothing narrower left. TestSecretRefNamesAreInstallationWide is the executable form.
	"secret_refs": true,
	// environments, environment_values (000046) — an agent's named env-var groups, the same posture and
	// the same 000066 decision. TestEnvironmentsAreInstallationWide and
	// TestInstallationWideTablesAreVisibleToEveryTenant assert it in both directions.
	"environments":       true,
	"environment_values": true,
	// projects — NOT installation-wide, and it is on this list because the list's QUESTION is narrower than
	// its name: the sweep asks "carries organization_id, carries no project_id, so no catalogue-driven rule
	// could have reached it — was that decided?", and for projects the answer is yes and it is the
	// TIGHTEST of the four. 000066 keys its policy on its own `id`, which IS the project column under the
	// name a project's own row gives it, so a project-scoped connection sees exactly its own row where it
	// used to see every project of its organization. Listed rather than special-cased, because an entry
	// with its reason is what the sweep exists to force; the moment organization_id leaves this table the
	// sweep stops finding it and this entry goes with it.
	"projects": true,
}

// queryNames runs query against the migration owner — a catalogue read needs no tenant scope, because
// pg_class/information_schema hold no tenant data — and returns the first column of every row.
func queryNames(t *testing.T, s *suite, query string, args ...any) []string {
	t.Helper()
	rows, err := s.owner.Query(context.Background(), query, args...)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	return out
}

// TestEveryTenantTableIsCoveredAndNoneWasSkipped is the two-directional sweep A.2 Task 3 requires; see the
// package comment above for why a one-directional walk cannot answer both halves.
func TestEveryTenantTableIsCoveredAndNoneWasSkipped(t *testing.T) {
	s := newSuite(t)

	uncovered := queryNames(t, s, `
		SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname='public' AND c.relkind='r' AND c.relrowsecurity = false
		   AND EXISTS (SELECT 1 FROM information_schema.columns col
		                WHERE col.table_schema='public' AND col.table_name=c.relname
		                  AND col.column_name='project_id')`)
	if len(uncovered) != 0 {
		t.Errorf("table(s) carrying project_id with no row-level security at all: %v", uncovered)
	}

	orphans := queryNames(t, s, `
		SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname='public' AND c.relkind='r'
		   AND EXISTS (SELECT 1 FROM information_schema.columns col
		                WHERE col.table_schema='public' AND col.table_name=c.relname
		                  AND col.column_name='organization_id')
		   AND NOT EXISTS (SELECT 1 FROM information_schema.columns col
		                    WHERE col.table_schema='public' AND col.table_name=c.relname
		                      AND col.column_name='project_id')`)
	for _, name := range orphans {
		if !installationWideOnly[name] {
			t.Errorf("table %q carries organization_id but no project_id and is not on installationWideOnly — "+
				"decide: add a project column, accept it as installation-wide (name it here with why), or drop it", name)
		}
	}
	// The allowlist itself must name only tables that are actually orphaned this way — an entry for a
	// table that has since grown a project_id column (or been dropped) is a stale exception, not a
	// decision, and TestEveryTenantTableIsRowLevelSecured already covers the "does it exist and is it
	// secured" half.
	found := map[string]bool{}
	for _, name := range orphans {
		found[name] = true
	}
	for name := range installationWideOnly {
		if !found[name] {
			t.Errorf("installationWideOnly names %q, but it no longer carries organization_id without project_id — remove the stale entry", name)
		}
	}
}
