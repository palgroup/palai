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
// ONE test: TestNoQueryBindsAnOrganizationIntoAProjectsSlot, the Go-side identity check described above.
//
// THIS SAID "THREE", THEN "TWO", AND IS NOW ONE. Both departures are recorded where the function used to
// be — TestTenantScopeIsPublishedOrganizationFirst below the surviving test, and the SQL-side inventory
// TestNoStatementStillFiltersByOrganization above it. The header is the part a reader checks the roster
// against, so a count it inflates is a guard someone believes is standing.
//
// It does not replace the arity guard next door, and the perturbations recorded in the task report show
// why: an organization-spelled identifier sitting in a project's slot is invisible to arity and caught
// here, while passing one argument too many is invisible here and caught there.
//
// The identity check is NOT retired along with the field it was written for. coordinator.Tenant is a
// one-field struct now, so the literal `tenant.Organization` of the original example no longer compiles —
// but identityNames matches any argument spelled organization/organizationID/org/orgID, and a local
// variable may still carry one of those names into a project's slot. Zero matches is this guard's PASS,
// not its retirement.
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

// TestNoStatementStillFiltersByOrganization WAS HERE, WITH THE TWO MAPS THAT WERE ITS WORKLIST, AND IT
// WAS VACUOUS — not loosened into vacuity, but reduced to it by the very sweep it was written to drive.
//
// It attributed every organization_id reference in storage/queries to the table it belonged to, required
// a FILTER to survive only where the database still refused a NULL or a mismatch, and counted the WRITES
// it could not yet demand the removal of under a ceiling of 93. identityOrgTables named the eleven tables
// a value still had to reach and identityOrgWriteTables the thirty-two it still had to be written to: 43
// entries describing NOT NULL columns, policies reading palai.org_id, and UNIQUE indexes over the column.
//
// A.2 Task 6 finished. Every one of those three mechanisms is gone, and so is the column:
//
//	grep -c organization_id storage/migrations/*.up.sql   -> one hit, in 000006's prose, no DDL (2026-08-05)
//	grep -c organization_id storage/queries/*.sql         -> two hits, both in comments (2026-08-05)
//
// The consequence is not a weakened guard, it is an absent one. identityOrgRefs opened with
// `if !strings.Contains(sql, "organization_id") { return nil, nil, nil }`, so with no statement containing
// the string EVERY call returned empty and the loop body below it never ran. Measured by the test's own
// log line on the tree that deleted it:
//
//	organization_id ile FİLTRELEYEN sorgu: 0; YAZAN/projeleyen sorgu: 0 (bunlardan 0'i …);
//	toplam sorgu: 497; çözümlenemeyen referans: 0
//
// filtering 0, writing 0, stale 0, unresolved 0 — over 497 parsed statements. The `stale > 93` ceiling
// could not fire at any value of the tree, and neither t.Errorf could be reached.
//
// AND ITS OWN VACUITY GUARD DID NOT SEE THIS, which is the part worth keeping. It read
// `if len(statements) == 0`, and 497 statements parsed. It checked that the INPUT was found, never that
// any of it was ATTRIBUTED — so the one number that had collapsed was the one number it did not look at.
// A vacuity floor belongs on what a sweep MATCHED, not on what it read; the surviving test's floor is on
// its project binds for exactly that reason, and this file has now shipped both shapes.
//
// IT IS DELETED RATHER THAN LEFT TO PASS, on this file's own precedent below: a guard whose subject no
// longer exists cannot be made honest by loosening it. Left in place it asserted nothing while its two
// maps read as live guidance for a schema that does not exist — a header claiming "32 tables must still
// be WRITTEN with an organization" is worse than absent, because a reader checks the roster against it.
//
// WHAT DID NOT GO WITH IT: identityProjectFloor in the test below. That floor watches identityKind's
// project spellings, nothing organization-shaped, and it is load-bearing today — a rename or a helper
// that forwards through a differently-named parameter drops the count while every assertion goes quiet.
// Measured across this deletion to prove the two are independent: 405 project binds before, 405 after.

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
	// MOVED DOWN, 2026-08-05, attributed to a DELETION rather than to a use the walk stopped reaching: the
	// Slack cutover (81aada0f and its companions) deleted apps/control-plane/{api,internal/extensions}/
	// slack*.go and the store's slack_*_component_test.go files outright — 10,800+ lines carrying their own
	// storage.Query/Exec/QueryRow call sites and project-naming binds — plus one bind each from
	// packages/coordinator/{approvals,publication,store}.go's Slack-only enqueue statements, which went with
	// their sole callers. `git grep -c 'storage\.Query(' a1a7362b^ -- '*.go'` vs the same at HEAD shows
	// exactly those files vanish and nothing else move down; go files parsed fell 1299 -> 1261 in step. The
	// unauditable count did NOT grow (26 -> 25), which is this floor's own signal that coverage did not
	// shrink by losing a match — it shrank because there was less to match.
	//
	// 407 project-naming bind arguments over 585 reached call sites, 2026-08-04 — measured by this test,
	// not chosen: the first run of it reported the number this constant now holds. Re-measured after the
	// cutover at 390 over 555 (git archive HEAD, isolated from any uncommitted tree), which is this floor.
	const identityProjectFloor = 390
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
// adjacent pair, and therefore no order to get wrong. The last policy that read
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

// identityColumnAt matches the $N a statement compares a tenant column to. The four regexes that stood
// beside it — a table/alias reader, a CTE reader, an organization_id finder and an INSERT/UPDATE/DELETE
// target reader — belonged to identityOrgRefs and went with it; they are recorded at the top of this file.
// This one is different in kind: it reads the PROJECT half too, which is the half that still exists.
var identityColumnAt = regexp.MustCompile(`(?i)(?:\b[a-z_][a-z0-9_]*\.)?\b(organization_id|project_id)\b\s*(?:=|<>)\s*\$([0-9]+)`)

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
