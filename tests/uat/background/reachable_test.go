package background

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

	"github.com/palgroup/palai/tests/uat"
)

// THE JOB A GATE EXISTS FOR: every exported function and method E26 added must be REACHABLE FROM THE SHIPPED
// BINARY. This repository has shipped "fully built, fully tested, reachable from nothing" FIVE TIMES —
// CreateSlackConnection (found by E19 T9), DecideToolApproval (found by E23), the runner pool key's own
// composition root (found by E24 T3), Revoke/Resume (found by E24 T5) and host.Executor.WallTime (found by
// E25 T9). Every test in every one of those packages passed. A fail-closed security surface with no production
// caller is a feature that does not exist, and behaviour tests cannot see it: they construct the thing
// themselves.
//
// FOUR FALSE-POSITIVE SHAPES ARE EXCLUDED BY CONSTRUCTION, and each is a real mistake somebody would make:
//
//  1. AN INTERFACE METHOD DECLARATION IS NOT A CALL. A name that appears only inside an InterfaceType's field
//     list is in no call and no assignment position, so it never registers. E26 makes this one LIVE rather
//     than theoretical: `Start`, `Probe` and `Kill` are declared on toolbroker.BackgroundRunner and
//     implemented twice, so the declaration is in the tree three times over.
//  2. A CALLER IN THE SAME FILE IS NOT REACH. The declaring file is excluded from its own evidence.
//  3. A tests/ HELPER IS NOT PRODUCTION. Everything under tests/ is skipped — which is the whole point,
//     because that is exactly where the five historical holes had their callers.
//  4. A SYMBOL WIRED AS A FUNCTION VALUE IS NEVER AN ast.CallExpr — E24 T8's own addition, found when its
//     first draft called a supervised loop unreachable. Mentions are therefore collected from call
//     ARGUMENTS, assignment values and composite-literal elements as well: strictly wider than a call, still
//     narrower than a grep.
//
// THE TARGET LIST IS HAND-WRITTEN AND THAT IS DELIBERATE. Deriving it from `git diff` against a fork point
// would make the gate depend on a ref that stops existing after a squash, and would silently shrink to
// nothing the day the diff came back empty — a guard that cannot fire. A reader checks this list against the
// epic's merge commits; the list itself is the reviewable artifact.

// e26ExportedSurface is every exported function or method E26's merged tasks added, grouped by the task that
// added it. Constructors, error values and types are deliberately absent: a type with no constructor call is
// caught by its constructor's row, and an error value's reachability is its returner's.
var e26ExportedSurface = map[string][]string{
	// T1 — THE OCI POSTURE. StartDetached is the whole epic in the container: create + start with NO deferred
	// remove. If it were unreachable, `background: true` on a sandboxed deployment would be a parameter
	// nothing honours. NewDockerDetachedDriver is its composition root, which is exactly the shape E24 T3
	// shipped unreachable.
	// `NewDockerDetachedDriver` WAS IN THIS GROUP AND IS NOT ANY MORE, because this sweep found nothing
	// called it and it was DELETED rather than filed: it was a byte-for-byte copy of NewDockerDriver's body
	// returning the same concrete type behind a second interface, and production reaches these three methods
	// by type-asserting the driver NewDockerDriver already built. The sixth instance of the shape, and the
	// first one whose honest fix was a deletion.
	"T1 the detached container": {
		"StartDetached", "InspectDetached", "KillDetached",
	},
	// T1 — THE HOST POSTURE. Start/Probe/Kill are also the BackgroundRunner interface's three methods, so
	// this group is the live instance of false-positive shape 1: the names are declared in an interface and
	// implemented twice, and only a real call site counts.
	"T1 the detached process group": {
		"Start", "Probe", "Kill", "Resolve",
	},
	// T2 — THE TOOL SURFACE. BackgroundKillTool is a registration, and a tool nobody registers is a tool the
	// model never sees — which is precisely the grant asymmetry T2 found from the other side (every
	// deployment could START a task and none could STOP one).
	"T2 the tool surface": {
		"BackgroundKillTool", "StartBackground", "KillBackground",
	},
	// T3 — THE RESPONSE STATE MACHINE'S FIRST PRODUCTION WRITER. AdvanceResponse is the single most important
	// row in this table: ResponseTable had NO production caller at all, and a published schema had advertised
	// its states for three years. If this were unreachable the epic's D3 finding would be unfixed while
	// looking fixed.
	"T3 the response middle": {
		"AdvanceResponse",
	},
	// T4 — THE WAKE. The sweep and the observer wiring: a sweep nothing calls is a notification that never
	// lands, which is D14's failure mode arrived at by a different route.
	"T4 the wake": {
		"SweepFinishedBackgroundTasks", "WithBackgroundTasks", "BackgroundObserver",
	},
	// T5 — THE REAPER. SetBackgroundKiller is the ONE setter that wires both halves — T5 collapsed two into
	// one so that a deployment able to START a task and unable to STOP one is unrepresentable — and the
	// counters are the enforcement E24's `runners.capacity` never got.
	"T5 the reaper": {
		"SetBackgroundKiller", "BackgroundKiller", "RecordBackgroundTask", "CountRunningBackgroundTasks",
		"BackgroundTaskForRun", "RunningBackgroundTasksOfRun", "SettleEndedBackgroundTask",
		// `BackgroundRunner()` was here too and is deleted for the same reason, with one difference worth
		// recording: its doc comment claimed it existed for a composition-root test, and E25 T9 found that
		// EXACT sentence on host.Executor.WallTime one epic earlier. Two symbols, one epic apart, both
		// justified by a reader nobody wrote.
		"BackgroundLogRetention", "SetBackgroundRunner",
	},
	// T6 — THE CREDENTIAL. RedactOutput is the ONE function both landing sites go through, and two redaction
	// points are two chances to diverge: if it were unreachable, the log the model reads and the notice the
	// pump carries would both be unmasked while a test proved the function works.
	"T6 the credential": {
		"RedactOutput",
	},
}

// unreachableWithAFiledGap is the exception list, kept as a NAMED map rather than by deleting a target —
// because a target quietly removed is a hole nobody meets again. It is EMPTY, and that is a measurement: every
// symbol above was found to have a production caller on the first run of this sweep. If a later task adds an
// exception here it owes a `BGT-P*` row with the one-line fix, the way E25 T9's `WallTime` owes CON-P9.
var unreachableWithAFiledGap = map[string]string{}

// reachedAsAFunctionValue is the explicit home for false-positive shape 4: a symbol whose production wiring is
// a function VALUE rather than a call, so it appears in no ast.CallExpr. Declaring it here — with the wiring
// named — is what stops a same-named struct FIELD anywhere in the tree from vouching for a dead method.
var reachedAsAFunctionValue = map[string]string{}

// TestEveryE26ExportedSurfaceHasAProductionCaller is the sweep. A symbol with no production reach fails with
// the epic task that added it named, because "which task shipped this unreachable" is the first question a
// reader asks.
func TestEveryE26ExportedSurfaceHasAProductionCaller(t *testing.T) {
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

	tasks := make([]string, 0, len(e26ExportedSurface))
	for task := range e26ExportedSurface {
		tasks = append(tasks, task)
	}
	sort.Strings(tasks)

	for _, task := range tasks {
		for _, symbol := range e26ExportedSurface[task] {
			files := callers[symbol]
			if len(files) == 0 {
				if why, wired := reachedAsAFunctionValue[symbol]; wired {
					if v := values[symbol]; len(v) != 0 {
						t.Logf("%-32s %-30s wired as a VALUE from %s (%s)", task, symbol, strings.Join(v, ", "), why)
						continue
					}
					t.Errorf("%s: %s is declared as wired-by-value (%s) but appears in NO value position either — the declaration is stale", task, symbol, why)
					continue
				}
				if gap, filed := unreachableWithAFiledGap[symbol]; filed {
					t.Logf("%-32s %-30s UNREACHABLE, filed: %s", task, symbol, gap)
					continue
				}
				t.Errorf("%s: %s has NO production reach anywhere outside its own file — that is the shape this repository has shipped FIVE times (CreateSlackConnection, DecideToolApproval, the pool key's composition root, Revoke/Resume, WallTime), and every test in those packages passed. A surface no shipped path constructs is a feature that does not exist. Reach counts a call, a function VALUE handed to something else, and an assignment — so if this really is wired, it is wired somewhere this sweep cannot see and the sweep is what needs fixing", task, symbol)
				continue
			}
			t.Logf("%-32s %-30s reached from %s", task, symbol, strings.Join(files, ", "))
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
}

// --- the component allow-list guard -----------------------------------------------------------------------
//
// `component-tier-run-allowlist`: a component test whose NAME is absent from scripts/test/component's `-run`
// selector NEVER RUNS, and a `-run` that matches nothing exits 0 in silence. This repository has now paid for
// that SIX times. So the guard is here rather than in a reviewer's head.
//
// IT ONLY JUDGES THE FILTERED PACKAGES, and that distinction is the whole correctness of it: most packages in
// scripts/test/component run with NO `-run` at all, so requiring their tests to appear in a selector would be
// a false red. The filtered packages are discovered from the script rather than listed here.

// componentRunFilters extracts, from scripts/test/component, every `go test` invocation and returns the
// packages that are ALWAYS run behind a `-run` selector, mapped to that selector's alternatives.
//
// "ALWAYS" IS THE CORRECTNESS OF THIS GUARD. A package can be invoked TWICE by this script — once unfiltered
// in one tier and once behind a selector in another — and judging it on the filtered invocation alone reports
// its unfiltered tests as never running. So a package with even ONE unfiltered invocation is out of scope
// here, and only a package that is filtered EVERYWHERE can hide a test.
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

// e26ComponentPackages are the packages E26 added component-tagged or background-suite tests to.
var e26ComponentPackages = []string{
	"./apps/control-plane/internal/execution",
	"./adapters/sandboxes/host",
	"./apps/control-plane/cmd/palai-control-plane",
	"./tests/component/background",
}

// TestEveryE26ComponentTestIsNamedInTheRunAllowList walks the test files E26 touched and, for the packages the
// shipped script FILTERS, requires every test function's name to be selected by the shipped selector. Go's
// `-run` is an unanchored regexp over the test name, so an alternative that is a SUBSTRING of the name selects
// it; anything else does not.
func TestEveryE26ComponentTestIsNamedInTheRunAllowList(t *testing.T) {
	root := repoRoot(t)
	filters := componentRunFilters(t, root)

	checked, unfiltered := 0, 0
	for _, pkg := range e26ComponentPackages {
		alts, filtered := filters[pkg]
		if !filtered {
			unfiltered++
			t.Logf("%-50s runs UNFILTERED — every test in it is selected by construction", pkg)
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
			// Only the files this epic added, by name: an unrelated file in the same package is another
			// epic's business and is judged by that epic's gate.
			if !strings.Contains(e.Name(), "background") && !strings.Contains(e.Name(), "detached") && !strings.Contains(e.Name(), "main_test") {
				continue
			}
			body, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatalf("read %s: %v", e.Name(), err)
			}
			// ONLY COMPONENT-TAGGED FILES. An UNTAGGED test rides `make verify` and runs whatever any `-run`
			// selector says, so requiring it to appear in one would be a false red — measured: dropping this
			// check reported fourteen of main_test.go's untagged tests as never running, when in fact every
			// one of them runs on every `make verify`.
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
					t.Errorf("%s (%s/%s) is a test in a package the shipped script runs with a -run FILTER, and no alternative of that selector matches its name — so it NEVER RUNS, and a `-run` matching nothing exits 0 in silence. Add an own alternative to scripts/test/component", name, pkg, e.Name())
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("this guard checked ZERO test names — either the packages moved or the file-name filter stopped matching, and either way it is reporting green over nothing")
	}
	t.Logf("component allow-list: %d test name(s) checked across the filtered packages, %d package(s) run unfiltered", checked, unfiltered)
}

// TestEveryTestNamedInABundleLedgerExistsAndIsSelected is the OTHER half of the same rule, and it is the one
// the ledgers themselves owe. Six groups of the background proof name SHIPPED TESTS; a name that no longer
// resolves is a proof that resolves to nothing, and a name the shipped selector does not select is a proof
// that never runs — which reports exactly the same green.
//
// BGT-002 CARRIED THE FIRST KIND FOR TWO TASKS: it named a test T5 deleted along with the in-memory registry
// it guarded. A ledger is a worse place for that than a case file, because a ledger is what a bundle
// certifies.
func TestEveryTestNamedInABundleLedgerExistsAndIsSelected(t *testing.T) {
	root := repoRoot(t)
	declared := declaredTestNames(t, root)
	filters := componentRunFilters(t, root)

	// Every alternative of every filtered selector, flattened: a ledger's test must be selected by at least
	// one of them OR live in a package that runs unfiltered.
	var alternatives []string
	for _, alts := range filters {
		alternatives = append(alternatives, alts...)
	}

	type named struct{ Group, Test, Pkg string }
	var rows []named

	var semantics []struct {
		Test    string `json:"test"`
		Package string `json:"package"`
	}
	if err := json.Unmarshal([]byte(uat.BackgroundSemanticsLedger), &semantics); err != nil {
		t.Fatalf("decode the semantics ledger: %v", err)
	}
	for _, r := range semantics {
		rows = append(rows, named{"semantics", r.Test, r.Package})
	}
	for _, ledger := range []struct {
		group string
		body  string
	}{
		{"refusal", uat.BackgroundRefusalLedger},
		{"ownership", uat.BackgroundOwnershipLedger},
		{"notice", uat.BackgroundNoticeLedger},
		{"reaper", uat.BackgroundReaperLedger},
	} {
		var generic []struct {
			Test string `json:"test"`
		}
		if err := json.Unmarshal([]byte(ledger.body), &generic); err != nil {
			t.Fatalf("decode the %s ledger: %v", ledger.group, err)
		}
		for _, r := range generic {
			rows = append(rows, named{ledger.group, r.Test, ""})
		}
	}
	if len(rows) < 15 {
		t.Fatalf("only %d ledger row(s) named a test — the decode is reading nothing, which is the vacuous form of this guard", len(rows))
	}

	selected := 0
	for _, row := range rows {
		if !declared[row.Test] {
			t.Errorf("the %s ledger names %q, which is DECLARED NOWHERE in the tree — the bundle certifies a proof that does not exist (BGT-002 carried exactly this for two tasks, naming a test T5 had deleted)", row.Group, row.Test)
			continue
		}
		if slices.ContainsFunc(alternatives, func(alt string) bool { return strings.Contains(row.Test, alt) }) {
			selected++
			continue
		}
		// Not selected by any filtered selector: it must be in a package the script runs UNFILTERED, which is
		// how tests/component/background's legs run. Anything else is a test the tier never runs.
		if row.Pkg == "" || strings.HasPrefix(row.Pkg, "tests/component/") {
			selected++
			continue
		}
		t.Errorf("the %s ledger names %q in %s, and no alternative of any shipped -run selector matches it — a guard this `-run` does not name is a guard this tier never runs, and the bundle would be certifying it anyway", row.Group, row.Test, row.Pkg)
	}
	t.Logf("ledger test names: %d named, %d resolved AND selected", len(rows), selected)
}

// declaredTestNames collects every `func TestXxx(` in the tree, so a ledger's reference is resolved against a
// DECLARATION rather than against a mention.
func declaredTestNames(t *testing.T, root string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	pattern := regexp.MustCompile(`(?m)^func (Test\w+)\(`)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", ".git", "dist":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		for _, m := range pattern.FindAllStringSubmatch(string(body), -1) {
			out[m[1]] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the tree for test declarations: %v", err)
	}
	if len(out) < 500 {
		t.Fatalf("only %d test declarations were found — the walk is not reading the tree", len(out))
	}
	return out
}

// collectProductionCallSites parses every non-test .go file outside tests/ and returns TWO maps: the files
// that CALL each name, and the files that mention it in a VALUE position (a call argument, an assignment
// right-hand side, a composite-literal element).
//
// THE SPLIT IS NOT COSMETIC. Shape 4 — a symbol handed somewhere as a function VALUE — is invisible to a
// call-only collector, so value positions have to be counted. But a value position is ALSO where a plain FIELD
// READ lives, and a field can share a method's name, which is how E25 T9's sweep first reported a dead
// accessor green. One map reports it green; two report it as what it is.
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

// declaringFiles maps each exported name in e26ExportedSurface to the repo-relative file that DECLARES it, so
// the sweep can refuse that file as its own witness.
//
// IT FAILS LOUDLY ON A NAME IT CANNOT FIND, which is the vacuity check earning its place before any target has
// been judged: a misspelled target would otherwise be "unreachable" for the wrong reason, or — worse — a
// renamed one would silently stop being swept.
//
// `Start`, `Probe` and `Kill` are declared THREE times each (an interface and two implementations), and the
// first declaration found wins. That is correct for this sweep's purpose: excluding one declaring file from
// the evidence still leaves the other two, so an implementation nothing calls is still caught by the
// implementation that is called — and the interface declaration is in no call position at all.
func declaringFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	targets := map[string]bool{}
	for _, symbols := range e26ExportedSurface {
		for _, s := range symbols {
			targets[s] = true
		}
	}
	out := map[string]string{}
	for _, dir := range []string{
		filepath.Join("adapters", "sandboxes", "host"),
		filepath.Join("adapters", "sandboxes", "oci"),
		filepath.Join("adapters", "sandboxes", "oci", "workspace"),
		filepath.Join("apps", "control-plane", "internal", "execution"),
		filepath.Join("apps", "control-plane", "internal", "execution", "tools"),
		filepath.Join("packages", "coordinator"),
		filepath.Join("packages", "tool-broker"),
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
		t.Fatalf("these targets are not DECLARED in any of the packages E26 touched: %v — either the name is wrong (so the sweep has been looking for nothing, which is the vacuous form of this guard) or the symbol moved", missing)
	}
	return out
}
