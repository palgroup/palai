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
// test inspects their REGISTERED SHAPE. Two differences make the pair worth keeping, and both are
// checkable by reading the two files rather than by trusting this sentence:
//
//  1. THE BEHAVIOUR TEST CAN DECLINE TO RUN. It t.Skips when PALAI_COMPONENT_POSTGRES_URL is unset
//     (fleet_gate_component_test.go), so on any invocation that does not stand up the component tier it
//     asserts nothing at all — neither pass nor fail. This one has no skip condition and rides
//     `TEST=tenancy scripts/test/security`, whose run_tenancy carries no -run allow-list. So of the two,
//     only this one is guaranteed to fire.
//  2. IT NAMES THE LINE. A refused HTTP status says a route is ungated; a file:line says which
//     registration to fix.
//
// What it cannot do is check what systemOnly MEANS — that a wrapped route actually refuses a tenant key
// is the behaviour test's claim, not this one's. This test only asserts the identifier is called at the
// registration site.
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
