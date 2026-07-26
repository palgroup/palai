package wiring

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/palgroup/palai/tests/uat"
)

// The CREDENTIAL-GATED LIVE INVENTORY (plan §T9), checked against the tree rather than against itself.
//
// The phase's deliverable is "the owner supplies credentials and ONE command runs every live leg with NO
// code change". That sentence is only auditable if the inventory is written down AND mechanically tied to
// the tests that exist — otherwise it is a promise, and this family's signature failure is a promise that
// reads green because nothing ran (nine findings and counting: a security-suite arm reported as a denial
// that had SKIPPED; three branches that could never fire because their producer never emitted the value
// they tested for).
//
// So this file checks the inventory in BOTH directions:
//
//	table -> tree   every listed leg names a test that EXISTS under a `live` build tag, and that test's
//	                body actually reads every env var the table says it needs
//	tree  -> table  every TestLive* under tests/live/slack and tests/live/a2a is IN the table
//
// and it checks the behaviour the inventory claims: every leg SKIPS (never fails, never passes) when its
// credential is absent, which is what makes a PARTIAL handover report partial-green.

// liveRoots are the directories whose live legs this epic owns. Deliberately narrow: tests/live/provider,
// /repository, /subagents and /workspace are earlier epics' legs with their own gates, and sweeping them in
// here would make this inventory a claim about work E19 did not do.
var liveRoots = []string{
	filepath.Join("tests", "live", "slack"),
	filepath.Join("tests", "live", "a2a"),
}

var (
	funcPattern = regexp.MustCompile(`(?m)^func (TestLive[A-Za-z0-9_]*)\(`)
	// needPattern matches the `need(t, "VAR", …)` helper AND a bare os.Getenv("VAR"), because a leg may
	// gate on either and the inventory has to reflect what the test really reads.
	needPattern = regexp.MustCompile(`(?:need\(t,\s*|os\.Getenv\()"([A-Z0-9_]+)"`)
)

// liveTestSource maps every live test name in the owned roots to the source of the file that declares it.
func liveTestSource(t *testing.T) map[string]string {
	t.Helper()
	root := repoRoot(t)
	out := map[string]string{}
	for _, dir := range liveRoots {
		entries, err := os.ReadDir(filepath.Join(root, dir))
		if err != nil {
			t.Fatalf("read %s (a live root named in the inventory must exist): %v", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
				continue
			}
			path := filepath.Join(root, dir, e.Name())
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			src := string(raw)
			// The build tag is load-bearing: a live leg WITHOUT it would ride `make verify` and either fail
			// the whole suite on a missing credential or, worse, pass having skipped.
			if !strings.Contains(src, "//go:build live") {
				t.Errorf("%s declares live tests but carries no `//go:build live` tag — it would ride make verify", path)
				continue
			}
			for _, m := range funcPattern.FindAllStringSubmatch(src, -1) {
				out[m[1]] = src
			}
		}
	}
	return out
}

// TestLiveInventoryMatchesTheTree is the two-directional check.
func TestLiveInventoryMatchesTheTree(t *testing.T) {
	existing := liveTestSource(t)

	listed := map[string]bool{}
	for _, leg := range uat.WiringLiveLegs {
		listed[leg.Test] = true

		src, ok := existing[leg.Test]
		if !ok {
			t.Errorf("the inventory lists %s but no such live test exists under %v — an inventory that names tests nobody wrote is the promise, not the proof",
				leg.Test, liveRoots)
			continue
		}
		body := testBody(src, leg.Test)
		for _, env := range leg.EnvVars {
			if !strings.Contains(body, `"`+env+`"`) {
				t.Errorf("%s: the inventory says it needs %s but the test body never reads it — the operator would supply a variable that changes nothing",
					leg.Test, env)
			}
		}
		// Every variable the test ACTUALLY gates on must be listed, or the operator supplies a subset and
		// the leg skips for a reason the handover never mentioned.
		for _, env := range readEnvVars(body) {
			if !contains(leg.EnvVars, env) {
				t.Errorf("%s reads %s but the inventory does not list it — a leg that skips on an unlisted variable is an unexplained skip",
					leg.Test, env)
			}
		}
		if leg.HandoverRow == "" || !strings.Contains(leg.HandoverRow, "§0") {
			t.Errorf("%s: the handover row %q does not name a plan §0 row — the operator must be told WHERE to get the value", leg.Test, leg.HandoverRow)
		}
		// The skip contract, in the source: a missing credential must reach t.Skip, never t.Fatal.
		if !strings.Contains(body, "need(t,") && !strings.Contains(body, "t.Skip") {
			t.Errorf("%s neither uses the need() skip helper nor calls t.Skip — the inventory says it SKIPS without a credential, and that has to be true in the code",
				leg.Test)
		}
	}

	var missing []string
	for name := range existing {
		if !listed[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) != 0 {
		t.Errorf("these live tests exist but are NOT in uat.WiringLiveLegs: %v — an inventory the operator reads as complete must be complete, or `make uat-wiring-live` runs work nobody documented",
			missing)
	}
}

// TestLiveInventoryIsWhatTheBundleCarries pins the bundle's copy to the canonical table. The proof carries
// the inventory so a reader of the manifest alone meets it; this is what stops the two from drifting.
func TestLiveInventoryIsWhatTheBundleCarries(t *testing.T) {
	proof := canonicalWiringProof(t)
	if len(proof.LiveLegs) != len(uat.WiringLiveLegs) {
		t.Fatalf("the bundle carries %d live legs, the canonical table has %d", len(proof.LiveLegs), len(uat.WiringLiveLegs))
	}
	for i, leg := range proof.LiveLegs {
		if !sameLeg(leg, uat.WiringLiveLegs[i]) {
			t.Errorf("live leg %d differs from the canonical table:\n bundle: %+v\n table:  %+v", i, leg, uat.WiringLiveLegs[i])
		}
	}
	// EVERY leg skips. This is the design decision the plan names: the owner will supply credentials in
	// pieces, so a partial handover must be partial-green.
	for _, leg := range uat.WiringLiveLegs {
		if leg.WithoutCredential != "skip" {
			t.Errorf("%s declares %q without its credential — a live leg that FAILS turns one absent variable into a red wall, and one that PASSES asserts what it never ran",
				leg.Test, leg.WithoutCredential)
		}
	}
}

// TestEveryOwnedEnvVarComesFromAHandoverRow is the operator's side of the contract: the union of the
// inventory's variables IS the copy-paste list §0 hands over, so a variable that appears in no row is one
// the owner would never know to set.
func TestEveryOwnedEnvVarComesFromAHandoverRow(t *testing.T) {
	seen := map[string]string{}
	for _, leg := range uat.WiringLiveLegs {
		for _, env := range leg.EnvVars {
			seen[env] = leg.HandoverRow
		}
	}
	for env, row := range seen {
		if row == "" {
			t.Errorf("%s is required by a live leg but no handover row supplies it", env)
		}
	}
	names := make([]string, 0, len(seen))
	for env := range seen {
		names = append(names, env)
	}
	sort.Strings(names)
	t.Logf("credential-gated live inventory: %d legs over %d variables: %v", len(uat.WiringLiveLegs), len(names), names)
}

// testBody returns the source from `func <name>(` to the next top-level `func `, which is enough to see
// what one test reads without parsing Go.
func testBody(src, name string) string {
	start := strings.Index(src, "func "+name+"(")
	if start < 0 {
		return ""
	}
	rest := src[start+1:]
	if next := strings.Index(rest, "\nfunc "); next >= 0 {
		return rest[:next]
	}
	return rest
}

// readEnvVars extracts the variables a test body gates on, minus the tuning knobs that are not credentials
// (they have defaults and a handover row would be noise).
func readEnvVars(body string) []string {
	tuning := map[string]bool{
		"PALAI_SLACK_LIVE_TIMEOUT":     true,
		"PALAI_SLACK_LIVE_LISTEN_ADDR": true,
		"PALAI_SLACK_API_BASE_URL":     true,
		"A2A_PUSH_WEBHOOK_TOKEN":       true, // optional companion to the URL; the URL is what gates the leg
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range needPattern.FindAllStringSubmatch(body, -1) {
		if tuning[m[1]] || seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		out = append(out, m[1])
	}
	sort.Strings(out)
	return out
}

// sameLeg compares two inventory rows field by field (a struct holding a slice is not comparable with ==).
func sameLeg(a, b uat.LiveLeg) bool {
	if a.Test != b.Test || a.HandoverRow != b.HandoverRow || a.WithoutCredential != b.WithoutCredential ||
		len(a.EnvVars) != len(b.EnvVars) {
		return false
	}
	for i := range a.EnvVars {
		if a.EnvVars[i] != b.EnvVars[i] {
			return false
		}
	}
	return true
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
