package execution

// THE CANDIDATE QUERY HAND-WRITES A LIST OF TERMINAL RUN STATES, and a hand-written copy of a set that
// lives somewhere else is a copy that drifts. The drift here is silent in the direction that matters: add
// a tenth run state, make it terminal, forget this list, and every workspace whose run ends THAT way stays
// `leased` forever — which is precisely the defect AbandonedWriterLeases exists to end, reintroduced for
// one state instead of all of them. Nothing would go red, because a sweep that finds no candidates and a
// sweep that has no candidates report the same number.
//
// So the list is recomputed here from the RUN TABLE ITSELF rather than compared against a second literal.
// statemachines.TerminalStates derives terminality structurally — a state that appears as a destination
// and never as a source — so this test has no copy of the answer to get wrong either.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	statemachines "github.com/palgroup/palai/packages/state-machines"
)

// abandonedLeaseQueryPath is read from the SHIPPED query file, not from a string in this package: the
// bytes the server executes are the bytes under test.
const abandonedLeaseQueryPath = "../../../../storage/queries/workspaces.sql"

var terminalListRe = regexp.MustCompile(`r\.state IN \(([^)]*)\)`)

func TestAbandonedLeaseTerminalsMatchTheRunTable(t *testing.T) {
	raw, err := os.ReadFile(abandonedLeaseQueryPath)
	if err != nil {
		t.Fatalf("read %s: %v", abandonedLeaseQueryPath, err)
	}
	// Scope the read to the one query, so another query's list can never satisfy this.
	_, after, found := strings.Cut(string(raw), "-- name: AbandonedWriterLeases")
	if !found {
		t.Fatal("AbandonedWriterLeases is not in workspaces.sql; the reclaim sweep has no candidate query")
	}
	if next := strings.Index(after, "\n-- name: "); next >= 0 {
		after = after[:next]
	}

	m := terminalListRe.FindStringSubmatch(after)
	if m == nil {
		t.Fatal("AbandonedWriterLeases carries no `r.state IN (...)` list, so it does not gate on terminality at all")
	}
	var got []string
	for _, part := range strings.Split(m[1], ",") {
		if s := strings.Trim(strings.TrimSpace(part), "'"); s != "" {
			got = append(got, s)
		}
	}
	sort.Strings(got)

	var want []string
	for state, terminal := range statemachines.TerminalStates(statemachines.RunTable) {
		if terminal {
			want = append(want, string(state))
		}
	}
	sort.Strings(want)

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("AbandonedWriterLeases gates on %v, but the run table's terminal states are %v.\n"+
			"A terminal state missing from the query is a class of workspace that stays `leased` forever "+
			"and never reaches the idle sweep; an extra one names a state a run can still leave, whose "+
			"lease must not be taken.", got, want)
	}
}

// THE ORDER OF reclaim's TWO WRITES IS A ONE-WAY DOOR, AND IT IS UNOBSERVABLE FROM ITS RESULT. Uninterrupted,
// releasing the lease first and moving the state first end in exactly the same place — which is why the
// behavioural sweep tests stay green under a reversal, and why they are not the wrong tests: the property
// they assert genuinely still holds. What differs is only what a crash BETWEEN the two statements leaves:
//
//	lease first (shipped)   interrupted -> still `leased` with an active lease. Unchanged from the start,
//	                        and this sweep's own candidate query finds it again on the next tick.
//	state first (reversed)  interrupted -> `ready` WITH an active lease. IdleWorkspacesForRelease requires
//	                        no active lease, so it skips it; this sweep requires `leased`, so it skips it
//	                        too. The workspace is invisible to both, forever.
//
// So the guard reads the SHAPE, for the same reason workspace_release_defer_test.go does: there is no
// behaviour to observe, because the difference only exists in a window a test cannot open without a seam
// invented solely to open it.
func TestReclaimReleasesTheLeaseBeforeItMovesTheState(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "lease_reclaim.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse lease_reclaim.go: %v", err)
	}

	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "reclaim" && fn.Body != nil {
			body = fn.Body
		}
	}
	if body == nil {
		t.Fatal("reclaim not found in lease_reclaim.go — this guard fails rather than passing over an empty walk")
	}

	releaseAt, advanceAt := -1, -1
	ast.Inspect(body, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "ReleaseWriterLease":
			if releaseAt < 0 {
				releaseAt = fset.Position(sel.Pos()).Line
			}
		case "AdvanceWorkspace":
			if advanceAt < 0 {
				advanceAt = fset.Position(sel.Pos()).Line
			}
		}
		return true
	})

	if releaseAt < 0 || advanceAt < 0 {
		t.Fatalf("reclaim calls ReleaseWriterLease at line %d and AdvanceWorkspace at line %d; it must call BOTH, or a reclaimed workspace is left half-returned", releaseAt, advanceAt)
	}
	if releaseAt > advanceAt {
		t.Fatalf("reclaim moves the workspace state (line %d) BEFORE it releases the lease (line %d). A crash between them leaves `ready` with an active lease, which the idle sweep skips (it requires no active lease) and this sweep skips too (it requires `leased`) — stuck for good. Release the lease first.", advanceAt, releaseAt)
	}
}
