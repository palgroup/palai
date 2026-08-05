package agentsurface

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/palgroup/palai/tests/uat"
)

// THE CREDENTIAL-GATED LIVE INVENTORY (plan §T5), EXTENDED FROM E19'S TABLE AND COLLAPSED TO ONE COMMAND.
//
// E20 adds no second inventory. uat.WiringLiveLegs is the canonical table and E20 T1's four legs were
// registered in it as they were written; what this file adds is the E20 half of the audit — that those legs
// exist, that they SKIP without their credentials, and that `make uat-agent-surface-live` actually runs
// them. A second table would have been a second thing to forget.
//
// WHY THE ONE-COMMAND CLAIM NEEDS A TEST AT ALL: "the owner supplies credentials and one command runs every
// live leg with no code change" is a promise, and this family's signature failure is a promise that reads
// green because nothing ran. So the target is READ and its live invocation checked against the roots the
// legs live in.

// agentSurfaceLiveLegs are the legs E20 CONTRIBUTED, by name. They are a SUBSET of uat.WiringLiveLegs — the
// canonical table — and this list exists so the epic's own contribution can be audited without splitting
// ownership of the inventory.
// E20 CONTRIBUTED FOUR AND THREE REMAIN. The status leg was withdrawn 2026-08-05 with the mechanism it
// measured — see the note in uat.WiringLiveLegs where the canonical row was removed, and
// evidence/superseded/slk-009-setstatus-2026-08-05.json. It is dropped from BOTH lists rather than only the
// canonical one, because a subset that names a leg the canonical table no longer carries fails the loop
// below in the direction that reads as a registration bug rather than as a withdrawal.
var agentSurfaceLiveLegs = []string{
	"TestLiveSlackStreamingWorksFromASocketModeApp",
	"TestLiveSlackUnstoppedStreamIsMeasured",
	"TestLiveSlackStreamRefusesWithoutARecipient",
}

// TestTheEpicsLiveLegsAreInTheCanonicalInventory closes the loop in both directions: every leg E20 claims is
// in the canonical table, and every leg in the canonical table SKIPS without its credential. The second half
// is the design decision the plan names — the owner supplies credentials in PIECES, so a partial handover
// must report partial-green rather than a red wall.
func TestTheEpicsLiveLegsAreInTheCanonicalInventory(t *testing.T) {
	byName := map[string]uat.LiveLeg{}
	for _, leg := range uat.WiringLiveLegs {
		byName[leg.Test] = leg
		if leg.WithoutCredential != "skip" {
			t.Errorf("%s declares %q without its credential — a leg that FAILS turns one absent variable into a red wall, and one that PASSES asserts what it never ran",
				leg.Test, leg.WithoutCredential)
		}
	}
	for _, name := range agentSurfaceLiveLegs {
		leg, ok := byName[name]
		if !ok {
			t.Errorf("%s is an E20 live leg but is not in uat.WiringLiveLegs — an inventory the operator reads as complete must be complete", name)
			continue
		}
		if !strings.Contains(leg.HandoverRow, "§0") {
			t.Errorf("%s: the handover row %q names no plan §0 row — the operator must be told WHERE to get the value", name, leg.HandoverRow)
		}
	}
}

// TestEveryLiveLegExistsAndSkips checks the table against the TREE rather than against itself: every listed
// test must exist under a `live` build tag (without it the leg would ride make verify and either fail on a
// missing credential or, worse, pass having skipped), and its body must read every variable the table says
// it needs.
func TestEveryLiveLegExistsAndSkips(t *testing.T) {
	root := repoRoot(t)
	sources := map[string]string{}
	for _, dir := range []string{filepath.Join("tests", "live", "slack"), filepath.Join("tests", "live", "a2a")} {
		entries, err := os.ReadDir(filepath.Join(root, dir))
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(root, dir, e.Name()))
			if err != nil {
				t.Fatalf("read %s/%s: %v", dir, e.Name(), err)
			}
			body := string(raw)
			if !strings.Contains(body, "//go:build live") {
				t.Errorf("%s/%s declares live tests but carries no `//go:build live` tag — it would ride make verify", dir, e.Name())
				continue
			}
			for _, m := range regexp.MustCompile(`(?m)^func (TestLive[A-Za-z0-9_]*)\(`).FindAllStringSubmatch(body, -1) {
				sources[m[1]] = body
			}
		}
	}
	for _, leg := range uat.WiringLiveLegs {
		src, ok := sources[leg.Test]
		if !ok {
			t.Errorf("the inventory lists %s but no such live test exists — an inventory that names tests nobody wrote is the promise, not the proof", leg.Test)
			continue
		}
		start := strings.Index(src, "func "+leg.Test+"(")
		body := src[start:]
		if next := strings.Index(body[1:], "\nfunc "); next >= 0 {
			body = body[:next]
		}
		for _, env := range leg.EnvVars {
			if !strings.Contains(body, `"`+env+`"`) {
				t.Errorf("%s: the inventory says it needs %s but the test body never reads it — the operator would supply a variable that changes nothing", leg.Test, env)
			}
		}
		if !strings.Contains(body, "need(t,") && !strings.Contains(body, "t.Skip") {
			t.Errorf("%s neither uses the need() skip helper nor calls t.Skip — the inventory says it SKIPS without a credential, and that has to be true in the code", leg.Test)
		}
	}
}

// TestOneCommandRunsEveryLiveLeg is the §T5 deliverable, checked rather than promised: `make
// uat-agent-surface-live` must exist, must run the live tier, and that tier must cover the roots the legs
// live in. Without this the "one command" sentence is prose in a plan.
func TestOneCommandRunsEveryLiveLeg(t *testing.T) {
	root := repoRoot(t)
	makefile, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("read the Makefile: %v", err)
	}
	if !strings.Contains(string(makefile), "uat-agent-surface-live:") {
		t.Fatal("the Makefile declares no `uat-agent-surface-live` target — the plan's one-command live inventory does not exist")
	}
	if !strings.Contains(string(makefile), ".PHONY") || !strings.Contains(string(makefile), "uat-agent-surface-live") {
		t.Error("uat-agent-surface-live is not declared .PHONY")
	}

	script, err := os.ReadFile(filepath.Join(root, "scripts", "uat", "agent-surface"))
	if err != nil {
		t.Fatalf("read scripts/uat/agent-surface: %v", err)
	}
	body := string(script)
	if !strings.Contains(body, "RUN_LIVE") {
		t.Error("the agent-surface script has no RUN_LIVE tier, so `make uat-agent-surface-live` would run no live leg at all")
	}
	if !strings.Contains(body, "-tags=live") {
		t.Error("the script's live tier does not build with -tags=live, so every credential-gated leg is excluded from the binary")
	}
	// BOTH roots, because the canonical inventory spans both: E20's four legs are Slack's, but
	// uat.WiringLiveLegs also carries an A2A leg, and "one command runs every live leg" is only true if the
	// command reaches the package that leg lives in.
	for _, root := range []string{"./tests/live/slack", "./tests/live/a2a"} {
		if !strings.Contains(body, root) {
			t.Errorf("the script's live tier does not name %s — a leg in the canonical inventory would never run, and the one-command claim would be an overclaim", root)
		}
	}
	// -v matters: a SKIP is only visible with it, and a green report over legs that never ran is precisely
	// the failure this whole file exists to prevent.
	if !strings.Contains(body, "-v ./tests/live/") && !strings.Contains(body, "-count=1 -v") {
		t.Error("the live tier does not run with -v, so a SKIP would be invisible and a partial handover would read as green")
	}

	names := map[string]bool{}
	for _, leg := range uat.WiringLiveLegs {
		for _, env := range leg.EnvVars {
			names[env] = true
		}
	}
	vars := make([]string, 0, len(names))
	for env := range names {
		vars = append(vars, env)
	}
	sort.Strings(vars)
	t.Logf("one command (`make uat-agent-surface-live`) covers %d legs over %d variables: %v",
		len(uat.WiringLiveLegs), len(vars), vars)
}
