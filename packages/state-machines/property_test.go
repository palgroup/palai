package statemachines

import (
	"errors"
	"math/rand"
	"testing"
)

// propertySeed fixes the PRNG so any property failure reproduces exactly; the
// suite never depends on wall-clock time.
const propertySeed int64 = 0x5EED

// stringRow is a transition projected to strings so all seven typed tables can be
// exercised through one facade.
type stringRow struct {
	from, command, to, event string
}

// tableSpec is one lifecycle table collapsed to strings for property testing.
type tableSpec struct {
	name     string
	initial  string
	commands []string
	rows     []stringRow
	apply    func(state, command string) (string, string, error)
	terminal map[string]bool
}

// stringifyRows projects a typed table's rows to strings.
func stringifyRows[S ~string, C ~string](table []Transition[S, C]) []stringRow {
	out := make([]stringRow, len(table))
	for i, tr := range table {
		out[i] = stringRow{string(tr.From), string(tr.Command), string(tr.To), tr.Event}
	}
	return out
}

// distinctCommands lists each command in the table once, in first-seen order,
// giving a random walk its full command alphabet.
func distinctCommands[S ~string, C ~string](table []Transition[S, C]) []string {
	seen := map[string]bool{}
	var out []string
	for _, tr := range table {
		c := string(tr.Command)
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}

// stringApply wraps the typed Apply so a caller drives the table with plain
// strings; an unknown string simply matches no row and yields ErrInvalidState.
func stringApply[S ~string, C ~string](table []Transition[S, C]) func(string, string) (string, string, error) {
	return func(state, command string) (string, string, error) {
		to, event, err := Apply(S(state), C(command), table)
		return string(to), event, err
	}
}

// newSpec builds a tableSpec from a typed table and its declared terminal set.
func newSpec[S ~string, C ~string](name, initial string, table []Transition[S, C], terminal map[string]bool) tableSpec {
	return tableSpec{
		name:     name,
		initial:  initial,
		commands: distinctCommands(table),
		rows:     stringifyRows(table),
		apply:    stringApply(table),
		terminal: terminal,
	}
}

// allTables collects the seven lifecycle tables behind the string facade. The
// terminal set is declared independently of the rows so a table gaining or losing
// a terminal is caught rather than silently accepted.
func allTables() []tableSpec {
	return []tableSpec{
		newSpec("run", string(RunQueued), RunTable, map[string]bool{
			string(RunCompleted): true, string(RunFailed): true, string(RunCanceled): true,
			string(RunTimedOut): true, string(RunBudgetExceeded): true,
		}),
		newSpec("attempt", string(AttemptAssigned), AttemptTable, map[string]bool{
			string(AttemptSucceeded): true, string(AttemptFailed): true,
			string(AttemptLost): true, string(AttemptPreempted): true,
		}),
		newSpec("response", string(ResponseQueued), ResponseTable, map[string]bool{
			string(ResponseCompleted): true, string(ResponseFailed): true, string(ResponseCanceled): true,
			string(ResponseTimedOut): true, string(ResponseBudgetExceeded): true,
		}),
		newSpec("session", string(SessionActive), SessionTable, map[string]bool{
			string(SessionDeleted): true,
		}),
		newSpec("command", string(CommandQueued), CommandTable, map[string]bool{
			string(CommandApplied): true, string(CommandRejected): true, string(CommandExpired): true,
		}),
		newSpec("tool_call", string(ToolCallProposed), ToolCallTable, map[string]bool{
			string(ToolCallCompleted): true, string(ToolCallFailed): true, string(ToolCallCanceled): true,
			string(ToolCallReconciledCompleted): true, string(ToolCallReconciledNotApplied): true,
			string(ToolCallManualResolution): true,
		}),
		newSpec("workspace", string(WorkspaceRequested), WorkspaceTable, map[string]bool{
			string(WorkspaceDestroyed): true,
		}),
		newSpec("trigger_delivery", string(TriggerDeliveryReceived), TriggerDeliveryTable, map[string]bool{
			string(TriggerDeliveryRunCreated): true, string(TriggerDeliveryRejected): true,
			string(TriggerDeliveryDuplicate): true, string(TriggerDeliveryFailed): true,
			string(TriggerDeliverySkipped): true,
		}),
	}
}

// terminalsFromRows recomputes terminals structurally: a state that is some row's
// destination but never any row's source.
func terminalsFromRows(rows []stringRow) map[string]bool {
	source := map[string]bool{}
	for _, r := range rows {
		source[r.from] = true
	}
	out := map[string]bool{}
	for _, r := range rows {
		if !source[r.to] {
			out[r.to] = true
		}
	}
	return out
}

// sameSet reports whether two true-valued sets hold identical keys.
func sameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func TestEveryTransitionPairIsUniqueAndEmitsExactlyOneEvent(t *testing.T) {
	for _, spec := range allTables() {
		seen := map[string]bool{}
		for _, r := range spec.rows {
			key := r.from + "\x00" + r.command
			if seen[key] {
				t.Errorf("%s: duplicate transition from %q via %q", spec.name, r.from, r.command)
			}
			seen[key] = true
			if r.event == "" {
				t.Errorf("%s: transition from %q via %q emits no event", spec.name, r.from, r.command)
			}
		}
	}
}

func TestTerminalMonotonicityUnderRandomCommandSequences(t *testing.T) {
	specs := allTables()

	// Declared terminals must match the table's structure, and every terminal
	// must reject every command (no outgoing rows).
	for _, spec := range specs {
		if got := terminalsFromRows(spec.rows); !sameSet(got, spec.terminal) {
			t.Fatalf("%s: structural terminals %v != declared %v", spec.name, got, spec.terminal)
		}
		for term := range spec.terminal {
			for _, cmd := range spec.commands {
				if _, _, err := spec.apply(term, cmd); !errors.Is(err, ErrInvalidState) {
					t.Errorf("%s: terminal %q accepted command %q", spec.name, term, cmd)
				}
			}
		}
	}

	// Random walks: once a walk enters a terminal, every later command is rejected
	// with ErrInvalidState and the state never moves again.
	rng := rand.New(rand.NewSource(propertySeed))
	const walks, steps = 10000, 50
	for i := 0; i < walks; i++ {
		spec := specs[rng.Intn(len(specs))]
		state := spec.initial
		for s := 0; s < steps; s++ {
			cmd := spec.commands[rng.Intn(len(spec.commands))]
			next, event, err := spec.apply(state, cmd)
			if spec.terminal[state] {
				if !errors.Is(err, ErrInvalidState) {
					t.Fatalf("%s: terminal %q accepted %q (walk %d step %d)", spec.name, state, cmd, i, s)
				}
				continue
			}
			if err == nil {
				if event == "" {
					t.Fatalf("%s: %q from %q produced no event", spec.name, cmd, state)
				}
				state = next
			}
		}
	}
}

func TestOneActiveFenceUnderRandomInterleavings(t *testing.T) {
	rng := rand.New(rand.NewSource(propertySeed))
	const rounds = 10000
	var current uint64
	var accepted []uint64
	for i := 0; i < rounds; i++ {
		offer := uint64(rng.Intn(64))
		err := AcceptFence(current, offer)
		switch {
		case offer > current:
			if err != nil {
				t.Fatalf("offer %d over current %d rejected: %v", offer, current, err)
			}
			accepted = append(accepted, offer)
			current = offer
		default:
			if !errors.Is(err, ErrStaleFence) {
				t.Fatalf("stale offer %d (current %d) not rejected: %v", offer, current, err)
			}
		}
	}
	// The accepted stream is exactly the strictly-increasing subsequence: sorted
	// and free of repeats.
	for i := 1; i < len(accepted); i++ {
		if accepted[i] <= accepted[i-1] {
			t.Fatalf("accepted fences not strictly increasing: %d then %d", accepted[i-1], accepted[i])
		}
	}
}

func TestSequenceGuardIsStrictlyMonotonic(t *testing.T) {
	rng := rand.New(rand.NewSource(propertySeed))
	const rounds = 10000
	var prev int64
	var accepted []int64
	for i := 0; i < rounds; i++ {
		next := int64(rng.Intn(64))
		err := NextSequence(prev, next)
		switch {
		case next > prev:
			if err != nil {
				t.Fatalf("next %d over prev %d rejected: %v", next, prev, err)
			}
			accepted = append(accepted, next)
			prev = next
		default:
			if !errors.Is(err, ErrNonMonotonicSequence) {
				t.Fatalf("non-monotonic next %d (prev %d) not rejected: %v", next, prev, err)
			}
		}
	}
	for i := 1; i < len(accepted); i++ {
		if accepted[i] <= accepted[i-1] {
			t.Fatalf("accepted sequence not strictly increasing: %d then %d", accepted[i-1], accepted[i])
		}
	}
}
