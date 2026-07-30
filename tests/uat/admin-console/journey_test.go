//go:build uat

// The E25 T9 EXIT-gate journey entry point. Like the E15 T6 / E16 T8 / E17 T11 / E19 T9 / E20 T5 / E21 T7 /
// E22 T7 / E23 T7 / E24 T8 gates before it, this file is an ORCHESTRATOR rather than a reimplementation: it
// drives `scripts/uat/admin-console`, which runs the narrative where its seams already live.
//
// THE NARRATIVE, in the order §T9 tells it, and every leg is a REAL test in the tree:
//
//	a clean stack, no Slack, zero agents                    auth.spec.ts        the un-configured console serves nothing
//	an unauthenticated write is REFUSED                     auth.spec.ts        401 from EVERY relay method
//	the operator signs in                                   auth.spec.ts        the correct password opens a session
//	an environment is created and keys are written          secret-never-…      and come back in no DOM node, body or map
//	…and rotating one moves the version, unbinding drops it secret-never-…      the binding goes, the bytes stay
//	a repository binding and an agent are created           config-journey      both read back on their own lists
//	…the agent's revision is bound to that environment      config-journey      and PUBLISHED from the console
//	…and RUN from the console, pinned to that revision      config-journey      the revision's model reaches the terminal
//	the agent's SHELL sees those keys                       execution (Go)      a real printenv, public HTTP routes only
//	an MCP connection is registered, discovered, approved   mcp-tools.spec.ts   and pinned into a published set
//	…using only the public API                              execution (Go)      the shipped runbook, executed
//	a gated tool call is DECIDED from the console           approval-queue      the hash comes out of the row's own data
//	…and one whose arguments changed authorizes nothing     approval-queue      the 409 is the binding genuinely failing
//	a past run is read from the list                        observability       and its journal replayed from the record
//	every page is axe-clean                                 a11y.spec.ts        in BOTH colour schemes, from lib/routes.ts
//	no response body carries a secret                       secret-never-…      DOM, bodies and source maps, all zero
//
// THE NARRATIVE IS THE CO-RUN RATHER THAN A SIXTEENTH TEST, and that is a deliberate choice with a reason:
// each leg drives the SHIPPED path at the seam that owns it, and a journey that re-implemented any of them
// would be asserting its own copy — exactly the shape of proof this family exists to refuse. What this file
// adds is the two things a co-run cannot give itself: the assertion that every leg RAN, and the assertion
// that the numbers the BUNDLE carries are the numbers this run PRINTED.
//
// AND THE SECOND HALF IS WHY THIS FILE MATTERS MORE THAN ITS SIBLINGS. E24's ledgers record outcomes of Go
// suites in the same process tree; E25's record outcomes of a BROWSER. So the bundle's relay count, axe
// coverage, sentinel sweep and approval decisions are all diffed here against the lines the specs themselves
// print. A ledger row nobody produced fails the gate.
//
// HONEST CEILING (plan §6 leg 8): every browser leg is a real Chromium against a deterministic fixture
// upstream on ONE box. There is no deployed console, no operator and no screen-reader pass. The REAL-upstream
// half — which is also the only thing that RE-OBSERVES the five repaired ledger rows — is `make
// uat-admin-console-live` and needs a running compose stack.
package adminconsole

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/tests/uat"
)

// TestAdminConsoleJourneyRunsThroughTheOperatorEntryPoint runs the §T9 narrative through the shipped target.
// It is Docker-bound (a throwaway Postgres for the Go legs), so it rides the `uat` tag and never `make
// verify`. It needs NO credential: every leg is deterministic against a fixture upstream and real PostgreSQL.
func TestAdminConsoleJourneyRunsThroughTheOperatorEntryPoint(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available; the E25 Go backing legs need a throwaway Postgres")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "scripts/uat/admin-console")
	cmd.Dir = repoRoot(t)
	cmd.Env = append(cmd.Environ(), "SKIP_JOURNEYS=0")
	raw, err := cmd.CombinedOutput()
	out := string(raw)
	t.Logf("$ scripts/uat/admin-console\n%s", out)
	if err != nil {
		t.Fatalf("the E25 admin-console gate failed: %v", err)
	}

	// EACH LINE IS ONE LEG OF THE NARRATIVE, and the browser legs are matched on the `list` reporter's PASS
	// mark rather than on the title alone. That distinction is the reason scripts/uat/admin-console overrides
	// the config's `line` reporter: `line` prints a title when the test STARTS, so a title in the output
	// proves nothing about whether it passed — and this repository has shipped green-by-omission more than a
	// dozen times. Read the diff, never the selector.
	for _, leg := range []struct{ kind, name, why string }{
		{"browser", "FAIL-CLOSED: a console with no PALAI_CONSOLE_PASSWORD_HASH serves nothing — not a read, not a write, not a sign-in", "CON-001 — the journey starts on a clean stack, and a console with no password hash is not \"logged out\", it is not serving"},
		{"browser", "an unauthenticated client gets 401 from EVERY relay method — including the approval decision surface", "CON-001 — an unauthenticated write is REFUSED, on every method including the one E23 T9 opened"},
		{"browser", "the correct password opens a session; the wrong one is refused in constant time and says nothing about which half", "CON-001 — the operator signs in, through the REAL login route with the REAL password"},
		{"browser", "a write carrying a foreign Origin is refused 403 even WITH a valid session — the CSRF second layer", "CON-001 — the Route Handler gets no framework CSRF for free (§3.5 N8), so the protection given up is written by hand"},
		{"browser", "a value written through the console appears in no DOM node, no response body and no source map", "CON-004 — an environment is created and a key written, and the value comes back nowhere"},
		{"browser", "rotate is the same route as create and it moves the version; unbinding drops the key from the list", "CON-004 — and removing a key removes the BINDING, which is what the dialog says and what secret_refs allows"},
		{"browser", "a repository binding is registered from the console and reads back on the list", "CON-006 — a stranger builds the repository half without writing curl"},
		{"browser", "an agent, a revision bound to an environment, and a publish — all from the console", "CON-006 — the agent lineage, bound to the environment T4 wrote"},
		{"browser", "a run started with a PUBLISHED revision carries that revision's model in its terminal projection", "CON-006 — and the agent is RUN from the console, pinned to that revision"},
		{"browser", "an MCP connection is registered, discovered, approved with a gate, and pinned into a published set", "CON-007 — the tool half, from a screen"},
		{"browser", "the decision carries the request hash the row displayed, and nothing on the page asks for one", "CON-005 — a gated tool call is DECIDED from the console, with the binding out of the row's own data"},
		{"browser", "an approval whose arguments changed is refused as no-longer-decidable, and the screen says which refusal it was", "CON-005 — and one whose arguments changed authorizes nothing"},
		{"browser", "a finished run is READABLE from the history list, and opening it replays the journal it actually wrote", "CON-002 — a past run is read from the list"},
		{"browser", "every route lib/routes.ts declares was actually scanned by axe", "CON-002 — an unscanned page is structurally impossible, in both colour schemes"},
		{"browser", "every exported relay method passes through requireSession — an ungated export is a FAIL", "CON-001 — the gate is TOTAL, counted over every route file rather than over a list"},
		{"browser", "every console request rides the /v1 relay — no privileged backchannel, no direct upstream/DB", "the E17 T10 crown, unchanged: this epic added an identity to a public-API-only console, it did not add a second path"},

		{"go", "TestAnEnvironmentReachesARunsShellAndItsValueEntersNoDurableRow", "CON-003 — an operator-authored value reaches a real shell command on this machine, and enters no durable row"},
		{"go", "TestAnEnvironmentKeyCannotShadowTheSandboxsOwnPATH", "CON-003 — a colliding key is refused BEFORE the process starts; a refusal reported afterwards would already have run the command"},
		{"go", "TestAConsoleWrittenEnvironmentReachesTheShellOfARunItPinned", "CON-006 — the one place T3's pipe and T6's console meet: public HTTP routes only, then a real printenv"},
		{"go", "TestTheJiraRunbookRunsOnThePublicAPIAlone", "CON-007 — the SHIPPED runbook, executed; it stopped at step (c) with a 405 before T7"},
		{"go", "TestAForeignTenantsToolRevisionIsNotFound", "CON-007 — the cross-tenant negative on both new read routes"},
		{"go", "TestOnlyTwoQueriesTouchTheCiphertextColumn", "CON-004 — the SQL layer of the three-layer absence, and the only one that fails before a new query has a caller"},
	} {
		if !legPassed(out, leg.kind, leg.name) {
			t.Errorf("%s did not report PASS (%s) — the suite that BACKS its case did not run in this invocation, so the bundle's authored PASS for it is unbacked", leg.name, leg.why)
		}
	}

	assertLedgersMatchTheRun(t, out)
	assertTheSkipCountIsWhatTheSuiteCanProduce(t, out)
}

// legPassed matches the shipped reporters' PASS marks. Playwright's `list` reporter writes
// `✓  12 [project] › file:line:col › title (34ms)`; `go test -v` writes `--- PASS: TestName`.
func legPassed(out, kind, name string) bool {
	switch kind {
	case "go":
		return strings.Contains(out, "--- PASS: "+name)
	default:
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "✓") && strings.Contains(line, "› "+name+" (") {
				return true
			}
		}
		return false
	}
}

// assertLedgersMatchTheRun is the half a co-run cannot give itself: the bundle's ledgers are AUTHORED data,
// and this diffs them against the lines the specs themselves printed in THIS invocation. A ledger row nobody
// produced fails here.
func assertLedgersMatchTheRun(t *testing.T, out string) {
	t.Helper()

	// (a) the relay gate. tests/relay-gate.spec.ts prints:
	//     RELAY GATE AUDIT — 3 route file(s), 6 exported HTTP method(s), 5 gated:
	relay, gated, doors, err := uat.SweepAdminConsoleRelay(json.RawMessage(uat.AdminConsoleRelayLedger))
	if err != nil {
		t.Fatalf("the canonical relay ledger cannot be swept: %v", err)
	}
	wantRelay := fmt.Sprintf("%d exported HTTP method(s), %d gated", relay+len(doors), gated)
	if !strings.Contains(out, wantRelay) {
		t.Errorf("the run's RELAY GATE AUDIT does not say %q — the bundle's relay ledger records %d relay method(s), %d gated and %d door(s), and nothing in this invocation produced those numbers", wantRelay, relay, gated, len(doors))
	}

	// (b) the axe coverage. tests/a11y.spec.ts prints, once per Playwright project:
	//     AXE ROUTE COVERAGE — 10/10 declared route(s) scanned: …
	declared, scanned, _, err := uat.SweepAdminConsoleRoutes(json.RawMessage(uat.AdminConsoleRouteLedger))
	if err != nil {
		t.Fatalf("the canonical route ledger cannot be swept: %v", err)
	}
	wantAxe := fmt.Sprintf("AXE ROUTE COVERAGE — %d/%d declared route(s) scanned", scanned, declared)
	coverage := strings.Count(out, wantAxe)
	if coverage < len(uat.AdminConsoleColourSchemes) {
		t.Errorf("the run printed %q %d time(s), want at least %d — one per colour scheme, and a single occurrence means the second Playwright project did not run (which is exactly the hole this epic closed: playwright-core defaults colorScheme to \"light\")", wantAxe, coverage, len(uat.AdminConsoleColourSchemes))
	}

	// (c) the sentinel sweep. tests/secret-never-returns.spec.ts prints its three layers, their byte counts,
	// their probes and the finding count. Only the ZERO and the probes are diffed here: the byte counts are a
	// property of the build and would make this test a version pin rather than a measurement.
	if !strings.Contains(out, "sentinel found in 0") {
		t.Error("the run does not report `sentinel found in 0` — the byte-scan ledger's zero was not produced by this invocation")
	}
	var rows []struct {
		Kind  string `json:"kind"`
		Probe string `json:"probe"`
	}
	if err := json.Unmarshal([]byte(uat.AdminConsoleByteScanLedger), &rows); err != nil {
		t.Fatalf("decode the canonical byte-scan ledger: %v", err)
	}
	for _, row := range rows {
		if !strings.Contains(out, "probe="+row.Probe) {
			t.Errorf("the run never names the probe %q for the %q layer — the ledger claims that layer found it, and a layer whose probe nothing produced is a layer nobody has shown was read", row.Probe, row.Kind)
		}
	}

	// (h) the approval decisions. tests/approval-queue.spec.ts prints one APPROVAL DECISION line per decision
	// it drives, and the ledger must be a subset of what ran: every (decision, hash_matched, applied) triple
	// the bundle claims has to appear.
	applied, refused, _, err := uat.SweepAdminConsoleApprovals(json.RawMessage(uat.AdminConsoleApprovalLedger))
	if err != nil {
		t.Fatalf("the canonical approval ledger cannot be swept: %v", err)
	}
	printedApplied := strings.Count(out, "hash_matched=true applied=true")
	printedRefused := strings.Count(out, "hash_matched=false applied=false")
	// The whole suite runs in BOTH colour schemes, so every decision is driven twice.
	schemes := len(uat.AdminConsoleColourSchemes)
	if printedApplied != applied*schemes {
		t.Errorf("the run printed %d applied decision(s) but the bundle's approval ledger records %d (× %d colour schemes = %d) — the count comes from the run, never from the manifest", printedApplied, applied, schemes, applied*schemes)
	}
	if printedRefused != refused*schemes {
		t.Errorf("the run printed %d hash-mismatch refusal(s) but the bundle's approval ledger records %d (× %d colour schemes = %d)", printedRefused, refused, schemes, refused*schemes)
	}
	if strings.Contains(out, "hash_matched=false applied=true") {
		t.Error("the run printed a decision that APPLIED on a mismatched hash — the one-shot binding failed open")
	}
}

// assertTheSkipCountIsWhatTheSuiteCanProduce is §3.5 N15's rule, and the source of the number is the point.
//
// A COUNTER RESTING ON AN UNDOCUMENTED RENDERING IS A COUNTER THAT SILENTLY GOES TO ZERO. Playwright's
// reporters print "N skipped" in a format nothing promises, and a suite that stopped skipping and a reporter
// that stopped saying so are indistinguishable in that output. So this counts the line `tests/profile.ts`
// PRINTS ITSELF — `SKIPPED ON REAL PROFILE — <id> [<kind>] <subject>` — which exists precisely because a
// silent skip is this repository's most-found defect (eight findings, the most recent a security-suite arm
// reported as a denial that had actually skipped).
//
// On the FAKE profile the expected count is ZERO, and asserting zero is a real assertion rather than a
// tautology: skipOnReal is deliberately ONE-DIRECTIONAL, so the fake layer runs everything, and a zero here
// says the whole spec set executed. The EXPECTED REAL count is derived — one line per `skipOnReal(` call site
// per colour scheme — and asserted against a real-upstream run when this invocation contains one.
func assertTheSkipCountIsWhatTheSuiteCanProduce(t *testing.T, out string) {
	t.Helper()
	root := repoRoot(t)

	// The renderer is read from the SOURCE rather than assumed, so a reworded log line fails here instead of
	// making every count below silently zero.
	profile, err := os.ReadFile(filepath.Join(root, "apps", "web-console", "tests", "profile.ts"))
	if err != nil {
		t.Fatalf("read tests/profile.ts: %v", err)
	}
	const marker = "SKIPPED ON REAL PROFILE —"
	if !strings.Contains(string(profile), marker) {
		t.Fatalf("tests/profile.ts no longer prints %q — the counter this gate derives has no source, which is exactly the state §3.5 N15 refuses (a counter resting on an undocumented rendering is a counter that silently goes to zero)", marker)
	}

	sites := skipOnRealCallSites(t, root)
	if sites == 0 {
		t.Fatal("no spec calls skipOnReal at all — \"zero skips on the fake profile\" would then be a statement about a suite that cannot skip")
	}
	schemes := len(uat.AdminConsoleColourSchemes)

	got := strings.Count(out, marker)
	ranOnReal := strings.Contains(out, "PROFILE=real")
	switch {
	case !ranOnReal:
		if got != 0 {
			t.Errorf("the FAKE profile emitted %d %q line(s), want 0 — skipOnReal is one-directional and the fake layer runs everything, so a skip here is a spec declining a profile it cannot decline", got, marker)
		}
		t.Logf("skip count: 0 on the fake profile, over a suite with %d skipOnReal call site(s) × %d colour scheme(s) = %d lines it COULD have printed", sites, schemes, sites*schemes)
	default:
		want := sites * schemes
		if got != want {
			t.Errorf("the REAL profile emitted %d %q line(s), want %d — one per skipOnReal call site (%d) per colour scheme (%d). A count below that means a spec stopped running rather than started passing; above it means a skip moved into a helper or a loop and the derivation is no longer the truth", got, marker, want, sites, schemes)
		}
		t.Logf("skip count: %d on the real profile, derived from %d call site(s) × %d colour scheme(s)", got, sites, schemes)
	}
}

// skipOnRealCallSites counts `skipOnReal(` in the spec files, and REFUSES a call site whose divergence id is
// not a literal — because the derivation "one line per call site" only holds while each call is one execution.
func skipOnRealCallSites(t *testing.T, root string) int {
	t.Helper()
	dir := filepath.Join(root, "apps", "web-console", "tests")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the console spec dir: %v", err)
	}
	literal := regexp.MustCompile(`skipOnReal\(\s*"([A-Z0-9-]+)"\s*\)`)
	any := regexp.MustCompile(`skipOnReal\(`)
	total := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".spec.ts") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		all := len(any.FindAllString(string(body), -1))
		lits := literal.FindAllStringSubmatch(string(body), -1)
		if all != len(lits) {
			t.Errorf("%s has %d skipOnReal call(s) but only %d with a LITERAL divergence id — a computed id, or a call inside a loop or a helper, breaks the one-line-per-call-site derivation this gate rests on", e.Name(), all, len(lits))
		}
		total += len(lits)
	}
	return total
}

// TestARedBackingSuiteFailsTheAdminConsoleGate is the load-bearing negative: the co-run is only worth anything
// if a RED backing suite actually fails this target. It re-runs the operator entry point with
// PALAI_CONSOLE_FAULT_SUITE pointing the execution suite (which holds the CON-003/006/007 Go legs) at a DEAD
// Postgres — a genuinely red suite, its tests failing on connect, not a stubbed-out command — and asserts a
// NON-ZERO exit.
//
// Without this, "the gate co-runs the suites" would be a claim about the script's TEXT rather than its
// behaviour: a swallowed status, a missing `set -e`, or a `|| true` would leave the gate green over a red
// suite. E21 T7 found exactly that hole in its own gate script, twice, as green-by-skip.
func TestARedBackingSuiteFailsTheAdminConsoleGate(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available; the fault run needs the throwaway Postgres the script stands up")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "scripts/uat/admin-console")
	cmd.Dir = repoRoot(t)
	cmd.Env = append(cmd.Environ(), "SKIP_JOURNEYS=0",
		"PALAI_CONSOLE_FAULT_SUITE=./apps/control-plane/internal/execution")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the gate PASSED with the execution backing suite pointed at a dead Postgres — a red backing suite must fail this target, or the co-run certifies nothing\n%s", out)
	}
	if !strings.Contains(string(out), "FAULT INJECTED") {
		t.Errorf("the run does not report the injected fault, so the non-zero exit may be unrelated:\n%s", out)
	}
	if !strings.Contains(string(out), "backing suite ./apps/control-plane/internal/execution FAILED") {
		t.Errorf("the failure is not attributed to the faulted backing suite:\n%s", out)
	}
	t.Logf("a red backing suite failed the gate as required: %v", err)
}
