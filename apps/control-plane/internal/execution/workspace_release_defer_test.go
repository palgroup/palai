package execution

// A DEFER THAT READ ITS ARGUMENTS TOO EARLY TURNED THE WHOLE RELEASE PATH OFF FOR A DAY, and nothing
// failed. This is the fence.
//
// WHAT HAPPENED. `ExecuteAttempt` ends with
//
//	defer o.releaseWorkspace(tenant, workspaceID, workspaceLeaseID)
//
// and Go evaluates a deferred call's ARGUMENTS at the `defer` statement, not at the exit. E09 T10 wrote
// it four lines BELOW the assignment, where that is correct. A.3 T5 (c025c9aa, 2026-08-04) moved
// provisioning below the dial and left the defer where it was, so the two strings it captured were EMPTY
// and releaseWorkspace returned immediately on an empty lease id. The workspace never went leased→ready
// and the writer lease was never released. Measured on the running stack the next day:
//
//	select state, max(created_at), count(*) from workspaces group by 1;
//	  ready  34, newest 2026-08-04 07:21Z   <- every one of them predates the change
//	  leased 17, newest 2026-08-04 22:12Z   <- every workspace created since, none with a live run
//
// WHY NOTHING WENT RED. The next attempt's acquireWriterLease reclaims a dangling lease on proof the
// holder is dead, so runs kept working; and the idle-release suite seeds its candidate by hand
// (`UPDATE workspaces SET state='ready'`), so it never needed production to produce one. What it cost was
// the feature: IdleWorkspacesForRelease selects `state = 'ready'` with no active writer lease, so a build
// where nothing releases has no idle candidate ever — no machine was handed back, and the occupancy A.4
// bills could never close.
//
// WHY THIS READS SYNTAX. There is no behaviour to observe: the mistake makes a call a silent no-op that
// logs nothing and errors nowhere, and the state it fails to reach is one a fixture can set for itself.
// The one thing that is unambiguous is the shape — a deferred call that must see values assigned later
// has to be wrapped in a func literal, which re-reads the variables at exit.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// deferredAtExit are the calls in ExecuteAttempt whose arguments are assigned AFTER the defer statement,
// so each one must be wrapped in a closure. Both are release paths for something the attempt is holding —
// the workspace's writer lease, and the machine occupancy's last-activity stamp — which is why getting
// this wrong is silent: nothing is asked for, so nothing complains when nothing is given back.
var deferredAtExit = []string{"releaseWorkspace", "keepMachineHeld"}

func TestExecuteAttemptDefersReadTheirArgumentsAtExit(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "orchestrator.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse orchestrator.go: %v", err)
	}

	body := executeAttemptBody(t, file)
	found := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		stmt, ok := n.(*ast.DeferStmt)
		if !ok {
			return true
		}
		// A closure defers nothing by value: whatever it names is read when it RUNS. That is the shape
		// being required, so a deferred func literal is accepted without looking inside it.
		if _, isClosure := stmt.Call.Fun.(*ast.FuncLit); isClosure {
			for _, name := range deferredAtExit {
				if callsMethod(stmt.Call.Fun, name) {
					found[name] = true
				}
			}
			return true
		}
		for _, name := range deferredAtExit {
			if callsMethod(stmt.Call.Fun, name) {
				// Marked found so the "no deferred call at all" check below stays quiet: this name IS
				// deferred, wrongly, and two failures for one defect send the reader looking for a second
				// one. The two messages describe different defects and must not both fire.
				found[name] = true
				t.Errorf("ExecuteAttempt defers o.%s(...) DIRECTLY at line %d. Go evaluates a deferred call's arguments at the defer statement, and this one's are assigned further down — so it would run with empty strings and give nothing back. Wrap it: defer func() { o.%s(...) }()",
					name, fset.Position(stmt.Pos()).Line, name)
			}
		}
		return true
	})

	for _, name := range deferredAtExit {
		if !found[name] {
			t.Errorf("no deferred o.%s(...) in ExecuteAttempt at all. This guard is an allow-list of things that must be given back on every exit; a name it cannot find is a release that stopped happening, not a guard that stopped being needed", name)
		}
	}
}

// executeAttemptBody returns ExecuteAttempt's body, failing loudly if the method is gone — a guard that
// silently found nothing to check is the failure mode this whole file is about.
func executeAttemptBody(t *testing.T, file *ast.File) *ast.BlockStmt {
	t.Helper()
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == "ExecuteAttempt" && fn.Body != nil {
			return fn.Body
		}
	}
	t.Fatal("ExecuteAttempt not found in orchestrator.go — this guard cannot check a function that moved, so it fails rather than passing over an empty walk")
	return nil
}

// callsMethod reports whether expr names the method — as the selector of a direct `defer o.name(...)`, or
// somewhere inside a deferred func literal. The selector alone is enough: each of these names appears once
// in this package.
func callsMethod(expr ast.Expr, name string) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == name {
			found = true
		}
		return !found
	})
	return found
}
