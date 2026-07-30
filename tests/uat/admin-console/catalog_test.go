package adminconsole

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/palgroup/palai/tests/uat"
)

// THE E25 CATALOG GATE. It owns the `CON-` ids this epic opens, and it owns them ALONE: the E17 gate's orphan
// sweep defers to uat.AdminConsoleCaseIDs and refuses an id claimed by more than one catalog, so ownership
// moved without lapsing.
//
// IT EXISTS FROM T1 RATHER THAN FROM THE EXIT GATE, AND THAT IS A MEASUREMENT RATHER THAN A PREFERENCE. A
// tests/uat/cases/CON-001 directory was created with `CON-` OUTSIDE extensionIDPrefixes and the whole UAT
// tree was run: every package reported ok, including tests/uat/extensions (the only place that walks the
// cases directory) and tests/uat/stable-release (the checksum sweep). A case directory can therefore be
// added, carry claims, and be resolved by NOTHING — reported green by silence. That is exactly the hole E23
// T2's HIL-004 fell into, and the same change that opens CON-001 closes it in both directions: `CON-` joins
// extensionIDPrefixes so no CON- dir escapes the sweep, and this file refuses an id in the canonical list
// that has no directory or whose declared proofs are not in the tree.
//
// PROOF RESOLUTION IS EXACT, NOT SUBSTRING. E25's proofs are Playwright specs, referenced as
// `file.spec.ts:<test title>`, and the existing console-proof resolver in tests/uat/extensions accepts any
// substring match — which a title mentioned in a COMMENT satisfies. Here the reference must resolve to an
// actual `test("<title>"` declaration, because this tree has four shipped instances of a verifier defeated by
// exactly that kind of loose comparison.
//
// NO TIER MOVES HERE. This gate resolves proofs and pins prose; the bundle, the promote gate and the tier
// recomputation are the exit gate's, and `console` stays preview.

type conCase struct {
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
	// is the prose half of an overclaim.
	localSeamWords = []string{"fake", "loopback", "deterministic", "live", "component-real", "local proof", "local seam"}
)

// expectedAdminConsoleCatalog maps each id to the proof class its case.yaml must declare and HOW MANY in-tree
// proofs must back it — the E24 shape rather than a retyped copy of every reference. What the count buys is
// the one thing the duplication bought: a case cannot silently DROP a proof leg. Every reference in the
// case's OWN list is still resolved, so a renamed test, a moved file or a dropped leg all fail.
var expectedAdminConsoleCatalog = map[string]struct {
	class  string
	proofs int
	why    string
}{
	"CON-001": {"e2e-deterministic", 8, "the console refuses every relay method without a session, refuses a foreign-Origin write with one, and serves nothing at all without a password hash"},
	"CON-002": {"e2e-deterministic", 8, "list truncation is stated in text and continued with the server's cursor, every route lib/routes.ts declares is axe-scanned under the WCAG 2.0+2.1+2.2 tags, and five divergence-ledger rows the console's evidence rested on were false"},
}

func TestAdminConsoleCatalogMaterialized(t *testing.T) {
	root := repoRoot(t)
	casesDir := filepath.Join(root, "tests", "uat", "cases")

	for id, want := range expectedAdminConsoleCatalog {
		raw, err := os.ReadFile(filepath.Join(casesDir, id, "case.yaml"))
		if err != nil {
			t.Errorf("%s: read case.yaml: %v", id, err)
			continue
		}
		var c conCase
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
			t.Errorf("%s: the case text names no LOCAL seam (want one of %v) — without it a reader cannot tell a fake-upstream proof from a real one", id, localSeamWords)
		}
		if !strings.Contains(c.Input, "§6") {
			t.Errorf("%s: the case text names no §6 operator leg — every case must say what real-infrastructure leg it does NOT cover", id)
		}
		assertConsoleProofs(t, root, id, want.proofs, c.Proof)
	}

	// The E25 side of the split ownership: every id this package claims must actually have a directory, and
	// the canonical list and this catalog must be the SAME SET. The E17 gate covers the other direction (no
	// CON- dir escapes every catalog).
	for _, id := range uat.AdminConsoleCaseIDs {
		if _, ok := expectedAdminConsoleCatalog[id]; !ok {
			t.Errorf("%s is in uat.AdminConsoleCaseIDs but this gate resolves no proofs for it — the canonical list and the catalog must be the same set", id)
		}
		if _, err := os.Stat(filepath.Join(casesDir, id, "case.yaml")); err != nil {
			t.Errorf("%s is claimed by this epic but has no case.yaml: %v", id, err)
		}
	}
	for id := range expectedAdminConsoleCatalog {
		if !slices.Contains(uat.AdminConsoleCaseIDs, id) {
			t.Errorf("%s is cataloged here but is not in uat.AdminConsoleCaseIDs, so the E17 orphan guard would report it as an escapee", id)
		}
	}

	// And the third direction, which is the one that was MEASURED to be silent: a CON- directory on disk that
	// this epic has not claimed. Without this, adding tests/uat/cases/CON-002 and forgetting the list leaves a
	// case nothing resolves.
	entries, err := os.ReadDir(casesDir)
	if err != nil {
		t.Fatalf("read cases dir: %v", err)
	}
	seen := 0
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "CON-") {
			seen++
			if !slices.Contains(uat.AdminConsoleCaseIDs, e.Name()) {
				t.Errorf("%s is a CON- case directory that uat.AdminConsoleCaseIDs does not claim — a case dir under an unclaimed prefix was measured to leave every UAT gate green", e.Name())
			}
		}
	}
	if seen != len(uat.AdminConsoleCaseIDs) {
		t.Errorf("the tree holds %d CON- case dir(s) but this epic claims %d — one of the two is stale", seen, len(uat.AdminConsoleCaseIDs))
	}
}

// TestTheCONPrefixIsInsideTheOrphanSweep is the standing dependency this file has on another package's
// variable, asserted rather than assumed. tests/uat/extensions is the ONLY place in the tree that walks
// tests/uat/cases, so a `CON-` removed from extensionIDPrefixes would silently stop every CON- directory from
// being swept for ownership — and this gate would keep reporting green about the ids it happens to know.
func TestTheCONPrefixIsInsideTheOrphanSweep(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "tests", "uat", "extensions", "catalog_test.go"))
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
	if !strings.Contains(line, `"CON-"`) {
		t.Errorf(`extensionIDPrefixes no longer contains "CON-": %s

The E17 sweep is the only place in the tree that walks tests/uat/cases, so a prefix outside it is a family
whose directories NOTHING checks. Ownership may live in uat.AdminConsoleCaseIDs; escaping the sweep may not.`, strings.TrimSpace(line))
	}
}

// assertConsoleProofs pins the leg COUNT and resolves every reference. A Playwright reference must resolve to
// a real `test("<title>"` declaration — not merely to the title appearing somewhere in the file, which a
// comment satisfies and which is how a verifier in this tree has been defeated before.
func assertConsoleProofs(t *testing.T, root, id string, want int, got []string) {
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
			// The declaration, not the mention. Both quote styles, because a spec may use either.
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
