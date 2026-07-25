//go:build security

// Package escape is SEC-102: the sandbox-escape suite (E18 T7).
//
// It INVENTS NO ESCAPE CLASS. Every denial it runs was already written and already proven — SAN-001
// through SAN-008 and the SAN-011 negatives, each living in the tier that can actually prove it (a
// real OCI sandbox, a real memory cgroup, a real host kill, real Postgres). What did not exist was a
// SINGLE PASS over them with a SINGLE report, so "no escape" was a claim assembled by hand out of
// seven separate green runs. This harness runs them as one suite and writes one report.
//
// It adds exactly one thing the corpus was missing: a finding -> QUARANTINE behaviour arm. The corpus
// proves denials — what happens when the boundary holds. It never proved what happens when the
// boundary's own machinery fails in a way that leaves the outcome UNCERTAIN. The two uncertain-failure
// quarantine seams answer that, and the suite drives both:
//
//   - E10 substrate: a failed workspace destroy quarantines the host and DENIES the next placement
//     (that is SAN-008, already in the corpus — the suite runs it as an arm and names it as the
//     substrate half of the quarantine claim rather than just another denial).
//   - E17 T9 job: an uncertain worker outcome quarantines the job, and NO worker re-claims it
//     (apps/control-plane/internal/workers/quarantine_component_test.go — the consequence WRK-007
//     recorded but never asserted).
//
// So the report says both halves: no escape, AND quarantine works.
//
// HONEST CEILING, stated where a reader meets the result (it is printed into the report itself):
// this is the LOCAL OCI seam. The microVM / managed high-isolation path is the SaaS plan and is
// "managed-scope, not claimed" (plan §T7/§T10), and kernel-exploit research is out of scope. The
// suite proves the DENIAL and QUARANTINE mechanics — not the absence of all escapes.
package escape

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// arm is one member of the escape suite: the SAN case ids it discharges and the command that runs
// their proofs. The command is the tier's OWN runner wherever the proof needs one — a real OCI
// fixture image, a real cgroup, a real Postgres — so the suite inherits each tier's container
// accounting and leak checks instead of re-implementing them.
type arm struct {
	Name    string   `json:"name"`
	Cases   []string `json:"cases"`
	What    string   `json:"what"`
	Command []string `json:"command"`
	Env     []string `json:"env,omitempty"`
	// Proved is filled in by the run: the tests whose `--- PASS:` line was actually seen. It is what
	// separates "denied" from "not attempted" in the report.
	Proved []string `json:"proved,omitempty"`
}

// requiredTests is the set of test names an arm's -run regex NAMES, derived from the regex itself
// rather than from a hand-kept second list that can drift away from it. Every arm binds to a plain
// alternation of literal Go test names, either as a `-run` argument or as PALAI_SUITE_RUN.
//
// This exists because `go test -run TestNoSuchThing ./pkg/` prints "ok ... [no tests to run]" and
// EXITS 0. Passed was `err == nil`, so the day someone renamed a test the suite would report
// no_escape=true quarantine_works=true having run absolutely nothing — the single value proposition
// of an aggregation harness, hollowed out silently.
func (a arm) requiredTests() []string {
	var pattern string
	for i, arg := range a.Command {
		if arg == "-run" && i+1 < len(a.Command) {
			pattern = a.Command[i+1]
		}
	}
	for _, e := range a.Env {
		if rest, ok := strings.CutPrefix(e, "PALAI_SUITE_RUN="); ok {
			pattern = rest
		}
	}
	if pattern == "" {
		return nil
	}
	return strings.Split(pattern, "|")
}

// missingProofs returns the tests an arm named but whose PASS line never appeared in its output, plus
// a note when go test reported it had nothing to run at all.
func (a arm) missingProofs(output string) (proved, missing []string) {
	for _, name := range a.requiredTests() {
		switch {
		case strings.Contains(output, "--- SKIP: "+name+" "):
			missing = append(missing, name+" (SKIPPED — a skip is not a denial)")
		case strings.Contains(output, "--- PASS: "+name+" "):
			proved = append(proved, name)
		default:
			missing = append(missing, name+" (no `--- PASS:` line — renamed, filtered out, or never compiled)")
		}
	}
	if strings.Contains(output, "no tests to run") {
		missing = append(missing, "go test reported `no tests to run` — the -run regex matched nothing and still exited 0")
	}
	return proved, missing
}

// suite is the whole corpus. Adding a SAN case without adding it here fails
// TestEveryMaterializedSANCaseIsInTheSuite — a denial nobody runs is not a denial anyone has proven.
var suite = []arm{
	{
		Name:  "file-tool-confinement",
		Cases: []string{"SAN-001"},
		What:  "path traversal, an absolute path and an escaping symlink are all denied outside the allocation root",
		Command: []string{"go", "test", "-count=1", "-v", "-run", "TestFileToolDeniesWorkspaceEscape",
			"./apps/control-plane/internal/execution/tools"},
	},
	{
		Name:    "oci-sandbox-isolation",
		Cases:   []string{"SAN-002", "SAN-004"},
		What:    "inside a REAL hardened OCI sandbox: argv form, unprivileged uid, no runtime socket, process-group kill, and a denied metadata-endpoint dial that emits a finding",
		Command: []string{"bash", "scripts/test/security"},
		Env: []string{"TEST=sandbox",
			"PALAI_SUITE_RUN=TestShellToolArgvFormSandboxUserLimitsProcessGroupKill|TestSandboxDeniesMetadataEgress"},
	},
	{
		Name:    "cgroup-resource-exhaustion",
		Cases:   []string{"SAN-003"},
		What:    "a memory-exhausting command is OOM-killed by the REAL memory cgroup with bounded termination and recorded usage",
		Command: []string{"bash", "scripts/test/fault"},
		Env:     []string{"TEST=sandbox", "PALAI_SUITE_RUN=TestResourceExhaustionBoundedTerminationUsageRecorded"},
	},
	{
		Name:  "snapshot-integrity-and-secret-exclusion",
		Cases: []string{"SAN-005"},
		What:  "a snapshot restore reproduces the create-side checksums byte for byte and the secret/credential never enter the archive bytes",
		Command: []string{"go", "test", "-count=1", "-v", "-run", "TestSnapshotRestoreChecksumsMatchCreate",
			"./adapters/sandboxes/oci/snapshot"},
	},
	{
		Name:  "host-kill-fences-stale-writer",
		Cases: []string{"SAN-006"},
		What: "a REAL host kill advances the fence and the old host's later write/snapshot is rejected AT THE DATABASE. " +
			"The recovery tier pins its own PALAI_FAULT_RUN, so this arm also carries E10 T7's uncertain-TOOL " +
			"reconcile leg for free — the third uncertain-failure seam: irreversible effects escalate to " +
			"manual_resolution instead of being guessed at",
		Command: []string{"bash", "scripts/test/fault"},
		Env:     []string{"TEST=recovery", "PALAI_SUITE_RUN=TestHostKillFencesStaleWriter"},
	},
	{
		Name:    "allocation-hygiene-and-substrate-quarantine",
		Cases:   []string{"SAN-007", "SAN-008"},
		What:    "reuse leaves zero prior-tenant residue; and the QUARANTINE half — a failed destroy quarantines the host, DENIES the next placement, and surfaces in the doctor",
		Command: []string{"bash", "scripts/test/component"},
		Env: []string{"TEST=artifacts",
			"PALAI_SUITE_RUN=TestAllocationReuseLeavesNoTenantResidue|TestFailedDestroyQuarantinesHost"},
	},
	{
		Name:  "runner-cordon-drain-revoke",
		Cases: []string{"SAN-011"},
		What:  "the runner gateway refuses new leases when cordoned, drains in-flight work, and a revoked runner's connect AND its in-flight session frames are both refused",
		Command: []string{"go", "test", "-count=1", "-v", "-run",
			"TestGatewayDialRefusesWhenCordoned|TestGatewayDrainWaitsForInFlightLease|TestGatewayRevokeRefusesConnectAndDial|TestGatewayRevokeDropsInFlightSessionFrames",
			"./apps/control-plane/internal/execution"},
	},
	{
		Name:    "uncertain-failure-job-quarantine",
		Cases:   []string{"SEC-102"},
		What:    "THE ADDED ARM — an uncertain worker outcome quarantines the job and NO worker re-claims it; leaving quarantine takes a deliberate re-dispatch that fences the worker whose outcome was uncertain",
		Command: []string{"bash", "scripts/test/component"},
		Env: []string{"TEST=postgres",
			"PALAI_SUITE_PKG=./apps/control-plane/internal/workers",
			"PALAI_SUITE_RUN=TestUncertainOutcomeQuarantineIsNotSilentlyReclaimed"},
	},
}

// unownedSANCases are the ids the SAN family reserves but this repo has never materialized (recorded
// in SAN-011's own ceiling note and the E10 plan §5). They are listed so the coverage guard below can
// tell "not written" from "written and quietly dropped from the suite".
var unownedSANCases = map[string]string{
	"SAN-009": "microVM tenant isolation — managed-scope, not claimed (plan §T7 honest ceiling)",
	"SAN-010": "unowned (E10 plan §5)",
	"SAN-012": "unowned (E10 plan §5)",
}

// result is one arm's outcome in the report.
type result struct {
	arm
	Passed     bool   `json:"passed"`
	DurationMS int64  `json:"duration_ms"`
	Output     string `json:"output,omitempty"`
}

// report is THE deliverable: one document that says no escape, and quarantine works.
type report struct {
	Suite          string   `json:"suite"`
	GeneratedAt    string   `json:"generated_at"`
	Arms           []result `json:"arms"`
	CasesCovered   []string `json:"cases_covered"`
	CasesUnowned   []string `json:"cases_unowned"`
	NoEscape       bool     `json:"no_escape"`
	QuarantineOK   bool     `json:"quarantine_works"`
	QuarantineArms []string `json:"quarantine_arms"`
	Ceiling        []string `json:"ceiling"`
	Failures       []string `json:"failures"`
}

// quarantineArms are the arms whose PASS is what "quarantine works" means. The report's
// quarantine_works is computed from THESE arms, never declared: if the substrate arm or the job arm
// is red, the claim is false no matter how green the denials are.
var quarantineArms = []string{"allocation-hygiene-and-substrate-quarantine", "uncertain-failure-job-quarantine"}

// TestEveryMaterializedSANCaseIsInTheSuite is the aggregation guard. It is Docker-free and runs first:
// a SAN case that exists on disk but no arm discharges would let the suite report "no escape" over a
// corpus it never ran.
func TestEveryMaterializedSANCaseIsInTheSuite(t *testing.T) {
	root := repoRoot(t)
	entries, err := os.ReadDir(filepath.Join(root, "tests", "uat", "cases"))
	if err != nil {
		t.Fatalf("read case catalog: %v", err)
	}
	covered := map[string]string{}
	for _, a := range suite {
		for _, id := range a.Cases {
			if prev, dup := covered[id]; dup {
				t.Fatalf("case %s is claimed by two arms (%s and %s)", id, prev, a.Name)
			}
			covered[id] = a.Name
		}
	}
	for _, e := range entries {
		id := e.Name()
		if !e.IsDir() || !strings.HasPrefix(id, "SAN-") {
			continue
		}
		if _, ok := covered[id]; ok {
			continue
		}
		if why, unowned := unownedSANCases[id]; unowned {
			t.Fatalf("%s is materialized on disk but listed as unowned (%q) — one of the two is wrong", id, why)
		}
		t.Fatalf("%s is materialized in tests/uat/cases but no escape-suite arm runs it; a denial nobody runs "+
			"is not a denial anyone has proven", id)
	}
	for id := range covered {
		if strings.HasPrefix(id, "SAN-") {
			if _, err := os.Stat(filepath.Join(root, "tests", "uat", "cases", id)); err != nil {
				t.Fatalf("arm %q claims %s, which has no case dir: %v", covered[id], id, err)
			}
		}
	}
}

// TestSandboxEscapeSuite runs the whole corpus as ONE pass and writes ONE report.
//
// It is Docker-bound and slow (real fixture images, a real cgroup, real Postgres + SeaweedFS), so it
// is gated behind `make uat-escape` rather than riding in `make verify`.
func TestSandboxEscapeSuite(t *testing.T) {
	if os.Getenv("PALAI_ESCAPE_SUITE") != "1" {
		t.Skip("PALAI_ESCAPE_SUITE=1 is required; run `make uat-escape` (Docker-bound: real OCI sandbox, real cgroup, real Postgres)")
	}
	root := repoRoot(t)
	rep := report{
		Suite:       "SEC-102 sandbox-escape suite",
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Ceiling: []string{
			"LOCAL OCI seam only: the microVM / managed high-isolation path is the SaaS plan and is marked managed-scope, not claimed (plan §T7, §T10).",
			"Kernel-exploit research is OUT OF SCOPE: this suite proves the DENIAL and QUARANTINE mechanics, not the absence of all escapes.",
			"Every arm is an EXISTING proof. This suite invents no escape class; it aggregates the corpus into one pass with one report.",
		},
	}

	for _, a := range suite {
		t.Run(a.Name, func(t *testing.T) {
			started := time.Now()
			cmd := exec.Command(a.Command[0], a.Command[1:]...)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), a.Env...)
			out, err := cmd.CombinedOutput()

			// An arm passes only when every test it NAMED actually reported PASS. Exit 0 alone is not
			// evidence of a denial: a -run regex that matches nothing exits 0 too, and so does a skip.
			proved, missing := a.missingProofs(string(out))
			a.Proved = proved
			res := result{arm: a, Passed: err == nil && len(missing) == 0, DurationMS: time.Since(started).Milliseconds()}
			if !res.Passed {
				res.Output = redact(tail(string(out), 4000))
				why := fmt.Sprintf("%v", err)
				if len(missing) > 0 {
					why = "NOT ATTEMPTED: " + strings.Join(missing, "; ")
					if err != nil {
						why = fmt.Sprintf("%v; %s", err, why)
					}
				}
				rep.Failures = append(rep.Failures, fmt.Sprintf("%s (%s): %s", a.Name, strings.Join(a.Cases, ","), why))
				t.Errorf("escape arm %s FAILED (%s)\ncases: %v\ncommand: %v %v\n%s",
					a.Name, why, a.Cases, a.Env, a.Command, res.Output)
			}
			rep.Arms = append(rep.Arms, res)
		})
	}

	passed := map[string]bool{}
	for _, r := range rep.Arms {
		passed[r.Name] = r.Passed
		if r.Passed {
			rep.CasesCovered = append(rep.CasesCovered, r.Cases...)
		}
	}
	sort.Strings(rep.CasesCovered)
	for id, why := range unownedSANCases {
		rep.CasesUnowned = append(rep.CasesUnowned, id+": "+why)
	}
	sort.Strings(rep.CasesUnowned)

	// Both claims are COMPUTED from the arm outcomes, never declared.
	rep.NoEscape = len(rep.Failures) == 0
	rep.QuarantineArms = quarantineArms
	rep.QuarantineOK = true
	for _, name := range quarantineArms {
		if !passed[name] {
			rep.QuarantineOK = false
		}
	}

	out := os.Getenv("PALAI_ESCAPE_REPORT")
	if out == "" {
		out = filepath.Join(root, "dist", "escape-suite", "escape-report.json")
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		t.Fatalf("create report dir: %v", err)
	}
	body, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	// The gate is over the MARSHALLED BODY, not over the strings redact() happened to be handed: this
	// report is a T10 evidence input, and the hardening commit's own premise is that a credential WAS
	// reaching it. Asserting here means a new field, a new arm, or a credential shape redact() does
	// not know fails the run instead of landing in the file.
	//
	// The gate is "redacting again changes nothing", NOT "the pattern does not match": the pattern
	// still matches its OWN output (`://user:REDACTED@`), so a MatchString gate would fire on every
	// correctly redacted failure report. redact is idempotent, so equality is the honest form — it is
	// false exactly when some credential got past on the way in.
	if redacted := redact(string(body)); redacted != string(body) {
		t.Fatalf("the escape report carries a scheme://user:PASSWORD@ credential that redact() did not "+
			"strip on the way in — a new field or arm is bypassing it. Report NOT written to %s", out)
	}
	if err := os.WriteFile(out, append(body, '\n'), 0o644); err != nil {
		t.Fatalf("write report: %v", err)
	}
	t.Logf("escape suite report: %s", out)
	t.Logf("no_escape=%v quarantine_works=%v over %d arms / %d SAN cases",
		rep.NoEscape, rep.QuarantineOK, len(rep.Arms), len(rep.CasesCovered))
	for _, c := range rep.Ceiling {
		t.Logf("CEILING: %s", c)
	}
	if !rep.NoEscape || !rep.QuarantineOK {
		t.Fatalf("escape suite is RED: no_escape=%v quarantine_works=%v failures=%v",
			rep.NoEscape, rep.QuarantineOK, rep.Failures)
	}
}

// throwawayCred matches the per-run database credential the tier scripts mint. Those die with their
// container and are never a real secret, but a failed arm's output goes into a FILE, so it is redacted
// on the way in rather than argued about afterwards (plan §2: the secret-scan gate covers every new
// output surface).
var throwawayCred = regexp.MustCompile(`(://[^:/@\s]+):[^@/\s]+@`)

func redact(s string) string { return throwawayCred.ReplaceAllString(s, "$1:REDACTED@") }

// tail keeps the last n bytes of a failed arm's output so the report stays readable.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "... (truncated)\n" + s[len(s)-n:]
}

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// TestRedactStripsEveryCredentialShapeTheTiersEmit is the test the hardening commit shipped without.
// redact() exists because a throwaway database credential WAS reaching a report file; a redactor with
// no test is a redactor nobody has checked, and the shapes below are the ones the tier scripts
// actually build (scripts/test/{component,fault,security} compose postgres://user:pw@host/db, and the
// artifacts tier adds an S3 endpoint).
func TestRedactStripsEveryCredentialShapeTheTiersEmit(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{
			name: "postgres component url",
			in:   "PALAI_COMPONENT_POSTGRES_URL=postgres://postgres:palai-component-1234567@127.0.0.1:55123/palai?sslmode=disable",
			want: "PALAI_COMPONENT_POSTGRES_URL=postgres://postgres:REDACTED@127.0.0.1:55123/palai?sslmode=disable",
		},
		{
			name: "fault url inside prose",
			in:   "dial postgres://postgres:palai-fault-987@127.0.0.1:5432/palai failed",
			want: "dial postgres://postgres:REDACTED@127.0.0.1:5432/palai failed",
		},
		{
			name: "two credentials on one line",
			in:   "a=postgres://u:p1@h/db b=http://k:p2@s3/bucket",
			want: "a=postgres://u:REDACTED@h/db b=http://k:REDACTED@s3/bucket",
		},
		{
			name: "no credential is left alone",
			in:   "postgres://127.0.0.1:5432/palai and http://127.0.0.1:8333/",
			want: "postgres://127.0.0.1:5432/palai and http://127.0.0.1:8333/",
		},
		{
			name: "a bare colon in prose is not a credential",
			in:   "note: the run took 3:04 and reached user@example.com",
			want: "note: the run took 3:04 and reached user@example.com",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := redact(tc.in); got != tc.want {
				t.Fatalf("redact(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
			// IDEMPOTENCE is what the pre-write gate rests on: it asserts redact(body) == body, which
			// is only a credential detector if a second pass over clean text is a no-op. (The naive
			// gate — throwawayCred.MatchString(body) — is NOT usable here: the pattern matches its own
			// `://user:REDACTED@` output, so it would fire on every correctly redacted failure.)
			if again := redact(redact(tc.in)); again != redact(tc.in) {
				t.Fatalf("redact() is not idempotent: %q -> %q -> %q", tc.in, redact(tc.in), again)
			}
		})
	}
}

// TestAnArmThatRanNothingIsNotAPass pins the discrimination the suite is FOR. `go test -run
// TestNoSuchThing ./pkg/` prints "ok ... [no tests to run]" and exits 0, so an arm scored on `err ==
// nil` reports a denial it never attempted. All 13 names exist today; this is the gate that notices
// the day one is renamed.
func TestAnArmThatRanNothingIsNotAPass(t *testing.T) {
	a := arm{
		Name:    "probe",
		Command: []string{"go", "test", "-count=1", "-v", "-run", "TestAlpha|TestBeta", "./pkg"},
	}
	if got := a.requiredTests(); len(got) != 2 || got[0] != "TestAlpha" || got[1] != "TestBeta" {
		t.Fatalf("requiredTests() = %v, want the two names the -run regex spells out", got)
	}

	// The exact output of a -run regex that matched nothing. It exits 0.
	if _, missing := a.missingProofs("testing: warning: no tests to run\nPASS\nok  \t./pkg\t0.1s [no tests to run]\n"); len(missing) == 0 {
		t.Fatalf("an arm that ran NOTHING was scored as a pass")
	}
	// A skip is not a denial either.
	if _, missing := a.missingProofs("--- PASS: TestAlpha (0.01s)\n--- SKIP: TestBeta (0.00s)\nPASS\nok\n"); len(missing) != 1 {
		t.Fatalf("a SKIPPED test was accepted as proof: missing=%v", missing)
	}
	// A subtest PASS is not its parent's PASS.
	if _, missing := a.missingProofs("--- PASS: TestAlpha (0.01s)\n    --- PASS: TestBeta/sub (0.00s)\nPASS\nok\n"); len(missing) != 1 {
		t.Fatalf("a subtest line was mistaken for the parent's proof: missing=%v", missing)
	}
	// Both really passed.
	proved, missing := a.missingProofs("--- PASS: TestAlpha (0.01s)\n--- PASS: TestBeta (0.02s)\nPASS\nok\n")
	if len(missing) != 0 || len(proved) != 2 {
		t.Fatalf("two genuine passes scored proved=%v missing=%v", proved, missing)
	}

	// Every arm in the real suite must name at least one test, or its PASS means nothing at all.
	for _, s := range suite {
		if len(s.requiredTests()) == 0 {
			t.Fatalf("arm %q binds to no -run regex, so an empty run would score it green", s.Name)
		}
	}
}
