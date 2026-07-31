package coordinator

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// The no-fan-out fence for the session list (E29). The Sessions screen's whole premise is that a page
// of 50 rows costs ONE request, and the way that premise dies is not dramatically: someone adds a
// column to the row, finds the SQL awkward, and fills it in a `for` loop over the page. That reads
// fine, passes every behavioural test in this package, and turns one query into fifty-one.
//
// WHAT THIS PROVES AND WHAT IT DOES NOT. It reads the shipped function's AST, so it proves the SHAPE:
// exactly one statement is executed, and no execution happens inside a loop. It does NOT count wire
// round trips — nothing in this tree can, because the production pool is opened by storage.OpenPool
// and a test that built its own traced pool would be measuring a pool production never uses. The
// behavioural half lives in the component test, which asserts a fully-populated page comes back from
// this one call.
func TestListSessionsIssuesExactlyOneQuery(t *testing.T) {
	body := funcBodyOf(t, "list.go", "ListSessions")

	var executions, inLoop int
	ast.Inspect(body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.RangeStmt:
			inLoop += countPoolExecutions(node.Body)
		case *ast.ForStmt:
			inLoop += countPoolExecutions(node.Body)
		case *ast.CallExpr:
			if poolExecution(node) {
				executions++
			}
		}
		return true
	})
	if executions != 1 {
		t.Fatalf("ListSessions executes %d statements, want exactly 1 — a page of 50 sessions must not be 50 reads", executions)
	}
	if inLoop != 0 {
		t.Fatalf("ListSessions executes %d statements inside a loop; that is the N+1 this row shape exists to avoid", inLoop)
	}
}

// TestGetSessionAndListSessionsRenderTheSameFields is the divergence fence. The two statements are
// separate SQL — a detail read filters by id, a page filters by keyset — so nothing but this stops one
// of them from gaining a field the other does not have, which is how a list row and the resource it
// links to start disagreeing. Both scan into SessionView, so the check is that both name every one of
// its projected fields.
func TestGetSessionAndListSessionsRenderTheSameFields(t *testing.T) {
	projected := []string{
		"v.ID", "v.State", "v.CreatedAt", "v.Name", "derived", "v.Agents",
		"v.InputTokens", "v.OutputTokens", "v.FirstActivityAt", "v.LastActivityAt",
	}
	for _, where := range []struct{ file, fn string }{
		{"sessions.go", "GetSession"},
		{"list.go", "ListSessions"},
	} {
		body := funcBodyOf(t, where.file, where.fn)
		text := nodeText(t, where.file, body)
		for _, field := range projected {
			if !strings.Contains(text, "&"+field) {
				t.Fatalf("%s (%s) does not scan %s; a detail read and a list row must project the same fields", where.fn, where.file, field)
			}
		}
	}
}

func poolExecution(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	switch sel.Sel.Name {
	case "Query", "QueryRow", "Exec":
	default:
		return false
	}
	// s.pool.Query(...) — the receiver is itself a selector ending in `pool`.
	inner, ok := sel.X.(*ast.SelectorExpr)
	return ok && inner.Sel.Name == "pool"
}

func countPoolExecutions(body *ast.BlockStmt) int {
	n := 0
	ast.Inspect(body, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok && poolExecution(call) {
			n++
		}
		return true
	})
	return n
}

var fanoutFiles = map[string]struct {
	fset *token.FileSet
	file *ast.File
}{}

func funcBodyOf(t *testing.T, filename, name string) *ast.BlockStmt {
	t.Helper()
	parsed, ok := fanoutFiles[filename]
	if !ok {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, filename, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", filename, err)
		}
		parsed = struct {
			fset *token.FileSet
			file *ast.File
		}{fset, f}
		fanoutFiles[filename] = parsed
	}
	for _, decl := range parsed.file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == name && fn.Body != nil {
			return fn.Body
		}
	}
	t.Fatalf("%s declares no func %s — this fence names a function that no longer exists", filename, name)
	return nil
}

func nodeText(t *testing.T, filename string, node ast.Node) string {
	t.Helper()
	src, ok := fanoutFiles[filename]
	if !ok {
		t.Fatalf("%s was not parsed", filename)
	}
	start := src.fset.Position(node.Pos()).Offset
	end := src.fset.Position(node.End()).Offset
	raw, err := os.ReadFile(filename)
	if err != nil {
		t.Fatalf("read %s: %v", filename, err)
	}
	return string(raw[start:end])
}
