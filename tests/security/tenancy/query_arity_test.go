//go:build security

// Package tenancy, continued (A.2 Task 4): the scope guard Task 5's cut-over needs, because at that
// step the COMPILER stops holding it.
//
// Task 3 removed middleware.Scope.Organization and the type system found every reader for free: a
// deleted struct field is a compile error at each use. Task 5 removes `AND organization_id = $2` from
// the SQL and `Tenant.Organization` from the call sites, and nothing in Go connects the two halves —
// storage.Query(name) (storage/embed.go) returns a string out of an embedded file, positional bind
// arguments are typed `...any`, and there is no code generation between them. Drop `$2` from a
// statement, forget the argument at the call site, and the tree still builds.
//
// Postgres does object: `bind message supplies 3 parameters, but prepared statement requires 2`. But
// only for a statement some test actually executes, and the surface is larger than any suite drives:
//
//	grep -rn 'storage\.Query("' --include='*.go' . | grep -v _test | wc -l  -> 570 (2026-08-03)
//	grep -rh '^-- name:' storage/queries/*.sql | wc -l                      -> 514 (2026-08-03)
//
// So this test does statically what the type system will not: it reads each statement's highest $N and
// compares it against the number of bind arguments its call sites pass.
package tenancy

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// arityRepoRoot is this package's directory depth below the tree root: tests/security/tenancy.
const arityRepoRoot = "../../.."

// arityDBVerbs are the pgx methods that take a statement followed by positional bind arguments. They
// are the ONLY callees whose bind arity is decided from a name, and the reason is not preference:
// pgx is not in this tree, so there is no *ast.FuncDecl to read `...` out of. Every other callee —
// helper, wrapper, method — is resolved against its own package's declarations by arityCallSites, so a
// call form added tomorrow is audited because of its SIGNATURE, not because someone remembered to name
// it here.
var arityDBVerbs = map[string]bool{"Exec": true, "Query": true, "QueryRow": true, "Queue": true}

// arityKnownUses is how many storage.Query uses this sweep reached when the floor below was last set:
// 582 audited + 27 reported unauditable, 2026-08-04 (A.2 Task 6). Cross-checked against a reader that
// knows no Go, `grep -rn 'storage\.Query(' --include='*.go' . | grep -v node_modules | wc -l` -> 615,
// minus the SIX that are not calls, named rather than rounded away: three in this file (two in prose
// above, one inside the t.Errorf string below), two in organization_identity_test.go's prose, and
// cmd/cli/internal/stack/doctor_v2_test.go:19 inside a format string. 615 - 6 = 609.
//
// IT MOVED DOWN BY ONE, and the floor exists to make that a decision rather than a drift: A.2 Task 6 gave
// POST /v1/projects the tenant-opening job, and the two paths that now open a tenant share ONE
// storage.Query("InsertProject") in identity's seedTenant where CreateOrganization and CreateProject
// previously carried one each. Confirmed deleted, not merely no longer matched:
// `git show 7386cbe8^:apps/control-plane/internal/identity/store.go | grep -c 'storage\.Query('` -> 18,
// the same grep on the file today -> 17.
const arityKnownUses = 609

// aritySkip is a report line for a storage.Query use this test does not audit. Every one of them is
// logged: the guard's VALUE to Task 5 is its coverage, and a hole nobody can see is a false assurance.
type aritySkip struct {
	pos    token.Position
	reason string
}

// TestEveryQueryCallSiteBindsItsStatementsParameters compares, for each storage.Query("X") passed as a
// bind-carrying argument, the number of arguments after it against the highest $N in X's SQL.
//
// ARITY IS NOT IDENTITY, and Task 5's most likely mistake is on the identity side. This test counts HOW
// MANY bind arguments a call site passes and never checks WHICH. Drop `AND organization_id = $2` from a
// statement and leave `tenant.Organization` behind INSTEAD of `tenant.Project` — two parameters, two
// arguments — and the statement now filters `project_id = <an organization id>` while every count still
// agrees. That was measured on GetResponse/retention.go: the compiler, `go vet` and this test are all
// three silent on it. At least 115 call sites carry both values on one line
// (`grep -rn 'storage\.Query(' --include='*.go' . | grep -c 'Organization.*Project\|Project.*Organization'`),
// and choosing between two adjacent string arguments is precisely the edit Task 5 makes 372 times. This
// test narrows that work; it does not check it. An identity guard would be a different test — one that
// asserts tenant.Organization is no longer passed to any storage.Query call at all.
func TestEveryQueryCallSiteBindsItsStatementsParameters(t *testing.T) {
	params := arityStatementParams(t, filepath.Join(arityRepoRoot, "storage", "queries"))
	if len(params) == 0 {
		t.Fatal("no -- name: blocks parsed out of storage/queries — the statement reader is broken, this test is VACUOUS")
	}

	fset := token.NewFileSet()
	files := arityParseTree(t, arityRepoRoot, fset)
	decls := arityPackageDecls(files)

	var (
		checked, inTests int
		skips            []aritySkip
		callees          = map[string]int{}
	)

	for _, pf := range files {
		bound := map[token.Pos]bool{}

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
				bound[inner.Pos()] = true
				at := fset.Position(inner.Pos())

				name, literal := arityQueryName(inner)
				if !literal {
					skips = append(skips, aritySkip{at, "statement name is not a constant"})
					continue
				}
				callee, named := arityCalleeName(outer)
				if !named {
					skips = append(skips, aritySkip{at, "callee is not a name (" + name + ")"})
					continue
				}
				callees[callee]++

				binds, ok, why := arityBindCount(outer, i, callee, decls[pf.pkg])
				if !ok {
					skips = append(skips, aritySkip{at, why + " (" + name + ")"})
					continue
				}
				want, known := params[name]
				if !known {
					// storage.Query panics on an unknown name, so this is a live defect, not a gap.
					t.Errorf("%s: storage.Query(%q) names no -- name: block in storage/queries — this call panics when reached", at, name)
					continue
				}
				checked++
				if pf.isTest {
					inTests++
				}
				if binds != want {
					t.Errorf("%s: %s takes %d parameter(s) ($%d is its highest) but %s is passed %d bind argument(s)",
						at, name, want, want, callee, binds)
				}
			}
			return true
		})

		ast.Inspect(pf.file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !arityIsStorageQuery(call, pf.imports) || bound[call.Pos()] {
				return true
			}
			name, _ := arityQueryName(call)
			skips = append(skips, aritySkip{fset.Position(call.Pos()), "not passed directly to a call (" + name + ")"})
			return true
		})
	}

	if checked == 0 {
		t.Fatal("no query call site was audited — the pattern is broken, this test is VACUOUS")
	}
	// The zero check above is one floor with 610 silent steps beneath it. This is the second, and it
	// guards the direction that hides: a sweep that stops REACHING part of the tree reports a smaller
	// audited count AND a shorter unauditable list, so both numbers move the way a healthier sweep would
	// move them and the log reads better than before. Their SUM is what cannot improve by accident.
	//
	// A floor, not an equality: query call sites are added and removed legitimately, and only the
	// downward direction is the failure this test exists to notice. Lowering it is a decision — take it
	// once the missing uses are confirmed deleted rather than merely no longer matched.
	if total := checked + len(skips); total < arityKnownUses {
		t.Errorf("this sweep reached %d storage.Query use(s), fewer than the %d it reached when this floor was last set: "+
			"it now covers LESS of the tree, and that is the failure that arrives looking like an improvement "+
			"(an import alias in one file measured 583 -> 542 audited with the unauditable list SHRINKING 27 -> 25). "+
			"Find what stopped matching before lowering this number", total, arityKnownUses)
	}
	t.Logf("denetlenen sorgu çağrısı: %d (%d of them in _test.go files); statements: %d; go files parsed: %d",
		checked, inTests, len(params), len(files))

	names := make([]string, 0, len(callees))
	for name := range callees {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		t.Logf("çağıran: %s — %d call site(s)", name, callees[name])
	}

	sort.Slice(skips, func(i, j int) bool { return skips[i].pos.String() < skips[j].pos.String() })
	t.Logf("denetlenemeyen: %d", len(skips))
	for _, s := range skips {
		t.Logf("denetlenemeyen: %s — %s", s.pos, s.reason)
	}
}

// arityBindCount reports how many of outer's arguments are positional bind parameters for the statement
// at index queryArg. A callee declared in the caller's own package is resolved to its *ast.FuncDecl and
// answers structurally: bind arguments are the ones absorbed by its variadic parameter, so a helper that
// takes the statement and forwards `args ...any` is audited exactly like a pgx call, and a helper that
// merely INSPECTS the string (statementBody, oneLine, queryRecorder.indexOf) is not mistaken for one.
// That distinction cannot be made from the argument list alone: scanCounts(ctx, pool, stmt, dest) passes
// one argument after the statement and binds none of it.
func arityBindCount(outer *ast.CallExpr, queryArg int, callee string, pkg map[string]arityDecl) (int, bool, string) {
	if outer.Ellipsis.IsValid() {
		return 0, false, "arguments are spread with ..."
	}
	if decl, ok := pkg[callee]; ok {
		switch {
		case decl.ambiguous:
			return 0, false, "package declares more than one " + callee + " with differing shapes"
		case decl.variadic < 0:
			// NOT "binds nothing" — that would claim something this code never checks. A non-variadic
			// helper can perfectly well bind a fixed parameter; a helper taking (query string, id string)
			// and executing both was written and this branch skipped it, correctly reporting it and
			// wrongly describing it. What the declaration actually tells us is only that no variadic
			// parameter absorbs the trailing arguments, so their bind arity is not readable here.
			return 0, false, callee + " is not variadic — its bind arity cannot be read from the declaration"
		case decl.variadic != queryArg+1:
			return 0, false, "the statement is not the parameter before " + callee + "'s variadic"
		}
		return len(outer.Args) - decl.variadic, true, ""
	}
	if arityDBVerbs[callee] {
		return len(outer.Args) - queryArg - 1, true, ""
	}
	return 0, false, callee + " is declared outside this tree"
}

// arityDecl is what this test needs of a declaration: the flattened index of its variadic parameter, or
// -1 when it has none.
type arityDecl struct {
	variadic  int
	ambiguous bool
}

// arityFile pairs a parsed file with the package key its unqualified identifiers resolve in and the
// import paths its qualified ones resolve through.
type arityFile struct {
	file    *ast.File
	pkg     string
	imports map[string]string
	isTest  bool
}

// arityPackageDecls indexes every func and method by name, PER PACKAGE, because that is the scope Go
// resolves an unqualified call in. Tree-wide indexing would be wrong and would lose sites: five packages
// declare a helper called countRows, three of them with the statement at a different parameter index.
func arityPackageDecls(files []arityFile) map[string]map[string]arityDecl {
	out := map[string]map[string]arityDecl{}
	for _, pf := range files {
		pkg, ok := out[pf.pkg]
		if !ok {
			pkg = map[string]arityDecl{}
			out[pf.pkg] = pkg
		}
		for _, d := range pf.file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			this := arityDecl{variadic: arityVariadicIndex(fn.Type)}
			// Ambiguity, once seen, STAYS seen. Comparing only against the previous entry lets three
			// declarations shaped 3, 2, 2 clear the flag on the last step — and a cleared flag makes the
			// sweep answer with an arity instead of declining to. No package key reaches that today
			// (three duplicate names differ in shape tree-wide, all three keep the flag, none is a query
			// callee), which is exactly when it is cheap to fix.
			if prior, seen := pkg[fn.Name.Name]; seen {
				this.ambiguous = prior.ambiguous || prior.variadic != this.variadic
			}
			pkg[fn.Name.Name] = this
		}
	}
	return out
}

// arityVariadicIndex returns the position of the variadic parameter in the flattened parameter list, or
// -1. Flattening matters: `func f(a, b string, args ...any)` declares three parameters in two fields.
func arityVariadicIndex(fn *ast.FuncType) int {
	at := 0
	for _, field := range fn.Params.List {
		if _, ok := field.Type.(*ast.Ellipsis); ok {
			return at
		}
		if n := len(field.Names); n > 0 {
			at += n
		} else {
			at++
		}
	}
	return -1
}

// arityStoragePkg is the import path Query must resolve to. Matching the PATH rather than the spelling
// in front of the dot is what keeps the sweep's scope stable: keyed on the literal identifier `storage`,
// a single `st "github.com/palgroup/palai/storage"` — an ordinary refactor, and Task 5 touches 372 call
// sites — drops every call in that file. It does so SILENTLY and in the REASSURING direction: measured
// on packages/coordinator/store.go, the audited count fell 583 -> 542 while the unauditable list also
// SHRANK, 27 -> 25. Both numbers moved the way a healthier sweep would move them.
const arityStoragePkg = "github.com/palgroup/palai/storage"

func arityIsStorageQuery(call *ast.CallExpr, imports map[string]string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Query" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && imports[pkg.Name] == arityStoragePkg
}

// arityImports maps a file's local package names to their import paths. An import with no explicit name
// is addressed by the last element of its path, which is the package's own name for every import in
// this module; a renamed import overrides that with the name it declares.
func arityImports(f *ast.File) map[string]string {
	out := make(map[string]string, len(f.Imports))
	for _, spec := range f.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		name := path[strings.LastIndexByte(path, '/')+1:]
		if spec.Name != nil {
			name = spec.Name.Name
		}
		out[name] = path
	}
	return out
}

func arityQueryName(call *ast.CallExpr) (string, bool) {
	if len(call.Args) != 1 {
		return "", false
	}
	lit, ok := call.Args[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	name, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return name, true
}

func arityCalleeName(call *ast.CallExpr) (string, bool) {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name, true
	case *ast.SelectorExpr:
		return fun.Sel.Name, true
	}
	return "", false
}

// arityParseTree parses every .go file in the tree. Build tags are deliberately not honoured — a call
// site behind `//go:build component` binds the same parameters as one that is not, and parser.ParseFile
// reads the file either way.
func arityParseTree(t *testing.T, root string, fset *token.FileSet) []arityFile {
	t.Helper()
	var out []arityFile
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules", "testdata":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			// Not a skip: an unparsed file is a call site nobody looked at.
			t.Errorf("%s: parse failed, its query call sites went unaudited: %v", path, err)
			return nil
		}
		out = append(out, arityFile{
			file:    file,
			pkg:     filepath.Dir(path) + "\x00" + file.Name.Name,
			imports: arityImports(file),
			isTest:  strings.HasSuffix(path, "_test.go"),
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(out) == 0 {
		t.Fatalf("no .go file found under %s — this test is VACUOUS", root)
	}
	return out
}

var arityParam = regexp.MustCompile(`\$([0-9]+)`)

// arityStatementParams returns each statement's highest $N. Comments and string literals are removed
// first, and that is load-bearing rather than tidy: parseNamedQueries (storage/embed.go) ends a block
// only at the next "-- name:" line, so the prose paragraph documenting the NEXT statement belongs to the
// PREVIOUS statement's string. Scanning a raw block read GetResponse — `WHERE id = $1 AND
// organization_id = $2 AND project_id = $3` — as taking seven parameters, because the paragraph below it
// discusses ListResponses's keyset $6 and $7.
func arityStatementParams(t *testing.T, dir string) map[string]int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	out := map[string]int{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		name := ""
		var statement strings.Builder
		flush := func() {
			if name == "" {
				return
			}
			highest := 0
			for _, m := range arityParam.FindAllStringSubmatch(arityExecutableSQL(statement.String()), -1) {
				if n, _ := strconv.Atoi(m[1]); n > highest {
					highest = n
				}
			}
			out[name] = highest
			statement.Reset()
		}
		for _, line := range strings.Split(string(body), "\n") {
			if marker, ok := strings.CutPrefix(line, "-- name:"); ok {
				flush()
				statement.Reset()
				name = strings.TrimSpace(marker)
				continue
			}
			statement.WriteString(line)
			statement.WriteString("\n")
		}
		flush()
	}
	return out
}

// arityExecutableSQL drops line comments, block comments and single-quoted literals, leaving only text
// Postgres would parse as SQL. A $N inside any of the three is not a parameter.
func arityExecutableSQL(sql string) string {
	var out strings.Builder
	for i := 0; i < len(sql); {
		switch {
		case strings.HasPrefix(sql[i:], "--"):
			end := strings.IndexByte(sql[i:], '\n')
			if end < 0 {
				return out.String()
			}
			i += end
		case strings.HasPrefix(sql[i:], "/*"):
			end := strings.Index(sql[i+2:], "*/")
			if end < 0 {
				return out.String()
			}
			i += 2 + end + 2
		case sql[i] == '\'':
			i++
			for i < len(sql) {
				if sql[i] != '\'' {
					i++
					continue
				}
				if i+1 < len(sql) && sql[i+1] == '\'' {
					i += 2
					continue
				}
				i++
				break
			}
		default:
			out.WriteByte(sql[i])
			i++
		}
	}
	return out.String()
}
