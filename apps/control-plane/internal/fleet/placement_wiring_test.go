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

// TestTheMachineLifecycleIsWiredIntoTheCompositionRoot is E24 T5's fence, and it is the one this task
// exists for. §3.6 D15 measured `Revoke()` — SAN-011's hard stop, written for a compromised runner —
// implemented, tested, registered in the UAT catalogue, and CALLED BY NOTHING. This repository has now
// shipped that exact shape four times (E19 T9's CreateSlackConnection, E23's DecideToolApproval, T2's
// ResolvePool, and this), so the three lines that make the new surfaces reachable are pinned BY NAME.
//
// None of them can be caught by the proofs above: every component test in this epic constructs the gateway,
// the registry and the router itself, so all of them would pass against a binary that wired none.
//
// WHY THESE THREE AND NOT FOUR OR TWO. `WithLifecycle` is what joins the API surface to the live gateway —
// without it a cordon writes a row and reaches no session, which for a Mac mid-run means hours. `WithRunners`
// is what mounts the routes at all. `HeartbeatLoop` is the reaper: without it `last_seen_at` freezes the
// moment a machine finishes connecting and a half-open session keeps its queue slot forever. Any two of the
// three is a half-built feature, which is why it counts to three — T3's fence took the same position for the
// same reason.
func TestTheMachineLifecycleIsWiredIntoTheCompositionRoot(t *testing.T) {
	const mainFile = "../../cmd/palai-control-plane/main.go"
	sites := map[string]string{
		"WithLifecycle": "a cordon/revoke reaches the SESSIONS that machine is holding, not only its row",
		"WithRunners":   "the lifecycle routes are mounted on the public API at all",
	}
	for name, wired := range callsMade(t, mainFile, sites) {
		if !wired {
			t.Errorf("%s: nothing calls %s — %s. A security control with no operator surface is a security control that does not exist (§3.6 D15)", mainFile, name, sites[name])
		}
	}
	// HeartbeatLoop is checked as a MENTION rather than as a call, and the distinction is the supervisor's
	// signature rather than a weakening: it is handed to `supervisor.Supervise` as a method VALUE — which is
	// the idiomatic shape for a `func(context.Context) error` and the one every other supervised loop in that
	// file uses — so there is no CallExpr to find. An *ast.Ident is not a comment, so a mention still means
	// the composition root names it in code.
	if !mentionsIdent(t, mainFile, "HeartbeatLoop") {
		t.Errorf("%s does not name HeartbeatLoop: `runners.last_seen_at` would freeze the moment a machine finished connecting, and a session alive to the kernel and dead to the process would keep its queue slot and be handed the next lease forever", mainFile)
	}
	// The park-TTL reaper is the other half of the same argument: T4 filed FLT-P7 saying a run parked in a
	// pool that will never have a machine waits FOREVER and named T5 as the owner. A TTL nothing reads is
	// that ceiling still open with a knob bolted to it.
	if !callsMade(t, mainFile, map[string]string{"WithCapacityParkTTL": ""})["WithCapacityParkTTL"] {
		t.Errorf("%s: nothing calls WithCapacityParkTTL — PALAI_FLEET_PARK_TTL would be an environment variable that does nothing, and T4's FLT-P7 would stay open", mainFile)
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
