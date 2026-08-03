//go:build security

package tenancy

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// Task 3's behaviour test (TestEveryFleetRouteRefusesATenantKey, internal/store) DRIVES the routes; this
// test inspects their REGISTERED SHAPE. The two catch different things: the behaviour test catches a
// route that has no gate, this test catches a route registered WITHOUT one at compile time and names the
// file:line — the failure a human-triggered security scan already flagged once while Task 3 was
// mid-flight.
func TestEveryFleetHandleFuncIsSystemOnly(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "../../../apps/control-plane/api/router.go", nil, 0)
	if err != nil {
		t.Fatalf("router.go ayrıştırılamadı: %v", err)
	}
	var checked int
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 2 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "HandleFunc" {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok {
			return true
		}
		pattern := strings.Trim(lit.Value, `"`)
		if !strings.Contains(pattern, "/v1/runners") && !strings.Contains(pattern, "/v1/runner-pool") {
			return true
		}
		checked++
		wrapped, _ := call.Args[1].(*ast.CallExpr)
		if wrapped == nil {
			t.Errorf("%s: %s kapısız kayıtlı (systemOnly bekleniyordu)", fset.Position(call.Pos()), pattern)
			return true
		}
		if id, ok := wrapped.Fun.(*ast.Ident); !ok || id.Name != "systemOnly" {
			t.Errorf("%s: %s systemOnly ile sarılmamış", fset.Position(call.Pos()), pattern)
		}
		return true
	})
	if checked == 0 {
		t.Fatal("hiçbir filo rotası denetlenmedi — desen bozuk, test VAKUM")
	}
	t.Logf("denetlenen filo rotası: %d", checked)
}
