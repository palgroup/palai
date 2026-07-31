package fleetconsole

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/palgroup/palai/tests/uat"
)

// THE E28 CATALOG GATE. It owns the `FLC-` ids this epic opens, and it owns them ALONE: the E17 gate's orphan
// sweep defers to uat.FleetConsoleCaseIDs and refuses an id claimed by more than one catalog, so ownership
// moved without lapsing.
//
// UNLIKE E25's, THIS FILE ARRIVES AT THE EXIT GATE AND THAT IS THE FINDING RATHER THAN THE PLAN. E25 T1
// created its catalog with its first case precisely so a `CON-` directory could not sit unresolved; E28 T1,
// T2 and T3 each shipped a case directory and none of them added the prefix or the owner list, so FLC-001,
// FLC-002 and FLC-003 were resolved by NOTHING for three tasks while `go test ./tests/uat/...` reported `ok`
// for every package. It was demonstrated RED here — three directories reported at once — before this file
// existed, which is the third time this repository has reproduced that silence on request.
//
// PROOF RESOLUTION IS EXACT, NOT SUBSTRING. E28's proofs are mostly Playwright specs, referenced as
// `file.spec.ts:<test title>`, and a title mentioned in a COMMENT must not satisfy the reference — this tree
// has four shipped instances of a verifier defeated by exactly that kind of loose comparison.
//
// NO TIER MOVES HERE, and none moves anywhere in E28: `console` closes PREVIEW.

type flcCase struct {
	ID           string   `yaml:"id"`
	Name         string   `yaml:"name"`
	ProofClass   string   `yaml:"proof_class"`
	Provider     string   `yaml:"provider"`
	Input        string   `yaml:"input"`
	ExpectStatus string   `yaml:"expect_status"`
	Proof        []string `yaml:"proof"`
}

var (
	honestNamePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	validProofClasses = map[string]bool{
		"unit": true, "component-real": true, "e2e-deterministic": true,
		"live-provider": true, "external-receipt": true, "fault-live": true,
	}
	// localSeamWords are the words that tell a reader WHAT produced the evidence. A case naming none of them
	// is the prose half of an overclaim, and for an epic whose every machine is a fake runner that matters
	// more than usual.
	localSeamWords = []string{"fake", "loopback", "deterministic", "live", "component-real", "local proof", "local seam"}
)

// expectedFleetConsoleCatalog maps each id to the proof class its case.yaml must declare and HOW MANY in-tree
// proofs must back it — the E24/E25 shape rather than a retyped copy of every reference. What the count buys
// is the one thing duplication bought: a case cannot silently DROP a proof leg. Every reference in the case's
// OWN list is still resolved, so a renamed test, a moved file or a dropped leg all fail.
var expectedFleetConsoleCatalog = map[string]struct {
	class  string
	proofs int
	why    string
}{
	"FLC-001": {"component-real", 16, "a pool can be created with a posture and with strict enrolment switched on, a machine enrols into it over real mTLS and lands `pending`, a Dial answers ErrPoolHasNoRunner so the waiting room is structural rather than a comparison, the approve route is reached over HTTP against a machine an operator's own pool produced, and FLT-P14's counter finally has a reader — none of which any code path in this tree could do before"},
	"FLC-002": {"e2e-deterministic", 13, "a policy form writes the whole document so setting a pool does not silently erase an approver list HIL-P11 measured to be permissive when empty, the REQUEST is asserted beside the stored outcome because a server that merged would pass the outcome alone, and a server-minted value is shown once in a node nothing reads back"},
	"FLC-003": {"e2e-deterministic", 22, "the waiting room is on a screen for the first time and a machine is admitted from it, two pointer fields keep could-not-ask distinct from zero, last_seen_at states what it means and no machine carries an invented health badge, and the page names the three things a fleet screen must not let an operator assume"},
	"FLC-004": {"e2e-deterministic", 9, "the confirmation split is a property of the SET rather than of the actions a spec named: a source sweep over every page resolves each destructive action to its confirmation and refuses both directions, so an irreversible action outside an alertdialog and a reversible one inside it are equally red"},
}

func TestFleetConsoleCatalogMaterialized(t *testing.T) {
	root := repoRoot(t)
	casesDir := filepath.Join(root, "tests", "uat", "cases")

	for id, want := range expectedFleetConsoleCatalog {
		raw, err := os.ReadFile(filepath.Join(casesDir, id, "case.yaml"))
		if err != nil {
			t.Errorf("%s: read case.yaml: %v", id, err)
			continue
		}
		var c flcCase
		if err := yaml.Unmarshal(raw, &c); err != nil {
			t.Errorf("%s: decode case.yaml: %v", id, err)
			continue
		}
		if c.ID != id {
			t.Errorf("%s: id = %q, want the directory name", id, c.ID)
		}
		if c.ProofClass != want.class {
			t.Errorf("%s: proof_class = %q, want %q", id, c.ProofClass, want.class)
		}
		if !validProofClasses[c.ProofClass] {
			t.Errorf("%s: proof_class = %q, not a master-plan §10.2 class", id, c.ProofClass)
		}
		if !honestNamePattern.MatchString(c.Name) {
			t.Errorf("%s: name = %q, want a kebab-case behaviour assertion", id, c.Name)
		}
		if c.Provider == "" || c.Input == "" || c.ExpectStatus == "" {
			t.Errorf("%s: provider/input/expect_status must all be set (case.yaml discipline)", id)
		}
		if c.ProofClass == "live-provider" && c.Provider == "fake" {
			t.Errorf("%s: a live-provider case must not declare the fake provider", id)
		}

		lower := strings.ToLower(c.Input)
		named := false
		for _, word := range localSeamWords {
			if strings.Contains(lower, word) {
				named = true
				break
			}
		}
		if !named {
			t.Errorf("%s: the case text names no LOCAL seam (want one of %v) — without it a reader cannot tell a fake-upstream proof from a real one, and every machine in this epic is a fake runner", id, localSeamWords)
		}
		if !strings.Contains(c.Input, "§6") {
			t.Errorf("%s: the case text names no §6 operator leg — every case must say what real-infrastructure leg it does NOT cover, and for this epic leg 1 (a REAL rented Mac) is the one a reader will assume was covered", id)
		}
		assertFleetConsoleProofs(t, root, id, want.proofs, c.Proof)
	}

	// The E28 side of the split ownership: every id this package claims must actually have a directory, and
	// the canonical list and this catalog must be the SAME SET. The E17 gate covers the other direction (no
	// FLC- dir escapes every catalog).
	for _, id := range uat.FleetConsoleCaseIDs {
		if _, ok := expectedFleetConsoleCatalog[id]; !ok {
			t.Errorf("%s is in uat.FleetConsoleCaseIDs but this gate resolves no proofs for it — the canonical list and the catalog must be the same set", id)
		}
		if _, err := os.Stat(filepath.Join(casesDir, id, "case.yaml")); err != nil {
			t.Errorf("%s is claimed by this epic but has no case.yaml: %v", id, err)
		}
	}
	for id := range expectedFleetConsoleCatalog {
		if !slices.Contains(uat.FleetConsoleCaseIDs, id) {
			t.Errorf("%s is cataloged here but is not in uat.FleetConsoleCaseIDs, so the E17 orphan guard would report it as an escapee", id)
		}
	}

	// And the third direction, which is the one that was MEASURED to be silent in THIS epic: an `FLC-`
	// directory on disk that nothing claims. Three of them sat here for three tasks.
	entries, err := os.ReadDir(casesDir)
	if err != nil {
		t.Fatalf("read cases dir: %v", err)
	}
	seen := 0
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "FLC-") {
			seen++
			if !slices.Contains(uat.FleetConsoleCaseIDs, e.Name()) {
				t.Errorf("%s is an FLC- case directory that uat.FleetConsoleCaseIDs does not claim — three such directories were measured to leave every UAT gate green in this very epic", e.Name())
			}
		}
	}
	if seen != len(uat.FleetConsoleCaseIDs) {
		t.Errorf("the tree holds %d FLC- case dir(s) but this epic claims %d — one of the two is stale", seen, len(uat.FleetConsoleCaseIDs))
	}
}

// TestTheFLCPrefixIsInsideTheOrphanSweep is the standing dependency this file has on another package's
// variable, asserted rather than assumed. tests/uat/extensions is the ONLY place in the tree that walks
// tests/uat/cases, so an `FLC-` removed from extensionIDPrefixes would silently stop every FLC- directory
// from being checked at all — which is not hypothetical here: that is the state this epic ran in for three
// tasks.
func TestTheFLCPrefixIsInsideTheOrphanSweep(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "tests", "uat", "extensions", "catalog_test.go"))
	if err != nil {
		t.Fatalf("read the E17 catalog gate: %v", err)
	}
	line := ""
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "var extensionIDPrefixes ") {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatal("extensionIDPrefixes is no longer declared in tests/uat/extensions/catalog_test.go — the orphan sweep this gate depends on has moved or gone")
	}
	if !strings.Contains(line, `"FLC-"`) {
		t.Errorf(`extensionIDPrefixes no longer contains "FLC-": %s

The E17 sweep is the only place in the tree that walks tests/uat/cases, so a prefix outside it is a family
whose directories NOTHING checks. Ownership may live in uat.FleetConsoleCaseIDs; escaping the sweep may not.`, strings.TrimSpace(line))
	}
}

// TestTheFLCPrefixCollidesWithNoOtherFamily re-derives the disjointness claim `FLC-` was CHOSEN for, from the
// directory rather than from a paragraph. `FLT-` and `CON-` are both live prefixes with their own gates, and
// the whole argument for a third one is that an E28 id must never match an earlier family marker in
// PromoteGateFor.
func TestTheFLCPrefixCollidesWithNoOtherFamily(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join(repoRoot(t), "tests", "uat", "cases"))
	if err != nil {
		t.Fatalf("read cases dir: %v", err)
	}
	prefixes := map[string]bool{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		cut := strings.LastIndex(name, "-")
		if cut <= 0 {
			continue
		}
		prefixes[name[:cut+1]] = true
	}
	if !prefixes["FLC-"] {
		t.Error("no FLC- directory exists at all — this epic's whole case family is missing")
	}
	// The three prefixes an E28 id could plausibly have been given, and the reason it was not: each already
	// belongs to a family with its own owner list and its own gate.
	for _, taken := range []string{"FLT-", "CON-", "BGT-"} {
		if !prefixes[taken] {
			t.Errorf("%s no longer exists in tests/uat/cases — the disjointness argument for FLC- names it as an OCCUPIED prefix, and an argument whose premise moved is an argument nobody re-checked", taken)
		}
	}
	for _, id := range uat.FleetConsoleCaseIDs {
		if !strings.HasPrefix(id, "FLC-") {
			t.Errorf("%s is in uat.FleetConsoleCaseIDs and does not carry the FLC- prefix — PromoteGateFor's E28 clause is a case-id membership test, so an id outside the family would dispatch to whichever earlier clause matched it", id)
		}
	}
}

// assertFleetConsoleProofs pins the leg COUNT and resolves every reference. A Playwright reference must
// resolve to a real `test("<title>"` declaration — not merely to the title appearing somewhere in the file,
// which a comment satisfies and which is how a verifier in this tree has been defeated before.
func assertFleetConsoleProofs(t *testing.T, root, id string, want int, got []string) {
	t.Helper()
	if len(got) != want {
		t.Errorf("%s: declares %d proof(s), want %d — a case that DROPS a leg is how a claim rots into a vacuous green, and one that gains a leg is a catalog nobody updated. Change the count deliberately, with the reason", id, len(got), want)
	}
	for _, ref := range got {
		file, name, ok := strings.Cut(ref, ":")
		if !ok {
			t.Errorf("%s: proof %q is not file:reference", id, ref)
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, file))
		if err != nil {
			t.Errorf("%s: proof file %q does not exist: %v", id, file, err)
			continue
		}
		body := string(raw)
		switch {
		case strings.HasSuffix(file, ".ts") || strings.HasSuffix(file, ".mjs"):
			if !strings.Contains(body, `test("`+name+`"`) && !strings.Contains(body, "test(`"+name+"`") && !strings.Contains(body, `it("`+name+`"`) {
				t.Errorf("%s: %q is not a DECLARED test in %s — the case claims a proof that is not in the tree (a title that only appears in a comment does not count)", id, name, file)
			}
		default:
			if !strings.Contains(body, "func "+name+"(") {
				t.Errorf("%s: proof %q not found in %s", id, name, file)
			}
		}
	}
}

// repoRoot walks up to the module root (the dir holding go.mod).
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test's working directory")
		}
		dir = parent
	}
}

// caseAssertion derives a case's bundle db_assertion line from THIS gate's catalog rather than retyping it
// into evidence_fleet_console.go — the E24/E25 shape. One owner for "what this case proves and at which tier".
func caseAssertion(t *testing.T, id string) string {
	t.Helper()
	entry, ok := expectedFleetConsoleCatalog[id]
	if !ok {
		t.Fatalf("%s has no catalog entry to derive its bundle assertion from", id)
	}
	return fmt.Sprintf("%s: proven by %d in-tree proof(s) at the %s tier — %s",
		id, entry.proofs, entry.class, entry.why)
}

// caseProofClass is the same one-owner rule for the bundle's `proof_class` field. E28's cases are NOT all one
// class — FLC-001 is `component-real` (T1's Go half, which touches no console file) while the other three are
// `e2e-deterministic` — so the bundle reads it here instead of hard-coding one value for all four.
func caseProofClass(t *testing.T, id string) string {
	t.Helper()
	entry, ok := expectedFleetConsoleCatalog[id]
	if !ok {
		t.Fatalf("%s has no catalog entry to derive its proof class from", id)
	}
	return entry.class
}
