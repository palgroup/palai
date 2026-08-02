package stack

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"
)

// A COMPOSE-ONLY HELPER REACHED FROM A NATIVE BRING-UP IS THIS FILE'S WHOLE SUBJECT, and it is here
// because the same defect shipped THREE times in one evening, in three different functions, each written
// by somebody who knew about the other two.
//
//	`palai up --native` brought up its native control plane, then repaired desired-config drift by asking
//	compose to start the control-plane service — which wanted the port the native process had just taken.
//	One ordering gave `bind: address already in use`; the other killed the native process on bind, left
//	the container serving, and printed PROVEN LIVE for a round trip against the container it was replacing.
//
//	Then the Slack step read the COMPOSE container's logs to decide whether Socket Mode was connected. On
//	a native bring-up that log holds the previous posture's boot or nothing, so a socket that WAS connected
//	— and had said so in .palai/control-plane.log seconds earlier — read as disconnected, and the step
//	"repaired" it by recreating the same absent container.
//
// WHY A TEST RATHER THAN CARE: none of these fail loudly. A compose command against a service in an
// inactive profile exits non-zero with a message about a container, and reading an empty log looks
// exactly like reading a log with nothing in it. Both render as "the thing you asked about is not
// there", which is the one answer that sends a reader in the wrong direction.
//
// So the rule is structural: the compose-only helpers may be called ONLY from a function that has already
// decided the shape — the shape-dispatchers themselves — and never from the bring-up path directly.
func TestComposeOnlyHelpersAreReachedThroughAShapeDispatcher(t *testing.T) {
	// composeOnly are helpers whose bodies drive `docker compose` against the control-plane SERVICE. A
	// native deployment does not run that service.
	composeOnly := map[string]string{
		"recreateControlPlane": "restartControlPlane(cfg, p, get, native)",
	}
	// dispatchers are the functions allowed to call them: each one picks by the native flag, which is
	// what makes the call correct rather than lucky.
	dispatchers := map[string]bool{
		"restartControlPlane": true,
		"controlPlaneLog":     true,
	}

	fset := token.NewFileSet()
	pkg, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	for _, p := range pkg {
		for path, file := range p.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil || dispatchers[fn.Name.Name] {
					continue
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					id, ok := call.Fun.(*ast.Ident)
					if !ok {
						return true
					}
					if want, bad := composeOnly[id.Name]; bad {
						t.Errorf("%s:%d: %s calls %s directly.\n"+
							"  %s drives `docker compose` against the control-plane SERVICE, which a NATIVE\n"+
							"  deployment does not run — the call fails with a message about a container, which reads\n"+
							"  as 'the control plane is broken' rather than 'this deployment has no such container'.\n"+
							"  Call %s instead, or add %s to the dispatcher list if it decides the shape itself.",
							path, fset.Position(id.Pos()).Line, fn.Name.Name, id.Name, id.Name, want, fn.Name.Name)
					}
					return true
				})
			}
		}
	}
}

// AND THE DISPATCHERS MUST ACTUALLY BRANCH. A restartControlPlane that forgot its native arm would pass
// the test above while doing exactly the wrong thing — the defect would simply have moved one function
// down, which is where this tree has watched corrections go before.
func TestTheShapeDispatchersBranchOnNative(t *testing.T) {
	fset := token.NewFileSet()
	pkg, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	// want maps each dispatcher to the native-side call its branch must make.
	want := map[string]string{
		"restartControlPlane": "restartNative",
		"controlPlaneLog":     "ReadFile",
	}
	seen := map[string]bool{}
	for _, p := range pkg {
		for _, file := range p.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				needle, tracked := want[fn.Name.Name]
				if !tracked {
					continue
				}
				seen[fn.Name.Name] = true
				var reads bool
				var branches bool
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					if _, ok := n.(*ast.IfStmt); ok {
						branches = true
					}
					if id, ok := n.(*ast.Ident); ok && id.Name == needle {
						reads = true
					}
					if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == needle {
						reads = true
					}
					return true
				})
				if !branches || !reads {
					t.Errorf("%s does not dispatch: branches=%v, calls %s=%v.\n"+
						"  A dispatcher that lost its native arm passes the reachability test above while doing\n"+
						"  precisely the thing that test exists to prevent.", fn.Name.Name, branches, needle, reads)
				}
			}
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("dispatcher %s is gone. Either it was renamed — update this test — or the shape decision\n"+
				"  was inlined back into its callers, which is the arrangement the three shipped defects had.", name)
		}
	}
}
