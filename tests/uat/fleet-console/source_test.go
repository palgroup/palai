// The E28 T4 exit gate's SOURCE half (plan §T4): the three ledgers that are re-derived from the console's own
// files rather than from a run's stdout, and the guard that each recompute can actually fail.
//
// WHY THESE THREE AND NOT THE OTHER FIVE. Groups (a), (b), (c), (d) and (h) are OUTCOMES — a pool that exists,
// a machine that waited, bytes that were not found, a list that survived, collections that matched — and an
// outcome is only observable by running something, so their numbers are diffed against a run in journey_test.go.
// Groups (e), (f) and (g) are PROPERTIES OF THE TREE: which routes are declared, which confirmation each
// destructive action goes through, and which ceilings the pages state. A property of the tree can be measured
// FROM the tree, and measuring it there is strictly stronger — it holds on a checkout where nothing ran.
//
// AND THE DIRECTION MATTERS, WHICH IS THE E26 T7 LESSON APPLIED TO A SOURCE SCAN. Each sweep below checks BOTH
// ways: every ledger row must be found in the source, AND every occurrence in the source must have a ledger
// row. One direction alone is the shape this repository keeps shipping — a ledger that names three dialogs is
// satisfied by a console with thirty, and a scan that finds three occurrences is satisfied by a ledger naming
// none of them.
package fleetconsole

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/palgroup/palai/tests/uat"
)

// consoleDir is apps/web-console, the one tree these sweeps read.
func consoleDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "apps", "web-console")
}

// routeDeclPattern matches one row of lib/routes.ts. It is anchored on the object literal's own fields rather
// than on a line number, so reordering the list or reformatting it does not move the measurement.
var routeDeclPattern = regexp.MustCompile(`\{ path: "([^"]+)", label: "[^"]*", readyTestId: "([^"]+)", lead: "`)

// declaredRoutes parses apps/web-console/lib/routes.ts — the SINGLE source of both the navigation and the axe
// sweep — and returns the paths in file order with their readiness signals.
func declaredRoutes(t *testing.T) []struct{ Path, ReadyTestID string } {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(consoleDir(t), "lib", "routes.ts"))
	if err != nil {
		t.Fatalf("read lib/routes.ts (the route ledger is DERIVED from it, never retyped): %v", err)
	}
	matches := routeDeclPattern.FindAllStringSubmatch(string(raw), -1)
	if len(matches) == 0 {
		t.Fatal("no route rows parsed out of lib/routes.ts — the literal's SHAPE changed, and a parser that matches nothing reports an empty list exactly like a console with no pages")
	}
	out := make([]struct{ Path, ReadyTestID string }, 0, len(matches))
	for _, m := range matches {
		out = append(out, struct{ Path, ReadyTestID string }{m[1], m[2]})
	}
	return out
}

// TestTheRouteLedgerIsTheRouteTable re-derives group (e)'s declared side from lib/routes.ts. The SCANNED side
// is a run's answer and is diffed in journey_test.go; what this pins is that the ledger's idea of "declared"
// is the file's, so a page added to the console without a ledger row cannot leave the bundle claiming full
// coverage of a smaller console.
func TestTheRouteLedgerIsTheRouteTable(t *testing.T) {
	var rows []struct {
		Path        string `json:"path"`
		ReadyTestID string `json:"ready_test_id"`
	}
	if err := json.Unmarshal([]byte(uat.FleetConsoleRouteLedger), &rows); err != nil {
		t.Fatalf("decode the canonical route ledger: %v", err)
	}
	declared := declaredRoutes(t)

	inLedger := map[string]string{}
	for _, r := range rows {
		inLedger[r.Path] = r.ReadyTestID
	}
	inFile := map[string]string{}
	for _, r := range declared {
		inFile[r.Path] = r.ReadyTestID
	}

	for path, ready := range inFile {
		got, ok := inLedger[path]
		if !ok {
			t.Errorf("lib/routes.ts declares %q and the bundle's route ledger does not carry it — the bundle would then certify \"every declared route was scanned\" over a console that has one more page than the ledger knows about", path)
			continue
		}
		if got != ready {
			t.Errorf("%s: the ledger's readiness signal is %q and lib/routes.ts says %q — axe on a page still showing a spinner reports a clean bill of health for markup it never saw, so the signal is the measurement rather than a label", path, got, ready)
		}
	}
	for path := range inLedger {
		if _, ok := inFile[path]; !ok {
			t.Errorf("the route ledger carries %q, which lib/routes.ts does not declare — a row for a page the console does not have inflates both sides of an equality at once", path)
		}
	}
	for _, want := range uat.FleetConsoleNewRoutes {
		if _, ok := inFile[want]; !ok {
			t.Errorf("%s is a page THIS EPIC opened and lib/routes.ts does not declare it", want)
		}
	}
}

// --- group (f): the confirmation every destructive action goes through --------------------------------------

// alertDialogPattern finds one mounted ConfirmDestructive by the testId it carries. The testId is used as the
// identity because it is the handle the spec drives, so a dialog the sweep sees and a dialog a spec asserts
// are provably the same dialog rather than two things with similar names.
var alertDialogPattern = regexp.MustCompile(`<ConfirmDestructive\s+testId="([^"]+)"`)

// nativeConfirmPattern finds one window.confirm CALL. The `(` is load-bearing: app/fleet/page.tsx and
// components/ConfirmDestructive.tsx both discuss `window.confirm` at length in comments explaining WHY the
// split exists, and a scan that counted those would find confirmations in files that perform none.
var nativeConfirmPattern = regexp.MustCompile(`window\.confirm\(`)

// consolePageSources returns every app/**/page.tsx, which is where a destructive action can be mounted.
func consolePageSources(t *testing.T) map[string]string {
	t.Helper()
	root := filepath.Join(consoleDir(t), "app")
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Name() != "page.tsx" {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		rel, relErr := filepath.Rel(consoleDir(t), path)
		if relErr != nil {
			return relErr
		}
		out[filepath.ToSlash(rel)] = string(raw)
		return nil
	})
	if err != nil {
		t.Fatalf("walk apps/web-console/app: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("no page.tsx found under apps/web-console/app — a walk that finds nothing reports zero confirmations exactly like a console with no destructive actions")
	}
	return out
}

// sweepConfirmationSites is the recompute behind group (f): every confirmation SITE in the console's pages,
// keyed by the id the ledger uses. An alertdialog's id is its testId; a native confirm has none, so it is
// keyed by the file that performs it — which is also why the ledger's reversible rows are one per FILE and
// not one per action (app/fleet/page.tsx's single window.confirm serves both cordon and resume, and splitting
// it in the ledger would make a 1:1 sweep impossible to write honestly).
func sweepConfirmationSites(t *testing.T) map[string]string {
	t.Helper()
	sites := map[string]string{}
	for file, src := range consolePageSources(t) {
		for _, m := range alertDialogPattern.FindAllStringSubmatch(src, -1) {
			if prev, dup := sites[m[1]]; dup {
				t.Errorf("two ConfirmDestructive mounts carry testId %q (%s and %s) — a spec driving that id would assert against whichever rendered", m[1], prev, file)
			}
			sites[m[1]] = file
		}
		if n := len(nativeConfirmPattern.FindAllString(src, -1)); n > 0 {
			if n > 1 {
				t.Errorf("%s performs %d window.confirm calls — the ledger's reversible rows are one per FILE, so a second call in one file is a confirmation with no row", file, n)
			}
			sites[file] = file
		}
	}
	return sites
}

// TestEveryIrreversibleActionInTheConsoleIsBehindAnAlertDialog is FLC-004's proof and group (f)'s recompute.
// It is the claim neither T2's spec nor T3's could make: not that the actions a spec NAMED are guarded, but
// that the SET of destructive actions in this console splits exactly along the published criterion.
func TestEveryIrreversibleActionInTheConsoleIsBehindAnAlertDialog(t *testing.T) {
	var rows []struct {
		Action       string `json:"action"`
		Reversible   bool   `json:"reversible"`
		Confirmation string `json:"confirmation"`
		SourceFile   string `json:"source_file"`
	}
	if err := json.Unmarshal([]byte(uat.FleetConsoleActionLedger), &rows); err != nil {
		t.Fatalf("decode the canonical action ledger: %v", err)
	}
	sites := sweepConfirmationSites(t)

	claimed := map[string]bool{}
	for _, row := range rows {
		file, ok := sites[row.Action]
		if !ok {
			t.Errorf("the action ledger names %q and no page mounts it — a guarded action nobody renders is a row, not a control", row.Action)
			continue
		}
		claimed[row.Action] = true
		if file != row.SourceFile {
			t.Errorf("%s: the ledger says %s and the sweep found it in %s", row.Action, row.SourceFile, file)
		}
		// The confirmation KIND is derived from HOW the sweep found it, never from the row: an id that came
		// out of alertDialogPattern is an alertdialog by construction.
		want := uat.FleetConsoleAlertDialog
		if row.Action == row.SourceFile {
			want = uat.FleetConsoleNativeConfirm
		}
		if row.Confirmation != want {
			t.Errorf("%s: the ledger declares confirmation %q but the source performs %q", row.Action, row.Confirmation, want)
		}
		if !row.Reversible && want != uat.FleetConsoleAlertDialog {
			t.Errorf("%s is IRREVERSIBLE and goes through %s — SC 3.3.4 leg 1 is unreachable for it (nothing un-revokes), leg 2 is empty with no entered data, so leg 3 is what is left and leg 3 needs a review", row.Action, want)
		}
		if row.Reversible && want != uat.FleetConsoleNativeConfirm {
			t.Errorf("%s is REVERSIBLE and was put behind an %s — leg 1 already discharges the criterion, and a build that guards everything has stopped distinguishing rather than become more careful", row.Action, want)
		}
	}
	for id, file := range sites {
		if !claimed[id] {
			t.Errorf("%s performs a confirmation the ledger does not carry (%q) — this is the direction a ledger-only check cannot see: a new destructive action ships, the bundle's counters do not move, and the release certifies a console with one fewer action than it has", file, id)
		}
	}
}

// TestTheActionLedgerIsTheSweepsOutput is the counter half: the numbers the BUNDLE carries must be the ones
// this sweep produces, so the proof cannot declare a split the source contradicts.
func TestTheActionLedgerIsTheSweepsOutput(t *testing.T) {
	guarded, native, misplaced, err := uat.SweepFleetConsoleActions(json.RawMessage(uat.FleetConsoleActionLedger))
	if err != nil {
		t.Fatalf("the canonical action ledger cannot be swept: %v", err)
	}
	if len(misplaced) != 0 {
		t.Fatalf("the canonical action ledger is itself misplaced: %v", misplaced)
	}
	sites := sweepConfirmationSites(t)
	dialogs, natives := 0, 0
	for id, file := range sites {
		if id == file {
			natives++
			continue
		}
		dialogs++
	}
	if guarded != dialogs {
		t.Errorf("the ledger carries %d irreversible action(s) behind an alertdialog and the source mounts %d ConfirmDestructive(s)", guarded, dialogs)
	}
	if native != natives {
		t.Errorf("the ledger carries %d reversible action(s) on window.confirm and the source performs %d call(s)", native, natives)
	}
}

// TestTheActionSweepRefusesBothDirections is the guard for the guard, and it is the reason this file exists
// rather than a comment claiming the sweep is total. A sweep that could not fail reports the same green as one
// that passes — this tree's most-found defect — so both directions are demonstrated by MUTATION against the
// canonical ledger rather than argued.
func TestTheActionSweepRefusesBothDirections(t *testing.T) {
	var rows []map[string]any
	if err := json.Unmarshal([]byte(uat.FleetConsoleActionLedger), &rows); err != nil {
		t.Fatalf("decode the canonical action ledger: %v", err)
	}
	if len(rows) < 2 {
		t.Fatalf("the action ledger carries %d row(s) — a difference cannot be shown over fewer than two", len(rows))
	}

	// Direction 1: an IRREVERSIBLE action left on the native confirmation.
	loosened := deepCopyRows(t, rows)
	for _, row := range loosened {
		if row["reversible"] == false {
			row["confirmation"] = uat.FleetConsoleNativeConfirm
			break
		}
	}
	if _, _, misplaced, err := uat.SweepFleetConsoleActions(mustJSON(t, loosened)); err != nil || len(misplaced) == 0 {
		t.Errorf("an irreversible action on window.confirm was NOT refused (misplaced=%v, err=%v) — that is the whole content of SC 3.3.4 leg 3 for a revoke", misplaced, err)
	}

	// Direction 2: a REVERSIBLE action promoted into the dialog, which is the change the next reader makes
	// "for consistency" and which components/ConfirmDestructive.tsx's own header warns against.
	tightened := deepCopyRows(t, rows)
	for _, row := range tightened {
		if row["reversible"] == true {
			row["confirmation"] = uat.FleetConsoleAlertDialog
			break
		}
	}
	if _, _, misplaced, err := uat.SweepFleetConsoleActions(mustJSON(t, tightened)); err != nil || len(misplaced) == 0 {
		t.Errorf("a reversible action behind an alertdialog was NOT refused (misplaced=%v, err=%v) — a console that guards everything has stopped distinguishing, and the ledger has to see that as a change", misplaced, err)
	}

	// Direction 3: one side of the split deleted entirely. A ledger holding only irreversible rows would make
	// every row "correctly guarded" and prove no DIFFERENCE at all.
	onlyIrreversible := []map[string]any{}
	for _, row := range deepCopyRows(t, rows) {
		if row["reversible"] == false {
			onlyIrreversible = append(onlyIrreversible, row)
		}
	}
	if _, _, _, err := uat.SweepFleetConsoleActions(mustJSON(t, onlyIrreversible)); err == nil {
		t.Error("a ledger with only irreversible rows was ACCEPTED — the claim is a difference, and a ledger with one kind in it cannot show one")
	}
}

// --- group (g): the ceilings the pages state ----------------------------------------------------------------

// TestEveryCeilingTheBundleClaimsIsInThePageSource re-derives group (g). The gap ID is what makes this
// countable: "the page warns about remote execution" is a sentence a reviewer judges, while "the page renders
// FLT-P15 inside a node carrying this testid" is two strings a scan finds and a deletion reddens.
func TestEveryCeilingTheBundleClaimsIsInThePageSource(t *testing.T) {
	var rows []struct {
		GapID      string `json:"gap_id"`
		Page       string `json:"page"`
		TestID     string `json:"test_id"`
		SourceFile string `json:"source_file"`
	}
	if err := json.Unmarshal([]byte(uat.FleetConsoleCeilingLedger), &rows); err != nil {
		t.Fatalf("decode the canonical ceiling ledger: %v", err)
	}
	sources := consolePageSources(t)
	for _, row := range rows {
		src, ok := sources[row.SourceFile]
		if !ok {
			t.Errorf("%s: the ceiling ledger names %s, which is not a console page", row.GapID, row.SourceFile)
			continue
		}
		if !strings.Contains(src, `data-testid="`+row.TestID+`"`) {
			t.Errorf("%s: %s renders no node with data-testid=%q — a ceiling with no handle is a sentence the next refactor removes silently", row.GapID, row.SourceFile, row.TestID)
		}
		if !strings.Contains(src, "<code>"+row.GapID+"</code>") {
			t.Errorf("%s: %s does not render the gap id itself — the id is what lets an operator find the row that explains the ceiling, and prose without it is a caution rather than a reference", row.GapID, row.SourceFile)
		}
	}
	onScreen, missing, err := uat.SweepFleetConsoleCeilings(json.RawMessage(uat.FleetConsoleCeilingLedger))
	if err != nil {
		t.Fatalf("the canonical ceiling ledger cannot be swept: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("the ledger states %v and owes %v — the missing ones are %v", onScreen, uat.FleetConsoleCeilings, missing)
	}
}

// TestTheCeilingSweepFailsWhenAPageStopsSayingIt is the mutation that makes the test above load-bearing. It
// does not edit the tree: it asks the SWEEP what it does with a row whose page no longer states its id, which
// is the same question a deletion would ask and is answerable without touching a shipped file.
func TestTheCeilingSweepFailsWhenAPageStopsSayingIt(t *testing.T) {
	sources := consolePageSources(t)
	var rows []struct {
		GapID      string `json:"gap_id"`
		SourceFile string `json:"source_file"`
	}
	if err := json.Unmarshal([]byte(uat.FleetConsoleCeilingLedger), &rows); err != nil {
		t.Fatalf("decode the canonical ceiling ledger: %v", err)
	}
	for _, row := range rows {
		src := sources[row.SourceFile]
		stripped := strings.ReplaceAll(src, "<code>"+row.GapID+"</code>", "")
		if stripped == src {
			t.Errorf("%s: removing the id from %s changed nothing, so the assertion above cannot fail — the page never rendered it in the form the scan looks for", row.GapID, row.SourceFile)
		}
		if strings.Contains(stripped, "<code>"+row.GapID+"</code>") {
			t.Errorf("%s: %s states the id more than once, so a single deletion would not redden the scan", row.GapID, row.SourceFile)
		}
	}
	// And the ledger side: a dropped ceiling must be REPORTED rather than silently lowering the count.
	kept := []map[string]any{}
	var all []map[string]any
	if err := json.Unmarshal([]byte(uat.FleetConsoleCeilingLedger), &all); err != nil {
		t.Fatalf("decode the canonical ceiling ledger: %v", err)
	}
	for _, row := range all[1:] {
		kept = append(kept, row)
	}
	_, missing, err := uat.SweepFleetConsoleCeilings(mustJSON(t, kept))
	if err != nil {
		t.Fatalf("the shortened ceiling ledger cannot be swept: %v", err)
	}
	if len(missing) != 1 {
		t.Errorf("dropping one ceiling row reported %d missing, want 1 — a ledger that can shrink without a refusal is a ceiling nobody has to state", len(missing))
	}
}

// --- shared helpers ------------------------------------------------------------------------------------------

func deepCopyRows(t *testing.T, rows []map[string]any) []map[string]any {
	t.Helper()
	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatalf("copy rows: %v", err)
	}
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("copy rows: %v", err)
	}
	return out
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

// TestTheNewRoutesAreDeclaredAndNamed is the smallest of the source guards and the one that keeps this epic's
// two pages from being counted by accident: FleetConsoleNewRoutes is a canonical list, and both the ledger and
// the file have to carry both members.
func TestTheNewRoutesAreDeclaredAndNamed(t *testing.T) {
	declared := declaredRoutes(t)
	paths := make([]string, 0, len(declared))
	for _, r := range declared {
		paths = append(paths, r.Path)
	}
	for _, want := range uat.FleetConsoleNewRoutes {
		if !slices.Contains(paths, want) {
			t.Errorf("%s is not declared in lib/routes.ts; declared: %v", want, paths)
		}
	}
}
