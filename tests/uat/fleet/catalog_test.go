package fleet

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

// The E24 CATALOG GATE. It owns the five case ids this epic opened, and it owns them ALONE — the E17 gate's
// orphan sweep defers to uat.FleetCaseIDs and refuses an id claimed by more than one catalog, so ownership
// moved without lapsing. Everything the E17/E20/E21/E22/E23 catalog gates enforce is enforced here too: the
// dir exists, the id matches the directory, the name is a kebab-case behaviour assertion, the declared class
// is a §10.2 class its proofs actually support, every referenced proof RESOLVES in the tree, and the case
// text names both its LOCAL SEAM and its §6 operator leg.
//
// ONE DELIBERATE DIFFERENCE FROM THE FIVE GATES BEFORE IT, and it is a simplification rather than a
// weakening. Those catalogs retype every proof reference, so the map is a SECOND COPY of each case.yaml's
// list — E23's runs to a hundred and forty lines for five cases, and E24's five cases carry eighty-two
// references between them. What that duplication actually buys is one thing: a case cannot silently DROP a
// proof leg. This gate buys the same thing with an integer. The count is pinned per case, every reference in
// the case's own list is RESOLVED in the tree, and the tier is checked against the build tags those files
// carry — so a dropped leg moves the count, a renamed test fails to resolve, a moved file fails to resolve,
// and an added leg moves the count too. The full list stays in ONE place, which is where a reader edits it.
type fltCase struct {
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
	// dockerBoundClasses are the classes whose //go:build TAG is load-bearing: a case declaring one must
	// reference at least one proof carrying that exact tag, or it claims a backing service it never touched.
	dockerBoundClasses = map[string]bool{"component-real": true, "live-provider": true, "fault-live": true}
	// localSeamWords are the words that tell a reader WHAT produced the evidence. A case that names none of
	// them is the prose half of an overclaim.
	localSeamWords = []string{"fake", "loopback", "deterministic", "live", "component-real", "local proof", "local seam"}
)

// expectedFleetCatalog is the E24 UAT catalog: the five ids this epic materializes (plan §T8) mapped to the
// proof class their case.yaml must declare and HOW MANY in-tree proofs must back it. See the type comment
// above for why the count rather than the list.
//
// EVERY CASE CARRIES BOTH HALVES OF ITS NEGATIVE, which is this family's standing rule: a claim shaped "X
// cannot happen" rots into a vacuous green, so each case's list also names the proof that shows the guard can
// FIRE. FLT-002 carries the bit-unchanged single-runner deployment beside the wrong-pool refusal; FLT-003
// carries the key enrolling into the pool it NAMES beside the one it refuses, and the machine that keeps
// renewing beside the enrolment that is turned away; FLT-004 carries the run left alone beside the run that
// is woken; FLT-005 carries the resume beside the cordon and the unrevoked machine beside the revoked one.
var expectedFleetCatalog = map[string]struct {
	class  string
	proofs int
	why    string
}{
	// T1 — the inventory. The load-bearing pair is the two machines that asked for the SAME name and got two
	// identities, beside the bit-unchanged bootstrap install: without the second, a build that refused every
	// enrolment would satisfy the first.
	"FLT-001": {"component-real", 10, "the registry, the server-minted id, the append-only journal, tenant confinement, and the bootstrap install that is bit-unchanged"},
	// T2 — the pool as a queue, a label and a posture. The bit-unchanged leg is listed for the same reason.
	"FLT-002": {"component-real", 12, "the wrong-pool refusal (two-sided), the chosen FIFO order, the posture compared at the door, and the no-pool-configured deployment that is bit-unchanged"},
	// T3+T6 — the pool key and the waiting room. The three halves of revocation are three separate proofs, and
	// two of them are REGRESSION FENCES rather than features.
	"FLT-003": {"component-real", 32, "scope, hash, constant-time comparison, expiry, the three halves of revocation, the key that is not an API key, the value that reaches no row or log, and strict enrolment with its bit-unchanged default"},
	// T4 — the tenant and the park. The wake and the run left ALONE are both listed: a waker that woke
	// everything would satisfy the first.
	"FLT-004": {"component-real", 14, "the cross-tenant refusal, the park instead of the dead letter, the wake that does not reserve, the other waiting run left alone, and the pool recorded on the run"},
	// T5 — one machine's lifecycle. The restart is the whole claim, and the unrevoked machine still taking a
	// lease through the new process is what keeps it a revocation rather than an outage.
	"FLT-005": {"component-real", 21, "the revocation that survives a restart, the cordon that survives it and the resume that clears it, the irreversible journalled revoke, the heartbeat that advances liveness, the whole-gateway drain over two machines, and the parked-run TTL"},
}

// caseAssertion is the bundle's per-case sentence, DERIVED from this catalog and from the case's own proof
// list so a bundle assertion can never describe a proof count the case does not declare.
func caseAssertion(t *testing.T, id string) string {
	t.Helper()
	entry, ok := expectedFleetCatalog[id]
	if !ok {
		t.Fatalf("%s has no catalog entry to derive its bundle assertion from", id)
	}
	return fmt.Sprintf("%s: proven by %d in-tree proof(s) at the %s tier — %s",
		id, entry.proofs, entry.class, entry.why)
}

// TestFleetCatalogMaterialized is the E24 catalog gate. It rides make verify (no Docker, no credential), so a
// forgotten case, a vanished proof leg, or a case text that hides which seam produced its evidence fails fast.
func TestFleetCatalogMaterialized(t *testing.T) {
	root := repoRoot(t)
	casesDir := filepath.Join(root, "tests", "uat", "cases")

	for id, want := range expectedFleetCatalog {
		raw, err := os.ReadFile(filepath.Join(casesDir, id, "case.yaml"))
		if err != nil {
			t.Errorf("%s: read case.yaml: %v", id, err)
			continue
		}
		var c fltCase
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
			t.Errorf("%s: the case text names no LOCAL seam (want one of %v) — without it a reader cannot tell a fake-peer proof from a real one", id, localSeamWords)
		}
		if !strings.Contains(c.Input, "§6") {
			t.Errorf("%s: the case text names no §6 operator leg — every case must say what real-infrastructure leg it does NOT cover", id)
		}
		assertFleetProofs(t, root, id, want.class, want.proofs, c.Proof)
	}

	// The E24 side of the split ownership: every id this package claims must actually have a directory. The
	// E17 gate covers the other direction (no dir under a guarded prefix escapes every catalog).
	for _, id := range uat.FleetCaseIDs {
		if _, ok := expectedFleetCatalog[id]; !ok {
			t.Errorf("%s is in uat.FleetCaseIDs but this gate resolves no proofs for it — the canonical list and the catalog must be the same set", id)
		}
		if _, err := os.Stat(filepath.Join(casesDir, id, "case.yaml")); err != nil {
			t.Errorf("%s is claimed by this epic but has no case.yaml: %v", id, err)
		}
	}
	for id := range expectedFleetCatalog {
		if !slices.Contains(uat.FleetCaseIDs, id) {
			t.Errorf("%s is cataloged here but is not in uat.FleetCaseIDs, so the E17 orphan guard would report it as an escapee", id)
		}
	}
}

// TestEveryFLTDirectoryIsOwned is the E24 half of the orphan rule, asserted HERE as well as in the E17 gate.
// Two gates checking it is not redundancy: the E17 sweep walks the directory and defers to this epic's list,
// so if somebody deleted the `FLT-` entry from extensionIDPrefixes the sweep would go quiet and stay green —
// which is EXACTLY the state this task found the tree in, because FLT-002 through FLT-005 shipped in T2..T6
// while `FLT-` was in no prefix list at all. This one walks the same directory and cannot be silenced by that
// edit.
func TestEveryFLTDirectoryIsOwned(t *testing.T) {
	casesDir := filepath.Join(repoRoot(t), "tests", "uat", "cases")
	entries, err := os.ReadDir(casesDir)
	if err != nil {
		t.Fatalf("read cases dir: %v", err)
	}
	seen := 0
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "FLT-") {
			continue
		}
		seen++
		if _, ok := expectedFleetCatalog[e.Name()]; !ok {
			t.Errorf("%s is a FLT- case dir that NO catalog resolves proofs for — it escapes proof resolution entirely", e.Name())
		}
	}
	if seen != len(uat.FleetCaseIDs) {
		t.Errorf("the tree holds %d FLT- case dir(s) but this epic claims %d — one of the two is stale", seen, len(uat.FleetCaseIDs))
	}
}

// TestTheFLTPrefixIsGuardedBySweep is the guard for the guard, and it exists because of what this task
// MEASURED rather than as a precaution. T2 through T6 shipped four FLT- case directories while `FLT-` was in
// NO prefix list, so all four escaped the ONLY sweep in the tree that walks tests/uat/cases — four cases with
// proof lists nothing resolved, reported green by silence, which is the identical hole E23 T7 found for
// `HIL-` one epic earlier. It was shown RED here (five directories reported at once) before the ownership
// half was added. The prefix is in extensionIDPrefixes now, and this asserts it stays there, because deleting
// one string from that slice would restore the hole without failing anything else.
func TestTheFLTPrefixIsGuardedBySweep(t *testing.T) {
	path := filepath.Join(repoRoot(t), "tests", "uat", "extensions", "catalog_test.go")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the E17 orphan sweep: %v", err)
	}
	body := string(raw)
	if !strings.Contains(body, `"FLT-"`) {
		t.Error(`extensionIDPrefixes no longer contains "FLT-" — the E17 sweep is the only place in the tree that walks tests/uat/cases, so a prefix outside it is a family whose directories NOTHING checks; ownership may live in uat.FleetCaseIDs, escaping the sweep may not`)
	}
	if !strings.Contains(body, "uat.FleetCaseIDs") {
		t.Error("the E17 orphan sweep does not defer to uat.FleetCaseIDs — with the prefix guarded but the ownership list unknown to it, every FLT- dir would be reported as an escapee and the sweep would be turned off again to silence it")
	}
}

// assertFleetProofs resolves every referenced proof in the tree and refuses a tier overclaim: a case
// declaring component-real must reference at least one proof carrying the `component` build tag, or it is
// claiming a real backing service it never touched.
func assertFleetProofs(t *testing.T, root, id, class string, want int, got []string) {
	t.Helper()
	if len(got) != want {
		t.Errorf("%s: declares %d proof(s), want %d — a case that DROPS a leg is how a negative claim rots into a vacuous green, and one that gains a leg is a catalog nobody updated. Change the count in expectedFleetCatalog deliberately, with the reason", id, len(got), want)
	}
	if len(got) == 0 {
		return
	}
	tiers := map[string]bool{}
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
		if !strings.Contains(body, "func "+name+"(") {
			t.Errorf("%s: proof %q not found in %s (the case claims a proof that is not in the tree)", id, name, file)
		}
		tiers[buildClass(body)] = true
	}
	if dockerBoundClasses[class] && !tiers[class] {
		t.Errorf("%s: declares proof_class %q (a Docker/credential-bound tier) but references no proof carrying the matching //go:build tag — tier overclaim; referenced tiers were %v", id, class, tiers)
	}
	if !dockerBoundClasses[class] && !tiers["unit"] {
		t.Errorf("%s: declares proof_class %q but every referenced proof is Docker/credential-bound (%v) — the declared tier cannot actually run this case's proof", id, class, tiers)
	}
}

// buildClass maps a proof file's //go:build tag to its master-plan §10.2 proof class. A file with no tag runs
// in make verify, so it is the "unit" tier.
func buildClass(body string) string {
	for _, line := range strings.Split(body, "\n") {
		if constraint, ok := strings.CutPrefix(strings.TrimSpace(line), "//go:build "); ok {
			switch strings.TrimSpace(constraint) {
			case "fault":
				return "fault-live"
			case "component":
				return "component-real"
			case "e2e":
				return "e2e-deterministic"
			case "live":
				return "live-provider"
			}
		}
	}
	return "unit"
}
