package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"strings"
	"testing"
)

// WHAT A MACHINE CAN ISOLATE WITH IS MEASURED BEFORE IT ENROLS, AND ON THE REAL SOCKET.
//
// This file used to assert a refusal: a native Mac with no palai-agentd killed the process. That was
// fail-closed in the wrong place. It made one administrator action a precondition for joining ANY pool,
// including a single-customer pool where the login account is the boundary the operator intended — and a
// hundred cloud Macs cannot each wait for a person. The refusal now lives at the gateway, which is the
// only party that knows what the pool requires (fleet.Store.Register -> isolationSatisfied), and the
// machine's job is to state what it measured.
//
// WHICH MODES A MACHINE CLAIMS IS device.Measure's PROPERTY AND IS TESTED THERE. What cannot be tested
// there, and is the whole of this file, is that main() performs the measurement AT ALL, BEFORE enrolment,
// and against the socket production uses. A measurement written and never reached is the exact shape
// this tree keeps finding, and its absence is invisible from every other surface.
//
// There is no behaviour to observe for it: the function cannot watch itself being called and driving the
// whole binary would need a control plane. So the call site is what is read.
func TestIsolationIsMeasuredBeforeEnrolmentOnTheRealSocket(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing main.go: %v", err)
	}
	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "main" && fn.Recv == nil {
			body = fn.Body
		}
	}
	if body == nil {
		t.Fatal("main.go declares no main(); everything below would be vacuously green")
	}

	measure, enrol, socket := -1, -1, false
	for i, stmt := range body.List {
		ast.Inspect(stmt, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				if fn.Name == "reportIsolation" && measure < 0 {
					measure = i
					// AND ON THE REAL SOCKET. A probe pointed at a path production never uses would pass
					// the ordering check below and measure nothing on a machine.
					socket = strings.Contains(exprText(fset, call), "macagent.DefaultSocketPath")
				}
			case *ast.SelectorExpr:
				if pkg, ok := fn.X.(*ast.Ident); ok && pkg.Name == "runner" && fn.Sel.Name == "Enroll" && enrol < 0 {
					enrol = i
				}
			}
			return true
		})
	}

	switch {
	case measure < 0:
		t.Fatal("main() never calls reportIsolation: the measurement is written and nothing reaches it")
	case enrol < 0:
		t.Fatal("main() never calls runner.Enroll; this test can no longer tell whether the measurement precedes it")
	case measure >= enrol:
		t.Errorf("reportIsolation is called at statement %d and runner.Enroll at %d: a fact established AFTER enrolment describes a machine the control plane has already counted", measure, enrol)
	}
	if !socket {
		t.Error("main() calls reportIsolation with a prober that is not macagent.DefaultSocketPath: a probe pointed anywhere else measures nothing on a real machine")
	}
}

// exprText renders a node back to source so an argument can be read as written.
func exprText(fset *token.FileSet, node ast.Node) string {
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, node); err != nil {
		return ""
	}
	return buf.String()
}
