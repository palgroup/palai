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
