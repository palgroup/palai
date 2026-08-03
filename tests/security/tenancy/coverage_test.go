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

// installationWideOnly are the tables this task's own sweep found carrying organization_id with no
// project_id column at all — each one predates per-project structure and is genuinely organization-wide,
// not merely unswept by accident. None of these is news: every one already has its own documented
// exception (storage/tenant.go's WithOrgScope doc, or 000062's own header) for exactly this reason. Once
// organizations are gone (Task 4), each such table has exactly one project's worth of installation, so
// staying keyed on organization_id costs nothing — it is the SAME single boundary project_id would give,
// under the column that already carries it. A NEW table that lands here unnamed is exactly what this
// allowlist exists to catch: it forces the same three-way decision (project column / installation-wide /
// drop) the brief for this task asked for, rather than letting it slide as an accident.
var installationWideOnly = map[string]bool{
	// projects — an organization's own project catalogue. A project cannot be scoped to itself; identity's
	// org-wide provisioning surface (Store.orgScope, apps/control-plane/internal/identity/store.go) reads
	// and writes it under storage.WithOrgScope, structurally the same as api_keys/principals (000062's
	// named exclusions) even though 000062's own sweep loop never had to name it — projects has no
	// project_id to sweep, so it was never a candidate for accidental inclusion in the first place.
	"projects": true,
	// secret_refs (000031) — the org-scoped secret store fronting the env-file bridge
	// (identity.SecretStore.Resolve). Documented in storage/tenant.go's WithOrgScope comment.
	"secret_refs": true,
	// environments, environment_values (000046) — an agent's named env-var groups. Documented in
	// storage/tenant.go's WithOrgScope comment and automation/agents.go's verifyEnvironment.
	"environments":       true,
	"environment_values": true,
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
