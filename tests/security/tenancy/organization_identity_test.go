//go:build security

// Package tenancy, continued (A.2 Task 5): the IDENTITY guard, which the arity guard next door
// deliberately is not.
//
// query_arity_test.go counts HOW MANY bind arguments a call site passes. Task 5's most likely mistake is
// on the other axis: drop `AND organization_id = $2` from a statement, shift `$3` down to `$2`, and then
// delete the WRONG one of the two adjacent string arguments at the call site —
//
//	QueryRow(ctx, storage.Query("GetResponse"), id, tenant.Project)
//	QueryRow(ctx, storage.Query("GetResponse"), id, tenant.Organization)   <- project's slot, org's value
//
// Two parameters, two arguments: the compiler, `go vet` and the arity guard are all three silent, and the
// statement now filters `project_id = <an organization id>`. These tests read the argument's NAME against
// the statement's own SQL, so that edit fails at the file:line where it was made.
//
// Three tests, split by what each can be authoritative about:
//
//   - TestNoStatementStillFiltersByOrganization is the SQL-side inventory. It attributes every
//     organization_id reference in storage/queries to the table it belongs to, and requires a FILTER to
//     survive only where the database still refuses a NULL or a mismatch. It was this task's worklist and
//     is now its completion test.
//   - TestNoQueryBindsAnOrganizationIntoAProjectsSlot is the Go-side identity check described above.
//   - TestTenantScopeIsPublishedOrganizationFirst reads the OTHER adjacent pair of strings this epic
//     keeps touching, the one no query mentions: WithTenant(ctx, project).
//
// None of them replaces the arity guard, and three perturbations recorded in the task report show why:
// leaving tenant.Organization in tenant.Project's slot is invisible to arity and caught here, while
// passing one argument too many is invisible here and caught there.
package tenancy

import (
	"fmt"
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// identityOrgTables are the tables an organization value must still reach, and the reason for each. The
// reasons are two DIFFERENT mechanisms and conflating them is how the epic's own plan came to name nine
// tables with the wrong justification for six of them, so both are measured here, on a database at
// migration 000063 (2026-08-04):
//
//	-- the column itself refuses a NULL (only three; 000063 could not DROP NOT NULL on a primary-key member)
//	SELECT c.relname FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid
//	  JOIN pg_namespace n ON n.oid = c.relnamespace
//	 WHERE n.nspname = 'public' AND a.attname = 'organization_id' AND a.attnum > 0
//	   AND NOT a.attisdropped AND c.relkind = 'r' AND a.attnotnull ORDER BY 1;
//	-- commands, config_revisions, delivered_messages
//
//	-- the row-level-security policy still reads palai.org_id (12 policies, on 12 tables)
//	SELECT tablename FROM pg_policies WHERE schemaname = 'public'
//	   AND (coalesce(qual,'') LIKE '%palai.org_id%' OR coalesce(with_check,'') LIKE '%palai.org_id%')
//	 ORDER BY 1;
//	-- api_keys, delivery_attempts, environment_values, environments, job_attempts, model_route_revisions,
//	-- organizations, principals, projects, schedule_occurrences, secret_refs, usage_ledger
//
// Only the eleven below appear here. The other four of those twelve — delivery_attempts, job_attempts,
// model_route_revisions, schedule_occurrences — have NO organization_id column of their own: their policy
// reads their PARENT's (`EXISTS (SELECT 1 FROM webhook_deliveries d WHERE d.id = delivery_attempts.
// delivery_id AND d.organization_id = current_setting('palai.org_id', true))`). Nothing about a query's
// text can satisfy those; what they require is that the CONNECTION still publishes a real palai.org_id,
// which is storage.scope.organization's job (storage/embed.go, applyScope) and not a bind argument's.
// Measured, with a control, in one rolled-back transaction:
//
//	palai.org_id = 'org_probe' -> delivery_attempts 1 row, webhook_deliveries 1 row
//	palai.org_id = ''          -> delivery_attempts 0 rows, webhook_deliveries 1 row  <- control: the
//	                                                                                    refusal is the
//	                                                                                    policy's, not the
//	                                                                                    harness's
//
// ALL ELEVEN ARE A.2 TASK 6'S WORK, NOT THIS TASK'S. When T6 drops the columns and the policies, entries
// leave this map and the tests below tighten by themselves.
var identityOrgTables = map[string]string{
	"commands":           "organization_id is NOT NULL — composite primary key member, 000063 could not relax it",
	"config_revisions":   "organization_id is NOT NULL — composite primary key member, 000063 could not relax it",
	"delivered_messages": "organization_id is NOT NULL — composite primary key member, 000063 could not relax it",
	"api_keys":           "tenant_isolation compares its own organization_id to palai.org_id",
	"environment_values": "tenant_isolation compares its own organization_id to palai.org_id",
	"environments":       "tenant_isolation compares its own organization_id to palai.org_id",
	"organizations":      "tenant_isolation compares its id to palai.org_id",
	"principals":         "tenant_isolation compares its own organization_id to palai.org_id",
	"projects":           "tenant_isolation compares its own organization_id to palai.org_id",
	"secret_refs":        "tenant_isolation compares its own organization_id to palai.org_id",
	"usage_ledger":       "tenant_isolation compares its own organization_id to palai.org_id",
}

// identityOrgWriteTables are the tables an organization value must still be WRITTEN to. It is a superset
// of identityOrgTables and the extra members are the ones no plan for this epic has named, because the
// mechanism holding them announces itself nowhere in the schema's text: the column participates in a
// UNIQUE index, and a unique index treats NULL as distinct from NULL. Stop writing the column and the
// index stops constraining anything, with no error at write time and no static signal at all.
//
// Measured on a database at migration 000063 (2026-08-04) as the union of three catalogue queries — the
// NOT NULL columns, the policies comparing their own organization_id to palai.org_id, and:
//
//	SELECT DISTINCT c.relname FROM pg_index x JOIN pg_class c ON c.oid = x.indrelid
//	  JOIN pg_namespace n ON n.oid = c.relnamespace
//	 WHERE n.nspname = 'public' AND (x.indisunique OR x.indisprimary)
//	   AND EXISTS (SELECT 1 FROM pg_attribute a
//	                WHERE a.attrelid = c.oid AND a.attnum = ANY (x.indkey)
//	                  AND a.attname = 'organization_id')
//	 ORDER BY 1;   -- 30 indexes over 29 tables (tools carries two)
//
// Ten ON CONFLICT clauses in storage/queries name organization_id first, and those at least fail loudly
// — Postgres answers `there is no unique or exclusion constraint matching the ON CONFLICT specification`.
// The silent half is every other write. Both leave with Task 6, which rebuilds the indexes.
var identityOrgWriteTables = map[string]string{
	"agent_profiles":         "UNIQUE (organization_id, project_id, name)",
	"api_keys":               "policy compares its own organization_id to palai.org_id",
	"budgets":                "UNIQUE (organization_id, project_id, meter_prefix)",
	"capability_jobs":        "UNIQUE (organization_id, project_id, idempotency_key) — idempotency",
	"commands":               "NOT NULL, and PRIMARY KEY (organization_id, project_id, id)",
	"config_revisions":       "NOT NULL, and PRIMARY KEY (organization_id, project_id, id)",
	"delivered_messages":     "NOT NULL, and PRIMARY KEY (organization_id, project_id, command_id)",
	"document_revisions":     "UNIQUE (organization_id, project_id, source_id, version)",
	"environment_values":     "policy compares its own organization_id to palai.org_id",
	"environments":           "policy compares its own organization_id to palai.org_id, and UNIQUE (organization_id, name)",
	"hooks":                  "UNIQUE (organization_id, project_id, name)",
	"idempotency_records":    "UNIQUE (organization_id, project_id, principal_id, method, route, idempotency_key)",
	"index_revisions":        "UNIQUE (organization_id, project_id, knowledge_base_id, version)",
	"integration_bots":       "UNIQUE (organization_id, project_id, name)",
	"knowledge_bases":        "UNIQUE (organization_id, project_id, name)",
	"mcp_connections":        "UNIQUE (organization_id, project_id, name)",
	"principals":             "policy compares its own organization_id to palai.org_id",
	"projects":               "policy compares its own organization_id to palai.org_id, and UNIQUE (organization_id, id)",
	"publications":           "UNIQUE (organization_id, project_id, idempotency_key)",
	"quotas":                 "UNIQUE (organization_id, project_id, meter_prefix)",
	"run_template_revisions": "UNIQUE (organization_id, project_id, template_name, revision_number)",
	"runner_pools":           "UNIQUE (organization_id, project_id, name)",
	"schedules":              "UNIQUE (organization_id, project_id, name)",
	"secret_refs":            "policy compares its own organization_id to palai.org_id, and UNIQUE (organization_id, name, version)",
	"skills":                 "UNIQUE (organization_id, project_id, name)",
	"slack_connections":      "UNIQUE (organization_id, project_id, team_id, enterprise_id)",
	"slack_message_turns":    "UNIQUE (organization_id, project_id, team_id, channel_id, message_ts)",
	"slack_thread_sessions":  "UNIQUE (organization_id, project_id, team_id, channel_id, thread_ts)",
	"tool_set_revisions":     "UNIQUE (organization_id, project_id, set_name, revision_number)",
	"tools":                  "UNIQUE (organization_id, project_id, canonical_name) and (…, model_visible_name)",
	"triggers":               "UNIQUE (organization_id, project_id, name)",
	"usage_ledger":           "policy compares its own organization_id to palai.org_id, and UNIQUE (organization_id, project_id, dedupe_key)",
}

// TestNoStatementStillFiltersByOrganization requires that an `organization_id = $N` comparison survives
// only on a table whose row-level-security policy still reads palai.org_id, or whose column is still NOT
// NULL. Everywhere else the comparison is vestigial: 000062 rekeyed the policy to project_id, so the
// boundary the WHERE clause claims to add is already enforced one layer down, and the bind argument it
// consumes is the one this epic is removing.
//
// IT JUDGES FILTERS, NOT WRITES, AND THE DIFFERENCE IS NOT A TECHNICALITY. A statement may keep naming
// organization_id in an INSERT column list, an ON CONFLICT target or a SELECT list on a table this test
// says must not FILTER by it, and that is correct: 32 tables must still be WRITTEN with an organization
// (identityOrgWriteTables), and 24 of them for a reason no policy and no NOT NULL announces — the column
// is part of a UNIQUE index, and in a unique index NULL is distinct from NULL. Measured, with a control,
// on a temporary table with UNIQUE (organization_id, project_id, name):
//
//	('org_a', 'proj_a', 'dup') twice -> the second is refused, duplicate key   <- control
//	(NULL,    'proj_a', 'dup') three times -> all three accepted, 3 rows
//
// So dropping the WRITE on those tables does not raise an error; it silently retires the uniqueness
// guarantee (two agent profiles of one name, two tools of one canonical_name, an idempotency_key
// processed twice). Those writes leave with Task 6's index rebuild, not with this test.
//
// It is red before the cut-over on purpose, and each failure line is one statement to edit.
func TestNoStatementStillFiltersByOrganization(t *testing.T) {
	statements := identityStatements(t)
	if len(statements) == 0 {
		t.Fatal("no -- name: block parsed out of storage/queries — this test is VACUOUS")
	}

	var filtering, writing, unresolved, stale int
	for _, name := range identitySortedNames(statements) {
		stmt := statements[name]
		filters, writes, unknown := identityOrgRefs(stmt.sql)
		if len(writes) > 0 {
			writing++
		}
		if len(filters) > 0 {
			filtering++
		}
		for _, table := range identitySortedStrings(filters) {
			if _, keep := identityOrgTables[table]; !keep {
				t.Errorf("%s: statement %s filters on %s.organization_id, and %s's policy has keyed on "+
					"project_id since 000062 — drop the comparison, renumber the $N below it, and drop the "+
					"bind argument at every call site", stmt.at, name, table, table)
			}
		}
		for _, table := range identitySortedStrings(writes) {
			if _, keep := identityOrgWriteTables[table]; !keep {
				// COUNTED, NOT FAILED, and the asymmetry is deliberate. Dropping a FILTER is this task's
				// work and is complete above. Dropping a WRITE is not: the value still has to come from
				// somewhere for the 32 tables that do need it, and until Tenant.Organization's remaining
				// readers are resolved (A.2 Task 6 removes the columns and the policies) a statement that
				// merely PROJECTS organization_id is harmless — it feeds a struct field that still exists.
				// Failing on it here would redden the tree for work this task cannot finish.
				stale++
			}
		}
		for _, why := range unknown {
			unresolved++
			t.Errorf("%s: statement %s has an organization_id reference this test cannot attribute to a table "+
				"(%s) — an unattributed reference is an unchecked one, so read it by hand and either drop it "+
				"or qualify it so this test can", stmt.at, name, why)
		}
	}
	// A CEILING on the write debt, so the count cannot grow while it waits for Task 6. 93 references,
	// 2026-08-04, measured by this test on the tree the cut-over left. A new statement writing
	// organization_id to a table nothing needs it on fails here; removing one requires lowering this
	// number, which is the edit that records the work.
	const identityStaleWriteCeiling = 93
	if stale > identityStaleWriteCeiling {
		t.Errorf("%d reference(s) write or project organization_id to a table with no NOT NULL, no org policy "+
			"and no unique index over it — more than the %d this tree carried when the ceiling was set. "+
			"Task 6 removes these; nothing should be ADDING them", stale, identityStaleWriteCeiling)
	}
	t.Logf("organization_id ile FİLTRELEYEN sorgu: %d; YAZAN/projeleyen sorgu: %d (bunlardan %d'i artık hiçbir "+
		"kısıtın istemediği bir tabloya — T6'nın işi); toplam sorgu: %d; çözümlenemeyen referans: %d",
		filtering, writing, stale, len(statements), unresolved)
}

// TestNoQueryBindsAnOrganizationIntoAProjectsSlot is the identity half of the arity guard.
//
// It checks two things, and NOT a third. What it checks:
//
//   - a bind argument that names an organization may only go to a statement whose SQL still names
//     organization_id, and
//   - it must sit at a $N that SQL compares to organization_id, while an argument naming a project must
//     sit at some OTHER position.
//
// What it does not check: that the argument at an organization_id position actually holds one. A call site
// may pass a value this test cannot name — a bare `id`, a struct field called something else, a function
// result. Those are counted and logged rather than assumed innocent.
func TestNoQueryBindsAnOrganizationIntoAProjectsSlot(t *testing.T) {
	statements := identityStatements(t)
	if len(statements) == 0 {
		t.Fatal("no -- name: block parsed out of storage/queries — this test is VACUOUS")
	}

	fset := token.NewFileSet()
	files := arityParseTree(t, arityRepoRoot, fset)
	decls := arityPackageDecls(files)

	var orgBinds, projectBinds, unnamed, sites int
	for _, pf := range files {
		ast.Inspect(pf.file, func(n ast.Node) bool {
			outer, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			for i, arg := range outer.Args {
				inner, ok := arg.(*ast.CallExpr)
				if !ok || !arityIsStorageQuery(inner, pf.imports) {
					continue
				}
				name, literal := arityQueryName(inner)
				if !literal {
					continue
				}
				callee, named := arityCalleeName(outer)
				if !named {
					continue
				}
				binds, ok, _ := arityBindCount(outer, i, callee, decls[pf.pkg])
				if !ok {
					continue
				}
				stmt, known := statements[name]
				if !known {
					continue // the arity guard already reports this as a live panic.
				}
				sites++
				first := len(outer.Args) - binds
				orgAt, _ := identityColumnPositions(stmt.sql)
				hasOrg := identityCarriesAnOrganization(stmt.sql)

				for b := 0; b < binds; b++ {
					at := fset.Position(outer.Args[first+b].Pos())
					position := b + 1 // $N is 1-based.
					switch identityKind(outer.Args[first+b]) {
					case identityOrganization:
						orgBinds++
						if !hasOrg {
							t.Errorf("%s: %s is passed an organization at $%d, but statement %s names no "+
								"column that holds one — this argument is filling some OTHER column's slot "+
								"(%s:%d)", at, callee, position, name, stmt.file, stmt.line)
							continue
						}
						if len(orgAt) > 0 && !orgAt[position] {
							t.Errorf("%s: %s is passed an organization at $%d, but statement %s compares "+
								"organization_id to %s (%s:%d)", at, callee, position, name,
								identityDollars(orgAt), stmt.file, stmt.line)
						}
					case identityProject:
						projectBinds++
						if orgAt[position] {
							t.Errorf("%s: %s is passed a project at $%d, but statement %s compares $%d to "+
								"organization_id (%s:%d)", at, callee, position, name, position,
								stmt.file, stmt.line)
						}
					default:
						unnamed++
					}
				}
			}
			return true
		})
	}

	if sites == 0 {
		t.Fatal("no query call site was reached — the pattern is broken, this test is VACUOUS")
	}
	// The vacuity floor is on PROJECT arguments, not organization ones, and the direction is the reason:
	// this task drives the organization count toward its floor deliberately, so a collapse there is the
	// expected result and proves nothing about the sweep. Project arguments are not being removed by
	// anything, so if identityKind stops recognising a spelling — a rename, an import alias, a helper that
	// forwards through a differently-named parameter — this number falls while every assertion above goes
	// quiet. That is the shape this tree has shipped before: a sweep reporting a cleaner result because it
	// stopped looking.
	//
	// 407 project-naming bind arguments over 585 reached call sites, 2026-08-04 — measured by this test,
	// not chosen: the first run of it reported the number this constant now holds.
	const identityProjectFloor = 407
	if projectBinds < identityProjectFloor {
		t.Errorf("only %d bind argument(s) were recognised as naming a project, fewer than the %d recognised "+
			"when this floor was set: identityKind has stopped matching a spelling it used to, and every "+
			"assertion in this test went quiet with it. Find what stopped matching before lowering this number",
			projectBinds, identityProjectFloor)
	}
	t.Logf("kimlik: denetlenen çağrı yeri: %d; bind argümanı — organizasyon: %d, proje: %d, adlandırılamayan: %d",
		sites, orgBinds, projectBinds, unnamed)
}

// TestTenantScopeIsPublishedOrganizationFirst WAS HERE, AND A.2 TASK 6 DELETED THE HAZARD IT WATCHED.
//
// It read the pair of adjacent strings in `storage.WithTenant(ctx, organization, project)` and the same
// pair in ScopeToTenant, and failed when they were the wrong way round — two `string` parameters, so
// swapping them scoped the whole connection to the wrong pair with the compiler, `go vet` and the arity
// guard next door all three silent.
//
// Task 6 removed the organization parameter. Both functions now take `(ctx, project)`: ONE string, no
// adjacent pair, and therefore no order to get wrong. Migration 000066 rekeyed the last policy that read
// palai.org_id and applyScope stopped writing it, so there is no longer a GUC for a misplaced argument to
// land in — session_guc_test.go's TestOrgIDGUCIsGone asserts that directly.
//
// IT IS DELETED RATHER THAN LEFT TO PASS, because left in place it would have been WORSE than absent. Its
// matcher required `len(call.Args) != 3` to skip, so against the new signature it would have reached zero
// call sites — and its own floor (`checked < 312`) exists to catch exactly that collapse. It would have
// gone red for a rename it was not watching for while its actual claim had become unstateable. A guard
// whose subject no longer exists cannot be made honest by loosening it.
//
// The two tests above are NOT affected and stay: they read bind arguments against the SQL of statements
// in storage/queries, and the `organizations` table plus provisioning.sql's three statements are still
// there — that is A.2's remaining work, not this task's.

// identityKind classifies a bind argument by the name it ends in.
type identityKindOf int

const (
	identityUnknown identityKindOf = iota
	identityOrganization
	identityProject
)

// identityNames maps the spellings this tree threads organization and project ids through. It is a
// closed list rather than a substring rule on purpose: `projectRoot`, `organizationCache` and
// `orgScope` all contain one of these words and none of them is an id.
var identityNames = map[string]identityKindOf{
	"organization":   identityOrganization,
	"organizationid": identityOrganization,
	"org":            identityOrganization,
	"orgid":          identityOrganization,
	"project":        identityProject,
	"projectid":      identityProject,
	"proj":           identityProject,
}

// identityKind reads the rightmost identifier of an expression, which is the one that names its value:
// `tenant.Organization`, `c.Tenant.Organization`, `string(org)` and `orgID` all answer from their last
// name. An expression with no such name — a literal, a composite, an index — is identityUnknown, which
// asserts nothing.
func identityKind(e ast.Expr) identityKindOf {
	return identityNames[strings.ToLower(identityLeaf(e))]
}

func identityLeaf(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return x.Sel.Name
	case *ast.CallExpr:
		return identityLeaf(x.Fun)
	case *ast.ParenExpr:
		return identityLeaf(x.X)
	case *ast.StarExpr:
		return identityLeaf(x.X)
	case *ast.UnaryExpr:
		return identityLeaf(x.X)
	}
	return ""
}

// identityRender is a diagnostic spelling of an expression, enough to recognise the line it came from.
func identityRender(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return identityRender(x.X) + "." + x.Sel.Name
	case *ast.CallExpr:
		return identityRender(x.Fun) + "(…)"
	}
	return "…"
}

// identityStatement is one -- name: block, with the position a failure should point at.
type identityStatement struct {
	sql  string // executable SQL only: comments and single-quoted literals removed
	file string
	line int
	at   string
}

func identitySortedNames(m map[string]identityStatement) []string {
	out := make([]string, 0, len(m))
	for name := range m {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func identitySortedStrings(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for s := range m {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func identityDollars(positions map[int]bool) string {
	out := make([]string, 0, len(positions))
	for n := range positions {
		out = append(out, fmt.Sprintf("$%d", n))
	}
	sort.Strings(out)
	return strings.Join(out, "/")
}

// identityStatements reads storage/queries the way storage/embed.go's parseNamedQueries does — a block
// runs to the next "-- name:" line — and strips it to executable SQL with the arity guard's own reader,
// so both tests see exactly the text Postgres would parse.
func identityStatements(t *testing.T) map[string]identityStatement {
	t.Helper()
	dir := filepath.Join(arityRepoRoot, "storage", "queries")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	out := map[string]identityStatement{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		name, line := "", 0
		var statement strings.Builder
		flush := func() {
			if name == "" {
				return
			}
			out[name] = identityStatement{
				sql:  arityExecutableSQL(statement.String()),
				file: filepath.Join("storage", "queries", e.Name()),
				line: line,
				at:   fmt.Sprintf("%s:%d", filepath.Join("storage", "queries", e.Name()), line),
			}
			statement.Reset()
		}
		for i, text := range strings.Split(string(body), "\n") {
			if marker, ok := strings.CutPrefix(text, "-- name:"); ok {
				flush()
				statement.Reset()
				name, line = strings.TrimSpace(marker), i+1
				continue
			}
			statement.WriteString(text)
			statement.WriteString("\n")
		}
		flush()
	}
	return out
}

var (
	// identityTableRef matches the table after FROM/JOIN/UPDATE/INTO and its optional alias. The negative
	// lookahead Go's regexp does not have is done by hand below: a name followed by '(' is a set-returning
	// function (jsonb_array_elements), not a table.
	identityTableRef = regexp.MustCompile(`(?is)\b(?:from|join|update|into)\s+([a-z_][a-z0-9_]*)\s*(\(?)\s*(?:(?:as\s+)?([a-z_][a-z0-9_]*))?`)
	identityCTERef   = regexp.MustCompile(`(?is)(?:\bwith\b|,)\s*([a-z_][a-z0-9_]*)\s+as\s*\(`)
	identityOrgRef   = regexp.MustCompile(`(?i)(?:\b([a-z_][a-z0-9_]*)\.)?\borganization_id\b`)
	identityColumnAt = regexp.MustCompile(`(?i)(?:\b[a-z_][a-z0-9_]*\.)?\b(organization_id|project_id)\b\s*(?:=|<>)\s*\$([0-9]+)`)
	identityTarget   = regexp.MustCompile(`(?is)\b(?:insert\s+into|update|delete\s+from)\s+([a-z_][a-z0-9_]*)`)
)

// identitySQLKeywords are the words that can stand where a table name or an alias would, so that
// `DELETE FROM x USING y` and `UPDATE t SET c = 1` do not invent tables called `using` and `set`.
var identitySQLKeywords = map[string]bool{
	"all": true, "and": true, "as": true, "asc": true, "by": true, "case": true, "conflict": true,
	"cross": true, "desc": true, "distinct": true, "do": true, "else": true, "end": true, "exists": true,
	"for": true, "from": true, "full": true, "group": true, "having": true, "inner": true, "into": true,
	"is": true, "join": true, "lateral": true, "left": true, "limit": true, "locked": true, "natural": true,
	"not": true, "nothing": true, "null": true, "of": true, "offset": true, "on": true, "only": true,
	"or": true, "order": true, "outer": true, "returning": true, "right": true, "select": true,
	"set": true, "share": true, "skip": true, "then": true, "union": true, "update": true, "using": true,
	"values": true, "when": true, "where": true, "with": true,
}

// identityOrgRefs attributes each organization_id reference in a statement to the table it belongs to AND
// to its role. A reference is a FILTER when it is compared — to a bind parameter (`organization_id = $2`)
// or to another table's column (`a.organization_id = w.organization_id`, the redundant half of a join
// predicate). Every other appearance — an INSERT column list, a SET, an ON CONFLICT target, a SELECT or
// RETURNING list — is a WRITE or a projection. The two are governed by different tables and a sweep that
// conflated them would demand the removal of writes that silently disable a unique index.
//
// It returns, separately, a description of every reference it could not attribute: an unattributed
// reference is one this sweep is not checking, and the tree has shipped a sweep that reported a cleaner
// number while covering less.
func identityOrgRefs(sql string) (filters, writes map[string]bool, unknown []string) {
	if !strings.Contains(sql, "organization_id") {
		return nil, nil, nil
	}
	ctes := map[string]bool{}
	for _, m := range identityCTERef.FindAllStringSubmatch(sql, -1) {
		ctes[strings.ToLower(m[1])] = true
	}

	alias := map[string]string{}
	var tables []string
	for _, m := range identityTableRef.FindAllStringSubmatch(sql, -1) {
		name := strings.ToLower(m[1])
		if identitySQLKeywords[name] || ctes[name] || m[2] == "(" {
			continue // a keyword, a CTE reference, or a set-returning function call — none is a table.
		}
		if _, seen := alias[name]; !seen {
			tables = append(tables, name)
		}
		alias[name] = name
		if a := strings.ToLower(m[3]); a != "" && !identitySQLKeywords[a] {
			if _, taken := alias[a]; !taken {
				alias[a] = name
			}
		}
	}

	target := ""
	if m := identityTarget.FindStringSubmatch(sql); m != nil {
		if name := strings.ToLower(m[1]); !identitySQLKeywords[name] && !ctes[name] {
			target = name
		}
	}

	filters, writes = map[string]bool{}, map[string]bool{}
	for _, loc := range identityOrgRef.FindAllStringSubmatchIndex(sql, -1) {
		qualifier := ""
		if loc[2] >= 0 {
			qualifier = strings.ToLower(sql[loc[2]:loc[3]])
		}
		var table string
		switch {
		case qualifier != "":
			resolved, ok := alias[qualifier]
			if !ok {
				unknown = append(unknown, "qualifier "+qualifier+" names no table this statement reads")
				continue
			}
			table = resolved
		case len(tables) == 1:
			table = tables[0]
		case target != "":
			// An unqualified column in a statement that writes belongs to the table it writes: Postgres
			// resolves `SET organization_id`, an INSERT column list and a RETURNING list against the target.
			table = target
		default:
			unknown = append(unknown, fmt.Sprintf("unqualified, and the statement reads %v", tables))
			continue
		}
		if identityFilterAfter.MatchString(sql[loc[1]:]) || identityFilterBefore.MatchString(sql[:loc[0]]) {
			filters[table] = true
		} else {
			writes[table] = true
		}
	}
	return filters, writes, unknown
}

var (
	// identityFilterAfter matches a reference standing on the LEFT of a comparison: `organization_id = $2`
	// and `d.organization_id = w.organization_id`.
	identityFilterAfter = regexp.MustCompile(`(?is)^\s*(?:=|<>)\s*(?:\$[0-9]+|[a-z_][a-z0-9_]*\.organization_id\b)`)
	// identityFilterBefore matches one standing on the RIGHT of a comparison against another org column.
	// Without it the right-hand side of a join predicate is counted as a write and the statement is told
	// to keep a reference it should drop with its partner.
	identityFilterBefore = regexp.MustCompile(`(?is)organization_id\s*(?:=|<>)\s*$`)
)

// identityCarriesAnOrganization reports whether a statement has any column an organization id belongs in.
// For every table but one that column is organization_id. The exception is `organizations` itself, whose
// own id IS the organization — `INSERT INTO organizations (id, display_name, ...)` binds one and names no
// organization_id anywhere, and reading only the column name called that statement a defect.
func identityCarriesAnOrganization(sql string) bool {
	return strings.Contains(sql, "organization_id") || identityOrganizationsTable.MatchString(sql)
}

var identityOrganizationsTable = regexp.MustCompile(`(?is)\b(?:from|join|into|update)\s+organizations\b`)

// identityColumnPositions reports which $N a statement compares to organization_id and which to
// project_id. A statement may compare neither (an INSERT whose columns are positional), in which case the
// caller's positional check does not fire and says so rather than passing silently.
func identityColumnPositions(sql string) (organization, project map[int]bool) {
	organization, project = map[int]bool{}, map[int]bool{}
	for _, m := range identityColumnAt.FindAllStringSubmatch(sql, -1) {
		n := 0
		for _, c := range m[2] {
			n = n*10 + int(c-'0')
		}
		if strings.EqualFold(m[1], "organization_id") {
			organization[n] = true
		} else {
			project[n] = true
		}
	}
	return organization, project
}
