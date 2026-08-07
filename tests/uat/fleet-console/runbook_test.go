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

// cliResources is the resource set `cmd/cli/main.go` dispatches DIRECTLY (`palai <resource> …`), read from
// the source rather than retyped — because a retyped copy is the thing this file exists to refuse.
//
// THERE IS NO SECOND LIST FOR `palai admin <resource>`, AND MEASURING THAT IS THE POINT. main.go's `admin`
// case hands `args[1]` straight to `admin.Run`, so the resources reachable that way are exactly the ones
// admin.Run's own switch knows — which is why the second return here is derived from `cliSubcommands` rather
// than parsed a second time. A guard that maintained its own copy of that set would be asserting against
// itself, and `palai admin runner …` is reachable for the same reason `palai admin pool …` is NOT: the first
// is a resource admin.Run dispatches and the second is not.
func cliResources(t *testing.T) (direct, admin []string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "cmd", "cli", "main.go"))
	if err != nil {
		t.Fatalf("read cmd/cli/main.go: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, `case "`) {
			continue
		}
		names := regexp.MustCompile(`"([a-z-]+)"`).FindAllStringSubmatch(trimmed, -1)
		set := make([]string, 0, len(names))
		for _, n := range names {
			set = append(set, n[1])
		}
		// TWO ANCHORS, AND THEY MOVED ON 2026-08-07 WHEN `project` LEFT THE CLI. The line is identified by
		// members rather than by position, so a reordered switch still resolves; what it cannot survive is
		// an anchor that is DELETED, which is what `project` became. `poolkey` and `model` are the two now,
		// and neither appears on any other `case "` line in main.go — measured, not assumed. `poolkey` was
		// the first anchor and it lasted one slice: T1 deletes it too. `pool` and `model` survive T1 whole,
		// which is the property an anchor needs — a member that outlives the change it is anchoring.
		if slices.Contains(set, "pool") && slices.Contains(set, "model") {
			direct = set
		}
	}
	if len(direct) == 0 {
		t.Fatal("could not read the CLI's direct resource dispatch out of cmd/cli/main.go — the switch moved, and a guard that parses nothing accepts everything")
	}
	for resource := range cliSubcommands(t) {
		admin = append(admin, resource)
	}
	slices.Sort(admin)
	return direct, admin
}

// cliSubcommands returns, per resource, the subcommands `admin.Run` dispatches — read from the source for the
// same reason.
func cliSubcommands(t *testing.T) map[string][]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "cmd", "cli", "internal", "admin", "admin.go"))
	if err != nil {
		t.Fatalf("read admin.go: %v", err)
	}
	out := map[string][]string{}
	current := ""
	inSwitch := false
	for _, line := range strings.Split(string(raw), "\n") {
		// The dispatch switch is the one whose cases are followed by `switch sub {`.
		if m := regexp.MustCompile(`^\tcase "([a-z-]+)":$`).FindStringSubmatch(line); m != nil {
			current, inSwitch = m[1], false
			continue
		}
		if strings.TrimSpace(line) == "switch sub {" {
			inSwitch = true
			continue
		}
		if !inSwitch || current == "" {
			continue
		}
		// EVERY name on the case line, not the first: `case "cordon", "resume", "revoke", "approve":` is one
		// line carrying four subcommands, and a first-match parse would report three of them as absent —
		// which is exactly what this guard's own first run did.
		if strings.HasPrefix(line, "\t\tcase \"") {
			for _, m := range regexp.MustCompile(`"([a-z-]+)"`).FindAllStringSubmatch(line, -1) {
				out[current] = append(out[current], m[1])
			}
		}
	}
	if len(out) < 5 {
		t.Fatalf("only %d resource(s) had subcommands parsed out of admin.go — the switch shape moved, and a guard that parses nothing accepts everything", len(out))
	}
	return out
}

// TestEveryPalaiCommandInAnOperatorRunbookExists is §3.6 D2's general form, and it is the closure the plan
// asked for: "commands that WORK".
func TestEveryPalaiCommandInAnOperatorRunbookExists(t *testing.T) {
	root := repoRoot(t)
	direct, admin := cliResources(t)
	subs := cliSubcommands(t)

	checked := 0
	for _, doc := range runbooksUnderGuard {
		raw, err := os.ReadFile(filepath.Join(root, doc))
		if err != nil {
			t.Fatalf("read %s: %v", doc, err)
		}
		for _, m := range palaiCommand.FindAllStringSubmatch(string(raw), -1) {
			first, second, third := m[1], m[2], m[3]
			resource, sub := first, second
			if first == "admin" {
				resource, sub = second, third
				if !slices.Contains(admin, resource) {
					t.Errorf("%s prints `palai admin %s …` and `admin <resource>` dispatches only %v — this is D2's exact shape: a runbook block an operator copies, naming a resource nothing registers", doc, resource, admin)
					continue
				}
			} else if !slices.Contains(direct, resource) {
				// `palai up`, `palai local …`, `palai doctor` and friends are top-level verbs rather than
				// admin resources, and they are out of this guard's scope rather than failures. They are
				// recognised by NOT being in either dispatch set AND having no subcommand this file could
				// resolve — stated so a reader knows the guard's edge rather than guessing at its silence.
				checked++
				continue
			}
			checked++
			known, ok := subs[resource]
			if !ok {
				continue // a resource with no `switch sub` (nothing to resolve against)
			}
			if sub == "" {
				t.Errorf("%s prints `palai %s` with no subcommand, and that resource dispatches on one of %v", doc, resource, known)
				continue
			}
			if !slices.Contains(known, sub) {
				t.Errorf("%s prints `palai %s %s` and the CLI dispatches only %v for that resource — a runbook step nobody ran reads exactly like one that was", doc, resource, sub, known)
			}
		}
	}
	if checked == 0 {
		t.Fatal("this guard resolved ZERO commands — either the runbooks stopped printing them or the regexp shape moved, and either way it is reporting green over nothing")
	}
	t.Logf("runbook commands: %d `palai …` invocation(s) resolved across %d operator doc(s)", checked, len(runbooksUnderGuard))
}

// TestTheRunbookCommandGuardCanActuallyFail is the guard for the guard, and its first draft was WRONG in the
// same direction the thing it guards was wrong — which is recorded rather than quietly fixed, because it is
// the second instance of one shape in one file.
//
// The draft asserted that `admin pool` does NOT resolve, taking §3.6 D2's sentence at face value. It DOES:
// main.go's `case "admin"` hands `args[1]` to admin.Run, so once E28 T1 added the `pool` resource,
// `palai admin pool create …` began working — verified against the real binary, which builds the request and
// fails on CONNECT. D2 was true when it was written and this epic made it false, which is exactly what
// CLAUDE.md's fourth rule says an inherited ceiling does.
//
// So the negative control is the half that is still true, and it is asserted by SHAPE rather than by a
// remembered sentence: `pool key` is not one resource, it is a resource followed by a subcommand the pool
// switch does not have.
func TestTheRunbookCommandGuardCanActuallyFail(t *testing.T) {
	direct, admin := cliResources(t)
	subs := cliSubcommands(t)

	// The positive half FIRST, so the negatives below cannot both be true of an empty parse.
	// ‼️ THE MEMBERS ARE `pool` AND `model`, AND THE MESSAGE SAYS WHAT THE CONDITION CHECKS. It required
	// `pool` AND `poolkey` while saying "neither … is", which reads as an OR and describes a different
	// failure than the one it produces — the shape this tree files under "an assertion that points at the
	// wrong file". `poolkey` also left the CLI on 2026-08-07, so requiring it made this floor fail for the
	// deletion rather than for a broken parse, which is exactly the confusion a floor exists to prevent.
	for _, member := range []string{"pool", "model"} {
		if !slices.Contains(direct, member) {
			t.Fatalf("%q is missing from the direct dispatch set %v — the parse produced nothing usable, so every assertion here is vacuous", member, direct)
		}
	}
	if !slices.Contains(admin, "runner") {
		t.Fatalf("`runner` is not reachable through `palai admin <resource>` (%v) — the machine lifecycle is reached ONLY that way, so a parse that lost it would accept a runbook printing anything", admin)
	}

	// The negative: `key` is not a subcommand of `pool`, which is what makes the E24 block's second line
	// wrong. If it ever becomes one, this guard has lost its control and the runbook needs re-reading.
	if slices.Contains(subs["pool"], "key") {
		t.Error("`pool key` resolves as a subcommand — the E24 handover block's second line would then be correct, and this guard has no negative control left")
	}
	// And a name nothing dispatches, so a parse that returned every string would be caught.
	if slices.Contains(direct, "notaresource") || slices.Contains(admin, "notaresource") {
		t.Error("a name that is in no dispatch switch resolved — the parser is not matching what it claims to match")
	}
}
