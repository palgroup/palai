package adminconsole

import (
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

// THE JOB A GATE EXISTS FOR: every exported function and method E25 added must be REACHABLE FROM THE SHIPPED
// BINARY. This repository has shipped "fully built, fully tested, reachable from nothing" FOUR TIMES —
// CreateSlackConnection (found by E19 T9), DecideToolApproval (found by E23), the runner pool key's own
// composition root (found by E24 T3), and Revoke/Resume (found by E24 T5). Every test in every one of those
// packages passed. A fail-closed security surface with no production caller is a feature that does not exist,
// and behaviour tests cannot see it: they construct the thing themselves.
//
// FOUR FALSE-POSITIVE SHAPES ARE EXCLUDED BY CONSTRUCTION, and each is a real mistake somebody would make:
//
//  1. AN INTERFACE METHOD DECLARATION IS NOT A CALL. A name that appears only inside an InterfaceType's field
//     list is in no call and no assignment position, so it never registers.
//  2. A CALLER IN THE SAME FILE IS NOT REACH. The declaring file is excluded from its own evidence.
//  3. A tests/ HELPER IS NOT PRODUCTION. Everything under tests/ is skipped — which is the whole point,
//     because that is exactly where the four historical holes had their callers.
//  4. A SYMBOL WIRED AS A FUNCTION VALUE IS NEVER AN ast.CallExpr — E24 T8's own addition, found when its
//     first draft called a supervised loop unreachable. Mentions are therefore collected from call
//     ARGUMENTS, assignment values and composite-literal elements as well: strictly wider than a call, still
//     narrower than a grep.
//
// THE TARGET LIST IS HAND-WRITTEN AND THAT IS DELIBERATE. Deriving it from `git diff` against a fork point
// would make the gate depend on a ref that stops existing after a squash, and would silently shrink to
// nothing the day the diff came back empty — a guard that cannot fire. A reader checks this list against the
// epic's merge commits; the list itself is the reviewable artifact.

// e25ExportedSurface is every exported function or method E25's merged tasks added, grouped by the task that
// added it. Constructors, error values and types are deliberately absent: a type with no constructor call is
// caught by its constructor's row, and an error value's reachability is its returner's.
var e25ExportedSurface = map[string][]string{
	// T3 — THE PIPE, and this is the group the sweep exists for. `LayerEnv` is the ONE function both sandbox
	// adaptors call, so the layering rule cannot hold in one posture only; if it were unreachable the
	// environment would be a table nothing reads. `RunEnvironmentKeys` and `SetEnvironmentSecrets` are the
	// orchestrator's two halves of the claim (the key NAMES come from the run's pinned revision; the VALUES
	// are resolved one statement before Execute and dropped).
	"T3 the environment pipe": {
		"LayerEnv", "RedactValues", "ValidEnvKey", "RunEnvironmentKeys", "SetEnvironmentSecrets",
	},
	// T3 — the store and the routes. A write path with no route is a screen nobody can reach, and a route
	// with no store method is a 404 with a handler.
	"T3 the environment store and routes": {
		"CreateEnvironment", "ListEnvironments", "GetEnvironment",
		"PutEnvironmentValue", "DeleteEnvironmentValue", "WithEnvironments",
	},
	// T7 + the tool-registry contract fix — the three read routes whose absence made a SHIPPED runbook
	// unexecutable. `ListToolRevisions` is the one that did not exist at all; `GetToolRevisionOfTool` is the
	// contract fix's twin (a Location header that named an unmounted prefix); `GetToolSetRevision` is the set's
	// PINS, the field the list projection has never carried.
	"T7 the tool revision reads": {
		"ListToolRevisions", "GetToolRevisionOfTool", "GetToolSetRevision",
	},
	// The shell tool's wall time, unset in every shipped file. Both halves are targets because the defect was
	// exactly that one of them was computed and never consulted.
	"the shell wall time": {
		"WallTime",
	},
}

// unreachableWithAFiledGap is the exception list, kept as a NAMED map rather than by deleting a target —
// because a target quietly removed is a hole nobody meets again. It is EMPTY, and that is a measurement: every
// symbol above was found to have a production caller on the first run of this sweep. If a later task adds an
// exception here it owes a `CON-P*` row with the one-line fix, the way E24 T8's `Waiting` owes FLT-P14.
var unreachableWithAFiledGap = map[string]string{
	"WallTime": "CON-P9 — `host.Executor.WallTime()` reports the bound the executor was built with, and its own doc comment says it exists \"so a composition root can be asked what it actually wired\". NOTHING asks: not `palai local doctor`, not `palai up`'s report, not /healthz. That matters more than an unread accessor usually would, because ZERO IS NOT A NEUTRAL VALUE HERE — it means UNBOUNDED, and an unbounded host executor is indistinguishable from a bounded one until a command hangs, which is the exact defect the wall-time fix landed for. Fix: one line in the doctor's shell-posture check reading it back. It is FILED rather than fixed here because adding a diagnostic line to a shipped operator report is a surface decision, not an exit-gate edit",
}

// reachedAsAFunctionValue is the explicit home for false-positive shape 4: a symbol whose production wiring is
// a function VALUE rather than a call, so it appears in no ast.CallExpr. Declaring it here — with the wiring
// named — is what stops a same-named struct FIELD anywhere in the tree from vouching for a dead method, which
// is precisely what happened to `WallTime` on this sweep's first run. It is EMPTY today; E25 wired nothing by
// value, and an entry added later owes the call site that consumes it.
var reachedAsAFunctionValue = map[string]string{}

// knownNamesakeCallers is false-positive shape 5, found by 607cf84f: a DIFFERENT symbol sharing a target's
// bare name. `collectProductionCallSites` matches a `*ast.SelectorExpr` by its LAST identifier only — it has
// no type information, so `x.WallTime()` registers as reach for the target "WallTime" whatever `x` is. Before
// 607cf84f the only other `WallTime` in the tree was the FIELD shape 4's comment already names
// (oci.Limits.WallTime, a value position, kept out of `byCall`). 607cf84f added a THIRD, call-position one:
// `posture.WallTime()`, a package-level FUNCTION that reads the CONFIGURED bound from the environment — not
// `(*host.Executor).WallTime()`, the METHOD CON-P9 is about, which reads back what an ALREADY-BUILT executor
// was actually wired with. The two do different jobs (one computes an input, the other reports an output) and
// share nothing but a name.
//
// Each entry is HAND-VERIFIED rather than pattern-matched, the same discipline `declaringFiles` uses: grep for
// `.WallTime()` in each listed file finds exactly one hit, and it is `posture.WallTime()` (main.go, qualified)
// or the bare `WallTime()` self-call inside the posture package's own body (posture.go) — neither file
// constructs or holds a *host.Executor to call the real target on. If either file EVER gains a genuine
// host.Executor.WallTime() call, that call sits beside an unrelated one this map hides, so an entry here is
// dated at the commit that added it and re-verified by hand rather than carried forward blind.
var knownNamesakeCallers = map[string][]string{
	"WallTime": {
		// 607cf84f (2026-08-04): posture.WallTime()'s own declaration and its one production caller.
		"adapters/sandboxes/posture/posture.go",
		"apps/control-plane/cmd/palai-control-plane/main.go",
	},
}

// withoutKnownNamesakes returns files with the known-namesake call sites removed for symbol, so a target's
// reach is judged on evidence the AST can actually attribute to it.
func withoutKnownNamesakes(symbol string, files []string) []string {
	namesakes := knownNamesakeCallers[symbol]
	if len(namesakes) == 0 {
		return files
	}
	out := make([]string, 0, len(files))
	for _, f := range files {
		if !slices.Contains(namesakes, f) {
			out = append(out, f)
		}
	}
	return out
}

// TestEveryE25ExportedSurfaceHasAProductionCaller is the sweep. A symbol with no production reach fails with
// the epic task that added it named, because "which task shipped this unreachable" is the first question a
// reader asks.
func TestEveryE25ExportedSurfaceHasAProductionCaller(t *testing.T) {
	root := repoRoot(t)
	callers, values := collectProductionCallSites(t, root)
	// Shape 5 (see knownNamesakeCallers): subtract call sites the AST can attribute to a DIFFERENT symbol of
	// the same bare name before either judgement below reads `callers`, so both the staleness check and the
	// per-target loop see the same, correctly-attributed evidence.
	for symbol := range knownNamesakeCallers {
		callers[symbol] = withoutKnownNamesakes(symbol, callers[symbol])
	}

	// The exception list is checked FIRST and in the direction that can go stale: a symbol listed here that
	// HAS gained reach must be removed, or the list becomes a place where a fixed gap goes on being reported
	// as open — which is worse than no list, because the next reader trusts it.
	for symbol, gap := range unreachableWithAFiledGap {
		if files := callers[symbol]; len(files) != 0 {
			t.Errorf("%s is listed as unreachable with a filed gap (%s) but IS now reached from %v — delete the exception and close the gap row, or this list starts lying about what is open", symbol, gap, files)
		}
	}

	tasks := make([]string, 0, len(e25ExportedSurface))
	for task := range e25ExportedSurface {
		tasks = append(tasks, task)
	}
	sort.Strings(tasks)

	for _, task := range tasks {
		for _, symbol := range e25ExportedSurface[task] {
			files := callers[symbol]
			if len(files) == 0 {
				if why, wired := reachedAsAFunctionValue[symbol]; wired {
					if v := values[symbol]; len(v) != 0 {
						t.Logf("%-36s %-24s wired as a VALUE from %s (%s)", task, symbol, strings.Join(v, ", "), why)
						continue
					}
					t.Errorf("%s: %s is declared as wired-by-value (%s) but appears in NO value position either — the declaration is stale", task, symbol, why)
					continue
				}
				if gap, filed := unreachableWithAFiledGap[symbol]; filed {
					t.Logf("%-36s %-24s UNREACHABLE, filed: %s", task, symbol, gap)
					if v := values[symbol]; len(v) != 0 {
						t.Logf("%-36s %-24s   (mentioned in value position from %s — a FIELD of the same name, not this symbol)", task, symbol, strings.Join(v, ", "))
					}
					continue
				}
				t.Errorf("%s: %s has NO production reach anywhere outside its own file — that is the shape this repository has shipped FOUR times (CreateSlackConnection, DecideToolApproval, the pool key's composition root, Revoke/Resume), and every test in those packages passed. A surface no shipped path constructs is a feature that does not exist. Reach counts a call, a function VALUE handed to something else, and an assignment — so if this really is wired, it is wired somewhere this sweep cannot see and the sweep is what needs fixing", task, symbol)
				continue
			}
			t.Logf("%-36s %-24s reached from %s", task, symbol, strings.Join(files, ", "))
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
					t.Errorf("the collector counted %s as a production site for %s — the four historical holes all had their callers in tests", file, name)
				}
			}
		}
	}
	// AND THE SPLIT ITSELF, asserted: the two maps must not be the same map. If value positions ever stopped
	// being collected separately, a field read would vouch for a method again and the WallTime finding below
	// would silently revert to green.
	if len(values) == 0 {
		t.Fatal("the value-position map is empty — shape 4 (a symbol handed somewhere as a function value) is no longer collected at all")
	}
	if files := values["WallTime"]; len(files) == 0 {
		t.Error("`WallTime` is no longer seen in any value position — the field/method collision this split exists to separate has moved, and the exception below needs re-measuring rather than trusting")
	}
}

// --- the component allow-list guard -----------------------------------------------------------------------
//
// `component-tier-run-allowlist`: a component test whose NAME is absent from scripts/test/component's `-run`
// selector NEVER RUNS, and a `-run` that matches nothing exits 0 in silence. This repository has now paid for
// that SIX times — the tool-registry contract fix's own commit says "the fifth and sixth time this list has
// needed an own alternative". So the guard is here rather than in a reviewer's head.
//
// IT ONLY JUDGES THE FILTERED PACKAGES, and that distinction is the whole correctness of it: most packages in
// scripts/test/component run with NO `-run` at all, so requiring their tests to appear in a selector would be
// a false red. The filtered packages are discovered from the script rather than listed here.

// componentRunFilters extracts, from scripts/test/component, every `go test` invocation and returns the
// packages that are ALWAYS run behind a `-run` selector, mapped to that selector's alternatives.
//
// "ALWAYS" IS THE CORRECTNESS OF THIS GUARD AND ITS FIRST DRAFT GOT IT WRONG. A package can be invoked TWICE
// by this script: `./apps/control-plane/internal/automation` runs UNFILTERED in the postgres tier and again
// with `-run 'TestQueueAdapter|TestQueueOutbox'` in the queues tier. Judging it on the filtered invocation
// alone reported forty-odd of its tests as never running, when in fact every one of them runs in the tier
// that has no selector. So a package with even ONE unfiltered invocation is out of scope here, and only a
// package that is filtered EVERYWHERE can hide a test.
func componentRunFilters(t *testing.T, root string) map[string][]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "scripts", "test", "component"))
	if err != nil {
		t.Fatalf("read scripts/test/component: %v", err)
	}
	// One invocation may span several lines (`-run '…' \` then the package). Normalise continuations first.
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

// TestEveryE25ComponentTestIsNamedInTheRunAllowList walks the component-tagged test files E25 touched and, for
// the packages the shipped script FILTERS, requires every test function's name to be selected by the shipped
// selector. Go's `-run` is an unanchored regexp over the test name, so an alternative that is a PREFIX of the
// name selects it; anything else does not.
func TestEveryE25ComponentTestIsNamedInTheRunAllowList(t *testing.T) {
	root := repoRoot(t)
	filters := componentRunFilters(t, root)

	// The packages E25 added component-tagged tests to. Two of them (identity, automation) are run UNFILTERED
	// by the shipped script and are listed here so the guard reports that fact rather than silently skipping
	// them — a package that leaves the filtered set is exactly the drift this checks for.
	packages := []string{
		"./apps/control-plane/internal/execution",
		"./apps/control-plane/internal/execution/tools",
		"./apps/control-plane/internal/identity",
		"./apps/control-plane/internal/automation",
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
			// Only component-tagged files: an untagged test rides `make verify` and needs no allow-list entry.
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

// collectProductionCallSites parses every non-test .go file outside tests/ and returns TWO maps: the files
// that CALL each name, and the files that mention it in a VALUE position (a call argument, an assignment
// right-hand side, a composite-literal element).
//
// THE SPLIT IS NOT COSMETIC AND THIS SWEEP EARNED IT ON ITS FIRST RUN. Shape 4 — a symbol handed somewhere as
// a function VALUE — is invisible to a call-only collector, so value positions have to be counted. But a
// value position is ALSO where a plain FIELD READ lives, and a field can share a method's name: `WallTime` is
// a method on adapters/sandboxes/host.Executor AND a field on adapters/sandboxes/oci.Limits, so
// `spec.Limits.WallTime` registered as reach for a method nothing calls. One map reported the accessor green;
// two report it as what it is. A target reached only in value positions must therefore be declared in
// reachedAsAFunctionValue with the wiring named, or a same-named field anywhere in the tree can vouch for a
// dead symbol.
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
	// The declaring file is excluded from its own evidence, so a method called only by its own file's other
	// methods still counts as unreachable.
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

// declaringFiles maps each exported name in e25ExportedSurface to the repo-relative file that DECLARES it, so
// the sweep can refuse that file as its own witness.
//
// IT FAILS LOUDLY ON A NAME IT CANNOT FIND, which is the vacuity check earning its place before any target has
// been judged: a misspelled target would otherwise be "unreachable" for the wrong reason, or — worse — a
// renamed one would silently stop being swept.
func declaringFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	targets := map[string]bool{}
	for _, symbols := range e25ExportedSurface {
		for _, s := range symbols {
			targets[s] = true
		}
	}
	out := map[string]string{}
	for _, dir := range []string{
		filepath.Join("apps", "control-plane", "api"),
		filepath.Join("apps", "control-plane", "internal", "identity"),
		filepath.Join("apps", "control-plane", "internal", "execution"),
		filepath.Join("apps", "control-plane", "internal", "store"),
		filepath.Join("apps", "control-plane", "internal", "extensions"),
		filepath.Join("packages", "tool-broker"),
		// RunEnvironmentKeys is the COORDINATOR's, not the identity store's — the run row it reads is the
		// coordinator's, and the sweep found that by refusing to look for a name it could not find a
		// declaration for. WallTime is the host sandbox EXECUTOR's; the wall-time defect was exactly that one
		// half was computed and the other never consulted it, so both live where the process is started.
		filepath.Join("packages", "coordinator"),
		filepath.Join("adapters", "sandboxes", "host"),
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
		t.Fatalf("these targets are not DECLARED in any of the packages E25 touched: %v — either the name is wrong (so the sweep has been looking for nothing, which is the vacuous form of this guard) or the symbol moved", missing)
	}
	return out
}
