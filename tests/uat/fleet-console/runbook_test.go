// The E28 T4 exit gate's RUNBOOK half (plan §T4, §3.6 D2): every `palai …` command a shipped operator runbook
// prints must be one this CLI actually has.
//
// WHY THIS IS A GATE JOB RATHER THAN A REVIEW ONE. §3.6 D2 measured that `phase-24-runner-fleet.md`'s §0.2
// handover block — the place an owner COPY-PASTES from — spelled three commands and TWO of them named
// resources the CLI does not register. T1 pinned that specific block as a test. What it could not pin is the
// general shape: a runbook is prose, prose is not run, and a command that has never been executed reads
// exactly like one that has. E25 T7 paid for the same shape from the other end — a shipped runbook's step (c)
// named a revision id no route returned, and it stood three and a half days and was edited three times while
// wrong.
//
// WHAT THIS CHECKS AND WHAT IT DOES NOT. It resolves the RESOURCE and the SUBCOMMAND against the CLI's own
// dispatch, which is what "this command exists" means; it does not execute anything, so it says nothing about
// whether the flags are right for the deployment reading them. That is a real limit and it is the honest one:
// running these would need a control plane, and the §6 leg that provides one is open.
package fleetconsole

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// runbooksUnderGuard are the operator docs E28 touched or completed. A runbook not listed here is not
// unguarded by accident — the list is the reviewable artifact, the same argument the reachability sweep's
// hand-written target list makes.
var runbooksUnderGuard = []string{
	"docs/operations/runner-fleet.md",
	"docs/operations/console.md",
}

// palaiCommand matches a `palai …` invocation at the start of a line inside a fenced block. Anchoring on the
// line start is what keeps a MENTION in prose ("`palai pool create` fronts it") out of the sample: a mention
// is a reference, and a reference to a command that does not exist is a different (smaller) mistake than a
// copy-paste block that fails.
var palaiCommand = regexp.MustCompile(`(?m)^palai ([a-z-]+)(?: ([a-z-]+))?(?: ([a-z-]+))?`)

// cliVerbs reads the top-level verbs `dispatch` still has, straight out of main.go.
//
// IT PARSED THE `admin <resource> <sub>` FAMILY UNTIL 2026-08-07, in two functions and about eighty
// lines. That family is gone — `/v1` and the panel served the same writes, so the CLI's copies were
// deleted and the runbooks now print curl. What survived the deletion is the property that mattered all
// along and nothing narrower: a runbook must not print a command this binary does not have.
func cliVerbs(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "cmd", "cli", "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	body := string(raw)
	start := strings.Index(body, "func dispatch(args []string) error {")
	if start < 0 {
		t.Fatal("cmd/cli/main.go has no dispatch function — the composition of the CLI moved and this guard " +
			"must follow it rather than reporting every runbook command as absent")
	}
	end := strings.Index(body[start:], "\n}\n")
	if end < 0 {
		t.Fatal("could not find the end of dispatch in cmd/cli/main.go")
	}
	var verbs []string
	for _, m := range regexp.MustCompile(`case "([a-z-]+)"`).FindAllStringSubmatch(body[start:start+end], -1) {
		verbs = append(verbs, m[1])
	}
	// NON-VACUITY: a regexp that stopped matching would let every runbook command through. The floor is
	// well under what dispatch has so an ordinary deletion does not touch this line, and far enough above
	// zero that a broken parse cannot pass.
	if len(verbs) < 5 {
		t.Fatalf("parsed %d verb(s) out of dispatch, want at least 5 — the switch shape moved and this guard "+
			"is now vacuous", len(verbs))
	}
	slices.Sort(verbs)
	return verbs
}

// TestEveryPalaiCommandInAnOperatorRunbookExists is §3.6 D2's general form, and it is the closure the plan
// asked for: "commands that WORK".
func TestEveryPalaiCommandInAnOperatorRunbookExists(t *testing.T) {
	root := repoRoot(t)
	verbs := cliVerbs(t)

	checked := 0
	for _, doc := range runbooksUnderGuard {
		raw, err := os.ReadFile(filepath.Join(root, doc))
		if err != nil {
			t.Fatalf("read %s: %v", doc, err)
		}
		for _, m := range palaiCommand.FindAllStringSubmatch(string(raw), -1) {
			verb := m[1]
			checked++
			if !slices.Contains(verbs, verb) {
				t.Errorf("%s prints `palai %s …` and this binary dispatches only %v — an operator following "+
					"this page types a command that does not exist", doc, verb, verbs)
			}
		}
	}
	if checked == 0 {
		t.Fatal("this guard resolved ZERO commands — either the runbooks stopped printing them or the regexp " +
			"stopped matching, and both are the vacuity it exists to avoid")
	}
	t.Logf("runbook commands: %d `palai …` invocation(s) resolved across %d operator doc(s)", checked, len(runbooksUnderGuard))
}

// TestTheRunbookCommandGuardCanActuallyFail is the guard for the guard: a parse that produced nothing
// would let every runbook command through while reporting success.
//
// IT USED TO CONTROL ON THE `admin <resource> <sub>` FAMILY, and that family was deleted on 2026-08-07
// when /v1 and the panel were left as the single client. The control moved with the parse: what has to
// be shown now is that cliVerbs finds the verbs this binary really dispatches, and that a verb it does
// NOT dispatch is reported rather than passed.
func TestTheRunbookCommandGuardCanActuallyFail(t *testing.T) {
	verbs := cliVerbs(t)

	// The positive half FIRST, so the negative below cannot be true of an empty parse. These three are
	// the verbs with no /v1 route at all, which is exactly why they survived the deletion — a member that
	// outlives the change it anchors is the property an anchor needs.
	for _, member := range []string{"backup", "restore", "doctor"} {
		if !slices.Contains(verbs, member) {
			t.Fatalf("%q is missing from the parsed verb set %v — the parse produced nothing usable, so the "+
				"negative below would pass for the wrong reason", member, verbs)
		}
	}
	// And the negative: a verb that left the CLI must NOT resolve, or a runbook could keep printing it.
	for _, gone := range []string{"admin", "apikey", "provider", "config"} {
		if slices.Contains(verbs, gone) {
			t.Errorf("%q is still in the parsed verb set %v — it was deleted, and a guard that still resolves "+
				"it would let a runbook print a command this binary does not have", gone, verbs)
		}
	}
}
