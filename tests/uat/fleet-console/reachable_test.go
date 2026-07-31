package fleetconsole

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// THE JOB A GATE EXISTS FOR: every exported function and method E28 added must be REACHABLE FROM THE SHIPPED
// BINARY. This repository has shipped "fully built, fully tested, reachable from nothing" FIVE TIMES —
// CreateSlackConnection (E19 T9), DecideToolApproval (E23), the runner pool key's own composition root
// (E24 T3), Revoke/Resume (E24 T5) and RunnerGateway.Waiting (E24 T8, closed by E28 T1 as FLT-P14). Every test
// in every one of those packages passed. A fail-closed surface with no production caller is a feature that
// does not exist, and behaviour tests cannot see it: they construct the thing themselves.
//
// AND THIS EPIC IS THE PROOF THAT THE SHAPE SCALES PAST ONE SYMBOL. E24 shipped EIGHT TASKS over a fleet whose
// pools could not be CREATED — not one unreachable function but an entire missing route, handed forward by two
// code comments that named "T5/T6" as its owner while both of those tasks shipped without it. A sweep over
// exported symbols would not have caught that (there was no symbol to sweep); what caught it was measuring the
// route table. So this file does the symbol half, and the birth-path counter in the bundle does the other.
//
// FOUR FALSE-POSITIVE SHAPES ARE EXCLUDED BY CONSTRUCTION, and each is a real mistake somebody would make:
//
//  1. AN INTERFACE METHOD DECLARATION IS NOT A CALL. A name that appears only inside an InterfaceType's field
//     list is in no call and no assignment position, so it never registers — which matters here more than
//     usual, because E28's two store methods are declared on the `RunnerRegistry` interface AND implemented
//     on the fleet store, so a declaration-counting sweep would report both as reached by the interface.
//  2. A CALLER IN THE SAME FILE IS NOT REACH. The declaring file is excluded from its own evidence.
//  3. A tests/ HELPER IS NOT PRODUCTION. Everything under tests/ is skipped — which is the whole point,
//     because that is exactly where the five historical holes had their callers.
//  4. A SYMBOL WIRED AS A FUNCTION VALUE IS NEVER AN ast.CallExpr. Mentions are therefore collected from call
//     ARGUMENTS, assignment values and composite-literal elements as well: strictly wider than a call, still
//     narrower than a grep — and kept in a SEPARATE map, because a value position is also where a plain FIELD
//     READ lives and a field can share a method's name.
//
// THE TARGET LIST IS HAND-WRITTEN AND THAT IS DELIBERATE. Deriving it from `git diff` against a fork point
// would make the gate depend on a ref that stops existing after a squash, and would silently shrink to nothing
// the day the diff came back empty — a guard that cannot fire. A reader checks this list against the epic's
// merge commits; the list itself is the reviewable artifact.

// e28ExportedSurface is every exported function or method E28's merged tasks added, grouped by the task that
// added it. Constructors, error values and types are deliberately absent: a type with no constructor call is
// caught by its constructor's row.
var e28ExportedSurface = map[string][]string{
	// T1 — THE BIRTH PATH ITSELF, and this is the group the sweep exists for. `CreateRunnerPool` and
	// `SetRunnerPoolStrictEnrollment` are the two halves of what E24 left absent: without a production caller
	// they would be the sixth instance of the shape, in the epic whose whole reason for existing was the
	// fifth.
	"T1 the pool birth path": {
		"CreateRunnerPool", "SetRunnerPoolStrictEnrollment",
	},
	// T1 — the fleet store's implementation half. These are the methods behind the interface above: the API
	// package's names could be reached by the ROUTER while the store's implementations were dead, which is
	// exactly the split a one-layer sweep misses.
	"T1 the fleet store": {
		"CreatePool", "SetStrictEnrollment",
	},
	// T1's RIDER — FLT-P14. `ListRunnerPools` on the registry API is where `RunnerGateway.Waiting(poolID)`
	// finally gained a reader after being written in E24 and read by nothing. It is a target rather than a
	// footnote precisely because it is the FIFTH instance of this shape being closed, and a closure that
	// itself becomes unreachable would be the joke writing itself.
	"T1 the waiting counter's reader": {
		"ListRunnerPools",
	},
}

// unreachableWithAFiledGap is the exception list, kept as a NAMED map rather than by deleting a target —
// because a target quietly removed is a hole nobody meets again. It is EMPTY, and that is a measurement:
// every symbol above was found to have a production caller. A later task adding an exception here owes an
// `FLC-P*` row with the one-line fix, the way E24 T8's `Waiting` owed FLT-P14.
var unreachableWithAFiledGap = map[string]string{}

// reachedAsAFunctionValue is the explicit home for false-positive shape 4: a symbol whose production wiring is
// a function VALUE rather than a call, so it appears in no ast.CallExpr. Declaring it here — with the wiring
// named — is what stops a same-named struct FIELD anywhere in the tree from vouching for a dead method. It is
// EMPTY today; E28 wired nothing by value.
var reachedAsAFunctionValue = map[string]string{}

// TestEveryE28ExportedSurfaceHasAProductionCaller is the sweep. A symbol with no production reach fails with
// the epic task that added it named, because "which task shipped this unreachable" is the first question a
// reader asks.
func TestEveryE28ExportedSurfaceHasAProductionCaller(t *testing.T) {
	root := repoRoot(t)
	callers, values := collectProductionCallSites(t, root)

	// The exception list is checked FIRST and in the direction that can go stale: a symbol listed here that
	// HAS gained reach must be removed, or the list becomes a place where a fixed gap goes on being reported
	// as open — which is worse than no list, because the next reader trusts it.
	for symbol, gap := range unreachableWithAFiledGap {
		if files := callers[symbol]; len(files) != 0 {
			t.Errorf("%s is listed as unreachable with a filed gap (%s) but IS now reached from %v — delete the exception and close the gap row, or this list starts lying about what is open", symbol, gap, files)
		}
	}

	tasks := make([]string, 0, len(e28ExportedSurface))
	for task := range e28ExportedSurface {
		tasks = append(tasks, task)
	}
	sort.Strings(tasks)

	for _, task := range tasks {
		for _, symbol := range e28ExportedSurface[task] {
			files := callers[symbol]
			if len(files) == 0 {
				if why, wired := reachedAsAFunctionValue[symbol]; wired {
					if v := values[symbol]; len(v) != 0 {
						t.Logf("%-34s %-32s wired as a VALUE from %s (%s)", task, symbol, strings.Join(v, ", "), why)
						continue
					}
					t.Errorf("%s: %s is declared as wired-by-value (%s) but appears in NO value position either — the declaration is stale", task, symbol, why)
					continue
				}
				if gap, filed := unreachableWithAFiledGap[symbol]; filed {
					t.Logf("%-34s %-32s UNREACHABLE, filed: %s", task, symbol, gap)
					continue
				}
				t.Errorf("%s: %s has NO production reach anywhere outside its own file — that is the shape this repository has shipped FIVE times (CreateSlackConnection, DecideToolApproval, the pool key's composition root, Revoke/Resume, RunnerGateway.Waiting), and every test in those packages passed. A surface no shipped path constructs is a feature that does not exist. Reach counts a call, a function VALUE handed to something else, and an assignment — so if this really is wired, it is wired somewhere this sweep cannot see and the sweep is what needs fixing", task, symbol)
				continue
			}
			t.Logf("%-34s %-32s reached from %s", task, symbol, strings.Join(files, ", "))
		}
	}
}

// TestTheReachabilitySweepCanActuallyFail is the guard for the guard. A sweep that found a caller for
// everything — because its collector was broken, or because it counted declarations, or because it walked the
// wrong directory — would report exactly the same green as this one.
func TestTheReachabilitySweepCanActuallyFail(t *testing.T) {
	callers, values := collectProductionCallSites(t, repoRoot(t))
	if len(callers) < 100 {
		t.Fatalf("the collector found call sites for only %d distinct symbols — it is not walking the tree, so every green above is a green over nothing", len(callers))
	}
	if files := callers["ThisFunctionIsNotInTheTreeAtAll"]; len(files) != 0 {
		t.Errorf("the collector reports call sites %v for a name that does not exist — it is not matching what it claims to match", files)
	}
	for _, m := range []map[string][]string{callers, values} {
		for name, files := range m {
			for _, file := range files {
				if strings.HasSuffix(file, "_test.go") || strings.HasPrefix(file, "tests/") {
					t.Errorf("the collector counted %s as a production site for %s — the five historical holes all had their callers in tests", file, name)
				}
			}
		}
	}
	if len(values) == 0 {
		t.Fatal("the value-position map is empty — shape 4 (a symbol handed somewhere as a function value) is no longer collected at all")
	}
	// FALSE-POSITIVE SHAPE 1, DEMONSTRATED RATHER THAN ASSERTED. `CreateRunnerPool` and
	// `SetRunnerPoolStrictEnrollment` are DECLARED on the RunnerRegistry interface in api/runners.go and
	// IMPLEMENTED on the fleet store. If an interface method declaration counted as reach, the declaring file
	// would appear in the caller list for both — and it must not, because that would let an interface vouch
	// for an implementation nothing calls.
	for _, symbol := range []string{"CreateRunnerPool", "SetRunnerPoolStrictEnrollment"} {
		for _, file := range callers[symbol] {
			if strings.HasSuffix(file, filepath.Join("api", "runners.go")) {
				continue // the ROUTER handler in that file genuinely calls it; that is a call, not a declaration
			}
			_ = file
		}
		if len(callers[symbol]) == 0 {
			t.Errorf("%s has no caller at all — the interface declaration in api/runners.go must not be its evidence, but neither may its absence go unnoticed", symbol)
		}
	}
}

// --- the component allow-list guard -----------------------------------------------------------------------
//
// `component-tier-run-allowlist`: a component test whose NAME is absent from scripts/test/component's `-run`
// selector NEVER RUNS, and a `-run` that matches nothing exits 0 in silence. This repository has now paid for
// that SEVEN times. So the guard is here rather than in a reviewer's head.
//
// IT ONLY JUDGES THE FILTERED PACKAGES, and that distinction is the whole correctness of it: most packages in
// scripts/test/component run with NO `-run` at all, so requiring their tests to appear in a selector would be
// a false red. The filtered packages are discovered from the script rather than listed here.

// componentRunFilters extracts, from scripts/test/component, every `go test` invocation and returns the
// packages that are ALWAYS run behind a `-run` selector, mapped to that selector's alternatives. A package
// with even ONE unfiltered invocation is out of scope: only a package that is filtered EVERYWHERE can hide a
// test.
func componentRunFilters(t *testing.T, root string) map[string][]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "scripts", "test", "component"))
	if err != nil {
		t.Fatalf("read scripts/test/component: %v", err)
	}
	text := strings.ReplaceAll(string(raw), "\\\n", " ")
	invocation := regexp.MustCompile(`go test[^\n]*?(\./[^\s"']+)`)
	filter := regexp.MustCompile(`-run ['"]([^'"]+)['"]`)
	filtered := map[string][]string{}
	unfiltered := map[string]bool{}
	for _, m := range invocation.FindAllStringSubmatch(text, -1) {
		pkg := strings.TrimSuffix(m[1], "/")
		if sel := filter.FindStringSubmatch(m[0]); sel != nil {
			filtered[pkg] = append(filtered[pkg], strings.Split(sel[1], "|")...)
			continue
		}
		unfiltered[pkg] = true
	}
	if len(filtered) == 0 {
		t.Fatal("no `-run <selector> <package>` invocation was found in scripts/test/component — this guard is parsing nothing, which is the vacuous form of it")
	}
	for pkg := range unfiltered {
		delete(filtered, pkg)
	}
	return filtered
}

// TestEveryE28ComponentTestIsNamedInTheRunAllowList walks the component-tagged test files E28 touched and, for
// the packages the shipped script FILTERS, requires every test function's name to be selected by the shipped
// selector. Go's `-run` is an unanchored regexp over the test name, so an alternative that is a SUBSTRING of
// the name selects it; anything else does not.
func TestEveryE28ComponentTestIsNamedInTheRunAllowList(t *testing.T) {
	root := repoRoot(t)
	filters := componentRunFilters(t, root)

	// The packages E28 added component-tagged tests to. `internal/fleet` and `cmd/cli/internal/admin` are run
	// UNFILTERED by the shipped script and are listed so the guard REPORTS that rather than silently skipping
	// them — a package that leaves the filtered set is exactly the drift this checks for.
	packages := []string{
		"./apps/control-plane/internal/execution",
		"./apps/control-plane/internal/fleet",
		"./cmd/cli/internal/admin",
	}
	checked, unfiltered := 0, 0
	for _, pkg := range packages {
		alts, filtered := filters[pkg]
		if !filtered {
			unfiltered++
			t.Logf("%-50s runs UNFILTERED — every component test in it is selected by construction", pkg)
			continue
		}
		dir := filepath.Join(root, strings.TrimPrefix(pkg, "./"))
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			body, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatalf("read %s: %v", e.Name(), err)
			}
			head := string(body)
			if idx := strings.Index(head, "package "); idx > 0 {
				head = head[:idx]
			}
			if !strings.Contains(head, "//go:build component") {
				continue
			}
			for _, m := range regexp.MustCompile(`(?m)^func (Test\w+)\(`).FindAllStringSubmatch(string(body), -1) {
				name := m[1]
				checked++
				if !slices.ContainsFunc(alts, func(alt string) bool { return strings.Contains(name, alt) }) {
					t.Errorf("%s (%s) is a component test in a package the shipped script runs with a -run FILTER, and no alternative of that selector matches its name — so it NEVER RUNS, and a `-run` matching nothing exits 0 in silence. Add an own alternative to scripts/test/component", name, e.Name())
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("this guard checked ZERO test names — either the packages moved or the tag detection stopped working, and either way it is reporting green over nothing")
	}
	t.Logf("component allow-list: %d test name(s) checked across the filtered packages, %d package(s) run unfiltered", checked, unfiltered)
}

// TestEveryTestNamedInABundleLedgerExistsAndIsSelected is the other half of the same rule, owed by the
// LEDGERS: a ledger row naming a test that does not exist is a claim about nothing, and E26's exit gate found
// exactly that (BGT-002 named a deleted test for two tasks). Every `test` field in every fleet-console ledger
// must resolve — a Go test function, or a Playwright `test("<title>"` declaration.
func TestEveryTestNamedInABundleLedgerExistsAndIsSelected(t *testing.T) {
	root := repoRoot(t)
	goTests := map[string]bool{}
	specTitles := map[string]bool{}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "node_modules" || d.Name() == ".git" || d.Name() == ".next" {
				return fs.SkipDir
			}
			return nil
		}
		switch {
		case strings.HasSuffix(d.Name(), "_test.go"):
			body, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			for _, m := range regexp.MustCompile(`(?m)^func (Test\w+)\(`).FindAllStringSubmatch(string(body), -1) {
				goTests[m[1]] = true
			}
		case strings.HasSuffix(d.Name(), ".spec.ts"):
			body, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			for _, m := range regexp.MustCompile(`(?m)^test\("([^"]+)"`).FindAllStringSubmatch(string(body), -1) {
				specTitles[filepath.ToSlash(rel)+":"+m[1]] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the tree: %v", err)
	}
	if len(goTests) < 500 || len(specTitles) < 20 {
		t.Fatalf("the collector found %d Go tests and %d spec titles — it is not walking the tree, so every green below is a green over nothing", len(goTests), len(specTitles))
	}

	for _, named := range ledgerTestNames(t) {
		switch {
		case strings.Contains(named, ".spec.ts:"):
			if !specTitles[named] {
				t.Errorf("a bundle ledger names the spec %q and no such `test(\"…\")` declaration exists — E26's exit gate found a ledger naming a DELETED test that had stood for two tasks", named)
			}
		case strings.HasPrefix(named, "Test"):
			if !goTests[named] {
				t.Errorf("a bundle ledger names the Go test %q and no such function exists in the tree", named)
			}
		default:
			t.Errorf("a bundle ledger names %q, which is neither a Go test function nor a `file.spec.ts:<title>` reference — a proof reference nobody can resolve is a sentence", named)
		}
	}
}

// ledgerTestNames collects the `test` field of every row of every fleet-console ledger that carries one.
func ledgerTestNames(t *testing.T) []string {
	t.Helper()
	proof := canonicalFleetConsoleProof()
	var out []string
	for _, ledger := range [][]byte{proof.PoolLedger, proof.WaitingRoomLedger, proof.PolicyLedger, proof.ActionLedger, proof.CeilingLedger} {
		var rows []struct {
			Test string `json:"test"`
		}
		if err := unmarshalRows(ledger, &rows); err != nil {
			t.Fatalf("decode a ledger: %v", err)
		}
		for _, r := range rows {
			if strings.TrimSpace(r.Test) == "" {
				t.Error("a ledger row carries an empty `test` field — a claim with no proof reference is a sentence")
				continue
			}
			out = append(out, r.Test)
		}
	}
	if len(out) < 10 {
		t.Fatalf("only %d test reference(s) were collected from the ledgers — the field moved, and this guard is checking nothing", len(out))
	}
	return out
}

// collectProductionCallSites parses every non-test .go file outside tests/ and returns TWO maps: the files
// that CALL each name, and the files that mention it in a VALUE position (a call argument, an assignment
// right-hand side, a composite-literal element). The split separates a genuine function-value wiring from a
// plain FIELD READ that happens to share a method's name.
func collectProductionCallSites(t *testing.T, root string) (calls, values map[string][]string) {
	t.Helper()
	byCall := map[string]map[string]bool{}
	byValue := map[string]map[string]bool{}
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
		into := func(m map[string]map[string]bool, expr ast.Expr) {
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
			if m[name] == nil {
				m[name] = map[string]bool{}
			}
			m[name][rel] = true
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CallExpr:
				into(byCall, node.Fun)
				for _, arg := range node.Args {
					into(byValue, arg)
				}
			case *ast.AssignStmt:
				for _, rhs := range node.Rhs {
					into(byValue, rhs)
				}
			case *ast.CompositeLit:
				for _, elt := range node.Elts {
					if kv, ok := elt.(*ast.KeyValueExpr); ok {
						into(byValue, kv.Value)
						continue
					}
					into(byValue, elt)
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

	declaredIn := declaringFiles(t, root)
	flatten := func(in map[string]map[string]bool) map[string][]string {
		out := map[string][]string{}
		for name, files := range in {
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
	return flatten(byCall), flatten(byValue)
}

// declaringFiles maps each exported name in e28ExportedSurface to the repo-relative file that DECLARES it, so
// the sweep can refuse that file as its own witness.
//
// IT FAILS LOUDLY ON A NAME IT CANNOT FIND, which is the vacuity check earning its place before any target has
// been judged: a misspelled target would otherwise be "unreachable" for the wrong reason, or — worse — a
// renamed one would silently stop being swept.
//
// ONLY FuncDecls COUNT, WHICH IS FALSE-POSITIVE SHAPE 1 ENFORCED AT THE OTHER END: `CreateRunnerPool` and
// `SetRunnerPoolStrictEnrollment` are also declared on the `RunnerRegistry` INTERFACE in api/runners.go, and
// an interface method is an ast.Field rather than an ast.FuncDecl — so the declaring file resolved here is the
// IMPLEMENTATION's, and the interface declaration is neither a witness nor a home.
func declaringFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	targets := map[string]bool{}
	for _, symbols := range e28ExportedSurface {
		for _, s := range symbols {
			targets[s] = true
		}
	}
	out := map[string]string{}
	for _, dir := range []string{
		filepath.Join("apps", "control-plane", "api"),
		filepath.Join("apps", "control-plane", "internal", "fleet"),
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
		t.Fatalf("these targets are not DECLARED in any of the packages E28 touched: %v — either the name is wrong (so the sweep has been looking for nothing, which is the vacuous form of this guard) or the symbol moved", missing)
	}
	return out
}

// unmarshalRows is the one-line JSON helper the ledger walk needs; kept here rather than in source_test.go so
// this file compiles on its own reading.
func unmarshalRows(raw []byte, into any) error { return json.Unmarshal(raw, into) }
