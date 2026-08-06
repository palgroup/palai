package docs_test

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A SETTING THE CATALOGUE ADVERTISES AND THE DEPLOYMENT CANNOT PASS IS A KNOB WITH NO HANDLE.
//
// Measured 2026-08-06: apps/control-plane/api/deployment.go catalogued 71 variables and
// deploy/compose/compose.yaml carried 36 keys, so 41 of them could not be turned by an operator of the
// shipped deployment at all — including PALAI_PUBLIC_BASE_URL, every artifact-GC and webhook knob, both
// egress gates, and PALAI_WORKSPACE_IDLE_TTL, which Faz A.4's own exit criterion demanded. compose passes
// ONLY the keys it lists: `--env-file` supplies VALUES for interpolation and cannot introduce a name, so
// there is no passthrough an operator can fall back on.
//
// ‼️ THIS IS THE SWEEP THAT DID NOT EXIST, AND THE ONE THAT DID LOOKED THE OTHER WAY. The tree already
// records that nothing walks read→catalogue (149 PALAI_ reads, 36 catalogued). This walks
// catalogue→deployment, which is the direction an OPERATOR experiences: they read the settings screen,
// find the variable, set it in .env.local, and nothing happens.
//
// ‼️ IT MATCHES YAML KEYS AND NEVER GREPS FOR THE NAME. A grep counts a variable named in a COMMENT as a
// passed one, and the first attempt at this number did exactly that — it also used `PALAI_[A-Z_]*`, which
// has no digit class, so every PALAI_S3_* read as absent while two of them sat in the file. The number
// was not wrong; the search was, and the search was invisible. Both mistakes are impossible against a
// parsed key.

// unreachableByDesign are the catalogued settings the shipped compose deliberately does NOT pass. Each
// carries the reason, because an exemption without one is indistinguishable from an oversight — which is
// the state all 41 were in before this file.
var unreachableByDesign = map[string]string{
	"PALAI_WORKSPACE_ROOT": "capabilities.go gates the whole `workspaces` capability on this one variable, " +
		"and no service in compose mounts a workspace tree. Passing it would let an operator switch a " +
		"capability on for a deployment that cannot serve it, so `palai up --native` stays the path.",
	"PALAI_SESSION_ACCOUNT_HELPER": "a privileged HOST path that mints macOS session accounts. A Linux " +
		"container has none, and a host path interpolated into one names a file that is not there.",
	"PALAI_CAPABILITY_WORKER_LISTEN_ADDR": "a SHIPPED release manifest states that no deployment config " +
		"sets it and that no deployed binary serves the capability map. Passing it here would make a " +
		"released sentence false while every test of that sentence stayed green — which is what " +
		"scripts/release's TestNoDeploymentMountsTheCapabilityWorkerGateway caught when this sweep first " +
		"added it. Mounting the gateway is a deliberate change that corrects the release's claim in the " +
		"same commit, never a side effect of making a knob reachable.",
	"PALAI_CAPABILITY_WORKER_IDENTITY_TTL": "it bounds an identity the capability gateway issues, and the " +
		"gateway is unmounted in every deployment by the release claim above. A TTL for a service nothing " +
		"serves is a setting whose only effect would be to suggest the service is there.",
}

func TestEveryCatalogedSettingIsReachableFromTheShippedDeployment(t *testing.T) {
	catalogue := readRepoFile(t, "apps/control-plane/api/deployment.go")
	compose := readRepoFile(t, "deploy/compose/compose.yaml")

	passed := map[string]bool{}
	key := regexp.MustCompile(`^(PALAI_[A-Z0-9_]+):`)
	for _, line := range strings.Split(compose, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if m := key.FindStringSubmatch(trimmed); m != nil {
			passed[m[1]] = true
		}
	}
	if len(passed) == 0 {
		t.Fatal("no PALAI_ key was parsed out of compose.yaml, so this test asserts nothing — the file's " +
			"shape changed and the matcher above did not")
	}

	var unreachable []string
	seenExempt := map[string]bool{}
	for _, m := range regexp.MustCompile(`Name:\s*"(PALAI_[A-Z0-9_]+)"`).FindAllStringSubmatch(catalogue, -1) {
		name := m[1]
		if _, exempt := unreachableByDesign[name]; exempt {
			seenExempt[name] = true
			continue
		}
		if !passed[name] {
			unreachable = append(unreachable, name)
		}
	}
	sort.Strings(unreachable)
	if len(unreachable) > 0 {
		t.Fatalf("%d catalogued setting(s) cannot be set on the shipped compose deployment:\n  %s\n\n"+
			"compose passes only the keys it lists — `--env-file` supplies values and cannot introduce a "+
			"name — so an operator who reads the settings screen, sets one of these and restarts gets no "+
			"change and no error. Add `NAME: ${NAME:-}` to the service that READS it (the empty form is "+
			"behaviour-preserving: every reader here treats empty as absent), or add it to "+
			"unreachableByDesign WITH THE REASON.",
			len(unreachable), strings.Join(unreachable, "\n  "))
	}

	// AND AN EXEMPTION THAT NO LONGER NAMES A REAL SETTING IS ITSELF A STALE CLAIM. A variable deleted from
	// the catalogue leaves its excuse behind, and the next reader takes that excuse for a live decision —
	// the shape this tree paid for when a comment outlived the mechanism it described.
	for name := range unreachableByDesign {
		if !seenExempt[name] {
			t.Errorf("unreachableByDesign exempts %s and the catalogue no longer declares it: the reason "+
				"outlived the setting", name)
		}
	}
}
