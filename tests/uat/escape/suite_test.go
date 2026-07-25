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
}

// suite is the whole corpus. Adding a SAN case without adding it here fails
// TestEveryMaterializedSANCaseIsInTheSuite — a denial nobody runs is not a denial anyone has proven.
var suite = []arm{
	{
		Name:  "file-tool-confinement",
		Cases: []string{"SAN-001"},
		What:  "path traversal, an absolute path and an escaping symlink are all denied outside the allocation root",
		Command: []string{"go", "test", "-count=1", "-run", "TestFileToolDeniesWorkspaceEscape",
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
		Command: []string{"go", "test", "-count=1", "-run", "TestSnapshotRestoreChecksumsMatchCreate",
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
		Command: []string{"go", "test", "-count=1", "-run",
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
			res := result{arm: a, Passed: err == nil, DurationMS: time.Since(started).Milliseconds()}
			if err != nil {
				res.Output = tail(string(out), 4000)
				rep.Failures = append(rep.Failures, fmt.Sprintf("%s (%s): %v", a.Name, strings.Join(a.Cases, ","), err))
				t.Errorf("escape arm %s FAILED (%v)\ncases: %v\ncommand: %v %v\n%s",
					a.Name, err, a.Cases, a.Env, a.Command, res.Output)
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
