package fleet

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// THE §3.6 D4 SWEEP: no line in this tree may say the runner enrolment credential is one-use.
//
// The belief is FALSE and has been for a while. `FileEnrollmentTokens`, the only production implementation,
// carries the heading "WHY THIS IS NOT ONE-USE, AND WHAT REPLACED THAT": the control plane admits one
// redemption per issued-certificate lifetime, and re-presentation inside that window is the ONLY path back
// for a machine whose certificate expired. Delete that and an expired machine is unrecoverable.
//
// WHY IT IS SWEPT RATHER THAN TRUSTED, in three measurements rather than as a principle:
//
//   - the plan's §3.6 D4 counted FOUR copies of the belief and asked for three to be corrected;
//   - E24 T3 grepped and found TEN, corrected nine files, and wrote in its commit that one remained;
//   - THIS sweep found the belief in twelve more places T3 never visited, including FIVE lines inside
//     `packages/runner/enrollment.go` — the package that IMPLEMENTS enrolment, and the package
//     `cmd/runner/main.go` calls, whose own copy T3 did correct. That is E23 T7's D7 lesson arriving for the
//     third time: a correction that visits the file a plan names and not every place the belief lives keeps
//     shipping the belief. A sweep is the only form of that correction that cannot decay.
//
// AND THE EXCEPTION T3 ASKED FOR IS NOT HERE, BECAUSE ITS REASON WAS MEASURED FALSE. T3 left
// tests/uat/cases/LP-012/case.yaml alone on the grounds that "a case file is checksummed into a released
// bundle, so correcting the sentence there would redden the RC". Measured: local-live-0.1.0 is declared
// LegacyShapeOnly, so its checksums are shape-only and are not recomputed at all, and NOTHING in this tree
// hashes a case file's CONTENT — the release index reads only whether case.yaml EXISTS (os.Stat). The comment
// was corrected and `go test ./tests/uat/...`, `evidence-verify RELEASE=local-live-0.1.0` and
// `evidence-verify RELEASE=release-1.0.0-rc1` all stayed green. So this sweep has NO allow-list.

// beliefPhrases are the ways the tree has actually said it. `single-use` is included and the plan's three
// literals are not enough: MASTER-SPEC §24.9 says "a single-use enrollment token", which none of "one-use",
// "already-spent" or "spent once" matches. A sweep written to the plan's letter would have walked past the
// program's own specification.
var beliefPhrases = regexp.MustCompile(`(?i)one-use|one use|single-use|single use|already-spent|already spent|spent once|spent exactly once`)

// enrolmentWords scope the sweep to the RUNNER ENROLMENT credential. Without this the sweep would fail on
// things that are correctly one-use and have nothing to do with a runner: the remote-tool callback token
// (TOL-017, genuinely one-use and audience-bound), the A2A push notification id, and Vault's response-wrapping
// pattern in MASTER-SPEC's reference table. A guard that fires on true statements gets deleted.
var enrolmentWords = regexp.MustCompile(`(?i)enroll?ment|enrol\b|enrols|bootstrap token|bootstrap credential|runner.?token|runner enrol`)

// correctionMarkers exempt a line that NEGATES the belief or attributes it to a test stub. A correction has to
// be able to quote what it is correcting, and the tree's corrected lines read "NOT ONE-USE", "not one-use",
// "the bound that replaced one-use" and "THIS COMMENT USED TO SAY IT WAS". This is the ONE mechanism — there
// is no per-file allow-list, so a new wrong line anywhere fails, including in a file that already holds a
// corrected one.
var correctionMarkers = regexp.MustCompile(`(?i)not one-use|not one use|not single-use|replaced one-use|used to say|is NOT ONE-USE|WHY THIS IS NOT`)

// sweepOwnPath is this file, as `git ls-files` names it. It is asserted to EXIST by
// TestTheOneUseSweepExcludesExactlyItself, so a rename that stranded the exclusion would leave the sweep
// failing on its own fixtures rather than silently excluding nothing.
const sweepOwnPath = "tests/uat/fleet/oneuse_test.go"

// TestNoLineClaimsTheEnrolmentCredentialIsOneUse walks every TRACKED file — `git ls-files`, so generated
// artefacts and node_modules cannot dilute it and a file somebody forgot to add cannot hide in it.
//
// TWO PATHS ARE EXCLUDED AND BOTH ARE SELF-REFERENCE RATHER THAN CONVENIENCE:
//
//   - docs/superpowers/plans/ RECORDS the belief as the thing being corrected — §3.6 D4 quotes all four copies
//     verbatim, which is its job — and a plan that could not quote a wrong belief could not describe
//     correcting one;
//   - THIS FILE, which is built out of the vocabulary it bans: its fixtures below are the tree's own wrong
//     lines verbatim, and its failure message names the phrase to look for. It found itself the moment it was
//     committed and became a tracked file, which is a small joke and also the reason the exclusion is by exact
//     path rather than by prefix — `tests/uat/fleet/` as a directory would let a second file in this package
//     ship the belief unchecked.
func TestNoLineClaimsTheEnrolmentCredentialIsOneUse(t *testing.T) {
	root := repoRoot(t)
	cmd := exec.Command("git", "ls-files", "-z")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("list tracked files: %v", err)
	}
	files := strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
	if len(files) < 500 {
		t.Fatalf("git ls-files returned %d paths — the sweep is not walking the tree, so a green here is a green over nothing", len(files))
	}

	var hits []string
	scanned := 0
	for _, rel := range files {
		if rel == "" || strings.HasPrefix(rel, "docs/superpowers/plans/") || rel == sweepOwnPath {
			continue
		}
		body, readErr := readTracked(root, rel)
		if readErr != nil {
			continue // a binary or unreadable blob is not evidence either way
		}
		scanned++
		for n, line := range strings.Split(body, "\n") {
			if !beliefPhrases.MatchString(line) || !enrolmentWords.MatchString(line) {
				continue
			}
			if correctionMarkers.MatchString(line) {
				continue
			}
			hits = append(hits, trimHit(rel, n+1, line))
		}
	}
	if scanned < 400 {
		t.Fatalf("only %d tracked files were read — the sweep is not reading the tree", scanned)
	}
	if len(hits) != 0 {
		sort.Strings(hits)
		t.Errorf("%d line(s) still say the runner enrolment credential is one-use. It is NOT: the control plane admits one redemption per issued-certificate lifetime, and re-presenting it inside that window is the only way a machine whose certificate expired can recover. Say \"fresh per boot, re-presentable within that boot\" instead:\n  %s",
			len(hits), strings.Join(hits, "\n  "))
	}
}

// TestTheOneUseSweepCanActuallyFail is the guard for the guard, and it is not ceremony: a sweep with a typo in
// either regexp reports exactly the same green as a clean tree. So both halves are driven over synthetic lines
// — the belief must be CAUGHT, and each exclusion must actually exclude, or the sweep is either blind or so
// loud it gets switched off.
func TestTheOneUseSweepCanActuallyFail(t *testing.T) {
	caught := []string{
		"// the one-use bootstrap token is spent once at enrollment",
		"1. An administrator creates a single-use enrollment token scoped to organization",
		"Mint the one-use runner enrollment token as well",
		"# mint a fresh one-use runner token",
	}
	for _, line := range caught {
		if !beliefPhrases.MatchString(line) || !enrolmentWords.MatchString(line) || correctionMarkers.MatchString(line) {
			t.Errorf("the sweep does NOT catch %q — every phrasing the tree actually used must be caught, and this list is those phrasings verbatim", line)
		}
	}

	// The two exclusions, each shown to exclude the thing it exists for.
	notAboutEnrolment := []string{
		"// the callback token is one-use and audience-bound",
		"| Vault response wrapping | one-use secret handoff pattern |",
		"every delivery carries a timestamp and a single-use notification id",
	}
	for _, line := range notAboutEnrolment {
		if enrolmentWords.MatchString(line) {
			t.Errorf("the enrolment scope matches %q, which is about a DIFFERENT credential that really is one-use — a guard that fires on true statements is a guard somebody deletes", line)
		}
	}
	corrected := []string{
		"// THE BOOTSTRAP CREDENTIAL IS NOT ONE-USE, AND THIS COMMENT USED TO SAY IT WAS",
		"# --- 4. runner enrollment token (the in-stack runner presents it; re-presentable, not one-use) ---",
		"// recovery path depends on, and the bound that replaced one-use. The token is a bootstrap",
	}
	for _, line := range corrected {
		if !correctionMarkers.MatchString(line) {
			t.Errorf("a CORRECTED line is not exempted: %q — a correction has to be able to quote what it corrects, or the tree cannot say the true thing at all", line)
		}
	}
}

func trimHit(rel string, line int, text string) string {
	text = strings.TrimSpace(text)
	if len(text) > 150 {
		text = text[:150] + "…"
	}
	return rel + ":" + itoa(line) + ": " + text
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// readTracked reads a tracked file as text, refusing anything that looks binary.
func readTracked(root, rel string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		return "", err
	}
	if bytes.IndexByte(raw, 0) >= 0 {
		return "", errBinary
	}
	return string(raw), nil
}

var errBinary = errors.New("binary file")

// TestTheOneUseSweepExcludesExactlyItself keeps the self-exclusion honest in both directions. If this file is
// renamed the constant goes stale, and a stale exclusion is the WORSE failure of the two available: it would
// exclude nothing, the sweep would go red on its own fixtures, and the cheapest way to silence that is to
// widen the exclusion to the whole directory — which is how a second file in this package would come to ship
// the belief unchecked.
func TestTheOneUseSweepExcludesExactlyItself(t *testing.T) {
	if _, err := os.Stat(filepath.Join(repoRoot(t), sweepOwnPath)); err != nil {
		t.Fatalf("sweepOwnPath = %q does not exist: %v — the self-exclusion now excludes nothing, and this file's own fixtures will be reported as tree findings", sweepOwnPath, err)
	}
	if !strings.HasSuffix(sweepOwnPath, ".go") || strings.Count(sweepOwnPath, "/") < 2 {
		t.Errorf("sweepOwnPath = %q is not an exact file path — a directory or prefix exclusion would let a sibling file in this package ship the belief unchecked", sweepOwnPath)
	}
}
