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

// aritySkip is a report line for a storage.Query use this test does not audit. Every one of them is
// logged: the guard's VALUE to Task 5 is its coverage, and a hole nobody can see is a false assurance.
type aritySkip struct {
	pos    token.Position
	reason string
}

// TestEveryQueryCallSiteBindsItsStatementsParameters compares, for each storage.Query("X") passed as a
// bind-carrying argument, the number of arguments after it against the highest $N in X's SQL.
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
				if !ok || !arityIsStorageQuery(inner) {
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
			if !ok || !arityIsStorageQuery(call) || bound[call.Pos()] {
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
			return 0, false, callee + " binds nothing (its declaration is not variadic)"
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

// arityFile pairs a parsed file with the package key its unqualified identifiers resolve in.
type arityFile struct {
	file   *ast.File
	pkg    string
	isTest bool
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
			if prior, seen := pkg[fn.Name.Name]; seen && prior.variadic != this.variadic {
				this.ambiguous = true
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

func arityIsStorageQuery(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Query" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "storage"
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
			file:   file,
			pkg:    filepath.Dir(path) + "\x00" + file.Name.Name,
			isTest: strings.HasSuffix(path, "_test.go"),
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
