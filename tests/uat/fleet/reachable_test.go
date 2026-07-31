package fleet

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// THE JOB A GATE EXISTS FOR: every exported function and method E24 added must be REACHABLE FROM THE SHIPPED
// BINARY. This repository has shipped "fully built, fully tested, reachable from nothing" FOUR TIMES —
// CreateSlackConnection (found by E19 T9), DecideToolApproval (found by E23), the pool key's own composition
// root (found inside this epic, by T3), and Revoke/Resume (found inside this epic, by T5, which is where the
// count reached four). Every test in every one of those packages passed. A fail-closed security surface with
// no production caller is a feature that does not exist, and behaviour tests cannot see it: they construct
// the thing themselves.
//
// So this sweep parses the tree and, for each target below, requires a CALL SITE in a non-test file that is
// not the file declaring it. Three false-positive shapes are excluded by construction, and each one is a real
// mistake somebody would otherwise make here:
//
//  1. AN INTERFACE METHOD DECLARATION IS NOT A CALL. `Registry interface { Register(...) }` mentions the name
//     in a shape that a grep counts and an AST does not: only ast.CallExpr is collected, so a symbol whose
//     only other mention is an interface contract still fails.
//  2. A CALLER IN THE SAME FILE IS NOT REACH. A method called only by its own file's other methods is
//     internally consistent and externally dead, so the declaring file is excluded from its own evidence.
//  3. A tests/ HELPER IS NOT PRODUCTION. Everything under tests/ is skipped when collecting call sites —
//     which is the whole point, because that is exactly where the four historical holes had their callers.
//
// AND A FOURTH SHAPE THE BRIEF DID NOT NAME, WHICH THIS SWEEP FOUND BY REPORTING IT AS A HOLE: a symbol
// handed to something else AS A FUNCTION VALUE is never an ast.CallExpr. `HeartbeatLoop` is wired by
// `supervisor.Supervise(ctx, "runner-heartbeat", gateway.HeartbeatLoop)` — supervised, restarted, entirely
// reachable, and invisible to a call-only collector. The first draft of this test called it unreachable. So
// mentions are collected from CALL ARGUMENTS and assignment values as well, which is strictly wider than a
// call and still narrower than a grep: an interface's method declaration lives in an InterfaceType field
// list and appears in neither position, so false-positive shape 1 stays excluded.
//
// THE TARGET LIST IS HAND-WRITTEN AND THAT IS DELIBERATE. Deriving it from `git diff` against a fork point
// would make the gate depend on a ref that stops existing after a squash, and would silently shrink to
// nothing the day the diff came back empty — a guard that cannot fire. A reader checks this list against the
// epic's merge commits; the list itself is the reviewable artifact.

// e24ExportedSurface is every exported function or method E24's six merged tasks added, grouped by the task
// that added it. Constructors, error values and types are deliberately absent: a type with no constructor
// call is caught by its constructor's row, and an error value's reachability is its returner's.
var e24ExportedSurface = map[string][]string{
	// T1 — the registry. `Register` and `RecordSeen` are the enrolment and renewal paths; a registry nothing
	// writes to is a table.
	"T1 runner registry": {
		"NewStore", "Register", "RecordSeen", "SetRegistry", "ResolvePool",
	},
	// T2 — pools. `Pool` is the posture comparison at the door and `ListPools` is the operator's read.
	"T2 pools": {
		"Pool", "ListPools", "ListRunnerPools", "Waiting",
	},
	// T3 — the pool key. THIS GROUP IS WHY THE SWEEP EXISTS: T3 found its own composition root unwired and
	// fenced three call sites by name. `RedeemPoolKey` is the door itself; `Mint`, `Revoke` and `List` are the
	// operator's verbs, and a mint with no caller is a credential nobody can create.
	"T3 pool enrolment key": {
		"NewPoolEnrollmentKeys", "Mint", "Revoke", "RedeemPoolKey", "SetPoolKeys",
		"MintRunnerPoolKey", "RevokeRunnerPoolKey", "ListRunnerPoolKeys",
	},
	// T4 — placement, the tenant and the park. `ParkRunForCapacity` and `RecordRunPool` are the two writes
	// that make a parked run findable and a placement decision auditable.
	"T4 placement and park": {
		"ParkRunForCapacity", "RecordRunPool", "SetCapacityWaker",
	},
	// T5 — one machine's lifecycle. `RevokeRunner` and `ResumeRunner` are the pair whose ABSENCE from every
	// production path was §3.6 D15 — SAN-011's hard stop, implemented and unreachable — so their rows here
	// are the fence against that returning.
	// `Heartbeat` itself is deliberately NOT a target: its only production caller is `HeartbeatLoop` in the
	// same file, which IS a target and IS supervised, so it is reached through that row. Listing it would
	// force either a false red or a same-file exemption that weakens the rule for everything else.
	"T5 machine lifecycle": {
		"RevokeRunner", "ResumeRunner", "CordonRunner", "RunnerActiveLeases",
		"SetRunnerState", "HeartbeatLoop", "WithCapacityParkTTL",
	},
	// T6 — the waiting room. `Approve` is the human's decision and `ApproveRunner` is what lets the admitted
	// machine into its pool's rendezvous; without the second the operator's approval would land on a row and
	// change nothing.
	"T6 strict enrolment": {
		"Approve", "ApproveRunner",
	},
}

// unreachableWithAFiledGap is EMPTY, and it is kept rather than deleted because the mechanism is the point:
// a symbol this sweep finds with no production reach is named here with its gap id instead of being dropped
// from the target list, since a target quietly removed is a hole nobody meets again.
//
// IT HELD ONE ENTRY AND E28 T1 CLOSED IT. `RunnerGateway.Waiting(poolID)` counts the attempts queued for a
// pool with no free machine — "the number behind the one question an operator of a fleet actually asks",
// says its own doc comment — and nothing read it from E24 until E28. What kept it filed rather than fixed
// was a placement decision this file refused to make from an exit gate: the value is PER POOL, pool ids are
// TENANT-SCOPED NAMES, and the endpoint exposing its sibling `Connected()` is UNAUTHENTICATED. E28 T1 put it
// where `FLT-P14` said it belonged — a per-pool field on the authenticated runner-pool read
// (`RegistryAPI.ListRunnerPools`, rendered by `api.runnerPoolView`) — and the loop below is what made the
// step unskippable: a listed symbol that HAS gained reach fails, so the exception could not be left behind.
var unreachableWithAFiledGap = map[string]string{}

// TestEveryE24ExportedSurfaceHasAProductionCaller is the sweep. A symbol with no production reach fails with
// the epic task that added it named, because "which task shipped this unreachable" is the first question a
// reader asks.
func TestEveryE24ExportedSurfaceHasAProductionCaller(t *testing.T) {
	root := repoRoot(t)
	callers := collectProductionCallSites(t, root)

	// The exception list is checked FIRST and in the direction that can go stale: a symbol listed here that
	// HAS gained reach must be removed, or the list becomes a place where a fixed gap goes on being reported
	// as open — which is worse than no list, because the next reader trusts it.
	for symbol, gap := range unreachableWithAFiledGap {
		if files := callers[symbol]; len(files) != 0 {
			t.Errorf("%s is listed as unreachable with a filed gap (%s) but IS now reached from %v — delete the exception and close the gap row, or this list starts lying about what is open", symbol, gap, files)
		}
	}

	tasks := make([]string, 0, len(e24ExportedSurface))
	for task := range e24ExportedSurface {
		tasks = append(tasks, task)
	}
	sort.Strings(tasks)

	for _, task := range tasks {
		for _, symbol := range e24ExportedSurface[task] {
			files := callers[symbol]
			if len(files) == 0 {
				if gap, filed := unreachableWithAFiledGap[symbol]; filed {
					t.Logf("%-22s %-26s UNREACHABLE, filed: %s", task, symbol, gap)
					continue
				}
				t.Errorf("%s: %s has NO production reach anywhere outside its own file — that is the shape this repository has shipped four times (CreateSlackConnection, DecideToolApproval, the pool key's composition root, Revoke/Resume), and every test in those packages passed. A surface no shipped path constructs is a feature that does not exist. Reach counts a call, a function VALUE handed to a supervisor, and an assignment — so if this really is wired, it is wired somewhere this sweep cannot see and the sweep is what needs fixing", task, symbol)
				continue
			}
			t.Logf("%-22s %-26s reached from %s", task, symbol, strings.Join(files, ", "))
		}
	}
}

// TestTheReachabilitySweepCanActuallyFail is the guard for the guard. A sweep that found a caller for
// everything — because its collector was broken, or because it counted declarations, or because it walked the
// wrong directory — would report exactly the same green as this one. So it asks for a name that CANNOT have a
// production caller and requires the collector to say so.
func TestTheReachabilitySweepCanActuallyFail(t *testing.T) {
	callers := collectProductionCallSites(t, repoRoot(t))
	if len(callers) < 100 {
		t.Fatalf("the collector found call sites for only %d distinct symbols — it is not walking the tree, so every green above is a green over nothing", len(callers))
	}
	if files := callers["ThisFunctionIsNotInTheTreeAtAll"]; len(files) != 0 {
		t.Errorf("the collector reports call sites %v for a name that does not exist — it is not matching what it claims to match", files)
	}
	// And the interface-declaration exclusion, checked rather than asserted: `SweepExpiredCapacityParks` is
	// declared on an interface in the execution package. If the collector counted declarations this would be
	// indistinguishable from a call, which is the first false positive this sweep had to exclude.
	for _, file := range callers["SweepExpiredCapacityParks"] {
		if strings.Contains(file, "_test.go") {
			t.Errorf("the collector counted a _test.go file (%s) as a production call site — the four historical holes all had their callers in tests", file)
		}
	}
}

// collectProductionCallSites parses every non-test .go file outside tests/ and returns, per called name, the
// repo-relative files that CALL it. Only ast.CallExpr is inspected, so an interface method declaration, a
// struct field of a function type and a doc comment all fail to register — which is the point.
func collectProductionCallSites(t *testing.T, root string) map[string][]string {
	t.Helper()
	byName := map[string]map[string]bool{}
	scanned := 0

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		if d.IsDir() {
			switch {
			case d.Name() == "node_modules" || d.Name() == ".git" || d.Name() == "dist":
				return fs.SkipDir
			// tests/ is skipped WHOLESALE and that is the third false-positive exclusion: a helper under
			// tests/uat calling a symbol proves the symbol compiles, not that anything ships it.
			case rel == "tests":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		file, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if perr != nil {
			return nil // a file this Go version cannot parse is not evidence either way
		}
		scanned++
		note := func(expr ast.Expr) {
			name := ""
			switch e := expr.(type) {
			case *ast.Ident:
				name = e.Name
			case *ast.SelectorExpr:
				name = e.Sel.Name
			}
			if name == "" {
				return
			}
			if byName[name] == nil {
				byName[name] = map[string]bool{}
			}
			byName[name][rel] = true
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CallExpr:
				// The call itself…
				note(node.Fun)
				// …and every argument that is a bare name or a selector, which is how a FUNCTION VALUE
				// travels: `supervisor.Supervise(ctx, "runner-heartbeat", gateway.HeartbeatLoop)`. Wider
				// than a call and still not a grep — an interface's method declaration is in neither
				// position, so it never registers.
				for _, arg := range node.Args {
					note(arg)
				}
			case *ast.AssignStmt:
				for _, rhs := range node.Rhs {
					note(rhs)
				}
			case *ast.CompositeLit:
				// A method value stored in a struct or a map literal — the registration-table shape.
				for _, elt := range node.Elts {
					if kv, ok := elt.(*ast.KeyValueExpr); ok {
						note(kv.Value)
						continue
					}
					note(elt)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk the tree: %v", err)
	}
	if scanned < 200 {
		t.Fatalf("the sweep parsed only %d non-test Go files — it is not walking the tree", scanned)
	}

	// The declaring file is excluded from its own evidence, so a method called only by its own file's other
	// methods still counts as unreachable.
	declaredIn := declaringFiles(t, root)
	out := map[string][]string{}
	for name, files := range byName {
		kept := make([]string, 0, len(files))
		for file := range files {
			if declaredIn[name] == file {
				continue
			}
			kept = append(kept, file)
		}
		if len(kept) == 0 {
			continue
		}
		sort.Strings(kept)
		out[name] = kept
	}
	return out
}

// declaringFiles maps each exported name in e24ExportedSurface to the repo-relative file that DECLARES it, so
// the sweep can refuse that file as its own witness. A name declared in more than one place (a fake's method,
// an interface's) resolves to the non-test production declaration; the walk skips tests/ and _test.go, so
// only production declarations are seen at all.
func declaringFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	targets := map[string]bool{}
	for _, symbols := range e24ExportedSurface {
		for _, s := range symbols {
			targets[s] = true
		}
	}
	out := map[string]string{}
	// Only the two packages E24 declares this surface in are searched, which keeps the map unambiguous: a
	// same-named method on an unrelated type elsewhere is not this symbol.
	for _, dir := range []string{
		filepath.Join("apps", "control-plane", "internal", "fleet"),
		filepath.Join("apps", "control-plane", "internal", "execution"),
		filepath.Join("apps", "control-plane", "internal", "store"),
		// T4's two writes live in the coordinator rather than in the fleet package, because the run row they
		// touch is the coordinator's. The sweep found this by refusing to look for a name it could not find a
		// declaration for — which is the vacuity check earning its place before any target had been judged.
		filepath.Join("packages", "coordinator"),
	} {
		entries, err := os.ReadDir(filepath.Join(root, dir))
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			rel := filepath.Join(dir, e.Name())
			file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, rel), nil, 0)
			if err != nil {
				continue
			}
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || !targets[fn.Name.Name] {
					continue
				}
				if _, already := out[fn.Name.Name]; !already {
					out[fn.Name.Name] = rel
				}
			}
		}
	}
	missing := make([]string, 0)
	for name := range targets {
		if _, ok := out[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) != 0 {
		slices.Sort(missing)
		t.Fatalf("these targets are not DECLARED in the fleet/execution/store packages at all: %v — either the name is wrong (so the sweep has been looking for nothing, which is the vacuous form of this guard) or the symbol moved", missing)
	}
	return out
}
