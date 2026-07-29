package fleet_test

// THE FENCE FOR THIS TASK'S OWN SIGNATURE FAILURE (E24 T4).
//
// T2 built `ResolvePool` — four ordered sources, a table-driven proof of each step — and NOTHING in the
// tree called it. Every run still went to whichever machine the single queue handed over, and every test
// in that package passed. This repository has shipped a fully-built, fully-tested, unreachable surface
// at least three times: E19 T9 found `CreateSlackConnection` reachable from nothing, E23 found
// `DecideToolApproval` in the same state, and T3 fenced three composition-root sites because of it.
//
// So the two halves of THIS task are fenced by name, in the two files that have to contain them, matched
// on the selector rather than on a line or a substring — a rename must break this and a reformat must
// not. Neither half can be caught by the proofs above it: the component tests construct the orchestrator
// and the gateway themselves, so they would pass against a binary that wires neither.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// callsMade returns which of the tracked selector/function names are called anywhere in a file.
func callsMade(t *testing.T, path string, names map[string]string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	found := map[string]bool{}
	for name := range names {
		found[name] = false
	}
	ast.Inspect(parsed, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			if _, tracked := found[fn.Sel.Name]; tracked {
				found[fn.Sel.Name] = true
			}
		case *ast.Ident:
			if _, tracked := found[fn.Name]; tracked {
				found[fn.Name] = true
			}
		}
		return true
	})
	return found
}

// TestPlacementIsReachedFromTheProductionDispatchPath pins the orchestrator half: the kernel that every
// dispatch worker drives resolves a pool, records it, and parks a run whose pool is empty. Without these
// three calls in ExecuteAttempt's file, placement is a pure function nobody asks and the park is an error
// message.
func TestPlacementIsReachedFromTheProductionDispatchPath(t *testing.T) {
	sites := map[string]string{
		"place":              "ExecuteAttempt resolves WHERE the attempt runs",
		"parkForCapacity":    "an empty pool parks the run instead of failing the attempt",
		"ErrPoolHasNoRunner": "the empty-pool answer is distinguished from a timeout",
	}
	// ErrPoolHasNoRunner is a value, not a call, so it is checked as a mention rather than through
	// callsMade: what matters is that the dial error arm reads it at all.
	found := callsMade(t, "../execution/orchestrator.go", map[string]string{
		"place": sites["place"], "parkForCapacity": sites["parkForCapacity"],
	})
	for name, why := range map[string]string{"place": sites["place"], "parkForCapacity": sites["parkForCapacity"]} {
		if !found[name] {
			t.Errorf("orchestrator.go: nothing calls %s — %s, so this task's behaviour is unreachable from the shipped binary", name, why)
		}
	}
	if !mentionsIdent(t, "../execution/orchestrator.go", "ErrPoolHasNoRunner") {
		t.Errorf("orchestrator.go does not mention ErrPoolHasNoRunner: without the distinction an empty pool is a timeout, which is the ~2.5-minute dead-letter this task removes")
	}
}

// TestTheCapacityWakerIsWiredIntoTheGateway pins the gateway half. A park with no wake is strictly worse
// than the bug it replaces — a run that used to die in two and a half minutes would wait forever — so
// this is the one line in the composition root whose absence must be a test failure rather than a
// support ticket.
func TestTheCapacityWakerIsWiredIntoTheGateway(t *testing.T) {
	const mainFile = "../../cmd/palai-control-plane/main.go"
	found := callsMade(t, mainFile, map[string]string{
		"SetCapacityWaker":   "the gateway wakes a parked run when a machine joins its pool",
		"startRunnerGateway": "the gateway is started at all",
	})
	for name, wired := range found {
		if !wired {
			t.Errorf("%s: nothing calls %s — a run parked for want of a machine would then wait FOREVER, which is worse than the dead-letter it replaced", mainFile, name)
		}
	}
}

// mentionsIdent reports whether a file names an identifier anywhere.
func mentionsIdent(t *testing.T, path, name string) bool {
	t.Helper()
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	found := false
	ast.Inspect(parsed, func(node ast.Node) bool {
		if ident, ok := node.(*ast.Ident); ok && ident.Name == name {
			found = true
		}
		return true
	})
	return found
}
