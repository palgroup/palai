package background

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

// THE E26 CATALOG GATE. It owns the `BGT-` ids this epic opens, and it owns them ALONE: the E17 gate's orphan
// sweep defers to uat.BackgroundCaseIDs and refuses an id claimed by more than one catalog, so ownership moved
// without lapsing.
//
// IT CLOSES A HOLE THAT WAS OPEN FOR FIVE TASKS, IN BOTH DIRECTIONS, AND THE TWO DIRECTIONS FOUND DIFFERENT
// THINGS. Outward: `BGT-` was in NO prefix list while BGT-001, BGT-002, BGT-004 and BGT-005 shipped, so all
// four case directories escaped tests/uat/extensions — the ONLY sweep in the tree that walks tests/uat/cases —
// and every UAT package reported ok. Adding the prefix was shown RED first, four directories at once. Inward:
// BGT-003's directory DID NOT EXIST. T3 shipped the park, the response state machine's first production write
// in this repository's history and nine proofs, and opened no case for them — which no directory sweep could
// ever have caught, because a MISSING directory is invisible to a walk over directories. The canonical list is
// the authority for that direction and this file enforces it.
//
// NO TIER MOVES HERE. This gate resolves proofs and pins prose; the bundle, the promote gate and the tier
// recomputation are the exit gate's, and every tier stays exactly where E25 left it.

type bgtCase struct {
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
	localSeamWords = []string{"fake", "loopback", "deterministic", "live", "component-real", "local proof", "local seam", "kernel"}
)

// expectedBackgroundCatalog maps each id to the proof class its case.yaml must declare and HOW MANY in-tree
// proofs must back it — the E24/E25 shape rather than a retyped copy of every reference. What the count buys
// is that a case cannot silently DROP a proof leg; every reference in the case's OWN list is still resolved,
// so a renamed test, a moved file or a dropped leg all fail.
//
// EVERY CASE HERE IS `component-real` AND THAT IS THE EPIC'S SHAPE RATHER THAN A CONVENIENCE. There is nothing
// browser-tier and nothing live-provider in background execution: every claim is about a real process, a real
// container, a real PostgreSQL or a real reconciler, and the two that are not about a process are about a
// durable row. A case here declaring `e2e-deterministic` would be claiming an httptest server proved something
// about the operating system.
var expectedBackgroundCatalog = map[string]struct {
	class  string
	proofs int
	why    string
}{
	"BGT-001": {"component-real", 18, "process ownership moves from the CALL to the RUN in both postures — the OCI driver's unconditional defer-remove and the host executor's context-bound process-GROUP kill are each measured as they stand before either changes, `dockerDriver.Run` is byte-unchanged against a committed sha256, the detached container's hardening is proven IDENTICAL structurally by go/ast rather than by reading it, and a handle whose recorded start time does not match the live process is `lost` and receives NO signal"},
	"BGT-002": {"component-real", 14, "the tool returns a HANDLE while the process still runs and carries no output, the output is read mid-flight by the file tool that already reads files (no fourth output mechanism, which is the vendor's own conclusion shipped as a deprecation), THE MODEL CALLS ANOTHER TOOL WHILE THE PROCESS IS STILL RUNNING AND THAT CALL COMPLETES, three tasks run at once without interleaving, the approval gate and the kill switch each start zero processes with their own non-vacuity half, and both credential landing sites are masked from one place"},
	"BGT-003": {"component-real", 9, "a run with a live task PARKS instead of completing — governed by the operating system's answer rather than by a row — a failed terminal does not park and an unprovable handle does not either, and a response reads `waiting_for_tool` for the first time in this repository's history: ResponseTable had no production caller, `in_progress` and `provisioning` were both unreachable, and a published schema had advertised all three for three years"},
	"BGT-004": {"component-real", 6, "an exit calls the model back EXACTLY ONCE across two reconciler ticks, two concurrent control planes and a crash-restart, a run that never parked is not interrupted (the command is enqueued either way and the wake happens only if the run is waiting, in one transaction), an already-terminal run's notice is STAMPED rather than queued or dropped, and on a stack with PALAI_DISPATCH_WORKERS=0 no notification ever lands — pinned as a declaration rather than left as a surprise"},
	"BGT-005": {"component-real", 11, "the reaper enforces a wall-clock ceiling from a COLUMN rather than a context, a cancelled run kills every live task it started (which it did nowhere before), a restarted control plane ADOPTS its rows rather than losing them to memory, an unprovable handle receives no signal, the sixth task of a run is refused rather than queued with the count taken from the database and the process table, and a settled task's log is collected before `.palai-session` grows unbounded where the snapshot would never show it"},
}

func TestBackgroundCatalogMaterialized(t *testing.T) {
	root := repoRoot(t)
	casesDir := filepath.Join(root, "tests", "uat", "cases")

	for id, want := range expectedBackgroundCatalog {
		raw, err := os.ReadFile(filepath.Join(casesDir, id, "case.yaml"))
		if err != nil {
			t.Errorf("%s: read case.yaml: %v", id, err)
			continue
		}
		var c bgtCase
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
			t.Errorf("%s: the case text names no LOCAL seam (want one of %v) — without it a reader cannot tell a real-machine proof from a fabricated one", id, localSeamWords)
		}
		if !strings.Contains(c.Input, "§6") {
			t.Errorf("%s: the case text names no §6 operator leg — every case must say what real-infrastructure leg it does NOT cover", id)
		}
		// EVERY E26 CASE MUST SAY THE TIER SENTENCE, because it is the one claim a reader will assume the
		// opposite of: a feature this visible looks like a capability, and it is not one.
		if !strings.Contains(strings.ToLower(c.Input), "no tier advances") {
			t.Errorf("%s: the case text does not say NO TIER ADVANCES — making a tool asynchronous is not evidence about the plane that tool runs on, and a case that omits it invites the reading this whole epic refuses", id)
		}
		assertBackgroundProofs(t, root, id, want.proofs, c.Proof)
	}

	// The E26 side of the split ownership: every id this package claims must actually have a directory, and
	// the canonical list and this catalog must be the SAME SET. The E17 gate covers the other direction.
	for _, id := range uat.BackgroundCaseIDs {
		if _, ok := expectedBackgroundCatalog[id]; !ok {
			t.Errorf("%s is in uat.BackgroundCaseIDs but this gate resolves no proofs for it — the canonical list and the catalog must be the same set", id)
		}
		if _, err := os.Stat(filepath.Join(casesDir, id, "case.yaml")); err != nil {
			t.Errorf("%s is claimed by this epic but has no case.yaml: %v — this is the direction that found BGT-003 missing after T3 shipped nine proofs for it", id, err)
		}
	}
	for id := range expectedBackgroundCatalog {
		if !slices.Contains(uat.BackgroundCaseIDs, id) {
			t.Errorf("%s is cataloged here but is not in uat.BackgroundCaseIDs, so the E17 orphan guard would report it as an escapee", id)
		}
	}

	// And the third direction, the one that WAS silent for four tasks: a BGT- directory on disk that this
	// epic has not claimed.
	entries, err := os.ReadDir(casesDir)
	if err != nil {
		t.Fatalf("read cases dir: %v", err)
	}
	seen := 0
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "BGT-") {
			seen++
			if !slices.Contains(uat.BackgroundCaseIDs, e.Name()) {
				t.Errorf("%s is a BGT- case directory that uat.BackgroundCaseIDs does not claim — four such directories were measured to leave every UAT gate green", e.Name())
			}
		}
	}
	if seen != len(uat.BackgroundCaseIDs) {
		t.Errorf("the tree holds %d BGT- case dir(s) but this epic claims %d — one of the two is stale", seen, len(uat.BackgroundCaseIDs))
	}
}

// TestTheBGTPrefixIsInsideTheOrphanSweep is the standing dependency this file has on another package's
// variable, asserted rather than assumed. tests/uat/extensions is the ONLY place in the tree that walks
// tests/uat/cases, so a `BGT-` removed from extensionIDPrefixes would silently stop every BGT- directory from
// being swept for ownership — and this gate would keep reporting green about the ids it happens to know.
func TestTheBGTPrefixIsInsideTheOrphanSweep(t *testing.T) {
	line := extensionPrefixLine(t)
	if !strings.Contains(line, `"BGT-"`) {
		t.Errorf(`extensionIDPrefixes no longer contains "BGT-": %s

The E17 sweep is the only place in the tree that walks tests/uat/cases, so a prefix outside it is a family
whose directories NOTHING checks. Ownership may live in uat.BackgroundCaseIDs; escaping the sweep may not.`, strings.TrimSpace(line))
	}
}

// TestTheBGTPrefixCollidesWithNoOtherFamily is the DIRECTION the plan asserted in prose and this gate
// re-derives from the tree: `BGT-` must be the prefix of every id this epic owns and of NO id any other family
// owns, and the prefix set on disk is counted rather than quoted.
//
// THE PLAN'S NUMBER WAS RIGHT AND IS NOW STALE, WHICH IS EXACTLY WHY IT IS RE-COUNTED. §T7 listed
// thirty-three prefixes counted on 2026-07-30 and said `BGT-` collided with none of them. It still collides
// with none of them — and there are now THIRTY-FOUR, because this epic's own four case directories made
// `BGT-` the thirty-fourth. A number written without its command is a number the next reader cannot tell
// apart from a wrong one.
func TestTheBGTPrefixCollidesWithNoOtherFamily(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join(repoRoot(t), "tests", "uat", "cases"))
	if err != nil {
		t.Fatalf("read the cases dir: %v", err)
	}
	prefixes := map[string]bool{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name, _, ok := strings.Cut(e.Name(), "-")
		if !ok || name == "" {
			t.Errorf("case directory %q has no <PREFIX>-<n> shape, so it belongs to no family at all", e.Name())
			continue
		}
		prefixes[name] = true
	}
	if !prefixes["BGT"] {
		t.Fatal("no BGT- case directory exists at all — this gate is judging a family with no cases")
	}
	if len(prefixes) < 30 {
		t.Fatalf("only %d case-id prefixes were found on disk — the walk is not reading tests/uat/cases, so the disjointness claim below is a claim over nothing", len(prefixes))
	}
	t.Logf("case-id prefixes on disk: %d (plan §T7 counted 33 on 2026-07-30, before this epic's own BGT- directories)", len(prefixes))

	// The collision itself, in the direction that matters: no id owned by ANOTHER family may start with BGT-,
	// and no id this epic owns may start with anything else.
	others := map[string][]string{
		"uat.AgentSurfaceCaseIDs": uat.AgentSurfaceCaseIDs,
		"uat.ToolsMemoryCaseIDs":  uat.ToolsMemoryCaseIDs,
		"uat.CodeAndShipCaseIDs":  uat.CodeAndShipCaseIDs,
		"uat.ToolApprovalCaseIDs": uat.ToolApprovalCaseIDs,
		"uat.FleetCaseIDs":        uat.FleetCaseIDs,
		"uat.AdminConsoleCaseIDs": uat.AdminConsoleCaseIDs,
	}
	for name, ids := range others {
		for _, id := range ids {
			if strings.HasPrefix(id, "BGT-") {
				t.Errorf("%s holds %s — a BGT- id owned by another family would make PromoteGateFor dispatch this epic's bundle to that family's gate, or that family's bundle to this one", name, id)
			}
			if slices.Contains(uat.BackgroundCaseIDs, id) {
				t.Errorf("%s and uat.BackgroundCaseIDs both claim %s — two owners is no owner", name, id)
			}
		}
	}
	for _, id := range uat.BackgroundCaseIDs {
		if !strings.HasPrefix(id, "BGT-") {
			t.Errorf("%s is in uat.BackgroundCaseIDs but is not a BGT- id — the family marker PromoteGateFor dispatches on would then match a prefix another gate owns", id)
		}
	}
}

// extensionPrefixLine reads the declaration line of extensionIDPrefixes out of the E17 gate's source.
func extensionPrefixLine(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "tests", "uat", "extensions", "catalog_test.go"))
	if err != nil {
		t.Fatalf("read the E17 catalog gate: %v", err)
	}
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "var extensionIDPrefixes ") {
			return l
		}
	}
	t.Fatal("extensionIDPrefixes is no longer declared in tests/uat/extensions/catalog_test.go — the orphan sweep this gate depends on has moved or gone")
	return ""
}

// assertBackgroundProofs pins the leg COUNT and resolves every reference against a real `func Name(`
// declaration. E26's proofs are all Go tests, so there is no spec-title branch here — and the resolution is a
// declaration rather than a mention, because a name that appears only in a comment is how a verifier in this
// tree has been defeated before.
func assertBackgroundProofs(t *testing.T, root, id string, want int, got []string) {
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
			t.Errorf("%s: proof %q is not a DECLARED test in %s — the case claims a proof that is not in the tree. BGT-002 carried exactly this for two tasks: it named a test T5 had DELETED along with the in-memory registry it guarded, which is the same family of hole as a `-run` allow-list that does not name a test", id, name, file)
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
// into evidence_background.go — the E24/E25 shape. One owner for "what this case proves and at which tier".
func caseAssertion(t *testing.T, id string) string {
	t.Helper()
	entry, ok := expectedBackgroundCatalog[id]
	if !ok {
		t.Fatalf("%s has no catalog entry to derive its bundle assertion from", id)
	}
	return fmt.Sprintf("%s: proven by %d in-tree proof(s) at the %s tier — %s",
		id, entry.proofs, entry.class, entry.why)
}

// caseProofClass is the same one-owner rule for the bundle's `proof_class` field.
func caseProofClass(t *testing.T, id string) string {
	t.Helper()
	entry, ok := expectedBackgroundCatalog[id]
	if !ok {
		t.Fatalf("%s has no catalog entry to derive its proof class from", id)
	}
	return entry.class
}
