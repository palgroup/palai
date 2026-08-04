package toolexecution

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

// THE FAZ A.3 CATALOG GATE. It owns the `EXE-` ids this phase opens, and it owns them ALONE: the E17 gate's
// orphan sweep defers to uat.ToolExecutionCaseIDs and refuses an id claimed by more than one catalog, so
// ownership moved without lapsing.
//
// THIS FILE ARRIVES WITH THE FIRST CASE DIRECTORY RATHER THAN AFTER IT, which is the one thing about it worth
// noting. HIL-, CON-, BGT- and FLC- each shipped case directories BEFORE their prefix entered the E17 sweep,
// and every one of those directories was resolved by nothing while `go test ./tests/uat/...` reported ok for
// every package — measured three separate times, most recently in E28 where three directories sat unresolved
// for three tasks. `EXE-` entered extensionIDPrefixes and uat.ToolExecutionCaseIDs in the SAME change as
// EXE-001, so there is no fourth instance to report here.
//
// PROOF RESOLUTION IS EXACT, NOT SUBSTRING. A `func <Name>(` is required, so a test name that appears only in
// a comment does not satisfy a reference — this tree has four shipped instances of a verifier defeated by
// exactly that kind of loose comparison.
//
// NO TIER MOVES HERE, and none moves anywhere in A.3: `apple-build` stays DISABLED. The phase moved WHERE a
// command runs and never once ran `xcodebuild` on a Mac.

type exeCase struct {
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
	// localSeamWords are the words that tell a reader WHAT produced the evidence. For THIS phase that matters
	// more than for any release before it: the sentence "the tool runs on the attempt's machine" invites a
	// reader to picture a second computer, and there was not one. A case naming no local seam is the prose
	// half of exactly that overclaim.
	localSeamWords = []string{"local seam", "one box", "fake", "loopback", "deterministic", "component-real"}
)

// expectedToolExecutionCatalog maps each id to the proof class its case.yaml must declare and HOW MANY
// in-tree proofs must back it. What the count buys is the thing duplication bought: a case cannot silently
// DROP a proof leg. Every reference in the case's OWN list is still resolved, so a renamed test, a moved file
// or a dropped leg all fail.
var expectedToolExecutionCatalog = map[string]struct {
	class  string
	proofs int
	why    string
}{
	"EXE-001": {"component-real", 13, "a synchronous command crosses a real lease wire to the machine and its answer comes back, a non-zero exit is a RESULT rather than an error, the executor is derived PER ATTEMPT so two attempts on two machines get two answers, and an attempt with NO machine is handed nothing and the tool refuses instead of falling back to this host — the process-wide `Orchestrator.shell` and `SetShellRunner` that E24's published ceiling rested on are deleted"},
	"EXE-002": {"component-real", 9, "all six coding tools act on the machine that holds the lease and the allocation and clone are opened there, the machine's allocation walk refuses a symlinked component rather than letting MkdirAll follow it, and the NEGATIVE half is asserted per tool: with the machine's answer withheld each REFUSES rather than falling back to this host's disk, and the refusal keeps its sentinel across the wire"},
	"EXE-003": {"component-real", 10, "all three background verbs are addressed to the attempt's machine — including a machine holding NO lease, which is the case the reconciler actually needs — cordon still answers while revoke and an unknown machine are unreachable, an unreachable machine's task is `lost` rather than `exited` and is NEVER signalled, and two unknowns are not a match"},
	"EXE-004": {"component-real", 9, "the runner starts natively on the Mac and the container runner is refused from two independent directions, the dial address and the verified certificate name are separate fields so no /etc/hosts edit is needed, readiness is read from the control plane's session gauge rather than the process table, and `whoami` resolving to a NAME is a separate assertion from `uname` because uname succeeds under the broken namespace that aborts Xcode"},
}

func TestToolExecutionCatalogMaterialized(t *testing.T) {
	root := repoRoot(t)
	casesDir := filepath.Join(root, "tests", "uat", "cases")

	for id, want := range expectedToolExecutionCatalog {
		raw, err := os.ReadFile(filepath.Join(casesDir, id, "case.yaml"))
		if err != nil {
			t.Errorf("%s: read case.yaml: %v", id, err)
			continue
		}
		var c exeCase
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
			t.Errorf("%s: the case text names no LOCAL seam (want one of %v) — for this phase that is the load-bearing disclosure: \"the tool runs on the attempt's machine\" reads as a second computer and there was not one", id, localSeamWords)
		}
		if !strings.Contains(c.Input, "§6") {
			t.Errorf("%s: the case text names no §6 operator leg — every case must say what real-infrastructure leg it does NOT cover, and for this phase leg 1 (a REAL rented Mac) and leg 2 (`linux/amd64`) are both the ones a reader will assume were covered", id)
		}
		// AND THE DISCLOSURE THIS PHASE OWES SPECIFICALLY. Every one of these cases is proven by COMPOSITION
		// — a real wire here, a real router there — and not one of them was driven through a run. A case that
		// does not say so is a case whose reader will assume the opposite, because the claim it carries is
		// precisely the one an end-to-end run would have made.
		if !strings.Contains(strings.ToUpper(c.Input), "RUN") || !strings.Contains(lower, "run") {
			t.Errorf("%s: the case text never mentions a RUN, so it cannot be saying whether one drove it — every A.3 case is proven by composition and owes that sentence", id)
		}
		assertToolExecutionProofs(t, root, id, want.proofs, c.Proof)
	}

	// The A.3 side of the split ownership: every id this package claims must actually have a directory, and
	// the canonical list and this catalog must be the SAME SET. The E17 gate covers the other direction (no
	// EXE- dir escapes every catalog).
	for _, id := range uat.ToolExecutionCaseIDs {
		if _, ok := expectedToolExecutionCatalog[id]; !ok {
			t.Errorf("%s is in uat.ToolExecutionCaseIDs but this gate resolves no proofs for it — the canonical list and the catalog must be the same set", id)
		}
		if _, err := os.Stat(filepath.Join(casesDir, id, "case.yaml")); err != nil {
			t.Errorf("%s is claimed by this phase but has no case.yaml: %v", id, err)
		}
	}
	for id := range expectedToolExecutionCatalog {
		if !slices.Contains(uat.ToolExecutionCaseIDs, id) {
			t.Errorf("%s is cataloged here but is not in uat.ToolExecutionCaseIDs, so the E17 orphan guard would report it as an escapee", id)
		}
	}

	// And the third direction — the one measured to be SILENT three times in this repository: an `EXE-`
	// directory on disk that nothing claims.
	entries, err := os.ReadDir(casesDir)
	if err != nil {
		t.Fatalf("read cases dir: %v", err)
	}
	seen := 0
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "EXE-") {
			seen++
			if !slices.Contains(uat.ToolExecutionCaseIDs, e.Name()) {
				t.Errorf("%s is an EXE- case directory that uat.ToolExecutionCaseIDs does not claim — directories in exactly this state were measured to leave every UAT gate green in E23, E25 and E28", e.Name())
			}
		}
	}
	if seen != len(uat.ToolExecutionCaseIDs) {
		t.Errorf("the tree holds %d EXE- case dir(s) but this phase claims %d — one of the two is stale", seen, len(uat.ToolExecutionCaseIDs))
	}
}

// TestTheEXEPrefixIsInsideTheOrphanSweep is the standing dependency this file has on another package's
// variable, asserted rather than assumed. tests/uat/extensions is the ONLY place in the tree that walks
// tests/uat/cases, so an `EXE-` removed from extensionIDPrefixes would silently stop every EXE- directory
// from being checked at all.
func TestTheEXEPrefixIsInsideTheOrphanSweep(t *testing.T) {
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
	if !strings.Contains(line, `"EXE-"`) {
		t.Errorf(`extensionIDPrefixes no longer contains "EXE-": %s

The E17 sweep is the only place in the tree that walks tests/uat/cases, so a prefix outside it is a family
whose directories NOTHING checks. Ownership may live in uat.ToolExecutionCaseIDs; escaping the sweep may not.`, strings.TrimSpace(line))
	}
}

// TestTheA3AndE2xFamiliesAreDisjoint re-derives the claim `EXE-` was CHOSEN for, and PromoteGateFor's first
// clause now rests on it. If an A.3 id ever appeared in an earlier family's list, PromoteGateFor would match
// that family FIRST — this gate is dispatched ahead of E28's, but the id sets are what the dispatch reads —
// and a bundle carrying A.3's placement ledger, its no-fallback half and its SUPERSEDED-ceiling record would
// be judged by a gate that asks for none of them.
func TestTheA3AndE2xFamiliesAreDisjoint(t *testing.T) {
	others := map[string][]string{
		"uat.FleetConsoleCaseIDs": uat.FleetConsoleCaseIDs,
		"uat.BackgroundCaseIDs":   uat.BackgroundCaseIDs,
		"uat.AdminConsoleCaseIDs": uat.AdminConsoleCaseIDs,
		"uat.FleetCaseIDs":        uat.FleetCaseIDs,
		"uat.ToolApprovalCaseIDs": uat.ToolApprovalCaseIDs,
		"uat.CodeAndShipCaseIDs":  uat.CodeAndShipCaseIDs,
		"uat.ToolsMemoryCaseIDs":  uat.ToolsMemoryCaseIDs,
		"uat.AgentSurfaceCaseIDs": uat.AgentSurfaceCaseIDs,
	}
	for name, ids := range others {
		for _, id := range ids {
			if slices.Contains(uat.ToolExecutionCaseIDs, id) {
				t.Errorf("%s is claimed by BOTH uat.ToolExecutionCaseIDs and %s — PromoteGateFor dispatches on these sets, so an overlap sends this release to a gate that never asks for its superseded-ceiling record", id, name)
			}
		}
	}
}

func assertToolExecutionProofs(t *testing.T, root, id string, want int, got []string) {
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
		if !strings.Contains(string(raw), "func "+name+"(") {
			t.Errorf("%s: proof %q not found in %s — an exact `func <Name>(` is required, so a name appearing only in a comment does not satisfy the reference", id, name, file)
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
// into the generator — the E24/E25/E28 shape. One owner for "what this case proves and at which tier".
func caseAssertion(t *testing.T, id string) string {
	t.Helper()
	entry, ok := expectedToolExecutionCatalog[id]
	if !ok {
		t.Fatalf("%s has no catalog entry to derive its bundle assertion from", id)
	}
	return fmt.Sprintf("%s: proven by %d in-tree proof(s) at the %s tier — %s",
		id, entry.proofs, entry.class, entry.why)
}

// caseProofClass is the same one-owner rule for the bundle's `proof_class` field.
func caseProofClass(t *testing.T, id string) string {
	t.Helper()
	entry, ok := expectedToolExecutionCatalog[id]
	if !ok {
		t.Fatalf("%s has no catalog entry to derive its proof class from", id)
	}
	return entry.class
}
