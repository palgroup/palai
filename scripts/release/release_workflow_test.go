// E18 T5 — .github/workflows/release.yml under test WITHOUT a GitHub run.
//
// There is no real CI run behind any of this (plan §T5 honest ceiling, §6 leg 1): no protected
// environment, no second maintainer, no OIDC workflow identity, and no `act`-class emulation is
// claimed either. What IS proven here, mechanically:
//
//   - the workflow parses, and every property the release depends on is asserted from the parsed
//     document — SHA-pinned actions, a manual-only trigger, least privilege, a protected environment
//     on the publishing job, and no `${{ }}` interpolated into any shell;
//   - the workflow is THIN — every `run:` is a call into scripts/release/*.sh and nothing else, which
//     is what makes the logic testable HERE instead of only on a runner;
//   - DRIFT — the T1→T2→T3→T4 chain order the workflow invokes is BIT-EQUAL to the order the scripts
//     themselves declare, recomputed from their own headers (the scripts are the canonical source;
//     the workflow is a consumer of it);
//   - the chain in THAT order really runs green end to end, locally: the order is read OUT of the
//     workflow and then EXECUTED, so a reordered workflow does not merely fail a string comparison,
//     it fails a run. (The order is also enforced by the scripts at runtime —
//     TestSBOMRefusesToRunOverAnAlreadyAttestedDir and TestProvenanceRejectsUnlistedFile are the two
//     halves of that fence; this test does not duplicate them.)
package release

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/palgroup/palai/tests/uat"
	"gopkg.in/yaml.v3"
)

const workflowPath = ".github/workflows/release.yml"

type wfStep struct {
	Name string            `yaml:"name"`
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	With map[string]any    `yaml:"with"`
	Env  map[string]string `yaml:"env"`
}

type wfJob struct {
	Name        string            `yaml:"name"`
	RunsOn      string            `yaml:"runs-on"`
	Environment string            `yaml:"environment"`
	Needs       []string          `yaml:"needs"`
	Timeout     int               `yaml:"timeout-minutes"`
	Env         map[string]string `yaml:"env"`
	Steps       []wfStep          `yaml:"steps"`
}

type workflow struct {
	Name        string            `yaml:"name"`
	On          map[string]any    `yaml:"on"`
	Permissions map[string]string `yaml:"permissions"`
	Concurrency struct {
		Group  string `yaml:"group"`
		Cancel bool   `yaml:"cancel-in-progress"`
	} `yaml:"concurrency"`
	Jobs map[string]wfJob `yaml:"jobs"`
}

func loadWorkflow(t *testing.T) workflow {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), workflowPath))
	if err != nil {
		t.Fatalf("read %s: %v", workflowPath, err)
	}
	var wf workflow
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("%s is not valid YAML (or `needs:` is a scalar where this test expects a list): %v", workflowPath, err)
	}
	// go-yaml v3 follows the YAML 1.2 core schema, so a bare `on:` key stays the string "on" (YAML 1.1
	// would have resolved it to the boolean true). If that ever changes the trigger assertions below fail
	// loudly rather than silently seeing no trigger at all.
	if len(wf.On) == 0 {
		t.Fatalf("%s parsed with no `on:` triggers — every trigger assertion below would be vacuous", workflowPath)
	}
	if len(wf.Jobs) == 0 {
		t.Fatalf("%s declares no jobs — every assertion below would be vacuous", workflowPath)
	}
	return wf
}

// jobsInDependencyOrder linearizes the workflow. The drift test can only speak about an order it can
// compute, so anything but a single linear chain is a hard failure telling the author to teach it the
// new shape rather than quietly comparing the wrong sequence.
func jobsInDependencyOrder(t *testing.T, wf workflow) []string {
	t.Helper()
	var roots []string
	dependents := map[string][]string{}
	for id, j := range wf.Jobs {
		switch len(j.Needs) {
		case 0:
			roots = append(roots, id)
		case 1:
			dependents[j.Needs[0]] = append(dependents[j.Needs[0]], id)
		default:
			t.Fatalf("job %q needs %d jobs; this drift test knows a LINEAR chain only — teach it the new shape", id, len(j.Needs))
		}
	}
	if len(roots) != 1 {
		t.Fatalf("the workflow has %d jobs with no `needs` (%v); this drift test knows a LINEAR chain only", len(roots), roots)
	}
	order := []string{roots[0]}
	for {
		next := dependents[order[len(order)-1]]
		if len(next) == 0 {
			break
		}
		if len(next) > 1 {
			t.Fatalf("job %q fans out to %v; this drift test knows a LINEAR chain only", order[len(order)-1], next)
		}
		order = append(order, next[0])
	}
	if len(order) != len(wf.Jobs) {
		t.Fatalf("linearized %d of %d jobs (%v) — some job is unreachable from the root", len(order), len(wf.Jobs), order)
	}
	return order
}

var scriptCall = regexp.MustCompile(`scripts/release/([a-z0-9-]+)\.sh`)

// workflowScriptCalls is every scripts/release/*.sh the workflow invokes, in execution order. When
// onRelease is set, only the calls that OPERATE ON the release directory are returned — that is what makes
// a call a step of the T1→T2→T3→T4 chain. `sbom.sh --hydrate-db` fetches the pinned vulnerability DB
// before anything is built and is not a chain step; discriminating on the directory rather than on a flag
// keeps the rule about what the step DOES.
func workflowScriptCalls(t *testing.T, wf workflow, onRelease bool) []string {
	t.Helper()
	var calls []string
	for _, id := range jobsInDependencyOrder(t, wf) {
		for _, s := range wf.Jobs[id].Steps {
			m := scriptCall.FindStringSubmatch(s.Run)
			if m == nil {
				continue
			}
			if onRelease && !strings.Contains(s.Run, "$PALAI_RELEASE_DIR") {
				continue
			}
			calls = append(calls, m[1]+".sh")
		}
	}
	return calls
}

// declaredChainOrder recomputes the release chain from the CANONICAL source: the scripts' own `# ORDER —`
// headers. sbom.sh and provenance.sh each declare the whole chain, and they must AGREE — two independent
// statements of the same rule, so a half-edited chain is caught before the workflow is even consulted.
func declaredChainOrder(t *testing.T) []string {
	t.Helper()
	var agreed []string
	for _, script := range []string{"sbom.sh", "provenance.sh"} {
		raw, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts/release", script))
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, line := range strings.Split(string(raw), "\n") {
			if !strings.HasPrefix(line, "# ORDER") {
				continue
			}
			if i := strings.Index(line, ". "); i >= 0 { // the ORDER sentence, not the paragraph under it
				line = line[:i]
			}
			for _, m := range scriptName.FindAllStringSubmatch(line, -1) {
				got = append(got, m[1])
			}
			break
		}
		if len(got) < 4 {
			t.Fatalf("scripts/release/%s declares no usable `# ORDER —` chain (parsed %v) — the drift test's canonical source is gone", script, got)
		}
		if agreed == nil {
			agreed = got
			continue
		}
		if strings.Join(agreed, " ") != strings.Join(got, " ") {
			t.Fatalf("the chain scripts disagree about their own order: sbom.sh says %v, %s says %v", agreed, script, got)
		}
	}
	if agreed[0] != "build.sh" {
		t.Fatalf("the declared chain does not start at build.sh: %v", agreed)
	}
	return agreed
}

var scriptName = regexp.MustCompile(`([a-z0-9-]+\.sh)`)

// TestReleaseWorkflowIsSchemaValidAndPinned — the workflow's own properties, read out of the parsed
// document rather than grepped: a release is a manual ACT (never a side effect of a push), it runs with
// least privilege, it is never cancelled mid-flight, every action is pinned to a full commit SHA (ci.yml's
// precedent), and the publishing job sits behind the PROTECTED environment the promote gate names.
func TestReleaseWorkflowIsSchemaValidAndPinned(t *testing.T) {
	wf := loadWorkflow(t)

	if _, ok := wf.On["workflow_dispatch"]; !ok {
		t.Errorf("the release workflow has no workflow_dispatch trigger: %v", wf.On)
	}
	for _, forbidden := range []string{"push", "pull_request", "schedule"} {
		if _, ok := wf.On[forbidden]; ok {
			t.Errorf("the release workflow triggers on %q — a release is an ACT a maintainer takes, never a side effect", forbidden)
		}
	}
	if wf.Permissions["contents"] != "read" {
		t.Errorf("top-level permissions.contents = %q, want read (least privilege; ci.yml's precedent)", wf.Permissions["contents"])
	}
	if wf.Concurrency.Group == "" || wf.Concurrency.Cancel {
		t.Errorf("concurrency = %+v; a release needs a group and must NOT be cancel-in-progress (a half-cancelled release is the thing tags exist to prevent)", wf.Concurrency)
	}

	pinned := regexp.MustCompile(`^[^@\s]+@[0-9a-f]{40}$`)
	uses := 0
	for id, job := range wf.Jobs {
		if job.Timeout == 0 {
			t.Errorf("job %q has no timeout-minutes — an ephemeral runner that never ends is a held signing identity", id)
		}
		if strings.HasSuffix(job.RunsOn, "-latest") || job.RunsOn == "" {
			t.Errorf("job %q runs-on %q — pin the runner image (ubuntu-24.04), a moving label is a moving build", id, job.RunsOn)
		}
		for _, s := range job.Steps {
			if s.Uses == "" {
				continue
			}
			uses++
			if !pinned.MatchString(s.Uses) {
				t.Errorf("job %q step %q uses %q — every action must be pinned to a full 40-char commit SHA (ci.yml's precedent)", id, s.Name, s.Uses)
			}
		}
	}
	if uses < 2 {
		t.Fatalf("only %d `uses:` steps parsed — the pinning assertion is vacuous", uses)
	}

	// The protected environment is the two-person gate's home, and its NAME is the one the gate checks.
	// Binding the assertion to uat.ReleaseEnvironment (rather than a second copy of the string) is what
	// keeps the workflow and the gate from drifting apart silently.
	protected := 0
	for id, job := range wf.Jobs {
		if job.Environment == "" {
			continue
		}
		protected++
		if job.Environment != uat.ReleaseEnvironment {
			t.Errorf("job %q runs in environment %q but the promote gate accepts only %q", id, job.Environment, uat.ReleaseEnvironment)
		}
	}
	if protected != 1 {
		t.Errorf("%d jobs declare an environment, want exactly 1 (the publishing job) — the two-person approval has one home", protected)
	}
}

// TestReleaseWorkflowIsThin — the job logic is ONLY calls into scripts/release/*.sh. That is the property
// that makes every other test in this file possible: logic that lives in a `run:` block can only ever be
// exercised on a runner, and there is no runner (plan §T5 honest ceiling).
//
// It also refuses `${{ }}` inside any `run:`. A workflow input interpolated into a shell is a script
// injection, and it would additionally mean the shell line differs from the one that runs locally.
func TestReleaseWorkflowIsThin(t *testing.T) {
	wf := loadWorkflow(t)
	runs := 0
	for id, job := range wf.Jobs {
		for _, s := range job.Steps {
			if strings.TrimSpace(s.Run) == "" {
				continue
			}
			runs++
			if strings.Contains(s.Run, "${{") {
				t.Errorf("job %q step %q interpolates ${{ }} into a shell — pass workflow inputs through `env:` instead (script injection, and it makes the line untestable locally):\n%s", id, s.Name, s.Run)
			}
			if !scriptCall.MatchString(s.Run) {
				t.Errorf("job %q step %q is job LOGIC, not a call into scripts/release/*.sh:\n%s", id, s.Name, s.Run)
				continue
			}
			// One command per step, and it IS the script: a script call with a pipeline or a second
			// command hanging off it is logic again, by another spelling.
			for _, sep := range []string{"|", "&&", "||", ";"} {
				if strings.Contains(s.Run, sep) {
					t.Errorf("job %q step %q chains %q onto the script call — keep the logic inside the script:\n%s", id, s.Name, sep, s.Run)
				}
			}
		}
	}
	if runs < 4 {
		t.Fatalf("only %d `run:` steps parsed — the thin-workflow assertion is vacuous", runs)
	}

	// Every script the workflow names must exist and be executable, or the thinness is aspirational.
	for _, script := range workflowScriptCalls(t, wf, false) {
		info, err := os.Stat(filepath.Join(repoRoot(t), "scripts/release", script))
		if err != nil {
			t.Errorf("the workflow calls scripts/release/%s, which does not exist: %v", script, err)
			continue
		}
		if info.Mode()&0o111 == 0 {
			t.Errorf("scripts/release/%s is not executable", script)
		}
	}
}

// TestReleaseWorkflowChainOrderMatchesTheScripts — THE DRIFT TEST. The T1→T2→T3→T4 order the workflow
// invokes must be BIT-EQUAL to the order the scripts declare for themselves. The scripts are the canonical
// source (they are what enforces the order at runtime); the workflow is a consumer of it, so when the two
// disagree the workflow is wrong.
func TestReleaseWorkflowChainOrderMatchesTheScripts(t *testing.T) {
	want := declaredChainOrder(t)
	inChain := map[string]bool{}
	for _, s := range want {
		inChain[s] = true
	}

	var got []string
	for _, call := range workflowScriptCalls(t, loadWorkflow(t), true) {
		if inChain[call] {
			got = append(got, call)
		}
	}
	if strings.Join(got, " → ") != strings.Join(want, " → ") {
		t.Fatalf("release.yml invokes the chain as\n  %s\nbut the scripts declare\n  %s", strings.Join(got, " → "), strings.Join(want, " → "))
	}
	// Exactly once each: a chain script invoked twice would satisfy a subsequence comparison while
	// running the release through a step it already passed.
	seen := map[string]int{}
	for _, c := range got {
		seen[c]++
	}
	for _, c := range want {
		if seen[c] != 1 {
			t.Errorf("the workflow invokes %s %d time(s), want exactly 1", c, seen[c])
		}
	}
}

// TestReleaseChainRunsInTheWorkflowsOrderLocally — the end-to-end local proof. The order is READ OUT of
// release.yml and then EXECUTED against a real release directory, so this is not a second string
// comparison: a workflow that reordered the chain would run the chain in that order and fail here.
//
// The steps are the real scripts. build.sh's leg is the output of the build.sh run TestMain performs (the
// package builds one pristine release and every case copies it); everything after it execs the shipped
// script over that directory.
func TestReleaseChainRunsInTheWorkflowsOrderLocally(t *testing.T) {
	chain := map[string]bool{}
	for _, s := range declaredChainOrder(t) {
		chain[s] = true
	}

	var dir, pub string
	key := ""
	steps := map[string]func(){
		"build.sh": func() {
			dir = stageRelease(t)
			addFixtureImage(t, dir)
		},
		"sbom.sh":       func() { rollupSBOMs(t, dir) },
		"provenance.sh": func() { key, pub = mintKey(t); attest(t, dir, key) },
		"release-verify.sh": func() {
			ok, out := releaseVerify(t, dir, pub)
			if !ok {
				t.Fatalf("release-verify.sh FAILED at the end of the workflow's own chain order:\n%s", out)
			}
			if !strings.Contains(out, "release-verify: OK") {
				t.Fatalf("release-verify.sh exited 0 without its OK line:\n%s", out)
			}
		},
	}

	ran := 0
	for _, call := range workflowScriptCalls(t, loadWorkflow(t), true) {
		if !chain[call] {
			continue
		}
		step, ok := steps[call]
		if !ok {
			t.Fatalf("the workflow's chain names %s, which this local runner cannot execute — teach it the new step", call)
		}
		step()
		ran++
	}
	if ran != len(chain) {
		t.Fatalf("ran %d of the %d chain steps", ran, len(chain))
	}
}

// --- the publication act: two-person, immutable, dry run ------------------------------------------------

// publish runs the real scripts/release/publish.sh with a controlled environment.
func publish(t *testing.T, env ...string) (bool, string) {
	t.Helper()
	cmd := exec.Command("/usr/bin/env", "bash", filepath.Join(repoRoot(t), "scripts/release/publish.sh"))
	cmd.Dir = repoRoot(t)
	cmd.Env = append(os.Environ(), append([]string{
		"PALAI_RELEASE_VERSION=18.0.0-rc1",
		"PALAI_RELEASE_EVIDENCE=self-host-0.2.0",
		"PALAI_RELEASE_TARGET=rc",
		"PALAI_RELEASE_APPROVAL=",
	}, env...)...)
	out, err := cmd.CombinedOutput()
	return err == nil, string(out)
}

func writeApproval(t *testing.T, mutate func(map[string]any)) string {
	t.Helper()
	rec := map[string]any{
		"schema": uat.ReleaseApprovalSchema, "release": "self-host-0.2.0", "target": "rc",
		"environment": uat.ReleaseEnvironment, "workflow_run": "https://github.com/palgroup/palai/actions/runs/1",
		"builder": "pal-salih", "approvers": []string{"someone-else"}, "admin_bypass": false,
	}
	if mutate != nil {
		mutate(rec)
	}
	path := filepath.Join(t.TempDir(), "approval.json")
	writeJSON(t, path, rec)
	return path
}

// TestPublishRefusesWithoutATwoPersonApproval — the publication act, unlike the local evidence gate, ALWAYS
// requires the approval: no record at all, a self-approval, and an admin bypass are each refused, each for
// its own named reason. This is release-policy.md's "The builder cannot bypass this gate, including as a
// repository administrator" with an exit code behind it.
func TestPublishRefusesWithoutATwoPersonApproval(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     []string
		wantMsg string
	}{
		{"no approval presented at all", nil, "no two-person approval"},
		{"an approval file that is not there", []string{"PALAI_RELEASE_APPROVAL=" + filepath.Join(t.TempDir(), "absent.json")}, "read the approval record"},
		{"a self-approval", []string{"PALAI_RELEASE_APPROVAL=" + writeApproval(t, func(m map[string]any) { m["approvers"] = []string{"@Pal-Salih "} })}, "cannot approve their own release"},
		{"an admin bypass", []string{"PALAI_RELEASE_APPROVAL=" + writeApproval(t, func(m map[string]any) { m["admin_bypass"] = true })}, "cannot bypass this gate"},
		{"an approval for another release", []string{"PALAI_RELEASE_APPROVAL=" + writeApproval(t, func(m map[string]any) { m["release"] = "sdk-provider-parity-0.1.0" })}, "approval is for release"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, out := publish(t, tc.env...)
			if ok {
				t.Fatalf("publish.sh PUBLISHED without a valid two-person approval:\n%s", out)
			}
			if !strings.Contains(out, tc.wantMsg) {
				t.Fatalf("the refusal does not name %q:\n%s", tc.wantMsg, out)
			}
		})
	}

	// ...and the honest ceiling travels with the refusal: even a perfectly formed record cannot pass in a
	// one-maintainer repository, and the transcript says WHY rather than reporting a generic denial.
	ok, out := publish(t, "PALAI_RELEASE_APPROVAL="+writeApproval(t, nil))
	if ok {
		t.Fatalf("publish.sh published in a repository whose CODEOWNERS names one maintainer:\n%s", out)
	}
	if !strings.Contains(out, "until two maintainers") {
		t.Fatalf("the refusal does not name release-policy's own sentence:\n%s", out)
	}
}

// dryRun runs the real scripts/release/publish-dryrun.sh over a bundle.
func dryRun(t *testing.T, bundle string, args []string, env ...string) (bool, string) {
	t.Helper()
	cmd := exec.Command("/usr/bin/env", append([]string{"bash",
		filepath.Join(repoRoot(t), "scripts/release/publish-dryrun.sh"), bundle}, args...)...)
	cmd.Dir = repoRoot(t)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return err == nil, string(out)
}

// TestDryRunNeverTagsAndNeverPublishes — the tag half of the rehearsal. A release is anchored to an
// annotated, cryptographically SIGNED tag (release-policy.md, "Required release identity"), so with no
// signing identity the rehearsal REFUSES rather than falling back to an unsigned tag; with one present it
// prints the exact commands and creates NOTHING. Either way the repository's tags are untouched.
func TestDryRunNeverTagsAndNeverPublishes(t *testing.T) {
	before := gitTags(t)

	t.Run("no signing identity refuses rather than tagging unsigned", func(t *testing.T) {
		ok, out := dryRun(t, pristineBundle, nil, "PALAI_RELEASE_TAG_SIGNER=")
		if ok {
			t.Fatalf("the rehearsal passed with NO tag signing identity:\n%s", out)
		}
		for _, want := range []string{"no tag signing identity", "SIGNED tag or it is not a release", "git tag -s v"} {
			if !strings.Contains(out, want) {
				t.Errorf("the refusal never says %q:\n%s", want, out)
			}
		}
	})

	t.Run("a signing identity rehearses, it does not tag", func(t *testing.T) {
		ok, out := dryRun(t, pristineBundle, nil, "PALAI_RELEASE_TAG_SIGNER=e18t5-fixture-identity")
		if !ok {
			t.Fatalf("the rehearsal failed with a signing identity present:\n%s", out)
		}
		for _, want := range []string{"git tag -s", "REHEARSED, not created", "NOTHING was published"} {
			if !strings.Contains(out, want) {
				t.Errorf("the transcript never says %q:\n%s", want, out)
			}
		}
	})

	if now := gitTags(t); now != before {
		t.Fatalf("the rehearsal changed the repository's tags (%d -> %d) — it must create none", before, now)
	}
}

// TestDryRunRefusesToOverwriteAReleasedTag — release-policy.md, "Revocation and rebuilds": "A released tag
// or artifact is never overwritten." The fence is proven by putting a tag in its way, which is the only way
// to know it fires: `git rev-parse -q --verify` against a name nothing ever holds proves nothing.
func TestDryRunRefusesToOverwriteAReleasedTag(t *testing.T) {
	const probe = "0.0.0-e18t5-immutability-probe"
	root := repoRoot(t)
	if out, err := exec.Command("git", "-C", root, "tag", "v"+probe).CombinedOutput(); err != nil {
		t.Fatalf("stage the probe tag: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("git", "-C", root, "tag", "-d", "v"+probe).Run() })

	ok, out := dryRun(t, pristineBundle, []string{probe}, "PALAI_RELEASE_TAG_SIGNER=e18t5-fixture-identity")
	if ok {
		t.Fatalf("the rehearsal was willing to re-tag an already released version:\n%s", out)
	}
	if !strings.Contains(out, "already exists") || !strings.Contains(out, "NEVER overwritten") {
		t.Fatalf("the refusal does not name the immutability rule:\n%s", out)
	}
}

func gitTags(t *testing.T) int {
	t.Helper()
	out, err := exec.Command("git", "-C", repoRoot(t), "tag", "-l").Output()
	if err != nil {
		t.Fatal(err)
	}
	return len(strings.Fields(string(out)))
}

// TestDryRunPublishesNothingToAnyRegistry — the three publish rehearsals over a REAL three-package bundle:
// the npm tarball, the wheel/sdist metadata, the Go module tag. Every leg runs with its registry blackholed
// (npm at the discard port, Go with GOPROXY=off), so the proof that nothing was uploaded is not the words
// "dry run" in the transcript — it is that a leg which reached out would have FAILED.
//
// It needs npm + uv to build the TypeScript and Python packages; without them the package-level TestMain
// bundle carries only the Go source snapshot and the first two legs would be honestly SKIPPED rather than
// proven, so the test skips instead of passing vacuously.
func TestDryRunPublishesNothingToAnyRegistry(t *testing.T) {
	for _, tool := range []string{"npm", "uv"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not on PATH; the npm/PyPI rehearsal legs are a workstation/CI gate", tool)
		}
	}
	bundle := t.TempDir()
	build := exec.Command("/usr/bin/env", "bash", filepath.Join(repoRoot(t), "scripts/release/sdk-package.sh"))
	build.Env = append(os.Environ(), "OUT="+bundle, "VERSION=0.1.0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("sdk-package.sh (three packages): %v\n%s", err, out)
	}

	ok, out := dryRun(t, bundle, nil, "PALAI_RELEASE_TAG_SIGNER=e18t5-fixture-identity")
	if !ok {
		t.Fatalf("the three-leg rehearsal failed:\n%s", out)
	}
	for _, want := range []string{
		"npm publish --dry-run",           // (1) the npm leg really ran the publish verb
		"registry blackholed at http://",  // ...against a registry that is not there
		"(1) npm OK",                      //
		"twine-check-class",               // (2) the wheel/sdist metadata leg
		"Metadata-Version=2.4 Name=palai", // ...over metadata read out of the built artifacts
		"(2) python OK",                   //
		"go module tag rehearsal",         // (3) the Go leg
		"GOPROXY=off",                     // ...with the module proxy off
		"(3) go module tag OK",            //
		"NOTHING was published",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the dry-run transcript never says %q:\n%s", want, out)
		}
	}
	// A wheel and an sdist that disagree, or that disagree with the version being published, would publish
	// something other than the release under its name. The RED arm: the same bundle at another version.
	if ok, out := dryRun(t, bundle, []string{"9.9.9"}, "PALAI_RELEASE_TAG_SIGNER=e18t5-fixture-identity"); ok {
		t.Fatalf("the rehearsal passed a bundle whose packages carry a DIFFERENT version:\n%s", out)
	} else if !strings.Contains(out, "but this publication is") {
		t.Fatalf("the refusal does not name the version disagreement:\n%s", out)
	}
}

// TestNoScriptCanPublishForReal — the standing rule of this phase (plan §5, §6 leg 2): no script in the
// repository publishes to a registry. Every publish verb must carry --dry-run, and the upload verbs must
// not appear at all. sdk-package.sh's manifest has said "publish is E18" since E16 T7; this is E18 saying
// the same thing with a test behind it.
func TestNoScriptCanPublishForReal(t *testing.T) {
	root := repoRoot(t)
	var offenders []string
	err := filepath.Walk(filepath.Join(root, "scripts"), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		// The scripts, not the tests that talk ABOUT the scripts (this file names every forbidden verb).
		if ext := filepath.Ext(path); ext != ".sh" && ext != ".py" && ext != "" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for i, line := range strings.Split(string(b), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "#") { // prose about publishing is not publishing
				continue
			}
			for _, verb := range []string{"npm publish", "uv publish", "twine upload", "poetry publish", "docker push"} {
				if !strings.Contains(line, verb) {
					continue
				}
				if verb == "npm publish" && strings.Contains(line, "--dry-run") {
					continue
				}
				offenders = append(offenders, fmt.Sprintf("%s:%d: %s", rel, i+1, trimmed))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Fatalf("these lines could publish for real (there are no credentials and this session publishes nothing):\n%s", strings.Join(offenders, "\n"))
	}
}
